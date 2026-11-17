package work

import (
	"fmt"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

// A tracker with more items than one page holds is mirrored whole (paged),
// and a partial full listing drops nothing it did not return: the items
// it left out stay readable rather than turning not_found.
func TestMirrorPartialListing(t *testing.T) {
	quiet(t)
	oldFull := fullEvery
	fullEvery = 0 // every sync is a full listing
	t.Cleanup(func() { fullEvery = oldFull })
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		for n := 1; n <= 10; n++ {
			id := fmt.Sprintf("app-%02d", n)
			st.Issues[id] = &fake.Issue{ID: id, Title: fmt.Sprint("issue ", n), Status: "open", UpdatedAt: fake.Now()}
		}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"page": "4", "partial": "true"}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	for n := 1; n <= 10; n++ {
		if _, err := orchclient.GetWork(h.ctx, owner, fmt.Sprintf("bd:app-%02d", n)); err != nil {
			t.Fatalf("#%d after a paged listing: %v", n, err)
		}
	}
	// Gone from the tracker, but the next full listing is partial: kept.
	edit(t, state, func(st *fake.State) { delete(st.Issues, "app-01") })
	h.sync()
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:app-02"); err != nil {
		t.Fatalf("#2 after a partial listing: %v", err)
	}
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:app-01"); err != nil {
		t.Fatalf("a partial listing dropped what it did not return: %v", err)
	}
}

// An item without a title is still mirrored (nothing says trackers must
// give one).
func TestMirrorUntitled(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Status: "open", UpdatedAt: fake.Now()}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if it, err := orchclient.GetWork(h.ctx, h.as(h.owner, ""), "bd:solver-1"); err != nil || it.Title != "" {
		t.Fatalf("untitled %+v %v", it, err)
	}
}
