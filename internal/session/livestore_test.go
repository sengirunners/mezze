package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
)

// TestActivityNotStored: a busy agent's activity stream reaches live
// subscribers but stores nothing in the replayable log; a title change
// (and a state change) is still stored, also when it rides the same
// coalesced event as an activity.
func TestActivityNotStored(t *testing.T) {
	old := updatedEvery
	updatedEvery = 20 * time.Millisecond
	t.Cleanup(func() { updatedEvery = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	hook := func(state orchapi.SessionState, activity string) orchapi.Session {
		t.Helper()
		r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-live", State: state},
			Activity: activity})
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}
	ss := hook(orchapi.StateRunning, "Thinking")
	stored := func(types ...string) []api.Event {
		t.Helper()
		got, err := h.d.Events.Since(ctx, h.owner(), "", events.Filter{Subject: ss.Address, Types: types}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := len(stored(orchapi.EventSessionUpdated))
	evs(100 * time.Millisecond)
	steps := []string{"Reading a.go", "Editing a.go", "Searching", "Running the tests", "Editing b.go"}
	for _, a := range steps {
		hook(orchapi.StateRunning, a)
		time.Sleep(40 * time.Millisecond) // past the coalescing gap: each its own event
	}
	var live []string
	for _, e := range evs(200 * time.Millisecond) {
		var d orchapi.Session
		if e.Type == orchapi.EventSessionUpdated && json.Unmarshal(e.Data, &d) == nil && d.Address == ss.Address {
			live = append(live, d.Activity)
		}
	}
	if len(live) < len(steps)-1 || live[len(live)-1] != "Editing b.go" {
		t.Fatalf("live subscribers saw %q", live)
	}
	if n := len(stored(orchapi.EventSessionUpdated)) - before; n != 0 {
		t.Fatalf("%d activity-only events stored", n)
	}
	// A title change is stored, also when it rides with an activity: a
	// title and an activity inside one coalescing gap.
	updatedEvery = time.Hour
	if _, err := h.s.update(ctx, ss.ID, "", func(r *row) (bool, error) { r.TermTitle = "topic"; return true, nil }); err != nil {
		t.Fatal(err)
	}
	h.s.updates.settle(ss.Address, true)         // a fresh slot: the next publish goes out at once
	hook(orchapi.StateRunning, "Thinking again") // activity: live, at once (no last)
	updatedEvery = 50 * time.Millisecond
	r, _ := h.s.load(ctx, ss.ID)
	h.s.publish(orchapi.EventSessionUpdated, r.Session) // the title: inside the gap, trailing, durable
	hook(orchapi.StateRunning, "Still thinking")        // another activity inside the same gap
	time.Sleep(200 * time.Millisecond)
	got := stored(orchapi.EventSessionUpdated)
	if n := len(got) - before; n != 1 {
		t.Fatalf("%d updated stored, want the one carrying the title", n)
	}
	var d orchapi.Session
	_ = json.Unmarshal(got[len(got)-1].Data, &d)
	if d.TermTitle != "topic" || d.Activity != "Still thinking" {
		t.Fatalf("stored %+v", d)
	}
	// State changes stay stored.
	hook(orchapi.StateIdle, "")
	if len(stored(orchapi.EventSessionStateChanged)) == 0 {
		t.Fatal("no stored state_changed")
	}
}
