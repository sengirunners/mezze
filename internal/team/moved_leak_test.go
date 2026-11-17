package team_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestAttackWalkCreateDeps(t *testing.T) {
	f := newMovedFixture(t)
	h := raw(f.only, "harbor", "POST", "/v0/issues", `{"title":"withdep","deps":["`+f.oldID+`"]}`)
	m := raw(f.only, "harbor", "POST", "/v0/issues", `{"title":"withdep","deps":["harbor-qqq"]}`)
	t.Logf("create with hidden dep: %d %s", h.code, trunc(h.body))
	t.Logf("create with missing dep: %d %s", m.code, trunc(m.body))
	if h.code != m.code {
		t.Errorf("create deps differ")
	}
	hi := raw(f.only, "harbor", "POST", "/v0/issues", `{"id":"`+f.oldID+`","title":"reuse"}`)
	t.Logf("create naming hidden id: %d %s", hi.code, trunc(hi.body))
	if strings.Contains(hi.body, f.nid) {
		t.Errorf("create names where the hidden issue went: %s", hi.body)
	}
	if hi.code != 409 {
		t.Fatalf("moved id must stay reserved: %d %s", hi.code, hi.body)
	}
	free := raw(f.only, "harbor", "POST", "/v0/issues", `{"id":"harbor-qqq","title":"free"}`)
	if free.code != 200 {
		t.Fatalf("missing id should be usable: %d %s", free.code, free.body)
	}
}

// Edges, history, events, listings: does anything on the old project's side name the new id?
func TestAttackEdgesAndEvents(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	mustf(orchclient.SetupIssues(ctx, ao, "harbor"))(t)
	mustf(orchclient.SetupIssues(ctx, az, "zm"))(t)
	for _, c := range []*client.Client{ao, az} {
		if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "bd", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("add native tracker: %+v %v", res, err)
		}
	}
	only, _ := replica(t, tn, a, "cyd-mac", "nCYD", "harbor", "cyd", "member")
	mv := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "secret-title moves"}))(t)
	blocker := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "blocked by moved", Deps: []string{mv.ID}}))(t)
	dep := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "moved depends on"}))(t)
	mustf(orchclient.AddIssueDep(ctx, ao, mv.ID, orchapi.IssueDepRequest{Target: dep.ID}))(t)
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{mv.ID}}))(t)
	if res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	nid := res.Items[0].NewID
	t.Logf("old %s new %s cross %v", mv.ID, nid, res.Items[0].Cross)
	for _, id := range []string{blocker.ID, dep.ID} {
		for _, p := range []string{"/v0/issues/" + id, "/v0/issues/" + id + "/history", "/v0/issues/" + id + "/comments", "/v0/work/bd:" + id, "/v0/issues?status=all&deps=1", "/v0/work", "/v0/issues/stats"} {
			r := raw(only, "harbor", "GET", p, "")
			if strings.Contains(r.body, nid) || strings.Contains(r.body, "secret-title") {
				t.Errorf("%s names the new id or the moved issue: %s", p, around(r.body, nid, "secret-title"))
			} else {
				t.Logf("clean %d %s %s", r.code, p, trunc(r.body))
			}
		}
	}
	for _, q := range []string{"SELECT * FROM dependencies", "SELECT * FROM events", "SELECT * FROM issues"} {
		r := raw(only, "harbor", "POST", "/v0/issues/sql", fmt.Sprintf(`{"query":%q}`, q))
		t.Logf("sql %s: %d %s", q, r.code, trunc(r.body))
		if strings.Contains(r.body, nid) {
			t.Errorf("sql %q names the new id: %s", q, trunc(r.body))
		}
	}
	// The machine owner can read both projects, so the same edge remains visible.
	for _, r := range []rawRes{
		raw(a, "harbor", "GET", "/v0/issues?status=all&deps=1", ""),
		raw(a, "harbor", "POST", "/v0/issues/sql", `{"query":"SELECT * FROM dependencies"}`),
	} {
		if r.code != 200 || !strings.Contains(r.body, nid) {
			t.Errorf("visible edge missing: %d %s", r.code, r.body)
		}
	}
	// Events, as the replica's reader.
	onlyc := at(only.owner, "harbor", "")
	n := 0
	if err := onlyc.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0"}, func(e api.Event) error {
		n++
		if strings.Contains(string(e.Data), nid) || strings.Contains(e.Subject, nid) {
			t.Errorf("event %s names the new id: %s %s", e.Type, e.Subject, around(string(e.Data), nid))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d events walked", n)
}

// Returning to the old project must clear both the authority and replica markers.
func TestMovedWorkReturnsToOldProject(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	res := mustf(orchclient.MoveIssues(ctx, f.az, orchapi.IssueMoveRequest{To: "harbor", IDs: []string{f.nid}}))(t)
	if res.Moved != 1 || res.Items[0].NewID != f.oldID {
		t.Fatalf("move back: %+v", res)
	}
	waitCopy(t, f.only, "harbor", "bd:"+f.oldID)
	got := mustf(orchclient.GetWork(ctx, at(f.only.owner, "harbor", ""), "bd:"+f.oldID))(t)
	if got.ID != "bd:"+f.oldID {
		t.Fatalf("returned work: %+v", got)
	}
}
