package team_test

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func waitRoster(t *testing.T, state func() (bool, bool)) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, valid := state(); valid {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the roster cache never filled")
}

// The machine owner's `zm ls` on a replica is answered from the roster cache
// while the authority is cut, and returns the authority's sessions; a session
// caller on the same replica still goes to the authority (and gets nothing
// from the cache); leaving drops the cache.
func TestOwnerSessionListFromRosterCache(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.SetRosterWait(200 * time.Millisecond))
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	state := teamService(t, r).RosterState("harbor")
	waitRoster(t, state)

	ms := mustf(orchclient.Hook(ctx, at(a.owner, "harbor", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "mini-roster-1", State: orchapi.StateRunning}}))(t).Session
	local := mustf(orchclient.Hook(ctx, at(r.owner, "", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-roster-1", State: orchapi.StateRunning}}))(t).Session

	// A healthy read is live, and teaches the cache the authority's new session.
	if findSession(mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t), ms.Address) == nil {
		t.Fatal("live owner read misses the authority's session")
	}

	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	t0 := time.Now()
	got := mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("owner read with the authority cut took %s", d)
	}
	if findSession(got, ms.Address) == nil {
		t.Fatalf("cached roster lacks the authority's session: %+v", got)
	}
	if findSession(got, local.Address) == nil {
		t.Fatalf("own session missing: %+v", got)
	}

	// A session caller is not the owner: it forwards (and the cut shows).
	sc := mustf(orchclient.Sessions(ctx, at(r.owner, "", local.Address), orchapi.SessionQuery{}))(t)
	if findSession(sc, ms.Address) != nil {
		t.Fatalf("a session caller was served the owner's cached roster: %+v", sc)
	}
	// So does an owner read with a query.
	if q := mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{All: true}))(t); findSession(q, ms.Address) != nil {
		t.Fatalf("a query read was served from the cache: %+v", q)
	}

	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	mustf(r.owner.Leave(ctx, "harbor"))(t)
	if cached, _ := state(); cached {
		t.Fatal("leave left the roster cache behind")
	}
}

// A revoked replica serves nothing from its cache: with the authority cut
// after the revoke is seen, the owner's list has none of its sessions.
func TestRosterCacheDroppedOnRevoke(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.SetRosterWait(200 * time.Millisecond))
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	state := teamService(t, r).RosterState("harbor")
	ms := mustf(orchclient.Hook(ctx, at(a.owner, "harbor", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "mini-roster-revoke", State: orchapi.StateRunning}}))(t).Session
	if findSession(mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t), ms.Address) == nil {
		t.Fatal("fixture: the owner's list lacks the authority's session")
	}
	waitRoster(t, state)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}) // observes the revoke
		if _, valid := state(); !valid && teamService(t, r).Ended("harbor") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", true) // only the replica's own state can answer now
	defer tn.Cut("potluck-bea-mac", "potluck-mini", false)
	got, _ := orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{})
	if findSession(got, ms.Address) != nil {
		t.Fatalf("a revoked replica served its cached roster: %+v", got)
	}
}

// Owner reads at once, some answered live and some from the cache while the
// link flaps, never share a map (run with -race).
func TestOwnerSessionListConcurrentReads(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.SetRosterWait(20 * time.Millisecond))
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitRoster(t, teamService(t, r).RosterState("harbor"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			tn.Cut("potluck-bea-mac", "potluck-mini", i%2 == 0)
			time.Sleep(15 * time.Millisecond)
		}
		tn.Cut("potluck-bea-mac", "potluck-mini", false)
	}()
	errs := make(chan error, 4)
	for g := 0; g < 4; g++ {
		go func() {
			var err error
			for i := 0; i < 15 && err == nil; i++ {
				_, err = orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{})
			}
			errs <- err
		}()
	}
	for g := 0; g < 4; g++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	<-done
}

// A stalled authority costs the owner's list RosterWait, not the stall.
func TestOwnerSessionListStalledAuthority(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.SetRosterWait(100 * time.Millisecond))
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitRoster(t, teamService(t, r).RosterState("harbor"))
	ms := mustf(orchclient.Hook(ctx, at(a.owner, "harbor", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "mini-roster-2", State: orchapi.StateRunning}}))(t).Session
	mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t) // live: teaches the cache
	tn.Stall("potluck-bea-mac", "potluck-mini", 10*time.Second)
	t0 := time.Now()
	got := mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("owner read during a 10 s stall took %s", d)
	}
	if findSession(got, ms.Address) == nil {
		t.Fatalf("stalled read lacks the authority's session: %+v", got)
	}
}
