package team_test

import (
	"context"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestRescopeAndPurge(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	billing := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "billing item", Area: "billing"}))(t)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	sv := j.Remote.ScopeVersion
	if _, err := orchclient.GetWork(ctx, r.owner, billing.ID); err != nil {
		t.Fatalf("member reads billing: %v", err)
	}
	// Narrowing: bea becomes a guest of solver only. The replica's next
	// call meets 409 rescope, refreshes its header and goes on.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	// Round C: the replica's copy learns of it from its sync (a waiting
	// GET /v0/sync answers 409 rescope), then takes a snapshot again.
	eventually(t, 3*time.Second, func() error {
		if _, err := orchclient.GetWork(ctx, r.owner, billing.ID); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("after narrowing, billing: %v", err)
		}
		return nil
	})
	rs := mustf(r.owner.Remotes(ctx))(t)
	if len(rs) != 1 || rs[0].ScopeVersion <= sv || len(rs[0].Areas) != 1 || rs[0].Areas[0] != "solver" || rs[0].Role != "guest" {
		t.Fatalf("header after rescope: %+v", rs)
	}
	// A grant revoked outright (token left alone): the replica stops,
	// says so, and lists name the project in skipped (SPEC 17.4 "or stops,
	// if revoked"); zm leave still revokes the token.
	v, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	vs := register(t, v, "harbor", "vida-sess-1")
	heartbeat(t, v)
	if s := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), vs.Address); s == nil {
		t.Fatal("guest session not present at the authority")
	}
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "vida", Project: "harbor", Area: "solver"}))(t)
	// Its sessions leave the authority's lists with the grant (the token
	// is unusable without one), not an hour later as "presence lost".
	if s := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), vs.Address); s != nil {
		t.Fatalf("revoked guest's session still listed: %+v", s)
	}
	if s := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{All: true}))(t), vs.Address); s == nil || s.State != orchapi.StateEnded || s.Note != "replica left" {
		t.Fatalf("revoked guest's session: %+v", s)
	}
	// Round C: the replica learns from its sync (a waiting call answers
	// 409 rescope, the new header has no role) and drops its copy.
	eventually(t, 3*time.Second, func() error {
		if _, err := orchclient.WorkList(ctx, at(v.owner, "harbor", ""), orchapi.WorkQuery{}); !api.IsCode(err, api.CodeUnauthenticated) || !strings.Contains(err.Error(), "revoked") {
			return fmt.Errorf("after revoke: %v", err)
		}
		return nil
	})
	vr := mustf(v.owner.Remotes(ctx))(t)
	if len(vr) != 1 || vr[0].State != api.RemoteRevoked || !strings.Contains(vr[0].LastError, "mini") {
		t.Fatalf("remote after revoke: %+v", vr)
	}
	if _, err := orchclient.WorkList(ctx, at(v.owner, "harbor", ""), orchapi.WorkQuery{}); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("revoked, second call: %v", err)
	}
	if wl := mustf(orchclient.WorkList(ctx, v.owner, orchapi.WorkQuery{}))(t); len(wl.Skipped) != 1 || !strings.Contains(wl.Skipped[0].Error, "revoked") {
		t.Fatalf("union after revoke: %+v", wl)
	}
	if pl := mustf(v.owner.ProjectList(ctx))(t); len(pl.Projects) != 0 || len(pl.Skipped) != 1 {
		t.Fatalf("projects after revoke: %+v", pl)
	}
	if _, err := v.owner.GetProject(ctx, "harbor"); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("project after revoke: %v", err)
	}
	if lv := mustf(v.owner.Leave(ctx, "harbor"))(t); !lv.Revoked {
		t.Fatalf("leave after revoke: %+v", lv)
	}

	// Revoked token: 401s; three of them PurgeAfter apart purge the copy.
	old := teamlink.PurgeAfter
	teamlink.PurgeAfter = 50 * time.Millisecond
	t.Cleanup(func() { teamlink.PurgeAfter = old })
	for _, tk := range mustf(a.owner.Tokens(ctx))(t) {
		if tk.Node != "" {
			mustf(a.owner.RevokeToken(ctx, tk.ID))(t)
		}
	}
	// Round C: reads come from the copy, so the replica's own sync meets
	// the 401s (a waiting sync call ends when its token is revoked), and the
	// third, PurgeAfter after the first, purges the token and the copy.
	eventually(t, 5*time.Second, func() error {
		rs = mustf(r.owner.Remotes(ctx))(t)
		if len(rs) != 1 || rs[0].State != api.RemotePurged || !strings.Contains(rs[0].LastError, "3 times") {
			return fmt.Errorf("after 401s: %+v", rs)
		}
		return nil
	})
	if _, err := os.Stat(rs[0].TokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file kept after purge: %v", err)
	}
	eventually(t, 2*time.Second, func() error {
		if _, err := os.Stat(filepath.Join(r.cfg.Home, team.DirReplicas, rs[0].Project.ID+".db")); !os.IsNotExist(err) {
			return fmt.Errorf("replica copy kept after purge: %v", err)
		}
		return nil
	})
	if _, err := orchclient.WorkList(ctx, at(r.owner, "harbor", ""), orchapi.WorkQuery{}); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("after purge: %v", err)
	}
	// A list naming nothing still answers, naming the purged project.
	wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
	if len(wl.Skipped) != 1 || wl.Skipped[0].Source != "authority" {
		t.Fatalf("union after purge: %+v", wl)
	}
	mustf(r.owner.Leave(ctx, "harbor"))(t)
}

// A single 401 (a restarting authority) purges nothing.
func TestOne401PurgesNothing(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	var tid string
	for _, tk := range mustf(a.owner.Tokens(ctx))(t) {
		if tk.Node != "" {
			tid = tk.ID
		}
	}
	mustf(a.owner.RevokeToken(ctx, tid))(t)
	for i := 0; i < 3; i++ { // three quick 401s: under PurgeAfter (10 min)
		_, _ = orchclient.WorkList(ctx, at(r.owner, "harbor", ""), orchapi.WorkQuery{})
	}
	if rs := mustf(r.owner.Remotes(ctx))(t); rs[0].State != api.RemoteOK {
		t.Fatalf("purged on quick 401s: %+v", rs[0])
	}
}

// A header is computed when its request is authenticated, so an answer can
// carry an older scope version than the replica holds: a long poll begun
// before a scope change, or a later call that reached the authority first.
// That must not take the replica back, or a call between its rescope and its
// one retry is refused twice, and the caller sees a transient error right
// after any scope change.
func TestOldAnswerDoesNotRollBackScope(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao := at(a.owner, "harbor", "")
	mustf(orchclient.SetupIssues(ctx, ao, "harbor"))(t)
	mustf(orchclient.SetupIssues(ctx, at(a.owner, "zm", ""), "zm"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	it := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "moves"}))(t)
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	if res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	nid := res.Items[0].NewID
	var svc *team.Service
	for _, m := range r.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			svc = s
		}
	}
	rc := at(r.owner, "harbor", "")
	read := func() {
		t.Helper()
		if d := mustf(orchclient.Issue(ctx, rc, it.ID))(t); d.ID != nid {
			t.Fatalf("old id answers %s, want %s", d.ID, nid)
		}
	}
	read()
	raw0, sv0 := svc.HeldHeader("harbor")

	// A scope change just before an old-id read: the read succeeds, at once.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor", Area: "solver"}))(t)
	read()
	raw1, sv1 := svc.HeldHeader("harbor")
	if sv1 <= sv0 {
		t.Fatalf("scope version %d after a grant, was %d", sv1, sv0)
	}
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: "solver"}))(t)
	read()
	raw2, sv2 := svc.HeldHeader("harbor")
	if sv2 <= sv1 {
		t.Fatalf("scope version %d after a revoke, was %d", sv2, sv1)
	}

	// Answers older than what is held change nothing: an old long poll (sent
	// before the rest), the inversion (sent last, but it reached the authority
	// before the scope change), an old correction, and an old revoke, which
	// would stop this replica and delete its copy though bea still has access.
	old := mustf(api.DecodeTeamHeader(raw1))(t)
	old.Role = ""
	revoked := api.EncodeTeamHeader(old)
	for _, c := range []struct {
		name   string
		raw    string
		status int
		sent   int64
		later  bool
	}{
		{"long poll 409", raw0, 409, sv0, false},
		{"long poll 200", raw0, 200, sv0, false},
		{"inversion", raw0, 200, sv0, true},
		{"old correction", raw1, 409, sv0, true},
		{"old revoke", revoked, 200, sv1, true},
	} {
		svc.DeliverAnswer(ctx, "harbor", c.raw, c.status, c.sent, c.later)
		if raw, sv := svc.HeldHeader("harbor"); sv != sv2 || raw != raw2 {
			t.Fatalf("%s took the replica to version %d (holds %d)", c.name, sv, sv2)
		}
	}
	if rs := mustf(r.owner.Remotes(ctx))(t); rs[0].State != api.RemoteOK || rs[1].State != api.RemoteOK {
		t.Fatalf("an old answer stopped the replica: %+v", rs)
	}
	// Old answers arriving all the time never fail a read.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				svc.DeliverAnswer(ctx, "harbor", raw0, 409, sv0, true)
			}
		}
	}()
	for range 200 {
		read()
	}
	close(stop)
	<-done

	// A newer version is taken though its call was sent before the rest: a
	// 409's correction is not dropped by the send order. (Its version is the
	// authority's own, so later calls keep it.)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor", Area: "billing"}))(t)
	newer := mustf(api.DecodeTeamHeader(raw2))(t)
	newer.ScopeVersion = sv2 + 1
	svc.DeliverAnswer(ctx, "harbor", api.EncodeTeamHeader(newer), 409, sv2, false)
	if _, sv := svc.HeldHeader("harbor"); sv != sv2+1 {
		t.Fatalf("a newer version dropped: holds %d, want %d", sv, sv2+1)
	}
	read()
	// An authority restored from a backup answers lower than the call sent:
	// the replica takes that, and its calls go on at the authority's version.
	svc.DeliverAnswer(ctx, "harbor", raw0, 409, sv2+1, true)
	read()
	if _, sv := svc.HeldHeader("harbor"); sv != sv2+1 {
		t.Fatalf("after the authority's correction: holds %d, want %d", sv, sv2+1)
	}
}
