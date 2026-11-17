package work

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

// A claim that replaces another holder's expired lease (before the ticker
// dropped it) ends the old holder's tracker claim first; otherwise the
// tracker still holds the item for the old holder and refuses the new one,
// and the new lease is released as a tracker conflict. Seen on a
// handover between agents: the successor could not take its lane's beads.
func TestClaimOverExpiredLeaseReleasesOldTrackerClaim(t *testing.T) {
	for _, team := range []bool{false, true} {
		name := "solo"
		if team {
			name = "team"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			state := h.installFake()
			st, _ := fake.Load(state)
			st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "handed over", Status: "open"}
			if err := st.Save(state); err != nil {
				t.Fatal(err)
			}
			trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"actor": "zm"}, Env: "inherit"}}
			if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
				t.Fatal(err)
			}
			h.sync()
			if team {
				if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			drain := func() {
				for {
					select {
					case j := <-h.s.wb:
						h.s.writeOne(h.ctx, j)
					default:
						return
					}
				}
			}
			old := h.as(h.session("mini/0dd0c0de", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
			next := h.as(h.session("mini/c1a0de01", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
			if _, err := orchclient.Claim(h.ctx, old, "bd:solver-1", orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
				t.Fatal(err)
			}
			drain()
			st, _ = fake.Load(state)
			if is := st.Issues["solver-1"]; is.Status != "in_progress" || is.Assignee != "mini/0dd0c0de" {
				t.Fatalf("first claim in the tracker: %+v", is)
			}
			// The old lease runs out; the successor claims before any tick.
			h.sessions.setState("mini/0dd0c0de", orchapi.StateEnded)
			h.clock.add(61 * time.Second)
			if r, err := orchclient.Claim(h.ctx, next, "bd:solver-1", orchapi.ClaimRequest{}); err != nil || r.Lease.Holder != "mini/c1a0de01" {
				t.Fatalf("successor claim %+v %v", r, err)
			}
			drain()
			st, _ = fake.Load(state)
			if is := st.Issues["solver-1"]; is.Status != "in_progress" || is.Assignee != "mini/c1a0de01" {
				t.Fatalf("tracker after the successor's claim: %+v", is)
			}
			if l, _ := h.s.liveLease(h.ctx, h.harbor.ID, "solver", "bd:solver-1"); l == nil || l.Holder != "mini/c1a0de01" {
				t.Fatalf("successor lease %+v", l)
			}
			for _, e := range h.events(orchapi.EventWorkReleased) {
				if strings.Contains(string(e.Data), NoteTrackerConflict) {
					t.Fatalf("successor's lease released as a tracker conflict: %s", e.Data)
				}
			}
			if evs := h.events(orchapi.EventWorkLeaseExpired); len(evs) != 1 {
				t.Fatalf("lease_expired events %+v", evs)
			}
		})
	}
}
