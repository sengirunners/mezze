package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// byProcess is POST path as the daemon's socket sees a caller with no Zm-As:
// the socket owner, with the test's own pid as its peer.
func (f *projectFixture) byProcess(t *testing.T, path string, body any) int {
	t.Helper()
	return f.byProcessDo(t, http.MethodPost, path, body)
}

func (f *projectFixture) byProcessDo(t *testing.T, method, path string, body any) int {
	t.Helper()
	r := core.NewRouter()
	f.h.s.Routes(r)
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	ctx := core.WithPeerPID(core.WithSubject(core.WithLocal(req.Context()), f.h.d.Projects.Local("")), os.Getpid())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req.WithContext(ctx))
	return w.Code
}

// Found by its process alone (no Zm-As), a capped session cannot set the
// state of, rename or stop another session by its id; it still does all
// three to its own.
func TestCappedSessionCannotWriteOthersByID(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	victim := f.register(t, "victim", f.capDir, "")
	// A shared structured session on another machine, in the team project
	// side: the owner may stop it there (stopFar).
	if _, err := f.h.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "far-box", Kind: api.IdentityMachine}); err != nil {
		t.Fatal(err)
	}
	beat := []orchapi.PresenceSession{{Address: "far/4bb7545a", Agent: "codex", State: orchapi.StateRunning, Started: time.Now(),
		Shared: true, Mode: orchapi.SessionModePipe}}
	if err := f.h.s.RecordPresence(core.WithSubject(ctx, f.h.d.Projects.Local("")), f.side.ID, "far", "far-box", beat); err != nil {
		t.Fatal(err)
	}
	shell := f.h.spawn("bshell", "exec sleep 60", func(r *orchapi.SpawnRequest) { r.Cwd = f.capDir })
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	// From now on the test's process is found as the capped session.
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	post := func(id, op string, body any) int {
		return f.byProcess(t, "/v0/sessions/"+url.PathEscape(id)+"/"+op, body)
	}
	for _, tc := range []struct {
		id, op string
		body   any
	}{
		{victim.ID, "state", orchapi.StateRequest{State: orchapi.StateIdle}},
		{victim.ID, "name", orchapi.NameRequest{Name: "taken"}},
		{shell.ID, "stop", orchapi.StopRequest{}},
	} {
		if code := post(tc.id, tc.op, tc.body); code != http.StatusNotFound {
			t.Fatalf("%s of another session: %d", tc.op, code)
		}
	}
	if r, _ := f.h.s.load(ctx, victim.ID); r.State != orchapi.StateRunning || r.Name != "" {
		t.Fatalf("victim written: %s %q", r.State, r.Name)
	}
	if r, _ := f.h.s.load(ctx, shell.ID); r.State == orchapi.StateEnded {
		t.Fatal("the shell was stopped by the capped session")
	}
	if code := post(capped.ID, "state", orchapi.StateRequest{State: orchapi.StateIdle}); code != http.StatusOK {
		t.Fatalf("own state: %d", code)
	}
	if code := post(capped.ID, "name", orchapi.NameRequest{Name: "mine"}); code != http.StatusOK {
		t.Fatalf("own name: %d", code)
	}
	if r, _ := f.h.s.load(ctx, capped.ID); r.State != orchapi.StateIdle || r.Name != "mine" {
		t.Fatalf("own writes not applied: %s %q", r.State, r.Name)
	}
	// Nor stop another machine's session (stopFar), which it would relay
	// as the owner; the owner's stop gets past the gate (here: no relay).
	if code := post("far/4bb7545a", "stop", orchapi.StopRequest{}); code != http.StatusNotFound {
		t.Fatalf("far stop: %d", code)
	}
	if _, err := orchclient.Stop(ctx, f.h.c, "far/4bb7545a", orchapi.StopRequest{}); err == nil || api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("owner's far stop never reached the relay: %v", err)
	}
	byProc := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid())
	if f.h.s.capGate(f.h.d.Projects.Local("")).uncapped(byProc) {
		t.Fatal("the capped process counts as uncapped")
	}
	// The owner, not found as the capped session, still writes them all.
	if !f.h.s.capGate(f.h.d.Projects.Local("")).uncapped(core.WithSubject(ctx, f.h.d.Projects.Local(""))) {
		t.Fatal("the owner counts as capped")
	}
	if _, err := orchclient.SetSessionName(ctx, f.h.c, victim.ID, "owner"); err != nil {
		t.Fatalf("owner's rename: %v", err)
	}
	if _, err := orchclient.SetState(ctx, f.h.c, victim.ID, orchapi.StateRequest{State: orchapi.StateIdle}); err != nil {
		t.Fatalf("owner's state: %v", err)
	}
	if _, err := orchclient.Stop(ctx, f.h.c, shell.ID, orchapi.StopRequest{}); err != nil {
		t.Fatalf("owner's stop: %v", err)
	}
}

// A session registered from inside a capped one is never visible without
// its cap and lineage: a hook raced into that window would find it by its
// pid as uncapped.
func TestRegisteredSessionVisibleOnlyCapped(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	seen := 0
	registeredSeam = func(address string) {
		seen++
		a, _ := api.ParseAddress(address)
		var parent string
		_ = f.h.d.Store.Registry().QueryRowContext(ctx, `SELECT parent FROM session_lineage WHERE id = ?`, a.Session).Scan(&parent)
		if _, _, ok := f.h.s.SessionCap(address); !ok || parent != capped.Address {
			t.Errorf("%s is visible with cap %v and parent %q", address, ok, parent)
		}
		if len(f.h.s.chainCaps(ctx, address)) == 0 {
			t.Errorf("%s is visible uncapped", address)
		}
	}
	t.Cleanup(func() { registeredSeam = nil })
	child := f.register(t, "child", f.capDir, capped.Address)
	if seen != 1 {
		t.Fatalf("seam ran %d times", seen)
	}
	if p, _, ok := f.h.s.SessionCap(child.Address); !ok || p != f.side.ID {
		t.Fatalf("child cap: %q %v", p, ok)
	}
}

// Found by its process alone (no Zm-As), a capped session cannot type into,
// resize, attach to, read, move, prompt or drive another session by its id,
// here or on another machine; the owner's same calls get past the gate.
func TestCappedSessionCannotOperateOthersByID(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	if _, err := f.h.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "far-box", Kind: api.IdentityMachine}); err != nil {
		t.Fatal(err)
	}
	beat := []orchapi.PresenceSession{{Address: "far/4bb7545a", Agent: "codex", State: orchapi.StateRunning, Started: time.Now(),
		Shared: true, Mode: orchapi.SessionModePipe}}
	if err := f.h.s.RecordPresence(core.WithSubject(ctx, f.h.d.Projects.Local("")), f.side.ID, "far", "far-box", beat); err != nil {
		t.Fatal(err)
	}
	shell := f.h.spawn("bshell", "exec sleep 60", func(r *orchapi.SpawnRequest) { r.Cwd = f.capDir })
	routes := []struct {
		method, id, op string
		body           any
	}{
		{http.MethodPost, shell.ID, "input", map[string]any{"data": "x"}},
		{http.MethodPost, shell.ID, "resize", map[string]any{"rows": 10, "cols": 10}},
		{http.MethodGet, shell.ID, "attach", nil},
		{http.MethodGet, shell.ID, "output", nil},
		{http.MethodPost, shell.ID, "project", map[string]any{"project": "none"}},
		{http.MethodPost, shell.ID, "prompt", map[string]any{"text": "hello"}},
		{http.MethodPost, shell.ID, "turn", map[string]any{"text": "hello"}},
		{http.MethodPost, shell.ID, "interrupt", map[string]any{}},
		{http.MethodPost, shell.ID, "answer", map[string]any{"request": "r", "decision": "allow"}},
		{http.MethodGet, shell.ID, "events", nil},
		{http.MethodPost, "far/4bb7545a", "prompt", map[string]any{"text": "hello"}},
		{http.MethodPost, "far/4bb7545a", "turn", map[string]any{"text": "hello"}},
		{http.MethodPost, "far/4bb7545a", "interrupt", map[string]any{}},
		{http.MethodGet, "far/4bb7545a", "events", nil},
	}
	call := func(method, id, op string, body any) int {
		return f.byProcessDo(t, method, "/v0/sessions/"+url.PathEscape(id)+"/"+op, body)
	}
	// The owner first (no capped session yet): every route but attach (an
	// upgrade the recorder cannot take) gets past the not-found of a gate.
	for _, tc := range routes {
		if tc.op == "attach" || tc.op == "project" {
			continue
		}
		if code := call(tc.method, tc.id, tc.op, tc.body); code == http.StatusNotFound {
			t.Fatalf("owner's %s %s: %d", tc.op, tc.id, code)
		}
	}
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range routes {
		if code := call(tc.method, tc.id, tc.op, tc.body); code != http.StatusNotFound {
			t.Fatalf("capped process's %s of %s: %d", tc.op, tc.id, code)
		}
	}
	// A process outside the capped tree (the test's grandparent) is still
	// the owner while the capped one runs.
	if gp, ok := procStat(os.Getppid()); ok && gp.ppid > 1 {
		h := core.NewRouter()
		f.h.s.Routes(h)
		for _, tc := range routes {
			if tc.op == "attach" || tc.op == "project" {
				continue
			}
			var b []byte
			if tc.body != nil {
				b, _ = json.Marshal(tc.body)
			}
			req := httptest.NewRequest(tc.method, "/v0/sessions/"+url.PathEscape(tc.id)+"/"+tc.op, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(api.HeaderProtocol, api.Protocol)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req.WithContext(core.WithPeerPID(core.WithSubject(core.WithLocal(req.Context()), f.h.d.Projects.Local("")), gp.ppid)))
			if w.Code == http.StatusNotFound {
				t.Fatalf("owner's %s %s while a capped session runs: %d", tc.op, tc.id, w.Code)
			}
		}
	}
	if r, _ := f.h.s.load(ctx, shell.ID); r.Project != shell.Project || r.State == orchapi.StateEnded {
		t.Fatalf("the shell was moved or ended: %q %s", r.Project, r.State)
	}
	// Its own session it still reads.
	if code := call(http.MethodGet, capped.ID, "output", nil); code == http.StatusNotFound {
		t.Fatalf("own output: %d", code)
	}
}
