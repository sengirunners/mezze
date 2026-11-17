package fulltest

// Automation records (SPEC 18.6) in the golden leak fixture: one in every
// place, written by the fixture machine's own daemon as the home, so the
// walk sends every hidden record id through every route (hidden equals
// missing; TestGoldenLeakTeams walks them through a member too). And the
// home rule end to end with a home on another machine, a member replica:
// only that machine's own daemon, outside any agent session, naming the
// install that holds the home's claim, writes its records.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// seedAutomations claims the fixture machine (mini) as a home in harbor and
// zz-side and writes a record in every place; the hidden ones are walked.
func seedAutomations(t *testing.T, f *leakFixture) {
	t.Helper()
	ctx := context.Background()
	places := []struct {
		project, area, marker string
		visible               bool
	}{
		{"harbor/solver", "solver", "VISIBLE-solver", true},
		{"harbor/zz-billing", "zz-billing", "HIDDEN-billing", false},
		{"harbor", "", "HIDDEN-projectlevel", false},
		{"zz-side", "", "HIDDEN-side", false},
	}
	for _, p := range []string{"harbor", "zz-side"} {
		c := f.ownerAt(p, "")
		seedStep(t, f, "claim home mini in "+p, func() (obj, error) {
			var out obj
			err := c.Do(ctx, http.MethodPut, "/v0/automation-homes/mini?project="+p, obj{"install": "z3-install-mini", "store": "zm", "path": "/nonexistent/HIDDEN-z3-userdata"}, &out)
			return out, err
		})
	}
	for _, p := range places {
		slug, _, _ := strings.Cut(p.project, "/")
		c := f.ownerAt(p.project, "")
		got, ok := seedStep(t, f, "automation record in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/automations", obj{"project": slug, "area": p.area, "home": "mini", "install": "z3-install-mini",
				"name": p.marker + " automation", "body": p.marker + " prompt", "owner": p.marker + "-owner",
				"fields": obj{"on": obj{"manual": true}, "note": p.marker + " field"}})
		})
		if !ok {
			continue
		}
		id := got.str("id")
		if p.visible {
			f.visible[id] = true
			continue
		}
		f.hidden = append(f.hidden, id)
		f.forbid(id, "a hidden automation record's id")
		f.forbid(got.str("task_id"), "a hidden automation record's task id")
	}
}

// A home on another machine (a member replica): its own daemon writes its
// records through its socket, forwarded to the authority as its node-bound
// token; nobody else does.
func TestAutomationHomeRuleThroughMember(t *testing.T) {
	ctx := context.Background()
	f := newLeakFixture(t)
	tn := tailnettest.New()
	useLoopNode(t, f.dm, tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	owner := client.WithHandler(f.dm.SocketHandler())
	if _, err := owner.ProjectTeam(ctx, "harbor"); err != nil {
		t.Fatal(err)
	}
	_, bea, _, _ := newReplica(t, f, tn, "bea-mac", "bea", "nBEA", api.InviteRequest{Project: "harbor", As: "bea", Role: "member"})
	beaH := as(bea, "harbor", "")
	refused := func(what string, err error, code, has string) {
		t.Helper()
		if !api.IsCode(err, code) || !strings.Contains(err.Error(), has) {
			t.Fatalf("%s: %v (want %s with %q)", what, err, code, has)
		}
	}

	// The member's Mezze app claims its home and creates a record in a hidden area.
	var home orchapi.AutomationHome
	if err := beaH.Do(ctx, http.MethodPut, "/v0/automation-homes/bea-mac?project=harbor", orchapi.AutomationHomeClaim{Install: "i-bea", Store: "zm", Path: "/srv/z3/bea-z3"}, &home); err != nil || home.Install != "i-bea" {
		t.Fatalf("member's claim: %+v %v", home, err)
	}
	// The Mezze app's client names the project in the body only: the member's proxy still routes it.
	var rec orchapi.Automation
	if err := bea.Do(ctx, http.MethodPost, "/v0/automations", orchapi.AutomationCreate{Project: "harbor", Area: "zz-billing", Home: "bea-mac", Install: "i-bea",
		Name: "nightly", Body: "Summarize the night.", Fields: []byte(`{"on":{"schedule":"0 3 * * *"}}`)}, &rec); err != nil || rec.Rev != 1 || rec.UpdatedBy != "machine:bea-mac" {
		t.Fatalf("member's create: %+v %v", rec, err)
	}
	// A body-only create in a project of the member's own stays here with its body whole.
	if _, err := bea.CreateProject(ctx, api.NewProjectRequest{Slug: "bea-own"}); err != nil {
		t.Fatal(err)
	}
	if err := as(bea, "bea-own", "").Do(ctx, http.MethodPut, "/v0/automation-homes/bea-mac?project=bea-own", orchapi.AutomationHomeClaim{Install: "i-bea", Store: "zm"}, nil); err != nil {
		t.Fatalf("claim in the member's own project: %v", err)
	}
	var own orchapi.Automation
	if err := bea.Do(ctx, http.MethodPost, "/v0/automations", orchapi.AutomationCreate{Project: "bea-own", Home: "bea-mac", Install: "i-bea",
		Name: "local", Body: "Tidy up.", Fields: []byte(`{"on":{"schedule":"0 4 * * *"}}`)}, &own); err != nil || own.Rev != 1 || own.Project != "bea-own" {
		t.Fatalf("member's create in its own project: %+v %v", own, err)
	}
	path := "/v0/automations/" + rec.ID
	patch := orchapi.AutomationPatch{Rev: 1, Install: "i-bea", Body: ptr("Summarize the night, briefly.")}

	// Not the home: the authority's own socket owner.
	refused("the authority's socket owner", owner.Do(ctx, http.MethodPatch, path, patch, nil), api.CodeForbidden, "written only by bea-mac's own daemon")
	// A minted token, even a member's of the whole project.
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "zz-tok", Project: "harbor", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	req := func(method, target string, body string) (int, string) {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set(api.HeaderProtocol, api.Protocol)
		r.Header.Set(api.HeaderAuthorization, "Bearer "+tok.Secret)
		r = r.WithContext(core.WithRemote(r.Context(), core.Remote{Login: "zz-tok@example.com", Node: "zz-laptop"}))
		w := httptest.NewRecorder()
		f.dm.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := req(http.MethodPatch, path, `{"rev":1,"install":"i-bea","name":"x"}`); code != http.StatusForbidden || !strings.Contains(body, "home machine's own daemon") {
		t.Fatalf("a minted token: %d %s", code, body)
	}
	// An agent session on the home itself: refused on the member, before any forward.
	ss, err := orchclient.Register(ctx, as(bea, "harbor", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "beaagent1-uuid", Title: "an agent"})
	if err != nil {
		t.Fatal(err)
	}
	refused("an agent session on the home", as(bea, "harbor", ss.Address).Do(ctx, http.MethodPatch, path, patch, nil), api.CodeForbidden, "inside agent session")
	// The home, but not the install that holds the claim.
	wrong := patch
	wrong.Install = "i-copy"
	refused("a wrong install", bea.Do(ctx, http.MethodPatch, path, wrong, nil), api.CodeConflict, "does not hold home bea-mac")
	// A second install's claim is 409, recorded as the home's wanted install.
	refused("a second install's claim", beaH.Do(ctx, http.MethodPut, "/v0/automation-homes/bea-mac?project=harbor", orchapi.AutomationHomeClaim{Install: "i-copy", Store: "zm", Path: "/srv/z3/bea-copy"}, nil),
		api.CodeConflict, "recorded as the home's wanted install")
	var homes orchapi.AutomationHomes
	if err := owner.Do(ctx, http.MethodGet, "/v0/automation-homes?project=harbor", nil, &homes); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range homes.Homes {
		if h.Home == "bea-mac" {
			found = h.Install == "i-bea" && h.Wanted != nil && h.Wanted.Install == "i-copy" && h.Wanted.Path == "/srv/z3/bea-copy"
		}
	}
	if !found {
		t.Fatalf("homes %+v", homes)
	}
	// None of those changed it; the home's own write does.
	var got orchapi.Automation
	if err := owner.Do(ctx, http.MethodGet, path, nil, &got); err != nil || got.Rev != 1 {
		t.Fatalf("after the refusals: %+v %v", got, err)
	}
	if err := bea.Do(ctx, http.MethodPatch, path, patch, &got); err != nil || got.Rev != 2 || got.Body != *patch.Body {
		t.Fatalf("the home's patch: %+v %v", got, err)
	}
	// And it reads its record and history through the member.
	var h orchapi.AutomationHistory
	if err := bea.Do(ctx, http.MethodGet, path+"/history", nil, &h); err != nil || len(h.Revisions) != 2 {
		t.Fatalf("history through the member: %+v %v", h, err)
	}
}

func ptr[T any](v T) *T { return &v }
