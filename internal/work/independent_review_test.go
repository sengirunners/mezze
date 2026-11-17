package work

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"testing"
	"time"
)

func TestIndependentReviewRemovedDeadlineAnnounced(t *testing.T) {
	quiet(t)
	old := fullEvery
	fullEvery = 0
	t.Cleanup(func() { fullEvery = old })
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["a"] = &fake.Issue{ID: "a", Title: "a", Status: "open", IssueType: "task", UpdatedAt: fake.Now(), DueAt: time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	r, err := orchclient.WorkRollup(h.ctx, owner, "harbor", "")
	if err != nil || len(r.Rollups[0].Deadlines) != 1 {
		t.Fatalf("seed %+v %v", r, err)
	}
	before := len(h.events(orchapi.EventWorkChanged))
	edit(t, state, func(st *fake.State) { st.Issues["a"].DueAt = "" })
	h.sync()
	r, err = orchclient.WorkRollup(h.ctx, owner, "harbor", "")
	if err != nil || len(r.Rollups[0].Deadlines) != 0 {
		t.Fatalf("authority %+v %v", r, err)
	}
	if after := len(h.events(orchapi.EventWorkChanged)); after != before+1 {
		t.Fatalf("authority removed deadline but replica gets no change: events before=%d after=%d", before, after)
	}
}
