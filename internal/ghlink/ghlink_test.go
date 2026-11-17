package ghlink_test

// Acceptance tests for GitHub linking: the signed receiver (bad and unknown
// signatures, redelivery and replay of older snapshots), the transitions
// (opened, merged, closed unmerged, an item someone holds), polling through
// a fake gh with an allowlisted environment, and the linker's token being
// capped to its project. Nothing calls the real GitHub API or a real gh.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink"
	"github.com/sengirunners/mezze/internal/ghlink/gate/gatetest"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

func TestMain(m *testing.M) {
	if code, ok := gatetest.Main(); ok {
		os.Exit(code)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

type gx struct {
	t      *testing.T
	home   string
	dm     *server.Daemon
	svc    *ghlink.Service
	owner  *client.Client
	net    *httptest.Server // the daemon's handler on an unmarked listener (GitHub's view)
	secret string
}

func newGX(t *testing.T) *gx {
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
	g := &gx{t: t, home: home, dm: dm, owner: client.WithHandler(dm.SocketHandler())}
	for _, m := range dm.Modules() {
		if s, ok := m.(*ghlink.Service); ok {
			g.svc = s
		}
	}
	g.svc.Retry = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	if err := g.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	g.net = httptest.NewServer(dm.Handler)
	t.Cleanup(func() {
		g.net.Close()
		cancel()
		dm.Close()
	})
	for _, p := range []string{"harbor", "side"} {
		if _, err := g.owner.CreateProject(ctx, api.NewProjectRequest{Slug: p}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *gx) at(project string) *client.Client {
	c := client.WithHandler(g.dm.SocketHandler())
	c.Project, c.ProjectFrom = project, "flag"
	return c
}

func (g *gx) item(project, title string) orchapi.WorkItem {
	g.t.Helper()
	it, err := orchclient.CreateWork(context.Background(), g.at(project), orchapi.CreateWorkRequest{Title: title})
	if err != nil {
		g.t.Fatal(err)
	}
	return it
}

func (g *gx) get(project, id string) orchapi.WorkItem {
	g.t.Helper()
	it, err := orchclient.GetWork(context.Background(), g.at(project), id)
	if err != nil {
		g.t.Fatal(err)
	}
	return it
}

func (g *gx) link(req orchapi.GitHubLinkRequest) orchapi.GitHubLinkResponse {
	g.t.Helper()
	if req.Project == "" {
		req.Project = "harbor"
	}
	if req.Repos == nil {
		req.Repos = []string{"example-org/harbor"}
	}
	r, err := orchclient.LinkGitHub(context.Background(), g.owner, req)
	if err != nil {
		g.t.Fatal(err)
	}
	if r.Secret != "" {
		g.secret = r.Secret
	}
	return r
}

type pull struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at,omitempty"`
	HTMLURL  string  `json:"html_url"`
	Updated  string  `json:"updated_at"`
	Draft    bool    `json:"draft"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha,omitempty"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login,omitempty"`
	} `json:"user"`
}

func mkPull(n int, title, body, branch, state string, merged bool, updated time.Time) pull {
	p := pull{Number: n, Title: title, Body: body, State: state, Merged: merged,
		HTMLURL: fmt.Sprintf("https://github.com/example-org/harbor/pull/%d", n), Updated: updated.UTC().Format(time.RFC3339)}
	p.Head.Ref, p.Base.Ref = branch, "main"
	if merged {
		p.MergedAt = &p.Updated
	}
	return p
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// deliver posts a GitHub delivery to the receiver like GitHub would.
func (g *gx) deliver(event, delivery, secret string, payload any) (int, string) {
	g.t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, g.net.URL+ghlink.ReceiverPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func prEvent(action string, p pull, repo string) map[string]any {
	return map[string]any{"action": action, "pull_request": p, "repository": map[string]string{"full_name": repo}}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (g *gx) comments(project, id string) []orchapi.Comment {
	cs, err := orchclient.Comments(context.Background(), g.at(project), id)
	if err != nil {
		g.t.Fatal(err)
	}
	return cs
}

func (g *gx) settle() {
	g.t.Helper()
	eventually(g.t, "the inbox to drain", func() bool {
		ls, _ := orchclient.GitHubLinks(context.Background(), g.owner)
		for _, l := range ls {
			if l.Pending > 0 {
				return false
			}
		}
		return true
	})
}

// ---- tests ----

func TestMatcher(t *testing.T) {
	m := ghlink.NewMatcher([]string{"harbor"}, []string{"bd"})
	got := m.IDs("ivo/harbor-abc12-fix-thing", "Fix [harbor-Q9x7z] and bd:harbor-zz1.2.", "see local:k3x9qa, gh:x, nope:abc, xharbor-nope")
	want := []string{"bd:harbor-zz1.2", "local:k3x9qa", "bd:harbor-abc12", "bd:harbor-q9x7z"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
	var long strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&long, "harbor-a%d ", i)
	}
	if n := len(m.IDs(long.String())); n != ghlink.MaxIDs {
		t.Fatalf("cap: %d", n)
	}
}

func TestReceiverMovesItems(t *testing.T) {
	g := newGX(t)
	it := g.item("harbor", "fix the solver")
	other := g.item("side", "another project's item")
	res := g.link(orchapi.GitHubLinkRequest{})
	if res.Secret == "" || res.Link.Identity != "human:zm-github" || res.Link.OnOpen != "claimed" {
		t.Fatalf("link: %+v", res)
	}
	t0 := time.Now().Add(-time.Hour)
	opened := mkPull(9, "Fix the solver ["+it.ID+"]", "also touches "+other.ID, "ivo/fix", "open", false, t0)

	// Unsigned, badly signed and unknown-repo deliveries: the same 401.
	st1, b1 := g.deliver("pull_request", "d-0", "", prEvent("opened", opened, "example-org/harbor"))
	st2, b2 := g.deliver("pull_request", "d-0", "wrong-secret-0123456789", prEvent("opened", opened, "example-org/harbor"))
	st3, b3 := g.deliver("pull_request", "d-0", "wrong-secret-0123456789", prEvent("opened", opened, "someone/else"))
	if st1 != 401 || st2 != 401 || st3 != 401 || b1 != b2 || b2 != b3 {
		t.Fatalf("refusals differ: %d %s / %d %s / %d %s", st1, b1, st2, b2, st3, b3)
	}
	if st, _ := g.deliver("ping", "d-p", g.secret, map[string]any{"zen": "hi"}); st != 200 {
		t.Fatalf("ping: %d", st)
	}
	if st, b := g.deliver("issues", "d-i", g.secret, map[string]any{}); st != 202 || !strings.Contains(b, "ignored") {
		t.Fatalf("other event: %d %s", st, b)
	}

	if st, b := g.deliver("pull_request", "d-1", g.secret, prEvent("opened", opened, "example-org/harbor")); st != 202 {
		t.Fatalf("opened: %d %s", st, b)
	}
	eventually(t, "the item in review", func() bool { return g.get("harbor", it.ID).Status == orchapi.WorkInReview })
	// The link and the comment are written after the status: wait for them.
	eventually(t, "the link and the comment", func() bool {
		return len(g.get("harbor", it.ID).Links) >= 1 && len(g.comments("harbor", it.ID)) >= 1
	})
	got := g.get("harbor", it.ID)
	if len(got.Links) != 1 || got.Links[0].Rel != "pr" || got.Links[0].Target != opened.HTMLURL {
		t.Fatalf("links: %+v", got.Links)
	}
	cs := g.comments("harbor", it.ID)
	if len(cs) != 1 || cs[0].Author != "human:zm-github" || !strings.Contains(cs[0].Body, opened.HTMLURL) || !strings.Contains(cs[0].Body, "ivo/fix") {
		t.Fatalf("comments: %+v", cs)
	}
	// The linker's token is capped to harbor: the other project's item named
	// in the body is untouched.
	if o := g.get("side", other.ID); o.Status != orchapi.WorkOpen || len(g.comments("side", other.ID)) != 0 {
		t.Fatalf("other project's item moved: %+v", o)
	}

	// A redelivery is dropped.
	g.deliver("pull_request", "d-1", g.secret, prEvent("opened", opened, "example-org/harbor"))
	g.settle()
	if n := len(g.comments("harbor", it.ID)); n != 1 {
		t.Fatalf("redelivery commented again: %d", n)
	}

	merged := mkPull(9, opened.Title, opened.Body, "ivo/fix", "closed", true, t0.Add(10*time.Minute))
	g.deliver("pull_request", "d-2", g.secret, prEvent("closed", merged, "example-org/harbor"))
	eventually(t, "the item done", func() bool { return g.get("harbor", it.ID).Status == orchapi.WorkDone })
	eventually(t, "the close comment", func() bool { return len(g.comments("harbor", it.ID)) >= 2 })
	cs = g.comments("harbor", it.ID)
	if len(cs) != 2 || !strings.Contains(cs[1].Body, "Closed by merged PR example-org/harbor#9") || !strings.Contains(cs[1].Body, merged.HTMLURL) {
		t.Fatalf("close reason: %+v", cs)
	}

	// A replay of the older "opened" snapshot under a new delivery id
	// changes nothing.
	g.deliver("pull_request", "d-3", g.secret, prEvent("opened", opened, "example-org/harbor"))
	g.settle()
	if g.get("harbor", it.ID).Status != orchapi.WorkDone || len(g.comments("harbor", it.ID)) != 2 {
		t.Fatal("an older replayed snapshot moved the item")
	}
}

func TestClosedUnmergedReopensAndHeldItemsStay(t *testing.T) {
	g := newGX(t)
	a := g.item("harbor", "reverted later")
	b := g.item("harbor", "someone holds it")
	if _, err := orchclient.Claim(context.Background(), g.at("harbor"), b.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	g.link(orchapi.GitHubLinkRequest{})
	t0 := time.Now().Add(-time.Hour)
	p := mkPull(4, "Try "+a.ID, "", "x", "open", false, t0)
	g.deliver("pull_request", "a1", g.secret, prEvent("opened", p, "example-org/harbor"))
	eventually(t, "a in review", func() bool { return g.get("harbor", a.ID).Status == orchapi.WorkInReview })
	p = mkPull(4, "Try "+a.ID, "", "x", "closed", false, t0.Add(time.Minute))
	g.deliver("pull_request", "a2", g.secret, prEvent("closed", p, "example-org/harbor"))
	eventually(t, "a open again", func() bool { return g.get("harbor", a.ID).Status == orchapi.WorkOpen })
	// The comment is written after the status change: wait for it too.
	eventually(t, "a's closed comment", func() bool { return len(g.comments("harbor", a.ID)) >= 2 })
	if cs := g.comments("harbor", a.ID); len(cs) != 2 || !strings.Contains(cs[1].Body, "closed without merging") {
		t.Fatalf("comments: %+v", cs)
	}

	q := mkPull(5, "Work on "+b.ID, "", "y", "open", false, t0)
	g.deliver("pull_request", "b1", g.secret, prEvent("opened", q, "example-org/harbor"))
	eventually(t, "a comment on b", func() bool { return len(g.comments("harbor", b.ID)) == 1 })
	got := g.get("harbor", b.ID)
	if got.Status != orchapi.WorkClaimed || got.Owner == "" || !strings.Contains(g.comments("harbor", b.ID)[0].Body, "held by") {
		t.Fatalf("held item: %+v %+v", got, g.comments("harbor", b.ID))
	}
	q = mkPull(5, "Work on "+b.ID, "", "y", "closed", false, t0.Add(time.Minute))
	g.deliver("pull_request", "b2", g.secret, prEvent("closed", q, "example-org/harbor"))
	eventually(t, "a second comment on b", func() bool { return len(g.comments("harbor", b.ID)) == 2 })
	if got := g.get("harbor", b.ID); got.Status != orchapi.WorkClaimed {
		t.Fatalf("closing reopened a held item: %+v", got)
	}
}

func TestTransitionsConfigurable(t *testing.T) {
	g := newGX(t)
	it := g.item("harbor", "only comments")
	g.link(orchapi.GitHubLinkRequest{OnOpen: "none", OnMerge: "none"})
	t0 := time.Now().Add(-time.Hour)
	g.deliver("pull_request", "c1", g.secret, prEvent("opened", mkPull(1, it.ID, "", "z", "open", false, t0), "example-org/harbor"))
	g.deliver("pull_request", "c2", g.secret, prEvent("closed", mkPull(1, it.ID, "", "z", "closed", true, t0.Add(time.Minute)), "example-org/harbor"))
	eventually(t, "two comments", func() bool { return len(g.comments("harbor", it.ID)) == 2 })
	if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("moved with transitions off: %s", got.Status)
	}
	// The merge note says nothing moved, never "Closed by".
	if cs := g.comments("harbor", it.ID); strings.Contains(cs[1].Body, "Closed by") || !strings.Contains(cs[1].Body, "status left as is") {
		t.Fatalf("merge note with on-merge none: %q", cs[1].Body)
	}
	if _, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"x/y"}, OnOpen: "done"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad transition: %v", err)
	}
	if _, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"../etc"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad repo: %v", err)
	}
}

// writeFakeGH writes a gh stand-in that records its arguments and
// environment and answers page 1 with pulls.json.
func writeFakeGH(t *testing.T, dir string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]s/calls
env | grep -E '^(BEADS_|BD_|DOLT_|ZM_SECRET_PROBE)' >> %[1]s/leaked
printf '%%s\n' "GH_TOKEN=$GH_TOKEN" >> %[1]s/tokens
case "$*" in
  *"/jobs?"*) cat %[1]s/jobs.json 2>/dev/null || echo '{"jobs":[]}' ;;
  *"actions/runs?head_sha="*) cat %[1]s/headruns.json 2>/dev/null || echo '{"workflow_runs":[]}' ;;
  *"actions/runs?"*) cat %[1]s/runs.json 2>/dev/null || echo '{"workflow_runs":[]}' ;;
  *"/pulls/"*) cat %[1]s/pull.json 2>/dev/null || echo '{}' ;;
  *"page=1"*) cat %[1]s/pulls.json ;;
  *) echo '[]' ;;
esac
`, dir)
	p := filepath.Join(dir, "gh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPollingWithFakeGH(t *testing.T) {
	g := newGX(t)
	t.Setenv("BEADS_DIR", "/should/not/leak")
	t.Setenv("ZM_SECRET_PROBE", "nope")
	dir := t.TempDir()
	gh := writeFakeGH(t, dir)
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte("ghp_fake\n"), 0o600)
	fresh := g.item("harbor", "polled")
	stale := g.item("harbor", "too old for the first poll")
	now := time.Now()
	pulls := []pull{
		mkPull(12, "Polled ["+fresh.ID+"]", "", "p", "closed", true, now.Add(-time.Minute)),
		mkPull(11, "Old "+stale.ID, "", "q", "open", false, now.Add(-72*time.Hour)),
	}
	b, _ := json.Marshal(pulls)
	os.WriteFile(filepath.Join(dir, "pulls.json"), b, 0o600)
	g.link(orchapi.GitHubLinkRequest{Bin: gh, TokenFile: tokFile})
	r, err := orchclient.SyncGitHub(context.Background(), g.owner, "harbor")
	if err != nil || len(r.Errors) > 0 {
		t.Fatalf("sync: %+v %v", r, err)
	}
	if r.PRs != 1 { // Applied may be 0: the background worker can drain the inbox first
		t.Fatalf("sync counts: %+v", r)
	}
	eventually(t, "the polled item done", func() bool { return g.get("harbor", fresh.ID).Status == orchapi.WorkDone })
	if got := g.get("harbor", stale.ID); got.Status != orchapi.WorkOpen || len(g.comments("harbor", stale.ID)) != 0 {
		t.Fatal("a PR older than the lookback was applied")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(l, "api --method GET repos/example-org/harbor/pulls?") &&
			!strings.HasPrefix(l, "api --method GET repos/example-org/harbor/actions/runs?status=completed&") {
			t.Fatalf("gh called with %q", l)
		}
	}
	if leaked, _ := os.ReadFile(filepath.Join(dir, "leaked")); len(bytes.TrimSpace(leaked)) > 0 {
		t.Fatalf("environment leaked into gh: %s", leaked)
	}
	if toks, _ := os.ReadFile(filepath.Join(dir, "tokens")); !strings.Contains(string(toks), "GH_TOKEN=ghp_fake") {
		t.Fatalf("GH_TOKEN from the token file: %s", toks)
	}
	// A second sync sees nothing new (the cursor moved; the overlap is a no-op).
	r, _ = orchclient.SyncGitHub(context.Background(), g.owner, "harbor")
	if r.Applied != 0 || len(g.comments("harbor", fresh.ID)) != 1 {
		t.Fatalf("second sync: %+v", r)
	}
}

func TestUnlinkRevokesLinker(t *testing.T) {
	g := newGX(t)
	g.link(orchapi.GitHubLinkRequest{})
	// Re-linking keeps the secret and the token.
	r2 := g.link(orchapi.GitHubLinkRequest{PollSeconds: 0, Prefixes: []string{"harbor"}})
	if r2.Secret != "" {
		t.Fatal("re-link made a new secret")
	}
	toks, _ := g.owner.Tokens(context.Background())
	live := 0
	for _, tk := range toks {
		if tk.Identity == ghlink.Identity && tk.Revoked == nil {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("linker tokens: %d", live)
	}
	// A linker whose grant was revoked by hand is repaired by linking again.
	it := g.item("harbor", "after repair")
	if _, err := g.owner.Revoke(context.Background(), api.GrantRequest{Identity: ghlink.Identity, Role: "member", Project: "harbor"}); err != nil {
		t.Fatal(err)
	}
	g.link(orchapi.GitHubLinkRequest{})
	g.deliver("pull_request", "r1", g.secret, prEvent("opened", mkPull(30, it.ID, "", "r", "open", false, time.Now().Add(-time.Minute)), "example-org/harbor"))
	eventually(t, "the repaired linker's comment", func() bool { return len(g.comments("harbor", it.ID)) == 1 })
	if _, err := orchclient.UnlinkGitHub(context.Background(), g.owner, "harbor"); err != nil {
		t.Fatal(err)
	}
	toks, _ = g.owner.Tokens(context.Background())
	for _, tk := range toks {
		if tk.Identity == ghlink.Identity && tk.Revoked == nil {
			t.Fatal("unlink left the linker's token live")
		}
	}
	if _, err := os.Stat(filepath.Join(g.home, "secrets", "ghlink")); err == nil {
		ents, _ := os.ReadDir(filepath.Join(g.home, "secrets", "ghlink"))
		if len(ents) != 0 {
			t.Fatalf("token file left: %v", ents)
		}
	}
	// Its secret no longer verifies anything.
	if st, _ := g.deliver("ping", "z", g.secret, map[string]any{}); st != 401 {
		t.Fatalf("after unlink: %d", st)
	}
	// Network callers never reach the link routes.
	tok, _ := g.owner.MintToken(context.Background(), api.MintTokenRequest{As: "bea", Project: "harbor", Role: "owner"})
	c := client.WithHandler(g.dm.Handler)
	c.Token = tok.Secret
	if _, err := orchclient.GitHubLinks(context.Background(), c); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("remote owner listed links: %v", err)
	}
}

// TestRelinkAfterRestoreMintsASecret: off-site sets carry no link
// secret, so a restored link has none and answers no delivery; linking it
// again without --secret mints and prints a fresh one rather than keeping
// the blank.
func TestRelinkAfterRestoreMintsASecret(t *testing.T) {
	g := newGX(t)
	first := g.link(orchapi.GitHubLinkRequest{})
	if first.Secret == "" {
		t.Fatal("no secret at first link")
	}
	ctx := context.Background()
	reg := g.dm.Deps.Store.Registry()
	tx, err := reg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range g.dm.Deps.BackupScrubbers() {
		if err := f(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	again := g.link(orchapi.GitHubLinkRequest{})
	if again.Secret == "" || again.Secret == first.Secret {
		t.Fatalf("relink after a restore: secret %q (first %q)", again.Secret, first.Secret)
	}
	if kept := g.link(orchapi.GitHubLinkRequest{}); kept.Secret != "" {
		t.Fatal("a link with a secret printed a new one")
	}
}
