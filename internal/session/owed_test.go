package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// What a session still owes (SPEC 8): the sessions it started that still run,
// by Parent; an ended child is not owed, and nobody else's child is.
func TestOwedLanesAreLiveChildren(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	ctx := context.Background()
	child, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	sub := h.d.Projects.Local("")
	rctx := core.WithSubject(ctx, sub)
	who := orchcore.OwedWho{Sessions: []orchapi.Session{parent}}

	part, err := h.s.owed(rctx, sub, sub.Scope, who)
	if err != nil {
		t.Fatal(err)
	}
	rows := part.Items
	if len(rows) != 1 || rows[0].Kind != orchapi.OwedLane || rows[0].Ref != child.Address || rows[0].Holder != parent.Address {
		t.Fatalf("rows for the parent: %+v", rows)
	}
	// Nobody else's: the child itself started nothing.
	if part, err = h.s.owed(rctx, sub, sub.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{child}}); err != nil || len(part.Items) != 0 {
		t.Fatalf("rows for the child: %+v %v", part.Items, err)
	}
	// An ended child is no longer owed.
	if _, err := orchclient.Stop(ctx, h.c, child.ID, orchapi.StopRequest{}); err != nil {
		t.Fatalf("stop the child: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if part, _ = h.s.owed(rctx, sub, sub.Scope, who); len(part.Items) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("an ended child is still owed: %+v", part.Items)
}

// SessionsOf lists the sessions a person's launcher started, newest first,
// and stops at the cutoff.
func TestSessionsOfPerson(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	sub := h.d.Projects.Local("")
	rctx := core.WithSubject(context.Background(), sub)
	got, truncated, err := h.s.SessionsOf(rctx, sub.Scope, h.d.Config.Human, time.Now().Add(-time.Hour))
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	found := false
	for _, ss := range got {
		found = found || ss.Address == parent.Address
	}
	if !found {
		t.Fatalf("the person's own session is not listed: %+v", got)
	}
	if got, _, err = h.s.SessionsOf(rctx, sub.Scope, "someone-else", time.Now().Add(-time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("another person's: %+v %v", got, err)
	}
	// A live session counts however long it has been idle; an ended one only
	// since the cutoff.
	future := time.Now().Add(time.Hour)
	if got, _, err = h.s.SessionsOf(rctx, sub.Scope, h.d.Config.Human, future); err != nil || len(got) == 0 {
		t.Fatalf("a live session older than the cutoff is dropped: %+v %v", got, err)
	}
	if _, err := orchclient.Stop(context.Background(), h.c, parent.ID, orchapi.StopRequest{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _, err = h.s.SessionsOf(rctx, sub.Scope, h.d.Config.Human, future); err == nil && len(got) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("an ended session older than the cutoff is listed: %+v %v", got, err)
}

// A read capped short of the sessions says so, never a quiet cut.
func TestOwedSessionCapIsUnknown(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	ctx := context.Background()
	if _, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home}); err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	sub := h.d.Projects.Local("")
	rctx := core.WithSubject(ctx, sub)
	old := maxOwedSessions
	maxOwedSessions = 0
	defer func() { maxOwedSessions = old }()
	part, err := h.s.owed(rctx, sub, sub.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{parent}})
	if err != nil || len(part.Items) != 0 || len(part.Gaps) != 1 || part.Gaps[0].Kind != orchapi.OwedGapPartial {
		t.Fatalf("capped lanes: %+v %v", part, err)
	}
	if _, truncated, err := h.s.SessionsOf(rctx, sub.Scope, h.d.Config.Human, time.Now().Add(-time.Hour)); err != nil || !truncated {
		t.Fatalf("capped person's sessions: truncated %v %v", truncated, err)
	}
}

// On a team authority the lanes a replica's session started count too: their
// parent is on the presence row.
func TestOwedLanesOnAnotherMachine(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	if _, err := f.h.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "far-box", Kind: api.IdentityMachine}); err != nil {
		t.Fatal(err)
	}
	beat := []orchapi.PresenceSession{
		{Address: "far/aaaa0000", Agent: "codex", State: orchapi.StateRunning, Started: time.Now()},
		{Address: "far/4bb7545a", Agent: "codex", Title: "far lane", Parent: "far/aaaa0000", State: orchapi.StateRunning, Started: time.Now()},
		{Address: "far/5cc8656b", Agent: "codex", Parent: "far/aaaa0000", State: orchapi.StateEnded, Started: time.Now()},
	}
	sub := f.h.d.Projects.Local("")
	rctx := core.WithSubject(ctx, sub)
	if err := f.h.s.RecordPresence(rctx, f.side.ID, "far", "far-box", beat); err != nil {
		t.Fatal(err)
	}
	part, err := f.h.s.owed(rctx, sub, sub.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{{Address: "far/aaaa0000"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Items) != 1 || part.Items[0].Kind != orchapi.OwedLane || part.Items[0].Ref != "far/4bb7545a" || part.Items[0].Holder != "far/aaaa0000" {
		t.Fatalf("a replica's live child (and not its ended one): %+v", part.Items)
	}
}

// A failed read of who started a session is said, never a quiet drop: owed
// filters on the parent, so a missing parent would hide a child.
func TestOwedFailedParentReadIsUnknown(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	if _, err := f.h.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "far-box", Kind: api.IdentityMachine}); err != nil {
		t.Fatal(err)
	}
	beat := []orchapi.PresenceSession{
		{Address: "far/aaaa0000", Agent: "codex", State: orchapi.StateRunning, Started: time.Now()},
		{Address: "far/4bb7545a", Agent: "codex", Parent: "far/aaaa0000", State: orchapi.StateRunning, Started: time.Now()},
	}
	sub := f.h.d.Projects.Local("")
	rctx := core.WithSubject(ctx, sub)
	if err := f.h.s.RecordPresence(rctx, f.side.ID, "far", "far-box", beat); err != nil {
		t.Fatal(err)
	}
	who := orchcore.OwedWho{Sessions: []orchapi.Session{{Address: "far/aaaa0000"}}}
	if part, err := f.h.s.owed(rctx, sub, sub.Scope, who); err != nil || len(part.Gaps) != 0 || len(part.Items) != 1 {
		t.Fatalf("a readable parent: %+v %v", part, err)
	}
	// Who started a session cannot be read now.
	testHookParentsRead = func() error { return errors.New("registry busy") }
	defer func() { testHookParentsRead = nil }()
	part, err := f.h.s.owed(rctx, sub, sub.Scope, who)
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Gaps) != 1 || part.Gaps[0].Kind != orchapi.OwedGapPartial || part.Gaps[0].Name != "sessions" {
		t.Fatalf("a failed parent read must be a gap: %+v", part)
	}
}
