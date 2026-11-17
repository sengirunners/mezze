package session

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// A running hook session no hook has reached for bound_lease_ttl goes idle
// (its bound leases stop renewing: a dead agent never holds work for ever);
// one heard from recently stays running, and the next hook runs it again.
func TestQuietHookSessionGoesIdle(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	quiet, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "quiet-1", State: orchapi.StateRunning})
	busy, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "busy-1", State: orchapi.StateRunning})
	ttl := h.s.d.Config.BoundLeaseTTL()
	later := time.Now().Add(ttl + time.Minute)
	h.s.hooked.Store(busy.ID, later.Add(-time.Minute)) // a hook a minute before
	h.s.quietHooks(time.Now().Add(ttl - time.Minute))
	if got, _ := orchclient.Session(ctx, h.c, quiet.ID); got.State != orchapi.StateRunning {
		t.Fatalf("idled before bound_lease_ttl: %+v", got)
	}
	h.s.quietHooks(later)
	if got, _ := orchclient.Session(ctx, h.c, quiet.ID); got.State != orchapi.StateIdle || got.Note != "no hooks" {
		t.Fatalf("quiet session: %+v", got)
	}
	if got, _ := orchclient.Session(ctx, h.c, busy.ID); got.State != orchapi.StateRunning {
		t.Fatalf("a session heard from a minute ago: %+v", got)
	}
	if got, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "quiet-1", State: orchapi.StateRunning}); got.State != orchapi.StateRunning {
		t.Fatalf("the next hook: %+v", got)
	}
}

// register() notes every hook it serves (hooked), so a hook that changes
// nothing in the store (the same state again) still counts as the session
// being alive: without the note the session would go idle and its bound
// leases lapse while its agent works.
func TestRegisterRecordsHook(t *testing.T) {
	h := newHarness(t, nil)
	h.s.d.Config.File.BoundLeaseTTLSeconds = 1
	ctx := context.Background()
	ss, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "hooked-1", State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	// The same state: no row change, only the note.
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "hooked-1", State: orchapi.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.s.hooked.Load(ss.ID); !ok {
		t.Fatal("register did not note the hook")
	}
	h.s.quietHooks(time.Now())
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateRunning {
		t.Fatalf("a session hooked just now went idle: %+v", got)
	}
}
