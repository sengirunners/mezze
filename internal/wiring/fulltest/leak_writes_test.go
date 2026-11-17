package fulltest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// hiddenWrite is one write whose BODY names something the guest cannot see.
// The hidden and the missing variant must answer identically (status and
// body, with the differing values normalized away): "hidden == missing".
type hiddenWrite struct {
	name            string
	method, path    string
	project         string // Zm-Project header
	missProject     string // Zm-Project header of the missing variant (default: project)
	missPath        string // path of the missing variant (default: path)
	hidden, missing obj
	hiddenV, missV  []string // values to normalize in each response
	statusOnly      bool     // bodies legitimately differ (e.g. timestamps); compare status only
}

// checkHiddenWrites calls every body-bearing write that can name a hidden
// project, area, recipient or link target. Rows whose prerequisites were not
// seeded (module still a stub) still run with a stand-in value: both variants
// then answer 501 and the row starts biting the day the module lands.
func (f *leakFixture) checkHiddenWrites(t *testing.T) {
	t.Helper()
	or := func(v, standIn string) string {
		if v != "" {
			return v
		}
		return standIn
	}
	hiddenSess := or(f.sessions["harbor"], "mini/zzhidden")
	hiddenItem := or(f.items["harbor/zz-billing"], "local:zzhid1")
	sideItem := or(f.items["zz-side"], "local:zzhid2")
	solverItem := or(f.items["harbor/solver"], "local:zzvis1")
	missSess, missItem := "mini/zzmissin", "local:zzmis1"
	// An ask's mail in the guest's own area is restricted (owners, needs=human):
	// replying to or acking it answers exactly like a missing mail id, not
	// with the open-ask refusal a party gets (cockpit gap G2).
	solverAsk := or(f.asks["harbor/solver"], "01JZZZZZZZZZZZZZZZZZZZZZZY")
	sideAsk := or(f.asks["zz-side"], "01JZZZZZZZZZZZZZZZZZZZZZZX")
	missAsk := "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	rows := []hiddenWrite{
		{name: "reply to an ask's mail in the guest's area", method: "POST", path: "/v0/mail/" + solverAsk + "/reply", project: "harbor",
			missPath: "/v0/mail/" + missAsk + "/reply", hidden: obj{"body": "x"}, missing: obj{"body": "x"},
			hiddenV: []string{solverAsk}, missV: []string{missAsk}},
		{name: "ack an ask's mail in the guest's area", method: "POST", path: "/v0/mail/" + solverAsk + "/ack", project: "harbor",
			missPath: "/v0/mail/" + missAsk + "/ack", hidden: obj{}, missing: obj{},
			hiddenV: []string{solverAsk}, missV: []string{missAsk}},
		{name: "reply to another project's ask mail", method: "POST", path: "/v0/mail/" + sideAsk + "/reply", project: "harbor",
			missPath: "/v0/mail/" + missAsk + "/reply", hidden: obj{"body": "x"}, missing: obj{"body": "x"},
			hiddenV: []string{sideAsk}, missV: []string{missAsk}},
		{name: "mail to a hidden session", method: "POST", path: "/v0/mail", project: "harbor",
			hidden: obj{"to": hiddenSess, "body": "x", "area": "solver"}, missing: obj{"to": missSess, "body": "x", "area": "solver"},
			hiddenV: []string{hiddenSess}, missV: []string{missSess}},
		{name: "mail to a hidden work item", method: "POST", path: "/v0/mail", project: "harbor",
			hidden: obj{"to": hiddenItem, "body": "x", "area": "solver"}, missing: obj{"to": missItem, "body": "x", "area": "solver"},
			hiddenV: []string{hiddenItem}, missV: []string{missItem}},
		{name: "mail to another project's item", method: "POST", path: "/v0/mail", project: "harbor",
			hidden: obj{"to": sideItem, "body": "x", "area": "solver"}, missing: obj{"to": missItem, "body": "x", "area": "solver"},
			hiddenV: []string{sideItem}, missV: []string{missItem}},
		{name: "mail with a hidden ref", method: "POST", path: "/v0/mail", project: "harbor",
			hidden:  obj{"to": "human:ivo", "body": "x", "area": "solver", "ref": hiddenItem},
			missing: obj{"to": "human:ivo", "body": "x", "area": "solver", "ref": missItem},
			hiddenV: []string{hiddenItem}, missV: []string{missItem}},
		{name: "mail into a hidden area", method: "POST", path: "/v0/mail", project: "harbor",
			hidden: obj{"to": "human:ivo", "body": "x", "area": "zz-billing"}, missing: obj{"to": "human:ivo", "body": "x", "area": "nope-area"},
			hiddenV: []string{"zz-billing"}, missV: []string{"nope-area"}},
		{name: "work in a hidden area", method: "POST", path: "/v0/work", project: "harbor",
			hidden: obj{"title": "x", "area": "zz-billing"}, missing: obj{"title": "x", "area": "nope-area"},
			hiddenV: []string{"zz-billing"}, missV: []string{"nope-area"}},
		{name: "work in a hidden project", method: "POST", path: "/v0/work", project: "zz-side",
			hidden: obj{"title": "x"}, missing: obj{"title": "x"},
			hiddenV: []string{"zz-side"}, missV: []string{"zz-side"}},
		{name: "link to a hidden item", method: "POST", path: "/v0/work/" + url.PathEscape(solverItem) + "/link", project: "harbor",
			hidden: obj{"rel": "related", "target": hiddenItem}, missing: obj{"rel": "related", "target": missItem},
			hiddenV: []string{hiddenItem}, missV: []string{missItem}},
		{name: "link to a hidden session", method: "POST", path: "/v0/work/" + url.PathEscape(solverItem) + "/link", project: "harbor",
			hidden: obj{"rel": "session", "target": hiddenSess}, missing: obj{"rel": "session", "target": missSess},
			hiddenV: []string{hiddenSess}, missV: []string{missSess}},
		{name: "lease in a hidden area", method: "POST", path: "/v0/leases", project: "harbor/zz-billing", missProject: "harbor/nope-area",
			hidden: obj{"key": "lock:x"}, missing: obj{"key": "lock:x"},
			hiddenV: []string{"zz-billing"}, missV: []string{"nope-area"}},
		// Lease keys are unique per (project, area, key): a hidden area's lock
		// of the same name neither conflicts nor shows. Both variants succeed.
		{name: "lease on a key a hidden area holds", method: "POST", path: "/v0/leases", project: "harbor",
			hidden: obj{"key": "lock:HIDDEN-billing"}, missing: obj{"key": "lock:zz-free"},
			hiddenV: []string{"lock:HIDDEN-billing"}, missV: []string{"lock:zz-free"}, statusOnly: true},
		{name: "grant on a hidden project", method: "POST", path: "/v0/grants",
			hidden: obj{"identity": "vida", "role": "guest", "project": "zz-side"}, missing: obj{"identity": "vida", "role": "guest", "project": "nope-zz"},
			hiddenV: []string{"zz-side"}, missV: []string{"nope-zz"}},
		{name: "grant on a hidden area", method: "POST", path: "/v0/grants",
			hidden:  obj{"identity": "vida", "role": "guest", "project": "harbor", "area": "zz-billing"},
			missing: obj{"identity": "vida", "role": "guest", "project": "harbor", "area": "nope-area"},
			hiddenV: []string{"zz-billing"}, missV: []string{"nope-area"}},
		{name: "revoke on a hidden project", method: "DELETE", path: "/v0/grants",
			hidden: obj{"identity": "vida", "role": "guest", "project": "zz-side"}, missing: obj{"identity": "vida", "role": "guest", "project": "nope-zz"},
			hiddenV: []string{"zz-side"}, missV: []string{"nope-zz"}},
		{name: "area on a hidden project", method: "POST", path: "/v0/projects/zz-side/areas",
			hidden: obj{"name": "x"}, missing: obj{"name": "x"}, hiddenV: []string{"zz-side"}},
		{name: "area set on a hidden area", method: "PATCH", path: "/v0/projects/harbor/areas/zz-billing", missPath: "/v0/projects/harbor/areas/nope-area",
			hidden: obj{"labels": []string{"solver"}}, missing: obj{"labels": []string{"solver"}},
			hiddenV: []string{"zz-billing"}, missV: []string{"nope-area"}},
		{name: "area set on a hidden project", method: "PATCH", path: "/v0/projects/zz-side/areas/x", missPath: "/v0/projects/nope-zz/areas/x",
			hidden: obj{"work": []string{"*"}}, missing: obj{"work": []string{"*"}},
			hiddenV: []string{"zz-side"}, missV: []string{"nope-zz"}},
	}
	for _, tr := range transports {
		for _, w := range rows {
			hb, _ := json.Marshal(w.hidden)
			mb, _ := json.Marshal(w.missing)
			mpath := w.path
			if w.missPath != "" {
				mpath = w.missPath
			}
			if w.name == "area on a hidden project" {
				mpath = "/v0/projects/nope-zz/areas"
				w.missV = []string{"nope-zz"}
			}
			mproj := w.project
			if w.missProject != "" {
				mproj = w.missProject
			}
			if w.project == "zz-side" {
				mproj = "nope-zz"
				w.missV = []string{"nope-zz"}
			}
			hid := f.callBody(w.method, w.path, w.project, tr, string(hb))
			mis := f.callBody(w.method, mpath, mproj, tr, string(mb))
			what := tr.String() + " " + w.name
			f.checkNoLeak(t, what, leakResp{hid.status, normalize(hid, w.hiddenV...)})
			f.checkNoLeak(t, what+" (missing)", mis)
			if hid.status != mis.status || (!w.statusOnly && normalize(hid, w.hiddenV...) != normalize(mis, w.missV...)) {
				t.Errorf("%s: hidden answered %d %q, missing answered %d %q", what, hid.status, clip(hid.body), mis.status, clip(mis.body))
			}
		}
	}
}

// checkLiveEvents subscribes to the live stream as the guest over a plain
// TCP listener (no transport marks: token auth only) and checks the
// per-subscriber filter end to end: hidden-project, project-level, hidden
// area and machine events never arrive; solver events arrive with hidden
// session addresses replaced by "someone".
func (f *leakFixture) checkLiveEvents(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(f.dm.Handler)
	defer srv.Close()
	c := client.NewRemote(srv.URL, nil)
	c.Token = f.guestTok
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := make(chan api.Event, 256)
	done := make(chan error, 1)
	go func() {
		done <- c.Events(ctx, api.EventQuery{}, func(e api.Event) error {
			got <- e
			if e.Type == "test.leak.end" {
				return events.ErrStop
			}
			return nil
		})
	}()
	bus := f.dm.Deps.Events
	pub := func(typ, project, area string, data any, mentions ...string) {
		if _, err := bus.Publish(context.Background(), events.Input{Type: typ, Subject: typ, Project: project, Area: area, Data: data, Mentions: mentions}); err != nil {
			t.Fatal(err)
		}
	}
	// Publish a visible marker until the subscription is live.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
ready:
	for {
		select {
		case e := <-got:
			if e.Type == "test.leak.ready" {
				break ready
			}
		case <-tick.C:
			pub("test.leak.ready", f.harbor, "solver", "VISIBLE-ready")
		case err := <-done:
			t.Fatalf("guest event stream ended: %v", err)
		case <-ctx.Done():
			t.Fatal("guest never received a solver event")
		}
	}
	tick.Stop()
	hiddenSess := f.sessions["harbor"]
	if hiddenSess == "" {
		hiddenSess = "mini/zzhidden"
		f.forbid(hiddenSess, "a hidden session's address (stand-in)")
	}
	pub("test.leak.hidden", f.side, "", obj{"m": "HIDDEN-side live"})
	pub("test.leak.hidden", f.harbor, "", obj{"m": "HIDDEN-projectlevel live"})
	pub("test.leak.hidden", f.harbor, "zz-billing", obj{"m": "HIDDEN-billing live"})
	pub("test.leak.hidden", "", "", obj{"m": "HIDDEN-machine live"})
	pub("test.leak.mention", f.harbor, "solver", obj{"holder": hiddenSess, "note": "held by " + hiddenSess}, hiddenSess)
	pub("test.leak.end", f.harbor, "solver", "VISIBLE-end")

	var seen []api.Event
	for {
		select {
		case e := <-got:
			if e.Type == "test.leak.ready" {
				continue
			}
			seen = append(seen, e)
		case err := <-done:
			for len(got) > 0 {
				if e := <-got; e.Type != "test.leak.ready" {
					seen = append(seen, e)
				}
			}
			if err != nil {
				t.Fatalf("guest stream: %v", err)
			}
			if len(seen) != 2 || seen[0].Type != "test.leak.mention" || seen[1].Type != "test.leak.end" {
				var types []string
				for _, e := range seen {
					types = append(types, e.Type+"@"+e.Area)
				}
				t.Fatalf("guest live stream delivered %v, want [mention, end]", types)
			}
			for _, e := range seen {
				js, _ := json.Marshal(e)
				f.checkNoLeak(t, "live SSE "+e.Type, leakResp{200, string(js)})
			}
			if !strings.Contains(string(seen[0].Data), events.Someone) || strings.Contains(string(seen[0].Data), hiddenSess) {
				t.Fatalf("hidden holder not redacted on the live stream: %s", seen[0].Data)
			}
			return
		case <-ctx.Done():
			t.Fatalf("guest live stream timed out after %d events", len(seen))
		}
	}
}

// Any listener that does not mark its connections local is the network: no
// token, no WhoIs -> 401 on everything but health. This is what protects
// the daemon if a future listener forgets core.WithRemote.
func TestUnmarkedListenerFailsClosed(t *testing.T) {
	f := newLeakFixture(t)
	srv := httptest.NewServer(f.dm.Handler) // plain TCP, no ConnContext, no WithRemote
	defer srv.Close()
	for _, rt := range f.dm.Router.Routes() {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		path = paramRe.ReplaceAllString(path, "x")
		req, _ := http.NewRequest(method, srv.URL+path+"?live=0", strings.NewReader("{}"))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set(api.HeaderAs, "human:ivo") // claiming the owner changes nothing
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := http.StatusUnauthorized
		if rt.Access == "public" && rt.Pattern != "POST /v0/invites/redeem" && rt.Pattern != "POST /v0/github/hook" {
			want = http.StatusOK // redeem is public but answers only a WhoIs-identified tailnet node;
			// the GitHub receiver answers only a delivery signed with a link's secret
		}
		if rt.Pattern == "POST /v0/join/github" {
			want = http.StatusUnauthorized // a GitHub join answers only a WhoIs-identified tailnet node
		}
		if rt.Pattern == "POST /v0/login/github/{id}" {
			want = http.StatusNotFound // only a live login's own poll secret (none here) collects anything
		}
		switch rt.Pattern {
		case "POST /v0/runners/hook":
			want = http.StatusUnauthorized // the runner broker answers only a delivery signed with the App's secret
		case "GET /v0/runners/oauth", "POST /v0/runners/oauth", "GET /v0/runners/app/new", "GET /v0/runners/app/created",
			"GET /v0/runners/app/installed":
			want = http.StatusNotFound // only a live single-use state (none here) does anything
		case "POST /v0/door/hook/{token}":
			want = http.StatusNotFound // only a live task's URL token (none here) queues anything
		}
		if resp.StatusCode != want {
			t.Errorf("%s over an unmarked listener: %d, want %d", rt.Pattern, resp.StatusCode, want)
		}
	}
	// The real socket server marks only Unix connections:
	// potluck/kit/server's TestMarkUnixMarksOnlyUnix.
}

// An owner from the network (an owner-role token, on any transport, or a
// tailnet login) administers a project but never sets what the daemon runs
// or reads on this machine: tracker adapters/config (bin, -C dir, --db) and
// repo paths (the --sandbox policy). Slug and name stay theirs to change.
func TestRemoteOwnerCannotSetMachineSettings(t *testing.T) {
	f := newLeakFixture(t)
	ctx := context.Background()
	owner := client.WithHandler(f.dm.SocketHandler())
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "zz-remote-owner", Project: "zz-side", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	do := func(via transport, method, path, body string) leakResp {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set(api.HeaderAuthorization, "Bearer "+tok.Secret)
		switch via {
		case viaTailnet:
			req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: "o@example.com", Node: "o-laptop"}))
		case viaSocket:
			req = req.WithContext(core.WithLocal(req.Context()))
		}
		rec := httptest.NewRecorder()
		f.dm.Handler.ServeHTTP(rec, req)
		return leakResp{status: rec.Code, body: rec.Body.String()}
	}
	refused := []struct{ name, method, path, body string }{
		{"tracker with a chosen bin and dir", "PATCH", "/v0/projects/zz-side",
			`{"trackers":[{"adapter":"beads","prefix":"bd","config":{"bin":"/usr/bin/true","dir":"/etc"}}]}`},
		{"empty tracker list", "PATCH", "/v0/projects/zz-side", `{"trackers":[]}`},
		{"repo paths", "PATCH", "/v0/projects/zz-side", `{"repos":{"paths":["/"]}}`},
		{"area with repo paths", "POST", "/v0/projects/zz-side/areas", `{"name":"x","paths":["/etc"]}`},
		{"area with repo remotes", "POST", "/v0/projects/zz-side/areas", `{"name":"y","remotes":["github.com/a/b"]}`},
	}
	for _, tr := range transports {
		if tr == viaTunnel {
			continue // a minted token is 401 there (TestGoldenLeak)
		}
		for _, c := range refused {
			r := do(tr, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || !strings.Contains(r.body, "machine settings") {
				t.Errorf("%s %s: %d %s, want 403 machine settings", tr, c.name, r.status, clip(r.body))
			}
		}
	}
	p, err := owner.GetProject(ctx, "zz-side")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Trackers) != 0 || len(p.Paths) != 0 || len(p.Areas) != 0 {
		t.Fatalf("a refused write changed the project: %+v", p)
	}
	// Admin that touches nothing on the machine still works from the network.
	if r := do(viaTailnet, "PATCH", "/v0/projects/zz-side", `{"name":"Renamed"}`); r.status != http.StatusOK {
		t.Fatalf("remote owner rename: %d %s", r.status, r.body)
	}
	if r := do(viaTailnet, "POST", "/v0/projects/zz-side/areas", `{"name":"docs","work":["bd:docs-*"]}`); r.status != http.StatusOK {
		t.Fatalf("remote owner area without repo rules: %d %s", r.status, r.body)
	}
	for _, tr := range transports {
		if tr == viaTunnel {
			continue
		}
		for _, body := range []string{`{"paths":["/etc"]}`, `{"remotes":["github.com/a/b"]}`} {
			if r := do(tr, "PATCH", "/v0/projects/zz-side/areas/docs", body); r.status != http.StatusForbidden || !strings.Contains(r.body, "machine settings") {
				t.Errorf("%s area set %s: %d %s, want 403 machine settings", tr, body, r.status, clip(r.body))
			}
		}
	}
	if r := do(viaTailnet, "PATCH", "/v0/projects/zz-side/areas/docs", `{"labels":["docs"],"work":[]}`); r.status != http.StatusOK ||
		!strings.Contains(r.body, `"labels":["docs"]`) || strings.Contains(r.body, "bd:docs-*") {
		t.Fatalf("remote owner area set labels: %d %s", r.status, r.body)
	}
	// The machine's owner on its socket still sets trackers.
	trk := []api.Tracker{{Adapter: "beads", Prefix: "bd"}}
	if _, err := owner.UpdateProject(ctx, "zz-side", api.UpdateProjectRequest{Trackers: &trk}); err != nil {
		t.Fatal(err)
	}
}
