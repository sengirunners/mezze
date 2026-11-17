package work

import (
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fencing"
	"github.com/sengirunners/potluck/kit/api"
)

func mustStale(t *testing.T, what string, err error) {
	t.Helper()
	if !orchapi.IsStaleLease(err) {
		t.Fatalf("%s: want a stale-lease refusal, got %v", what, err)
	}
}

// A holder that outlived its lease cannot write or release under it (SPEC
// "Leases": fencing): its fence is refused once the lease lapsed, was taken
// by another holder, or was taken again by the same holder, and a late
// release never drops the newer lease. Solo and team projects alike.
func TestFenceRefusesOutlivedHolder(t *testing.T) {
	for _, team := range []bool{false, true} {
		name := "solo"
		if team {
			name = "team"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			owner := h.as(h.owner, "harbor")
			if team {
				if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			it, err := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "fenced", Area: "solver"})
			if err != nil {
				t.Fatal(err)
			}
			a := h.as(h.d.Projects.Local("mini/aaaa0001"), "")
			b := h.as(h.d.Projects.Local("mini/bbbb0002"), "")

			r, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60})
			if err != nil || r.Lease.Fence <= 0 {
				t.Fatalf("claim %+v %v", r, err)
			}
			f1 := r.Lease.Fence
			// A renewal keeps the fence; one naming it renews only that lease.
			if r, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60, Renew: true, Fence: f1}); err != nil || r.Lease.Fence != f1 {
				t.Fatalf("renew with fence %+v %v", r, err)
			}
			if _, err := orchclient.Comment(h.ctx, a, it.ID, orchapi.CommentRequest{Body: "under the lease", Fence: f1}); err != nil {
				t.Fatal(err)
			}
			if _, err := orchclient.SetStatus(h.ctx, a, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview, Fence: f1}); err != nil {
				t.Fatal(err)
			}
			// Only the holder writes under its fence.
			mustStale(t, "another caller's write under a's fence", func() error {
				_, err := orchclient.Comment(h.ctx, b, it.ID, orchapi.CommentRequest{Body: "not mine", Fence: f1})
				return err
			}())

			// The lease lapses (no tick yet): a's writes under it are refused,
			// unfenced writes are as before.
			h.clock.add(61 * time.Second)
			_, err = orchclient.Comment(h.ctx, a, it.ID, orchapi.CommentRequest{Body: "resumed after expiry", Fence: f1})
			mustStale(t, "comment after expiry", err)
			_, err = orchclient.SetStatus(h.ctx, a, it.ID, orchapi.StatusRequest{Status: orchapi.WorkDone, Fence: f1})
			mustStale(t, "status after expiry", err)
			_, err = orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{Renew: true, Fence: f1})
			mustStale(t, "renew after expiry", err)
			if _, err := orchclient.Comment(h.ctx, a, it.ID, orchapi.CommentRequest{Body: "a plain comment"}); err != nil {
				t.Fatalf("unfenced comment: %v", err)
			}
			if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Status == orchapi.WorkDone {
				t.Fatal("a stale status write landed")
			}

			// b takes it over: a larger fence; a's late release is refused and
			// b's lease stands.
			rb, err := orchclient.Claim(h.ctx, b, it.ID, orchapi.ClaimRequest{TTLSeconds: 600})
			if err != nil || rb.Lease.Fence <= f1 {
				t.Fatalf("b's claim %+v %v (a's fence %d)", rb, err, f1)
			}
			_, err = orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{Fence: f1})
			mustStale(t, "a's release over b's lease", err)
			if l, _ := h.s.liveLease(h.ctx, h.harbor.ID, "solver", it.ID); l == nil || l.Holder != "mini/bbbb0002" || l.Fence != rb.Lease.Fence {
				t.Fatalf("b's lease after a's late release: %+v", l)
			}
			// Even an owner's --force names a fence only to release that lease.
			_, err = orchclient.Release(h.ctx, owner, it.ID, orchapi.ReleaseRequest{Force: true, Fence: f1})
			mustStale(t, "forced release with a stale fence", err)
			if rr, err := orchclient.Release(h.ctx, b, it.ID, orchapi.ReleaseRequest{Fence: rb.Lease.Fence}); err != nil || !rr.Released {
				t.Fatalf("b's own release %+v %v", rr, err)
			}

			// The same holder claiming again gets a new fence: a release
			// carrying the old one (a late, unsettled release) drops nothing.
			r2, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60})
			if err != nil || r2.Lease.Fence <= rb.Lease.Fence {
				t.Fatalf("a's new claim %+v %v", r2, err)
			}
			_, err = orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{Fence: f1})
			mustStale(t, "late release of a's first lease", err)
			if rr, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{Fence: r2.Lease.Fence}); err != nil || !rr.Released {
				t.Fatalf("release with the current fence %+v %v", rr, err)
			}
			// Gone: a fenced release answers like an unfenced one (nothing held).
			if rr, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{Fence: r2.Lease.Fence}); err != nil || rr.Released {
				t.Fatalf("release of a gone lease %+v %v", rr, err)
			}

			// Plain locks: DELETE /v0/leases/{key} with a stale fence.
			lock := h.as(h.d.Projects.Local("mini/aaaa0001"), "harbor/solver")
			l1, err := orchclient.Lease(h.ctx, lock, orchapi.ClaimRequest{Key: "lock:migrations", TTLSeconds: 60})
			if err != nil {
				t.Fatal(err)
			}
			h.clock.add(61 * time.Second)
			l2, err := orchclient.Lease(h.ctx, lock, orchapi.ClaimRequest{Key: "lock:migrations", TTLSeconds: 60})
			if err != nil || l2.Lease.Fence <= l1.Lease.Fence {
				t.Fatalf("lock taken again %+v %v (was %d)", l2, err, l1.Lease.Fence)
			}
			_, err = orchclient.ReleaseLease(h.ctx, lock, "lock:migrations", orchapi.ReleaseRequest{Fence: l1.Lease.Fence})
			mustStale(t, "late lock release", err)
			if rr, err := orchclient.ReleaseLease(h.ctx, lock, "lock:migrations", orchapi.ReleaseRequest{Fence: l2.Lease.Fence}); err != nil || !rr.Released {
				t.Fatalf("lock release %+v %v", rr, err)
			}
		})
	}
}

// A fenced write on an item the caller cannot see answers exactly like a
// missing item, never with the lease's refusal.
func TestFenceHiddenEqualsMissing(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, err := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "solver only", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := orchclient.Claim(h.ctx, owner, it.ID, orchapi.ClaimRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "bill", Project: "harbor", Area: "billing", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	billing := h.as(sub, "")
	for name, err := range map[string]error{
		"comment": func() error {
			_, err := orchclient.Comment(h.ctx, billing, it.ID, orchapi.CommentRequest{Body: "x", Fence: r.Lease.Fence})
			return err
		}(),
		"release": func() error {
			_, err := orchclient.Release(h.ctx, billing, it.ID, orchapi.ReleaseRequest{Fence: r.Lease.Fence})
			return err
		}(),
		"missing": func() error {
			_, err := orchclient.Comment(h.ctx, billing, "local:zzzzzz", orchapi.CommentRequest{Body: "x", Fence: r.Lease.Fence})
			return err
		}(),
	} {
		if !isCode(err, api.CodeNotFound) || orchapi.IsStaleLease(err) {
			t.Fatalf("%s on a hidden item: %v", name, err)
		}
	}
}

// Fences only grow, whichever file holds a project's leases: zm project
// team carries the counter with the rows, and so does solo.
func TestFenceMonotonicAcrossMoves(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor/solver")
	last := int64(0)
	take := func(key string) {
		t.Helper()
		r, err := orchclient.Lease(h.ctx, c, orchapi.ClaimRequest{Key: key, TTLSeconds: 60})
		if err != nil || r.Lease.Fence <= last {
			t.Fatalf("%s: %+v %v (last %d)", key, r, err, last)
		}
		last = r.Lease.Fence
	}
	// A counter far above what one random step reaches: a move that left it
	// behind would hand out a smaller fence, always (not one run in two).
	if err := fencing.Raise(h.ctx, h.d.Store.Registry(), h.harbor.ID, 1<<40); err != nil {
		t.Fatal(err)
	}
	take("lock:one")
	if last <= 1<<40 {
		t.Fatalf("fence %d is not above the seeded counter", last)
	}
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatal(err)
	}
	take("lock:two")
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, false); err != nil {
		t.Fatal(err)
	}
	take("lock:three")
	// A renewal of a moved lease keeps its fence.
	ls, err := orchclient.Leases(h.ctx, c, orchapi.LeaseQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.Fence == 0 {
			t.Fatalf("a moved lease lost its fence: %+v", l)
		}
		if l.Key == "lock:one" {
			r, err := orchclient.Lease(h.ctx, c, orchapi.ClaimRequest{Key: "lock:one", TTLSeconds: 60, Renew: true, Fence: l.Fence})
			if err != nil || r.Lease.Fence != l.Fence {
				t.Fatalf("renew moved lease %+v %v", r, err)
			}
		}
	}
}
