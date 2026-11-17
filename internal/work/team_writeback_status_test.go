package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
)

type writeBackNative struct {
	orchcore.IssueService
	reply orchapi.WorkItem
	moved bool
	err   error
}

func (n *writeBackNative) NativeDescribe() orchapi.AdapterDescribe {
	return orchapi.AdapterDescribe{Spec: api.Protocol, Prefix: "issue", Verbs: []string{orchapi.VerbGet, orchapi.VerbRelease, orchapi.VerbChanges}}
}
func (n *writeBackNative) NativeCall(ctx context.Context, _ string, _ orchapi.AdapterRequest, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.err != nil {
		return n.err
	}
	b, err := json.Marshal(n.reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func (n *writeBackNative) MovedOut(context.Context, string, string) (bool, error) {
	return n.moved, nil
}

func writeBackFixture(t *testing.T) (*harness, tracked, *writeBackNative, orchapi.WorkItem) {
	t.Helper()
	h := newHarness(t)
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatal(err)
	}
	tr := api.Tracker{Adapter: orchapi.AdapterNative, Prefix: "bd"}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &[]api.Tracker{tr}}); err != nil {
		t.Fatal(err)
	}
	// Disable only the background sync context: synchronous mirror writes
	// still run normally. No incremental pass can mask a missing write-back event.
	h.s.m.cancel()
	n := &writeBackNative{}
	orchcore.Wire(h.d).Issues = n
	tk := tracked{project: h.harbor.ID, tr: tr, desc: n.NativeDescribe()}
	before := orchapi.WorkItem{ID: "bd:solver-1", Source: "bd", Title: "closed elsewhere", Area: "solver", Status: orchapi.WorkClaimed,
		Owner: "mini/aaaaaaaa", Updated: time.Unix(100, 0).UTC()}
	if err := mirrorUpsert(h.ctx, mustDB(t, h), "bd", []orchapi.WorkItem{before}, 1); err != nil {
		t.Fatal(err)
	}
	return h, tk, n, before
}

func writeBackEvents(t *testing.T, h *harness) []orchapi.WorkItem {
	t.Helper()
	var items []orchapi.WorkItem
	for _, e := range h.events(orchapi.EventWorkStatusChanged) {
		var it orchapi.WorkItem
		if err := json.Unmarshal(e.Data, &it); err != nil {
			t.Fatal(err)
		}
		if e.Subject != it.ID || e.Area != it.Area || it.Links != nil || it.Comments != nil {
			t.Fatalf("unsafe status event %+v", e)
		}
		items = append(items, it)
	}
	return items
}

func TestTeamWriteBackPublishesBeforeIncremental(t *testing.T) {
	h, tk, native, before := writeBackFixture(t)
	native.reply = before
	native.reply.ID, native.reply.Status, native.reply.Updated = "issue:solver-1", orchapi.WorkDone, before.Updated.Add(time.Second)
	h.s.writeOne(h.ctx, wbJob{project: h.harbor.ID, key: before.ID, holder: before.Owner})
	got, ok, err := mirrorGet(h.ctx, mustDB(t, h), h.harbor.ID, "bd", "solver-1")
	if err != nil || !ok || got.Status != orchapi.WorkDone {
		t.Fatalf("authority mirror after release: %+v %v", got, err)
	}
	if ls, err := h.s.queryLeasesAll(h.ctx, "key = ?", before.ID); err != nil || len(ls) != 0 {
		t.Fatalf("closed item's leases %+v %v", ls, err)
	}
	if es := writeBackEvents(t, h); len(es) != 1 || es[0].Status != orchapi.WorkDone {
		t.Fatalf("write-back did not publish done: %+v", es)
	}
	// The following incremental pass sees the silently updated fingerprint.
	// It has no change to announce, so the earlier write-back event is essential.
	if err := h.s.m.write(h.ctx, h.harbor.ID, mustDB(t, h), tk, []orchapi.WorkItem{native.reply}, 1, true); err != nil {
		t.Fatal(err)
	}
	if es := h.events(orchapi.EventWorkChanged); len(es) != 0 {
		t.Fatalf("incremental should have no new state: %+v", es)
	}
	h.s.writeOne(h.ctx, wbJob{project: h.harbor.ID, key: before.ID, holder: before.Owner})
	if es := writeBackEvents(t, h); len(es) != 1 {
		t.Fatalf("unchanged write-back published again: %+v", es)
	}
}

func TestTeamWriteBackDoesNotAnnounceRejectedRows(t *testing.T) {
	for _, kind := range []string{"older", "concurrent newer", "filtered with concurrent row", "stub", "moved", "error"} {
		t.Run(kind, func(t *testing.T) {
			h, _, native, before := writeBackFixture(t)
			native.reply = before
			native.reply.ID, native.reply.Status = "issue:solver-1", orchapi.WorkDone
			native.reply.Updated = before.Updated.Add(time.Second)
			want := before.Status
			switch kind {
			case "older":
				native.reply.Updated = before.Updated.Add(-time.Second)
			case "concurrent newer", "filtered with concurrent row":
				newer := before
				newer.Status, newer.Updated = orchapi.WorkInReview, before.Updated.Add(2*time.Second)
				if kind == "filtered with concurrent row" {
					native.moved = true
					native.reply.Updated = before.Updated.Add(3 * time.Second)
				}
				want = newer.Status
				t.Cleanup(mirrorstate.SetHook(func(_ string, _ []orchapi.WorkItem, _ bool) func() {
					if err := mirrorUpsert(h.ctx, mustDB(t, h), "bd", []orchapi.WorkItem{newer}, 2); err != nil {
						t.Fatal(err)
					}
					return nil
				}))
			case "stub":
				native.reply.Title = ""
			case "moved":
				native.moved = true
			case "error":
				native.err = api.Errorf(api.CodeConflict, "claimed by another holder")
			}
			h.s.writeOne(h.ctx, wbJob{project: h.harbor.ID, key: before.ID, holder: before.Owner})
			got, ok, err := mirrorGet(h.ctx, mustDB(t, h), h.harbor.ID, "bd", "solver-1")
			if err != nil || !ok || got.Status != want {
				t.Fatalf("mirror after refused candidate %+v %v", got, err)
			}
			if es := writeBackEvents(t, h); len(es) != 0 {
				t.Fatalf("rejected response published status: %+v", es)
			}
		})
	}
}

func TestTeamWriteBackReadsCurrentRowAndAreaAtPublish(t *testing.T) {
	h, tk, _, before := writeBackFixture(t)
	reply := before
	reply.Status, reply.Updated, reply.Area = orchapi.WorkDone, time.Now(), "billing"
	// mirror JSON drops time.Now's monotonic reading; acceptance uses time.Equal.
	current := reply
	current.Title, current.Body = "current title", "current body"
	t.Cleanup(mirrorstate.SetHook(func(_ string, _ []orchapi.WorkItem, _ bool) func() {
		return func() {
			if err := mirrorUpsert(h.ctx, mustDB(t, h), "bd", []orchapi.WorkItem{current}, 2); err != nil {
				t.Fatal(err)
			}
		}
	}))
	h.s.mirrorWriteBack(h.ctx, tk, reply, "")
	if es := writeBackEvents(t, h); len(es) != 1 || es[0].Status != current.Status || es[0].Area != "billing" || es[0].Title != current.Title || es[0].Body != current.Body {
		t.Fatalf("published adapter response instead of current row: %+v", es)
	}
	visible, err := h.d.Events.Since(h.ctx, h.guest(), "", events.Filter{Types: []string{orchapi.EventWorkStatusChanged}}, 0)
	if err != nil || len(visible) != 0 {
		t.Fatalf("solver guest saw billing status: %+v %v", visible, err)
	}
	if !h.guest().Scope.Can(authz.Read, authz.Object{Project: h.harbor.ID, Area: "solver"}) {
		t.Fatal("guest fixture cannot read the original area")
	}
}
