package rollup

import (
	"context"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

type cycleSource struct {
	reviewSource
	marked []Item
	groups []Group
	up     map[string]string
}

func (s cycleSource) Groups(context.Context) ([]Group, error) { return s.groups, nil }
func (s cycleSource) Marked(context.Context) ([]Item, error)  { return s.marked, nil }
func (s cycleSource) Parents(context.Context, string, []string) (map[string]string, error) {
	return s.up, nil
}

func TestCyclesFormStableForestWithoutLosingItems(t *testing.T) {
	s := cycleSource{up: map[string]string{"bridge": "b", "closed": "a"}}
	// A three-epic cycle through a task and a closed epic, with a branch
	// entering the cycle, plus a separate two-cycle and a self-cycle.
	for _, p := range [][2]string{{"a", "bridge"}, {"b", "c"}, {"c", "closed"}, {"branch", "b"}, {"x", "y"}, {"y", "x"}, {"self", "self"}} {
		s.marked = append(s.marked, Item{ID: "bd:" + p[0], Source: "bd", Ref: p[0], Parent: p[1], Type: EpicType, Status: "open", Labels: []string{ThemeLabel}})
		s.groups = append(s.groups, Group{Source: "bd", Parent: p[0], Type: "task", Status: "open", Priority: 1, Count: 1})
	}
	s.marked = append(s.marked, Item{ID: "bd:closed", Source: "bd", Ref: "closed", Parent: "a", Type: EpicType, Status: "done"})
	s.groups = append(s.groups, Group{Source: "bd", Parent: "orphan", Type: "task", Status: "done", Count: 1})
	var want orchapi.WorkRollup
	for run := 0; run < 100; run++ {
		rand.Shuffle(len(s.marked), func(i, j int) { s.marked[i], s.marked[j] = s.marked[j], s.marked[i] })
		rand.Shuffle(len(s.groups), func(i, j int) { s.groups[i], s.groups[j] = s.groups[j], s.groups[i] })
		got, err := Build(context.Background(), s, "P", "p", nil, time.Unix(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			want = got
		} else if !reflect.DeepEqual(want, got) {
			t.Fatalf("unstable forest: %+v vs %+v", want, got)
		}
		by := map[string]orchapi.RollupNode{}
		for _, n := range got.Nodes {
			by[n.ID] = n
		}
		seen := map[string]bool{}
		var visit func(string) int
		visit = func(id string) int {
			if seen[id] {
				t.Fatalf("cycle or repeated child %s", id)
			}
			seen[id] = true
			n, ok := by[id]
			if !ok {
				t.Fatalf("missing node %s", id)
			}
			total := 1 // one directly owned task per open epic
			for _, kid := range n.Children {
				if by[kid].Parent != id {
					t.Fatalf("inconsistent parent for %s", kid)
				}
				total += visit(kid)
			}
			if n.Total != total || n.Open != total || n.Urgent != total {
				t.Fatalf("lost or duplicated counts for %s: %+v want %d", id, n, total)
			}
			return total
		}
		total := got.NoEpic.Total
		for _, n := range got.Nodes {
			if n.Parent == "" {
				total += visit(n.ID)
			}
		}
		if len(seen) != len(by) || total != got.Totals.Total || total != 8 || got.NoEpic.Total != 1 {
			t.Fatalf("unreachable/lost items: %+v seen %v", got, seen)
		}
		for _, id := range []string{"bd:a", "bd:x", "bd:self"} {
			if by[id].Parent != "" || by[id].Kind != orchapi.RollupTheme {
				t.Fatalf("noncanonical root %s: %+v", id, by[id])
			}
		}
	}
}
