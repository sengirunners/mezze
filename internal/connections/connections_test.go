package connections_test

// Conformance and acceptance for connections (SPEC 24): the example provider
// (fakeci, a CI with a queue) passes the contract; zm fetches it over HTTP
// and a Unix socket, caches the last good summary, marks it stale when old,
// refused or malformed, and refuses it when too old; scope decides who sees
// and registers what. Providers are httptest servers and sockets in a temp
// ZM_HOME; nothing leaves the machine.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/connections"
	"github.com/sengirunners/mezze/internal/connections/fakeci"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// clock is a settable now shared by the daemon and the fake CI.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type cx struct {
	t     *testing.T
	home  string
	dm    *server.Daemon
	svc   *connections.Service
	owner *client.Client
	clk   *clock
}

func newCX(t *testing.T) *cx {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	x := &cx{t: t, home: home, dm: dm, owner: client.WithHandler(dm.SocketHandler()),
		clk: &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}}
	for _, m := range dm.Modules() {
		if s, ok := m.(*connections.Service); ok {
			x.svc = s
		}
	}
	if x.svc == nil {
		t.Fatal("connections module not wired")
	}
	x.svc.Now = x.clk.Now
	ctx := context.Background()
	if _, err := x.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "billing"} {
		if _, err := x.owner.AddArea(ctx, "harbor", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

// as is a network caller with a token for project/area in role.
func (x *cx) as(who, project, area, role string) *client.Client {
	x.t.Helper()
	tok, err := x.owner.MintToken(context.Background(), api.MintTokenRequest{As: who, Project: project, Area: area, Role: role})
	if err != nil {
		x.t.Fatal(err)
	}
	c := client.WithHandler(x.dm.Handler)
	c.Token = tok.Secret
	return c
}

func (x *cx) ci() (*fakeci.CI, *httptest.Server) {
	ci := fakeci.New("ci-token-123", "example-org", 2)
	ci.Now = x.clk.Now
	srv := httptest.NewServer(ci)
	x.t.Cleanup(srv.Close)
	return ci, srv
}

func (x *cx) show(c *client.Client, id string) orchapi.ConnectionShow {
	x.t.Helper()
	s, err := orchclient.ShowConnection(context.Background(), c, id, "")
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

func TestFakeCIConforms(t *testing.T) {
	ci := fakeci.New("", "example-org", 1)
	ci.Enqueue(fakeci.Job{ID: "j1", Name: "check", Repo: "example-org/harbor", URL: "https://github.com/example-org/harbor/actions/runs/1"})
	ci.Enqueue(fakeci.Job{ID: "j2", Name: "smoke", Repo: "example-org/harbor"})
	ci.Tick()
	ci.SetRunners(0)
	b, err := json.Marshal(ci.Summary())
	if err != nil {
		t.Fatal(err)
	}
	s, err := connections.Validate(b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Headline) != 2 || s.Headline[0].State != orchapi.ConnectionRed || len(s.Running) != 1 || len(s.Meters) != 1 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestValidateRefuses(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	good := func() map[string]any {
		return map[string]any{
			"schema": orchapi.ConnectionSchema, "as_of": now.Format(time.RFC3339),
			"headline": []any{map[string]any{"key": "q", "label": "Queue", "state": "ok", "count": 3, "account": "a"}},
			"meters":   []any{map[string]any{"key": "cpu", "label": "Cores", "used": 4, "total": 8, "unit": "cores", "account": "a"}},
			"running": []any{map[string]any{"id": "1", "title": "check", "account": "a",
				"links": []any{map[string]any{"label": "Run", "url": "https://ci.example.com/1"}}}},
			"future_field": "ignored",
		}
	}
	enc := func(m map[string]any) []byte { b, _ := json.Marshal(m); return b }
	if _, err := connections.Validate(enc(good()), now); err != nil {
		t.Fatalf("good summary refused: %v", err)
	}
	if _, err := connections.Validate([]byte(`{"schema":"`+orchapi.ConnectionSchema+`","as_of":"`+now.Format(time.RFC3339)+`"}`), now); err != nil {
		t.Fatalf("empty lists refused: %v", err)
	}
	row := func(m map[string]any) map[string]any { return m["headline"].([]any)[0].(map[string]any) }
	meter := func(m map[string]any) map[string]any { return m["meters"].([]any)[0].(map[string]any) }
	item := func(m map[string]any) map[string]any { return m["running"].([]any)[0].(map[string]any) }
	cases := map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"schema":         {func(m map[string]any) { m["schema"] = "zm.connection.show/2" }, "schema"},
		"no as_of":       {func(m map[string]any) { delete(m, "as_of") }, "as_of: required"},
		"future as_of":   {func(m map[string]any) { m["as_of"] = now.Add(time.Hour).Format(time.RFC3339) }, "as_of: in the future"},
		"stale_after":    {func(m map[string]any) { m["stale_after_s"] = 5 }, "stale_after_s"},
		"state":          {func(m map[string]any) { row(m)["state"] = "yellow" }, "headline[0].state"},
		"negative count": {func(m map[string]any) { row(m)["count"] = -1 }, "headline[0].count"},
		"row account":    {func(m map[string]any) { delete(row(m), "account") }, "headline[0].account: required"},
		"meter account":  {func(m map[string]any) { meter(m)["account"] = " " }, "meters[0].account: required"},
		"item account":   {func(m map[string]any) { delete(item(m), "account") }, "running[0].account: required"},
		"zero total":     {func(m map[string]any) { meter(m)["total"] = 0 }, "meters[0].total"},
		"negative used":  {func(m map[string]any) { meter(m)["used"] = -2 }, "meters[0].used"},
		"no unit":        {func(m map[string]any) { delete(meter(m), "unit") }, "meters[0].unit"},
		"script link":    {func(m map[string]any) { item(m)["links"].([]any)[0].(map[string]any)["url"] = "javascript:alert(1)" }, "links[0].url"},
		"control char":   {func(m map[string]any) { item(m)["title"] = "a\x1b[31mb" }, "control character"},
		"duplicate key":  {func(m map[string]any) { m["headline"] = append(m["headline"].([]any), row(good())) }, "repeats"},
		"wrong type":     {func(m map[string]any) { m["headline"] = "lots" }, "not JSON in the summary's shape"},
		"too many items": {func(m map[string]any) { m["running"] = make([]any, connections.MaxRunning+1) }, "more than"},
		"future started": {func(m map[string]any) { item(m)["started"] = now.Add(time.Hour).Format(time.RFC3339) }, "started: in the future"},
		"long label":     {func(m map[string]any) { row(m)["label"] = strings.Repeat("x", 81) }, "longer than"},
		"empty item id":  {func(m map[string]any) { item(m)["id"] = "" }, "running[0].id: required"},
		"credentialed ln": {func(m map[string]any) {
			item(m)["links"].([]any)[0].(map[string]any)["url"] = "https://u:p@x.example.com"
		}, "links[0].url"},
	}
	for name, c := range cases {
		m := good()
		c.edit(m)
		_, err := connections.Validate(enc(m), now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := connections.Validate([]byte(strings.Repeat(" ", connections.MaxBody+1)), now); err == nil {
		t.Error("oversize body accepted")
	}
	if _, err := connections.Validate([]byte(`{} {}`), now); err == nil {
		t.Error("two values accepted")
	}
}

func TestJudge(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	body := func(asOf time.Time) []byte {
		b, _ := json.Marshal(orchapi.ConnectionSummary{Schema: orchapi.ConnectionSchema, AsOf: asOf, StaleAfterS: 60})
		return b
	}
	for _, c := range []struct {
		name, lastErr, state, reason string
		body                         []byte
	}{
		{"fresh", "", orchapi.ConnectionFresh, "", body(now.Add(-30 * time.Second))},
		{"old numbers", "", orchapi.ConnectionStale, "has not updated its numbers for 5 minutes", body(now.Add(-5 * time.Minute))},
		{"failed refresh", "the provider answered HTTP 502", orchapi.ConnectionStale, "could not refresh it: the provider answered HTTP 502; showing numbers from 10 seconds ago", body(now.Add(-10 * time.Second))},
		{"too old", "", orchapi.ConnectionUnavailable, "2 days old, too old to show", body(now.Add(-49 * time.Hour))},
		{"never", "", orchapi.ConnectionUnavailable, "has not had an answer", nil},
		{"never, failing", "nothing is listening", orchapi.ConnectionUnavailable, "could not read it: nothing is listening", nil},
	} {
		got := connections.Judge(c.body, &now, &now, c.lastErr, now)
		if got.State != c.state || !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: %s %q", c.name, got.State, got.Reason)
		}
		if (got.Summary == nil) != (c.state == orchapi.ConnectionUnavailable) {
			t.Errorf("%s: summary %v with state %s", c.name, got.Summary != nil, got.State)
		}
	}
}

func TestShowOverHTTP(t *testing.T) {
	x := newCX(t)
	ctx := context.Background()
	ci, srv := x.ci()
	ci.Enqueue(fakeci.Job{ID: "j1", Name: "check", Repo: "example-org/harbor", URL: "https://github.com/example-org/harbor/actions/runs/1"})
	ci.Tick()
	c, err := orchclient.AddConnection(ctx, x.owner, orchapi.NewConnectionRequest{Project: "harbor", Name: "Fake CI", Endpoint: srv.URL,
		Token: "ci-token-123", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	s := x.show(x.owner, c.ID)
	if s.State != orchapi.ConnectionFresh || s.Summary == nil || len(s.Summary.Running) != 1 || s.Summary.Running[0].Title != "check" {
		t.Fatalf("first show: %+v", s)
	}
	if s.Connection.Endpoint != srv.URL || s.Connection.Project != "harbor" {
		t.Fatalf("connection: %+v", s.Connection)
	}

	// Within the refresh window: the cache answers, even though the CI moved on.
	for i := 0; i < 12; i++ {
		ci.Enqueue(fakeci.Job{ID: "q" + string(rune('a'+i)), Name: "queued"})
	}
	if s = x.show(x.owner, c.ID); *s.Summary.Headline[0].Count != 0 {
		t.Fatalf("cache not used: %+v", s.Summary.Headline[0])
	}
	x.clk.add(connections.DefaultRefresh)
	if s = x.show(x.owner, c.ID); s.Summary.Headline[0].State != orchapi.ConnectionWarn || *s.Summary.Headline[0].Count != 12 {
		t.Fatalf("queue: %+v", s.Summary.Headline[0])
	}

	// A wrong token: the last good summary, stale, in plain words.
	ci.Token = "rotated"
	x.clk.add(connections.DefaultRefresh)
	s = x.show(x.owner, c.ID)
	if s.State != orchapi.ConnectionStale || s.Summary == nil || !strings.Contains(s.Reason, "refused zm's token") {
		t.Fatalf("refused token: %s %q", s.State, s.Reason)
	}
	if strings.Contains(s.Reason, "ci-token-123") || strings.Contains(s.Reason, srv.URL) {
		t.Fatalf("reason leaks the token or URL: %q", s.Reason)
	}

	// Malformed: still the last good one, marked, naming the break.
	ci.Token = "ci-token-123"
	ci.Serve([]byte(`{"schema":"` + orchapi.ConnectionSchema + `","as_of":"2026-11-05T12:00:30Z","headline":[{"key":"q","label":"Q","state":"purple","account":"a"}]}`))
	x.clk.add(connections.DefaultRefresh)
	s = x.show(x.owner, c.ID)
	if s.State != orchapi.ConnectionStale || !strings.Contains(s.Reason, "headline[0].state") || s.Summary.Headline[0].State != orchapi.ConnectionWarn {
		t.Fatalf("malformed: %s %q %+v", s.State, s.Reason, s.Summary)
	}

	// The provider stops updating as_of: stale; after a day, refused.
	old := ci.Summary()
	b, _ := json.Marshal(old)
	ci.Serve(b)
	x.clk.add(2 * time.Minute)
	if s = x.show(x.owner, c.ID); s.State != orchapi.ConnectionStale || !strings.Contains(s.Reason, "has not updated its numbers for 2 minutes") {
		t.Fatalf("old as_of: %s %q", s.State, s.Reason)
	}
	x.clk.add(25 * time.Hour)
	if s = x.show(x.owner, c.ID); s.State != orchapi.ConnectionUnavailable || s.Summary != nil || !strings.Contains(s.Reason, "too old to show") {
		t.Fatalf("too old: %s %q", s.State, s.Reason)
	}

	// Gone: not found, and ls is empty.
	if _, err := orchclient.RemoveConnection(ctx, x.owner, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.ShowConnection(ctx, x.owner, c.ID, ""); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("removed: %v", err)
	}
}

func TestShowOverSocketAndAccounts(t *testing.T) {
	x := newCX(t)
	ctx := context.Background()
	ci := fakeci.New("sock-token", "acme", 3)
	ci.Now = x.clk.Now
	sock := filepath.Join(x.home, "ci.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /zm/show", ci)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	tokFile := filepath.Join(x.home, "ci-token")
	if err := os.WriteFile(tokFile, []byte("sock-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := orchclient.AddConnection(ctx, x.owner, orchapi.NewConnectionRequest{Project: "harbor", Name: "Socket CI",
		Endpoint: "unix:" + sock + "?path=/zm/show", TokenRef: "file:" + tokFile})
	if err != nil {
		t.Fatal(err)
	}
	s := x.show(x.owner, c.ID)
	if s.State != orchapi.ConnectionFresh || s.Summary.Meters[0].Total != 3 {
		t.Fatalf("socket show: %s %q %+v", s.State, s.Reason, s.Summary)
	}
	other, err := orchclient.ShowConnection(ctx, x.owner, c.ID, "someone-else")
	if err != nil || len(other.Summary.Headline)+len(other.Summary.Meters)+len(other.Summary.Running) != 0 {
		t.Fatalf("account filter: %v %+v", err, other.Summary)
	}
	mine, _ := orchclient.ShowConnection(ctx, x.owner, c.ID, "acme")
	if len(mine.Summary.Headline) != 2 {
		t.Fatalf("own account: %+v", mine.Summary)
	}
}

func TestScope(t *testing.T) {
	x := newCX(t)
	ctx := context.Background()
	ci, srv := x.ci()
	_ = ci
	// Network callers fetch through the SSRF guard; point it at the test server.
	x.svc.Fetcher = func(e connections.Endpoint, _ bool) connections.Fetcher {
		u, _ := connections.ParseEndpoint(srv.URL, true)
		return u.Fetcher(true)
	}
	admin := x.as("vida", "harbor", "solver", "owner")
	for name, req := range map[string]orchapi.NewConnectionRequest{
		"socket":        {Project: "harbor/solver", Name: "s", Endpoint: "unix:/tmp/x.sock"},
		"allow private": {Project: "harbor/solver", Name: "p", Endpoint: "http://127.0.0.1:9/", AllowPrivate: true},
		"token ref":     {Project: "harbor/solver", Name: "r", Endpoint: "https://ci.example.com/", TokenRef: "file:/etc/passwd"},
	} {
		if _, err := orchclient.AddConnection(ctx, admin, req); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("%s: %v, want forbidden", name, err)
		}
	}
	if _, err := orchclient.AddConnection(ctx, admin, orchapi.NewConnectionRequest{Project: "harbor/solver", Name: "loop", Endpoint: "http://127.0.0.1:9/"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("private URL without allow_private: %v", err)
	}
	if _, err := orchclient.AddConnection(ctx, admin, orchapi.NewConnectionRequest{Project: "harbor/billing", Name: "x", Endpoint: "https://ci.example.com/"}); err == nil {
		t.Error("an admin of solver added one in billing")
	}
	c, err := orchclient.AddConnection(ctx, admin, orchapi.NewConnectionRequest{Project: "harbor/solver", Name: "Solver CI",
		Endpoint: "https://ci.example.com/zm", Token: "ci-token-123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.AddConnection(ctx, admin, orchapi.NewConnectionRequest{Project: "harbor/solver", Name: "Solver CI", Endpoint: "https://ci.example.com/zm"}); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("duplicate name: %v", err)
	}
	if s := x.show(admin, c.ID); s.State != orchapi.ConnectionFresh {
		t.Fatalf("admin show: %s %q", s.State, s.Reason)
	}

	// A member of solver reads it, without the endpoint, and may not remove it.
	member := x.as("bea", "harbor", "solver", "member")
	l, err := orchclient.Connections(ctx, member, "")
	if err != nil || len(l) != 1 || l[0].Endpoint != "" || l[0].CreatedBy != "" {
		t.Fatalf("member list: %v %+v", err, l)
	}
	if s := x.show(member, c.ID); s.State != orchapi.ConnectionFresh || s.Connection.Endpoint != "" {
		t.Fatalf("member show: %+v", s.Connection)
	}
	if _, err := orchclient.RemoveConnection(ctx, member, c.ID); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("member remove: %v", err)
	}

	// Billing's owner sees nothing: hidden is exactly missing.
	billing := x.as("ana", "harbor", "billing", "owner")
	if l, err := orchclient.Connections(ctx, billing, ""); err != nil || len(l) != 0 {
		t.Fatalf("billing list: %v %+v", err, l)
	}
	_, hidden := orchclient.ShowConnection(ctx, billing, c.ID, "")
	_, missing := orchclient.ShowConnection(ctx, billing, "01JZZZZZZZZZZZZZZZZZZZZZZZ", "")
	if !api.IsCode(hidden, api.CodeNotFound) || !api.IsCode(missing, api.CodeNotFound) {
		t.Fatalf("hidden %v, missing %v", hidden, missing)
	}
	if _, err := orchclient.RemoveConnection(ctx, billing, c.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("billing remove: %v", err)
	}
}
