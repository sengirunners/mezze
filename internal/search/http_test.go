package search_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/search"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// TestSearchRoute: GET /v0/search for the owner over the project's paths
// (or one of them), the query always text, a symlink never followed, and a
// token caller refused.
func TestSearchRoute(t *testing.T) {
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
	for _, m := range dm.Modules() {
		if s, ok := m.(*search.Service); ok {
			s.Searcher = search.Walk{} // the same answers as ripgrep (conformance); no rg needed here
		}
	}
	owner := client.WithHandler(dm.SocketHandler())
	ctx := context.Background()

	base, _ := filepath.EvalSymlinks(t.TempDir())
	app, docs, outside := filepath.Join(base, "app"), filepath.Join(base, "docs"), filepath.Join(base, "outside")
	for p, body := range map[string]string{
		filepath.Join(app, "main.go"):        "// needle in app\n-- --version needle\n",
		filepath.Join(docs, "readme.md"):     "needle in docs\n",
		filepath.Join(outside, "secret.txt"): "needle outside\n",
	} {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(app, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{app}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AddArea(ctx, "acme", api.Area{Name: "docs", RepoRules: api.RepoRules{Paths: []string{docs}}}); err != nil {
		t.Fatal(err)
	}
	get := func(c *client.Client, v url.Values) (orchapi.SearchResult, error) {
		var res orchapi.SearchResult
		err := c.Do(ctx, http.MethodGet, "/v0/search?"+v.Encode(), nil, &res)
		return res, err
	}
	// An area path inside the project's: searched once, not twice.
	inner := filepath.Join(app, "inner")
	_ = os.MkdirAll(inner, 0o755)
	if err := os.WriteFile(filepath.Join(inner, "i.txt"), []byte("needle inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AddArea(ctx, "acme", api.Area{Name: "inner", RepoRules: api.RepoRules{Paths: []string{inner}}}); err != nil {
		t.Fatal(err)
	}
	res, err := get(owner, url.Values{"project": {"acme"}, "q": {"needle"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Matches {
		got = append(got, filepath.Base(m.File))
		if strings.Contains(m.Text, "outside") {
			t.Fatalf("followed a symlink out of the project: %+v", m)
		}
	}
	if strings.Join(got, ",") != "i.txt,main.go,main.go,readme.md" || len(res.Roots) != 2 || res.Engine != "walk" {
		t.Fatalf("matches %v roots %v engine %q", got, res.Roots, res.Engine)
	}
	// path: one of the project's paths only.
	res, err = get(owner, url.Values{"project": {"acme"}, "q": {"needle"}, "path": {docs}})
	if err != nil || len(res.Matches) != 1 || len(res.Roots) != 1 {
		t.Fatalf("path=docs: %+v %v", res, err)
	}
	if _, err := get(owner, url.Values{"project": {"acme"}, "q": {"needle"}, "path": {outside}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a path outside the project: %v", err)
	}
	// An option-shaped query is text.
	res, err = get(owner, url.Values{"project": {"acme"}, "q": {"-- --version"}})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].Line != 2 {
		t.Fatalf("q=-- --version: %+v %v", res.Matches, err)
	}
	for _, bad := range []url.Values{
		{"project": {"acme"}},
		{"project": {"acme"}, "q": {strings.Repeat("x", orchapi.SearchMaxQuery+1)}},
		{"project": {"acme"}, "q": {"a(b"}, "regex": {"1"}},
		{"project": {"acme"}, "q": {"a"}, "regex": {"yes"}},
		{"q": {"needle"}},
	} {
		if _, err := get(owner, bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := get(owner, url.Values{"project": {"nope"}, "q": {"needle"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("no such project: %v", err)
	}
	// Not the owner: a token caller (even the project's owner by grant) is refused.
	m, err := owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "acme", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	tc := client.WithHandler(dm.SocketHandler())
	tc.Token = m.Secret
	if _, err := get(tc, url.Values{"project": {"acme"}, "q": {"needle"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a token caller: %v", err)
	}
}

// A glob path rule searches the directories it matches now, and path= may
// name one of them.
func TestSearchGlobPaths(t *testing.T) {
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
	for _, m := range dm.Modules() {
		if s, ok := m.(*search.Service); ok {
			s.Searcher = search.Walk{}
		}
	}
	owner := client.WithHandler(dm.SocketHandler())
	ctx := context.Background()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"svc-a", "svc-b", "other"} {
		_ = os.MkdirAll(filepath.Join(base, d), 0o755)
		if err := os.WriteFile(filepath.Join(base, d, "x.txt"), []byte("needle in "+d+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{filepath.Join(base, "svc-*")}}}); err != nil {
		t.Fatal(err)
	}
	var res orchapi.SearchResult
	if err := owner.Do(ctx, http.MethodGet, "/v0/search?"+url.Values{"project": {"acme"}, "q": {"needle"}}.Encode(), nil, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Roots) != 2 || len(res.Matches) != 2 {
		t.Fatalf("glob roots %v matches %+v", res.Roots, res.Matches)
	}
	one := filepath.Join(base, "svc-b")
	if err := owner.Do(ctx, http.MethodGet, "/v0/search?"+url.Values{"project": {"acme"}, "q": {"needle"}, "path": {one}}.Encode(), nil, &res); err != nil ||
		len(res.Matches) != 1 || !strings.Contains(res.Matches[0].Text, "svc-b") {
		t.Fatalf("path= a glob's match: %+v %v", res.Matches, err)
	}
	if err := owner.Do(ctx, http.MethodGet, "/v0/search?"+url.Values{"project": {"acme"}, "q": {"needle"}, "path": {filepath.Join(base, "other")}}.Encode(), nil, &res); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("path= outside the glob: %v", err)
	}
}
