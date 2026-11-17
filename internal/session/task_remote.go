package session

// Machine choice (SPEC 8 "Machine choice"): a parent starts a one-shot task
// on another zm machine of its team project. The team module carries it
// through the authority (orchcore.RemoteTasks); this file is the session
// module's two halves: the parent's machine hands the start, status, cancel
// and read of an "rt-" task to the team module, and the target machine runs
// what its authority queued (orchcore.RemoteTaskRunner) as an ordinary task
// whose parent is the remote session, under this machine's own limits.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

var _ orchcore.RemoteTaskRunner = (*Service)(nil)

// remoteTasks is the team module's door, or nil (no team module).
func (s *Service) remoteTasks() orchcore.RemoteTasks {
	rt, _ := orchcore.Team(s.d).(orchcore.RemoteTasks)
	return rt
}

// isRemoteTask reports an ID of a task started through the authority.
func isRemoteTask(id string) bool { return strings.HasPrefix(id, orchapi.RemoteTaskPrefix) }

// remoteSubject is the caller as the parent of a remote task: its origin
// session, or the socket owner ("" sees every one started here), narrowed
// by its caps (processCaller).
func (s *Service) remoteSubject(ctx context.Context) (*authz.Subject, error) {
	sub, _, err := s.processCaller(ctx, core.SubjectOf(ctx))
	return sub, err
}

// startTaskOn hands a start with machine set to the team module, in the
// parent's project or the one named (taskProject). Same request checks as
// here; the target machine checks them again.
func (s *Service) startTaskOn(ctx context.Context, sub *authz.Subject, req orchapi.TaskStartRequest, named string) (orchapi.Task, error) {
	rt := s.remoteTasks()
	if rt == nil {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "machine choice needs the team module")
	}
	if req.ContinueFrom != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "continue_from runs where its source ran: leave machine out")
	}
	if isRun(req) {
		if err := checkRun(req); err != nil {
			return orchapi.Task{}, err
		}
		if req.Cwd != "" {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run makes its own checkout: leave cwd out")
		}
	} else if strings.TrimSpace(req.Prompt) == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt is required")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 7*24*60*60 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "timeout_s must be between 0 and 604800")
	}
	if len(req.Prompt) > 256<<10 || len(req.ClientRequestID) > 200 || len(req.Model) > 200 || len(req.Machine) > 200 || len(req.Cwd) > 4096 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt, model, machine, cwd or client_request_id is too long")
	}
	if req.Agent != "claude" && req.Agent != "codex" && !isRun(req) {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude, codex or run")
	}
	if req.Cwd != "" && !filepath.IsAbs(req.Cwd) {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "cwd must be an absolute path on that machine")
	}
	if named != "" && req.Cwd != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "cwd and project do not go together: with project, the machine uses its own checkout of that project").
			WithHint("leave cwd out")
	}
	parent, ok := s.Lookup(ctx, sub.Session)
	if !ok || parent.State == orchapi.StateEnded {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "parent session is not live")
	}
	if req.Sandbox != nil && req.Cwd != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a sandbox task makes no checkout on its machine: leave cwd out")
	}
	if req.Worktree == nil && !isRun(req) && req.Sandbox == nil {
		yes := true
		req.Worktree = &yes
	}
	depth := 1
	var pd int
	db := s.d.Store.Registry()
	err := db.QueryRowContext(ctx, `SELECT depth FROM session_lineage WHERE id=?`, parent.ID).Scan(&pd)
	if err != nil {
		err = db.QueryRowContext(ctx, `SELECT depth FROM session_tasks WHERE id=?`, parent.ID).Scan(&pd)
	}
	if err == nil {
		depth = pd + 1
	}
	if depth > taskDepthLimit {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "task depth limit %d reached", taskDepthLimit)
	}
	// sub is narrowed by every cap along the parent's chain (processCaller),
	// so the same check holds with a project named or not, here and through
	// a replica (whose authority sees only the machine's token). The parent
	// stays the caller's session (results, mail, status and cancel go to
	// it); the task and its records go to the named project.
	switch {
	case named != "":
		p, err := s.taskProject(ctx, sub, named)
		if err == nil {
			parent.Project = p
		} else if !req.Lookup {
			return orchapi.Task{}, err
		} // a lookup finds its start by client_request_id (the team module's key), whatever it names
	case !req.Lookup:
		if err := sub.Check(authz.Operate, authz.Object{Project: parent.Project}, "project", parent.Project); err != nil {
			return orchapi.Task{}, err
		}
	}
	return rt.StartTaskOn(ctx, sub, parent, depth, req)
}

// taskProject resolves a start's named project as sub writes there
// (WriteTarget, operate; sub is already narrowed by its caps): one sub
// cannot see answers exactly like one that does not exist.
func (s *Service) taskProject(ctx context.Context, sub *authz.Subject, name string) (string, error) {
	if len(name) > 200 || strings.Contains(name, "/") {
		return "", api.Errorf(api.CodeBadRequest, "project names a team project (its slug or ID), not an area")
	}
	in := project.ResolveInput{Explicit: name, ExplicitFrom: project.RuleFlag}
	p, _, err := s.target(core.WithSubject(core.WithResolveInput(ctx, in), sub), "", "", authz.Operate, false)
	if err == nil {
		// target's replica path (a joined team project) checks no scope:
		// check sub's here too.
		err = sub.Check(authz.Operate, authz.Object{Project: p.ID}, "project", name)
	} else if !sub.Scope.IsAll() {
		// A narrowed caller learns nothing more from a refusal (an ended
		// joined project, say) than it would of a missing project.
		err = api.NotFound("project", name)
	}
	if err != nil {
		return "", err
	}
	if !s.teamProject(p.ID) {
		return "", api.Errorf(api.CodeBadRequest, "machine choice needs a team project: %s is not one", p.Slug).
			WithHint("make it a team project (zm project team <slug>), or leave project out")
	}
	return p.ID, nil
}

// remoteTaskStatus is task_status on an "rt-" task, waiting as hTaskStatus
// does (the authority's copy changes with the target's heartbeats).
func (s *Service) remoteTaskStatus(ctx context.Context, id string, wait int) (orchapi.Task, error) {
	rt := s.remoteTasks()
	if rt == nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	sub, err := s.remoteSubject(ctx)
	if err != nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	return waitRemote(ctx, wait, func() (orchapi.Task, error) { return rt.RemoteTask(ctx, sub, id) })
}

func (s *Service) remoteTaskCancel(ctx context.Context, id string) (orchapi.Task, error) {
	rt := s.remoteTasks()
	if rt == nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	sub, err := s.remoteSubject(ctx)
	if err != nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	return rt.CancelTaskOn(ctx, sub, id)
}

func (s *Service) remoteTaskRead(ctx context.Context, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
	rt := s.remoteTasks()
	if rt == nil {
		return orchapi.TaskHistoryPage{}, api.NotFound("task", id)
	}
	sub, err := s.remoteSubject(ctx)
	if err != nil {
		return orchapi.TaskHistoryPage{}, api.NotFound("task", id)
	}
	return rt.ReadTaskOn(ctx, sub, id, item, offset)
}

// remoteSettled: a remote task's status that no later answer changes.
func remoteSettled(t orchapi.Task) bool { return t.State == "ended" || t.State == orchapi.TaskRefused }

// ---- the target machine (orchcore.RemoteTaskRunner) ----

// StartRemoteTask implements orchcore.RemoteTaskRunner: the op's task, started
// here as a task of the remote parent. The op's ID is its client_request_id,
// so the same op never starts two tasks (also across a restart).
func (s *Service) StartRemoteTask(ctx context.Context, proj, id string, op orchapi.TaskStartOp) (orchapi.Task, error) {
	if !s.d.Config.File.RemoteTasks {
		return orchapi.Task{}, api.Errorf(api.CodeForbidden, "%s takes no tasks from other machines", s.d.Config.Machine).
			WithHint("its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon")
	}
	if id == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "task_start without an id")
	}
	req := op.Request
	a, err := api.ParseAddress(op.Parent)
	// An approved ask's action runs on the machine the card names, also when
	// the asking session (its parent) is on this one: a parent
	// anywhere else names another machine.
	if err != nil || a.Kind != api.KindSession || a.Machine == s.d.Config.Machine && !op.Ask {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "the parent must be a session on another machine")
	}
	if op.Depth < 1 || op.Depth > taskDepthLimit {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "task depth limit %d reached", taskDepthLimit)
	}
	if req.ContinueFrom != "" || req.OnLimit != "" && req.OnLimit != "next_agent" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "continue_from is not taken from another machine; on_limit must be next_agent")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 7*24*60*60 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "timeout_s is out of bounds")
	}
	if err := orchapi.CheckTaskSandbox(req); err != nil {
		return orchapi.Task{}, err
	}
	if req.Sandbox != nil {
		return s.startRemoteSandbox(ctx, proj, id, op, a, req)
	}
	if isRun(req) {
		return s.startRemoteRun(ctx, proj, id, op, a, req)
	}
	if strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > 256<<10 || len(req.Model) > 200 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt or model is out of bounds")
	}
	if req.Agent != "claude" && req.Agent != "codex" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude, codex or run")
	}
	if req.Cwd == "" && op.Ask {
		// A repeat of an ask's op answers the task it started, wherever
		// that one stood (a checkout may have appeared here since).
		var started string
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM session_tasks WHERE parent=? AND request_id=?`, a.String(), id).Scan(&started) == nil {
			sub := s.d.Projects.Local(a.String())
			return s.getTask(core.WithSubject(ctx, sub), sub, started)
		}
	}
	// Where it stands: cwd as given, else this machine's checkouts of the
	// project, most recent first.
	cwds := []string{req.Cwd}
	if req.Cwd == "" {
		n := 1
		if op.Ask {
			n = askCheckouts
		}
		cwds = s.projectCheckouts(ctx, proj, n)
		if len(cwds) == 0 && !op.Ask {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "%s has no known checkout of the project", s.d.Config.Machine).
				WithHint("pass cwd: the checkout's absolute path on that machine")
		}
	}
	req.Machine = ""
	req.ClientRequestID = id
	sub := s.d.Projects.Local(a.String()) // the parent, as the owner of this machine starts it
	ctx = core.WithSubject(ctx, sub)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	start := func(r orchapi.TaskStartRequest) (orchapi.Task, bool, error) {
		if r.Worktree == nil {
			yes := true
			r.Worktree = &yes
		}
		b, _ := json.Marshal(r)
		rs := &remoteStart{project: proj, depth: op.Depth}
		t, err := s.startTaskLocked(ctx, sub, orchapi.Session{Address: a.String()}, r, string(b), "", "", rs)
		return t, rs.checkoutRefused, err
	}
	var refused error
	for _, cwd := range cwds {
		r := req
		r.Cwd = cwd
		t, checkoutRefused, err := start(r)
		// A checkout no worktree can be made from (a submodule, a filter, a
		// partial clone) is not where an approved ask's action
		// stands: its next checkout is, then its scratch directory.
		if !checkoutRefused || !op.Ask || req.Cwd != "" || ctx.Err() != nil {
			return t, err
		}
		refused = err
	}
	// An approved ask's agent action runs in a fresh scratch directory when
	// this machine has no checkout of the project it can use: its asker passes no cwd, and the owner's yes is never wasted
	// on where to stand. Removed when the task ends.
	scratch := s.askScratch(id)
	var started string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM session_tasks WHERE parent=? AND request_id=?`, a.String(), id).Scan(&started) == nil {
		return s.getTask(ctx, sub, started) // a repeat of the op: its directory may be gone with it
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return orchapi.Task{}, err
	}
	if refused != nil {
		s.d.Log.Warn("ask action: no usable checkout, running in scratch", "op", id, "err", refused)
	}
	no := false
	req.Cwd, req.Worktree = scratch, &no
	t, _, err := start(req)
	if err != nil {
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM session_tasks WHERE parent=? AND request_id=?`, a.String(), id).Scan(&started) != nil {
			removeSandboxDir(s.askScratchDir(), scratch) // no task stands in it
		}
	}
	return t, err
}

// startRemoteRun starts op's script run here (SPEC 8 "Script runs"): a
// clean checkout of the commit its ref resolves to on the remote, then the
// script as a task of the remote parent. A repeat of the op answers the task
// it started without checking out again.
func (s *Service) startRemoteRun(ctx context.Context, proj, opID string, op orchapi.TaskStartOp, parent api.Address, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	if err := checkRun(req); err != nil {
		return orchapi.Task{}, err
	}
	if req.Run.CommitOnRef && !op.Ask {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "run.commit_on_ref is only an approved ask's (its commit was checked when asked)")
	}
	if req.Cwd != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run makes its own checkout: leave cwd out")
	}
	sub := s.d.Projects.Local(parent.String())
	ctx = core.WithSubject(ctx, sub)
	started := func() (orchapi.Task, bool, error) {
		var id string
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM session_tasks WHERE parent=? AND request_id=? AND remote=1`, parent.String(), opID).Scan(&id) != nil {
			return orchapi.Task{}, false, nil
		}
		t, err := s.getTask(ctx, sub, id)
		return t, true, err
	}
	s.taskMu.Lock()
	t, ok, err := started()
	s.taskMu.Unlock()
	if ok {
		return t, err
	}
	// The checkout (network and disk) happens outside taskMu: other starts,
	// cancels and a shutdown must not wait on a slow remote.
	runStarts.RLock()
	defer runStarts.RUnlock()
	gctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	cmd, dir, commit, err := s.prepareRun(gctx, proj, opID, *req.Run)
	if err != nil {
		return orchapi.Task{}, err
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if t, ok, err := started(); ok {
		removeRunCheckout(ctx, s.runsDir(), dir)
		return t, err
	}
	// ctx ends when a cancel reached this machine during the checkout: the
	// script never runs. Past here the start is not cut halfway.
	if err := ctx.Err(); err != nil {
		removeRunCheckout(context.WithoutCancel(ctx), s.runsDir(), dir)
		return orchapi.Task{}, api.Errorf(api.CodeConflict, "cancelled before it started")
	}
	ctx = context.WithoutCancel(ctx)
	no := false
	req.Worktree, req.Cwd, req.Machine, req.ClientRequestID = &no, dir, "", opID
	run := *req.Run
	run.Commit = commit
	req.Run = &run
	b, _ := json.Marshal(req)
	t, err = s.startTaskLocked(ctx, sub, orchapi.Session{Address: parent.String()}, req, string(b), "", "", &remoteStart{project: proj, depth: op.Depth, command: cmd,
		asApprover: op.Ask && op.AsApprover})
	if err != nil {
		// Keep the checkout when the task exists (its end removes it).
		if _, exists, _ := started(); !exists {
			removeRunCheckout(ctx, s.runsDir(), dir)
		}
	}
	return t, err
}

// RemoteTaskStatus implements orchcore.RemoteTaskRunner. A task that handed off
// on a limit (on_limit next_agent) answers as its whole chain, from any of
// its IDs: the first task's ID, the newest continuation's agent, session,
// state and result, and Tried for the agents before it, so the parent's
// machine follows the work rather than the agent that stopped.
// While the next agent is being chosen it is running.
func (s *Service) RemoteTaskStatus(ctx context.Context, id string) (orchapi.Task, error) {
	sys := s.d.System()
	t, err := s.getTask(ctx, sys, id)
	if err != nil {
		return orchapi.Task{}, err
	}
	for i := 0; i < taskChainMax && t.ContinuedFrom != ""; i++ {
		prev, err := s.getTask(ctx, sys, t.ContinuedFrom)
		if err != nil {
			break
		}
		t = prev
	}
	root := t.ID
	var tried []orchapi.TaskTry
	for i := 0; i < taskChainMax && t.State == "ended" && t.Continuation != ""; i++ {
		try := orchapi.TaskTry{Agent: t.Agent, Session: t.Session}
		if t.Result != nil {
			try.Outcome = t.Result.Outcome.Kind
		}
		if t.Continuation == "pending" {
			tried = append(tried, try)
			t.State, t.Result = "running", nil
			break
		}
		next, err := s.getTask(ctx, sys, t.Continuation)
		if err != nil {
			break
		}
		tried = append(tried, try)
		t = next
	}
	if len(tried) > 0 {
		t.ID, t.ContinuedFrom, t.Tried = root, "", tried
	}
	if t.State != "ended" && t.Session != "" {
		t.Acted = s.taskActed(ctx, t.ID, t.Session) // a push (SPEC 8 "Waiting line")
	} else {
		s.pushes.Delete(t.ID)
	}
	return t, nil
}

// taskChainMax bounds a walk along continuations (each agent runs once).
const taskChainMax = 16

// CancelRemoteTask implements orchcore.RemoteTaskRunner: as task_cancel by the
// parent (a live continuation of an ended task is cancelled instead).
func (s *Service) CancelRemoteTask(ctx context.Context, id string) (orchapi.Task, error) {
	t, err := s.getTask(ctx, s.d.System(), id)
	if err != nil {
		return orchapi.Task{}, err
	}
	sub := s.d.Projects.Local(t.Parent)
	return s.cancelTask(core.WithSubject(ctx, sub), sub, id)
}

// ReadRemoteTask implements orchcore.RemoteTaskRunner.
func (s *Service) ReadRemoteTask(ctx context.Context, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
	return s.readTaskHistory(ctx, s.d.System(), id, item, offset)
}

// ProjectMachines implements orchcore.RemoteTaskRunner: this machine, then the
// replicas with numbers for project (an authority's record).
func (s *Service) ProjectMachines(ctx context.Context, proj string) []api.Machine {
	out := []api.Machine{{Machine: s.d.Config.Machine, Stats: s.MachineStats(ctx)}}
	ms, err := s.remoteMachines(ctx, authz.System(s.d.Config.Machine))
	if err != nil {
		return out
	}
	for _, m := range ms {
		for _, p := range m.Projects {
			if p == proj {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// projectCheckout is a checkout of proj on this machine: the main
// repository of its most recent sessions here (a zm worktree counts as its
// repository), first one that still exists.
// askScratchDir holds the scratch directories of approved ask actions that
// found no checkout here (one per op, removed when its task ends).
func (s *Service) askScratchDir() string { return filepath.Join(s.d.Config.Home, "ask-scratch") }

// askScratch is ask op opID's scratch directory.
func (s *Service) askScratch(opID string) string {
	return filepath.Join(s.askScratchDir(), pathSafe(opID))
}

// pathSafe is id as one path element: lower-case letters, digits and
// dashes, anything else an underscore.
func pathSafe(id string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			return c
		}
		return '_'
	}, strings.ToLower(id))
}

func (s *Service) projectCheckout(ctx context.Context, proj string) string {
	if cs := s.projectCheckouts(ctx, proj, 1); len(cs) > 0 {
		return cs[0]
	}
	return ""
}

// askCheckouts bounds how many checkouts an ask's action tries (each try
// may cost a worktree add under taskMu) before its scratch directory.
const askCheckouts = 3

// projectCheckouts is up to max distinct checkouts of proj here, most
// recent first (as projectCheckout).
func (s *Service) projectCheckouts(ctx context.Context, proj string, max int) []string {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT cwd, worktree FROM sessions WHERE project=? AND machine=? AND cwd != '' ORDER BY updated DESC LIMIT 50`, proj, s.d.Config.Machine)
	if err != nil {
		return nil
	}
	type c struct{ cwd, wt string }
	var cs []c
	for rows.Next() {
		var x c
		if rows.Scan(&x.cwd, &x.wt) == nil {
			cs = append(cs, x)
		}
	}
	rows.Close()
	var out []string
	for _, x := range cs {
		dir := x.cwd
		if x.wt != "" {
			dir = mainRepo(ctx, x.wt)
		} else if root := project.GitRoot(x.cwd); root != "" {
			dir = root
		}
		// zm's own worktrees and script-run checkouts (removed when a run
		// ends) are never the project's checkout.
		if dir == "" || strings.HasPrefix(dir, filepath.Join(s.d.Config.Home, "worktrees")+string(filepath.Separator)) ||
			strings.HasPrefix(dir, s.runsDir()+string(filepath.Separator)) || strings.HasPrefix(dir, s.askScratchDir()+string(filepath.Separator)) {
			continue
		}
		if slices.Contains(out, dir) {
			continue
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			if out = append(out, dir); len(out) >= max {
				break
			}
		}
	}
	return out
}
