package fulltest

// The golden leak test (SPEC "Leaks"). It is owned by the integrator and is
// the structural guarantee behind "a guest sees only its area": every route
// any module registers is called as a guest of harbor/solver, against a
// fixture with two projects and two areas, and no response may contain
// anything from outside that area. Out-of-scope ids must be answered exactly
// like ids that do not exist.
//
// Module routes join automatically. Seeding steps that answer 501 are
// skipped; once a module implements them they MUST succeed (fix the fixture
// here, with the integrator, if your module's contract needs other inputs).
//
// The fixture writes only through raw JSON on the HTTP contract (API.md),
// never through module-owned client methods, so a builder changing a Go
// signature cannot quietly turn a seeding step off.
//
// Guests call every route over four transports: tailnet (WithRemote +
// token), an unmarked network listener (token only: the fail-closed default),
// the socket with a token (WithLocal + token), and the tunnel listener's door
// (SPEC 17.11: real signed HTTP; a minted token is refused there, so the team
// test walks it with a key-bound guest token).

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sengirunners/mezze/internal/wiring"
	. "github.com/sengirunners/potluck/kit/server"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/tunnel"
)

type leakFixture struct {
	dm        *Daemon
	guestTok  string
	harbor    string            // harbor's ULID
	side      string            // zz-side's ULID
	sessions  map[string]string // place -> session address
	items     map[string]string // place -> work item id
	asks      map[string]string // place -> ask id (= its mail's id); all hidden from the guest
	forbidden map[string]string // string -> why
	visible   map[string]bool
	hidden    []string // ids/addresses/keys of out-of-scope objects, used as path params
	seeded    []string

	// The tunnel listener (SPEC 17.11): its door on a loopback server, a
	// client signing with a guest daemon's key, and the token it presents
	// there (key-bound once a test redeems one; else the minted guest's).
	tunURL    string
	tunHC     *http.Client
	tunnelTok string
	tunKey    *tunnel.Service
}

func (f *leakFixture) forbid(s, why string) {
	if s != "" && !f.visible[s] {
		f.forbidden[s] = why
	}
}

// ownerAt is an owner client on the socket that writes into project[/area]
// (and acts as a session when as != "").
func (f *leakFixture) ownerAt(project, as string) *client.Client {
	c := client.WithHandler(f.dm.SocketHandler())
	c.Project, c.ProjectFrom, c.As = project, "flag", as
	return c
}

// obj is a decoded JSON object from the HTTP contract.
type obj map[string]any

func (o obj) str(k string) string { s, _ := o[k].(string); return s }

// post is the fixture's only write path: raw JSON per API.md.
func post(ctx context.Context, c *client.Client, path string, body obj) (obj, error) {
	var out obj
	err := c.Do(ctx, http.MethodPost, path, body, &out)
	return out, err
}

func seedStep(t *testing.T, f *leakFixture, what string, fn func() (obj, error)) (obj, bool) {
	t.Helper()
	v, err := fn()
	if api.IsCode(err, api.CodeNotImplemented) {
		return v, false
	}
	if err != nil {
		t.Errorf("leak fixture: %s failed: %v (fix the fixture in leak_test.go or the module)", what, err)
		return v, false
	}
	f.seeded = append(f.seeded, what)
	return v, true
}

func newLeakFixture(t *testing.T) *leakFixture {
	t.Helper()
	ctx := context.Background()
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	gh := githubfake.New(t) // the owners routes ask GitHub; never the live one
	cfg.File.GitHub = config.GitHub{APIURL: gh.URL, WebURL: gh.URL}
	// A machine with a task driver (SPEC 8 "Where it runs"): its numbers carry
	// sandbox_tasks and sandbox_slots, which only whole-project readers see;
	// the driver's path is never answered at all.
	cfg.File.RemoteTasks, cfg.File.SandboxTaskCmd, cfg.File.SandboxTaskSlots = true, "/nonexistent/HIDDEN-sandbox-driver", 7
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	f := &leakFixture{dm: dm, forbidden: map[string]string{}, visible: map[string]bool{}, sessions: map[string]string{}, items: map[string]string{}, asks: map[string]string{}}
	freezeProbe(dm)
	liftNameLookups(dm)
	f.startTunnel(t)
	owner := client.WithHandler(dm.SocketHandler())

	harbor, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Paths: []string{"/nonexistent/harbor"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "zz-billing"} {
		if _, err := owner.AddArea(ctx, "harbor", api.Area{Name: a, Work: []string{"bd:" + a + "-*"}}); err != nil {
			t.Fatal(err)
		}
	}
	side, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zz-side", Name: "ZZ Side"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	f.guestTok = tok.Secret
	other, err := owner.MintToken(ctx, api.MintTokenRequest{As: "zz-bea", Project: "zz-side", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	f.forbid(side.ID, "the hidden project's ULID")
	f.forbid("zz-side", "the hidden project's slug")
	f.forbid("ZZ Side", "the hidden project's name")
	f.forbid("zz-billing", "a hidden area's name")
	f.forbid("/nonexistent/harbor", "project-level repo rules")
	f.forbid("zz-bea", "an identity with no grant in solver")
	f.forbid(other.Token.ID, "another identity's token")
	f.forbid(other.Secret, "another token's secret")
	f.forbid("ivo@", "never an email")
	f.forbid("HIDDEN", "a marker in hidden objects")
	f.forbid(`"sandbox_tasks"`, "the machine's sandbox numbers (members of the whole project only)")
	f.forbid(`"sandbox_slots"`, "the machine's sandbox numbers (members of the whole project only)")
	f.hidden = append(f.hidden, side.ID, "zz-side", other.Token.ID)
	f.harbor, f.side = harbor.ID, side.ID
	// The walk has sandbox numbers to look for: the owner sees them.
	seedStep(t, f, "machine numbers with sandbox slots", func() (obj, error) {
		var l api.MachineList
		if err := owner.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil {
			return nil, err
		}
		if len(l.Machines) == 0 || !l.Machines[0].Stats.SandboxTasks || l.Machines[0].Stats.SandboxSlots != 7 {
			return nil, fmt.Errorf("the owner's machines carry no sandbox numbers: %+v", l.Machines)
		}
		return obj{}, nil
	})

	type place struct {
		project, area, marker string
		visible               bool
	}
	places := []place{
		{"harbor/solver", "solver", "VISIBLE-solver", true},
		{"harbor/zz-billing", "zz-billing", "HIDDEN-billing", false},
		{"harbor", "", "HIDDEN-projectlevel", false},
		{"zz-side", "", "HIDDEN-side", false},
	}
	// Sessions first, so mail and leases can involve them.
	sessions, items := f.sessions, f.items
	for i, p := range places {
		c := f.ownerAt(p.project, "")
		s, ok := seedStep(t, f, "register session in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/sessions/register", obj{"agent": "claude", "runtime_id": strings.Repeat(string(rune('a'+i)), 8) + "-uuid", "title": p.marker})
		})
		if ok {
			addr, id := s.str("address"), s.str("id")
			sessions[p.project] = addr
			if p.visible {
				f.visible[addr], f.visible[id] = true, true
			} else {
				f.hidden = append(f.hidden, addr, id)
				f.forbid(addr, "a hidden session's address")
			}
		}
	}
	for _, p := range places {
		c := f.ownerAt(p.project, "")
		w, ok := seedStep(t, f, "create work in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/work", obj{"title": p.marker + " item", "area": p.area})
		})
		if ok {
			id := w.str("id")
			items[p.project] = id
			if p.visible {
				f.visible[id] = true
			} else {
				f.hidden = append(f.hidden, id)
			}
		}
		m, ok := seedStep(t, f, "send mail in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/mail", obj{"to": "human:ivo", "body": p.marker + " mail", "area": p.area})
		})
		if ok {
			if id := m.str("id"); p.visible {
				f.visible[id] = true
			} else {
				f.hidden = append(f.hidden, id)
				f.forbid(id, "a hidden mail id")
			}
		}
		key := "lock:" + p.marker
		if _, ok := seedStep(t, f, "lease in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/leases", obj{"key": key})
		}); ok && !p.visible {
			f.hidden = append(f.hidden, key)
		}
	}
	// Native issues (SPEC "Native issues"): one in every place; the hidden
	// ones are walked as path params. The solver issue depends on the hidden
	// billing issue and is claimed by a hidden session: the guest sees
	// neither the edge (nor its count) nor the holder.
	issueIDs := map[string]string{}
	for _, p := range places {
		c := f.ownerAt(p.project, "")
		if got, ok := seedStep(t, f, "create issue in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/issues", obj{"title": p.marker + " issue", "area": p.area, "description": p.marker + " body"})
		}); ok {
			id := got.str("id")
			issueIDs[p.project] = id
			if p.visible {
				f.visible[id] = true
			} else {
				f.hidden = append(f.hidden, id)
				f.forbid(id, "a hidden issue's id")
			}
		}
	}
	if vis, hid := issueIDs["harbor/solver"], issueIDs["harbor/zz-billing"]; vis != "" && hid != "" {
		c := f.ownerAt("harbor/solver", sessions["harbor"])
		seedStep(t, f, "solver issue depends on the billing issue", func() (obj, error) {
			return post(ctx, c, "/v0/issues/"+url.PathEscape(vis)+"/deps", obj{"target": hid})
		})
		seedStep(t, f, "hidden session claims the solver issue", func() (obj, error) {
			return post(ctx, c, "/v0/issues/"+url.PathEscape(vis)+"/claim", obj{})
		})
	}
	// A session the guest cannot see holds the solver item: the guest must see "someone".
	if hs, item := sessions["harbor"], items["harbor/solver"]; hs != "" && item != "" {
		c := f.ownerAt("harbor/solver", hs)
		seedStep(t, f, "hidden session claims the solver item", func() (obj, error) {
			return post(ctx, c, "/v0/work/"+url.PathEscape(item)+"/claim", obj{})
		})
		seedStep(t, f, "hidden session mails the guest", func() (obj, error) {
			return post(ctx, c, "/v0/mail", obj{"to": "human:vida", "body": "VISIBLE from a hidden session", "area": "solver", "kind": "report"})
		})
	}
	// Mail in the guest's own area that stays with those who can act on it:
	// the owners mailbox and needs=human mail between others. A guest of the
	// area reads neither (list, get, thread, events).
	for _, m := range []obj{
		{"to": "owners", "body": "HIDDEN-owners escalation", "area": "solver"},
		{"to": "human:ivo", "body": "HIDDEN-needs-human note", "area": "solver", "needs": "human"},
	} {
		c := f.ownerAt("harbor/solver", "")
		if got, ok := seedStep(t, f, "restricted mail in solver to "+m.str("to"), func() (obj, error) {
			return post(ctx, c, "/v0/mail", m)
		}); ok {
			f.hidden = append(f.hidden, got.str("id"))
			f.forbid(got.str("id"), "restricted mail's id")
		}
	}
	// Asks (approvals, SPEC section 16) are needs=human mail to owners: in
	// every place, including the guest's own area, the guest reads none.
	for _, p := range places {
		c := f.ownerAt(p.project, "")
		if got, ok := seedStep(t, f, "ask in "+p.project, func() (obj, error) {
			return post(ctx, c, "/v0/asks", obj{"body": "HIDDEN-ask " + p.marker, "area": p.area,
				"subject": obj{"repo": "example-org/harbor", "pr": 7, "head_sha": strings.Repeat("ab", 20)}})
		}); ok {
			f.hidden = append(f.hidden, got.str("id"))
			f.forbid(got.str("id"), "an ask's id")
			f.asks[p.project] = got.str("id")
		}
	}
	// Automation records (SPEC 18.6): one in every place, the machine its home.
	seedAutomations(t, f)
	// Read marks (SPEC 9) are their person's: the owner's, in the guest's own
	// project and the hidden one, are never the guest's to read.
	for _, p := range []string{"harbor", "zz-side"} {
		c := f.ownerAt(p, "")
		seedStep(t, f, "owner's read mark in "+p, func() (obj, error) {
			var out obj
			err := c.Do(ctx, http.MethodPut, "/v0/mail/read-marks?project="+p, obj{"marks": []obj{{"conv": "HIDDEN-mark " + p, "upto_ms": 1}}}, &out)
			return out, err
		})
	}
	return f
}

type leakResp struct {
	status int
	body   string
}

// transport is how a guest request reaches the daemon.
type transport int

const (
	viaTailnet  transport = iota // network listener that attached WhoIs (core.WithRemote) + token
	viaUnmarked                  // network listener that attached nothing + token (fail-closed default)
	viaSocket                    // the Unix socket (core.WithLocal) + token
	viaTunnel                    // the tunnel door, signed by a guest daemon's key + token
)

func (v transport) String() string {
	return [...]string{"tailnet", "unmarked-net", "socket+token", "tunnel"}[v]
}

var transports = []transport{viaTailnet, viaUnmarked, viaSocket, viaTunnel}

// freezeProbe fixes dm's admission probe (the machine's live memory, swap
// and disk), so two calls of GET /v0/sessions/admission answer the same
// bytes and hidden-equals-missing holds there exactly.
func freezeProbe(dm *Daemon) {
	for _, m := range dm.Modules() {
		if s, ok := m.(*session.Service); ok {
			s.Probe = func(...string) orchapi.AdmissionProbe {
				return orchapi.AdmissionProbe{MemoryFreePercent: 50, SwapUsedMB: 0, DiskFreeMB: 100000}
			}
		}
	}
}

// liftNameLookups lifts dm's per-caller limit on uncached name lookups (30
// a minute): the walk sends every hidden id through GET /v0/names as one
// guest within a minute, and that limit is not what it tests (a refused
// lookup answers 429 for hidden and missing alike only once it is reached).
func liftNameLookups(dm *Daemon) {
	for _, m := range dm.Modules() {
		if s, ok := m.(*owners.Service); ok {
			s.MaxLookups = 1 << 20
		}
	}
}

// startTunnel serves dm's tunnel door on a loopback server (as cloudflared
// would reach it) and names it in tunnel.url, and makes a guest key.
func (f *leakFixture) startTunnel(t *testing.T) {
	t.Helper()
	var home *tunnel.Service
	for _, m := range f.dm.Modules() {
		if s, ok := m.(*tunnel.Service); ok {
			home = s
		}
	}
	if home == nil {
		t.Fatal("no tunnel module")
	}
	srv := httptest.NewServer(home.Handler(f.dm.Handler))
	t.Cleanup(srv.Close)
	f.tunURL = srv.URL
	if _, err := client.WithHandler(f.dm.SocketHandler()).TunnelUp(context.Background(), api.TunnelUpRequest{Listen: "127.0.0.1:0", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	gcfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	f.tunKey = tunnel.New(&core.Deps{Config: gcfg})
	fp, err := home.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	f.tunHC = &http.Client{Transport: f.tunKey.Transport(fp, time.Second, nil)}
}

// callTunnel is one guest request through the tunnel door.
func (f *leakFixture) callTunnel(method, target, project, body string) leakResp {
	req, err := http.NewRequest(method, f.tunURL+target, strings.NewReader(body))
	if err != nil {
		return leakResp{status: 400, body: err.Error()}
	}
	tok := f.tunnelTok
	if tok == "" {
		tok = f.guestTok
	}
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+tok)
	if project != "" {
		req.Header.Set(api.HeaderProject, project)
	}
	resp, err := f.tunHC.Do(req)
	if err != nil {
		return leakResp{status: 599, body: err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return leakResp{status: resp.StatusCode, body: string(b)}
}

// call runs one request as the guest with an empty JSON body.
func (f *leakFixture) call(method, target, project string, via transport) leakResp {
	return f.callBody(method, target, project, via, "{}")
}

// callBody runs one request as the guest with body (writes only).
func (f *leakFixture) callBody(method, target, project string, via transport, body string) leakResp {
	if method != http.MethodPost && method != http.MethodPatch && method != http.MethodDelete {
		body = ""
	}
	if via == viaTunnel {
		return f.callTunnel(method, target, project, body)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+f.guestTok)
	if project != "" {
		req.Header.Set(api.HeaderProject, project)
	}
	switch via {
	case viaTailnet:
		req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: "vida@example.com", Node: "vida-laptop"}))
	case viaSocket:
		req = req.WithContext(core.WithLocal(req.Context()))
	}
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	return leakResp{status: rec.Code, body: rec.Body.String()}
}

var paramRe = regexp.MustCompile(`\{([a-z_]+)\}`)

func (f *leakFixture) checkNoLeak(t *testing.T, what string, r leakResp) {
	t.Helper()
	for s, why := range f.forbidden {
		if strings.Contains(r.body, s) {
			t.Errorf("LEAK %s: response contains %q (%s):\n%s", what, s, why, clip(r.body))
		}
	}
}

func clip(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

func normalize(r leakResp, values ...string) string {
	b := r.body
	for _, v := range values {
		b = strings.ReplaceAll(b, url.PathEscape(v), "X")
		b = strings.ReplaceAll(b, v, "X")
		b = strings.ReplaceAll(b, strings.ToUpper(v), "X") // an id echoed in its canonical (upper) case
		if j, err := json.Marshal(v); err == nil {
			b = strings.ReplaceAll(b, strings.Trim(string(j), `"`), "X")
		}
	}
	return b
}

func TestGoldenLeak(t *testing.T) {
	f := newLeakFixture(t)
	sort.Strings(f.seeded)
	t.Logf("seeded: %v", f.seeded)
	routes := f.dm.Router.Routes()
	if len(routes) < 40 {
		t.Fatalf("only %d routes registered", len(routes))
	}
	for _, tr := range transports {
		f.walk(t, tr.String(), routes, func(method, target, project string) leakResp { return f.call(method, target, project, tr) }, nil, nil)
	}

	f.checkHiddenWrites(t)
	f.checkLiveEvents(t)

	// The tunnel takes no minted token: every route, public ones included,
	// answers it 401 (SPEC 17.11). TestGoldenLeakTeams walks the tunnel
	// with a key-bound guest token.
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		target := paramRe.ReplaceAllString(path, "x")
		r := f.call(method, target, "", viaTunnel)
		// A grant add's body names a secret, so the tunnel client refuses the
		// placeholder before the door can 401 the minted token.
		if rt.Pattern == "POST /v0/secret_grants" {
			if r.status == 599 && strings.Contains(r.body, "secret grant") {
				// An empty body is not a secret, so the door still 401s the minted token.
				if empty := f.callTunnel("POST", "/v0/secret_grants", "", ""); empty.status != http.StatusUnauthorized {
					t.Errorf("tunnel, minted token, empty grant body: %d %s", empty.status, clip(empty.body))
				}
				continue
			}
		} else if r.status == http.StatusUnauthorized {
			continue
		}
		t.Errorf("tunnel, minted token: %s answered %d %s", rt.Pattern, r.status, clip(r.body))
	}
	// Not vacuous: the guest does see its own project and area.
	r := f.call("GET", "/v0/projects", "", viaTailnet)
	var pl api.ProjectList
	if err := json.Unmarshal([]byte(r.body), &pl); err != nil || len(pl.Projects) != 1 || pl.Projects[0].Slug != "harbor" ||
		len(pl.Projects[0].Areas) != 1 || pl.Projects[0].Areas[0].Name != "solver" || pl.Projects[0].Role != "guest" {
		t.Fatalf("guest projects: %s", r.body)
	}
	// Admin and local-only routes refuse the guest.
	if r := f.call("POST", "/v0/grants", "", viaTailnet); r.status == 200 {
		t.Fatalf("guest granted: %s", r.body)
	}
	if r := f.call("GET", "/v0/tokens", "", viaTailnet); r.status != 403 {
		t.Fatalf("guest listed tokens: %d %s", r.status, r.body)
	}
	// Task controls and results are socket-owner only, including a token on the socket.
	for _, tr := range transports {
		for _, tc := range []struct{ method, path string }{{"GET", "/v0/tasks/agents"}, {"POST", "/v0/tasks"}, {"GET", "/v0/tasks/nope"}, {"GET", "/v0/tasks/nope/read?item=nope%3A0"}, {"POST", "/v0/tasks/nope/cancel"}, {"DELETE", "/v0/tasks/nope/transcript"}, {"GET", "/v0/tasks/cloud"}, {"PUT", "/v0/tasks/cloud"}} {
			r := f.callBody(tc.method, tc.path, "", tr, `{}`)
			want := http.StatusForbidden
			if tr == viaTunnel {
				want = http.StatusUnauthorized
			}
			if r.status != want {
				t.Errorf("%s %s %s: task route answered %d %s", tr.String(), tc.method, tc.path, r.status, clip(r.body))
			}
		}
	}
	// No credentials on the network: only health answers.
	req := httptest.NewRequest("GET", "/v0/projects", nil)
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: "mallory@example.com"}))
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("anonymous network caller: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/v0/health", nil)
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{}))
	rec = httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "pid") {
		t.Fatalf("anonymous health: %d %s", rec.Code, rec.Body.String())
	}
}

// walk calls every route in routes through call (as the guest) with every
// hidden id as each path parameter, and asserts no response leaks and a
// hidden id answers exactly like a missing one. skip leaves routes out;
// local, when set, names routes that answer only from the caller's own
// machine (a replica's owner), where only the leak check applies.
func (f *leakFixture) walk(t *testing.T, via string, routes []core.Route, call func(method, target, project string) leakResp, skip, local func(string) bool) {
	t.Helper()
	for _, rt := range routes {
		if skip != nil && skip(rt.Pattern) {
			continue
		}
		if rt.Check == "" {
			t.Errorf("route %s documents no authz check", rt.Pattern)
		}
		method, path, _ := strings.Cut(rt.Pattern, " ")
		params := paramRe.FindAllString(path, -1)
		if len(params) == 0 {
			target := path
			if path == "/v0/events" {
				target += "?since=0&live=0"
			}
			r := call(method, target, "")
			f.checkNoLeak(t, via+" "+rt.Pattern, r)
			if method == http.MethodGet {
				// ?project= and Zm-Project naming a hidden project answer like a missing one.
				hid := call(method, target+sep(target)+"project=zz-side", "")
				mis := call(method, target+sep(target)+"project=nope-zz", "")
				f.checkNoLeak(t, via+" "+rt.Pattern+"?project=hidden", leakResp{hid.status, normalize(hid, "zz-side")})
				if (local == nil || !local(rt.Pattern)) && (hid.status != mis.status || normalize(hid, "zz-side") != normalize(mis, "nope-zz")) {
					t.Errorf("%s %s ?project=: hidden %d %q != missing %d %q", via, rt.Pattern, hid.status, clip(hid.body), mis.status, clip(mis.body))
				}
				hh := call(method, target, "zz-side")
				mh := call(method, target, "nope-zz")
				if local != nil && local(rt.Pattern) {
					continue
				}
				if hh.status != mh.status || normalize(hh, "zz-side") != normalize(mh, "nope-zz") {
					t.Errorf("%s %s Zm-Project: hidden %d %q != missing %d %q", via, rt.Pattern, hh.status, clip(hh.body), mh.status, clip(mh.body))
				}
			}
			continue
		}
		fill := func(v string) string {
			return paramRe.ReplaceAllStringFunc(path, func(string) string { return url.PathEscape(v) })
		}
		missing := "nope-" + strings.Trim(params[0], "{}")
		mis := call(method, fill(missing), "")
		f.checkNoLeak(t, via+" "+rt.Pattern+" (missing)", mis)
		for _, h := range f.hidden {
			hid := call(method, fill(h), "")
			// Echoing the caller's own input back is not a leak; anything else is.
			f.checkNoLeak(t, via+" "+rt.Pattern+" with "+h, leakResp{hid.status, normalize(hid, h)})
			if local != nil && local(rt.Pattern) {
				continue
			}
			if hid.status != mis.status || normalize(hid, h) != normalize(mis, missing) {
				t.Errorf("%s %s: hidden %q answered %d %q, missing answered %d %q",
					via, rt.Pattern, h, hid.status, clip(hid.body), mis.status, clip(mis.body))
			}
		}
	}
}

func sep(target string) string {
	if strings.Contains(target, "?") {
		return "&"
	}
	return "?"
}
