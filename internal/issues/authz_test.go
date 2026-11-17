package issues_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/server"
)

type authzFixture struct {
	t     *testing.T
	dm    *server.Daemon
	owner *client.Client
	tok   map[string]string
}

func newAuthzFixture(t *testing.T) *authzFixture {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	owner := client.WithHandler(dm.SocketHandler())
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	// billing follows a label, so a label moves issues into it.
	for _, a := range []api.Area{{Name: "solver"}, {Name: "billing", Labels: []string{"billing"}}} {
		if _, err := owner.AddArea(ctx, "harbor", a); err != nil {
			t.Fatal(err)
		}
	}
	f := &authzFixture{t: t, dm: dm, owner: owner, tok: map[string]string{}}
	for name, req := range map[string]api.MintTokenRequest{
		"guest":  {As: "vida", Project: "harbor", Area: "solver", Role: "guest"},
		"member": {As: "mina", Project: "harbor", Area: "solver", Role: "member"},
	} {
		tok, err := owner.MintToken(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		f.tok[name] = tok.Secret
	}
	return f
}

func (f *authzFixture) ownerAt(project string) *client.Client {
	c := client.WithHandler(f.dm.SocketHandler())
	c.Project, c.ProjectFrom = project, "flag"
	return c
}

// as calls the API over the tailnet path as a token holder.
func (f *authzFixture) as(who, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+f.tok[who])
	req.Header.Set(api.HeaderProject, "harbor/solver")
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: who + "@example.com"}))
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestIssueAuthz(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	solver, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor/solver"), orchapi.NewIssueRequest{Title: "solver work"})
	if err != nil {
		t.Fatal(err)
	}
	billing, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor"), orchapi.NewIssueRequest{Title: "SECRET billing", Labels: []string{"billing"}})
	if err != nil {
		t.Fatal(err)
	}
	if solver.Area != "solver" || billing.Area != "billing" {
		t.Fatalf("areas %q %q", solver.Area, billing.Area)
	}
	if _, err := orchclient.AddIssueDep(ctx, f.ownerAt("harbor"), solver.ID, orchapi.IssueDepRequest{Target: billing.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CommentIssue(ctx, f.ownerAt("harbor"), solver.ID, orchapi.IssueCommentRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}

	for _, who := range []string{"guest", "member"} {
		// Reads: the solver issue, never the billing one, its edge or its count.
		code, body := f.as(who, "GET", "/v0/issues?status=all", "")
		if code != 200 || !strings.Contains(body, solver.ID) || strings.Contains(body, billing.ID) || strings.Contains(body, "SECRET") {
			t.Fatalf("%s list: %d %s", who, code, body)
		}
		code, body = f.as(who, "GET", "/v0/issues/"+solver.ID, "")
		if code != 200 || strings.Contains(body, billing.ID) || !strings.Contains(body, `"dependency_count":0`) || !strings.Contains(body, `"blocked":true`) {
			t.Fatalf("%s get: %d %s", who, code, body)
		}
		code, body = f.as(who, "GET", "/v0/issues/"+solver.ID+"/history", "")
		if code != 200 || strings.Contains(body, billing.ID) {
			t.Fatalf("%s history: %d %s", who, code, body)
		}
		// Hidden answers exactly like missing, on every route.
		for _, rt := range []struct{ m, p, b string }{{"GET", "", ""}, {"GET", "/comments", ""}, {"GET", "/history", ""},
			{"PATCH", "", `{"title":"x"}`}, {"POST", "/claim", "{}"}, {"POST", "/close", "{}"}, {"POST", "/comments", `{"text":"x"}`}} {
			hc, hb := f.as(who, rt.m, "/v0/issues/"+billing.ID+rt.p, rt.b)
			mc, mb := f.as(who, rt.m, "/v0/issues/harbor-zzzzz"+rt.p, rt.b)
			if hc != 404 || hc != mc || strings.ReplaceAll(hb, billing.ID, "X") != strings.ReplaceAll(mb, "harbor-zzzzz", "X") {
				t.Fatalf("%s %s %s: hidden %d %s, missing %d %s", who, rt.m, rt.p, hc, hb, mc, mb)
			}
		}
		code, body = f.as(who, "GET", "/v0/issues/stats", "")
		var st orchapi.IssueStats
		if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.TotalIssues != 1 {
			t.Fatalf("%s stats: %s", who, body)
		}
	}

	// Guests are read-only.
	for _, rt := range []struct{ m, p, b string }{{"POST", "/v0/issues", `{"title":"g"}`}, {"PATCH", "/v0/issues/" + solver.ID, `{"title":"g"}`},
		{"POST", "/v0/issues/" + solver.ID + "/claim", "{}"}, {"POST", "/v0/issues/" + solver.ID + "/comments", `{"text":"g"}`}} {
		if code, body := f.as("guest", rt.m, rt.p, rt.b); code != 403 {
			t.Fatalf("guest %s %s: %d %s", rt.m, rt.p, code, body)
		}
	}
	// A project that sets guest_issues lets its guests write issues where they
	// may write (their area), never elsewhere and never project memories.
	on, off := true, false
	if _, err := f.ownerAt("harbor").UpdateProject(ctx, "harbor", api.UpdateProjectRequest{GuestIssues: &on}); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ m, p, b string }{{"POST", "/v0/issues", `{"title":"guest's"}`}, {"PATCH", "/v0/issues/" + solver.ID, `{"title":"solver work, retitled"}`},
		{"POST", "/v0/issues/" + solver.ID + "/comments", `{"text":"from the guest"}`}, {"POST", "/v0/issues/" + solver.ID + "/claim", "{}"}} {
		if code, body := f.as("guest", rt.m, rt.p, rt.b); code != 200 {
			t.Fatalf("guest_issues: guest %s %s: %d %s", rt.m, rt.p, code, body)
		}
	}
	if code, body := f.as("guest", "POST", "/v0/issues", `{"title":"g","area":"billing"}`); code == 200 {
		t.Fatalf("guest_issues: guest wrote in another area: %s", body)
	}
	if code, body := f.as("guest", "POST", "/v0/issues/"+billing.ID+"/comments", `{"text":"g"}`); code != 404 {
		t.Fatalf("guest_issues: guest comment on a hidden issue: %d %s", code, body)
	}
	if code, body := f.as("guest", "PUT", "/v0/issues/memories/k", `{"value":"v"}`); code == 200 {
		t.Fatalf("guest_issues: guest wrote a project memory: %s", body)
	}
	if _, err := f.ownerAt("harbor").UpdateProject(ctx, "harbor", api.UpdateProjectRequest{GuestIssues: &off}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.as("guest", "POST", "/v0/issues/"+solver.ID+"/comments", `{"text":"g"}`); code != 403 {
		t.Fatalf("guest_issues off: guest comment: %d %s", code, body)
	}
	// Members write in their area, as themselves.
	code, body := f.as("member", "POST", "/v0/issues", `{"title":"member's"}`)
	if code != 200 || !strings.Contains(body, `"area":"solver"`) || !strings.Contains(body, `"created_by":"mina"`) {
		t.Fatalf("member create: %d %s", code, body)
	}
	code, body = f.as("member", "POST", "/v0/issues/"+solver.ID+"/comments", `{"text":"from mina"}`)
	if code != 200 || !strings.Contains(body, `"author":"mina"`) {
		t.Fatalf("member comment: %d %s", code, body)
	}
	// ... but cannot move an issue into an area they cannot write, or depend on a hidden one.
	if code, body := f.as("member", "POST", "/v0/issues/"+solver.ID+"/labels", `{"label":"billing"}`); code != 403 {
		t.Fatalf("member label move: %d %s", code, body)
	}
	if code, body := f.as("member", "POST", "/v0/issues/"+solver.ID+"/deps", `{"target":"`+billing.ID+`","type":"related"}`); code != 404 || strings.Contains(body, "SECRET") {
		t.Fatalf("member dep on hidden: %d %s", code, body)
	}
	if code, body := f.as("member", "POST", "/v0/issues", `{"title":"x","parent":"`+billing.ID+`"}`); code != 404 {
		t.Fatalf("member child of hidden: %d %s", code, body)
	}
	// Closing names only the blockers the caller may see.
	if code, body := f.as("member", "POST", "/v0/issues/"+solver.ID+"/close", `{}`); code != 409 || strings.Contains(body, billing.ID) || !strings.Contains(body, "1 you cannot see") {
		t.Fatalf("member close blocked by hidden: %d %s", code, body)
	}
	// The owner moving an issue out of solver tells solver readers it is gone.
	if _, err := orchclient.AddIssueLabel(ctx, f.ownerAt("harbor"), solver.ID, "billing"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.as("member", "GET", "/v0/issues/"+solver.ID, ""); code != 404 {
		t.Fatalf("moved issue still visible: %d", code)
	}
	evs, err := f.dm.Deps.Events.Since(ctx, f.dm.Deps.System(), "", events.Filter{Types: []string{"dev.zm.issue."}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var moved, created, leaked bool
	for _, e := range evs {
		switch {
		case e.Type == orchapi.EventIssueMoved && e.Area == "solver" && strings.Contains(string(e.Data), `"gone":true`):
			moved = true
		case e.Type == orchapi.EventIssueCreated:
			created = true
		}
		if e.Area == "solver" && strings.Contains(string(e.Data), billing.ID) {
			leaked = true
		}
	}
	if !moved || !created || leaked {
		t.Fatalf("events: moved %v created %v leaked %v", moved, created, leaked)
	}
}
