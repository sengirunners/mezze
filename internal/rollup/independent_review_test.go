package rollup

import (
	"context"
	"testing"
	"time"
)

type reviewSource struct{}

func (reviewSource) Groups(context.Context) ([]Group, error) {
	return []Group{{Source: "bd", Parent: "a", Type: "task", Status: "open", Count: 1}, {Source: "bd", Parent: "b", Type: "task", Status: "open", Count: 1}}, nil
}
func (reviewSource) Marked(context.Context) ([]Item, error) {
	return []Item{{ID: "bd:a", Source: "bd", Ref: "a", Parent: "b", Type: "epic", Status: "open"}, {ID: "bd:b", Source: "bd", Ref: "b", Parent: "a", Type: "epic", Status: "open"}}, nil
}
func (reviewSource) Parents(context.Context, string, []string) (map[string]string, error) {
	return nil, nil
}
func (reviewSource) Items(context.Context, []string) ([]Item, error) { return nil, nil }

func TestIndependentReviewCyclicEpicsHaveReachableRoots(t *testing.T) {
	r, err := Build(context.Background(), reviewSource{}, "P", "p", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := 0
	for _, n := range r.Nodes {
		if n.Parent == "" {
			roots++
		}
	}
	if roots == 0 && len(r.Nodes) > 0 {
		t.Fatalf("all %d items disappear from a root-based UI: %+v", r.Totals.Total, r.Nodes)
	}
}
