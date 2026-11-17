package work

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
)

func TestDurableCommentRefusesUnsupportedExternalSink(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, err := fake.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "external decision", Status: "open"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trackers := []api.Tracker{{Adapter: "fake", Prefix: "bd", Area: "solver", Config: map[string]string{"actor": "zm"}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trackers}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	err = h.s.RecordComment(h.ctx, h.owner, h.harbor.ID, "bd:solver-1", ids.New(), "approved answer")
	if !api.IsCode(err, api.CodeNotImplemented) || !strings.Contains(err.Error(), "unknown:") {
		t.Fatalf("unsupported delivery: %v", err)
	}
	after, err := fake.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range after.Calls {
		if strings.Contains(call, "comment") {
			t.Fatalf("unsupported external write: %+v", call)
		}
	}
}
