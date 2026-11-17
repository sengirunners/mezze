package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// msOnly reports times cut to the table's precision (whole milliseconds).
func msOnly(ts ...*time.Time) bool {
	for _, t := range ts {
		if t != nil && t.Nanosecond()%int(time.Millisecond) != 0 {
			return false
		}
	}
	return true
}

// TestSessionTimesOnePrecision (harbor-q5kzn): a session's events and its
// reads carry the same millisecond times, so a client that upserts by
// updated keeps the later event: started then updated compare in order, and
// a read equals the last event.
func TestSessionTimesOnePrecision(t *testing.T) {
	old, oldU := titleEvery, updatedEvery
	titleEvery, updatedEvery = 20*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { titleEvery, updatedEvery = old, oldU })
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	ss := h.spawn("claude", `sleep 0.2; printf '\033]0;topic\007'; exec sleep 30`, nil)
	h.wait(ss.ID, "titled", func(s orchapi.Session) bool { return s.TermTitle == "topic" })
	var seq []orchapi.Session
	var types []string
	for _, e := range evs(300 * time.Millisecond) {
		var d orchapi.Session
		if json.Unmarshal(e.Data, &d) != nil || d.Address != ss.Address {
			continue
		}
		seq, types = append(seq, d), append(types, e.Type)
	}
	if len(seq) < 2 || types[0] != orchapi.EventSessionStarted || types[len(types)-1] != orchapi.EventSessionUpdated {
		t.Fatalf("events %v", types)
	}
	for i, d := range seq {
		if !msOnly(&d.Started, &d.Updated, d.Ended, d.ActivityAt, d.Seen) {
			t.Fatalf("%s carries sub-millisecond times: started %s updated %s", types[i], d.Started.Format(time.RFC3339Nano), d.Updated.Format(time.RFC3339Nano))
		}
		if i > 0 && d.Updated.Before(seq[i-1].Updated) {
			t.Fatalf("%s updated %s is before %s's %s", types[i], d.Updated.Format(time.RFC3339Nano), types[i-1], seq[i-1].Updated.Format(time.RFC3339Nano))
		}
		if !d.Started.Equal(seq[0].Started) {
			t.Fatalf("started moved: %s vs %s", d.Started, seq[0].Started)
		}
	}
	if !msOnly(&ss.Started, &ss.Updated) {
		t.Fatalf("the spawn answer carries sub-millisecond times: %s", ss.Updated.Format(time.RFC3339Nano))
	}
	got, err := orchclient.Session(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := seq[len(seq)-1]; !got.Updated.Equal(last.Updated) || !got.Started.Equal(last.Started) {
		t.Fatalf("read %s / %s, last event %s / %s", got.Started.Format(time.RFC3339Nano), got.Updated.Format(time.RFC3339Nano),
			last.Started.Format(time.RFC3339Nano), last.Updated.Format(time.RFC3339Nano))
	}
	// A hook-registered session and its state change.
	r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-ms", State: orchapi.StateRunning}, Activity: "Thinking"})
	if err != nil || !msOnly(&r.Session.Started, &r.Session.Updated, r.Session.ActivityAt) {
		t.Fatalf("hook answer %+v %v", r.Session, err)
	}
	// Presence rows.
	beat := []orchapi.PresenceSession{{Address: "bea-mac/4bb7545a", Agent: "claude", State: orchapi.StateRunning,
		Started: time.Now(), Activity: "Searching", ActivityAt: ptr(time.Now())}}
	if err := h.s.RecordPresence(ctx, api.ProjectNone, "bea-mac", "bea", beat); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs(150 * time.Millisecond) {
		var d orchapi.Session
		if json.Unmarshal(e.Data, &d) == nil && d.Address == "bea-mac/4bb7545a" && !msOnly(&d.Started, &d.Updated, d.Seen, d.ActivityAt) {
			t.Fatalf("presence event carries sub-millisecond times: %s", e.Data)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
