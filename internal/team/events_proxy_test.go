package team_test

import (
	"context"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func replay(t *testing.T, c *client.Client, q api.EventQuery) []api.Event {
	t.Helper()
	q.NoLive, q.Since = true, "0"
	var out []api.Event
	if err := c.Events(context.Background(), q, func(e api.Event) error { out = append(out, e); return nil }); err != nil {
		t.Fatalf("events %+v: %v", q, err)
	}
	return out
}

func subjects(es []api.Event) string {
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Subject + " ")
	}
	return b.String()
}

// A member reads a joined project's events: named or merged in a replay, live
// when named, and only what its token may see.
func TestReplicaEvents(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "secret"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "secret"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ao := at(a.owner, "harbor", "")
	seen := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "seen"}))(t)
	hid := mustf(orchclient.CreateWork(ctx, at(a.owner, "secret", ""), orchapi.CreateWorkRequest{Title: "hidden"}))(t)

	named := subjects(replay(t, r.owner, api.EventQuery{Project: "harbor"}))
	if !strings.Contains(named, seen.ID) || strings.Contains(named, hid.ID) {
		t.Fatalf("named replay: %s", named)
	}
	merged := subjects(replay(t, r.owner, api.EventQuery{}))
	if !strings.Contains(merged, seen.ID) || strings.Contains(merged, hid.ID) {
		t.Fatalf("merged replay: %s", merged)
	}
	if strings.Count(merged, seen.ID) != strings.Count(named, seen.ID) {
		t.Fatalf("merged replay repeats events: %s against %s", merged, named)
	}
	// A project this member has not joined answers like a missing one.
	err := r.owner.Events(ctx, api.EventQuery{Project: "secret", NoLive: true, Since: "0"}, func(api.Event) error { return nil })
	if !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a project not joined: %v", err)
	}

	// Follow, with a filter.
	got := make(chan api.Event, 1)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.sock.Events(sctx, api.EventQuery{Project: "harbor", Types: orchapi.EventWorkCreated}, func(e api.Event) error {
			got <- e
			return events.ErrStop
		})
	}()
	var live orchapi.WorkItem
	deadline := time.After(9 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond) // until the stream is up: live-only misses earlier ones
	defer tick.Stop()
	for ev := (api.Event{}); ev.Subject == ""; {
		select {
		case ev = <-got:
			if ev.Subject != live.ID && !strings.HasPrefix(ev.Subject, "local:") {
				t.Fatalf("live event: %+v", ev)
			}
		case <-tick.C:
			live = mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "live"}))(t)
		case <-deadline:
			t.Fatal("no live event through the replica")
		}
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("follow ended: %v", err)
	}
}

// A guest of one area gets only that area's events through its replica.
func TestReplicaEventsGuest(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: "other"}))(t)
	g, _ := replica(t, tn, a, "guest-mac", "nGUEST", "harbor/solver", "guest", "guest")
	ao := at(a.owner, "harbor", "")
	in := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "in", Area: "solver"}))(t)
	out := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "out", Area: "other"}))(t)
	got := subjects(replay(t, g.owner, api.EventQuery{Project: "harbor"}))
	if !strings.Contains(got, in.ID) || strings.Contains(got, out.ID) {
		t.Fatalf("guest replay: %s", got)
	}
}

// The authority row shows how long the authority has run and its backup.
func TestReplicaSeesAuthorityStart(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	g, _ := replica(t, tn, a, "guest-mac", "nGUEST", "harbor/solver", "guest", "guest")
	mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "wake the sync"}))(t)
	row := func(d *daemon) orchapi.WorkSource {
		for _, s := range mustf(orchclient.WorkSources(ctx, at(d.owner, "harbor", ""), ""))(t) {
			if s.Name == "authority" {
				return s
			}
		}
		t.Fatal("no authority row")
		return orchapi.WorkSource{}
	}
	deadline := time.Now().Add(10 * time.Second)
	for row(r).Started.IsZero() {
		if time.Now().After(deadline) {
			t.Fatalf("no start time: %+v", row(r))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := row(r).Started; !got.Equal(a.dm.Deps.Started) {
		t.Fatalf("started %v, want %v", got, a.dm.Deps.Started)
	}
	if got := row(g); !got.Started.IsZero() {
		t.Fatalf("a guest of one area sees the authority's start: %+v", got)
	}
}

// A live stream ends, with the reason, when its access does.
func TestReplicaEventsEndWhenAccessEnds(t *testing.T) {
	old := events.Heartbeat
	events.Heartbeat = 200 * time.Millisecond
	t.Cleanup(func() { events.Heartbeat = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.sock.Events(sctx, api.EventQuery{Project: "harbor", Types: orchapi.EventWorkCreated}, func(api.Event) error { return nil })
	}()
	time.Sleep(500 * time.Millisecond)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	select {
	case err := <-done:
		if err == nil || sctx.Err() != nil {
			t.Fatalf("stream ended with %v, ctx %v", err, sctx.Err())
		}
		var se *events.StreamError
		if !errors.As(err, &se) {
			t.Logf("ended: %v", err) // revoked: the replica may end it first
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a live stream outlived the revoke")
	}
}

// A stream cut short by the authority going silent is an error, never a clean end.
func TestReplicaEventsCutShortIsAnError(t *testing.T) {
	old := teamlink.CallTimeout
	teamlink.CallTimeout = 500 * time.Millisecond
	t.Cleanup(func() { teamlink.CallTimeout = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ao := at(a.owner, "harbor", "")
	body := strings.Repeat("x", 4096)
	for i := 0; i < 300; i++ {
		mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "t", Body: body}))(t)
	}
	got := 0
	err := r.sock.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0", Types: orchapi.EventWorkCreated}, func(api.Event) error {
		got++
		if got == 1 {
			tn.Stall("potluck-bea-mac", "potluck-mini", 5*time.Second)
			tn.Stall("potluck-mini", "potluck-bea-mac", 5*time.Second)
		}
		return nil
	})
	if got < 300 && err == nil {
		t.Fatalf("%d of 300 events and no error", got)
	}
}

// An unrelated grant does not end a healthy stream.
func TestReplicaEventsOtherGrantKeepsStream(t *testing.T) {
	old := events.Heartbeat
	events.Heartbeat = 100 * time.Millisecond
	t.Cleanup(func() { events.Heartbeat = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "other"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.sock.Events(sctx, api.EventQuery{Project: "harbor", Types: orchapi.EventWorkCreated}, func(api.Event) error { return nil })
	}()
	time.Sleep(400 * time.Millisecond)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "other", Role: "guest"}))(t)
	select {
	case err := <-done:
		t.Fatalf("a grant elsewhere ended the stream: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
}

// A daemon stopping during a replay is an error unless every event arrived.
func TestReplicaEventsReplayStoppedIsAnError(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ao := at(a.owner, "harbor", "")
	body := strings.Repeat("x", 4096)
	const n = 300
	for i := 0; i < n; i++ {
		mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "t", Body: body}))(t)
	}
	got := 0
	err := r.sock.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0", Types: orchapi.EventWorkCreated}, func(api.Event) error {
		if got++; got == 1 {
			halt := r.halt
			r.halt = func() {} // the harness's cleanup must not wait on it again
			go halt()
			time.Sleep(500 * time.Millisecond)
		}
		return nil
	})
	if got < n && err == nil {
		t.Fatalf("%d of %d events and no error", got, n)
	}
}
