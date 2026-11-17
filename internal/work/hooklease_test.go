package work

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A hook session (an agent zm did not start: cmux, a plain terminal) that
// claims with no TTL of its own holds a bound lease: renewed by the daemon
// while the session runs, however long, and released when it ends.
func TestHookSessionClaimHeldWhileActive(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "two hours of work", Area: "solver"})
	sub := h.session("mini/hook0001", h.harbor.ID, "solver", false, orchapi.StateRunning)
	r, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{})
	if err != nil || !r.Lease.Bound || r.Lease.Holder != "mini/hook0001" {
		t.Fatalf("hook session claim %+v %v", r, err)
	}
	// Active for 2 h (a tick every 30 s), past the 1 h default TTL twice over.
	for i := 0; i < 240; i++ {
		h.clock.add(30 * time.Second)
		h.s.tick(h.ctx)
	}
	ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{})
	if len(ls) != 1 || ls[0].Holder != "mini/hook0001" || !ls[0].Expires.After(h.clock.now().Add(59*time.Minute)) {
		t.Fatalf("after 2 h active: %+v", ls)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Status != orchapi.WorkClaimed {
		t.Fatalf("item after 2 h: %+v", got)
	}
	if len(h.mail.all()) != 0 || len(h.events(orchapi.EventWorkLeaseExpired)) != 0 {
		t.Fatal("an active session's lease expired")
	}
}

// A hook session that ends (its session-end hook, or its process gone)
// gives its bound leases up at the next tick, not an hour later.
func TestHookSessionEndedReleasesSoon(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "solver"})
	sub := h.session("mini/hook0002", h.harbor.ID, "solver", false, orchapi.StateRunning)
	if _, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	h.clock.add(10 * time.Minute)
	h.s.tick(h.ctx)
	h.sessions.setState("mini/hook0002", orchapi.StateEnded)
	h.clock.add(30 * time.Second) // one lease tick
	h.s.tick(h.ctx)
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 0 {
		t.Fatalf("ended session still holds %+v", ls)
	}
	if rel := h.events(orchapi.EventWorkReleased); len(rel) != 1 || !strings.Contains(string(rel[0].Data), "session ended") {
		t.Fatalf("released %+v", rel)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("item %+v", got)
	}
}

// A hook session that stops running (idle, or silent: quietHooks sets it
// idle) is not renewed: its lease lapses one bound TTL later and it is told.
func TestHookSessionIdleLapses(t *testing.T) {
	h := newHarness(t)
	h.d.Config.File.BoundLeaseTTLSeconds = 600
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "solver"})
	sub := h.session("mini/hook0003", h.harbor.ID, "solver", false, orchapi.StateRunning)
	if _, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	h.sessions.setState("mini/hook0003", orchapi.StateIdle)
	h.clock.add(601 * time.Second)
	h.s.tick(h.ctx)
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 0 {
		t.Fatalf("idle session still holds %+v", ls)
	}
	if sent := h.mail.all(); len(sent) != 1 || !strings.Contains(sent[0].req.Body, "bound to a session that stopped running") {
		t.Fatalf("mail %+v", sent)
	}
}

// A hook session's explicit TTL is a TTL lease, as before; a lease with the
// default TTL whose holder became known and runs afterwards (claimed before
// its replica's first heartbeat reached the authority, or before an upgrade)
// is bound at the next tick.
func TestHookSessionTTLAndAdoption(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	a, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "ttl", Area: "solver"})
	b, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "late", Area: "solver"})
	sub := h.session("mini/hook0004", h.harbor.ID, "solver", false, orchapi.StateRunning)
	if r, err := orchclient.Claim(h.ctx, h.as(sub, ""), a.ID, orchapi.ClaimRequest{TTLSeconds: 120}); err != nil || r.Lease.Bound {
		t.Fatalf("explicit TTL %+v %v", r, err)
	}
	late := h.d.Projects.Local("mini/late0001") // not known to the session module yet
	if r, err := orchclient.Claim(h.ctx, h.as(late, ""), b.ID, orchapi.ClaimRequest{}); err != nil || r.Lease.Bound {
		t.Fatalf("unknown session's claim %+v %v", r, err)
	}
	h.clock.add(30 * time.Second)
	h.s.tick(h.ctx) // still unknown: untouched
	h.sessions.put(orchapi.Session{Address: "mini/late0001", Project: h.harbor.ID, Area: "solver", State: orchapi.StateRunning})
	h.clock.add(30 * time.Second)
	h.s.tick(h.ctx)
	ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{})
	got := map[string]orchapi.Lease{}
	for _, l := range ls {
		got[l.Holder] = l
	}
	if l := got["mini/late0001"]; !l.Bound || !l.Expires.Equal(h.clock.now().Add(time.Hour)) {
		t.Fatalf("not adopted: %+v", l)
	}
	if l := got["mini/hook0004"]; l.Bound {
		t.Fatalf("an explicit TTL was bound: %+v", l)
	}
	// The explicit TTL runs out as asked, though its session still runs.
	h.clock.add(time.Minute)
	h.s.tick(h.ctx)
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 || ls[0].Holder != "mini/late0001" {
		t.Fatalf("after the explicit TTL: %+v", ls)
	}
}

// zm project team moves a hook session's bound lease with the rest (only a
// managed session's refuses the move), and the ticker keeps renewing it in
// the team file.
func TestHookSessionLeaseMovesToTeam(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "solver"})
	sub := h.session("mini/hook0005", h.harbor.ID, "solver", false, orchapi.StateRunning)
	if _, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatalf("move with a hook session's bound lease: %v", err)
	}
	for i := 0; i < 12; i++ { // 2 h, a tick every 10 min
		h.clock.add(10 * time.Minute)
		h.s.tick(h.ctx)
	}
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 || !ls[0].Bound || !ls[0].Expires.After(h.clock.now()) {
		t.Fatalf("after the move: %+v", ls)
	}
	managed := h.as(h.session("mini/managed9", h.harbor.ID, "solver", true, orchapi.StateRunning), "")
	other, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "y", Area: "solver"})
	if _, err := orchclient.Claim(h.ctx, managed, other.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, false); err != nil {
		t.Fatalf("back to solo: %v", err)
	}
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); !isCode(err, api.CodeConflict) {
		t.Fatalf("a managed session's bound lease must still refuse the move: %v", err)
	}
}

// An explicit ttl_seconds makes a TTL lease even on the renewal of a bound
// one (`zm lease lock:x --ttl 60` after a claim), and the ticker never binds
// a lease whose TTL was named, even one of exactly the default 3600 s.
func TestSessionExplicitTTLWins(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, team := range []bool{false, true} {
			t.Run(fmt.Sprintf("managed=%t/team=%t", managed, team), func(t *testing.T) {
				h := newHarness(t)
				if team {
					if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
						t.Fatal(err)
					}
				}
				owner := h.as(h.owner, "harbor")
				it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "solver"})
				other, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "y", Area: "solver"})
				c := h.as(h.session("mini/hook0006", h.harbor.ID, "solver", managed, orchapi.StateRunning), "")
				if r, err := orchclient.Claim(h.ctx, c, it.ID, orchapi.ClaimRequest{}); err != nil || !r.Lease.Bound {
					t.Fatalf("claim %+v %v", r, err)
				}
				r, err := orchclient.Claim(h.ctx, c, it.ID, orchapi.ClaimRequest{TTLSeconds: 60})
				if err != nil || r.Lease.Bound || !r.Lease.Expires.Equal(h.clock.now().Add(time.Minute)) {
					t.Fatalf("renewed with --ttl 60: %+v %v", r, err)
				}
				if r, err := orchclient.Claim(h.ctx, c, other.ID, orchapi.ClaimRequest{TTLSeconds: 3600}); err != nil || r.Lease.Bound {
					t.Fatalf("--ttl 3600: %+v %v", r, err)
				}
				if r, err := orchclient.Lease(h.ctx, c, orchapi.ClaimRequest{Key: "lock:heavy-build", TTLSeconds: 60}); err != nil || r.Lease.Bound || !r.Lease.Expires.Equal(h.clock.now().Add(time.Minute)) {
					t.Fatalf("lock with --ttl 60: %+v %v", r, err)
				}
				h.clock.add(30 * time.Second)
				h.s.tick(h.ctx)
				ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{})
				for _, l := range ls {
					if l.Bound {
						t.Fatalf("the ticker bound a named TTL: %+v", l)
					}
				}
				h.clock.add(31 * time.Second)
				h.s.tick(h.ctx)
				if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 || ls[0].Key != other.ID {
					t.Fatalf("the 60 s lease did not run out as asked: %+v", ls)
				}
			})
		}
	}
}

// A session store that does not answer is not a session that ended: the
// tick keeps the bound lease and decides at the next one.
func TestBoundLeaseKeptThroughLookupError(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "solver"})
	c := h.as(h.session("mini/hook0007", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, c, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	h.sessions.mu.Lock()
	h.sessions.err = errors.New("database is locked")
	h.sessions.mu.Unlock()
	h.clock.add(30 * time.Second)
	h.s.tick(h.ctx)
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 {
		t.Fatalf("a lookup error dropped the lease: %+v", ls)
	}
	h.sessions.mu.Lock()
	h.sessions.err = nil
	h.sessions.mu.Unlock()
	h.sessions.setState("mini/hook0007", orchapi.StateEnded)
	h.s.tick(h.ctx)
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 0 {
		t.Fatalf("ended, store answering: %+v", ls)
	}
}
