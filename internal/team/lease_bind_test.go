package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func teamOf(t *testing.T, d *daemon) *team.Service {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			return s
		}
	}
	t.Fatal("no team module")
	return nil
}

// A session's lease taken at spawn on a joined project's item:
// held by the session at the authority, bound once its heartbeat shows it;
// a conflict and an area the replica may not write come back as the
// authority's answer; an authority away is refused at once and nothing is
// queued (a late claim would assign a session that was rolled back).
func TestBindAtJoinedProject(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	for _, ar := range []string{"solver", "billing"} {
		mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: ar}))(t)
	}
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "solve", Area: "solver"}))(t)
	r := startDaemonWith(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"}, discard, roomy)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor/solver", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	waitReachable(t, r, true)
	ts := teamOf(t, r)
	owner := r.dm.Deps.Projects.Local("")
	dir := t.TempDir()
	spawn := func() orchapi.Session { // a managed session (bound leases are a managed session's)
		return mustf(orchclient.Spawn(ctx, at(r.owner, "harbor/solver", ""), orchapi.SpawnRequest{Agent: "claude", Command: []string{"/bin/sh", "-c", "sleep 120"},
			Cwd: dir, Env: map[string]string{"PATH": os.Getenv("PATH")}}))(t)
	}
	s1, s2 := spawn(), spawn()

	l, err := ts.BindAt(ctx, owner, "harbor", it.ID, s1.Address)
	if err != nil || l.Holder != s1.Address {
		t.Fatalf("bind: %+v %v", l, err)
	}
	heartbeat(t, r)
	if h := mustf(orchclient.Holder(ctx, ao, it.ID))(t); h.Holder != s1.Address {
		t.Fatalf("authority holder: %+v", h)
	}
	// A default-TTL session lease, the kind the authority's ticker binds
	// while its session runs (SPEC 10): not a fixed-TTL one it never binds.
	ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Holder: s1.Address}))(t)
	if len(ls) != 1 || ls[0].Expires == nil || ls[0].Expires.Sub(ls[0].Claimed) != time.Hour {
		t.Fatalf("the session's lease at the authority: %+v", ls)
	}
	if _, err := ts.BindAt(ctx, owner, "harbor", it.ID, s2.Address); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a held item: %v", err)
	}
	other := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "bill", Area: "billing"}))(t)
	if _, err := ts.BindAt(ctx, owner, "harbor", other.ID, s2.Address); err == nil || api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("an area the replica may not write: %v", err)
	}
	if _, err := ts.BindAt(ctx, owner, "harbor", it.ID, "mini/aaaa1111"); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("another machine's session: %v", err)
	}

	// A start that failed after the bind gives it back, live.
	if err := ts.ReleaseAt(ctx, "harbor", it.ID, s1.Address); err != nil {
		t.Fatal(err)
	}
	if h := mustf(orchclient.Holder(ctx, ao, it.ID))(t); h.Holder != "" {
		t.Fatalf("after ReleaseAt: %+v", h)
	}
	held := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "held", Area: "solver"}))(t)
	if _, err := ts.BindAt(ctx, owner, "harbor", held.ID, s2.Address); err != nil {
		t.Fatal(err)
	}

	free := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "later", Area: "solver"}))(t)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	if _, err := ts.BindAt(ctx, owner, "harbor", free.ID, s2.Address); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("authority away: %v", err)
	}
	if q := mustf(at(r.owner, "harbor", "").Outbox(ctx, "harbor", true))(t); len(q) != 0 {
		t.Fatalf("queued: %+v", q)
	}
	// A release while it is away queues (it only drops that session's own).
	if err := ts.ReleaseAt(ctx, "harbor", held.ID, s2.Address); err != nil {
		t.Fatalf("release while away: %v", err)
	}
	if q := mustf(at(r.owner, "harbor", "").Outbox(ctx, "harbor", true))(t); len(q) != 1 || q[0].Kind != "release" {
		t.Fatalf("queued release: %+v", q)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitReachable(t, r, true)
	eventually(t, 10*time.Second, func() error {
		if h := mustf(orchclient.Holder(ctx, ao, held.ID))(t); h.Holder != "" {
			return fmt.Errorf("queued release not run: %+v", h)
		}
		return nil
	})
	if h := mustf(orchclient.Holder(ctx, ao, free.ID))(t); h.Holder != "" {
		t.Fatalf("a refused bind landed later: %+v", h)
	}
}
