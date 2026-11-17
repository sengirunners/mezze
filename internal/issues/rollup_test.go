package issues_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

// GET /v0/work/rollup over native issues, as each reader of one project: the
// owner, an area member and an area guest. What another area holds is gone.
func TestWorkRollupScope(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	oc := f.ownerAt("harbor")
	// The tracker's area makes it visible to a reader of that area, as for GET
	// /v0/work; each row's own area then decides.
	if res, err := orchclient.AddTracker(ctx, oc, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", Area: "solver", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	solver, billing := f.ownerAt("harbor/solver"), f.ownerAt("harbor/billing")
	mk := func(c *client.Client, req orchapi.NewIssueRequest) orchapi.Issue {
		t.Helper()
		is, err := orchclient.CreateIssue(ctx, c, req)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	due := time.Now().Add(24 * time.Hour)
	theme := mk(solver, orchapi.NewIssueRequest{Title: "shown theme", IssueType: "epic", Labels: []string{"theme"}})
	mk(solver, orchapi.NewIssueRequest{Title: "shown item", Parent: theme.ID, DueAt: &due})
	hidden := mk(billing, orchapi.NewIssueRequest{Title: "secret theme", IssueType: "epic", Labels: []string{"theme"}})
	mk(billing, orchapi.NewIssueRequest{Title: "secret item", Parent: hidden.ID, DueAt: &due})
	// A visible item under a hidden epic counts, with no name for the epic.
	mk(solver, orchapi.NewIssueRequest{Title: "shown under secret", Parent: hidden.ID})

	read := func(who string) (orchapi.WorkRollup, string) {
		t.Helper()
		code, body := f.as(who, http.MethodGet, "/v0/work/rollup?project=harbor", "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", who, code, body)
		}
		var l orchapi.WorkRollupList
		if err := json.Unmarshal([]byte(body), &l); err != nil || len(l.Rollups) != 1 {
			t.Fatalf("%s: %s %v", who, body, err)
		}
		return l.Rollups[0], body
	}
	var whole orchapi.WorkRollupList
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		whole, err = orchclient.WorkRollup(ctx, oc, "harbor", "")
		if err == nil && len(whole.Rollups) == 1 && whole.Rollups[0].Totals.Total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner: %+v %v", whole, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r := whole.Rollups[0]; len(r.Nodes) != 2 || len(r.Deadlines) != 2 || r.Slug != "harbor" {
		t.Fatalf("owner sees it all: %+v", r)
	}
	for _, who := range []string{"guest", "member"} {
		r, body := read(who)
		if strings.Contains(body, hidden.ID) || strings.Contains(body, "secret") {
			t.Fatalf("%s sees the other area: %s", who, body)
		}
		if len(r.Nodes) != 1 || r.Nodes[0].ID != "issue:"+theme.ID || r.Nodes[0].Total != 1 {
			t.Fatalf("%s nodes %+v", who, r.Nodes)
		}
		if r.Totals.Total != 2 || r.NoEpic.Total != 1 || len(r.Deadlines) != 1 {
			t.Fatalf("%s %s", who, body)
		}
	}
	// area narrows further; a hidden one yields nothing.
	narrowed, err := orchclient.WorkRollup(ctx, oc, "harbor", "billing")
	if err != nil || narrowed.Rollups[0].Totals.Total != 1 {
		t.Fatalf("area: %+v %v", narrowed, err)
	}
	code, body := f.as("guest", http.MethodGet, "/v0/work/rollup?project=harbor&area=billing", "")
	if code != http.StatusOK || strings.Contains(body, "secret") || strings.Contains(body, hidden.ID) {
		t.Fatalf("guest asks for the other area: %d %s", code, body)
	}
}
