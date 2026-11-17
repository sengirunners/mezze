package work

import (
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func rollupNodeOf(r orchapi.WorkRollup, id string) *orchapi.RollupNode {
	for i := range r.Nodes {
		if r.Nodes[i].ID == id {
			return &r.Nodes[i]
		}
	}
	return nil
}

// A rollup over an adapter tracker and local items, and the deadlines a full
// listing brings: announced, though the item's updated time did not move.
func TestRollupBeadsTrackerAndLocal(t *testing.T) {
	quiet(t)
	oldFull := fullEvery
	fullEvery = 0 // every sync is a full listing
	t.Cleanup(func() { fullEvery = oldFull })
	h := newHarness(t)
	state := h.installFake()
	one, two := 1, 2
	edit(t, state, func(st *fake.State) {
		at := fake.Now()
		st.Issues["ep"] = &fake.Issue{ID: "ep", Title: "An epic", Status: "open", IssueType: "epic", Labels: []string{"theme"}, UpdatedAt: at}
		st.Issues["a"] = &fake.Issue{ID: "a", Title: "a", Status: "open", IssueType: "task", Parent: "ep", Priority: &one, UpdatedAt: at}
		st.Issues["b"] = &fake.Issue{ID: "b", Title: "b", Status: "in_progress", IssueType: "task", Parent: "ep", Priority: &two, UpdatedAt: at}
		st.Issues["c"] = &fake.Issue{ID: "c", Title: "c", Status: "closed", IssueType: "task", Parent: "ep", UpdatedAt: at}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	if _, err := orchclient.CreateWork(h.ctx, h.as(h.owner, "harbor"), orchapi.CreateWorkRequest{Title: "a local item"}); err != nil {
		t.Fatal(err)
	}
	read := func() orchapi.WorkRollup {
		t.Helper()
		l, err := orchclient.WorkRollup(h.ctx, owner, "harbor", "")
		if err != nil || len(l.Rollups) != 1 {
			t.Fatalf("%+v %v", l, err)
		}
		return l.Rollups[0]
	}
	r := read()
	n := rollupNodeOf(r, "bd:ep")
	if n == nil || n.Kind != orchapi.RollupTheme || n.Total != 3 || n.Open != 1 || n.Urgent != 1 || n.Working != 1 || n.Done != 1 {
		t.Fatalf("theme %+v in %+v", n, r)
	}
	if r.NoEpic.Total != 1 || r.NoEpic.Open != 1 || r.Totals.Total != 4 {
		t.Fatalf("a local item has no epic: %+v %+v", r.NoEpic, r.Totals)
	}
	if len(h.events(orchapi.EventWorkChanged)) != 0 {
		t.Fatalf("a full listing announced: %+v", h.events(orchapi.EventWorkChanged))
	}

	// bd starts reporting a due date on an item that did not otherwise change.
	due := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	edit(t, state, func(st *fake.State) { st.Issues["a"].DueAt = due })
	h.sync()
	if d := read().Deadlines; len(d) != 1 || d[0].ID != "bd:a" || d[0].DueAt == nil || d[0].Overdue {
		t.Fatalf("deadlines %+v", d)
	}
	evs := h.events(orchapi.EventWorkChanged)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"due_at"`) {
		t.Fatalf("the replica's feed: %+v", evs)
	}
	// The next listing has nothing new to say.
	h.sync()
	if evs := h.events(orchapi.EventWorkChanged); len(evs) != 1 {
		t.Fatalf("announced twice: %+v", evs)
	}

	// A guest of solver, whose area holds none of it, gets nothing of it.
	g := h.as(h.guest(), "")
	l, err := orchclient.WorkRollup(h.ctx, g, "harbor", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range l.Rollups {
		if x.Totals.Total != 0 || len(x.Nodes) != 0 || len(x.Deadlines) != 0 || len(x.Claimed) != 0 {
			t.Fatalf("guest %+v", x)
		}
	}
}

// The Work view's drill: GET /v0/work/page?epic= keeps what an
// open epic owns by the rollup's rule, within the reader's sight, and the
// rollup counts each node's claims and names each claim's epic.
func TestEpicPageAndClaimCounts(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		at := fake.Now()
		put := func(id, typ, parent string, labels ...string) {
			st.Issues[id] = &fake.Issue{ID: id, Title: id, Status: "open", IssueType: typ, Parent: parent, Labels: labels, UpdatedAt: at}
		}
		put("solver-th", "epic", "", "theme")
		put("solver-ep", "epic", "solver-th")
		put("solver-a", "task", "solver-ep")
		put("solver-s", "task", "solver-a")
		put("billing-h", "task", "solver-ep") // the guest's chain stops here
		put("solver-hc", "task", "billing-h")
		put("solver-in", "epic", "solver-ep")
		put("solver-u", "task", "solver-in")
		put("solver-x", "task", "")
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner, guest := h.as(h.owner, ""), h.as(h.guest(), "")
	page := func(c *client.Client, q url.Values) []string {
		t.Helper()
		var p orchapi.WorkPage
		if err := c.Do(h.ctx, "GET", "/v0/work/page?"+q.Encode(), nil, &p); err != nil {
			t.Fatal(err)
		}
		ids := itemIDs(p.Items)
		sort.Strings(ids)
		return ids
	}
	want := "bd:billing-h,bd:solver-a,bd:solver-hc,bd:solver-in,bd:solver-s"
	if got := strings.Join(page(owner, url.Values{"epic": {"bd:solver-ep"}}), ","); got != want {
		t.Fatalf("owner epic page %s", got)
	}
	if got := strings.Join(page(owner, url.Values{"epic": {h.harbor.ID + "/bd:solver-ep"}}), ","); got != want {
		t.Fatalf("project-named epic page %s", got)
	}
	if got := strings.Join(page(guest, url.Values{"epic": {"bd:solver-ep"}}), ","); got != "bd:solver-a,bd:solver-in,bd:solver-s" {
		t.Fatalf("guest epic page %s", got)
	}
	if got := page(owner, url.Values{"epic": {"bd:solver-ep"}, "id": {"bd:solver-a", "bd:solver-u"}}); strings.Join(got, ",") != "bd:solver-a" {
		t.Fatalf("epic and id intersect: %v", got)
	}
	for _, e := range []string{"bd:solver-a", "bd:billing-nope", "bd:solver-u"} {
		if got := page(owner, url.Values{"epic": {e}}); len(got) != 0 {
			t.Fatalf("epic %s is no open epic but pages %v", e, got)
		}
	}
	if got := page(guest, url.Values{"epic": {"bd:solver-th"}}); strings.Join(got, ",") != "bd:solver-ep" {
		t.Fatalf("guest theme page %v", got)
	}
	var p orchapi.WorkPage
	if err := owner.Do(h.ctx, "GET", "/v0/work/page?count=1&epic=bd:solver-ep", nil, &p); err != nil || p.Total == nil || *p.Total != 5 || p.Epic != "bd:solver-ep" {
		t.Fatalf("count %+v %v (echo %q)", p.Total, err, p.Epic)
	}
	// Review of PR 295: narrowed to an area, the page agrees with
	// rollup?area=: a chain through another area (solver-ep > billing-h >
	// solver-hc) stops in both, and solver-hc counts under no_epic.
	inSolver := page(owner, url.Values{"epic": {"bd:solver-ep"}, "in": {"harbor/solver"}})
	if strings.Join(inSolver, ",") != "bd:solver-a,bd:solver-in,bd:solver-s" {
		t.Fatalf("epic page in solver %v", inSolver)
	}
	l0, err := orchclient.WorkRollup(h.ctx, owner, "harbor", "solver")
	if err != nil || len(l0.Rollups) != 1 {
		t.Fatalf("rollup in solver %+v %v", l0, err)
	}
	// ep counts a, s and (through in) u; hc and x count under no_epic.
	if n := rollupNodeOf(l0.Rollups[0], "bd:solver-ep"); n == nil || n.Total != 3 || l0.Rollups[0].NoEpic.Total != 2 {
		t.Fatalf("rollup in solver: ep %+v, no_epic %+v", n, l0.Rollups[0].NoEpic)
	}
	if err := owner.Do(h.ctx, "GET", "/v0/work/page?epic=local:abc", nil, nil); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("a local epic: %v", err)
	}

	// Claims: a running session's is live, a waiting one's blocked, a
	// person's neither.
	sess := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, sess, "bd:solver-s", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, h.as(h.owner, "harbor"), "bd:solver-u", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	waits := h.session("mini/bbbbbbbb", h.harbor.ID, "solver", false, orchapi.StateNeedsHuman)
	if _, err := orchclient.Claim(h.ctx, h.as(waits, ""), "bd:solver-x", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	l, err := orchclient.WorkRollup(h.ctx, owner, "harbor", "")
	if err != nil || len(l.Rollups) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	r := l.Rollups[0]
	for id, c := range map[string]orchapi.RollupClaimCounts{"bd:solver-ep": {Live: 1}, "bd:solver-th": {Live: 1}, "bd:solver-in": {}} {
		if n := rollupNodeOf(r, id); n == nil || n.Claims != c {
			t.Errorf("%s claims %+v, want %+v", id, n, c)
		}
	}
	got := map[string]string{}
	for _, c := range r.Claimed {
		got[c.ID] = c.Epic + " " + c.State
	}
	if got["bd:solver-s"] != "bd:solver-ep live" || got["bd:solver-u"] != "bd:solver-in " || got["bd:solver-x"] != " blocked" {
		t.Fatalf("claimed %v", got)
	}
}
