package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// issueCols is every column scanIssue reads, in order (alias i).
const issueCols = `i.id, i.area, i.title, i.description, i.design, i.acceptance, i.notes, i.spec_id, i.status,
	i.priority, i.issue_type, i.assignee, i.claimed_by, i.owner, i.estimated_minutes, i.created_at, i.created_by,
	i.updated_at, i.started_at, i.closed_at, i.close_reason, i.closed_by_session, i.due_at, i.defer_until,
	i.external_ref, i.source_system, i.metadata, i.pinned, i.is_template, i.ephemeral, i.is_blocked, i.parent,
	i.extra, i.version, coalesce(i.number, 0), i.created_by_addr`

type scanner interface{ Scan(...any) error }

func scanIssue(sc scanner, project string) (orchapi.Issue, error) {
	var it orchapi.Issue
	var est, started, closed, due, deferU sql.NullInt64
	var created, updated int64
	var meta, extra string
	var pinned, tmpl, eph, blocked int
	err := sc.Scan(&it.ID, &it.Area, &it.Title, &it.Description, &it.Design, &it.AcceptanceCriteria, &it.Notes, &it.SpecID,
		&it.Status, &it.Priority, &it.IssueType, &it.Assignee, &it.ClaimedBy, &it.Owner, &est, &created, &it.CreatedBy,
		&updated, &started, &closed, &it.CloseReason, &it.ClosedBySession, &due, &deferU, &it.ExternalRef,
		&it.SourceSystem, &meta, &pinned, &tmpl, &eph, &blocked, &it.Parent, &extra, &it.Version, &it.Number, &it.CreatedByAddress)
	if err != nil {
		return it, err
	}
	it.Project = project
	it.ZmStatus = orchapi.ZmStatus(it.Status)
	if est.Valid {
		n := int(est.Int64)
		it.EstimatedMinutes = &n
	}
	it.CreatedAt, it.UpdatedAt = fromMillis(created), fromMillis(updated)
	it.StartedAt, it.ClosedAt, it.DueAt, it.DeferUntil = nullTime(started), nullTime(closed), nullTime(due), nullTime(deferU)
	if meta != "" {
		it.Metadata = json.RawMessage(meta)
	}
	if extra != "" {
		_ = json.Unmarshal([]byte(extra), &it.Extra)
	}
	it.Pinned, it.IsTemplate, it.Ephemeral, it.Blocked = pinned != 0, tmpl != 0, eph != 0, blocked != 0
	return it, nil
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func nullMillis(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// areaWhere selects rows of alias.area visible in vis.
func areaWhere(vis istore.Areas, col string) (string, []any) {
	if vis.All {
		return "1=1", nil
	}
	if len(vis.Names) == 0 {
		return "0=1", nil
	}
	args := make([]any, len(vis.Names))
	for i, n := range vis.Names {
		args[i] = n
	}
	return col + " IN (" + placeholders(len(args)) + ")", args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func strArgs(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (p *project) Get(ctx context.Context, id string, vis istore.Areas) (orchapi.Issue, error) {
	it, err := getIssue(ctx, p.r, p.id, id, vis)
	if err != nil {
		return it, err
	}
	out := []orchapi.Issue{it}
	if err := enrich(ctx, p.r, p.id, out, vis, false); err != nil {
		return it, err
	}
	return out[0], nil
}

func getIssue(ctx context.Context, q queryer, project, id string, vis istore.Areas) (orchapi.Issue, error) {
	aw, args := areaWhere(vis, "i.area")
	it, err := scanIssue(q.QueryRowContext(ctx, `SELECT `+issueCols+` FROM issues i WHERE i.id = ? AND `+aw, append([]any{id}, args...)...), project)
	if errors.Is(err, sql.ErrNoRows) {
		return it, istore.ErrNotFound
	}
	return it, err
}

// enrich fills labels, counts (blocks edges to visible issues) and, with
// deps, each issue's own edges.
func enrich(ctx context.Context, q queryer, project string, its []orchapi.Issue, vis istore.Areas, deps bool) error {
	if len(its) == 0 {
		return nil
	}
	idx := make(map[string]int, len(its))
	ids := make([]any, len(its))
	for i := range its {
		idx[its[i].ID] = i
		ids[i] = its[i].ID
	}
	in := placeholders(len(ids))
	rows, err := q.QueryContext(ctx, `SELECT issue_id, label FROM labels WHERE issue_id IN (`+in+`) ORDER BY issue_id, label`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, l string
		if err := rows.Scan(&id, &l); err != nil {
			rows.Close()
			return err
		}
		its[idx[id]].Labels = append(its[idx[id]].Labels, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	aw, aargs := areaWhere(vis, "t.area")
	count := func(sqlText string, set func(i, n int)) error {
		rows, err := q.QueryContext(ctx, sqlText, append(append([]any{}, ids...), aargs...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			set(idx[id], n)
		}
		return rows.Err()
	}
	if err := count(`SELECT d.issue_id, count(*) FROM deps d JOIN issues t ON t.id = d.depends_on
		WHERE d.issue_id IN (`+in+`) AND d.type = 'blocks' AND `+aw+` GROUP BY d.issue_id`, func(i, n int) { its[i].DependencyCount = n }); err != nil {
		return err
	}
	if err := count(`SELECT d.depends_on, count(*) FROM deps d JOIN issues t ON t.id = d.issue_id
		WHERE d.depends_on IN (`+in+`) AND d.type = 'blocks' AND `+aw+` GROUP BY d.depends_on`, func(i, n int) { its[i].DependentCount = n }); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT issue_id, count(*) FROM comments WHERE issue_id IN (`+in+`) GROUP BY issue_id`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		its[idx[id]].CommentCount = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// A parent the caller cannot read is not shown.
	if !vis.All {
		var parents []any
		for i := range its {
			if its[i].Parent != "" {
				parents = append(parents, its[i].Parent)
			}
		}
		if len(parents) > 0 {
			areas := map[string]string{}
			rows, err := q.QueryContext(ctx, `SELECT id, area FROM issues WHERE id IN (`+placeholders(len(parents))+`)`, parents...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, a string
				if err := rows.Scan(&id, &a); err != nil {
					rows.Close()
					return err
				}
				areas[id] = a
			}
			rows.Close()
			for i := range its {
				if a, ok := areas[its[i].Parent]; its[i].Parent != "" && (!ok || !vis.Has(a)) {
					its[i].Parent = ""
				}
			}
		}
	}
	if !deps {
		return nil
	}
	rows, err = q.QueryContext(ctx, `SELECT d.issue_id, d.depends_on, d.type, d.created_at, d.created_by, d.metadata, d.thread_id, d.external
		FROM deps d LEFT JOIN issues t ON t.id = d.depends_on
		WHERE d.issue_id IN (`+in+`) AND (d.external = 1 OR (t.id IS NOT NULL AND `+aw+`)) ORDER BY d.issue_id, d.source_id = '', d.source_id, d.created_at, d.depends_on`, append(append([]any{}, ids...), aargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDep(rows)
		if err != nil {
			return err
		}
		its[idx[d.IssueID]].Dependencies = append(its[idx[d.IssueID]].Dependencies, d)
	}
	return rows.Err()
}

func scanDep(sc scanner) (orchapi.IssueDep, error) {
	var d orchapi.IssueDep
	var at int64
	var ext int
	err := sc.Scan(&d.IssueID, &d.DependsOnID, &d.Type, &at, &d.CreatedBy, &d.Metadata, &d.ThreadID, &ext)
	d.CreatedAt, d.External = fromMillis(at), ext != 0
	return d, err
}

func (p *project) Detail(ctx context.Context, id string, vis istore.Areas) (orchapi.IssueDetail, error) {
	var out orchapi.IssueDetail
	it, err := p.Get(ctx, id, vis)
	if err != nil {
		return out, err
	}
	out.Issue = it
	aw, aargs := areaWhere(vis, "i.area")
	links := func(sqlText string) ([]orchapi.IssueLink, error) {
		rows, err := p.r.QueryContext(ctx, sqlText, append([]any{id}, aargs...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ls := []orchapi.IssueLink{}
		for rows.Next() {
			var typ string
			var l orchapi.IssueLink
			it, err := scanIssueWith(rows, p.id, &typ)
			if err != nil {
				return nil, err
			}
			l.Issue, l.DependencyType = it, typ
			ls = append(ls, l)
		}
		return ls, rows.Err()
	}
	if out.DependsOn, err = links(`SELECT ` + issueCols + `, d.type FROM deps d JOIN issues i ON i.id = d.depends_on
		WHERE d.issue_id = ? AND ` + aw + ` ORDER BY d.source_id = '', d.source_id, d.created_at, i.id`); err != nil {
		return out, err
	}
	// External edges (beads' depends_on_external) have no issue row.
	rows, err := p.r.QueryContext(ctx, `SELECT depends_on, type, created_at FROM deps WHERE issue_id = ? AND external = 1 ORDER BY depends_on`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var target, typ string
		var at int64
		if err := rows.Scan(&target, &typ, &at); err != nil {
			rows.Close()
			return out, err
		}
		out.DependsOn = append(out.DependsOn, orchapi.IssueLink{Issue: orchapi.Issue{ID: target, Project: p.id, CreatedAt: fromMillis(at), UpdatedAt: fromMillis(at)}, DependencyType: typ})
	}
	rows.Close()
	if out.Dependents, err = links(`SELECT ` + issueCols + `, d.type FROM deps d JOIN issues i ON i.id = d.issue_id
		WHERE d.depends_on = ? AND ` + aw + ` ORDER BY d.source_id = '', d.source_id, d.created_at, i.id`); err != nil {
		return out, err
	}
	// Linked issues carry their labels (bd show does).
	all := make([]orchapi.Issue, 0, len(out.DependsOn)+len(out.Dependents))
	for _, l := range out.DependsOn {
		all = append(all, l.Issue)
	}
	for _, l := range out.Dependents {
		all = append(all, l.Issue)
	}
	if err := labelsOnly(ctx, p.r, all); err != nil {
		return out, err
	}
	for i := range out.DependsOn {
		out.DependsOn[i].Issue.Labels = all[i].Labels
	}
	for i := range out.Dependents {
		out.Dependents[i].Issue.Labels = all[len(out.DependsOn)+i].Labels
	}
	if out.Comments, err = p.comments(ctx, id); err != nil {
		return out, err
	}
	return out, nil
}

func scanIssueWith(rows *sql.Rows, project string, extra ...any) (orchapi.Issue, error) {
	return scanIssue(scanFunc(func(dst ...any) error { return rows.Scan(append(dst, extra...)...) }), project)
}

type scanFunc func(...any) error

func (f scanFunc) Scan(dst ...any) error { return f(dst...) }

func labelsOnly(ctx context.Context, q queryer, its []orchapi.Issue) error {
	if len(its) == 0 {
		return nil
	}
	idx := map[string][]int{}
	ids := []any{}
	for i := range its {
		if _, ok := idx[its[i].ID]; !ok {
			ids = append(ids, its[i].ID)
		}
		idx[its[i].ID] = append(idx[its[i].ID], i)
	}
	rows, err := q.QueryContext(ctx, `SELECT issue_id, label FROM labels WHERE issue_id IN (`+placeholders(len(ids))+`) ORDER BY issue_id, label`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, l string
		if err := rows.Scan(&id, &l); err != nil {
			return err
		}
		for _, i := range idx[id] {
			its[i].Labels = append(its[i].Labels, l)
		}
	}
	return rows.Err()
}

func (p *project) comments(ctx context.Context, id string) ([]orchapi.IssueComment, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT `+commentCols+` FROM comments WHERE issue_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.IssueComment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *project) SearchComments(ctx context.Context, q orchapi.CommentQuery, vis istore.Areas) ([]orchapi.IssueComment, error) {
	where, args := areaWhere(vis, "i.area")
	if q.Author != "" {
		where += " AND c.author_addr = ?"
		args = append(args, q.Author)
	}
	if q.Prefix != "" {
		where += ` AND c.text LIKE ? ESCAPE '\'`
		args = append(args, likeEscape(q.Prefix)+"%")
	}
	if q.Contains != "" {
		where += ` AND c.text LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscape(q.Contains)+"%")
	}
	if q.After != "" {
		where += " AND c.id > ?"
		args = append(args, q.After)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 1001:
		limit = 1001 // the service asks one more than its 1,000
	}
	rows, err := p.r.QueryContext(ctx, `SELECT `+commentColsC+` FROM comments c
		JOIN issues i ON i.id = c.issue_id WHERE c.deleted_at IS NULL AND `+where+` ORDER BY c.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.IssueComment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *project) Comments(ctx context.Context, id string, vis istore.Areas) ([]orchapi.IssueComment, error) {
	if _, err := getIssue(ctx, p.r, p.id, id, vis); err != nil {
		return nil, err
	}
	return p.comments(ctx, id)
}

const changeCols = `id, issue_id, kind, actor, actor_addr, fields, comment, area_before, area_after, created_at`

func scanChange(sc scanner) (orchapi.IssueChange, error) {
	var c orchapi.IssueChange
	var fields string
	var at int64
	err := sc.Scan(&c.ID, &c.IssueID, &c.Kind, &c.Actor, &c.ActorAddress, &fields, &c.Comment, &c.AreaBefore, &c.AreaAfter, &at)
	if err != nil {
		return c, err
	}
	c.At = fromMillis(at)
	if fields != "" {
		_ = json.Unmarshal([]byte(fields), &c.Fields)
	}
	return c, nil
}

func (p *project) History(ctx context.Context, id string, vis istore.Areas) ([]orchapi.IssueChange, error) {
	if _, err := getIssue(ctx, p.r, p.id, id, vis); err != nil {
		return nil, err
	}
	rows, err := p.r.QueryContext(ctx, `SELECT `+changeCols+` FROM history WHERE issue_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.IssueChange{}
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *project) Changes(ctx context.Context, since string, limit int) ([]orchapi.IssueChange, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := p.r.QueryContext(ctx, `SELECT `+changeCols+` FROM history WHERE id > ? ORDER BY id LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orchapi.IssueChange
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *project) Snapshot(ctx context.Context, fn func(orchapi.Issue) error) (string, error) {
	tx, err := p.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM history`).Scan(&last); err != nil {
		return "", err
	}
	labels := map[string][]string{}
	rows, err := tx.QueryContext(ctx, `SELECT issue_id, label FROM labels ORDER BY issue_id, label`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id, l string
		if err := rows.Scan(&id, &l); err != nil {
			rows.Close()
			return "", err
		}
		labels[id] = append(labels[id], l)
	}
	rows.Close()
	deps := map[string][]orchapi.IssueDep{}
	rows, err = tx.QueryContext(ctx, `SELECT issue_id, depends_on, type, created_at, created_by, metadata, thread_id, external FROM deps ORDER BY issue_id, depends_on`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		d, err := scanDep(rows)
		if err != nil {
			rows.Close()
			return "", err
		}
		deps[d.IssueID] = append(deps[d.IssueID], d)
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT `+issueCols+` FROM issues i ORDER BY i.id`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanIssue(rows, p.id)
		if err != nil {
			return "", err
		}
		it.Labels, it.Dependencies = labels[it.ID], deps[it.ID]
		if err := fn(it); err != nil {
			return "", err
		}
	}
	return last.String, rows.Err()
}

// ftsDrive is the match count above which a search scans the ordered
// index and tests membership, instead of fetching every match by rowid and
// sorting them.
const ftsDrive = 300

// ftsIn is the WHERE term restricting issues to an FTS match (one ? arg):
// driven by the matches when they are few, else a membership test (the
// unary + keeps the planner on the ordered index).
func (p *project) ftsIn(ctx context.Context, match string) string {
	var n int
	_ = p.r.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT rowid FROM issues_fts WHERE issues_fts MATCH ? LIMIT ?)`, match, ftsDrive).Scan(&n)
	if n >= ftsDrive {
		return "+i.rowid IN (SELECT rowid FROM issues_fts WHERE issues_fts MATCH ?)"
	}
	return "i.rowid IN (SELECT rowid FROM issues_fts WHERE issues_fts MATCH ?)"
}

// query runs one page: limit+1 rows (0 = no limit).
func (p *project) query(ctx context.Context, from string, where []string, args []any, order string, limit int) ([]orchapi.Issue, error) {
	sqlText := `SELECT ` + issueCols + ` FROM ` + from + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(append([]any{}, args...), limit+1)
	}
	rows, err := p.r.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	its := []orchapi.Issue{}
	for rows.Next() {
		it, err := scanIssue(rows, p.id)
		if err != nil {
			return nil, err
		}
		its = append(its, it)
	}
	return its, rows.Err()
}

// infraTypes are hidden from default lists and ready work (beads).
var infraTypes = []any{"gate", "agent", "rig", "role", "message"}
var readyHiddenTypes = []any{"merge-request", "gate", "molecule", "message", "agent", "role", "rig"}

func (p *project) List(ctx context.Context, q orchapi.IssueQuery, vis istore.Areas, now time.Time) (orchapi.IssueList, error) {
	var where []string
	var args []any
	add := func(clause string, a ...any) { where = append(where, clause); args = append(args, a...) }
	aw, aargs := areaWhere(vis, "i.area")
	add(aw, aargs...)
	add("i.is_template = 0")
	nowMs := millis(now)
	switch {
	case q.Ready:
		add("i.status = 'open' AND i.is_blocked = 0 AND i.pinned = 0 AND i.ephemeral = 0")
		if q.Type == "" {
			add("i.issue_type NOT IN ("+placeholders(len(readyHiddenTypes))+")", readyHiddenTypes...)
		}
		add("(i.defer_until IS NULL OR i.defer_until <= ?)", nowMs)
		add("NOT EXISTS (SELECT 1 FROM issues pp WHERE pp.id = i.parent AND (pp.status = 'deferred' OR pp.defer_until > ?))", nowMs)
	case q.Status == "all" || (q.All && q.Status == ""):
	case q.Status == "":
		add("i.status NOT IN ('closed', 'pinned') AND i.pinned = 0")
		if q.Type == "" {
			add("i.issue_type NOT IN ("+placeholders(len(infraTypes))+")", infraTypes...)
		}
	default:
		sts := splitComma(q.Status)
		add("i.status IN ("+placeholders(len(sts))+")", strArgs(sts)...)
	}
	if q.Number != nil {
		add("i.number = ?", *q.Number)
	}
	if q.IDAfter != "" {
		add("i.id > ?", q.IDAfter)
	}
	if q.Blocked {
		add("i.is_blocked = 1 AND i.status NOT IN ('closed', 'pinned')")
	}
	if q.Priority != nil {
		add("i.priority = ?", *q.Priority)
	}
	if q.Type != "" {
		add("i.issue_type = ?", q.Type)
	}
	if q.Assignee != "" {
		add("i.assignee = ?", q.Assignee)
	}
	if q.NoAssignee {
		add("i.assignee = ''")
	}
	for _, l := range q.Labels {
		add(labelHas, l)
	}
	if len(q.LabelAny) > 0 {
		add("EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label IN ("+placeholders(len(q.LabelAny))+"))", strArgs(q.LabelAny)...)
	}
	if q.Parent != "" {
		if q.Ready {
			add(`(i.id IN (WITH RECURSIVE sub(id) AS (SELECT id FROM issues WHERE parent = ? UNION SELECT c.id FROM issues c JOIN sub ON c.parent = sub.id) SELECT id FROM sub)
				OR (i.id LIKE ? ESCAPE '\' AND i.parent = ''))`, q.Parent, likeEscape(q.Parent)+".%")
		} else {
			add("i.parent = ?", q.Parent)
		}
	}
	if len(q.IDs) > 0 {
		add("i.id IN ("+placeholders(len(q.IDs))+")", strArgs(q.IDs)...)
	}
	for _, c := range []struct{ col, v string }{{"i.title", q.TitleContains}, {"i.description", q.DescContains}, {"i.notes", q.NotesContains}} {
		if c.v != "" {
			add("instr(lower("+c.col+"), lower(?)) > 0", c.v)
		}
	}
	if q.Substr != "" {
		// LIKE folds ASCII case; Go's Unicode lowercasing would change exact
		// non-ASCII text that SQLite's lower() leaves as written.
		pat := "%" + likeEscape(q.Substr) + "%"
		add(`(lower(i.title) LIKE ? ESCAPE '\' OR i.id LIKE ? ESCAPE '\' OR lower(i.external_ref) LIKE ? ESCAPE '\')`, pat, pat, pat)
	}
	if q.UpdatedAfter != nil {
		add("i.updated_at > ?", millis(*q.UpdatedAfter))
	}
	if q.UpdatedBefore != nil {
		add("i.updated_at < ?", millis(*q.UpdatedBefore))
	}
	if q.CreatedAfter != nil {
		add("i.created_at > ?", millis(*q.CreatedAfter))
	}
	if q.CreatedBefore != nil {
		add("i.created_at < ?", millis(*q.CreatedBefore))
	}
	if q.Areas != nil {
		if len(q.Areas) == 0 {
			add("0=1")
		} else {
			add("i.area IN ("+placeholders(len(q.Areas))+")", strArgs(q.Areas)...)
		}
	}
	if q.Contains != "" {
		add("(instr(lower(i.title), lower(?)) > 0 OR instr(lower(i.description), lower(?)) > 0)", q.Contains, q.Contains)
	}
	if q.CreatedByAddr != "" {
		add("i.created_by_addr = ?", q.CreatedByAddr)
	}
	if q.ClaimedBy != "" {
		add("i.claimed_by = ?", q.ClaimedBy)
	}
	keyCol := ""
	switch q.Sort {
	case "created":
		keyCol = "i.created_at"
	case "updated":
		keyCol = "i.updated_at"
	}
	if q.After != "" {
		ms, id, err := istore.DecodeKey(q.After)
		if err != nil || keyCol == "" {
			return orchapi.IssueList{}, api.Errorf(api.CodeBadRequest, "bad cursor (after needs sort created or updated and a next from that sort)")
		}
		op := "<"
		if q.Reverse {
			op = ">"
		}
		add("("+keyCol+" "+op+" ? OR ("+keyCol+" = ? AND i.id "+op+" ?))", ms, ms, id)
	}
	limit := q.Limit
	if limit <= 0 && !q.All {
		limit = 50
	}
	if q.Sort == "rank" {
		return p.ranked(ctx, q, where, args, limit, vis)
	}
	var its []orchapi.Issue
	var err error
	countWhere, countArgs := where, args
	if q.Q != "" {
		fts := ftsQuery(q.Q)
		if fts == "" {
			return orchapi.IssueList{Issues: []orchapi.Issue{}}, nil
		}
		title := "{title} : (" + fts + ")"
		in := p.ftsIn(ctx, fts)
		countWhere, countArgs = append([]string{in}, where...), append([]any{fts}, args...)
		if q.Sort == "" {
			// Relevance without ranking every match (bm25 over thousands of
			// long bodies costs tens of ms): title matches first, by rank
			// when there are few; then body-only matches in the default order.
			if tin := p.ftsIn(ctx, title); strings.HasPrefix(tin, "+") {
				its, err = p.query(ctx, "issues i", append([]string{tin}, where...), append([]any{title}, args...), orderBy("", q.Reverse, false), limit)
			} else {
				its, err = p.query(ctx, "issues_fts f JOIN issues i ON i.rowid = f.rowid", append([]string{"issues_fts MATCH ?"}, where...),
					append([]any{title}, args...), "f.rank, i.priority, i.id", limit)
			}
			if err == nil && (limit == 0 || len(its) <= limit) {
				seen := make([]any, 0, len(its))
				for _, it := range its {
					seen = append(seen, it.ID)
				}
				w2 := append([]string{in}, where...)
				a2 := append([]any{fts}, args...)
				if len(seen) > 0 {
					w2 = append(w2, "i.id NOT IN ("+placeholders(len(seen))+")")
					a2 = append(a2, seen...)
				}
				rest := 0
				if limit > 0 {
					rest = limit + 1 - len(its)
				}
				var body []orchapi.Issue
				body, err = p.query(ctx, "issues i", w2, a2, orderBy("", q.Reverse, false), rest)
				its = append(its, body...)
			}
		} else {
			its, err = p.query(ctx, "issues i", countWhere, countArgs, orderBy(q.Sort, q.Reverse, false), limit)
		}
	} else {
		its, err = p.query(ctx, "issues i", where, args, orderBy(q.Sort, q.Reverse, false), limit)
	}
	if err != nil {
		return orchapi.IssueList{}, err
	}
	whereSQL, from := strings.Join(countWhere, " AND "), "issues i"
	args = countArgs
	more := false
	if limit > 0 && len(its) > limit {
		its, more = its[:limit], true
	}
	total := len(its)
	if more && q.Count {
		if err := p.r.QueryRowContext(ctx, `SELECT count(*) FROM `+from+` WHERE `+whereSQL, args...).Scan(&total); err != nil {
			return orchapi.IssueList{}, err
		}
	}
	if err := enrich(ctx, p.r, p.id, its, vis, q.Deps); err != nil {
		return orchapi.IssueList{}, err
	}
	if err := enrichMore(ctx, p.r, its, vis, q.Incoming, q.Comments); err != nil {
		return orchapi.IssueList{}, err
	}
	out := orchapi.IssueList{Issues: its, More: more, Total: total}
	if more && keyCol != "" && len(its) > 0 {
		last := its[len(its)-1]
		t := last.CreatedAt
		if keyCol == "i.updated_at" {
			t = last.UpdatedAt
		}
		out.Next = istore.EncodeKey(millis(t), last.ID)
	}
	return out, nil
}

func splitComma(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ftsQuery turns words into an FTS5 query: every word must match, the last
// as a prefix too (search as you type). Quotes keep FTS syntax out.
func ftsQuery(q string) string {
	words := strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
	if len(words) == 0 {
		return ""
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + w + `"`
		if i == len(words)-1 {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, " AND ")
}

func orderBy(sort string, reverse, _ bool) string {
	type col struct {
		expr string
		desc bool
	}
	var cols []col
	switch sort {
	case "created": // keyset pages rely on the id breaking ties the same way
		cols = []col{{"i.created_at", true}, {"i.id", true}}
	case "updated":
		cols = []col{{"i.updated_at", true}, {"i.id", true}}
	case "closed":
		cols = []col{{"i.closed_at IS NULL", false}, {"i.closed_at", true}, {"i.id", false}}
	case "id":
		cols = []col{{"i.id", false}}
	case "number":
		cols = []col{{"i.number", false}}
	case "title":
		cols = []col{{"lower(i.title)", false}, {"i.id", false}}
	case "status":
		cols = []col{{"i.status", false}, {"i.priority", false}, {"i.id", false}}
	case "type":
		cols = []col{{"i.issue_type", false}, {"i.priority", false}, {"i.id", false}}
	case "assignee":
		cols = []col{{"i.assignee", false}, {"i.priority", false}, {"i.id", false}}
	case "oldest":
		cols = []col{{"i.created_at", false}, {"i.id", false}}
	case "", "priority":
		cols = []col{{"i.priority", false}, {"i.created_at", true}, {"i.id", false}}
	default:
		cols = []col{{"i.priority", false}, {"i.created_at", true}, {"i.id", false}}
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		d := c.desc != reverse
		parts[i] = c.expr
		if d {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

func (p *project) Stats(ctx context.Context, vis istore.Areas, now time.Time) (orchapi.IssueStats, error) {
	var s orchapi.IssueStats
	aw, args := areaWhere(vis, "i.area")
	rows, err := p.r.QueryContext(ctx, `SELECT i.status, count(*), sum(i.is_blocked) FROM issues i WHERE `+aw+` GROUP BY i.status`, args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n, b int
		if err := rows.Scan(&st, &n, &b); err != nil {
			return s, err
		}
		s.TotalIssues += n
		switch st {
		case orchapi.IssueOpen:
			s.OpenIssues = n
		case orchapi.IssueInProgress:
			s.InProgressIssues = n
		case orchapi.IssueClosed:
			s.ClosedIssues = n
		case orchapi.IssueDeferred:
			s.DeferredIssues = n
		case orchapi.IssuePinned:
			s.PinnedIssues = n
		}
		if st != orchapi.IssueClosed && st != orchapi.IssuePinned {
			s.BlockedIssues += b
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	// Ready is what `ready` lists (not open minus blocked: deferred issues
	// and children of deferred parents are not ready either).
	r, err := p.List(ctx, orchapi.IssueQuery{Ready: true, Limit: 1, Count: true}, vis, now)
	if err != nil {
		return s, err
	}
	s.ReadyIssues = r.Total
	return s, nil
}

func (p *project) Counts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, t := range []string{"issues", "comments", "deps", "labels", "history", "kv", "child_counters"} {
		var n int
		if err := p.r.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

func (p *project) Labels(ctx context.Context, vis istore.Areas, areas []string) ([]string, error) {
	aw, args := areaWhere(vis, "i.area")
	if areas != nil {
		if len(areas) == 0 {
			return []string{}, nil
		}
		aw += " AND i.area IN (" + placeholders(len(areas)) + ")"
		args = append(args, strArgs(areas)...)
	}
	rows, err := p.r.QueryContext(ctx, `SELECT DISTINCT l.label FROM labels l JOIN issues i ON i.id = l.issue_id WHERE `+aw+` ORDER BY l.label`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// enrichMore fills incoming edges (from issues in vis) and comments.
func enrichMore(ctx context.Context, q queryer, its []orchapi.Issue, vis istore.Areas, incoming, comments bool) error {
	if len(its) == 0 || (!incoming && !comments) {
		return nil
	}
	idx := make(map[string]int, len(its))
	ids := make([]any, len(its))
	for i := range its {
		idx[its[i].ID] = i
		ids[i] = its[i].ID
	}
	in := placeholders(len(ids))
	if incoming {
		aw, aargs := areaWhere(vis, "s.area")
		rows, err := q.QueryContext(ctx, `SELECT d.issue_id, d.depends_on, d.type, d.created_at, d.created_by, d.metadata, d.thread_id, d.external
			FROM deps d JOIN issues s ON s.id = d.issue_id WHERE d.depends_on IN (`+in+`) AND `+aw+`
			ORDER BY d.depends_on, d.source_id = '', d.source_id, d.created_at, d.issue_id`, append(append([]any{}, ids...), aargs...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			d, err := scanDep(rows)
			if err != nil {
				rows.Close()
				return err
			}
			its[idx[d.DependsOnID]].Incoming = append(its[idx[d.DependsOnID]].Incoming, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if comments {
		rows, err := q.QueryContext(ctx, `SELECT `+commentCols+` FROM comments WHERE issue_id IN (`+in+`) ORDER BY issue_id, created_at, id`, ids...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanComment(rows)
			if err != nil {
				return err
			}
			its[idx[c.IssueID]].Comments = append(its[idx[c.IssueID]].Comments, c)
		}
		return rows.Err()
	}
	return nil
}

func (p *project) LastChange(ctx context.Context) (string, error) {
	var last sql.NullString
	err := p.r.QueryRowContext(ctx, `SELECT max(id) FROM history`).Scan(&last)
	return last.String, err
}

// commentCols are a comment row's columns, as scanComment reads them.
const (
	commentCols  = `id, issue_id, author, author_addr, text, created_at, edited_at, deleted_at, deleted_by`
	commentColsC = `c.id, c.issue_id, c.author, c.author_addr, c.text, c.created_at, c.edited_at, c.deleted_at, c.deleted_by`
)

func scanComment(sc scanner) (orchapi.IssueComment, error) {
	var c orchapi.IssueComment
	var at int64
	var edited, deleted sql.NullInt64
	if err := sc.Scan(&c.ID, &c.IssueID, &c.Author, &c.AuthorAddress, &c.Text, &at, &edited, &deleted, &c.DeletedBy); err != nil {
		return c, err
	}
	c.CreatedAt = fromMillis(at)
	if edited.Valid {
		t := fromMillis(edited.Int64)
		c.EditedAt = &t
	}
	if deleted.Valid {
		t := fromMillis(deleted.Int64)
		c.Deleted, c.DeletedAt = true, &t
	}
	return c, nil
}

// Compact implements ProjectStore: VACUUM on the writer, then fold the WAL
// in and truncate it (a redaction's old text lives in free pages and in
// WAL frames until both are gone).
func (p *project) Compact(ctx context.Context) error {
	if _, err := p.w.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return truncateWAL(ctx, p.ck)
}

// truncateWAL folds the WAL in and truncates it; a reader in the way
// (busy) is an error, not silence.
func truncateWAL(ctx context.Context, db *sql.DB) error {
	var busy, logFrames, done int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint busy (%d of %d frames folded)", done, logFrames)
	}
	return nil
}
