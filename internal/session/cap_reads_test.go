package session

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// Found by its process alone (no Zm-As), a capped session cannot read
// another session by its id: the session itself, its activity, subagents
// or prompt log, here or on another machine; its own it still reads.
func TestCappedSessionCannotReadOthersByID(t *testing.T) {
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
	victim := f.register(t, "victim", f.capDir, "")
	shell := f.h.spawn("bshell", "exec sleep 60", func(r *orchapi.SpawnRequest) { r.Cwd = f.capDir })
	var paths []string
	for _, id := range []string{victim.ID, shell.ID} {
		for _, op := range []string{"", "/activity", "/subagents", "/prompts"} {
			paths = append(paths, "/v0/sessions/"+url.PathEscape(id)+op)
		}
	}
	paths = append(paths, "/v0/sessions/"+url.PathEscape("far/4bb7545a"), "/v0/sessions/"+url.PathEscape("far/4bb7545a")+"/prompts")
	get := func(p string) int { return f.byProcessDo(t, http.MethodGet, p, nil) }
	for _, p := range paths {
		if code := get(p); code == http.StatusNotFound {
			t.Fatalf("owner's GET %s: %d", p, code)
		}
	}
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if code := get(p); code != http.StatusNotFound {
			t.Fatalf("capped process's GET %s: %d", p, code)
		}
	}
	for _, op := range []string{"", "/activity", "/subagents", "/prompts"} {
		if code := get("/v0/sessions/" + capped.ID + op); code == http.StatusNotFound {
			t.Fatalf("own GET %s: %d", op, code)
		}
	}
	// The owner (not found by process) still reads.
	if _, err := orchclient.Session(ctx, f.h.c, victim.ID); err != nil {
		t.Fatalf("owner's read: %v", err)
	}
}

// When the caps cannot load (a failed registry read at start), every cap
// check fails closed until a load succeeds: a capped process is still
// refused, and once they load it stays refused.
func TestCapsLoadFailureFailsClosed(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	victim := f.register(t, "victim", f.capDir, "")
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	// As after a daemon restart whose caps load fails.
	f.h.s.rcaps = remoteCaps{loadSeam: func() error { return errors.New("injected: registry unreadable") }}
	if !f.h.s.anyCaps() || !f.h.s.anyLiveCaps(ctx) {
		t.Fatal("caps that could not load count as none")
	}
	if p, _, ok := f.h.s.SessionCap(capped.Address); !ok || p != unloadedProject {
		t.Fatalf("an unknown cap is not capped to nothing: %q %v", p, ok)
	}
	state := func() int {
		return f.byProcess(t, "/v0/sessions/"+victim.ID+"/state", orchapi.StateRequest{State: orchapi.StateIdle})
	}
	if code := state(); code != http.StatusNotFound {
		t.Fatalf("capped process's state of another session with caps unloaded: %d", code)
	}
	byProc := core.WithPeerPID(core.WithSubject(ctx, f.h.d.Projects.Local("")), os.Getpid())
	if _, _, err := f.h.s.Register(byProc, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "victim", State: orchapi.StateIdle}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("capped process's hook on another session with caps unloaded: %v", err)
	}
	// The next call retries the load; it succeeds, and the cap holds.
	f.h.s.rcaps.loadSeam = nil
	if p, _, ok := f.h.s.SessionCap(capped.Address); !ok || p != f.side.ID {
		t.Fatalf("cap after the retried load: %q %v", p, ok)
	}
	if code := state(); code != http.StatusNotFound {
		t.Fatalf("capped process's state of another session: %d", code)
	}
	if r, _ := f.h.s.load(ctx, victim.ID); r.State != orchapi.StateRunning {
		t.Fatalf("victim written: %s", r.State)
	}
	if _, err := orchclient.SetState(ctx, f.h.c, victim.ID, orchapi.StateRequest{State: orchapi.StateIdle}); err != nil {
		t.Fatalf("owner's state: %v", err)
	}
}

// A capped row recorded with no process start (pid_start 0) gets its
// process's start on its next write, and a lingering entry with none is
// pinned to the process at its pid when loaded: the cap never follows a
// later process that reuses the pid. Exited entries are
// pruned whenever one is looked up or added.
func TestLingeringCapPinsProcessStart(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	ppid := os.Getppid()
	st, ok := procStat(ppid)
	if !ok || st.start == 0 {
		t.Skip("no process start readable here")
	}
	capped := f.register(t, "capped", f.capDir, "")
	f.capTo(t, capped.Address, f.side)
	if _, err := orchclient.Register(ctx, f.h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "capped", Cwd: f.capDir, State: orchapi.StateRunning, PID: ppid}); err != nil {
		t.Fatal(err)
	}
	reg := f.h.d.Store.Registry()
	if _, err := reg.ExecContext(ctx, `UPDATE sessions SET pid_start = 0 WHERE id = ?`, capped.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.s.update(ctx, capped.ID, "", func(r *row) (bool, error) { r.Title = "pinned"; return true, nil }); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := reg.QueryRowContext(ctx, `SELECT pid_start FROM sessions WHERE id = ?`, capped.ID).Scan(&got); err != nil || got != st.start {
		t.Fatalf("capped row's pid_start after a write: %d (%v), want %d", got, err, st.start)
	}
	// An ended row recorded before the fix, with no start, then a restart.
	if _, err := reg.ExecContext(ctx, `UPDATE sessions SET pid_start = 0, state = ? WHERE id = ?`, string(orchapi.StateEnded), capped.ID); err != nil {
		t.Fatal(err)
	}
	f.h.s.lingering = lingerSet{}
	if a, err := f.h.s.lingerAt(ppid, st.start); err != nil || a != capped.Address {
		t.Fatalf("the process itself is not found as its capped session: %q %v", a, err)
	}
	if a, err := f.h.s.lingerAt(ppid, st.start+1); err != nil || a != "" {
		t.Fatalf("a later process at the same pid is found as the capped session: %q %v", a, err)
	}
	// Exited entries go.
	const gone = 1 << 30 // no such pid
	f.h.s.lingering.mu.Lock()
	f.h.s.lingering.pids[gone] = []lingerProc{{"x/00000000", 7}}
	f.h.s.lingering.mu.Unlock()
	if a, _ := f.h.s.lingerAt(gone, 7); a != "" {
		t.Fatalf("an exited process lingers: %q", a)
	}
	f.h.s.lingering.mu.Lock()
	_, kept := f.h.s.lingering.pids[gone]
	f.h.s.lingering.mu.Unlock()
	if kept {
		t.Fatal("lingerAt kept an exited entry")
	}
}
