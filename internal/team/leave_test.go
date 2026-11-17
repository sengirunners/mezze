package team_test

import (
	"context"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A node with a legitimate invite cannot redeem under the authority's own
// machine name (zm join refuses it client-side; this is the direct POST),
// and a node-bound token that records it anyway (one from before the fix,
// or an authority renamed onto a replica's name) answers as none of the
// authority's sessions, its group mailbox or its human.
func TestRedeemAsAuthorityMachine(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mal := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nMAL", NodeKey: "km", Login: "mallory@example.com"})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "mallory", Role: "member"}))(t)
	if _, err := mal.redeem(inv.Code, "mini"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("redeem as mini: %v", err)
	}
	// The refusal spent nothing: the same code under its own name works.
	red := mustf(mal.redeem(inv.Code, "mal"))(t)

	// A session on the authority with mail, some of it for a human.
	ms := register(t, a, "harbor", "mini-sess-1")
	owner := at(a.owner, "harbor", "")
	m1 := mustf(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: ms.Address, Body: "for the authority's session"}))(t)
	m2 := mustf(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: ms.Address, Body: "decide", Needs: orchapi.NeedsHuman}))(t)

	// Simulate a token that recorded the authority's name.
	if _, err := a.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE tokens SET machine = 'mini' WHERE node = 'nMAL'`); err != nil {
		t.Fatal(err)
	}
	tc := client.NewRemote("http://potluck-mini:7433", tn.Node("zm-nmal").HTTPClient())
	tc.Token = red.Token
	for _, as := range []string{ms.Address, "machine:mini"} {
		x := *tc
		x.As = as
		if _, err := orchclient.Inbox(ctx, &x, orchapi.InboxQuery{}); !api.IsCode(err, api.CodeForbidden) {
			t.Fatalf("Zm-As %s: %v", as, err)
		}
		if _, err := orchclient.Ack(ctx, &x, m1.ID); err == nil {
			t.Fatalf("Zm-As %s acked the authority session's mail", as)
		}
	}
	x := *tc
	x.As = "human:mallory"
	in, err := orchclient.Inbox(ctx, &x, orchapi.InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if hasMail(in, m1.ID) || hasMail(in, m2.ID) {
		t.Fatalf("mallory reads the authority session's mail: %+v", in)
	}
}

// After zm leave, a reimaged box (a new node ID) rejoins under the same
// machine name with a new invite; while the first node's token is live the
// name stays taken.
func TestRejoinAfterLeaveNewNode(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "box1", "nBOX1", "harbor", "farm", "member")
	// Another node claiming box1 while the first is joined: taken.
	other := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nOTHER", NodeKey: "ko", Login: "farm@example.com"})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "farm", Role: "member"}))(t)
	if _, err := other.redeem(inv.Code, "box1"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("taken name: %v", err)
	}
	if lv := mustf(r.owner.Leave(ctx, "harbor"))(t); !lv.Revoked {
		t.Fatalf("leave %+v", lv)
	}
	r.stop() // the box is reimaged
	r2 := startDaemon(t, tn, "box1", tailnettest.Identity{NodeID: "nBOX1-new", NodeKey: "nodekey:box1-new", Login: "farm@example.com", Host: "box1"})
	inv2 := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "farm", Role: "member"}))(t)
	j := mustf(r2.owner.Join(ctx, api.JoinRequest{Code: inv2.Code}))(t)
	if j.Remote == nil || j.Remote.Machine != "box1" {
		t.Fatalf("rejoin %+v", j)
	}
	// It works: a proxied list answers.
	if _, err := orchclient.Work(ctx, r2.owner, orchapi.WorkQuery{Project: "harbor"}); err != nil {
		t.Fatal(err)
	}
}

// A replica's sessions end at the authority when it leaves, when its token
// is revoked, and when its heartbeat is long gone: zm ls at the authority
// does not collect ghosts from every box that ever joined.
func TestPresenceEndsWithReplica(t *testing.T) {
	quietPresence(t)
	old := session.PresenceStale
	session.PresenceStale = 50 * time.Millisecond
	t.Cleanup(func() { session.PresenceStale = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) { c.File.BoundLeaseTTLSeconds = 1 })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	live := func(addr string) *orchapi.Session {
		return findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), addr)
	}
	ended := func(addr, note string) {
		t.Helper()
		if s := live(addr); s != nil {
			t.Fatalf("%s still listed: %+v", addr, s)
		}
		s := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{All: true}))(t), addr)
		if s == nil || s.State != orchapi.StateEnded || s.Note != note {
			t.Fatalf("%s: %+v", addr, s)
		}
	}

	// zm leave.
	r1, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	s1 := register(t, r1, "harbor", "bea-sess-1")
	heartbeat(t, r1)
	if s := live(s1.Address); s == nil || s.State == orchapi.StateEnded {
		t.Fatalf("before leave: %+v", s)
	}
	mustf(r1.owner.Leave(ctx, "harbor"))(t)
	ended(s1.Address, "replica left")

	// zm token revoke at the authority.
	r2, _ := replica(t, tn, a, "box2", "nBOX2", "harbor", "farm", "member")
	s2 := register(t, r2, "harbor", "box2-sess-1")
	heartbeat(t, r2)
	if live(s2.Address) == nil {
		t.Fatal("box2 session not present")
	}
	for _, tk := range mustf(a.owner.Tokens(ctx))(t) {
		if tk.Node == "nBOX2" && tk.Revoked == nil {
			mustf(a.owner.RevokeToken(ctx, tk.ID))(t)
		}
	}
	ended(s2.Address, "replica left")

	// A replica that simply goes away: ended once its heartbeat is older
	// than bound_lease_ttl plus two stale periods (here 1.1 s).
	r3, _ := replica(t, tn, a, "box3", "nBOX3", "harbor", "farm3", "member")
	s3 := register(t, r3, "harbor", "box3-sess-1")
	heartbeat(t, r3)
	r3.stop()
	reap := func() { sessionModule(t, a).ReapPresence(ctx) }
	time.Sleep(150 * time.Millisecond)
	reap()
	if s := live(s3.Address); s == nil || s.Note != "presence stale" {
		t.Fatalf("box3 soon after it went: %+v", s)
	}
	time.Sleep(1200 * time.Millisecond)
	reap()
	ended(s3.Address, "presence lost")
}

// An unreachable authority (a dial that hangs, as to a closed laptop) costs
// a merged list one DialTimeout, not CallTimeout, and authorities are asked
// at once, not one after another.
func TestMergedListsWithOfflineAuthorities(t *testing.T) {
	timing := newTeamTiming(t, "merged-list")
	quietPresence(t)
	oldDial, oldCall := teamlink.DialTimeout, teamlink.CallTimeout
	teamlink.DialTimeout, teamlink.CallTimeout = 400*time.Millisecond, 5*time.Second
	t.Cleanup(func() { teamlink.DialTimeout, teamlink.CallTimeout = oldDial, oldCall })
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	b := startDaemon(t, tn, "box", tailnettest.Identity{NodeID: "nBOXA", NodeKey: "nodekey:box", Login: "ivo@example.com", Host: "box"})
	mustf(b.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	mustf(b.owner.ProjectTeam(ctx, "side"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(b.owner.Invite(ctx, api.InviteRequest{Project: "side", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); len(wl.Skipped) != 0 {
		t.Fatalf("both up: %+v", wl.Skipped)
	}
	// Both authorities go offline: their connections drop and new dials hang.
	tn.Offline("potluck-mini", true)
	tn.Offline("potluck-box", true)
	a.stop()
	b.stop()

	// Round C: each joined project has a copy, so the list is served from
	// it, marked behind (stale) once the replica saw its authority go, with
	// no tailnet call at all.
	eventually(t, 3*time.Second, func() error {
		for _, rm := range mustf(r.owner.Remotes(ctx))(t) {
			if rm.Replica == nil || rm.Replica.Reachable {
				return fmt.Errorf("remote %s still reachable: %+v", rm.Project.Slug, rm.Replica)
			}
		}
		return nil
	})
	start := time.Now()
	wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
	timing.record("team/merged-offline-work-list", []time.Duration{time.Since(start)}, 100*time.Millisecond)
	if len(wl.Skipped) != 0 || len(wl.Behind) != 2 {
		t.Fatalf("skipped %+v behind %+v", wl.Skipped, wl.Behind)
	}
	start = time.Now()
	var pl api.ProjectList
	if err := r.owner.Do(ctx, http.MethodGet, "/v0/projects", nil, &pl); err != nil {
		t.Fatal(err)
	}
	timing.record("team/merged-offline-project-list", []time.Duration{time.Since(start)}, 100*time.Millisecond)
	if len(pl.Skipped) != 2 || len(pl.Projects) < 2 {
		t.Fatalf("project ls: skipped %+v, projects %d", pl.Skipped, len(pl.Projects))
	}
}
