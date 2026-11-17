package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// TestStopClearsActivity: after the prompt hook ("Thinking") and the stop
// hook (idle), the session has no activity, and the event of the change
// carries that; an idle reported without a hook (the idle timer, mail) does
// not touch it.
func TestStopClearsActivity(t *testing.T) {
	old := updatedEvery
	updatedEvery = 10 * time.Millisecond // each change its own event here
	t.Cleanup(func() { updatedEvery = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	hook := func(state orchapi.SessionState, activity string) orchapi.Session {
		t.Helper()
		r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-stop", State: state},
			Activity: activity})
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}
	if ss := hook(orchapi.StateRunning, "Thinking"); ss.Activity != "Thinking" {
		t.Fatalf("prompt: %+v", ss)
	}
	evs(100 * time.Millisecond)
	ss := hook(orchapi.StateIdle, "") // Stop
	if ss.State != orchapi.StateIdle || ss.Activity != "" || ss.ActivityAt != nil {
		t.Fatalf("after stop: state %s activity %q at %v", ss.State, ss.Activity, ss.ActivityAt)
	}
	got, err := orchclient.Session(ctx, h.c, ss.ID)
	if err != nil || got.Activity != "" {
		t.Fatalf("read after stop: %q %v", got.Activity, err)
	}
	var last orchapi.Session
	n := 0
	for _, e := range evs(200 * time.Millisecond) {
		if json.Unmarshal(e.Data, &last) == nil && last.Address == ss.Address {
			n++
		}
	}
	if n == 0 || last.Activity != "" || last.State != orchapi.StateIdle {
		t.Fatalf("no event carries the cleared activity: %d events, last %+v", n, last)
	}
	// Already idle with an activity (a hook reported one while idle): a stop
	// clears it with an updated event.
	hook(orchapi.StateIdle, "Reading a.go")
	evs(100 * time.Millisecond)
	if ss = hook(orchapi.StateIdle, ""); ss.Activity != "" {
		t.Fatalf("idle again: %q", ss.Activity)
	}
	if ts := types(evs(200 * time.Millisecond)); len(ts) != 1 || ts[0] != "updated" {
		t.Fatalf("events %v", ts)
	}
	// An idle that no hook reported leaves the activity alone.
	hook(orchapi.StateRunning, "Editing b.go")
	if err := h.s.SetState(ctx, ss.Address, orchapi.StateRequest{State: orchapi.StateIdle, Note: "answered"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.Activity != "Editing b.go" {
		t.Fatalf("a non-hook idle cleared the activity: %q", got.Activity)
	}
}
