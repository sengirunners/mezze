package work

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// An issue move ends the old lease after the issue left its source,
// and a sync may drop the item's mirror row in between. EndItemLease must end
// the lease anyway, not read the missing row as "no lease".
func TestEndItemLeaseAfterTheMirrorRowWent(t *testing.T) {
	for _, team := range []bool{false, true} {
		name := map[bool]string{false: "solo", true: "team"}[team]
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			state := h.installFake()
			st, _ := fake.Load(state)
			st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "moving", Status: "open"}
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
			c := h.as(h.session("mini/0dd0c0de", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
			if _, err := orchclient.Claim(h.ctx, c, "bd:solver-1", orchapi.ClaimRequest{TTLSeconds: 600}); err != nil {
				t.Fatal(err)
			}
			// The item leaves its source and a sync drops its mirror row.
			st, _ = fake.Load(state)
			delete(st.Issues, "solver-1")
			if err := st.Save(state); err != nil {
				t.Fatal(err)
			}
			db, err := h.d.Store.Project(authz.All(), authz.Write, h.harbor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := mirrorDelete(h.ctx, db, "bd", []string{"bd:solver-1"}); err != nil || n != 1 {
				t.Fatalf("drop the mirror row: %d %v", n, err)
			}
			if err := h.s.EndItemLease(h.ctx, h.harbor.ID, "bd:solver-1", "", "moved to bd-x"); err != nil {
				t.Fatal(err)
			}
			if l, err := h.s.liveLease(h.ctx, h.harbor.ID, "", "bd:solver-1"); err != nil || l != nil {
				t.Fatalf("the lease outlived its end: %+v %v", l, err)
			}
		})
	}
}
