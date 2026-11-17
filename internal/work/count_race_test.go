package work

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// countRaceNative answers changes with count, after during runs (an item
// made once the tracker counted).
type countRaceNative struct {
	orchcore.IssueService
	count   int
	deleted []string
	during  func()
}

func (n *countRaceNative) NativeDescribe() orchapi.AdapterDescribe {
	return orchapi.AdapterDescribe{Spec: api.Protocol, Prefix: "issue", Verbs: []string{orchapi.VerbGet, orchapi.VerbChanges}}
}

func (n *countRaceNative) NativeCall(_ context.Context, _ string, req orchapi.AdapterRequest, out any) error {
	var args orchapi.AdapterChangesArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return err
	}
	if n.during != nil {
		n.during()
	}
	b, _ := json.Marshal(orchapi.AdapterChanges{Items: []orchapi.WorkItem{}, Deleted: n.deleted, Count: &n.count, Cursor: args.Since})
	return json.Unmarshal(b, out)
}

func (n *countRaceNative) MovedOut(context.Context, string, string) (bool, error) { return false, nil }

// An item made after the tracker counted (mirrorPut writes it into the
// mirror at once) is not a deletion: no full listing; nor is
// one the feed reported and the sync removed. A mirror above the count
// before the call is one.
func TestIncrementalCountRaceIsNoDeletion(t *testing.T) {
	for _, kind := range []string{"create after count", "reported deletion", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			tr := api.Tracker{Adapter: orchapi.AdapterNative, Prefix: "bd"}
			if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &[]api.Tracker{tr}}); err != nil {
				t.Fatal(err)
			}
			h.s.m.cancel() // no background sync: this test runs one by hand
			db := mustDB(t, h)
			item := func(ref string) orchapi.WorkItem {
				return orchapi.WorkItem{ID: "bd:" + ref, Source: "bd", Title: ref, Area: "solver", Status: orchapi.WorkOpen, Updated: time.Unix(100, 0).UTC()}
			}
			if err := mirrorUpsert(h.ctx, db, "bd", []orchapi.WorkItem{item("solver-1"), item("solver-2")}, 1); err != nil {
				t.Fatal(err)
			}
			n := &countRaceNative{count: 2}
			if kind != "deleted" {
				n.during = func() {
					if err := mirrorUpsert(h.ctx, db, "bd", []orchapi.WorkItem{item("solver-3")}, 1); err != nil {
						t.Error(err)
					}
				}
			}
			switch kind {
			case "deleted":
				n.count = 1
			case "reported deletion": // and a create after the count
				n.count, n.deleted = 1, []string{"bd:solver-2"}
			}
			orchcore.Wire(h.d).Issues = n
			tk := tracked{project: h.harbor.ID, tr: tr, desc: n.NativeDescribe()}
			sy := &syncer{m: h.s.m, project: h.harbor.ID, tr: tr}
			sy.cond = sync.NewCond(&sy.mu)
			sy.row.cursor, sy.row.gen = "01M6S2VZ0V3SE4SCT4706F79MV", 1
			if err := sy.incremental(h.ctx, db, tk); err != nil {
				t.Fatal(err)
			}
			if want := kind == "deleted"; sy.again != want {
				t.Fatalf("full listing asked = %v, want %v", sy.again, want)
			}
		})
	}
}
