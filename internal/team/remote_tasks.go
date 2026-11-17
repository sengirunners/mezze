package team

// Machine choice (SPEC 8 "Machine choice"): a parent session starts a
// one-shot task on another machine of its team project. Nothing connects to
// that machine: the parent's machine asks the project's authority (POST
// /v0/team/tasks with its node-bound token, or in-process on the authority);
// the authority queues a machine op; the target takes it in its next
// heartbeat answer, runs it at most once (team_ops_run, kept across a
// restart; the op ID is the task's client_request_id too), and reports the
// result and the task's status in the heartbeats after, until an answer took
// the final word. Status, cancel and read go the same way. An op for the
// authority's own machine runs at once. The queue is teamlink's (ops.go);
// the three task kinds are registered here (registerTaskOps). A start waits
// in the authority's line until a machine is free (task_line.go): each
// placement is one task_start op.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// The task op kinds (teamlink's queue carries them).
const (
	opTaskStart  = "task_start"  // payload orchapi.TaskStartOp; report orchapi.Task
	opTaskCancel = "task_cancel" // payload taskRef; report orchapi.Task
	opTaskRead   = "task_read"   // payload taskRef; report orchapi.TaskHistoryPage
)

// taskRef is a task_cancel or task_read op's payload: the start's op id
// (task_read: the item and offset too).
type taskRef struct {
	Task   string `json:"task"`
	Item   string `json:"item,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

// Tunables (tests shorten them).
var (
	// opTakeWithin: a start no machine took by then is refused.
	opTakeWithin = 10 * time.Minute
	// opReadWait: how long a read waits for the machine's answer.
	opReadWait = 30 * time.Second
	// opSeenWithin: auto only picks a machine heard from this recently.
	opSeenWithin = 2 * time.Minute
	// opTextMax bounds a reported result text.
	opTextMax = 64 << 10
)

// opTaskLimit is the per-parent cap across machines (SPEC 8: three).
const opTaskLimit = 3

var _ orchcore.RemoteTasks = (*Service)(nil)

func (s *Service) runner() (orchcore.RemoteTaskRunner, error) {
	rt, ok := orchcore.Sessions(s.d).(orchcore.RemoteTaskRunner)
	if !ok {
		return nil, api.Errorf(api.CodeUnavailable, "the session module runs no remote tasks")
	}
	return rt, nil
}

// ---- the parent's machine (orchcore.RemoteTasks) ----

var remoteTasksMigrations = []store.Migration{
	// The parent's machine keeps a start's client_request_id per parent
	// (not per project: one request is one run, whichever project it
	// names), and which of its remote tasks are known settled (the
	// per-parent limit counts the rest across projects).
	{Kind: store.KindRegistry, Name: "team/0101_task_keys", Script: `
CREATE TABLE team_task_keys (
	parent     TEXT NOT NULL, -- the parent session's address
	request_id TEXT NOT NULL,
	project    TEXT NOT NULL, -- the authority's project ULID it went to
	task       TEXT NOT NULL DEFAULT '', -- the rt- task it started ('': none yet)
	created    INTEGER NOT NULL,
	PRIMARY KEY (parent, request_id)
);
CREATE TABLE team_task_settled (
	id TEXT PRIMARY KEY -- an rt- task of team_task_refs that settled
);`},
}

// StartTaskOn implements orchcore.RemoteTasks. sub is the parent, already
// narrowed by its caps (the session module checked parent.Project with it).
func (s *Service) StartTaskOn(ctx context.Context, sub *authz.Subject, parent orchapi.Session, depth int, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	if !req.Lookup {
		// One start at a time per parent: the count and the start it
		// admits are one step, so concurrent starts cannot pass 3.
		defer s.parentStarts.lock(parent.Address)()
	}
	db := s.d.Store.Registry()
	rid := req.ClientRequestID
	stored, started := "", ""
	if rid != "" {
		err := db.QueryRowContext(ctx, `SELECT project, task FROM team_task_keys WHERE parent = ? AND request_id = ?`, parent.Address, rid).Scan(&stored, &started)
		if err != nil && err != sql.ErrNoRows {
			return orchapi.Task{}, err
		}
		if stored != "" && req.Lookup {
			// A lookup (zm exec --cancel) finds the run its request id
			// started, whatever project it names or leaves out.
			if !sub.Can(authz.Read, authz.Object{Project: stored}) {
				return orchapi.Task{}, api.NotFound("task", rid)
			}
			parent.Project = stored
		}
	}
	rr := orchapi.RemoteTaskRequest{Project: parent.Project, Machine: req.Machine, Parent: parent.Address, Depth: depth, Request: req}
	rr.Request.Machine = ""
	x := s.ByName(parent.Project)
	switch {
	case x != nil:
		rr.Project = x.Snapshot().Project.ID
	case s.d.Store.IsTeam(parent.Project):
	default:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "machine choice needs a team project: the parent session is in %s", parent.Project).
			WithHint("name a team project you own or administer in project, start the parent in one's checkout, or leave machine out")
	}
	fresh := false
	if !req.Lookup {
		if rid != "" && stored == "" {
			res, err := db.ExecContext(ctx, `INSERT INTO team_task_keys (parent, request_id, project, created) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				parent.Address, rid, rr.Project, store.Millis(time.Now()))
			if err != nil {
				return orchapi.Task{}, err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				fresh = true
			} else if err := db.QueryRowContext(ctx, `SELECT project, task FROM team_task_keys WHERE parent = ? AND request_id = ?`, parent.Address, rid).Scan(&stored, &started); err != nil {
				return orchapi.Task{}, err
			}
		}
		if stored != "" && stored != rr.Project {
			return orchapi.Task{}, api.Errorf(api.CodeConflict, "client_request_id was used for a start in another project")
		}
		if started == "" { // a retry of a start that made its task is no new task
			if n, err := s.liveTasks(ctx, sub, parent.Address, s.authorityOf(rr.Project)); err != nil {
				return orchapi.Task{}, err
			} else if n >= opTaskLimit {
				s.dropKey(ctx, fresh, parent.Address, rid)
				return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "parent task concurrency limit %d reached", opTaskLimit)
			}
		}
	}
	var t orchapi.Task
	var err error
	if x != nil {
		err = s.CallRemote(ctx, x, http.MethodPost, "/v0/team/tasks", rr, &t)
		err = olderForTasks(err, x)
	} else {
		t, err = s.createOp(ctx, sub, s.d.Config.Machine, rr, false, false)
	}
	if err != nil {
		// A refusal before anything was queued frees the request id.
		if api.IsCode(err, api.CodeBadRequest) || api.IsCode(err, api.CodeNotFound) || api.IsCode(err, api.CodeForbidden) {
			s.dropKey(ctx, fresh, parent.Address, rid)
		}
		return orchapi.Task{}, err
	}
	if !strings.HasPrefix(t.ID, orchapi.RemoteTaskPrefix) {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "the authority answered no remote task")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO team_task_refs (id, project, parent, created) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		t.ID, rr.Project, parent.Address, store.Millis(time.Now())); err != nil {
		return orchapi.Task{}, err
	}
	if rid != "" && !req.Lookup {
		if _, err := db.ExecContext(ctx, `UPDATE team_task_keys SET task = ? WHERE parent = ? AND request_id = ?`, t.ID, parent.Address, rid); err != nil {
			return orchapi.Task{}, err
		}
	}
	return t, nil
}

// parentLocks serializes remote task starts per parent session. An entry
// lives only while a start holds or waits for it, so the map holds no
// parent that is not starting one now.
type parentLocks struct {
	mu sync.Mutex
	m  map[string]*parentLock
}

type parentLock struct {
	mu    sync.Mutex
	users int
}

// lock takes parent's lock and returns its release.
func (p *parentLocks) lock(parent string) func() {
	p.mu.Lock()
	if p.m == nil {
		p.m = map[string]*parentLock{}
	}
	l := p.m[parent]
	if l == nil {
		l = &parentLock{}
		p.m[parent] = l
	}
	l.users++
	p.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		if l.users--; l.users == 0 {
			delete(p.m, parent)
		}
		p.mu.Unlock()
	}
}

// held is how many parents have a start in flight (tests).
func (p *parentLocks) held() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

// dropKey frees a request id this start reserved (fresh) and never used.
func (s *Service) dropKey(ctx context.Context, fresh bool, parent, rid string) {
	if fresh {
		_, _ = s.d.Store.Registry().ExecContext(context.WithoutCancel(ctx), `DELETE FROM team_task_keys WHERE parent = ? AND request_id = ?`, parent, rid)
	}
}

// liveTasksWithin: a remote task older than its longest deadline and the
// take window has settled whatever its last word was.
const liveTasksWithin = 7*24*time.Hour + time.Hour

// unreachableGrace: a remote task whose authority cannot be asked counts as
// live this long after its start (the take window), then not until it can
// be asked again.
var unreachableGrace = opTakeWithin

// authorityOf names the authority that holds team project id: "" for one
// this machine is the authority of.
func (s *Service) authorityOf(id string) string {
	if x := s.ByName(id); x != nil {
		r := x.Snapshot()
		if r.Authority != "" {
			return r.Authority
		}
		return r.URL
	}
	return ""
}

// liveTasks counts parent's unsettled remote tasks at every authority but
// skip, across their projects (SPEC 8: at most 3 per parent), asking each
// authority for those not yet known settled. One it hides (access revoked
// since) counts until it is known ended or past liveTasksWithin; one whose
// authority cannot answer counts for unreachableGrace. The authority asked
// now (skip) is left out: its waiting line places at most 3 of the parent's
// tasks at once across its projects and keeps the rest waiting, so a 4th
// start there waits instead of being refused.
// Every unsettled ref in the window is read (no row limit: refs at the
// skipped authority are never marked settled here and must not crowd out
// the others); only the others are asked about, and at most until 3 count.
func (s *Service) liveTasks(ctx context.Context, sub *authz.Subject, parent, skip string) (int, error) {
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(ctx, `SELECT r.id, r.project, r.created FROM team_task_refs r WHERE r.parent = ? AND r.created > ?
		AND NOT EXISTS (SELECT 1 FROM team_task_settled x WHERE x.id = r.id) ORDER BY r.created DESC`,
		parent, store.Millis(time.Now().Add(-liveTasksWithin)))
	if err != nil {
		return 0, err
	}
	type ref struct {
		id, project string
		created     int64
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if rows.Scan(&r.id, &r.project, &r.created) == nil {
			refs = append(refs, r)
		}
	}
	rows.Close()
	n := 0
	for _, r := range refs {
		if s.authorityOf(r.project) == skip {
			continue
		}
		t, err := s.RemoteTask(ctx, sub, r.id)
		switch {
		case err == nil && settled(t):
			_, _ = db.ExecContext(ctx, `INSERT INTO team_task_settled (id) VALUES (?) ON CONFLICT DO NOTHING`, r.id)
			continue
		case err != nil && !api.IsCode(err, api.CodeNotFound) && time.Since(time.UnixMilli(r.created)) > unreachableGrace:
			continue // asked again next time
		}
		if n++; n >= opTaskLimit {
			break
		}
	}
	return n, nil
}

// olderForTasks names an authority older than machine choice.
func olderForTasks(err error, x *remote) error { return teamlink.OlderFor(err, x, "machine choice") }

// ref is the project of a remote task this machine started, when sub may
// see it (its parent, or this machine's owner).
func (s *Service) ref(ctx context.Context, sub *authz.Subject, id string) (string, error) {
	var project, parent string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT project, parent FROM team_task_refs WHERE id = ?`, id).Scan(&project, &parent)
	if err != nil || sub == nil || (sub.Session != "" && sub.Session != parent) {
		return "", api.NotFound("task", id)
	}
	return project, nil
}

// RemoteTask implements orchcore.RemoteTasks.
func (s *Service) RemoteTask(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error) {
	project, err := s.ref(ctx, sub, id)
	if err != nil {
		return orchapi.Task{}, err
	}
	if x := s.ByName(project); x != nil {
		var t orchapi.Task
		err := s.CallRemote(ctx, x, http.MethodGet, "/v0/team/tasks/"+url.PathEscape(id), nil, &t)
		return t, olderForTasks(err, x)
	}
	return s.opTask(ctx, s.d.Projects.Local(""), s.d.Config.Machine, id)
}

// CancelTaskOn implements orchcore.RemoteTasks.
func (s *Service) CancelTaskOn(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error) {
	project, err := s.ref(ctx, sub, id)
	if err != nil {
		return orchapi.Task{}, err
	}
	if x := s.ByName(project); x != nil {
		var t orchapi.Task
		err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/tasks/"+url.PathEscape(id)+"/cancel", struct{}{}, &t)
		return t, olderForTasks(err, x)
	}
	return s.cancelOp(ctx, s.d.Projects.Local(""), s.d.Config.Machine, id)
}

// ReadTaskOn implements orchcore.RemoteTasks.
func (s *Service) ReadTaskOn(ctx context.Context, sub *authz.Subject, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
	project, err := s.ref(ctx, sub, id)
	if err != nil {
		return orchapi.TaskHistoryPage{}, err
	}
	if x := s.ByName(project); x != nil {
		var p orchapi.TaskHistoryPage
		q := url.Values{"item": {item}, "offset": {strconv.Itoa(offset)}}
		err := s.CallRemote(ctx, x, http.MethodGet, "/v0/team/tasks/"+url.PathEscape(id)+"/read?"+q.Encode(), nil, &p)
		return p, olderForTasks(err, x)
	}
	return s.readOp(ctx, s.d.Projects.Local(""), s.d.Config.Machine, id, item, offset)
}

// ---- the authority ----

// taskRoutes registers the authority's machine-choice routes.
func (s *Service) taskRoutes(r *core.Router) {
	replicaOnly := func(r *http.Request) (*authz.Subject, error) {
		sub := core.SubjectOf(r.Context())
		if sub.Node == "" || sub.Machine == "" {
			return nil, api.Errorf(api.CodeForbidden, "a team replica's daemon asks for tasks (its node-bound token)").
				WithHint("start one with task_start machine: on a machine of the project")
		}
		return sub, nil
	}
	r.HandleFunc("POST /v0/team/tasks", core.Authed, "a replica's node-bound token only; its identity owns or administers the team project; the parent on its machine; the machine takes remote tasks", jsonH(func(r *http.Request) (any, error) {
		sub, err := replicaOnly(r)
		if err != nil {
			return nil, err
		}
		var req orchapi.RemoteTaskRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.createOp(r.Context(), sub, sub.Machine, req, false, false)
	}))
	r.HandleFunc("GET /v0/team/tasks/{id}", core.Authed, "a replica's node-bound token only; the machine that started it, while its identity still owns or administers the project, else not_found", jsonH(func(r *http.Request) (any, error) {
		sub, err := replicaOnly(r)
		if err != nil {
			return nil, err
		}
		return s.opTask(r.Context(), sub, sub.Machine, r.PathValue("id"))
	}))
	r.HandleFunc("POST /v0/team/tasks/{id}/cancel", core.Authed, "as GET /v0/team/tasks/{id}", jsonH(func(r *http.Request) (any, error) {
		sub, err := replicaOnly(r)
		if err != nil {
			return nil, err
		}
		return s.cancelOp(r.Context(), sub, sub.Machine, r.PathValue("id"))
	}))
	r.HandleFunc("GET /v0/team/tasks/{id}/read", core.Authed, "as GET /v0/team/tasks/{id}; waits at most 30 s for the machine", jsonH(func(r *http.Request) (any, error) {
		sub, err := replicaOnly(r)
		if err != nil {
			return nil, err
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		return s.readOp(r.Context(), sub, sub.Machine, r.PathValue("id"), r.URL.Query().Get("item"), offset)
	}))
}

// createOp queues a start (or runs it here) for from's parent. ask marks an
// approved ask's action (StartAskAction): its target may be from itself.
func (s *Service) createOp(ctx context.Context, sub *authz.Subject, from string, req orchapi.RemoteTaskRequest, ask, asApprover bool) (orchapi.Task, error) {
	rt, err := s.runner()
	if err != nil {
		return orchapi.Task{}, err
	}
	project, err := s.OwnerProject(ctx, sub, req.Project, "starts tasks on other machines")
	if err != nil {
		return orchapi.Task{}, err
	}
	pa, err := api.ParseAddress(req.Parent)
	if err != nil || pa.Kind != api.KindSession || pa.Machine != from {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "the parent must be a session on %s", from)
	}
	if err := orchapi.CheckTaskSandbox(req.Request); err != nil {
		return orchapi.Task{}, err
	}
	switch run := req.Request.Run; {
	case req.Request.Agent == orchapi.TaskAgentRun:
		// A script run (SPEC 8 "Script runs"): the target machine checks it
		// in full and resolves its ref; here only its shape.
		if run == nil || run.Ref == "" || (run.Script == "") == (run.Command == "") || strings.TrimSpace(req.Request.Prompt) != "" {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run needs run.ref and one of run.script or run.command, and no prompt")
		}
		if run.CommitOnRef && !ask {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "run.commit_on_ref is only an approved ask's (its commit was checked when asked)")
		}
	case run != nil:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "run is for agent run")
	case req.Request.Agent != "claude" && req.Request.Agent != "codex" || strings.TrimSpace(req.Request.Prompt) == "" || len(req.Request.Prompt) > 256<<10:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude or codex, with a prompt of at most 256 KiB")
	}
	if req.Depth < 1 {
		req.Depth = 1
	}
	// lookup answers the start this client_request_id made, and never makes
	// one (zm exec --cancel): it is not part of the request compared.
	lookup := req.Request.Lookup
	req.Request.Lookup = false
	if lookup && req.Request.ClientRequestID == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "lookup needs a client_request_id")
	}
	req.Request.Machine = ""
	reqJSON, _ := json.Marshal(req.Request)
	if rid := req.Request.ClientRequestID; rid != "" {
		if !ask && !lookup && (strings.HasPrefix(rid, askRequestPrefix) || strings.HasPrefix(rid, laneRequestPrefix)) {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "client_request_id %s... and %s... are kept for approved asks' actions and fresh lanes", askRequestPrefix, laneRequestPrefix)
		}
		o, err := s.lineByKey(ctx, project, from, pa.String(), rid)
		if err == nil {
			// An approved action never answers as a start it did not make.
			if o.keyBody != string(reqJSON) || o.payload.Ask != ask {
				return orchapi.Task{}, api.Errorf(api.CodeConflict, "client_request_id was used for a different request")
			}
			return s.opTask(ctx, sub, from, o.id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return orchapi.Task{}, err
		}
		if lookup {
			return orchapi.Task{}, api.NotFound("task", rid)
		}
	}
	// Operator class (SPEC 8): a remote task carries the parent's authority
	// and the project's code, so it goes only to a trusted machine.
	trusted, public, err := s.TrustedMachines(ctx, project, rt.ProjectMachines(ctx, project))
	if err != nil {
		return orchapi.Task{}, err
	}
	if public[req.Machine] {
		return orchapi.Task{}, teamlink.PublicRefused(req.Machine, "takes remote tasks")
	}
	// The waiting line (SPEC 8 "Waiting line", task_line.go): a start that
	// some machine could ever take waits for a free one; only one that none
	// can is refused now.
	if err := lineCheck(trusted, req.Machine, from, req.Request.Agent == orchapi.TaskAgentRun, req.Request.Sandbox != nil); err != nil {
		return orchapi.Task{}, err
	}
	pin := req.Machine
	if pin == "auto" {
		pin = ""
	}
	id := orchapi.RemoteTaskPrefix + strings.ToLower(ids.New())
	payload, _ := json.Marshal(orchapi.TaskStartOp{Parent: pa.String(), Depth: req.Depth, Request: req.Request, Ask: ask,
		AsApprover: ask && asApprover && req.Request.Agent == orchapi.TaskAgentRun})
	t := orchapi.Task{ID: id, Machine: pin, Parent: pa.String(), Agent: req.Request.Agent, Model: req.Request.Model, Work: req.Request.Work,
		State: orchapi.TaskPending, MailDelivery: "pending", CommentDelivery: "pending", Sandbox: req.Request.Sandbox}
	taskJSON, _ := json.Marshal(t)
	now := store.Millis(time.Now())
	s.lineMu.Lock()
	if n := s.waitingFor(ctx, pa.String()); n >= opWaitingPerParent {
		s.lineMu.Unlock()
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "%d of this parent's tasks wait in the line already", n).WithHint("wait for some to start, or cancel some")
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_line (id, project, from_machine, from_identity, parent, request_id, request, payload, pin, front, line, state, status, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`, id, project, from, sub.Identity.Name, pa.String(), req.Request.ClientRequestID, string(reqJSON),
		string(payload), pin, req.Request.Front, s.nextLine(ctx, project), string(taskJSON), now, now)
	s.lineMu.Unlock()
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && req.Request.ClientRequestID != "" {
			return s.createOp(ctx, sub, from, req, ask, asApprover) // a racing retry: answer the first
		}
		return orchapi.Task{}, err
	}
	// An approved ask's action starts off the caller: its caller is a decision's delivery, inside the
	// owner's answer request and under the approvals locks every other answer waits on, and a start here (a
	// fetch, a worktree, a spawn) can outlast the answering app's timeout. The action's state carries the rest.
	s.placeLine(ctx, project, !ask)
	return s.opTask(ctx, sub, from, id)
}

// lineByKey is the task from's parent started with request id key; a start
// from before the line is taken in first.
func (s *Service) lineByKey(ctx context.Context, project, from, parent, key string) (lineRow, error) {
	q := `SELECT ` + lineCols + ` FROM team_task_line WHERE project = ? AND from_machine = ? AND parent = ? AND request_id = ?`
	o, err := scanLine(s.d.Store.Registry().QueryRowContext(ctx, q, project, from, parent, key))
	if errors.Is(err, sql.ErrNoRows) {
		if op, ferr := s.FindOp(ctx, project, from, parent, key, opTaskStart); ferr == nil && s.adopt(ctx, op.ID) {
			return scanLine(s.d.Store.Registry().QueryRowContext(ctx, q, project, from, parent, key))
		}
	}
	return o, err
}

// pickMachine is the named machine when it takes remote tasks, or for auto
// the roomiest one heard from lately: most free memory, less 1.5 GB per live
// session, never one at its session limit or under 2 GB of disk, never the
// parent's own machine (leave machine out to run there). A script run goes
// only to a machine whose zm reports it runs scripts (an older build would
// refuse it as a bad prompt).
func pickMachine(ms []api.Machine, want, from, self string, run bool) (string, error) {
	now := time.Now()
	type cand struct {
		name  string
		score int64
	}
	var cs []cand
	for _, m := range ms {
		st := m.Stats
		if want != "auto" {
			if m.Machine != want {
				continue
			}
			if !st.RemoteTasks {
				return "", api.Errorf(api.CodeForbidden, "%s takes no tasks from other machines", want).
					WithHint("its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon")
			}
			if st.MaxSessions > 0 && st.Sessions >= st.MaxSessions {
				return "", api.Errorf(api.CodeUnavailable, "%s is at its session limit (%d)", want, st.MaxSessions).
					WithHint("try again when a session there ends, or name auto")
			}
			if run && !st.ScriptRuns {
				return "", api.Errorf(api.CodeUnavailable, "%s's zm runs no scripts", want).
					WithHint("update zm there (zm daemon update)")
			}
			if m.Machine != self && (m.Seen == nil || now.Sub(*m.Seen) > opSeenWithin) {
				return "", api.Errorf(api.CodeUnavailable, "%s has not been heard from in %s", want, opSeenWithin)
			}
			return want, nil
		}
		if !st.RemoteTasks || run && !st.ScriptRuns || m.Machine == from || (m.Machine != self && (m.Seen == nil || now.Sub(*m.Seen) > opSeenWithin)) {
			continue
		}
		if st.MaxSessions > 0 && st.Sessions >= st.MaxSessions || st.DiskFreeMB >= 0 && st.DiskFreeMB < 2048 {
			continue
		}
		free := int64(0)
		if st.MemoryTotalMB > 0 && st.MemoryUsedMB >= 0 {
			free = st.MemoryTotalMB - st.MemoryUsedMB
		}
		cs = append(cs, cand{m.Machine, free - 1536*int64(st.Sessions)})
	}
	if want != "auto" {
		return "", api.NotFound("machine", want)
	}
	if len(cs) == 0 {
		return "", api.Errorf(api.CodeUnavailable, "no machine of the project takes remote tasks now").
			WithHint("a machine takes them once its owner sets \"remote_tasks\": true; GET /v0/machines shows which do")
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].name < cs[j].name
	})
	return cs[0].name, nil
}

// pickSandboxMachine is pickMachine for a sandbox task: the named machine
// when it takes sandbox tasks, or for auto one that does, heard from
// lately, never the parent's own, with 2 GB of disk; most sandbox slots
// first. Session limits and free memory do not count: the agent runs in a
// pool box and the driver is small (SPEC 8 "Machine choice").
func pickSandboxMachine(ms []api.Machine, want, from, self string) (string, error) {
	now := time.Now()
	heard := func(m api.Machine) bool {
		return m.Machine == self || m.Seen != nil && now.Sub(*m.Seen) <= opSeenWithin
	}
	best, slots := "", 0
	for _, m := range ms {
		st := m.Stats
		if want != "auto" {
			if m.Machine != want {
				continue
			}
			if !st.RemoteTasks || !st.SandboxTasks {
				return "", api.Errorf(api.CodeForbidden, "%s takes no sandbox tasks", want).
					WithHint("its owner sets \"remote_tasks\": true and \"sandbox_task_cmd\" in its zm config.json and restarts its daemon")
			}
			if !heard(m) {
				return "", api.Errorf(api.CodeUnavailable, "%s has not been heard from in %s", want, opSeenWithin)
			}
			return want, nil
		}
		if !st.RemoteTasks || !st.SandboxTasks || m.Machine == from || !heard(m) || st.DiskFreeMB >= 0 && st.DiskFreeMB < 2048 {
			continue
		}
		if n := max(1, st.SandboxSlots); best == "" || n > slots || n == slots && m.Machine < best {
			best, slots = m.Machine, n
		}
	}
	if want != "auto" {
		return "", api.NotFound("machine", want)
	}
	if best == "" {
		return "", api.Errorf(api.CodeUnavailable, "no machine of the project takes sandbox tasks now").
			WithHint("a machine takes them once its owner sets \"remote_tasks\": true and \"sandbox_task_cmd\"; GET /v0/machines shows which do")
	}
	return best, nil
}

// settled: a task status no later report changes.
func settled(t orchapi.Task) bool { return t.State == "ended" || t.State == orchapi.TaskRefused }

// visibleStart is task id as from's sub may see it: the machine that
// asked, while its identity still owns or administers the project.
func (s *Service) visibleStart(ctx context.Context, sub *authz.Subject, from, id string) (lineRow, error) {
	o, err := s.loadLine(ctx, id)
	if err != nil || o.from != from {
		return lineRow{}, api.NotFound("task", id)
	}
	if _, err := s.OwnerProject(ctx, sub, o.project, "starts tasks on other machines"); err != nil {
		return lineRow{}, api.NotFound("task", id)
	}
	return o, nil
}

// opTask is a task's status: its place while it waits, else its machine's
// last report (live here for a task on this machine).
func (s *Service) opTask(ctx context.Context, sub *authz.Subject, from, id string) (orchapi.Task, error) {
	s.ExpireOps(ctx)
	o, err := s.visibleStart(ctx, sub, from, id)
	if err != nil {
		return orchapi.Task{}, err
	}
	if t := o.task(); o.state == linePlaced && o.machine == s.d.Config.Machine && t.Session != "" && !settled(t) {
		if rt, err := s.runner(); err == nil {
			if a, err := api.ParseAddress(t.Session); err == nil {
				if cur, err := rt.RemoteTaskStatus(ctx, a.Session); err == nil {
					s.recordTask(ctx, o.op, cur)
					if o, err = s.loadLine(ctx, id); err != nil {
						return orchapi.Task{}, err
					}
				}
			}
		}
	}
	return s.lineView(ctx, o, true), nil
}

// asRemote is a machine's local task status as the parent's machine sees it.
func asRemote(t orchapi.Task, id, machine string) orchapi.Task {
	t.ID, t.Machine = id, machine
	return cutText(t)
}

// cutText bounds a task's result text to opTextMax.
func cutText(t orchapi.Task) orchapi.Task {
	if t.Result != nil && len(t.Result.Text) > opTextMax {
		r := *t.Result
		cut := r.Text[:opTextMax]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		r.Text = cut + "… (cut at 64 KiB)"
		t.Result = &r
	}
	return t
}

// refuseStart records why placement op never ran on its machine: when it
// is still where its task runs, the task ends refused.
func (s *Service) refuseStart(ctx context.Context, op, why string) {
	o, err := s.lineByOp(ctx, op)
	if err != nil {
		return
	}
	s.FinishOp(ctx, op, "", why)
	ended := false
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		if o, err = s.loadLine(ctx, o.id); err == nil && o.op == op && o.state == linePlaced {
			ended = s.endLine(ctx, o, refused(o.task(), why))
		}
	}()
	if ended {
		s.placeLine(ctx, o.project, false) // its place there is free again
	}
}

// cancelOp cancels a task: one waiting leaves the line and never runs; one
// placed but not carried never runs; else its machine stops it (here at
// once, else with its next heartbeat). Any copy a lost machine may still
// run is stopped too, also once the task ended.
func (s *Service) cancelOp(ctx context.Context, sub *authz.Subject, from, id string) (orchapi.Task, error) {
	if _, err := s.visibleStart(ctx, sub, from, id); err != nil {
		return orchapi.Task{}, err
	}
	var o lineRow
	var follow func() error
	freed := false
	err := func() error {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_task_line SET cancelled = 1, updated = ? WHERE id = ?`, store.Millis(time.Now()), id); err != nil {
			return err
		}
		var err error
		if o, err = s.loadLine(ctx, id); err != nil {
			return err
		}
		s.stopCopies(ctx, o)
		switch o.state {
		case lineQueued:
			s.endLine(ctx, o, refused(o.task(), "cancelled while waiting in the line"))
		case linePlaced:
			// Not carried to another machine: it never runs. Carried, or
			// for this machine (RunHere: maybe starting now), it gets a
			// cancel after it.
			if o.machine != s.d.Config.Machine {
				ok, err := s.UpdateOp(ctx, o.op, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: teamlink.Str(startCancelled), Unsent: true})
				if err != nil {
					return err
				}
				if ok {
					freed = s.endLine(ctx, o, refused(o.task(), startCancelled))
					return nil
				}
			}
			if settled(o.task()) {
				return nil
			}
			p := o
			follow = func() error {
				_, err := s.followOp(ctx, p.project, p.machine, p.from, p.parent, opTaskCancel, taskRef{Task: p.op})
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return orchapi.Task{}, err
	}
	if follow != nil {
		if err := follow(); err != nil {
			return orchapi.Task{}, err
		}
	}
	if freed {
		s.placeLine(ctx, o.project, false)
	}
	return s.opTask(ctx, sub, from, id)
}

// readOp is task_read on a task, waiting at most opReadWait for a machine
// other than this one.
func (s *Service) readOp(ctx context.Context, sub *authz.Subject, from, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
	o, err := s.visibleStart(ctx, sub, from, id)
	if err != nil {
		return orchapi.TaskHistoryPage{}, err
	}
	if o.op == "" || o.machine == "" {
		return orchapi.TaskHistoryPage{}, api.Errorf(api.CodeUnavailable, "task %s waits in the line: it has no history yet", id)
	}
	rid, err := s.followOp(ctx, o.project, o.machine, o.from, o.parent, opTaskRead, taskRef{Task: o.op, Item: item, Offset: offset})
	if err != nil {
		return orchapi.TaskHistoryPage{}, err
	}
	deadline := time.Now().Add(opReadWait)
	for {
		r, err := s.LoadOp(ctx, rid)
		if err != nil {
			return orchapi.TaskHistoryPage{}, err
		}
		if r.State == teamlink.OpDone {
			if r.Error != "" {
				return orchapi.TaskHistoryPage{}, api.Errorf(api.CodeNotFound, "%s", r.Error)
			}
			var p orchapi.TaskHistoryPage
			err := json.Unmarshal([]byte(r.Result), &p)
			return p, err
		}
		if time.Now().After(deadline) {
			return orchapi.TaskHistoryPage{}, api.Errorf(api.CodeUnavailable, "%s did not answer within %s", o.machine, opReadWait).WithHint("try again")
		}
		select {
		case <-ctx.Done():
			return orchapi.TaskHistoryPage{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// followOp adds a follow-up op of kind on machine for a placement (run here
// at once when it is this machine) and returns its id.
func (s *Service) followOp(ctx context.Context, project, machine, from, parent, kind string, ref taskRef) (string, error) {
	payload, _ := json.Marshal(ref)
	op := api.MachineOp{ID: ids.New(), Kind: kind, Payload: payload}
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: project, Machine: machine, FromMachine: from, Parent: parent}); err != nil {
		return "", err
	}
	if machine == s.d.Config.Machine {
		s.RunHere(ctx, project, op)
	}
	return op.ID, nil
}

// registerTaskOps registers the task kinds with teamlink's queue.
func (s *Service) registerTaskOps() {
	s.RegisterOp(opTaskStart, teamlink.OpHandler{
		Run:     s.runTaskStart,
		Apply:   s.applyTaskStart,
		Expiry:  opTakeWithin,
		Expired: s.startExpired, // task_line.go
		// A start's report carries its task's status until it settled here.
		Settled: func(rep api.MachineOpReport) bool {
			var t orchapi.Task
			return json.Unmarshal(rep.Payload, &t) != nil || settledHere(t)
		},
		Refresh: s.refreshTask,
	})
	follow := teamlink.OpHandler{Run: s.runTaskRef, Apply: s.applyTaskRef, Expiry: opTakeWithin}
	s.RegisterOp(opTaskCancel, follow)
	s.RegisterOp(opTaskRead, follow)
}

// applyTaskStart takes a machine's report on a placement: its task's
// status, or why it refused to start it. A report on a placement that is
// no longer where the task runs only stops that copy (recordTask).
func (s *Service) applyTaskStart(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	var t orchapi.Task
	if len(rep.Payload) == 0 || json.Unmarshal(rep.Payload, &t) != nil {
		if rep.Error != "" {
			s.refuseStart(ctx, o.ID, rep.Error)
		}
		return
	}
	s.recordTask(ctx, o.ID, t)
}

// applyTaskRef takes a machine's report on a cancel (its task's status) or
// a read (the page, kept as the op's result).
func (s *Service) applyTaskRef(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	var ref taskRef
	_ = json.Unmarshal(o.Payload, &ref)
	if o.Kind == opTaskRead {
		s.FinishOp(ctx, o.ID, string(rep.Payload), rep.Error)
		return
	}
	s.FinishOp(ctx, o.ID, "", rep.Error)
	var t orchapi.Task
	if len(rep.Payload) > 0 && json.Unmarshal(rep.Payload, &t) == nil {
		s.recordTask(ctx, ref.Task, t)
	}
}

// ---- the machine that runs them ----

// textErr is err as a refusal's text alone (a task's refusal reads as the
// error's whole text).
func textErr(err error) error { return errors.New(err.Error()) }

// runTaskStart starts a task_start op's task here. A cancel arriving while
// the start runs (a script run's checkout) ends gctx, and the start refuses
// to run its script.
func (s *Service) runTaskStart(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	rt, err := s.runner()
	if err != nil {
		return nil, textErr(err)
	}
	var p orchapi.TaskStartOp
	if len(op.Payload) == 0 || json.Unmarshal(op.Payload, &p) != nil {
		return nil, textErr(api.Errorf(api.CodeBadRequest, "task_start without a request"))
	}
	gctx, sg, ok := s.gates.begin(ctx, op.ID)
	if !ok {
		return nil, errors.New(startCancelled)
	}
	sctx := ctx
	if p.Request.Agent == orchapi.TaskAgentRun {
		sctx = gctx // an agent task's start is quick: never cut halfway
	}
	t, err := rt.StartRemoteTask(sctx, project, op.ID, p)
	if err != nil {
		s.gates.end(op.ID, sg, "")
		if s.gates.stopped(sg) {
			return nil, errors.New(startCancelled)
		}
		return nil, textErr(err)
	}
	s.SetOpLocal(ctx, op.ID, t.ID)
	s.gates.end(op.ID, sg, t.ID)
	b, _ := json.Marshal(cutText(t))
	return b, nil
}

// refreshTask is a start's report with its task's current status.
func (s *Service) refreshTask(ctx context.Context, local string, rep *api.MachineOpReport) {
	rt, err := s.runner()
	if err != nil {
		return
	}
	if t, err := rt.RemoteTaskStatus(ctx, local); err == nil {
		rep.Payload, _ = json.Marshal(cutText(t))
	}
}

// runTaskRef runs a task_cancel or task_read op here.
func (s *Service) runTaskRef(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	rt, err := s.runner()
	if err != nil {
		return nil, textErr(err)
	}
	var ref taskRef
	_ = json.Unmarshal(op.Payload, &ref)
	// local is the start's task here: recorded when it ran for a heartbeat
	// (waiting while that start still runs), else, on the authority, in its
	// status.
	local := func() (string, error) {
		deadline := time.Now().Add(opReadWait)
		for {
			id, state, err := s.OpLocal(ctx, ref.Task)
			if err == nil && id != "" {
				return id, nil
			}
			if err == nil && state == "running" && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			var t orchapi.Task
			if o, err := s.LoadOp(ctx, ref.Task); err == nil && o.Machine == s.d.Config.Machine && json.Unmarshal([]byte(o.Status), &t) == nil {
				if a, err := api.ParseAddress(t.Session); err == nil && a.Machine == s.d.Config.Machine {
					return a.Session, nil
				}
			}
			return "", api.NotFound("task", ref.Task)
		}
	}
	if op.Kind == opTaskRead {
		id, err := local()
		if err != nil {
			return nil, textErr(err)
		}
		p, err := rt.ReadRemoteTask(ctx, id, ref.Item, ref.Offset)
		if err != nil {
			return nil, textErr(err)
		}
		b, _ := json.Marshal(p)
		return b, nil
	}
	var id string
	if sg := s.gates.cancel(ref.Task); sg != nil {
		// Its start is still running here: it stops before the script runs
		// (its own report refuses it), or made its task just before,
		// cancelled now.
		select {
		case <-sg.done:
			id = sg.task
		case <-time.After(opReadWait):
			return nil, textErr(api.Errorf(api.CodeUnavailable, "its start did not stop within %s", opReadWait))
		}
		if id == "" {
			return nil, nil
		}
	} else if id, err = local(); err != nil {
		return nil, textErr(err)
	}
	t, err := rt.CancelRemoteTask(ctx, id)
	if err != nil {
		return nil, textErr(err)
	}
	b, _ := json.Marshal(cutText(t))
	return b, nil
}

// settledHere: a local task status nothing later changes in its report
// (ended, its mail and comment no longer on their way).
func settledHere(t orchapi.Task) bool {
	busy := func(d string) bool { return d == "pending" || d == "sending" }
	return t.State == "ended" && !busy(t.MailDelivery) && !busy(t.CommentDelivery)
}
