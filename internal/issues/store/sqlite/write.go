package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// Limits.
const (
	maxTitle = 500
	maxText  = 1 << 20 // description, notes, comments
	maxLabel = 255
)

// Apply implements istore.ProjectStore: one write, one IMMEDIATE transaction
// on the one writer connection, so history ids are minted in commit order.
func (p *project) Apply(ctx context.Context, m istore.Mutation) (istore.Result, error) {
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if m.AreaOf == nil {
		m.AreaOf = func(string, []string) string { return "" }
	}
	if m.Authorize == nil {
		m.Authorize = func(string, string) error { return nil }
	}
	if m.CanRead == nil {
		m.CanRead = func(string) bool { return true }
	}
	var res istore.Result
	err := zmstore.Tx(ctx, p.w, func(tx *sql.Tx) error {
		w := &writer{ctx: ctx, tx: tx, p: p, m: m}
		var err error
		switch m.Op {
		case istore.OpCreate:
			err = w.create()
		case istore.OpUpdate:
			err = w.update()
		case istore.OpClaim:
			err = w.claim()
		case istore.OpClose:
			err = w.close()
		case istore.OpReopen:
			err = w.reopen()
		case istore.OpComment:
			err = w.comment()
		case istore.OpCommentEdit:
			err = w.commentEdit()
		case istore.OpCommentDelete:
			err = w.commentDelete()
		case istore.OpCommentRedact:
			err = w.commentRedact()
		case istore.OpDepAdd:
			err = w.depAdd()
		case istore.OpDepRemove:
			err = w.depRemove()
		case istore.OpLabelAdd, istore.OpLabelRemove:
			err = w.label()
		case istore.OpMoveIn, istore.OpMoveOut, istore.OpMoveDone:
			err = w.move()
		default:
			err = w.more() // ops kept outside this file (attachments.go)
		}
		if err != nil {
			return err
		}
		if w.id == "" {
			return nil
		}
		if res.Issue, err = w.issue(); err != nil {
			return err
		}
		res.Changes, res.Comment, res.Noop, res.Redacted = w.changes, w.cmt, len(w.changes) == 0, w.redacted
		for _, id := range w.touched {
			if id == w.id {
				continue
			}
			it, err := w.load(id)
			if err != nil {
				return err
			}
			res.Touched = append(res.Touched, it)
		}
		return nil
	})
	return res, err
}

type writer struct {
	ctx      context.Context
	tx       *sql.Tx
	p        *project
	m        istore.Mutation
	id       string // the issue the result reports
	changes  []orchapi.IssueChange
	cmt      *orchapi.IssueComment
	touched  []string
	redacted []string       // the texts commentRedact overwrote
	gone     *orchapi.Issue // the issue a move out removed
}

// row is an issue as a write sees it: every column, labels, home area.
type row struct {
	orchapi.Issue
	homeArea string
	versions map[string]string
}

func (w *writer) load(id string) (orchapi.Issue, error) {
	it, err := getIssue(w.ctx, w.tx, w.p.id, id, istore.Areas{All: true})
	if err != nil {
		return it, err
	}
	out := []orchapi.Issue{it}
	if err := enrich(w.ctx, w.tx, w.p.id, out, istore.Areas{All: true}, false); err != nil {
		return it, err
	}
	return out[0], nil
}

func (w *writer) loadRow(id string) (*row, error) {
	it, err := w.load(id)
	if err != nil {
		return nil, err
	}
	r := &row{Issue: it}
	var vs string
	if err := w.tx.QueryRowContext(w.ctx, `SELECT home_area, versions FROM issues WHERE id = ?`, id).Scan(&r.homeArea, &vs); err != nil {
		return nil, err
	}
	r.versions = map[string]string{}
	if vs != "" {
		_ = json.Unmarshal([]byte(vs), &r.versions)
	}
	return r, nil
}

// areaOf is the area rules' answer, else the area the issue was filed in.
func (w *writer) areaOf(id, home string, labels []string) string {
	if a := w.m.AreaOf(id, labels); a != "" {
		return a
	}
	return home
}

// target loads an issue the write references (dependency target, parent):
// missing and unreadable answer the same not_found.
func (w *writer) target(id string) (*row, error) {
	r, err := w.loadRow(id)
	if errors.Is(err, istore.ErrNotFound) || (err == nil && !w.m.CanRead(r.Area)) {
		return nil, api.NotFound("issue", id)
	}
	return r, err
}

type fieldDiff map[string][2]any

func (d fieldDiff) set(field string, old, new any) {
	if fmt.Sprint(old) == fmt.Sprint(new) {
		return
	}
	d[field] = [2]any{old, new}
}

// record appends a history row and stamps the issue's version (and the
// changed fields' versions, which Base checks against).
func (w *writer) record(id, kind string, fields fieldDiff, comment, before, after string) error {
	cid := ids.New()
	var fj string
	if len(fields) > 0 {
		raw := map[string]json.RawMessage{}
		for k, v := range fields {
			b, _ := json.Marshal([]any{v[0], v[1]})
			raw[k] = b
		}
		b, _ := json.Marshal(raw)
		fj = string(b)
	}
	at := millis(w.m.At)
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO history (id, issue_id, kind, actor, actor_addr, fields, comment, area_before, area_after, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, cid, id, kind, w.m.Actor.Name, w.m.Actor.Address, fj, comment, before, after, at); err != nil {
		return err
	}
	var vs string
	if err := w.tx.QueryRowContext(w.ctx, `SELECT versions FROM issues WHERE id = ?`, id).Scan(&vs); err != nil {
		return err
	}
	versions := map[string]string{}
	if vs != "" {
		_ = json.Unmarshal([]byte(vs), &versions)
	}
	for k := range fields {
		versions[k] = cid
	}
	vb, _ := json.Marshal(versions)
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET version = ?, versions = ? WHERE id = ?`, cid, string(vb), id); err != nil {
		return err
	}
	ch := orchapi.IssueChange{ID: cid, IssueID: id, Kind: kind, Actor: w.m.Actor.Name, ActorAddress: w.m.Actor.Address, Comment: comment,
		AreaBefore: before, AreaAfter: after, At: fromMillis(at)}
	if fj != "" {
		_ = json.Unmarshal([]byte(fj), &ch.Fields)
	}
	w.changes = append(w.changes, ch)
	return nil
}

// ---- validation ----

func cleanTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", bad("title required")
	}
	if len(s) > maxTitle || strings.ContainsAny(s, "\n\r") {
		return "", bad("a title is one line of at most %d bytes", maxTitle)
	}
	return s, nil
}

func checkText(field, s string) error {
	if len(s) > maxText {
		return bad("%s is longer than %d bytes", field, maxText)
	}
	return nil
}

func checkPriority(p int) error {
	if p < 0 || p > 4 {
		return bad("invalid priority %d (expected 0-4)", p)
	}
	return nil
}

// typeAliases are beads' issue type spellings.
var typeAliases = map[string]string{"enhancement": "feature", "feat": "feature", "dec": "decision", "adr": "decision",
	"investigation": "spike", "timebox": "spike", "user-story": "story", "ms": "milestone"}

// NormalizeType trims, lowercases and maps beads' aliases.
func NormalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if a, ok := typeAliases[t]; ok {
		return a
	}
	return t
}

func checkType(t string) error {
	if t == "" || len(t) > 32 || strings.ContainsAny(t, " \t\n,") {
		return bad("invalid issue type %q", t)
	}
	return nil
}

func checkStatus(s string) error {
	if !orchapi.ValidIssueStatus(s) {
		return bad("invalid status %q (one of %s)", s, strings.Join(orchapi.IssueStatuses, ", "))
	}
	return nil
}

func cleanLabels(ls []string) ([]string, error) {
	out := []string{}
	for _, l := range ls {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if len(l) > maxLabel || strings.ContainsAny(l, "\n\r") {
			return nil, bad("invalid label %q", l)
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

func checkMeta(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", bad("metadata must be a JSON object")
	}
	if len(m) == 0 {
		return "", nil
	}
	b, _ := json.Marshal(m)
	return string(b), nil
}

// ---- create ----

func (w *writer) create() error {
	req := w.m.Create
	if req == nil {
		return bad("create needs a body")
	}
	title, err := cleanTitle(req.Title)
	if err != nil {
		return err
	}
	for f, v := range map[string]string{"description": req.Description, "design": req.Design, "acceptance criteria": req.AcceptanceCriteria, "notes": req.Notes} {
		if err := checkText(f, v); err != nil {
			return err
		}
	}
	prio := 2
	if req.Priority != nil {
		prio = *req.Priority
	}
	if err := checkPriority(prio); err != nil {
		return err
	}
	typ := NormalizeType(req.IssueType)
	if typ == "" {
		typ = "task"
	}
	if err := checkType(typ); err != nil {
		return err
	}
	status := req.Status
	if status == "" {
		status = orchapi.IssueOpen
		if req.DeferUntil != nil && req.DeferUntil.After(w.m.At) {
			status = orchapi.IssueDeferred
		}
	}
	if err := checkStatus(status); err != nil {
		return err
	}
	labels, err := cleanLabels(req.Labels)
	if err != nil {
		return err
	}
	meta, err := checkMeta(req.Metadata)
	if err != nil {
		return err
	}
	prefix, err := w.prefix()
	if err != nil {
		return err
	}
	var parent *row
	if req.Parent != "" {
		if req.ID != "" {
			return bad("--id and --parent cannot be combined")
		}
		if parent, err = w.target(req.Parent); err != nil {
			return err
		}
		if !req.NoInheritLabels {
			if labels, err = cleanLabels(append(labels, parent.Labels...)); err != nil {
				return err
			}
		}
	}
	id := strings.TrimSpace(req.ID)
	switch {
	case id != "":
		if !strings.HasPrefix(id, prefix+"-") || len(id) <= len(prefix)+1 || len(id) > 255 || strings.ContainsAny(id, " \t\n/") {
			return bad("id %q must start with %s-", id, prefix)
		}
		var n int
		if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM issues WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return conflict("issue %s already exists", id)
		}
	case parent != nil:
		if id, err = w.childID(parent.ID); err != nil {
			return err
		}
	default:
		if id, err = w.hashID(prefix, title, req.Description); err != nil {
			return err
		}
	}
	area := w.areaOf(id, req.Area, labels)
	if err := w.m.Authorize(area, area); err != nil {
		return err
	}
	at := millis(w.m.At)
	var started any
	if status == orchapi.IssueInProgress {
		started = at
	}
	var closed any
	if status == orchapi.IssueClosed {
		closed = at
	}
	var est any
	if req.EstimatedMinutes != nil {
		est = *req.EstimatedMinutes
	}
	parentID := ""
	if parent != nil {
		parentID = parent.ID
	}
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO issues (id, area, home_area, title, description, design, acceptance, notes, spec_id,
		status, priority, issue_type, assignee, owner, estimated_minutes, created_at, created_by, created_by_addr, updated_at,
		started_at, closed_at, due_at, defer_until, external_ref, metadata, parent, number)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		(SELECT coalesce(max(number), 0) + 1 FROM issues))`,
		id, area, req.Area, title, req.Description, req.Design, req.AcceptanceCriteria, req.Notes, req.SpecID,
		status, prio, typ, strings.TrimSpace(req.Assignee), req.Owner, est, at, w.m.Actor.Name, w.m.Actor.Address, at,
		started, closed, nullMillis(req.DueAt), nullMillis(req.DeferUntil), req.ExternalRef, meta, parentID); err != nil {
		return err
	}
	for _, l := range labels {
		if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO labels (issue_id, label) VALUES (?, ?)`, id, l); err != nil {
			return err
		}
	}
	w.id = id
	if err := w.record(id, "created", fieldDiff{"title": {nil, title}, "status": {nil, status}, "priority": {nil, prio}, "issue_type": {nil, typ}}, "", area, area); err != nil {
		return err
	}
	seeds := []string{id}
	if parent != nil {
		if err := w.insertDep(id, parent.ID, orchapi.DepParentChild, false); err != nil {
			return err
		}
	}
	for _, d := range req.Deps {
		typ, tid, ok := strings.Cut(strings.TrimSpace(d), ":")
		if !ok {
			typ, tid = orchapi.DepBlocks, d
		}
		tid = strings.TrimSpace(tid)
		switch typ {
		case "depends-on", "blocked-by":
			typ = orchapi.DepBlocks
		}
		if typ == "external" {
			if err := w.insertDep(id, d, orchapi.DepBlocks, true); err != nil {
				return err
			}
			continue
		}
		t, err := w.target(tid)
		if err != nil {
			return err
		}
		if typ == orchapi.DepBlocks && strings.HasPrefix(strings.TrimSpace(d), "blocks:") {
			if err := w.insertDep(t.ID, id, orchapi.DepBlocks, false); err != nil {
				return err
			}
			seeds = append(seeds, t.ID)
			continue
		}
		if typ == orchapi.DepParentChild {
			return bad("use --parent for a parent")
		}
		if err := w.insertDep(id, t.ID, typ, false); err != nil {
			return err
		}
	}
	return w.refreshBlocked(seeds)
}

func (w *writer) prefix() (string, error) {
	var v string
	err := w.tx.QueryRowContext(w.ctx, `SELECT value FROM meta WHERE key = 'prefix'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || v == "" {
		return "", bad("this project has no issue prefix yet")
	}
	return v, err
}

func (w *writer) insertDep(from, to, typ string, external bool) error {
	ext := 0
	if external {
		ext = 1
	}
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO deps (issue_id, depends_on, type, external, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		from, to, typ, ext, millis(w.m.At), w.m.Actor.Name)
	if zmstore.IsUnique(err) {
		return conflict("dependency %s -> %s already exists", from, to)
	}
	if err == nil && typ == orchapi.DepParentChild {
		_, err = w.tx.ExecContext(w.ctx, `UPDATE issues SET parent = ? WHERE id = ?`, to, from)
	}
	return err
}

// hashID mints a new top-level id: <prefix>-<handle> (handle.go). The name
// is beads' (its ids were hashes); the handle is random now. The length
// grows with the number of top-level issues, and a hit is drawn again.
func (w *writer) hashID(prefix, title, desc string) (string, error) {
	return w.hashIDWith(prefix, newHandle)
}

func (w *writer) hashIDWith(prefix string, draw func(int) string) (string, error) {
	var n int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM issues WHERE instr(id, '.') = 0`).Scan(&n); err != nil {
		return "", err
	}
	for l := handleLen(n); l <= 8; l++ {
		for try := 0; try < 10; try++ {
			id := prefix + "-" + draw(l)
			var c int
			if err := w.tx.QueryRowContext(w.ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?) OR EXISTS(SELECT 1 FROM moves WHERE dir = 'out' AND old_id = ?)`, id, id).Scan(&c); err != nil {
				return "", err
			}
			if c == 0 {
				return id, nil
			}
		}
	}
	return "", conflict("could not mint a free issue id")
}

// childID is <parent>.<N>: one past the larger of the parent's counter and
// its highest existing direct child; numbers are never reused.
func (w *writer) childID(parent string) (string, error) {
	last := 0
	var c sql.NullInt64
	if err := w.tx.QueryRowContext(w.ctx, `SELECT last_child FROM child_counters WHERE parent = ?`, parent).Scan(&c); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	last = int(c.Int64)
	// A range on the primary key ('/' sorts right after '.').
	rows, err := w.tx.QueryContext(w.ctx, `SELECT id FROM issues WHERE id > ? AND id < ?`, parent+".", parent+"/")
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(id, parent+".")); err == nil && n > last {
			last = n
		}
	}
	rows.Close()
	next := last + 1
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO child_counters (parent, last_child) VALUES (?, ?)
		ON CONFLICT (parent) DO UPDATE SET last_child = excluded.last_child`, parent, next); err != nil {
		return "", err
	}
	return parent + "." + strconv.Itoa(next), nil
}

// ---- update ----

func timeStr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (w *writer) update() error {
	req := w.m.Update
	if req == nil {
		return bad("update needs a body")
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	if w.m.Check != nil {
		if err := w.m.Check(cur.Issue); err != nil {
			return err
		}
	}
	nw := cur.Issue
	d := fieldDiff{}
	text := func(field string, dst *string, v *string) error {
		if v == nil {
			return nil
		}
		if err := checkText(field, *v); err != nil {
			return err
		}
		d.set(field, *dst, *v)
		*dst = *v
		return nil
	}
	if req.Title != nil {
		t, err := cleanTitle(*req.Title)
		if err != nil {
			return err
		}
		d.set("title", nw.Title, t)
		nw.Title = t
	}
	for _, f := range []struct {
		name string
		dst  *string
		v    *string
	}{{"description", &nw.Description, req.Description}, {"design", &nw.Design, req.Design},
		{"acceptance_criteria", &nw.AcceptanceCriteria, req.AcceptanceCriteria}, {"spec_id", &nw.SpecID, req.SpecID},
		{"external_ref", &nw.ExternalRef, req.ExternalRef}} {
		if err := text(f.name, f.dst, f.v); err != nil {
			return err
		}
	}
	if req.Notes != nil && req.AppendNotes != nil {
		return bad("--notes and --append-notes cannot be combined")
	}
	if err := text("notes", &nw.Notes, req.Notes); err != nil {
		return err
	}
	if req.AppendNotes != nil {
		v := *req.AppendNotes
		if nw.Notes != "" {
			v = nw.Notes + "\n" + v
		}
		if err := text("notes", &nw.Notes, &v); err != nil {
			return err
		}
	}
	if req.Assignee != nil {
		a := strings.TrimSpace(*req.Assignee)
		d.set("assignee", nw.Assignee, a)
		if a != nw.Assignee {
			nw.ClaimedBy = "" // a hand-set assignee is not a zm claim
		}
		nw.Assignee = a
	}
	if req.Priority != nil {
		if err := checkPriority(*req.Priority); err != nil {
			return err
		}
		d.set("priority", nw.Priority, *req.Priority)
		nw.Priority = *req.Priority
	}
	if req.IssueType != nil {
		t := NormalizeType(*req.IssueType)
		if err := checkType(t); err != nil {
			return err
		}
		d.set("issue_type", nw.IssueType, t)
		nw.IssueType = t
	}
	if req.EstimatedMinutes != nil {
		d.set("estimated_minutes", intOrNil(nw.EstimatedMinutes), *req.EstimatedMinutes)
		v := *req.EstimatedMinutes
		nw.EstimatedMinutes = &v
	}
	if req.DueAt != nil || req.ClearDue {
		var v *time.Time
		if !req.ClearDue {
			v = req.DueAt
		}
		d.set("due_at", timeStr(nw.DueAt), timeStr(v))
		nw.DueAt = v
	}
	status := nw.Status
	if req.Status != nil {
		if err := checkStatus(*req.Status); err != nil {
			return err
		}
		status = *req.Status
	}
	if req.DeferUntil != nil || req.ClearDefer {
		var v *time.Time
		if !req.ClearDefer {
			v = req.DeferUntil
		}
		d.set("defer_until", timeStr(nw.DeferUntil), timeStr(v))
		nw.DeferUntil = v
		if req.Status == nil {
			if v != nil && v.After(w.m.At) {
				status = orchapi.IssueDeferred
			} else if v == nil && nw.Status == orchapi.IssueDeferred {
				status = orchapi.IssueOpen
			}
		}
	}
	if len(req.Metadata) > 0 {
		var add map[string]any
		if err := json.Unmarshal(req.Metadata, &add); err != nil {
			return bad("metadata must be a JSON object")
		}
		m := map[string]any{}
		if len(nw.Metadata) > 0 {
			_ = json.Unmarshal(nw.Metadata, &m)
		}
		for k, v := range add {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		var s string
		if len(m) > 0 {
			b, _ := json.Marshal(m)
			s = string(b)
		}
		d.set("metadata", string(nw.Metadata), s)
		nw.Metadata = json.RawMessage(s)
	}
	w.statusChange(&nw, cur.Status, status, req.Session, d)
	labels, err := w.newLabels(cur.Labels, req.SetLabels, req.AddLabels, req.RemoveLabels)
	if err != nil {
		return err
	}
	if strings.Join(labels, "\x00") != strings.Join(cur.Labels, "\x00") {
		d.set("labels", strings.Join(cur.Labels, ","), strings.Join(labels, ","))
	}
	var newParent *row
	parentChanged := false
	if req.Parent != nil && strings.TrimSpace(*req.Parent) != cur.Parent {
		parentChanged = true
		if p := strings.TrimSpace(*req.Parent); p != "" {
			if newParent, err = w.target(p); err != nil {
				return err
			}
			if err := w.checkParentCycle(cur.ID, newParent.ID); err != nil {
				return err
			}
		}
		np := ""
		if newParent != nil {
			np = newParent.ID
		}
		d.set("parent", cur.Parent, np)
	}
	after := w.areaOf(cur.ID, cur.homeArea, labels)
	if after != cur.Area {
		if err := w.m.Authorize(cur.Area, after); err != nil {
			return err
		}
	}
	w.id = cur.ID
	if len(d) == 0 && after == cur.Area {
		return nil
	}
	if req.Base != "" {
		for f := range d {
			if v := cur.versions[f]; v > req.Base {
				return api.Errorf(api.CodeConflict, "%s changed after your copy (%s > %s)", f, v, req.Base).WithData(map[string]string{"field": f, "version": v})
			}
		}
	}
	if err := w.writeRow(&nw, after); err != nil {
		return err
	}
	if err := w.writeLabels(cur.ID, cur.Labels, labels); err != nil {
		return err
	}
	if parentChanged {
		if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM deps WHERE issue_id = ? AND type = 'parent-child'`, cur.ID); err != nil {
			return err
		}
		if _, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET parent = '' WHERE id = ?`, cur.ID); err != nil {
			return err
		}
		if newParent != nil {
			if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM deps WHERE issue_id = ? AND depends_on = ?`, cur.ID, newParent.ID); err != nil {
				return err
			}
			if err := w.insertDep(cur.ID, newParent.ID, orchapi.DepParentChild, false); err != nil {
				return err
			}
		}
	}
	if err := w.record(cur.ID, updateKind(d, cur.Status, nw.Status), d, "", cur.Area, after); err != nil {
		return err
	}
	if _, ok := d["status"]; ok || parentChanged {
		return w.refreshBlocked(w.statusSeeds(cur.ID))
	}
	return nil
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func updateKind(d fieldDiff, from, to string) string {
	if _, ok := d["status"]; ok {
		switch {
		case to == orchapi.IssueClosed:
			return "closed"
		case from == orchapi.IssueClosed:
			return "reopened"
		}
		return "status_changed"
	}
	if _, ok := d["labels"]; ok && len(d) == 1 {
		return "labels_changed"
	}
	return "updated"
}

func (w *writer) writeLabels(id string, old, nw []string) error {
	for _, l := range old {
		if !slices.Contains(nw, l) {
			if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, id, l); err != nil {
				return err
			}
		}
	}
	for _, l := range nw {
		if !slices.Contains(old, l) {
			if _, err := w.tx.ExecContext(w.ctx, `INSERT OR IGNORE INTO labels (issue_id, label) VALUES (?, ?)`, id, l); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeRow writes every mutable column of it (and the new area).
func (w *writer) writeRow(it *orchapi.Issue, area string) error {
	var meta string
	if len(it.Metadata) > 0 {
		meta = string(it.Metadata)
	}
	_, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET area = ?, title = ?, description = ?, design = ?, acceptance = ?, notes = ?,
		spec_id = ?, status = ?, priority = ?, issue_type = ?, assignee = ?, claimed_by = ?, estimated_minutes = ?, updated_at = ?,
		started_at = ?, closed_at = ?, close_reason = ?, closed_by_session = ?, due_at = ?, defer_until = ?, external_ref = ?,
		metadata = ? WHERE id = ?`,
		area, it.Title, it.Description, it.Design, it.AcceptanceCriteria, it.Notes, it.SpecID, it.Status, it.Priority, it.IssueType,
		it.Assignee, it.ClaimedBy, intOrNil(it.EstimatedMinutes), millis(w.m.At), nullMillis(it.StartedAt), nullMillis(it.ClosedAt),
		it.CloseReason, it.ClosedBySession, nullMillis(it.DueAt), nullMillis(it.DeferUntil), it.ExternalRef, meta, it.ID)
	return err
}

func (w *writer) checkParentCycle(child, parent string) error {
	seen := map[string]bool{}
	for cur := parent; cur != ""; {
		if cur == child {
			return bad("%s cannot be its own ancestor", child)
		}
		if seen[cur] {
			return nil
		}
		seen[cur] = true
		var next string
		if err := w.tx.QueryRowContext(w.ctx, `SELECT parent FROM issues WHERE id = ?`, cur).Scan(&next); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		cur = next
	}
	return nil
}

// ---- claim, close, reopen ----

func (w *writer) claim() error {
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	me := w.m.Actor
	if w.m.Assignee != "" {
		me.Name = w.m.Assignee // the claim's routing label (bd's actor)
	}
	// On an open issue an old claimed_by is no claim: the assignee decides, as in bd
	// (the claimer's own address counts: bd create -a <session>).
	mine := (cur.ClaimedBy == "" || cur.ClaimedBy == me.Address || cur.Status == orchapi.IssueOpen) &&
		(cur.Assignee == "" || cur.Assignee == me.Name || cur.Assignee == me.Address)
	switch {
	case (cur.Status == orchapi.IssueInProgress || cur.Status == orchapi.IssueHooked) && cur.ClaimedBy != "" && cur.ClaimedBy == me.Address:
		// Already this address's claim: idempotent, whatever name it was made
		// under (a team write-back repeats a claim the door made as the
		// claimer, possibly as the system). Under another label it is someone
		// else's claim, as bd refused a second actor (sessions may share an
		// address: scripts on the socket).
		// Its own unlabelled claim (zm work claim, MCP) takes the label; so does
		// one under a person's name, by a label naming it or a lane under it.
		if w.m.Assignee == "" || cur.Assignee == w.m.Assignee {
			return nil
		}
		if cur.Assignee != "" && cur.Assignee != w.m.Actor.Name && !api.LabelAdopts(cur.Assignee, w.m.Assignee, me.Address) {
			return conflict("issue already claimed by %s", cur.Assignee)
		}
		nw := cur.Issue
		d := fieldDiff{}
		d.set("assignee", nw.Assignee, w.m.Assignee)
		nw.Assignee = w.m.Assignee
		if err := w.writeRow(&nw, cur.Area); err != nil {
			return err
		}
		return w.record(cur.ID, "claimed", d, "", cur.Area, cur.Area)
	case cur.Status == orchapi.IssueInProgress && cur.Assignee == me.Name && (cur.ClaimedBy == "" || cur.ClaimedBy == me.Address):
		if cur.ClaimedBy == me.Address {
			return nil // already ours: idempotent
		}
	case cur.Status != orchapi.IssueOpen:
		if cur.Assignee != "" && cur.Assignee != me.Name {
			return conflict("issue already claimed by %s", cur.Assignee)
		}
		return conflict("issue not claimable: status %s", cur.Status)
	case !mine:
		who := cur.Assignee
		if who == "" || who == me.Name {
			who = "another session"
		}
		return conflict("issue already claimed by %s", who)
	}
	nw := cur.Issue
	d := fieldDiff{}
	d.set("assignee", nw.Assignee, me.Name)
	d.set("claimed_by", nw.ClaimedBy, me.Address)
	nw.Assignee, nw.ClaimedBy = me.Name, me.Address
	w.statusChange(&nw, cur.Status, orchapi.IssueInProgress, "", d)
	if err := w.writeRow(&nw, cur.Area); err != nil {
		return err
	}
	return w.record(cur.ID, "claimed", d, "", cur.Area, cur.Area)
}

func (w *writer) close() error {
	req := w.m.Close
	if req == nil {
		req = &orchapi.CloseIssueRequest{}
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	if cur.Status == orchapi.IssueClosed {
		return nil
	}
	if !req.Force {
		if cur.Pinned || cur.IsTemplate {
			return conflict("cannot close %s: it is pinned or a template (use --force to override)", cur.ID)
		}
		var open int
		if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM deps d JOIN issues c ON c.id = d.issue_id
			WHERE d.depends_on = ? AND d.type = 'parent-child' AND c.status != 'closed'`, cur.ID).Scan(&open); err != nil {
			return err
		}
		if cur.IssueType == "epic" && open > 0 {
			return conflict("cannot close epic %s: %d open child issue(s); close children first or use --force to override", cur.ID, open)
		}
		blockers, err := w.openBlockers(cur.ID)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			// Name only the blockers the caller may see.
			var seen []string
			for _, b := range blockers {
				if w.m.CanRead(b.area) {
					seen = append(seen, b.id)
				}
			}
			msg := "[" + strings.Join(seen, " ") + "]"
			if n := len(blockers) - len(seen); n > 0 {
				msg += fmt.Sprintf(" and %d you cannot see", n)
			}
			return conflict("cannot close %s: blocked by open issues %s (use --force to override)", cur.ID, msg)
		}
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "Closed"
	}
	if err := checkText("reason", reason); err != nil {
		return err
	}
	nw := cur.Issue
	d := fieldDiff{}
	w.statusChange(&nw, cur.Status, orchapi.IssueClosed, req.Session, d)
	d.set("close_reason", nw.CloseReason, reason)
	nw.CloseReason = reason
	if err := w.writeRow(&nw, cur.Area); err != nil {
		return err
	}
	if err := w.record(cur.ID, "closed", d, reason, cur.Area, cur.Area); err != nil {
		return err
	}
	return w.refreshBlocked(w.statusSeeds(cur.ID))
}

type blocker struct{ id, area string }

// openBlockers are the open issues id waits on (blocks edges).
func (w *writer) openBlockers(id string) ([]blocker, error) {
	rows, err := w.tx.QueryContext(w.ctx, `SELECT t.id, t.area FROM deps d JOIN issues t ON t.id = d.depends_on
		WHERE d.issue_id = ? AND d.type IN ('blocks', 'conditional-blocks') AND t.status NOT IN ('closed', 'pinned') ORDER BY t.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blocker
	for rows.Next() {
		var b blocker
		if err := rows.Scan(&b.id, &b.area); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (w *writer) reopen() error {
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	if cur.Status == orchapi.IssueOpen {
		return nil
	}
	nw := cur.Issue
	d := fieldDiff{}
	w.statusChange(&nw, cur.Status, orchapi.IssueOpen, "", d)
	if nw.DeferUntil != nil {
		d.set("defer_until", timeStr(nw.DeferUntil), nil)
		nw.DeferUntil = nil
	}
	if err := w.writeRow(&nw, cur.Area); err != nil {
		return err
	}
	reason := ""
	if w.m.Reopen != nil {
		reason = strings.TrimSpace(w.m.Reopen.Reason)
	}
	if err := w.record(cur.ID, "reopened", d, reason, cur.Area, cur.Area); err != nil {
		return err
	}
	if reason != "" {
		if err := w.insertComment(cur, "", reason); err != nil {
			return err
		}
	}
	return w.refreshBlocked(w.statusSeeds(cur.ID))
}

// ---- dependencies ----

func (w *writer) depAdd() error {
	req := w.m.Dep
	if req == nil || strings.TrimSpace(req.Target) == "" {
		return bad("a dependency needs a target")
	}
	typ := strings.TrimSpace(req.Type)
	if typ == "" {
		typ = orchapi.DepBlocks
	}
	if len(typ) > 50 || strings.ContainsAny(typ, " \t\n") {
		return bad("invalid dependency type %q", typ)
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	target := strings.TrimSpace(req.Target)
	external := strings.HasPrefix(target, "external:")
	if !external {
		t, err := w.target(target)
		if err != nil {
			return err
		}
		target = t.ID
		if target == cur.ID {
			return bad("an issue cannot depend on itself")
		}
	}
	var have string
	err = w.tx.QueryRowContext(w.ctx, `SELECT type FROM deps WHERE issue_id = ? AND depends_on = ?`, cur.ID, target).Scan(&have)
	switch {
	case err == nil && have == typ:
		return nil
	case err == nil && have == orchapi.DepParentChild:
		return conflict("cannot add dependency: %s is already a child of %s", cur.ID, target)
	case err == nil:
		return conflict("dependency %s -> %s already exists with type %q (requested %q); remove it first", cur.ID, target, have, typ)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if !external {
		switch typ {
		case orchapi.DepBlocks, orchapi.DepConditionalBlocks:
			if err := w.checkBlockCycle(cur.ID, target); err != nil {
				return err
			}
		case orchapi.DepParentChild:
			if cur.Parent != "" {
				return conflict("%s already has parent %s; use update --parent to move it", cur.ID, cur.Parent)
			}
			if err := w.checkParentCycle(cur.ID, target); err != nil {
				return err
			}
		}
	}
	if err := w.insertDep(cur.ID, target, typ, external); err != nil {
		return err
	}
	if err := w.record(cur.ID, "dep_added", fieldDiff{"depends_on": {nil, typ + ":" + target}}, "", cur.Area, cur.Area); err != nil {
		return err
	}
	return w.refreshBlocked([]string{cur.ID})
}

// checkBlockCycle refuses from -> to when to already waits on from.
func (w *writer) checkBlockCycle(from, to string) error {
	seen := map[string]bool{to: true}
	queue := []string{to}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		rows, err := w.tx.QueryContext(w.ctx, `SELECT depends_on FROM deps WHERE issue_id = ? AND type IN ('blocks', 'conditional-blocks') AND external = 0`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			if t == from {
				rows.Close()
				return bad("adding dependency would create a cycle")
			}
			if !seen[t] {
				seen[t] = true
				queue = append(queue, t)
			}
		}
		rows.Close()
	}
	return nil
}

func (w *writer) depRemove() error {
	req := w.m.Dep
	if req == nil || strings.TrimSpace(req.Target) == "" {
		return bad("a dependency needs a target")
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	target := strings.TrimSpace(req.Target)
	if !strings.HasPrefix(target, "external:") {
		if _, err := w.target(target); err != nil {
			return err
		}
	}
	var typ string
	err = w.tx.QueryRowContext(w.ctx, `SELECT type FROM deps WHERE issue_id = ? AND depends_on = ?`, cur.ID, target).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(w.ctx, `DELETE FROM deps WHERE issue_id = ? AND depends_on = ?`, cur.ID, target); err != nil {
		return err
	}
	if typ == orchapi.DepParentChild {
		if _, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET parent = '' WHERE id = ? AND parent = ?`, cur.ID, target); err != nil {
			return err
		}
	}
	if err := w.record(cur.ID, "dep_removed", fieldDiff{"depends_on": {typ + ":" + target, nil}}, "", cur.Area, cur.Area); err != nil {
		return err
	}
	return w.refreshBlocked([]string{cur.ID})
}

// ---- labels ----

func (w *writer) label() error {
	l := strings.TrimSpace(w.m.Label)
	if l == "" {
		return bad("label cannot be empty")
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	var labels []string
	kind := "label_added"
	if w.m.Op == istore.OpLabelAdd {
		labels, err = w.newLabels(cur.Labels, nil, []string{l}, nil)
	} else {
		kind = "label_removed"
		labels, err = w.newLabels(cur.Labels, nil, nil, []string{l})
	}
	if err != nil {
		return err
	}
	if slices.Equal(labels, cur.Labels) {
		return nil
	}
	after := w.areaOf(cur.ID, cur.homeArea, labels)
	if after != cur.Area {
		if err := w.m.Authorize(cur.Area, after); err != nil {
			return err
		}
	}
	if err := w.writeLabels(cur.ID, cur.Labels, labels); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET area = ?, updated_at = ? WHERE id = ?`, after, millis(w.m.At), cur.ID); err != nil {
		return err
	}
	d := fieldDiff{}
	d.set("labels", strings.Join(cur.Labels, ","), strings.Join(labels, ","))
	return w.record(cur.ID, kind, d, l, cur.Area, after)
}

// ---- blocked state ----

// statusSeeds: id itself and every issue waiting on it.
func (w *writer) statusSeeds(id string) []string {
	seeds := []string{id}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT issue_id FROM deps WHERE depends_on = ? AND type IN ('blocks', 'conditional-blocks')`, id)
	if err != nil {
		return seeds
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			seeds = append(seeds, s)
		}
	}
	return seeds
}

// refreshBlocked recomputes is_blocked for seeds, and for the children of
// every issue whose value changed (a blocked parent blocks its children).
func (w *writer) refreshBlocked(seeds []string) error {
	seen := map[string]bool{}
	queue := slices.Clone(seeds)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		var status, parent string
		var pinned, cur int
		err := w.tx.QueryRowContext(w.ctx, `SELECT status, pinned, is_blocked, parent FROM issues WHERE id = ?`, id).Scan(&status, &pinned, &cur, &parent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		nb, err := w.computeBlocked(id, status, pinned, parent)
		if err != nil {
			return err
		}
		if nb == cur {
			continue
		}
		if _, err := w.tx.ExecContext(w.ctx, `UPDATE issues SET is_blocked = ? WHERE id = ?`, nb, id); err != nil {
			return err
		}
		if !slices.Contains(w.touched, id) {
			w.touched = append(w.touched, id)
		}
		rows, err := w.tx.QueryContext(w.ctx, `SELECT id FROM issues WHERE parent = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			queue = append(queue, c)
		}
		rows.Close()
	}
	return nil
}

func (w *writer) computeBlocked(id, status string, pinned int, parent string) (int, error) {
	if status == orchapi.IssueClosed || status == orchapi.IssuePinned || pinned != 0 {
		return 0, nil
	}
	var n int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM deps d JOIN issues t ON t.id = d.depends_on
		WHERE d.issue_id = ? AND d.type IN ('blocks', 'conditional-blocks') AND t.status NOT IN ('closed', 'pinned') AND t.pinned = 0`, id).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 1, nil
	}
	if parent != "" {
		var pb int
		err := w.tx.QueryRowContext(w.ctx, `SELECT is_blocked FROM issues WHERE id = ?`, parent).Scan(&pb)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		return pb, nil
	}
	return 0, nil
}
