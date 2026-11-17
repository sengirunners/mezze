package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
)

// quiet makes the background sync wait for explicit kicks (h.sync), so a
// test decides when the tracker is asked and can count its calls.
func quiet(t testing.TB) {
	old, oldL := syncEvery, listEvery
	syncEvery, listEvery = time.Hour, time.Hour
	t.Cleanup(func() { syncEvery, listEvery = old, oldL })
}

func trackerCalls(t *testing.T, state string) []string {
	t.Helper()
	st, err := fake.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	return st.Calls
}

func edit(t testing.TB, state string, fn func(st *fake.State)) {
	t.Helper()
	if err := fake.Update(state, fn); err != nil {
		t.Fatal(err)
	}
}

// Creates, updates, closes and deletes in the tracker reach the mirror;
// areas follow labels (and area rules changed later); reads never call
// the tracker; zm's own writes show at once.
func TestMirrorPropagation(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "Fix solver timeout", Status: "open", UpdatedAt: fake.Now()}
		st.Issues["harbor-l1"] = &fake.Issue{ID: "harbor-l1", Title: "labelled", Status: "open", Labels: []string{"ux"}, UpdatedAt: fake.Now()}
	})
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "ux", Labels: []string{"ux"}}); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	g := h.as(h.guest(), "") // harbor/solver

	// Reads after the sync never call the tracker.
	before := len(trackerCalls(t, state))
	if l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{}); err != nil || len(l.Items) != 2 || len(l.Behind) != 0 {
		t.Fatalf("owner list %+v %v", l, err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-l1"); err != nil || it.Area != "ux" {
		t.Fatalf("label area %+v %v", it, err)
	}
	if _, err := orchclient.Holder(h.ctx, owner, "bd:solver-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.MailTarget(h.ctx, h.owner, "harbor", "bd:harbor-l1"); !isCode(err, api.CodeConflict) {
		t.Fatalf("mail target: %v", err)
	}
	if srcs, err := orchclient.WorkSources(h.ctx, owner, "harbor"); err != nil || !srcs[1].OK || srcs[1].Sync == nil || srcs[1].Sync.Items != 2 ||
		srcs[1].Sync.Mode != "changes" || !srcs[1].Sync.Serve {
		t.Fatalf("sources %+v %v", srcs, err)
	}
	if gl, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{}); err != nil || len(gl.Items) != 1 || gl.Items[0].ID != "bd:solver-1" {
		t.Fatalf("guest list %+v %v", gl, err)
	}
	if n := len(trackerCalls(t, state)); n != before {
		t.Fatalf("reads called the tracker %d times: %v", n-before, trackerCalls(t, state)[before:])
	}

	// Created, changed and closed in the tracker.
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-2"] = &fake.Issue{ID: "solver-2", Title: "New in the tracker", Status: "open", UpdatedAt: fake.Now()}
		st.Issues["solver-1"].Title, st.Issues["solver-1"].UpdatedAt = "Fix solver timeout (renamed)", fake.Now()
		st.Issues["harbor-l1"].Status, st.Issues["harbor-l1"].UpdatedAt = "closed", fake.Now()
		st.Issues["harbor-l1"].Labels = []string{"ux", "solver-ish"}
	})
	h.sync()
	// What changed in the tracker is published (live views), scoped by area.
	changed := map[string]bool{}
	for _, ev := range h.events(orchapi.EventWorkChanged) {
		changed[ev.Subject] = true
	}
	if len(changed) != 3 || !changed["bd:solver-2"] || !changed["bd:solver-1"] || !changed["bd:harbor-l1"] {
		t.Fatalf("changed events %v", changed)
	}
	gsub := h.guest()
	gevs, err := h.d.Events.Since(h.ctx, gsub, "", events.Filter{Types: []string{orchapi.EventWorkChanged}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range gevs {
		if ev.Subject == "bd:harbor-l1" {
			t.Fatalf("guest got another area's change: %+v", ev)
		}
	}
	if it, err := orchclient.GetWork(h.ctx, g, "bd:solver-2"); err != nil || it.Title != "New in the tracker" {
		t.Fatalf("created %+v %v", it, err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:solver-1"); err != nil || !strings.HasSuffix(it.Title, "(renamed)") {
		t.Fatalf("updated %+v %v", it, err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-l1"); err != nil || it.Status != orchapi.WorkDone {
		t.Fatalf("closed %+v %v", it, err)
	}
	if l, _ := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd"}); len(l.Items) != 2 {
		t.Fatalf("a closed item in the default list %+v", l.Items)
	}
	if l, _ := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd", Status: orchapi.WorkDone}); len(l.Items) != 1 {
		t.Fatalf("--status done %+v", l.Items)
	}

	// A label moves an item into the guest's area, and out again.
	edit(t, state, func(st *fake.State) {
		st.Issues["harbor-l1"].Labels, st.Issues["harbor-l1"].Status, st.Issues["harbor-l1"].UpdatedAt = []string{"solver"}, "open", fake.Now()
	})
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "triage", Labels: []string{"solver"}}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "rae", Project: "harbor", Area: "triage", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	tg := h.as(ts, "")
	if it, err := orchclient.GetWork(h.ctx, tg, "bd:harbor-l1"); err != nil || it.Area != "triage" {
		t.Fatalf("labelled into triage %+v %v", it, err)
	}
	edit(t, state, func(st *fake.State) {
		st.Issues["harbor-l1"].Labels, st.Issues["harbor-l1"].UpdatedAt = nil, fake.Now()
	})
	h.sync()
	_, errH := orchclient.GetWork(h.ctx, tg, "bd:harbor-l1")
	_, errM := orchclient.GetWork(h.ctx, tg, "bd:harbor-nope")
	if !isCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), "bd:harbor-l1", "X") != strings.ReplaceAll(errM.Error(), "bd:harbor-nope", "X") {
		t.Fatalf("label removed: hidden %v vs missing %v", errH, errM)
	}
	// An area rule changed in zm (not in the tracker) re-places rows at once.
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "late", Work: []string{"bd:harbor-l*"}}); err != nil {
		t.Fatal(err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-l1"); err != nil || it.Area != "late" {
		t.Fatalf("area rules changed %+v %v", it, err)
	}

	// zm's own write shows at once (no wait for a sync).
	sess := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, sess, "bd:solver-2", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd", Status: orchapi.WorkClaimed}); err != nil || len(l.Items) != 1 ||
		l.Items[0].Owner != "mini/aaaaaaaa" {
		t.Fatalf("claimed %+v %v", l, err)
	}
	if hr, err := orchclient.Holder(h.ctx, owner, "bd:solver-2"); err != nil || hr.MailTo != "mini/aaaaaaaa" {
		t.Fatalf("holder after claim %+v %v", hr, err)
	}

	// Deleted in the tracker: the count gives it away; the next full listing drops it.
	edit(t, state, func(st *fake.State) { delete(st.Issues, "solver-1") })
	h.sync() // incremental: count 3 vs 4 mirrored
	h.sync() // the full listing
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:solver-1"); !isCode(err, api.CodeNotFound) {
		st, loadErr := fake.Load(state)
		present := st != nil && st.Issues["solver-1"] != nil
		t.Fatalf("deleted item still served: %v (fake authority item present=%t, state read=%v)", err, present, loadErr)
	}
	if l, _ := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd", Status: orchapi.WorkOpen}); len(l.Items) != 1 {
		t.Fatalf("after delete %+v", l.Items)
	}
}

// A failing tracker is reported, never hidden: before its first sync
// (skipped, unavailable), and after one (items as last seen, stale, with
// the error); a guest gets generic words and nothing about hidden items.
func TestMirrorSyncFailure(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "Fix solver timeout", Status: "open", UpdatedAt: fake.Now()}
		st.Issues["billing-1"] = &fake.Issue{ID: "billing-1", Title: "HIDDEN", Status: "open", UpdatedAt: fake.Now()}
	})
	// A flag file makes the tracker fail like an unreachable server.
	flag := filepath.Join(t.TempDir(), "down")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"down": flag}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	g := h.as(h.guest(), "")

	// Never synced: skipped, and --source is an error; show says unavailable.
	l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{})
	if err != nil || len(l.Skipped) != 1 || !strings.Contains(l.Skipped[0].Error, "unreachable") {
		t.Fatalf("never synced %+v %v", l, err)
	}
	if _, err := orchclient.Work(h.ctx, owner, orchapi.WorkQuery{Source: "bd"}); !isCode(err, api.CodeUnavailable) {
		t.Fatalf("--source: %v", err)
	}
	_, errH := orchclient.GetWork(h.ctx, g, "bd:billing-1")
	_, errM := orchclient.GetWork(h.ctx, g, "bd:solver-9")
	if !isCode(errH, api.CodeUnavailable) || errH.Error() != errM.Error() || strings.Contains(errH.Error(), "10.0.0.9") {
		t.Fatalf("guest before the first sync: hidden %v vs missing %v", errH, errM)
	}
	if srcs, _ := orchclient.WorkSources(h.ctx, g, ""); len(srcs) != 2 || srcs[1].OK || !strings.Contains(srcs[1].Error, "the fake tracker is unavailable") ||
		srcs[1].Sync.Items != 0 || srcs[1].Path != "" || srcs[1].Sync.Mode != "" {
		t.Fatalf("guest sources %+v", srcs)
	}

	// Up: synced. Down again: stale, served as last seen, and said so.
	os.Remove(flag)
	h.sync()
	if l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{}); err != nil || len(l.Items) != 2 || len(l.Skipped)+len(l.Behind) != 0 {
		t.Fatalf("synced %+v %v", l, err)
	}
	os.WriteFile(flag, nil, 0o600)
	h.sync()
	l, err = orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{})
	if err != nil || len(l.Items) != 2 || len(l.Behind) != 1 || l.Behind[0].State != orchapi.SyncStale ||
		!strings.Contains(l.Behind[0].Error, "unreachable") || l.Behind[0].Synced.IsZero() {
		t.Fatalf("stale owner list %+v %v", l, err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:solver-1"); err != nil || !strings.Contains(it.Stale, "unreachable") {
		t.Fatalf("stale item %+v %v", it, err)
	}
	gl, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{})
	if err != nil || len(gl.Items) != 1 || len(gl.Behind) != 1 || strings.Contains(gl.Behind[0].Error, "10.0.0.9") ||
		!strings.Contains(gl.Behind[0].Error, "the fake tracker is unavailable") {
		t.Fatalf("stale guest list %+v %v", gl, err)
	}
	if srcs, _ := orchclient.WorkSources(h.ctx, owner, "harbor"); srcs[1].OK || srcs[1].Sync.State != orchapi.SyncStale || srcs[1].Sync.Items != 2 {
		t.Fatalf("stale sources %+v", srcs[1])
	}
	os.Remove(flag)
	h.sync()
	if srcs, _ := orchclient.WorkSources(h.ctx, owner, "harbor"); !srcs[1].OK || srcs[1].Error != "" {
		t.Fatalf("recovered sources %+v", srcs[1])
	}
	// Recovery with unchanged items still invalidates the app's stale banner.
	deadline := time.Now().Add(3 * time.Second)
	for len(h.events(orchapi.EventWorkRefreshed)) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	refreshed := h.events(orchapi.EventWorkRefreshed)
	if len(refreshed) < 4 {
		t.Fatalf("missing loading/ok/stale/recovered freshness hints: %d", len(refreshed))
	}
	for _, event := range refreshed {
		if event.Subject != "" || string(event.Data) != `{"project":"`+h.harbor.ID+`"}` {
			t.Fatalf("freshness hint disclosed tracker content: %+v", event)
		}
	}
}

// Reads before the first sync completes say so (loading), never block on
// the tracker, and answer a guest's hidden and missing ids alike.
func TestMirrorLoading(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	h.installFake()
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"hang": "1"}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	owner := h.as(h.owner, "")
	g := h.as(h.guest(), "")
	start := time.Now()
	l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{})
	if err != nil || len(l.Behind) != 1 || l.Behind[0].State != orchapi.SyncLoading || len(l.Skipped) != 0 {
		t.Fatalf("loading list %+v %v", l, err)
	}
	_, errH := orchclient.GetWork(h.ctx, g, "bd:billing-1")
	_, errM := orchclient.GetWork(h.ctx, g, "bd:solver-9")
	if !isCode(errM, api.CodeUnavailable) || errH.Error() != errM.Error() {
		t.Fatalf("loading: hidden %v vs missing %v", errH, errM)
	}
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:x-1"); !isCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "still loading") {
		t.Fatalf("owner show while loading: %v", err)
	}
	if srcs, _ := orchclient.WorkSources(h.ctx, owner, "harbor"); srcs[1].OK || srcs[1].Sync.State != orchapi.SyncLoading {
		t.Fatalf("loading sources %+v", srcs[1])
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("reads waited on a hung tracker: %s", d)
	}
}

func TestReviewTrackerImportAndClaim(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "a\u202eb\U000e0061", Status: "open", UpdatedAt: fake.Now()}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	c := h.as(h.owner, "harbor")
	it, err := orchclient.GetWork(h.ctx, c, "bd:solver-1")
	if err != nil || it.Title != "ab" {
		t.Fatalf("get %+v %v", it, err)
	}
	list, err := orchclient.WorkList(h.ctx, c, orchapi.WorkQuery{})
	if err != nil || len(list.Items) != 1 || list.Items[0].Title != "ab" {
		t.Fatalf("list %+v %v", list, err)
	}
	claim, err := orchclient.Claim(h.ctx, c, it.ID, orchapi.ClaimRequest{Note: "hi\u202e"})
	if err != nil || claim.Lease.Note != "hi" {
		t.Fatalf("claim %+v %v", claim, err)
	}
}
