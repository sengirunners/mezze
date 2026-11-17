package session

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/core"
)

// CallerSession finds the registered session the peer process runs inside,
// and errs (so callers refuse) when the peer cannot be read or is gone.
func TestCallerSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	owner := h.d.Projects.Local("")
	byPID := func(pid int) context.Context { return core.WithPeerPID(core.WithSubject(ctx, owner), pid) }

	if got, err := h.s.CallerSession(byPID(os.Getpid()), owner); err != nil || got != "" {
		t.Fatalf("no session above the caller: %q, %v", got, err)
	}
	if _, err := h.s.CallerSession(byPID(-1), owner); err == nil {
		t.Fatal("an unreadable peer was not an error")
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.CallerSession(byPID(dead.Process.Pid), owner); err == nil {
		t.Fatal("a peer that no longer exists was not an error")
	}
	sub := *owner
	sub.Session = "laptop/s-made-up"
	if got, err := h.s.CallerSession(core.WithSubject(ctx, &sub), &sub); err != nil || got != sub.Session {
		t.Fatalf("an in-process caller is its subject's session: %q, %v", got, err)
	}

	// The test's parent becomes a session: the test process runs inside it.
	s, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "caller-parent", State: orchapi.StateRunning, PID: os.Getppid()})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.CallerSession(byPID(os.Getpid()), owner); err != nil || got != s.Address {
		t.Fatalf("a child of a session: %q, %v, want %s", got, err, s.Address)
	}
}
