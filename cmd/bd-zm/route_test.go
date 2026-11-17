package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// routeShim is a daemon with two projects holding native issues (oc and sg),
// a client like one under $ZM_PROJECT=oc (it resolves writes, routing by id
// beats it), the requests each bd run sends, and a file cache.
type routeShim struct {
	*shim
	mu      sync.Mutex
	reqs    []string // "METHOD path project"
	extra   []orchapi.IssueProjectInfo
	skipped bool
	oc, sg  string // an issue in each
}

func newRouteShim(t *testing.T) *routeShim {
	t.Helper()
	for _, k := range []string{"BEADS_DIR", "BEADS_DB", "BD_ACTOR", "BEADS_ACTOR", "FARM_BD_ACTOR", "USER", "CLAUDE_CODE", "BD_AGENT_MODE",
		"CLAUDE_SESSION_ID", "ZM_AS", "ZM_HOST", "ZM_PROJECT", "ZM_TOKEN"} {
		t.Setenv(k, "")
	}
	git := gitUserName
	gitUserName = func() string { return "" }
	t.Cleanup(func() { gitUserName = git })
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	rs := &routeShim{}
	h := dm.SocketHandler()
	base := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.reqs = append(rs.reqs, r.Method+" "+r.URL.Path+" "+api.GetHeader(r.Header, api.HeaderProject)+"/"+api.GetHeader(r.Header, api.HeaderProjectFrom))
		extra, skipped := rs.extra, rs.skipped
		rs.mu.Unlock()
		if r.URL.Path != "/v0/issues/projects" || (len(extra) == 0 && !skipped) {
			h.ServeHTTP(w, r)
			return
		}
		// A replica's merged answer: another authority's projects, or one
		// that did not answer.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var ps struct {
			Projects []orchapi.IssueProjectInfo `json:"projects"`
			Skipped  []api.WorkSkipped          `json:"skipped,omitempty"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &ps)
		ps.Projects = append(ps.Projects, extra...)
		if skipped {
			ps.Skipped = []api.WorkSkipped{{Project: "01AWAY", Source: "authority", Error: "away: unreachable"}}
		}
		for k, vs := range rec.Header() {
			if k != "Content-Length" {
				w.Header()[k] = vs
			}
		}
		api.WriteJSON(w, http.StatusOK, ps)
	}))
	ctx := context.Background()
	for _, slug := range []string{"oc", "sg"} {
		if _, err := base.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
		c := *base
		c.Project, c.ProjectFrom = slug, "flag"
		it, err := orchclient.CreateIssue(ctx, &c, orchapi.NewIssueRequest{Title: "in " + slug})
		if err != nil {
			t.Fatal(err)
		}
		if slug == "oc" {
			rs.oc = it.ID
		} else {
			rs.sg = it.ID
		}
	}
	base.Project, base.ProjectFrom = "oc", "env"
	newClient = func(context.Context) (*client.Client, error) { c := *base; return &c, nil }
	t.Cleanup(func() { newClient = defaultClient })
	cacheHome = home
	t.Cleanup(func() { cacheHome = "" })
	rs.shim = &shim{t: t, c: base}
	return rs
}

// run is bd args and the requests it sent.
func (rs *routeShim) run(args ...string) (string, string, int, []string) {
	rs.t.Helper()
	rs.mu.Lock()
	rs.reqs = nil
	rs.mu.Unlock()
	out, errOut, code := rs.bd(args...)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return out, errOut, code, append([]string(nil), rs.reqs...)
}

func count(reqs []string, prefix string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func TestRouteByPrefix(t *testing.T) {
	rs := newRouteShim(t)
	if !strings.HasPrefix(rs.oc, "oc-") || !strings.HasPrefix(rs.sg, "sg-") {
		t.Fatalf("ids %s %s", rs.oc, rs.sg)
	}
	// Under $ZM_PROJECT=oc, an sg id names sg: each call goes to sg alone.
	for _, args := range [][]string{
		{"show", rs.sg, "--json"},
		{"update", rs.sg, "--title", "renamed"},
		{"comments", "add", rs.sg, "hello"},
		{"comment", rs.sg, "again"},
		{"comments", rs.sg},
		{"note", rs.sg, "a note"},
		{"label", "add", rs.sg, "x"},
		{"label", "remove", rs.sg, "x"},
		{"close", rs.sg},
		{"reopen", rs.sg},
	} {
		out, errOut, code, reqs := rs.run(args...)
		if code != 0 {
			t.Fatalf("bd %v: exit %d: %s %s", args, code, out, errOut)
		}
		for _, r := range reqs {
			if strings.Contains(r, "/v0/issues/"+rs.sg) && !strings.HasSuffix(r, " sg/flag") {
				t.Fatalf("bd %v: %q does not name sg", args, r)
			}
		}
		if count(reqs, "GET /v0/issues/"+rs.sg)+count(reqs, "POST /v0/issues/"+rs.sg)+count(reqs, "PATCH /v0/issues/"+rs.sg)+count(reqs, "DELETE /v0/issues/"+rs.sg) == 0 {
			t.Fatalf("bd %v sent no item call: %v", args, reqs)
		}
	}
	// A child lands in its parent's project; children and --parent lists cover it.
	child := strings.TrimSpace(rs.ok("create", "kid", "--parent", rs.sg, "--silent"))
	if !strings.HasPrefix(child, rs.sg+".") {
		t.Fatalf("child %q of %s", child, rs.sg)
	}
	for _, args := range [][]string{{"children", rs.sg, "--json"}, {"list", "--parent", rs.sg, "--json"}, {"ready", "--parent", rs.sg, "--json"}} {
		if out := rs.ok(args...); !strings.Contains(out, child) {
			t.Fatalf("bd %v lost the child: %s", args, out)
		}
	}
	// --project names the project outright (no routing) and lands creates.
	made := strings.TrimSpace(rs.ok("--project", "sg", "create", "flagged", "--silent"))
	if !strings.HasPrefix(made, "sg-") {
		t.Fatalf("--project sg create made %q", made)
	}
	if _, _, code, _ := rs.run("--project", "oc", "show", rs.sg); code == 0 {
		t.Fatal("--project oc found an sg issue")
	}
	// Lists stay on bd's project unless --all-projects.
	if out := rs.ok("list", "--json"); strings.Contains(out, rs.sg) || !strings.Contains(out, rs.oc) {
		t.Fatalf("list: %s", out)
	}
	for _, args := range [][]string{{"list", "--all-projects", "--json"}, {"ready", "--all-projects", "--json"}} {
		if out := rs.ok(args...); !strings.Contains(out, rs.sg) || !strings.Contains(out, rs.oc) {
			t.Fatalf("bd %v: %s", args, out)
		}
	}
	if _, errOut, code, _ := rs.run("--project", "oc", "list", "--all-projects"); code == 0 || !strings.Contains(errOut, "--all-projects") {
		t.Fatalf("--project with --all-projects: %d %s", code, errOut)
	}
	if _, errOut, code, _ := rs.run("list", "--all-projects", "--parent", rs.sg); code == 0 || !strings.Contains(errOut, "--parent") {
		t.Fatalf("--parent with --all-projects: %d %s", code, errOut)
	}
}

func TestRouteRefusals(t *testing.T) {
	rs := newRouteShim(t)
	// A prefix no visible project has: not found, naming the prefixes seen.
	_, errOut, code, _ := rs.run("show", "zz-abc")
	if code == 0 || !strings.Contains(errOut, "Issue zz-abc not found: no project you see uses its prefix; you see oc- (oc), sg- (sg)") {
		t.Fatalf("unknown prefix: %d %q", code, errOut)
	}
	_, errOut, _, _ = rs.run("update", "zz-abc", "--title", "x")
	if !strings.Contains(errOut, "you see oc- (oc), sg- (sg)") {
		t.Fatalf("update unknown prefix: %q", errOut)
	}
	// Two visible projects sharing a prefix (another authority's sg): refused
	// with the fix, before any item call; --project picks one.
	rs.extra = []orchapi.IssueProjectInfo{{Project: "01OTHERAUTHORITY", Slug: "sg2", Prefix: "sg"}}
	os.Remove(cachePath()) // within prefixTTL a cached map still routes to sg
	_, errOut, code, reqs := rs.run("show", rs.sg)
	if code == 0 || !strings.Contains(errOut, `issue prefix "sg" of `+rs.sg+` is used by 2 projects you see (sg, sg2): name one, like bd --project sg`) {
		t.Fatalf("ambiguous: %d %q", code, errOut)
	}
	if count(reqs, "GET /v0/issues/"+rs.sg) != 0 {
		t.Fatalf("ambiguous id was sent: %v", reqs)
	}
	if out := rs.ok("--project", "sg", "show", rs.sg, "--json"); !strings.Contains(out, rs.sg) {
		t.Fatalf("--project resolves the ambiguity: %s", out)
	}
	rs.extra = nil
	os.Remove(cachePath())
	// A dependency across projects is refused before any call, with the
	// alternative; within a project it is added.
	sg2 := strings.TrimSpace(rs.ok("--project", "sg", "create", "second", "--silent"))
	for _, args := range [][]string{{"dep", "add", rs.oc, rs.sg}, {"dep", rs.sg, "--blocks", rs.oc}, {"dep", "add", rs.oc, "--blocked-by", rs.sg}} {
		_, errOut, code, reqs := rs.run(args...)
		if code == 0 || !strings.Contains(errOut, "external:") || !strings.Contains(errOut, "one project's issues") {
			t.Fatalf("bd %v: %d %q", args, code, errOut)
		}
		if count(reqs, "POST") != 0 {
			t.Fatalf("bd %v wrote: %v", args, reqs)
		}
	}
	if _, errOut, code, _ := rs.run("dep", "remove", rs.oc, rs.sg); code == 0 || !strings.Contains(errOut, "no dependency joins two projects") {
		t.Fatalf("dep remove across: %d %q", code, errOut)
	}
	rs.ok("dep", "add", sg2, rs.sg)
	rs.ok("dep", "add", rs.oc, "external:"+rs.sg, "--type", "related")
	// An external ref is text: never completed with a prefix, never routed.
	if out := rs.ok("dep", "add", rs.oc, "external:JIRA123", "--type", "related", "--json"); !strings.Contains(out, `"depends_on_id": "external:JIRA123"`) {
		t.Fatalf("external ref: %s", out)
	}
	if out := rs.ok("show", sg2, "--json"); !strings.Contains(out, rs.sg) {
		t.Fatalf("dep within sg: %s", out)
	}
	rs.ok("dep", "remove", sg2, rs.sg)
}

// TestRouteRoundTrips holds the cost: a cold call adds one round trip (the
// prefix map), a warm one none; the map is cached for prefixTTL, never when
// an authority was skipped, and a prefix the cache lacks is looked up again.
func TestRouteRoundTrips(t *testing.T) {
	rs := newRouteShim(t)
	t0 := time.Now()
	now = func() time.Time { return t0 }
	t.Cleanup(func() { now = time.Now })
	maps := func(reqs []string) int { return count(reqs, "GET /v0/issues/projects") }
	_, _, _, reqs := rs.run("show", rs.sg)
	if maps(reqs) != 1 || len(reqs) != 2 {
		t.Fatalf("cold show: %v", reqs)
	}
	st, err := os.Stat(cachePath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", st, err)
	}
	b, _ := os.ReadFile(cachePath())
	if bytes.Contains(b, []byte(rs.c.Token)) && rs.c.Token != "" {
		t.Fatal("the token is in the cache")
	}
	_, _, _, reqs = rs.run("show", rs.sg)
	if maps(reqs) != 0 || len(reqs) != 1 {
		t.Fatalf("warm show: %v", reqs)
	}
	_, _, _, reqs = rs.run("update", rs.oc, rs.sg, "--title", "both")
	if maps(reqs) != 0 || len(reqs) != 2 {
		t.Fatalf("warm update of two ids: %v", reqs)
	}
	now = func() time.Time { return t0.Add(prefixTTL) }
	_, _, _, reqs = rs.run("show", rs.sg)
	if maps(reqs) != 1 {
		t.Fatalf("expired cache: %v", reqs)
	}
	// Another client (a token, another daemon) never reads this one's map.
	rs.c.Token = "zm_other"
	_, _, _, reqs = rs.run("show", rs.sg)
	if maps(reqs) != 1 {
		t.Fatalf("another key used the cache: %v", reqs)
	}
	rs.c.Token = ""
	// A prefix the warm cache lacks: one fresh look, then the answer.
	_, _, _, reqs = rs.run("show", "zz-abc")
	if maps(reqs) != 1 {
		t.Fatalf("unknown prefix on a warm cache: %v", reqs)
	}
	// An authority that did not answer: the map is not cached, and an
	// unknown prefix goes out unnamed (the daemon answers it).
	os.Remove(cachePath())
	rs.skipped = true
	_, errOut, _, reqs := rs.run("show", "zz-abc")
	if count(reqs, "GET /v0/issues/zz-abc") != 1 || strings.Contains(errOut, "you see") {
		t.Fatalf("partial map: %v %q", reqs, errOut)
	}
	if _, err := os.Stat(cachePath()); err == nil {
		t.Fatal("a partial map was cached")
	}
	rs.skipped = false
	// A cache file that is not ours is ignored, never trusted.
	if err := os.WriteFile(filepath.Join(cacheHome, "cache", "bd-prefixes.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, code, _ := rs.run("show", rs.sg); code != 0 || !strings.Contains(out, rs.sg) {
		t.Fatalf("bad cache file: %d %s", code, out)
	}
}
