package sqlite

// Moving an open issue between two projects of one store (istore.Move,
// OpMoveIn, OpMoveOut, OpMoveDone). The two projects are two files, and
// SQLite in WAL mode does not commit across attached files atomically, so a
// move is three transactions the service runs in order while both projects
// hold writes: move in on the target (the copy stays pending, refusing
// writes, and a rerun rewrites it), move out on the source (a no-op once the
// redirect is there), then done on the target (the copy takes writes).

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/ids"
)

var _ istore.Mover = (*project)(nil)

// movingRows is this driver's copy of one issue: every column of its rows.
type movingRows struct {
	Issue     map[string]any
	Labels    []string
	Comments  []map[string]any
	History   []map[string]any
	Deps      []map[string]any // its own edges
	LastChild int
}

// Export implements istore.Mover.
func (p *project) Export(ctx context.Context, id string) (istore.Moving, error) {
	var out istore.Moving
	tx, err := p.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if out.Issue, err = getIssue(ctx, tx, p.id, id, istore.Areas{All: true}); err != nil {
		return out, err
	}
	its := []orchapi.Issue{out.Issue}
	if err := enrich(ctx, tx, p.id, its, istore.Areas{All: true}, false); err != nil {
		return out, err
	}
	out.Issue = its[0]
	r := &movingRows{Labels: out.Issue.Labels}
	is, err := mapRows(ctx, tx, `SELECT * FROM issues WHERE id = ?`, id)
	if err != nil {
		return out, err
	}
	if len(is) != 1 {
		return out, istore.ErrNotFound
	}
	r.Issue = is[0]
	out.HomeArea = str(r.Issue["home_area"])
	if r.Comments, err = mapRows(ctx, tx, `SELECT * FROM comments WHERE issue_id = ? ORDER BY created_at, id`, id); err != nil {
		return out, err
	}
	if r.History, err = mapRows(ctx, tx, `SELECT * FROM history WHERE issue_id = ? ORDER BY id`, id); err != nil {
		return out, err
	}
	deps, err := mapRows(ctx, tx, `SELECT * FROM deps WHERE issue_id = ? OR depends_on = ? ORDER BY issue_id, depends_on`, id, id)
	if err != nil {
		return out, err
	}
	for _, d := range deps {
		dep := orchapi.IssueDep{IssueID: str(d["issue_id"]), DependsOnID: str(d["depends_on"]), Type: str(d["type"]), External: num(d["external"]) != 0,
			CreatedAt: fromMillis(num(d["created_at"]))}
		out.Deps = append(out.Deps, dep)
		if dep.IssueID == id {
			r.Deps = append(r.Deps, d)
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_child FROM child_counters WHERE parent = ?`, id).Scan(&r.LastChild); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.Rows = r
	return out, nil
}

// MovedTo implements istore.Mover.
func (p *project) MovedTo(ctx context.Context, id string) (string, string, bool, error) {
	return p.moveRow(ctx, `SELECT new_id, project FROM moves WHERE dir = 'out' AND old_id = ?`, id)
}

// MovedFrom implements istore.Mover.
func (p *project) MovedFrom(ctx context.Context, id string) (string, string, bool, error) {
	return p.moveRow(ctx, `SELECT old_id, project FROM moves WHERE dir = 'in' AND new_id = ?`, id)
}

// MoveEdges implements istore.Mover.
func (p *project) MoveEdges(ctx context.Context, project, id string) ([]istore.MoveEdge, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT dir, old_id, new_id, project, moved_at FROM moves
		WHERE (?1 = ?3 AND ((dir = 'out' AND old_id = ?2) OR (dir = 'in' AND new_id = ?2)))
			OR (project = ?3 AND ((dir = 'out' AND new_id = ?2) OR (dir = 'in' AND old_id = ?2)))`, p.id, id, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []istore.MoveEdge
	for rows.Next() {
		var dir, old, nid, other string
		var at int64
		if err := rows.Scan(&dir, &old, &nid, &other, &at); err != nil {
			return nil, err
		}
		e := istore.MoveEdge{FromProject: p.id, FromID: old, ToProject: other, ToID: nid, At: fromMillis(at)}
		if dir == "in" {
			e.FromProject, e.ToProject = other, p.id
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *project) moveRow(ctx context.Context, q, id string) (string, string, bool, error) {
	var other, project string
	err := p.r.QueryRowContext(ctx, q, id).Scan(&other, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return other, project, err == nil, err
}

// issue is what the write reports: the issue as it was, for a move out.
// Every write ends here, so it also refuses (rolling the write back) one to
// a copy still moving in, and a create that names an id that moved out.
func (w *writer) issue() (orchapi.Issue, error) {
	if w.gone != nil {
		return *w.gone, nil
	}
	if err := w.moveGuard(); err != nil {
		return orchapi.Issue{}, err
	}
	return w.load(w.id)
}

func (w *writer) moveGuard() error {
	switch w.m.Op {
	case istore.OpMoveIn, istore.OpMoveOut, istore.OpMoveDone:
		return nil
	case istore.OpCreate:
		if w.m.Create == nil || w.m.Create.ID == "" {
			return nil
		}
		var to string
		err := w.tx.QueryRowContext(w.ctx, `SELECT new_id FROM moves WHERE dir = 'out' AND old_id = ?`, w.id).Scan(&to)
		if err == nil {
			return conflict("%s is reserved and cannot be reused", w.id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	}
	var n int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM moves WHERE dir = 'in' AND new_id = ? AND pending = 1`, w.id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return conflict("%s is still moving in; run the move again to finish it", w.id)
	}
	return nil
}

func (w *writer) move() error {
	mv := w.m.Move
	if mv == nil || w.m.ID == "" || mv.NewID == "" || mv.Project == "" {
		return bad("a move needs the issue, its new id and the other project")
	}
	switch w.m.Op {
	case istore.OpMoveOut:
		return w.moveOut()
	case istore.OpMoveDone:
		var n int
		if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM moves m JOIN issues i ON i.id = m.new_id
			WHERE m.dir = 'in' AND m.old_id = ? AND m.new_id = ?`, w.m.ID, mv.NewID).Scan(&n); err != nil || n == 0 {
			return err
		}
		w.id = mv.NewID
		return w.exec(`UPDATE moves SET pending = 0 WHERE dir = 'in' AND old_id = ? AND new_id = ?`, w.m.ID, mv.NewID)
	}
	return w.moveIn()
}

func (w *writer) exec(q string, args ...any) error {
	_, err := w.tx.ExecContext(w.ctx, q, args...)
	return err
}

type stmt struct {
	q    string
	args []any
}

func (w *writer) execAll(ss []stmt) error {
	for _, s := range ss {
		if err := w.exec(s.q, s.args...); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) moveIn() error {
	mv, old := w.m.Move, w.m.ID
	var r *movingRows
	if mv.From != nil {
		r, _ = mv.From.Rows.(*movingRows)
	}
	if r == nil || r.Issue == nil {
		return bad("a move in needs this store's export of the issue")
	}
	nid := mv.NewID
	var n int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM issues WHERE id = ?`, nid).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		var from string
		err := w.tx.QueryRowContext(w.ctx, `SELECT old_id FROM moves WHERE dir = 'in' AND new_id = ?`, nid).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && from != old) {
			return conflict("%s is another issue in this project", nid)
		}
		if err != nil {
			return err
		}
		// Writes since its copy (made before writes to a pending copy were
		// refused, or by hand): history after the move's own row.
		var own int
		if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM history WHERE issue_id = ? AND kind != 'area_changed'
			AND id > (SELECT mark FROM moves WHERE dir = 'in' AND new_id = ?)`, nid, nid).Scan(&own); err != nil {
			return err
		}
		if own > 0 {
			return conflict("%s has changed since it moved in; it is not copied again", nid)
		}
		for _, q := range []string{`DELETE FROM labels WHERE issue_id = ?`, `DELETE FROM comments WHERE issue_id = ?`,
			`DELETE FROM history WHERE issue_id = ?`, `DELETE FROM deps WHERE issue_id = ?`, `DELETE FROM issues WHERE id = ?`} {
			if err := w.exec(q, nid); err != nil {
				return err
			}
		}
	}
	area := w.areaOf(nid, mv.HomeArea, r.Labels)
	if err := w.m.Authorize(area, area); err != nil {
		return err
	}
	// resolve is where an edge to t goes here: an issue of this run or one
	// that moved in before (by its new id), one of this project's, else t
	// stays a cross-project edge.
	resolve := func(t string) (string, bool, error) {
		if nt, ok := mv.Rename[t]; ok {
			return nt, true, nil
		}
		var nt string
		err := w.tx.QueryRowContext(w.ctx, `SELECT new_id FROM moves WHERE dir = 'in' AND old_id = ?`, t).Scan(&nt)
		if err == nil {
			return nt, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
		err = w.tx.QueryRowContext(w.ctx, `SELECT id FROM issues WHERE id = ?`, t).Scan(&nt)
		if errors.Is(err, sql.ErrNoRows) {
			return t, false, nil
		}
		return nt, err == nil, err
	}
	it := clone(r.Issue)
	parent := ""
	if op := str(it["parent"]); op != "" {
		np, local, err := resolve(op)
		if err != nil {
			return err
		}
		if local {
			parent = np
		}
	}
	var number int64
	if err := w.tx.QueryRowContext(w.ctx, `SELECT coalesce(max(number), 0) + 1 FROM issues`).Scan(&number); err != nil {
		return err
	}
	it["id"], it["area"], it["home_area"], it["parent"], it["number"], it["is_blocked"] = nid, area, mv.HomeArea, parent, number, 0
	it["version"], it["versions"] = "", ""
	if err := w.insertMap("issues", it); err != nil {
		return err
	}
	for _, l := range r.Labels {
		if err := w.exec(`INSERT OR IGNORE INTO labels (issue_id, label) VALUES (?, ?)`, nid, l); err != nil {
			return err
		}
	}
	for _, c := range r.Comments {
		c = clone(c)
		c["issue_id"] = nid
		if err := w.insertMap("comments", c); err != nil {
			return err
		}
	}
	for _, h := range r.History {
		h = clone(h)
		h["id"], h["issue_id"] = ids.New(), nid
		if err := w.insertMap("history", h); err != nil {
			return err
		}
	}
	for _, d := range r.Deps {
		d = clone(d)
		t, local, err := resolve(str(d["depends_on"]))
		if err != nil {
			return err
		}
		ext := 1
		if local {
			ext = 0
		}
		if t == nid {
			continue
		}
		d["issue_id"], d["depends_on"], d["external"] = nid, t, ext
		if err := w.insertMap("deps", d); err != nil {
			return err
		}
	}
	// A parent that moved before it reached it as an edge to its new id.
	if parent == "" {
		if err := w.exec(`UPDATE issues SET parent = coalesce((SELECT min(d.depends_on) FROM deps d JOIN issues p ON p.id = d.depends_on
			WHERE d.issue_id = ?1 AND d.type = 'parent-child'), '') WHERE id = ?1`, nid); err != nil {
			return err
		}
	}
	// Edges here to it now find it, including the alias left when it last
	// moved out of this project. After a trip through a third project, that
	// alias differs from both old and nid. Read it before removing the redirect.
	var seeds []string
	rows, err := w.tx.QueryContext(w.ctx, `SELECT issue_id FROM deps WHERE depends_on IN (?, ?,
		(SELECT new_id FROM moves WHERE dir = 'out' AND old_id = ?))`, old, nid, nid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		seeds = append(seeds, s)
	}
	rows.Close()
	if err := w.execAll([]stmt{{`UPDATE OR IGNORE deps SET depends_on = ?, external = 0 WHERE depends_on IN (?,
		(SELECT new_id FROM moves WHERE dir = 'out' AND old_id = ?))`, []any{nid, old, nid}},
		{`DELETE FROM deps WHERE depends_on != ? AND depends_on IN (?,
			(SELECT new_id FROM moves WHERE dir = 'out' AND old_id = ?))`, []any{nid, old, nid}},
		{`UPDATE issues SET parent = ? WHERE parent = '' AND id IN (SELECT issue_id FROM deps WHERE depends_on = ? AND type = 'parent-child')`, []any{nid, nid}}}); err != nil {
		return err
	}
	if r.LastChild > 0 {
		if err := w.exec(`INSERT INTO child_counters (parent, last_child) VALUES (?, ?)
			ON CONFLICT (parent) DO UPDATE SET last_child = max(last_child, excluded.last_child)`, nid, r.LastChild); err != nil {
			return err
		}
	}
	if err := w.record(nid, "moved", fieldDiff{"id": {old, nid}}, "", area, area); err != nil {
		return err
	}
	// It is here again: an earlier redirect away from nid is gone.
	if err := w.execAll([]stmt{{`DELETE FROM moves WHERE dir = 'out' AND old_id = ?`, []any{nid}},
		{`INSERT OR REPLACE INTO moves (dir, old_id, new_id, project, moved_at, pending, mark) VALUES ('in', ?, ?, ?, ?, 1, ?)`,
			[]any{old, nid, mv.Project, millis(w.m.At), w.changes[len(w.changes)-1].ID}}}); err != nil {
		return err
	}
	w.id = nid
	return w.refreshBlocked(append([]string{nid}, seeds...))
}

func (w *writer) moveOut() error {
	mv, old := w.m.Move, w.m.ID
	var done int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM moves WHERE dir = 'out' AND old_id = ?
		AND NOT EXISTS (SELECT 1 FROM issues WHERE id = ?)`, old, old).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		w.id, w.gone = old, &orchapi.Issue{ID: old, Project: w.p.id}
		return nil
	}
	cur, err := w.loadRow(old)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	if err := w.record(old, "moved", fieldDiff{"id": {old, mv.NewID}}, "", cur.Area, ""); err != nil {
		return err
	}
	var seeds []string
	rows, err := w.tx.QueryContext(w.ctx, `SELECT issue_id FROM deps WHERE depends_on = ? AND issue_id != ?`, old, old)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		seeds = append(seeds, s)
	}
	rows.Close()
	o := []any{old}
	if err := w.execAll([]stmt{
		{`DELETE FROM labels WHERE issue_id = ?`, o}, {`DELETE FROM comments WHERE issue_id = ?`, o},
		{`DELETE FROM history WHERE issue_id = ? AND id != ?`, []any{old, w.changes[0].ID}}, {`DELETE FROM deps WHERE issue_id = ?`, o},
		{`DELETE FROM child_counters WHERE parent = ?`, o}, {`DELETE FROM issues WHERE id = ?`, o},
		// Edges to it name its new id, in the other project.
		{`UPDATE OR IGNORE deps SET depends_on = ?, external = 1 WHERE depends_on = ?`, []any{mv.NewID, old}},
		{`DELETE FROM deps WHERE depends_on = ?`, o}, {`UPDATE issues SET parent = '' WHERE parent = ?`, o},
		// It came in from elsewhere once: that record goes with it.
		{`DELETE FROM moves WHERE dir = 'in' AND new_id = ?`, o},
		{`INSERT OR REPLACE INTO moves (dir, old_id, new_id, project, moved_at) VALUES ('out', ?, ?, ?, ?)`, []any{old, mv.NewID, mv.Project, millis(w.m.At)}},
	}); err != nil {
		return err
	}
	gone := cur.Issue
	w.id, w.gone = old, &gone
	sort.Strings(seeds)
	return w.refreshBlocked(seeds)
}

// mapRows reads every row of q as column -> value.
func mapRows(ctx context.Context, q queryer, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// insertMap inserts one row of column -> value (columns sorted).
func (w *writer) insertMap(table string, m map[string]any) error {
	cols := make([]string, 0, len(m))
	for c := range m {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = m[c]
	}
	q := `INSERT INTO ` + table + ` (` + strings.Join(cols, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`
	return w.exec(q, args...)
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func str(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

func num(v any) int64 {
	n, _ := v.(int64)
	return n
}
