package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// TestSessionFollowsCwd: a session in project none moves to the project its
// agent's hook cwd resolves to (cwd rules only), with an updated event; a
// session with a project never moves this way.
func TestSessionFollowsCwd(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	acme := filepath.Join(h.home, "acme")
	side := filepath.Join(h.home, "side")
	for _, d := range []string{filepath.Join(acme, "sub"), side, filepath.Join(h.home, "elsewhere")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pa := h.project("acme", acme)
	h.project("side", side)
	h.project("fallback")
	evs := sessionEvents(t, h)

	// A managed shell started outside every project.
	ss := h.spawn("claude", "exec sleep 30", func(r *orchapi.SpawnRequest) { r.Cwd = filepath.Join(h.home, "elsewhere") })
	if ss.Project != api.ProjectNone {
		t.Fatalf("spawned in %q", ss.Project)
	}
	h.d.Config.File.DefaultProject = "fallback" // set before any hook runs
	hook := func(cwd, rid string, managed bool) orchapi.Session {
		t.Helper()
		req := orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Cwd: cwd, State: orchapi.StateRunning}}
		c := h.c
		if managed {
			req.Session = ss.Address
			cc := *h.c
			cc.As = ss.Address
			c = &cc
		}
		r, err := orchclient.Hook(ctx, c, req)
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}
	evs(100 * time.Millisecond)
	// A cwd only default_project would claim: stays in none.
	if got := hook(filepath.Join(h.home, "elsewhere"), "rt-follow", true); got.Project != api.ProjectNone {
		t.Fatalf("followed default_project: %+v", got)
	}
	// The agent works in acme: the session moves there.
	got := hook(filepath.Join(acme, "sub"), "rt-follow", true)
	if got.Project != pa.ID || got.Address != ss.Address {
		t.Fatalf("did not follow: %+v", got)
	}
	if ts := strings.Join(types(evs(150*time.Millisecond)), ","); !strings.Contains(ts, "updated") {
		t.Fatalf("events %s", ts)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{Project: "acme"}); len(l) != 1 || l[0].Address != ss.Address {
		t.Fatalf("acme sessions %+v", l)
	}
	// Then it cds into side: a session with a project stays.
	if got := hook(side, "rt-follow", true); got.Project != pa.ID {
		t.Fatalf("moved a session with a project: %+v", got)
	}

	// An unmanaged session registered in none follows too.
	h.d.Config.File.DefaultProject = "" // (a new registration does use it)
	u := hook(filepath.Join(h.home, "elsewhere"), "rt-plain", false)
	if u.Project != api.ProjectNone {
		t.Fatalf("plain session %+v", u)
	}
	if u = hook(side, "rt-plain", false); u.Project == api.ProjectNone || u.Managed {
		t.Fatalf("plain session did not follow: %+v", u)
	}
}

// TestMoveByHand: POST /v0/sessions/{id}/project moves a session to a
// project (or area, or none) the caller may write in; a hidden or missing
// target is not_found; a session holding a lease is refused (409).
func TestMoveByHand(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	pa := h.project("acme")
	if _, err := h.d.Projects.AddArea(ctx, h.owner(), "acme", api.Area{Name: "solver"}); err != nil {
		t.Fatal(err)
	}
	h.project("side")
	evs := sessionEvents(t, h)
	ss := h.spawn("claude", "exec sleep 30", nil)
	evs(100 * time.Millisecond)
	got, err := orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{Project: "acme/solver"})
	if err != nil || got.Project != pa.ID || got.Area != "solver" || got.Who == nil {
		t.Fatalf("move: %+v %v", got, err)
	}
	// The old log hears it left (and nothing about where to); the new one the session.
	got1 := evs(150 * time.Millisecond)
	if ts := types(got1); strings.Join(ts, ",") != "left,updated" {
		t.Fatalf("events %v", ts)
	}
	if e := got1[0]; e.Project != api.ProjectNone || strings.Contains(string(e.Data), pa.ID) ||
		string(e.Data) != `{"address":"`+ss.Address+`","project":"none"}` {
		t.Fatalf("left event %s %s", e.Project, e.Data)
	}
	if e := got1[1]; e.Project != pa.ID || e.Area != "solver" {
		t.Fatalf("updated event in %s/%s", e.Project, e.Area)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{Project: "acme"}); len(l) != 1 || l[0].Area != "solver" {
		t.Fatalf("acme sessions %+v", l)
	}
	// The same place again: no change, no event.
	if _, err := orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{Project: "acme", Area: "solver"}); err != nil {
		t.Fatal(err)
	}
	if ts := types(evs(100 * time.Millisecond)); len(ts) != 0 {
		t.Fatalf("no-op move events %v", ts)
	}
	for _, bad := range []orchapi.MoveRequest{{Project: "nope"}, {Project: "acme", Area: "nope"}, {Project: "none", Area: "x"}} {
		if _, err := orchclient.MoveSession(ctx, h.c, ss.ID, bad); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("move to %+v: %v", bad, err)
		}
	}
	if _, err := orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("no project: %v", err)
	}
	if got, err = orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{Project: "none"}); err != nil || got.Project != api.ProjectNone || got.Area != "" {
		t.Fatalf("to none: %+v %v", got, err)
	}
	if e := evs(150 * time.Millisecond); len(e) != 2 || e[0].Type != orchapi.EventSessionLeft || e[0].Project != pa.ID || e[0].Area != "solver" ||
		string(e[0].Data) != `{"address":"`+ss.Address+`","project":"`+pa.ID+`","area":"solver"}` {
		t.Fatalf("left acme/solver: %+v", e)
	}
	// A session holding a (bound) lease stays put.
	repo := t.TempDir()
	h.project("heldp", repo)
	held := h.spawn("claude", "exec sleep 30", func(r *orchapi.SpawnRequest) { r.Cwd, r.Work = repo, "local:abc123" })
	if _, err := orchclient.MoveSession(ctx, h.c, held.ID, orchapi.MoveRequest{Project: "side"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("move with a lease: %v", err)
	}
	// Ended sessions do not move.
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil {
		t.Fatal(err)
	}
	h.wait(ss.ID, "ended", ended)
	if _, err := orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{Project: "side"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("move ended: %v", err)
	}
}
