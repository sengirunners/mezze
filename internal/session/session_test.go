package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
	"golang.org/x/sys/unix"
)

// fakeWork records the work-module calls the session module makes.
type fakeWork struct {
	mu        sync.Mutex
	bound     []string // "<project> <id> <session>"
	ended     []string
	commentFn func(context.Context, *authz.Subject, string, string, string) error
}

func (f *fakeWork) MailTarget(context.Context, *authz.Subject, string, string) (string, string, string, error) {
	return "", "", "", api.ErrNotImplemented("fake")
}

func (f *fakeWork) BindLease(_ context.Context, _ *authz.Subject, project, id, session string) (orchapi.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "local:refuse" {
		return orchapi.Lease{}, api.Errorf(api.CodeConflict, "held by someone")
	}
	f.bound = append(f.bound, project+" "+id+" "+session)
	return orchapi.Lease{Key: id, Project: project, Holder: session}, nil
}

func (f *fakeWork) SessionEnded(_ context.Context, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, address)
	return nil
}

func (f *fakeWork) OwnerOf(context.Context, *authz.Subject, string, string) (string, error) {
	return "", nil
}

func (f *fakeWork) ClaimItem(context.Context, *authz.Subject, string, string, orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	return orchapi.ClaimResponse{}, nil
}

func (f *fakeWork) CommentTask(ctx context.Context, sub *authz.Subject, project, id, body string) error {
	if f.commentFn != nil {
		return f.commentFn(ctx, sub, project, id, body)
	}
	return api.NotFound("work item", id)
}

func (f *fakeWork) EndItemLease(context.Context, string, string, string, string) error { return nil }
func (f *fakeWork) ItemLeased(context.Context, string, string) (bool, error)           { return false, nil }

func (f *fakeWork) endedHas(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return contains(f.ended, addr)
}

type harness struct {
	t     *testing.T
	close func() // stops this daemon (the session module's Close, then the store)
	home  string
	d     *core.Deps
	s     *Service
	work  *fakeWork
	c     *client.Client // the owner on the socket
	sock  string
	// wrap, when set, wraps the socket handler's ResponseWriter (a test
	// acting between a handler's read and its write).
	wrap *atomic.Pointer[func(http.ResponseWriter) http.ResponseWriter]
}

// newHarness wires the session module with the real store, registry, bus and
// authentication behind a Unix socket, like the daemon (the listener marks
// its connections local the way server.markUnix does).
func newHarness(t *testing.T, mut func(*config.Config)) *harness {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	h := startHarness(t, home, mut, false)
	t.Cleanup(func() {
		h.close()
		_ = os.RemoveAll(home)
	})
	return h
}

// startHarness starts a daemon's session module on home; keep makes its
// sessions outlive it (as `zm daemon` does). The caller calls h.close.
func startHarness(t *testing.T, home string, mut func(*config.Config), keep bool) *harness {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "me", "mini"
	if mut != nil {
		mut(cfg)
	}
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Started: time.Now(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := New(d)
	s.Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, SwapUsedMB: 0, DiskFreeMB: 100000}
	}
	s.PromptQuiet = 100 * time.Millisecond
	fw := &fakeWork{}
	orchcore.Wire(d).Sessions, orchcore.Wire(d).Work = s, fw
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, addr string) bool { return d.Visible(ctx, sc, addr) })
	r := core.NewRouter()
	s.Routes(r)
	wrap := &atomic.Pointer[func(http.ResponseWriter) http.ResponseWriter]{}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f := wrap.Load(); f != nil {
			w = (*f)(w)
		}
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		r.ServeHTTP(w, req.WithContext(d.Authenticate(req)))
	})
	s.KeepSessions(keep)
	sock := filepath.Join(home, "b.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		if _, ok := c.(*net.UnixConn); ok {
			return core.WithLocal(ctx)
		}
		return ctx
	}}
	go func() { _ = srv.Serve(ln) }()
	sctx, cancel := context.WithCancel(ctx)
	if err := s.Start(sctx); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = s.Close()
			_ = srv.Close()
			_ = st.Close()
		})
	}
	return &harness{t: t, close: stop, home: home, d: d, s: s, work: fw, c: client.NewUnix(sock), sock: sock, wrap: wrap}
}

func (h *harness) owner() *authz.Subject { return h.d.Projects.Local("") }

func (h *harness) project(slug string, paths ...string) api.Project {
	h.t.Helper()
	p, err := h.d.Projects.Create(context.Background(), h.owner(), api.NewProjectRequest{Slug: slug, RepoRules: api.RepoRules{Paths: paths}})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// spawn starts sh -c script as agent.
func (h *harness) spawn(agent, script string, mut func(*orchapi.SpawnRequest)) orchapi.Session {
	h.t.Helper()
	req := orchapi.SpawnRequest{Agent: agent, Command: []string{"/bin/sh", "-c", script}, Cwd: h.home}
	if mut != nil {
		mut(&req)
	}
	ss, err := orchclient.Spawn(context.Background(), h.c, req)
	if err != nil {
		h.t.Fatalf("spawn: %v", err)
	}
	return ss
}

// wait polls the session until ok or 10 s.
func (h *harness) wait(id string, what string, ok func(orchapi.Session) bool) orchapi.Session {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ss, err := orchclient.Session(context.Background(), h.c, id)
		if err == nil && ok(ss) {
			return ss
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("session %s never %s: %+v %v", id, what, ss, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// waitReleased waits for a session's bound leases to go: end records the
// session first, so a reader can see it ended a moment before they are released.
func (h *harness) waitReleased(address string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !h.work.endedHas(address) {
		if time.Now().After(deadline) {
			h.t.Fatalf("leases of %s never released", address)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ended(ss orchapi.Session) bool { return ss.State == orchapi.StateEnded && ss.Outcome != nil }

func TestSpawnAttachStop(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `echo "ready $ZM_SESSION scratch=$ZM_SCRATCH proj=[$ZM_PROJECT]"; exec cat`, func(r *orchapi.SpawnRequest) {
		r.Env = map[string]string{"ZM_PROJECT": "nope", "PATH": os.Getenv("PATH")}
		r.Title = "fake agent"
	})
	if !ss.Managed || ss.State != orchapi.StateRunning || ss.Project != api.ProjectNone || ss.PID == 0 ||
		ss.Address != "mini/"+ss.ID || ss.Launcher == "" || ss.Scratch != filepath.Join(h.home, "scratch", ss.ID) ||
		ss.Command[0] != "/bin/sh" || ss.Agent != "shell" {
		t.Fatalf("spawned: %+v", ss)
	}
	if fi, err := os.Stat(ss.Scratch); err != nil || !fi.IsDir() {
		t.Fatalf("scratch dir: %v", err)
	}
	h.waitOutput(ss.ID, "ready "+ss.Address+" scratch="+ss.Scratch+" proj=[]")

	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	readUntil(t, br, "ready "+ss.Address) // scrollback replayed
	if _, err := conn.Write([]byte("ping-from-attach\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, br, "ping-from-attach")
	if err := orchclient.Resize(ctx, h.c, ss.ID, orchapi.ResizeRequest{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	conn.Close() // detach: the session keeps running
	time.Sleep(100 * time.Millisecond)
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State == orchapi.StateEnded {
		t.Fatalf("detach ended the session: %+v", got)
	}
	if err := orchclient.Input(ctx, h.c, ss.Address, orchapi.InputRequest{Data: "typed-input", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "typed-input")
	if n := h.s.ManagedLive(ctx); n != 1 {
		t.Fatalf("managed live %d", n)
	}

	got, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !ended(got) || got.Outcome.Kind != orchapi.OutcomeKilled || got.Outcome.Signal != "TERM" || got.Ended == nil {
		t.Fatalf("after stop: %+v %+v", got, got.Outcome)
	}
	if !h.work.endedHas(ss.Address) {
		t.Fatal("work.SessionEnded not called")
	}
	// Output survives the end (from disk); a second stop is a conflict.
	o, err := orchclient.Output(ctx, h.c, ss.ID, 0)
	if err != nil || !strings.Contains(o.Data, "typed-input") {
		t.Fatalf("output after end: %v %q", err, o.Data)
	}
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("stop ended: %v", err)
	}
	if h.s.ManagedLive(ctx) != 0 {
		t.Fatal("still counted live")
	}
	// Default list hides ended sessions; --all shows them.
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{}); len(l) != 0 {
		t.Fatalf("list: %+v", l)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 1 {
		t.Fatalf("list all: %+v", l)
	}
}

func (h *harness) waitOutput(id, want string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		o, err := orchclient.Output(context.Background(), h.c, id, 0)
		if err == nil && strings.Contains(o.Data, want) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("output never had %q: %q %v", want, o.Data, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func readUntil(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if strings.Contains(b.String(), want) || err != nil {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, want) {
			t.Fatalf("attach stream ended without %q: %q", want, s)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("attach never showed %q", want)
	}
}

func TestTypedOutcomes(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		agent, script string
		kind          orchapi.OutcomeKind
		code          int
	}{
		{"claude", `echo "Claude AI usage limit reached|1761868800"; exit 0`, orchapi.OutcomeQuota, 0},
		{"codex", `echo "ERROR: You've hit your usage limit."; exit 1`, orchapi.OutcomeQuota, 1},
		{"claude", `echo "Invalid API key - Please run /login"; exit 1`, orchapi.OutcomeAuth, 1},
		{"shell", `echo working; exit 3`, orchapi.OutcomeFailed, 3},
		{"shell", `echo all good; exit 0`, orchapi.OutcomeOK, 0},
	}
	for _, c := range cases {
		ss := h.spawn(c.agent, c.script, nil)
		got := h.wait(ss.ID, "ended", ended)
		o := got.Outcome
		if o.Kind != c.kind || o.ExitCode == nil || *o.ExitCode != c.code {
			t.Errorf("%s %q: %+v", c.agent, c.script, o)
		}
	}
	// A process killed from outside zm is killed, with the signal.
	ss := h.spawn("shell", `exec sleep 30`, nil)
	time.Sleep(100 * time.Millisecond)
	_ = exec.Command("kill", "-KILL", itoa(ss.PID)).Run()
	got := h.wait(ss.ID, "ended", ended)
	if got.Outcome.Kind != orchapi.OutcomeKilled || got.Outcome.Signal != "KILL" {
		t.Errorf("outside kill: %+v", got.Outcome)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// GET /v0/sessions/admission probes the caller's directory, named by
// Zm-Cwd or (rename step 1) Potluck-Cwd, the new name first as kit reads it.
func TestAdmissionCwdHeaderNames(t *testing.T) {
	h := newHarness(t, nil)
	var mu sync.Mutex
	var got []string
	h.s.Probe = func(dirs ...string) orchapi.AdmissionProbe {
		if len(dirs) == 2 { // the admission call's (home, cwd)
			mu.Lock()
			got = slices.Clone(dirs)
			mu.Unlock()
		}
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
	}
	newName := api.HeaderNew(api.HeaderCwd)
	for _, c := range []struct {
		name string
		hdr  http.Header
		want string
	}{
		{"old name", http.Header{api.HeaderCwd: {"/old"}}, "/old"},
		{"new name", http.Header{newName: {"/new"}}, "/new"},
		{"both: new first", http.Header{api.HeaderCwd: {"/old"}, newName: {"/new"}}, "/new"},
		{"new name empty", http.Header{api.HeaderCwd: {"/old"}, newName: {""}}, "/old"},
		{"neither", http.Header{}, ""},
	} {
		mu.Lock()
		got = nil
		mu.Unlock()
		req := httptest.NewRequest(http.MethodGet, "/v0/sessions/admission", nil)
		req.Header = c.hdr
		rec := httptest.NewRecorder()
		h.s.hAdmission(rec, req)
		mu.Lock()
		dirs := got
		mu.Unlock()
		if rec.Code != http.StatusOK || len(dirs) != 2 || dirs[0] != h.d.Config.Home || dirs[1] != c.want {
			t.Errorf("%s: %d probed %q, want [home %q]", c.name, rec.Code, dirs, c.want)
		}
	}
}

func TestAdmissionRefused(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	evs, cancel := h.d.Events.Subscribe(h.d.System(), events.Filter{Types: []string{orchapi.EventSessionAdmissionRefused}}, 16)
	defer cancel()
	h.s.Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 12, SwapUsedMB: 6100, DiskFreeMB: 80000}
	}
	_, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exit 0"}, Cwd: h.home})
	var e *api.Error
	if !api.IsCode(err, api.CodeUnavailable) || !asErr(err, &e) || !strings.Contains(e.Message, "memory free 12% < 20%") {
		t.Fatalf("refusal: %v", err)
	}
	var adm orchapi.Admission
	if b, _ := json.Marshal(e.Data); json.Unmarshal(b, &adm) != nil || adm.OK || adm.Reason != orchapi.AdmitMemory || adm.Probe.SwapUsedMB != 6100 {
		t.Fatalf("refusal data: %+v", e.Data)
	}
	select {
	case ev := <-evs:
		if ev.Project != api.ProjectNone {
			t.Fatalf("admission event must be machine-level: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no admission_refused event")
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 0 {
		t.Fatalf("a refused spawn left a session: %+v", l)
	}
	a, err := orchclient.Admission(ctx, h.c)
	if err != nil || a.OK || a.Reason != orchapi.AdmitMemory {
		t.Fatalf("GET admission: %+v %v", a, err)
	}
	// The session cap counts live managed sessions.
	h.s.Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
	}
	h.d.Config.File.Admission.MaxManagedSessions = 1
	ss := h.spawn("shell", "exec sleep 30", nil)
	_, err = orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exit 0"}, Cwd: h.home})
	if !asErr(err, &e) || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "1 managed sessions alive") {
		t.Fatalf("session cap: %v", err)
	}
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State == orchapi.StateEnded {
		t.Fatal("a refusal must never kill anything")
	}
}

func asErr(err error, e **api.Error) bool {
	x, ok := err.(*api.Error)
	if ok {
		*e = x
	}
	return ok
}

func TestIdleTimer(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.File.IdleAfterSeconds = 1 })
	ss := h.spawn("shell", `echo hi; exec cat`, nil)
	h.wait(ss.ID, "idle", func(s orchapi.Session) bool { return s.State == orchapi.StateIdle && s.Note == "no output" })
	if err := orchclient.Input(context.Background(), h.c, ss.ID, orchapi.InputRequest{Data: "wake", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.wait(ss.ID, "running again", func(s orchapi.Session) bool { return s.State == orchapi.StateRunning })
	// A hook-set idle (Stop) is not undone by output.
	if _, err := orchclient.SetState(context.Background(), h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateIdle, Note: "turn done"}); err != nil {
		t.Fatal(err)
	}
	_ = orchclient.Input(context.Background(), h.c, ss.ID, orchapi.InputRequest{Data: "more", Enter: true})
	time.Sleep(300 * time.Millisecond)
	if got, _ := orchclient.Session(context.Background(), h.c, ss.ID); got.State != orchapi.StateIdle || got.Note != "turn done" {
		t.Fatalf("hook state overridden: %+v", got)
	}
	if _, err := orchclient.SetState(context.Background(), h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateEnded}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("state ended via hook: %v", err)
	}
}

// A permission prompt's needs_human (Notification hook) clears once the
// human answers and the agent prints again; output alone (a redraw) does
// not, and a mail-set needs_human is never cleared by the terminal.
func TestPermissionPromptAnswered(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("claude", `(while :; do printf .; sleep 0.1; done) & exec cat`, nil)
	if _, err := orchclient.SetState(ctx, h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "Claude needs your permission to use Bash"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // output keeps coming
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateNeedsHuman {
		t.Fatalf("output alone cleared the prompt: %+v", got)
	}
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "1", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.wait(ss.ID, "running after the answer", func(s orchapi.Session) bool { return s.State == orchapi.StateRunning && s.Note == "" })

	// The same through register (the hook's other door).
	if _, _, err := h.s.Register(core.WithSubject(ctx, h.owner()), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-perm", Session: ss.Address, State: orchapi.StateNeedsHuman, Note: "permission"}); err != nil {
		t.Fatal(err)
	}
	_ = orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "2", Enter: true})
	h.wait(ss.ID, "running after the register-set answer", func(s orchapi.Session) bool { return s.State == orchapi.StateRunning })

	// Mail-set needs_human: input and output never clear it.
	if err := h.s.SetState(ctx, ss.Address, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "waiting on mail m1"}); err != nil {
		t.Fatal(err)
	}
	_ = orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "more", Enter: true})
	time.Sleep(400 * time.Millisecond)
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateNeedsHuman || got.Note != "waiting on mail m1" {
		t.Fatalf("mail-set needs_human cleared by the terminal: %+v", got)
	}
	// Hook then mail: mail wins.
	if _, err := orchclient.SetState(ctx, h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "permission"}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.SetState(ctx, ss.Address, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "waiting on mail m2"}); err != nil {
		t.Fatal(err)
	}
	_ = orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "y", Enter: true})
	time.Sleep(400 * time.Millisecond)
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateNeedsHuman || got.Note != "waiting on mail m2" {
		t.Fatalf("mail-set needs_human (after a hook one) cleared: %+v", got)
	}
}

// A conversation that ran under `zm run` and is resumed in the user's own
// terminal becomes a new unmanaged session; a later `zm run` resuming it
// takes the runtime id back.
func TestRegisterAfterManagedEnd(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	m := h.spawn("claude", "read x", nil)
	if got, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-resume", Session: m.Address, State: orchapi.StateRunning}); err != nil || got.RuntimeID != "rt-resume" {
		t.Fatalf("hook in managed: %+v %v", got, err)
	}
	_ = orchclient.Input(ctx, h.c, m.ID, orchapi.InputRequest{Data: "bye", Enter: true})
	h.wait(m.ID, "ended", ended)

	// claude --resume rt-resume in a plain terminal.
	u, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-resume", Cwd: h.home, State: orchapi.StateRunning})
	if err != nil || u.Address == m.Address || u.Managed || u.State != orchapi.StateRunning || u.RuntimeID != "rt-resume" {
		t.Fatalf("resume after managed end: %+v %v (managed was %s)", u, err, m.Address)
	}
	if got, _ := orchclient.Session(ctx, h.c, m.ID); got.State != orchapi.StateEnded || got.RuntimeID != "" {
		t.Fatalf("ended managed row: %+v", got)
	}
	// Hooks keep reaching the new session (idempotent).
	again, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-resume", State: orchapi.StateNeedsHuman, Note: "permission"})
	if err != nil || again.Address != u.Address || again.State != orchapi.StateNeedsHuman {
		t.Fatalf("second hook: %+v %v", again, err)
	}
	// A session-end hook for an ended managed conversation creates nothing.
	m2 := h.spawn("claude", "read x", nil)
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-2", Session: m2.Address}); err != nil {
		t.Fatal(err)
	}
	_ = orchclient.Input(ctx, h.c, m2.ID, orchapi.InputRequest{Data: "bye", Enter: true})
	h.wait(m2.ID, "ended", ended)
	if got, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-2", End: true}); err != nil || got.Address != m2.Address {
		t.Fatalf("end after managed end: %+v %v", got, err)
	}

	// The unmanaged one ends; resuming it under zm run moves the id there.
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-resume", End: true}); err != nil {
		t.Fatal(err)
	}
	m3 := h.spawn("claude", "exec sleep 30", nil)
	got, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-resume", Session: m3.Address, State: orchapi.StateIdle})
	if err != nil || got.Address != m3.Address || got.RuntimeID != "rt-resume" {
		t.Fatalf("resume under zm run: %+v %v", got, err)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 4 {
		t.Fatalf("want 4 sessions (m, u, m2, m3), got %d: %+v", len(l), l)
	}
}

func TestRegisterAndScope(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	repo := t.TempDir()
	harbor := h.project("harbor", repo)
	if _, err := h.d.Projects.AddArea(ctx, h.owner(), "harbor", api.Area{Name: "solver", RepoRules: api.RepoRules{Paths: []string{filepath.Join(repo, "solver")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Projects.AddArea(ctx, h.owner(), "harbor", api.Area{Name: "billing"}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(repo, "solver", "x"), 0o755)

	// cwd resolution, idempotent per (agent, runtime_id).
	s1, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4BB7545A-aaaa", Cwd: filepath.Join(repo, "solver", "x"), Title: "VISIBLE"})
	if err != nil || s1.ID != "4bb7545a" || s1.Project != harbor.ID || s1.Area != "solver" || s1.Managed || s1.State != orchapi.StateRunning {
		t.Fatalf("register: %+v %v", s1, err)
	}
	again, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4BB7545A-aaaa", State: orchapi.StateIdle, PID: os.Getpid()})
	if err != nil || again.Address != s1.Address || again.State != orchapi.StateIdle || again.PID != os.Getpid() {
		t.Fatalf("register again: %+v %v", again, err)
	}
	// Same session8 prefix, different runtime id: a different session.
	s2, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4bb7545a-bbbb", Cwd: repo})
	if err != nil || s2.ID == s1.ID || s2.Area != "" || s2.Project != harbor.ID {
		t.Fatalf("collision: %+v %v", s2, err)
	}
	// Nothing resolves: project none, never an error.
	s3, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "zz-thread", Cwd: t.TempDir()})
	if err != nil || s3.Project != api.ProjectNone {
		t.Fatalf("none: %+v %v", s3, err)
	}
	// An explicit project that does not exist still registers (in none).
	cp := *h.c
	cp.Project, cp.ProjectFrom = "nope", "flag"
	if s4, err := orchclient.Register(ctx, &cp, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "t4"}); err != nil || s4.Project != api.ProjectNone {
		t.Fatalf("explicit missing: %+v %v", s4, err)
	}
	// The frozen seeding body: {agent, runtime_id, title} with Zm-Project slug/area.
	cp.Project = "harbor/billing"
	hidden, err := orchclient.Register(ctx, &cp, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "cccccccc-uuid", Title: "HIDDEN"})
	if err != nil || hidden.Area != "billing" {
		t.Fatalf("flag register: %+v %v", hidden, err)
	}

	// A guest of harbor/solver sees only solver sessions; hidden ones answer like missing ones.
	tok, err := h.d.Projects.Mint(ctx, h.owner(), api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	g := client.NewUnix(h.sock)
	g.Token = tok.Secret
	l, err := orchclient.Sessions(ctx, g, orchapi.SessionQuery{All: true})
	if err != nil || len(l) != 1 || l[0].Address != s1.Address {
		t.Fatalf("guest list: %+v %v", l, err)
	}
	_, errH := orchclient.Session(ctx, g, hidden.ID)
	_, errM := orchclient.Session(ctx, g, "zzzzzzzz")
	if !api.IsCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), hidden.ID, "X") != strings.ReplaceAll(errM.Error(), "zzzzzzzz", "X") {
		t.Fatalf("hidden vs missing: %v / %v", errH, errM)
	}
	if _, err := orchclient.Session(ctx, g, s1.ID); err != nil {
		t.Fatalf("guest get visible: %v", err)
	}
	if _, err := orchclient.Stop(ctx, g, s1.ID, orchapi.StopRequest{}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest stop: %v", err)
	}
	sc := authz.FromTuples([]authz.Tuple{{Subject: "x", Relation: authz.Guest, Object: authz.AreaObject(harbor.ID, "solver")}}, nil)
	if !h.s.Visible(ctx, sc, s1.Address) || h.s.Visible(ctx, sc, hidden.Address) || h.s.Visible(ctx, sc, s3.Address) || h.s.Visible(ctx, sc, "mini/nothere") {
		t.Fatal("Visible")
	}

	// SessionEnd hook: ended ok; a later register (resumed conversation) runs again.
	endd, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4BB7545A-aaaa", End: true, Note: "logout"})
	if err != nil || endd.State != orchapi.StateEnded || endd.Outcome == nil || endd.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("end: %+v %v", endd, err)
	}
	res, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4BB7545A-aaaa", State: orchapi.StateRunning})
	if err != nil || res.State != orchapi.StateRunning || res.Outcome != nil {
		t.Fatalf("resume: %+v %v", res, err)
	}
	// The reaper ends sessions whose pid is gone, never as ok.
	dead := exec.Command("sleep", "30")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	gone, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "gone-1", PID: dead.Process.Pid})
	_ = dead.Process.Kill()
	_, _ = dead.Process.Wait()
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, gone.ID); got.State != orchapi.StateEnded || got.Outcome.Kind != orchapi.OutcomeFailed {
		t.Fatalf("reaped: %+v", got)
	}
	if !h.work.endedHas(gone.Address) {
		t.Fatal("reaped session's leases not released")
	}
}

func TestHookInsideManagedSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("claude", "exec sleep 30", nil)
	got, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-123", Session: ss.Address, State: orchapi.StateNeedsHuman, Note: "permission"})
	if err != nil || got.Address != ss.Address || got.State != orchapi.StateNeedsHuman || got.RuntimeID != "rt-123" {
		t.Fatalf("hook in managed: %+v %v", got, err)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 1 {
		t.Fatalf("a second session was created: %+v", l)
	}
	// End is ignored for managed sessions (they end when the process exits).
	if got, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-123", Session: ss.Address, End: true}); got.State == orchapi.StateEnded {
		t.Fatal("hook ended a managed session")
	}
}

func TestSpawnInProjectWithWork(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	repo := t.TempDir()
	p := h.project("side", repo)
	ss := h.spawn("shell", "exit 0", func(r *orchapi.SpawnRequest) { r.Cwd, r.Work = repo, "local:abc123" })
	if ss.Project != p.ID || ss.Work != "local:abc123" {
		t.Fatalf("spawn: %+v", ss)
	}
	h.wait(ss.ID, "ended", ended)
	h.waitReleased(ss.Address)
	if len(h.work.bound) != 1 || h.work.bound[0] != p.ID+" local:abc123 "+ss.Address {
		t.Fatalf("work calls: %+v %+v", h.work.bound, h.work.ended)
	}
	// A refused lease leaves no session behind.
	_, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh"}, Cwd: repo, Work: "local:refuse"})
	if !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("refused lease: %v", err)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 1 {
		t.Fatalf("sessions: %+v", l)
	}
	// An explicit project that does not resolve is not_found, never project none.
	cp := *h.c
	cp.Project, cp.ProjectFrom = "nope", "flag"
	if _, err := orchclient.Spawn(ctx, &cp, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh"}, Cwd: repo}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("explicit missing: %v", err)
	}
	// A model is passed to known agents and recorded; refused for unknown ones.
	if _, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "mytool", Command: []string{"/bin/sh", "-c", "exit 0"}, Model: "opus", Cwd: repo}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("model for unknown agent: %v", err)
	}
	m := h.spawn("claude", "exit 0", func(r *orchapi.SpawnRequest) { r.Model = "opus" })
	if m.Model != "opus" || strings.Join(m.Command[len(m.Command)-2:], " ") != "--model opus" {
		t.Fatalf("model: %+v", m)
	}
}

func TestWorktreeAndGC(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	_ = os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "sub", "f.txt"), []byte("x"), 0o644)
	run(repo, "add", ".")
	run(repo, "commit", "-q", "-m", "init")

	a := h.spawn("shell", "pwd; exit 0", func(r *orchapi.SpawnRequest) { r.Cwd, r.Worktree = filepath.Join(repo, "sub"), true })
	if a.Worktree != filepath.Join(h.home, "worktrees", a.ID) || a.Branch != "zm/"+a.ID || a.Cwd != filepath.Join(a.Worktree, "sub") {
		t.Fatalf("worktree session: %+v", a)
	}
	h.wait(a.ID, "ended", ended)
	b := h.spawn("shell", "exit 0", func(r *orchapi.SpawnRequest) { r.Cwd, r.Worktree = repo, true })
	h.wait(b.ID, "ended", ended)
	run(b.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	_ = os.WriteFile(filepath.Join(b.Worktree, "dirty.txt"), []byte("x"), 0o644)

	gc, err := orchclient.SessionGC(ctx, h.c)
	if err != nil || len(gc) != 2 {
		t.Fatalf("gc: %+v %v", gc, err)
	}
	byS := map[string]orchapi.GCCandidate{}
	for _, c := range gc {
		byS[c.Session] = c
	}
	if c := byS[a.Address]; !c.Safe || c.Dirty || c.Ahead != 0 {
		t.Fatalf("clean worktree: %+v", c)
	}
	// The suggested removal runs from anywhere (dogfood: it needed the repo as cwd).
	if c := byS[a.Address]; c.Repo == "" || !strings.HasPrefix(c.Remove, "git -C ") {
		t.Fatalf("remove command: %+v", c)
	} else {
		cmd := exec.Command("sh", "-c", c.Remove)
		cmd.Dir = t.TempDir()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", c.Remove, err, out)
		}
		if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
			t.Fatal("the suggested command left the worktree")
		}
	}
	if c := byS[b.Address]; c.Remove != "" {
		t.Fatalf("an unsafe worktree got a remove command: %+v", c)
	}
	if c := byS[b.Address]; c.Safe || !c.Dirty || c.Ahead != 1 {
		t.Fatalf("dirty worktree: %+v", c)
	}
	// gc deletes nothing.
	if _, err := os.Stat(b.Worktree); err != nil {
		t.Fatal("gc deleted a worktree")
	}
	if _, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh"}, Cwd: t.TempDir(), Worktree: true}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("worktree outside git: %v", err)
	}
}

func TestSandbox(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	repo := t.TempDir()
	h.project("sbx", repo)
	bin := t.TempDir()
	path := bin + ":/usr/bin:/bin"
	_, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh"}, Cwd: repo, Sandbox: true, Env: map[string]string{"PATH": path}})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "srt") {
		t.Fatalf("no srt: %v", err)
	}
	// A fake srt that records its arguments.
	_ = os.WriteFile(filepath.Join(bin, "srt"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ZM_SCRATCH/srt-args\"\nexit 0\n"), 0o755)
	if _, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh"}, Cwd: t.TempDir(), Sandbox: true, Env: map[string]string{"PATH": path}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("sandbox without a project: %v", err)
	}
	ss := h.spawn("shell", "echo hi", func(r *orchapi.SpawnRequest) {
		r.Cwd, r.Sandbox, r.Env = repo, true, map[string]string{"PATH": path}
	})
	if ss.Sandbox == "" || ss.Command[0] != filepath.Join(bin, "srt") || ss.Command[1] != "--settings" {
		t.Fatalf("sandboxed: %+v", ss)
	}
	got := h.wait(ss.ID, "ended", ended)
	if got.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("outcome: %+v", got.Outcome)
	}
	args, _ := os.ReadFile(filepath.Join(ss.Scratch, "srt-args"))
	if !strings.Contains(string(args), "/bin/sh -c 'echo hi'") {
		t.Fatalf("srt args: %q", args)
	}
	var pol srtPolicy
	b, _ := os.ReadFile(ss.Sandbox)
	if err := json.Unmarshal(b, &pol); err != nil || !contains(pol.Filesystem.AllowWrite, project.ExpandPath(repo)) ||
		!contains(pol.Filesystem.AllowWrite, project.ExpandPath(ss.Scratch)) {
		t.Fatalf("policy: %s %v", b, err)
	}
}

func TestStartEndsLeftoverSessions(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	r := row{Session: orchapi.Session{Address: "mini/0ld5e551", ID: "0ld5e551", Machine: "mini", Project: api.ProjectNone,
		Agent: "claude", State: orchapi.StateRunning, Managed: true, PID: 1, Started: now, Updated: now}}
	if err := insertRow(ctx, h.d.Store.Registry(), r); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := h.s.Lookup(ctx, r.Address)
	if !ok || got.State != orchapi.StateEnded || got.Outcome.Kind != orchapi.OutcomeInfra || got.Outcome.Note != "daemon restarted" {
		t.Fatalf("leftover: %+v", got)
	}
	if !h.work.endedHas(r.Address) {
		t.Fatal("leftover leases not released")
	}
}

func TestNudge(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", "exec cat", nil)
	if ok, _ := h.s.Nudge(ctx, ss.Address, "anything"); ok {
		t.Fatal("nudged a running session")
	}
	if err := h.s.SetState(ctx, ss.Address, orchapi.StateRequest{State: orchapi.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.Nudge(ctx, ss.Address, "rm -rf / (a mail body)"); !ok || err != nil {
		t.Fatalf("nudge: %v %v", ok, err)
	}
	h.waitOutput(ss.ID, NudgeLine)
	if o, _ := orchclient.Output(ctx, h.c, ss.ID, 0); strings.Contains(o.Data, "rm -rf") {
		t.Fatal("a non-zm line was typed")
	}
	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Fatal("nudge not rate limited")
	}
}

func TestCloseEndsWithInfra(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.spawn("shell", "trap '' TERM; exec sleep 30", nil)
	time.Sleep(100 * time.Millisecond)
	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	got, ok := h.s.Lookup(context.Background(), ss.Address)
	if !ok || got.State != orchapi.StateEnded || got.Outcome.Kind != orchapi.OutcomeInfra || got.Outcome.Note != "daemon stopped" {
		t.Fatalf("after close: %+v %+v", got, got.Outcome)
	}
}

// With nobody attached, the daemon answers a cursor position query the way a
// terminal would, so a TTY program probing the terminal does not hang.
func TestCursorQueryAnsweredWhenUnattached(t *testing.T) {
	h := newHarness(t, nil)
	script := `stty raw -echo; printf '\033[6n'; r=$(dd bs=1 count=6 2>/dev/null | od -An -c | tr -d ' \n'); echo "got:$r"; exit 0`
	ss := h.spawn("shell", script, nil)
	h.wait(ss.ID, "ended", ended)
	out, err := orchclient.Output(context.Background(), h.c, ss.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Data, `got:033[1;1R`) {
		t.Errorf("no cursor reply seen by the program: %q", out.Data)
	}
}

func TestReview3RegisterCleansDisplayText(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	req := orchapi.RegisterRequest{Agent: "claude", RuntimeID: "review3", Title: "a\u200bb\u202e\U000e0061", Note: "n\u200bo", Version: "v\u200b1", Model: "m\u202e1"}
	s, err := orchclient.Register(ctx, h.c, req)
	if err != nil || s.Title != "ab" || s.Note != "no" || s.Version != "v1" || s.Model != "m1" {
		t.Fatalf("register %+v %v", s, err)
	}
	req.Title = "c\u200bd"
	s, err = orchclient.Register(ctx, h.c, req)
	if err != nil || s.Title != "cd" {
		t.Fatalf("reregister %+v %v", s, err)
	}
}

// TestAgentNice: an agent session and what it starts run at agent_nice
// (default 10), so an agent's build yields to the owner's UIs; a shell runs
// at the daemon's own priority, as does every session with agent_nice -1.
func TestAgentNice(t *testing.T) {
	niceOf := func(pid int) int {
		t.Helper()
		v, err := unix.Getpriority(unix.PRIO_PROCESS, pid)
		if err != nil {
			t.Fatalf("getpriority %d: %v", pid, err)
		}
		if runtime.GOOS == "linux" {
			return 20 - v // the raw syscall answers 20 - nice
		}
		return v
	}
	own := niceOf(0)
	tree := func(h *harness, id string) []int {
		t.Helper()
		p := h.s.proc(id)
		if p == nil {
			t.Fatalf("no proc for %s", id)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			table, err := procTable()
			if err != nil {
				t.Fatal(err)
			}
			pids := descendants(table, childrenOf(table), p.pid)
			if len(pids) >= 2 || time.Now().After(deadline) {
				return pids
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	check := func(h *harness, agent string, want int) {
		t.Helper()
		ss := h.spawn(agent, `sleep 30 & sleep 31; wait`, nil)
		pids := tree(h, ss.ID)
		if len(pids) < 2 {
			t.Fatalf("%s: tree %v, want the shell and its jobs", agent, pids)
		}
		for _, pid := range pids {
			if n := niceOf(pid); n != want {
				t.Errorf("%s: pid %d at nice %d, want %d (tree %v)", agent, pid, n, want, pids)
			}
		}
	}
	h := newHarness(t, nil)
	check(h, "claude", max(own, 10))
	check(h, "shell", own)
	off := newHarness(t, func(c *config.Config) { c.File.AgentNice = -1 })
	check(off, "claude", own)
}
