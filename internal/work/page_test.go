package work

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

// GET /v0/work/page: one total order across local items and a tracker's
// mirror, with ties on the sort key, paged by keyset without gaps or
// repeats; scope and in= as GET /v0/work.
func TestPageOrderAcrossSources(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	sec := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.clock.mu.Lock()
	h.clock.t = sec // every local item created at the same instant as the bd ones
	h.clock.mu.Unlock()
	edit(t, state, func(st *fake.State) {
		for i := range 4 {
			id := fmt.Sprintf("solver-%d", i)
			st.Issues[id] = &fake.Issue{ID: id, Title: "bd " + id, Status: []string{"open", "closed"}[i%2], Labels: []string{"x"},
				CreatedAt: sec.Format(time.RFC3339), UpdatedAt: fake.Now()}
		}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	owner := h.as(h.owner, "harbor")
	for i := range 5 {
		area := []string{"solver", "billing", ""}[i%3]
		if _, err := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: fmt.Sprintf("local %d", i), Area: area, Labels: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	h.sync()
	get := func(sub string, q url.Values) orchapi.WorkPage {
		t.Helper()
		c := h.as(h.owner, "")
		if sub == "guest" {
			c = h.as(h.guest(), "")
		}
		var p orchapi.WorkPage
		if err := c.Do(h.ctx, "GET", "/v0/work/page?"+q.Encode(), nil, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	all := get("owner", url.Values{"limit": {"500"}})
	if len(all.Items) != 9 || len(all.Cursors) != 9 || all.Next != "" {
		t.Fatalf("all: %d items, next %q", len(all.Items), all.Next)
	}
	for _, it := range all.Items[:5] {
		if !strings.HasPrefix(it.ID, "local:") {
			t.Fatalf("ties: local ids sort after bd ids, so they come first newest-first: %v", itemIDs(all.Items))
		}
	}
	for _, dir := range []string{"desc", "asc"} {
		for _, n := range []string{"1", "2", "4"} {
			var got []string
			after := ""
			for range 20 {
				p := get("owner", url.Values{"limit": {n}, "after": {after}, "dir": {dir}})
				got = append(got, itemIDs(p.Items)...)
				if p.Next == "" {
					break
				}
				after = p.Next
			}
			want := itemIDs(all.Items)
			if dir == "asc" {
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("dir %s limit %s:\n got  %v\n want %v", dir, n, got, want)
			}
		}
	}
	// Scope: the guest of harbor/solver sees solver's local and bd items only.
	g := get("guest", url.Values{"limit": {"500"}})
	for _, it := range g.Items {
		if it.Area != "solver" {
			t.Errorf("guest sees %s in %q", it.ID, it.Area)
		}
	}
	if len(g.Items) != 6 {
		t.Errorf("guest: %v", itemIDs(g.Items))
	}
	if p := get("guest", url.Values{"in": {"side", "harbor/billing"}}); len(p.Items) != 0 {
		t.Errorf("guest in= hidden: %v", itemIDs(p.Items))
	}
	// Filters, count, labels.
	if p := get("owner", url.Values{"status": {"done"}}); len(p.Items) != 2 {
		t.Errorf("done: %v", itemIDs(p.Items))
	}
	if p := get("owner", url.Values{"in": {h.harbor.ID + "/"}}); len(p.Items) != 1 || p.Items[0].Area != "" {
		t.Errorf("project-level only: %v", itemIDs(p.Items))
	}
	if p := get("owner", url.Values{"count": {"1"}, "in": {"harbor/solver"}}); p.Total == nil || *p.Total != 6 {
		t.Errorf("count: %+v", p.Total)
	}
	if _, err := orchclient.Claim(h.ctx, owner, all.Items[0].ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if p := get("owner", url.Values{"owner": {h.owner.Address}}); len(p.Items) != 1 || p.Items[0].ID != all.Items[0].ID {
		t.Errorf("owner: %v", itemIDs(p.Items))
	}
	// owner= never reveals a holder the caller cannot see (a session in
	// billing holding a solver item is "someone" to the guest).
	hidden := h.session("mini/deadbeef", "harbor", "billing", false, orchapi.StateRunning)
	var solverItem string
	for _, it := range g.Items {
		if strings.HasPrefix(it.ID, "local:") {
			solverItem = it.ID
		}
	}
	if _, err := orchclient.Claim(h.ctx, h.as(hidden, "harbor"), solverItem, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if p := get("guest", url.Values{"owner": {"mini/deadbeef"}}); len(p.Items) != 0 {
		t.Errorf("owner oracle: %v", itemIDs(p.Items))
	}
	if p := get("owner", url.Values{"owner": {"mini/deadbeef"}}); len(p.Items) != 1 {
		t.Errorf("owner sees the holder: %v", itemIDs(p.Items))
	}
	if err := h.as(h.owner, "").Do(h.ctx, "GET", "/v0/work/page?count=1&owner=x", nil, nil); !isCode(err, api.CodeBadRequest) {
		t.Errorf("count+owner: %v", err)
	}
	var ls orchapi.WorkLabels
	if err := h.as(h.owner, "").Do(h.ctx, "GET", "/v0/work/labels", nil, &ls); err != nil || strings.Join(ls.Labels, ",") != "x" {
		t.Errorf("labels %v %v", ls, err)
	}
	if err := h.as(h.owner, "").Do(h.ctx, "GET", "/v0/work/page?after=garbage", nil, nil); !isCode(err, api.CodeBadRequest) {
		t.Errorf("bad cursor: %v", err)
	}
}

func itemIDs(items []orchapi.WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
