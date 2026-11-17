package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// A claim, a release and an expiry that move a local item's status publish
// work.status_changed with the item (cockpit gap G3), not only the lease
// events; a renewal, which moves nothing, publishes none.
func TestClaimReleasePublishStatus(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, err := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "Status me", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	a := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	status := func() []orchapi.WorkItem {
		var out []orchapi.WorkItem
		for _, e := range h.events(orchapi.EventWorkStatusChanged) {
			var w orchapi.WorkItem
			if err := json.Unmarshal(e.Data, &w); err != nil {
				t.Fatal(err)
			}
			if e.Subject != it.ID || e.Area != "solver" || e.Project != h.harbor.ID || w.Links != nil {
				t.Fatalf("status event %+v", e)
			}
			out = append(out, w)
		}
		return out
	}

	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if got := status(); len(got) != 1 || got[0].Status != orchapi.WorkClaimed || got[0].Owner != "mini/aaaaaaaa" {
		t.Fatalf("after claim: %+v", got)
	}
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60, Renew: true}); err != nil {
		t.Fatal(err)
	}
	if got := status(); len(got) != 1 {
		t.Fatalf("a renewal published a status change: %+v", got)
	}
	if _, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := status(); len(got) != 2 || got[1].Status != orchapi.WorkOpen || got[1].Owner != "" {
		t.Fatalf("after release: %+v", got)
	}

	// Expiry: the ticker frees the key and reopens the item.
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	h.clock.add(61 * time.Second)
	h.s.tick(h.ctx)
	if got := status(); len(got) != 4 || got[3].Status != orchapi.WorkOpen {
		t.Fatalf("after expiry: %+v", got)
	}

	// An item a human moved on (in_review) is not reopened by a release, so
	// no status event either.
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetStatus(h.ctx, owner, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); err != nil {
		t.Fatal(err)
	}
	n := len(status())
	if _, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := status(); len(got) != n {
		t.Fatalf("release of an in_review item published %+v", got[n:])
	}
}

// A tracker item's claim and release publish work.status_changed with the
// adapter's answer whatever its mirror held before: a stale mirror that
// already said "claimed" does not suppress the claim's event, and a
// missing mirror row (purged by a resync) does not suppress the release's
// (comparing with the mirror dropped both).
func TestTrackerStatusEventsIgnoreMirror(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "Fix solver timeout", Status: "open"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"actor": "zm"}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	db, err := h.d.Store.Project(authz.All(), authz.Write, h.harbor.ID)
	if err != nil {
		t.Fatal(err)
	}
	moves := func() []orchapi.WorkStatus {
		var out []orchapi.WorkStatus
		for _, e := range h.events(orchapi.EventWorkStatusChanged) {
			var w orchapi.WorkItem
			if e.Subject == "bd:solver-1" && json.Unmarshal(e.Data, &w) == nil {
				if e.Area != "solver" {
					t.Fatalf("status event in area %q", e.Area)
				}
				out = append(out, w.Status)
			}
		}
		return out
	}
	if _, err := db.ExecContext(h.ctx, `UPDATE work_mirror SET status = 'claimed' WHERE source = 'bd' AND ref = 'solver-1'`); err != nil {
		t.Fatal(err)
	}
	sess := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, sess, "bd:solver-1", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := moves(); len(got) != 1 || got[0] != orchapi.WorkClaimed {
		t.Fatalf("claim over a stale mirror: %v", got)
	}
	if _, err := orchclient.Claim(h.ctx, sess, "bd:solver-1", orchapi.ClaimRequest{Renew: true}); err != nil {
		t.Fatal(err)
	}
	if got := moves(); len(got) != 1 {
		t.Fatalf("a renewal published a status change: %v", got)
	}
	h.sync()
	if _, err := db.ExecContext(h.ctx, `DELETE FROM work_mirror WHERE source = 'bd' AND ref = 'solver-1'`); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Hour)
	h.s.tick(h.ctx)
	if got := moves(); len(got) != 2 || got[1] != orchapi.WorkOpen {
		t.Fatalf("expiry with no mirror row: %v", got)
	}
}
