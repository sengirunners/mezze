package team

// The waiting line's per-write checks and its list (SPEC 8 "Waiting
// line", task_line.go): outward actions a placed task's session takes, the
// writes a lost copy may no longer make, and GET /v0/team/tasks.

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// ---- outward actions the authority sees ----

// Every write a remote task's session sends the authority is an outward
// action (it reaches people, other agents or shared state), so a task that
// made one never runs twice (a carried review finding), except the
// few in harmlessWrites. Each forwarded write route is classed in one of
// the two (a test fails on one in neither); a write in neither counts as
// "write".
var outwardWrites = map[string]string{
	"POST /v0/asks": "ask", "POST /v0/asks/{id}/answer": "ask", "POST /v0/asks/{id}/withdraw": "ask", "POST /v0/asks/{id}/choose": "ask",
	"POST /v0/asks/{id}/undo": "ask", "POST /v0/asks/{id}/device-undo": "ask", "POST /v0/asks/{id}/action/confirm": "ask",
	"POST /v0/asks/{id}/check": "ask", "POST /v0/asks/{id}/step": "ask", "POST /v0/asks/{id}/action/retry": "ask",
	// Person steps: they reach a person's daily card, mail the adder and comment on work items.
	"POST /v0/steps": "ask", "POST /v0/steps/{id}/drop": "ask", "POST /v0/steps/{id}/check": "ask", "POST /v0/bundles/{id}/steps/{n}/report": "ask",
	"POST /v0/approvals/pair-key": "approval", "POST /v0/approvers/self": "approval", "DELETE /v0/approvals/key": "approval",
	"POST /v0/aliases": "alias", "DELETE /v0/aliases/{alias}": "alias",
	"POST /v0/person-links": "alias", "DELETE /v0/person-links/{identity}": "alias",
	"POST /v0/sessions/{id}/prompt": "session", "POST /v0/sessions/{id}/turn": "session", "POST /v0/sessions/{id}/interrupt": "session",
	"POST /v0/sessions/{id}/answer": "session", "POST /v0/sessions/{id}/stop": "session",
	"POST /v0/summaries": "summary", "PUT /v0/summaries/config": "summary", "PUT /v0/github/repo-list": "repo-list",
	"PUT /v0/door": "door", "PUT /v0/door/tasks/{task}": "door", "DELETE /v0/door/tasks/{task}": "door", "POST /v0/door/tasks/{task}/rotate": "door",
	"POST /v0/work": "issue", "POST /v0/work/{id}/claim": "claim", "POST /v0/work/{id}/comment": "comment", "POST /v0/work/{id}/link": "link",
	"POST /v0/work/{id}/release": "claim", "POST /v0/work/{id}/status": "status",
	"POST /v0/leases": "claim", "DELETE /v0/leases/{key}": "claim",
	"POST /v0/mail": "mail", "POST /v0/mail/imports": "mail", "POST /v0/mail/{id}/reply": "mail",
	"POST /v0/issues": "issue", "PATCH /v0/issues/{id}": "status", "POST /v0/issues/restore": "issue", "POST /v0/issues/setup": "issue",
	"POST /v0/issues/move":       "issue",
	"POST /v0/issues/{id}/claim": "claim", "POST /v0/issues/{id}/close": "status", "POST /v0/issues/{id}/reopen": "status",
	"POST /v0/issues/{id}/comments": "comment", "PATCH /v0/issues/{id}/comments/{cid}": "comment", "DELETE /v0/issues/{id}/comments/{cid}": "comment",
	"POST /v0/issues/{id}/comments/{cid}/redact": "comment", "POST /v0/issues/{id}/comments/{cid}/ack-gap": "comment",
	"POST /v0/issues/{id}/deps": "issue", "DELETE /v0/issues/{id}/deps/{target}": "issue",
	"POST /v0/issues/{id}/labels": "issue", "DELETE /v0/issues/{id}/labels/{label}": "issue",
	"POST /v0/issues/{id}/attachments": "issue", "DELETE /v0/issues/{id}/attachments/{aid}": "issue",
	"PUT /v0/issues/memories/{key}": "issue", "DELETE /v0/issues/memories/{key}": "issue",
	"POST /v0/login/github/start": "login", "POST /v0/login/github/{id}": "login",
	// Boxes (run targets): what runs in one may push, deploy or write.
	"POST /v0/run_targets": "box", "DELETE /v0/run_targets/{target}": "box", "POST /v0/run_targets/{target}/acquire": "box",
	"POST /v0/run_targets/{target}/handles/{id}/exec": "box", "POST /v0/run_targets/{target}/handles/{id}/input": "box",
	"POST /v0/run_targets/{target}/handles/{id}/output": "box", "POST /v0/run_targets/{target}/handles/{id}/release": "box",
	"POST /v0/run_targets/{target}/handles/{id}/renew": "box", "POST /v0/run_targets/{target}/handles/{id}/signal": "box",
	"POST /v0/run_targets/{target}/handles/{id}/spawn": "box", "POST /v0/run_targets/{target}/handles/{id}/start": "box",
	"POST /v0/run_targets/{target}/handles/{id}/stop": "box", "POST /v0/run_targets/{target}/handles/{id}/secrets": "box",
	// A port can send application writes; publishing/revoking changes public access.
	"POST /v0/run_targets/{target}/handles/{id}/port": "box", "POST /v0/run_targets/{target}/handles/{id}/previews": "box",
	// The pool's sealing mode: a change of how secrets travel.
	"PUT /v0/sealing": "sealing", "POST /v0/sealing/forget": "sealing",
	// Automation records: a definition runs with its home's credentials.
	"POST /v0/automations": "automation", "PATCH /v0/automations/{id}": "automation", "DELETE /v0/automations/{id}": "automation",
	"PUT /v0/automations/{id}/health": "automation", "PUT /v0/automation-homes/{home}": "automation",
	// Owner rulings: the project's standing rules every agent reads.
	"POST /v0/rulings": "ruling", "DELETE /v0/rulings/{name}": "ruling",
}

// harmlessWrites change only what the session itself has read, or read
// only (a POST for its body).
var harmlessWrites = map[string]bool{"POST /v0/mail/{id}/ack": true, "POST /v0/mail/{id}/got-it": true, "PUT /v0/mail/read-marks": true, "POST /v0/issues/sql": true}

// outwardKind is how a write is recorded ("" a read or a harmless write).
func (s *Service) outwardKind(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return ""
	}
	rule, _ := s.Route(r)
	if harmlessWrites[rule.Pattern] {
		return ""
	}
	if k := outwardWrites[rule.Pattern]; k != "" {
		return k
	}
	return "write"
}

// taskSession is the session a replica's request acts as ("" none) and
// its machine.
func taskSession(r *http.Request) (string, string) {
	sub := core.SubjectOf(r.Context())
	if sub == nil || sub.Node == "" || sub.Machine == "" || sub.Session == "" {
		return "", ""
	}
	a, err := api.ParseAddress(sub.Session)
	if err != nil || a.Kind != api.KindSession || a.Machine != sub.Machine {
		return "", ""
	}
	return a.String(), sub.Machine
}

// The keyed lookups made on every write a replica forwards (a test reads
// their plans: none may scan).
const (
	lostBySession  = `SELECT task FROM team_task_copies WHERE session = ? LIMIT 1`
	lostByMachine  = `SELECT task FROM team_task_copies WHERE machine = ? AND project = ? AND live = 1 AND session != '' LIMIT 1`
	actedBySession = `SELECT id, acted FROM team_task_line WHERE session = ? AND state = 'placed' AND machine = ?`
)

// lostCopiesSeen: machine's heartbeat for project lists its live sessions;
// a lost copy not among them no longer runs there (its owner's writes go
// again; a report of it running revives it).
func (s *Service) lostCopiesSeen(ctx context.Context, project, machine string, live map[string]bool) {
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(ctx, `SELECT op, session FROM team_task_copies WHERE machine = ? AND project = ? AND live = 1`, machine, project)
	if err != nil {
		return
	}
	var gone []string
	for rows.Next() {
		var op, a string
		if rows.Scan(&op, &a) == nil && !live[a] {
			gone = append(gone, op)
		}
	}
	rows.Close()
	for _, op := range gone {
		_, _ = db.ExecContext(ctx, `UPDATE team_task_copies SET live = 0 WHERE op = ?`, op)
	}
}

// fromMember answers a replica's call before the router: a lost copy's
// writes are refused, a placed task's outward writes recorded, then the
// live-write record (livewrites.go).
func (s *Service) fromMember(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if s.lostWrite(w, r) {
		return true
	}
	s.noteOutward(r)
	return s.liveWrite(w, r, next)
}

// lostWrite refuses (409) a write from a task's copy on a lost machine (the
// task went back to the line or ended lost), so a held outbox or a laptop
// waking cannot act for it: any write its session sends, and, while the
// copy may still run there, the forwarded writes that machine sends as its
// owner (an agent can call the socket without Zm-As).
func (s *Service) lostWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	sub := core.SubjectOf(r.Context())
	if sub == nil || sub.Node == "" || sub.Machine == "" {
		return false
	}
	db := s.d.Store.Registry()
	var task string
	if session, _ := taskSession(r); session != "" {
		if db.QueryRowContext(r.Context(), lostBySession, session).Scan(&task) != nil {
			return false
		}
		api.WriteError(w, api.Errorf(api.CodeConflict, "%s is the copy of task %s on a machine that lost it: the task went back to the line or ended, so this session no longer acts", session, task))
		return true
	}
	if sub.Session != "" || sub.Cap == nil || sub.Cap.Project == "" || !s.IsForwardedWrite(r) {
		return false
	}
	if db.QueryRowContext(r.Context(), lostByMachine, sub.Machine, sub.Cap.Project).Scan(&task) != nil {
		return false
	}
	api.WriteError(w, api.Errorf(api.CodeConflict, "%s still runs a copy of task %s, which its machine lost: its writes as the machine's owner wait until that copy has stopped", sub.Machine, task).
		WithHint("it stops with the cancel the authority sent; try again in a few seconds"))
	return true
}

// noteOutward records an outward write a replica forwards for a session
// that is a placed remote task of this authority (live or queued writes,
// before they run: an attempt counts).
func (s *Service) noteOutward(r *http.Request) {
	session, machine := taskSession(r)
	if session == "" {
		return
	}
	kind := s.outwardKind(r)
	if kind == "" {
		return
	}
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(r.Context(), actedBySession, session, machine)
	if err != nil {
		return
	}
	type x struct{ id, acted string }
	var xs []x
	for rows.Next() {
		var v x
		if rows.Scan(&v.id, &v.acted) == nil && !inCSV(v.acted, kind) {
			xs = append(xs, v)
		}
	}
	rows.Close()
	for _, v := range xs {
		// Only on the acted read (a report or another write may add to it
		// meanwhile): read again and retry.
		for try := 0; try < 5; try++ {
			res, err := db.ExecContext(r.Context(), `UPDATE team_task_line SET acted = ?, updated = ? WHERE id = ? AND acted = ?`,
				addCSV(v.acted, kind), store.Millis(time.Now()), v.id, v.acted)
			if err != nil {
				break
			}
			if n, _ := res.RowsAffected(); n == 1 {
				break
			}
			if db.QueryRowContext(r.Context(), `SELECT acted FROM team_task_line WHERE id = ?`, v.id).Scan(&v.acted) != nil || inCSV(v.acted, kind) {
				break
			}
		}
	}
}

// ---- the list ----

func (s *Service) lineRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/team/tasks", core.Authed, "this machine's owner on its socket (sent on to a joined project's authority), or at the authority a replica's node-bound token; its identity owns or administers the team project; parent and session addresses redacted", jsonH(s.handleLine))
}

// lineListSettled: how many settled tasks the list ends with.
const lineListSettled = 20

// handleLine is GET /v0/team/tasks?project=.
func (s *Service) handleLine(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	replica := sub != nil && sub.Node != "" && sub.Machine != ""
	if !replica && (sub == nil || sub.Via != authz.ViaLocal) {
		return nil, api.Errorf(api.CodeForbidden, "only the project's owner (or an admin), on their machine's socket, lists its remote tasks").
			WithHint("run it there: zm tasks -P <project>")
	}
	name := r.URL.Query().Get("project")
	if name == "" {
		return nil, api.Errorf(api.CodeBadRequest, "name the team project: ?project=")
	}
	if !replica {
		if x := s.ByName(name); x != nil {
			snap := x.Snapshot()
			if err := teamlink.Ended(snap); err != nil {
				return nil, err
			}
			var out orchapi.RemoteTaskList
			err := s.CallRemote(ctx, x, http.MethodGet, "/v0/team/tasks?project="+url.QueryEscape(snap.Project.ID), nil, &out)
			return out, teamlink.OlderFor(err, x, "the waiting line")
		}
	}
	project, err := s.OwnerProject(ctx, sub, name, "lists its remote tasks")
	if err != nil {
		return nil, err
	}
	rows, err := s.lineRows(ctx, `project = ? AND state != 'done'`, project)
	if err != nil {
		return nil, err
	}
	done, err := s.lineRows(ctx, `project = ? AND state = 'done' ORDER BY created DESC LIMIT ?`, project, lineListSettled)
	if err != nil {
		return nil, err
	}
	var waiting, placed []lineRow
	for _, o := range rows {
		if o.state == lineQueued {
			waiting = append(waiting, o)
		} else {
			placed = append(placed, o)
		}
	}
	sort.Slice(waiting, func(i, j int) bool {
		a, b := waiting[i], waiting[j]
		if a.front != b.front {
			return a.front
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.created != b.created {
			return a.created < b.created
		}
		return a.id < b.id
	})
	sort.Slice(placed, func(i, j int) bool { return placed[i].placed < placed[j].placed })
	out := orchapi.RemoteTaskList{Project: project, Tasks: []orchapi.Task{}}
	for i, o := range append(append(waiting, placed...), done...) {
		t := s.lineView(ctx, o, false)
		if o.state == lineQueued {
			t.Position = i + 1
		}
		t.Parent = s.d.Redact(ctx, sub, t.Parent)
		if t.Session != "" {
			t.Session = s.d.Redact(ctx, sub, t.Session)
		}
		for i := range t.Tried {
			t.Tried[i].Session = s.d.Redact(ctx, sub, t.Tried[i].Session)
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, nil
}

// ---- comma-separated sets ----

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func inCSV(set, v string) bool {
	for _, x := range splitCSV(set) {
		if x == v {
			return true
		}
	}
	return false
}

func addCSV(set string, vs ...string) string {
	for _, v := range vs {
		if v != "" && !strings.Contains(v, ",") && !inCSV(set, v) {
			if set != "" {
				set += ","
			}
			set += v
		}
	}
	return set
}
