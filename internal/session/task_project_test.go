package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// fakeRemoteTasks is the team module's door: it records the project and
// parent each start was handed in.
type fakeRemoteTasks struct {
	mu   sync.Mutex
	got  []orchapi.Session
	subs []*authz.Subject
}

func (f *fakeRemoteTasks) StartTaskOn(_ context.Context, sub *authz.Subject, parent orchapi.Session, _ int, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got, f.subs = append(f.got, parent), append(f.subs, sub)
	return orchapi.Task{ID: orchapi.RemoteTaskPrefix + "x", Parent: parent.Address, Machine: req.Machine, State: orchapi.TaskPending}, nil
}

func (f *fakeRemoteTasks) RemoteTask(context.Context, *authz.Subject, string) (orchapi.Task, error) {
	return orchapi.Task{}, api.NotFound("task", "")
}

func (f *fakeRemoteTasks) CancelTaskOn(context.Context, *authz.Subject, string) (orchapi.Task, error) {
	return orchapi.Task{}, api.NotFound("task", "")
}

func (f *fakeRemoteTasks) ReadTaskOn(context.Context, *authz.Subject, string, string, int) (orchapi.TaskHistoryPage, error) {
	return orchapi.TaskHistoryPage{}, api.NotFound("task", "")
}

func (f *fakeRemoteTasks) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *fakeRemoteTasks) last(t *testing.T, n int) (orchapi.Session, *authz.Subject) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != n {
		t.Fatalf("%d starts reached the team module, want %d: %+v", len(f.got), n, f.got)
	}
	if n == 0 {
		return orchapi.Session{}, nil
	}
	return f.got[n-1], f.subs[n-1]
}

// projectFixture: projects solo (not a team project, soloDir), pool and side
// (team projects; side at capDir), a fake team module, and a raw socket
// client.
type projectFixture struct {
	h                *harness
	fr               *fakeRemoteTasks
	solo, pool, side api.Project
	soloDir, capDir  string
	raw              *http.Client
}

func newProjectFixture(t *testing.T) *projectFixture {
	h := newHarness(t, nil)
	f := &projectFixture{h: h, fr: &fakeRemoteTasks{}, soloDir: t.TempDir(), capDir: t.TempDir()}
	orchcore.Wire(h.d).Team = f.fr
	f.solo = h.project("solo", f.soloDir)
	f.pool = h.project("pool")
	f.side = h.project("side", f.capDir)
	for _, p := range []api.Project{f.pool, f.side} {
		if err := h.d.Store.SetTeam(context.Background(), p.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	f.raw = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	return f
}

// register registers a live session at cwd, as the socket owner or (as
// set) as that session.
func (f *projectFixture) register(t *testing.T, id, cwd, as string) orchapi.Session {
	t.Helper()
	c := *f.h.c
	c.As = as
	ss, err := orchclient.Register(context.Background(), &c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: id, Cwd: cwd, State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func (f *projectFixture) capTo(t *testing.T, address string, p api.Project) {
	t.Helper()
	a, err := api.ParseAddress(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.s.recordRemoteStart(context.Background(), a.Session, &remotePin{project: p.ID, role: authz.Owner, asker: "test"}); err != nil {
		t.Fatal(err)
	}
}

// start is POST /v0/tasks as the session as.
func (f *projectFixture) start(t *testing.T, as, project, machine, cwd string) (orchapi.Task, error) {
	t.Helper()
	c := *f.h.c
	c.As = as
	var tk orchapi.Task
	err := c.Do(context.Background(), http.MethodPost, "/v0/tasks", orchapi.TaskStartBody{Project: project,
		TaskStartRequest: orchapi.TaskStartRequest{Agent: "codex", Prompt: "hi", Machine: machine, Cwd: cwd}}, &tk)
	return tk, err
}

// rawStart is the whole answer (status and body, name replaced by NAME) to
// a start naming project.
func (f *projectFixture) rawStart(t *testing.T, as, project string) string {
	t.Helper()
	b, _ := json.Marshal(orchapi.TaskStartBody{Project: project, TaskStartRequest: orchapi.TaskStartRequest{Agent: "codex", Prompt: "hi", Machine: "box"}})
	req, _ := http.NewRequest(http.MethodPost, "http://zm/v0/tasks", bytes.NewReader(b))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set("Content-Type", "application/json")
	api.SetHeader(req.Header, api.HeaderAs, as)
	resp, err := f.raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.Status + " " + strings.ReplaceAll(string(body), project, "NAME")
}

// task_start's project: a start with machine goes through
// the named team project instead of the parent's, resolved by the same scope
// rules as any write. Left out, it is the parent's project, as before.
func TestTaskStartNamedProject(t *testing.T) {
	f := newProjectFixture(t)
	parent := f.register(t, "solo-parent", f.soloDir, "")
	if parent.Project != f.solo.ID {
		t.Fatalf("parent in %q, want solo %q", parent.Project, f.solo.ID)
	}
	// Left out: the parent's project, exactly as before.
	if _, err := f.start(t, parent.Address, "", "box", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.fr.last(t, 1); got.Project != f.solo.ID || got.Address != parent.Address {
		t.Fatalf("omitted project: handed %+v", got)
	}
	// Named: that team project (slug or ID); the parent stays the caller.
	for i, name := range []string{"pool", f.pool.ID} {
		if _, err := f.start(t, parent.Address, name, "auto", ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _ := f.fr.last(t, 2+i); got.Project != f.pool.ID || got.Address != parent.Address {
			t.Fatalf("named %s: handed %+v", name, got)
		}
	}
	// Refusals that reach no machine.
	for _, tc := range []struct{ what, project, machine, cwd, want string }{
		{"not a team project", "solo", "box", "", "solo is not one"},
		{"project without machine", "pool", "", "", "project is for a start with machine"},
		{"project with this machine", "pool", "mini", "", "project is for a start with machine"},
		{"an area", "pool/area", "box", "", "not an area"},
		{"cwd with project", "pool", "box", "/somewhere", "cwd and project do not go together"},
	} {
		if _, err := f.start(t, parent.Address, tc.project, tc.machine, tc.cwd); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.what, err)
		}
	}
	f.fr.last(t, 3)
}

// A project the caller cannot see answers exactly like one that does not
// exist (status, body and hint), and a capped session cannot widen: not by
// naming a project, not when found by its process, not through a session it
// registers, not below a capped parent.
func TestTaskStartNamedProjectCaps(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	open := f.register(t, "open-parent", f.soloDir, "")
	missing := f.rawStart(t, open.Address, "nosuch")
	if !strings.HasPrefix(missing, "404 ") || !strings.Contains(missing, "NAME") {
		t.Fatalf("missing project: %s", missing)
	}

	// A capped session, through Zm-As.
	capped := f.register(t, "capped-parent", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if got := f.rawStart(t, capped.Address, "pool"); got != missing {
		t.Fatalf("a capped session tells an existing project from a missing one:\n%s\n%s", got, missing)
	}
	if got := f.rawStart(t, capped.Address, "nosuch"); got != missing {
		t.Fatalf("capped, missing:\n%s\n%s", got, missing)
	}
	// Left out, its parent project outside its cap is refused the same way.
	outside := f.register(t, "capped-outside", f.soloDir, "")
	f.capTo(t, outside.Address, f.side)
	if _, err := f.start(t, outside.Address, "", "box", ""); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("capped, its own project outside its cap: %v", err)
	}
	// Its own project still works, and reaches the team module narrowed.
	if _, err := f.start(t, capped.Address, "side", "box", ""); err != nil {
		t.Fatal(err)
	}
	if got, sub := f.fr.last(t, 1); got.Project != f.side.ID || sub.Can(authz.Read, authz.Object{Project: f.pool.ID}) {
		t.Fatalf("capped, its own project: handed %+v (sub widened: %v)", got, sub.Can(authz.Read, authz.Object{Project: f.pool.ID}))
	}

	// Found by its process (in process here: no peer PID, an uncapped
	// subject naming the session), it stays capped.
	byProc := core.WithSubject(ctx, f.h.d.Projects.Local(capped.Address))
	_, err := f.h.s.startTask(byProc, orchapi.TaskStartRequest{Agent: "codex", Prompt: "hi", Machine: "box"}, "pool", "")
	if !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a capped session found by its process widened: %v", err)
	}

	// The reviewer's escape: a capped session registers a fresh session
	// (as itself, or from its process) and names pool through it.
	minted := f.register(t, "minted-by-capped", f.capDir, capped.Address)
	if p, _, ok := f.h.s.SessionCap(minted.Address); !ok || p != f.side.ID || minted.Project != f.side.ID {
		t.Fatalf("a session registered by a capped one: cap %q %v, project %q", p, ok, minted.Project)
	}
	if got := f.rawStart(t, minted.Address, "pool"); got != missing {
		t.Fatalf("the capped session's registered session widened:\n%s\n%s", got, missing)
	}
	procMinted, _, err := f.h.s.Register(byProc, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "minted-by-process", Cwd: f.soloDir})
	if err != nil {
		t.Fatal(err)
	}
	if p, _, ok := f.h.s.SessionCap(procMinted.Address); !ok || p != f.side.ID || procMinted.Project != f.side.ID {
		t.Fatalf("a session registered from a capped one's process: cap %q %v, project %q", p, ok, procMinted.Project)
	}

	// Below a capped parent (a session from before registration inherited
	// caps): the narrowest cap along the chain holds.
	child := f.register(t, "uncapped-child", f.capDir, "")
	if _, err := f.h.d.Store.Registry().Exec(`INSERT INTO session_lineage(id,parent,depth) VALUES(?,?,0)`, child.ID, capped.Address); err != nil {
		t.Fatal(err)
	}
	if got := f.rawStart(t, child.Address, "pool"); got != missing {
		t.Fatalf("a child of a capped session widened:\n%s\n%s", got, missing)
	}
	// Its task's notice writes as the task within the same caps.
	if sub := f.h.s.capChain(ctx, f.h.d.Projects.Local(child.Address)); sub.Can(authz.Read, authz.Object{Project: f.pool.ID}) || !sub.Can(authz.Write, authz.Object{Project: f.side.ID}) {
		t.Fatal("the chain's cap does not hold for a notice")
	}
	f.fr.last(t, 1)

	// A caller whose scope is narrowed (a token's, say) gets the same
	// answer for a project outside it as for none.
	sub := f.h.owner().ForSession(open.Address)
	sub.Scope = sub.Scope.Only(f.side.ID)
	_, e3 := f.h.s.taskProject(ctx, sub, "pool")
	_, e4 := f.h.s.taskProject(ctx, sub, "nosuch")
	a, b := strings.ReplaceAll(e3.Error(), "pool", "NAME"), strings.ReplaceAll(e4.Error(), "nosuch", "NAME")
	if !api.IsCode(e3, api.CodeNotFound) || a != b {
		t.Fatalf("a narrowed caller tells an existing project from a missing one:\n%s\n%s", a, b)
	}
	if id, err := f.h.s.taskProject(ctx, sub, "side"); err != nil || id != f.side.ID {
		t.Fatalf("narrowed, its own project: %q %v", id, err)
	}
}

// rebindVictims registers, as the socket owner with no session around it, a
// live session, an ended one, and a managed shell, all in side.
func (f *projectFixture) rebindVictims(t *testing.T) (live, ended, shell orchapi.Session) {
	t.Helper()
	ctx := context.Background()
	live = f.register(t, "victim-live", f.capDir, "")
	ended = f.register(t, "victim-ended", f.capDir, "")
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim-ended", End: true}); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.h.s.load(ctx, ended.ID); r.State != orchapi.StateEnded {
		t.Fatalf("victim not ended: %s", r.State)
	}
	shell = f.h.spawn("myshell", "exec sleep 60", func(r *orchapi.SpawnRequest) { r.Cwd = f.capDir })
	return live, ended, shell
}

// A capped session cannot take over another session by registering its
// runtime id: not a live one (rebinding it
// onto the capped session's pid would make process ancestry find the
// uncapped one), not an ended one (revive), not a managed shell (bind).
// The answer is the not-found a hidden session gets.
func TestCappedSessionCannotRebind(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	live, ended, shell := f.rebindVictims(t)
	capped := f.register(t, "capped-parent", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	// From now on the test's process is found as the capped session.
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped-parent", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	byProc := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid())
	if _, origin, err := f.h.s.processCaller(byProc, f.h.d.Projects.Local("")); err != nil || origin != capped.Address {
		t.Fatalf("the test process is found as %q (%v), want the capped session", origin, err)
	}
	c := *f.h.c
	c.As = capped.Address
	reg := func(req orchapi.RegisterRequest) (orchapi.Session, error) { return orchclient.Register(ctx, &c, req) }

	if _, err := reg(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim-live", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getpid()}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("rebinding a live session: %v", err)
	}
	if r, _ := f.h.s.load(ctx, live.ID); r.PID == os.Getpid() {
		t.Fatal("the live victim was moved onto the caller's pid")
	}
	if _, origin, _ := f.h.s.processCaller(byProc, f.h.d.Projects.Local("")); origin != capped.Address {
		t.Fatalf("after the attempt the process is found as %s", origin)
	}
	// Nor update or end it by naming its runtime id in a hook that sends no
	// pid.
	for _, req := range []orchapi.RegisterRequest{
		{Agent: "shell", RuntimeID: "victim-live", Cwd: f.capDir, State: orchapi.StateIdle, Title: "renamed by capped"},
		{Agent: "shell", RuntimeID: "victim-live", End: true},
	} {
		if _, err := reg(req); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("a pid-less hook (end=%v) on the live victim: %v", req.End, err)
		}
	}
	if r, _ := f.h.s.load(ctx, live.ID); r.State != live.State || r.Title == "renamed by capped" {
		t.Fatalf("the live victim was written: state %s title %q", r.State, r.Title)
	}
	// Its own pid-less hooks still apply.
	if ss, err := reg(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped-parent", State: orchapi.StateIdle, Title: "own title"}); err != nil || ss.ID != capped.ID {
		t.Fatalf("the capped session's own hook: %+v %v", ss, err)
	}
	if r, _ := f.h.s.load(ctx, capped.ID); r.State != orchapi.StateIdle || r.Title != "own title" {
		t.Fatalf("own hook not applied: %s %q", r.State, r.Title)
	}
	if _, err := f.h.s.startTask(byProc, orchapi.TaskStartRequest{Agent: "codex", Prompt: "hi", Machine: "box"}, "pool", ""); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("from the capped session's process, naming pool: %v", err)
	}
	if _, err := reg(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim-ended", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getpid()}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("reviving an ended session: %v", err)
	}
	if r, _ := f.h.s.load(ctx, ended.ID); r.State != orchapi.StateEnded {
		t.Fatalf("the ended victim revived: %s", r.State)
	}
	got, err := reg(orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bound-by-capped", Session: shell.ID, Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.h.s.load(ctx, shell.ID); got.ID == shell.ID || r.RuntimeAgent != nil {
		t.Fatalf("bound onto the managed shell: %+v %+v", got, r.RuntimeAgent)
	}
	if p, _, ok := f.h.s.SessionCap(got.Address); !ok || p != f.side.ID {
		t.Fatalf("the capped caller's own new session is uncapped: %q %v", p, ok)
	}
	f.fr.last(t, 0)
}

// The socket owner, in no session, still rebinds, revives and binds.
func TestOwnerStillRebinds(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	live, ended, shell := f.rebindVictims(t)
	if ss, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim-ended", Cwd: f.capDir, State: orchapi.StateRunning}); err != nil || ss.ID != ended.ID || ss.State == orchapi.StateEnded {
		t.Fatalf("revive: %+v %v", ss, err)
	}
	if ss, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bound-by-owner", Session: shell.ID, Cwd: f.capDir, State: orchapi.StateRunning}); err != nil || ss.ID != shell.ID {
		t.Fatalf("bind onto the shell: %+v %v", ss, err)
	}
	if r, _ := f.h.s.load(ctx, shell.ID); r.RuntimeAgent == nil || r.RuntimeAgent.Agent != "claude" {
		t.Fatalf("shell binding: %+v", r.RuntimeAgent)
	}
	if ss, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim-live", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getpid()}); err != nil || ss.ID != live.ID {
		t.Fatalf("rebind: %+v %v", ss, err)
	}
	if r, _ := f.h.s.load(ctx, live.ID); r.PID != os.Getpid() {
		t.Fatalf("the owner's rebind did not move the pid: %d", r.PID)
	}
}

// A hook of an already registered session takes no spawnMu (taskOrigin),
// with or without caps on the machine: it answers within the hook budget
// while a spawn holds the lock.
func TestHookDoesNotWaitForSpawn(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	steady := f.register(t, "steady", f.capDir, "")
	withPID, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "steady-pid", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.h.s.load(ctx, withPID.ID); r.PID != os.Getppid() {
		t.Fatalf("pid not recorded: %d", r.PID)
	}
	hook := func(agent, rid string, pid int) time.Duration {
		t.Helper()
		f.h.s.spawnMu.Lock()
		defer f.h.s.spawnMu.Unlock()
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			// As the daemon's socket sees it: with the caller's peer pid.
			byProc := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid())
			_, _, err := f.h.s.Register(byProc, orchapi.RegisterRequest{Agent: agent, RuntimeID: rid, Cwd: f.capDir, State: orchapi.StateIdle, PID: pid})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return time.Since(start)
		case <-time.After(2 * time.Second):
			t.Fatalf("a hook of %s waited for spawnMu", rid)
			return 0
		}
	}
	for _, capped := range []bool{false, true} {
		if capped {
			other := f.register(t, "someone-capped", f.capDir, "")
			f.capTo(t, other.Address, f.side)
		}
		t.Logf("caps=%v: no pid %s, its pid %s", capped, hook("shell", "steady", 0), hook("codex", "steady-pid", os.Getppid()))
	}
	if r, _ := f.h.s.load(ctx, steady.ID); r.State != orchapi.StateIdle {
		t.Fatalf("the hook did not apply: %s", r.State)
	}
}

// Found by its process alone (no Zm-As), a capped session cannot update or
// end another session by its runtime id with no pid, nor unbind the agent
// of a managed shell; ending its own session while its
// process runs does not shed the cap.
func TestCappedSessionCannotEndOthers(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	victim := f.register(t, "victim", f.capDir, "")
	shell := f.h.spawn("bshell", "exec sleep 60", func(r *orchapi.SpawnRequest) { r.Cwd = f.capDir })
	if ss, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bound", Session: shell.ID, Cwd: f.capDir, State: orchapi.StateRunning}); err != nil || ss.ID != shell.ID {
		t.Fatalf("bind: %+v %v", ss, err)
	}
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	byProc := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid())
	hook := func(req orchapi.RegisterRequest) error { _, _, err := f.h.s.Register(byProc, req); return err }
	for _, req := range []orchapi.RegisterRequest{
		{Agent: "shell", RuntimeID: "victim", State: orchapi.StateIdle, Title: "x"},
		{Agent: "shell", RuntimeID: "victim", End: true},
		{Agent: "shell", RuntimeID: "victim", Session: victim.ID, State: orchapi.StateIdle},
	} {
		if err := hook(req); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if r, _ := f.h.s.load(ctx, victim.ID); r.State != orchapi.StateRunning || r.Title == "x" {
		t.Fatalf("victim written: %s %q", r.State, r.Title)
	}
	_ = hook(orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bound", End: true})
	if r, _ := f.h.s.load(ctx, shell.ID); r.RuntimeAgent == nil || r.RuntimeAgent.RuntimeID != "bound" {
		t.Fatalf("the shell's agent was unbound by the capped session: %+v", r.RuntimeAgent)
	}
	// The capped session ends its own row while its process runs: the
	// process keeps the cap and still cannot write the
	// victim, nor shed the cap through a session it registers next.
	if err := hook(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", End: true}); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.h.s.load(ctx, capped.ID); r.State != orchapi.StateEnded {
		t.Fatalf("own end not applied: %s", r.State)
	}
	if !f.h.s.anyLiveCaps(ctx) {
		t.Fatal("an ended capped session's live process counts as uncapped")
	}
	f.h.s.lingering = lingerSet{} // as after a daemon restart: loaded from the registry
	if !f.h.s.anyLiveCaps(ctx) {
		t.Fatal("after a restart, an ended capped session's live process counts as uncapped")
	}
	for _, find := range []func(context.Context, *authz.Subject) (string, error){f.h.s.hookOrigin, f.h.s.taskOrigin} {
		if o, err := find(byProc, f.h.d.Projects.Local("")); err != nil || o != capped.Address {
			t.Fatalf("the process is found as %q (%v), not its ended capped session", o, err)
		}
	}
	if err := hook(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim", State: orchapi.StateIdle}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("victim hook after the capped session ended: %v", err)
	}
	// Nor revive its row onto another live process (after a restart only
	// the row's pid would say which process carries the cap).
	gp, ok := procStat(os.Getppid())
	if !ok || gp.ppid <= 1 {
		t.Skip("no grandparent process to test with")
	}
	if err := hook(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: gp.ppid}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("the ended capped session revived onto another live process: %v", err)
	}
	if r, _ := f.h.s.load(ctx, capped.ID); r.PID != os.Getppid() {
		t.Fatalf("capped row's pid moved to %d", r.PID)
	}
	// A process outside the capped tree is still the owner.
	outside := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), gp.ppid)
	if _, _, err := f.h.s.Register(outside, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim", State: orchapi.StateRunning}); err != nil {
		t.Fatalf("owner's hook while a capped process lingers: %v", err)
	}
	if err := hook(orchapi.RegisterRequest{Agent: "shell", RuntimeID: "again", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	if code := f.byProcess(t, "/v0/sessions/"+victim.ID+"/state", orchapi.StateRequest{State: orchapi.StateIdle}); code != http.StatusNotFound {
		t.Fatalf("state of the victim from the re-registered process: %d", code)
	}
	if r, _ := f.h.s.load(ctx, victim.ID); r.State != orchapi.StateRunning {
		t.Fatalf("victim written: %s", r.State)
	}
	// The owner (not found by process) still writes the victim.
	if _, err := orchclient.SetState(ctx, f.h.c, victim.ID, orchapi.StateRequest{State: orchapi.StateIdle}); err != nil {
		t.Fatalf("owner's state: %v", err)
	}
}

// During a spawn, a hook whose process is found as a session only past the
// daemon's own process (as a new child not yet recorded would be) waits
// for spawnMu instead of trusting that answer.
func TestHookOriginWaitsPastDaemon(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	// Outside every session and not below the daemon (an agent registered
	// with no pid, in the owner's terminal): no wait during a spawn.
	f.h.s.spawnMu.Lock()
	free := make(chan string, 1)
	go func() {
		o, _ := f.h.s.hookOrigin(core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getppid()), f.h.d.Projects.Local(""))
		free <- o
	}()
	select {
	case o := <-free:
		if o != "" {
			t.Fatalf("found as %q", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a hook in no session, not below the daemon, waited for spawnMu")
	}
	f.h.s.spawnMu.Unlock()
	outer, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "outer", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30") // below the daemon (this process)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	origin := func(pid int) chan string {
		out := make(chan string, 1)
		go func() {
			o, _ := f.h.s.hookOrigin(core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), pid), f.h.d.Projects.Local(""))
			out <- o
		}()
		return out
	}
	f.h.s.spawnMu.Lock()
	select {
	case o := <-origin(os.Getpid()):
		if o != outer.Address {
			t.Fatalf("found as %q", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a hook found below the daemon waited for spawnMu")
	}
	waiting := origin(child.Process.Pid)
	select {
	case o := <-waiting:
		t.Fatalf("answered %q during the spawn, past the daemon", o)
	case <-time.After(300 * time.Millisecond):
	}
	f.h.s.spawnMu.Unlock()
	if o := <-waiting; o != outer.Address {
		t.Fatalf("after the spawn: %q", o)
	}
}

// A walk that ends early (here: too deep) is not "no session": the hook
// waits for spawnMu and gets the full answer.
func TestHookOriginTooDeepWaits(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	outer, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "outer", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()})
	if err != nil {
		t.Fatal(err)
	}
	defer func(n int) { ancestryMax = n }(ancestryMax)
	ancestryMax = 1 // the test process only: its parent, outer, is past the limit
	f.h.s.spawnMu.Lock()
	done := make(chan string, 1)
	go func() {
		o, _ := f.h.s.hookOrigin(core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid()), f.h.d.Projects.Local(""))
		done <- o
	}()
	select {
	case o := <-done:
		t.Fatalf("answered %q from a walk cut short, during a spawn", o)
	case <-time.After(300 * time.Millisecond):
	}
	f.h.s.spawnMu.Unlock()
	if o := <-done; o != outer.Address {
		t.Fatalf("after the spawn: %q", o)
	}
}

// A caller below the daemon that a recorded session claims (a managed
// agent's hook) is answered during a spawn without waiting.
func TestHookOriginBelowDaemonNoWait(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	child := exec.Command("sleep", "30") // below the daemon (this process)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	agent, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "agent", Cwd: f.capDir, State: orchapi.StateRunning, PID: child.Process.Pid})
	if err != nil {
		t.Fatal(err)
	}
	f.h.s.spawnMu.Lock()
	defer f.h.s.spawnMu.Unlock()
	done := make(chan string, 1)
	go func() {
		o, _ := f.h.s.hookOrigin(core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), child.Process.Pid), f.h.d.Projects.Local(""))
		done <- o
	}()
	select {
	case o := <-done:
		if o != agent.Address {
			t.Fatalf("found as %q, want %s", o, agent.Address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a recorded session below the daemon waited for spawnMu")
	}
}
