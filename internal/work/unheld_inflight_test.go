package work

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// An item in review with a pr link whose lease lapsed (its agent waiting on
// CI) still shows in inflight, marked unheld; an open, done or unlinked item
// does not, nor does one a caller cannot read.
func TestInflightListsUnheldPRItems(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	mk := func(title, area string) orchapi.WorkItem {
		it, err := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: title, Area: area})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	pr := func(id string, n string) {
		if _, err := orchclient.Link(h.ctx, owner, id, orchapi.Link{Rel: "pr", Target: "https://github.com/o/r/pull/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	waiting, open, done, plain, hidden := mk("waiting on CI", "solver"), mk("open with a PR", "solver"), mk("merged", "solver"), mk("review, no PR", "solver"), mk("hidden review", "billing")
	for _, id := range []string{waiting.ID, open.ID, done.ID, hidden.ID} {
		pr(id, id[len(id)-3:])
	}
	for _, it := range []orchapi.WorkItem{waiting, plain, hidden} {
		if _, err := orchclient.SetStatus(h.ctx, owner, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := orchclient.SetStatus(h.ctx, owner, done.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}); err != nil {
		t.Fatal(err)
	}
	// A held item with a PR shows once, as its lease.
	held := mk("held with a PR", "solver")
	pr(held.ID, "777")
	a := h.as(h.session("mini/aaaa1111", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, a, held.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	got, err := orchclient.Inflight(h.ctx, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	unheld, leased := map[string]bool{}, map[string]int{}
	for _, r := range got {
		if r.Unheld {
			if r.Lease.Holder != "" || r.Lease.Expires != nil || r.Session != nil || r.Item == nil || r.Item.ID != r.Lease.Key {
				t.Fatalf("unheld row shape %+v", r)
			}
			unheld[r.Lease.Key] = true
		} else {
			leased[r.Lease.Key]++
		}
	}
	if !unheld[waiting.ID] || !unheld[hidden.ID] || unheld[open.ID] || unheld[done.ID] || unheld[plain.ID] || unheld[held.ID] || leased[held.ID] != 1 {
		t.Fatalf("owner rows: unheld %v leased %v", unheld, leased)
	}
	// A guest of solver sees the solver row, never billing's.
	g := h.as(h.guest(), "")
	gs, err := orchclient.Inflight(h.ctx, g, "")
	if err != nil {
		t.Fatal(err)
	}
	sawWaiting := false
	for _, r := range gs {
		if r.Lease.Key == hidden.ID {
			t.Fatalf("guest sees a hidden unheld item %+v", r)
		}
		sawWaiting = sawWaiting || r.Unheld && r.Lease.Key == waiting.ID
	}
	if !sawWaiting {
		t.Fatalf("guest misses the solver unheld item: %+v", gs)
	}
}
