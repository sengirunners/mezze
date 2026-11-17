package ghlink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// replicas is a team module that holds replicas of the projects in it.
type replicas map[string]bool

func (replicas) Proxy(http.ResponseWriter, *http.Request, http.Handler) bool { return false }
func (r replicas) Replicates(p string) bool                                  { return r[p] }

type moved struct {
	s     *Service
	team  replicas
	reg   *project.Registry
	owner *authz.Subject
	p     api.Project
	gh    string
	dir   string
}

func (m *moved) calls() string {
	b, _ := os.ReadFile(filepath.Join(m.dir, "calls"))
	return string(b)
}

func (m *moved) put(t *testing.T) (orchapi.GitHubLinkResponse, error) {
	t.Helper()
	body, _ := json.Marshal(orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"a/b"}, Bin: m.gh, TokenFile: filepath.Join(m.dir, "token"), PollSeconds: 0})
	r := httptest.NewRequest("POST", "/v0/github/links", bytes.NewReader(body))
	v, err := m.s.handlePut(r, m.owner)
	if err != nil {
		return orchapi.GitHubLinkResponse{}, err
	}
	return v.(orchapi.GitHubLinkResponse), nil
}

func (m *moved) tokens(t *testing.T) int {
	t.Helper()
	var n int
	if err := m.s.db().QueryRow(`SELECT count(*) FROM tokens WHERE revoked IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newMoved(t *testing.T) *moved {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "ivo", Machine: "mini"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := reg.Local("")
	p, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "harbor"})
	if err != nil {
		t.Fatal(err)
	}
	m := &moved{team: replicas{}, reg: reg, owner: owner, p: p, dir: t.TempDir()}
	m.s = New(&core.Deps{Config: cfg, Store: st, Projects: reg, Team: m.team, Log: slog.Default()})
	m.s.MinPoll = time.Millisecond
	m.gh = filepath.Join(m.dir, "gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"GH_TOKEN=$GH_TOKEN $*\" >> " + m.dir + "/calls\necho '{}'\n"
	if err := os.WriteFile(m.gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "token"), []byte("ghp_stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

// The real move: a project this machine hosted and linked to GitHub
// moves to another authority. This machine renames its old copy (it stays in
// the registry, same ULID) and joins the new authority, so team remotes hold
// a replica of that ULID. The old link then supplies no gh binary and no
// GH_TOKEN, does not poll or apply, keeps held decisions until it is swept,
// and is swept (row, waits and token) at the next list, read or poller turn,
// so leaving the replica later cannot bring it back.
func TestLinkOfMovedProjectIsSweptAndStaysGone(t *testing.T) {
	ctx := context.Background()
	m := newMoved(t)
	s := m.s
	resp, err := m.put(t)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.link(ctx, m.p.ID)
	if err != nil || !s.tokenLive(ctx, m.p.ID, l.TokenID) || resp.Link.Project == "" {
		t.Fatalf("linked: %v live=%v", err, s.tokenLive(ctx, m.p.ID, l.TokenID))
	}
	pr := PR{Repo: "a/b", Number: 1, State: StateOpen}
	raw, _ := json.Marshal(pr)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO ghlink_waits (project, repo, number, item, state, pr, first, next_at) VALUES (?, 'a/b', 1, 'x', 'open', ?, 1, 1)`, m.p.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.browseFetch(ctx, m.p.ID, "api", "x"); err != nil || !strings.Contains(m.calls(), "GH_TOKEN=ghp_stale") {
		t.Fatalf("hosted link: %v %q", err, m.calls())
	}
	if err := os.Remove(filepath.Join(m.dir, "calls")); err != nil {
		t.Fatal(err)
	}

	// The move: the replica exists, the registry still has the project.
	m.team[m.p.ID] = true
	if _, ok := m.reg.Lookup(m.p.ID); !ok {
		t.Fatal("the registry dropped the project (it never does)")
	}
	if _, err := s.link(ctx, m.p.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("link of a moved project: %v, want not found", err)
	}
	// Apply and the gate recheck do nothing and delete nothing.
	if n, err := s.apply(ctx, m.p.ID, pr); n != 0 || err != nil {
		t.Fatalf("apply: %d %v", n, err)
	}
	if n := s.recheckOne(ctx, m.p.ID, pr, "x", string(raw)); n != 0 {
		t.Fatalf("recheck: %d", n)
	}
	var waits int
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM ghlink_waits WHERE project = ?`, m.p.ID).Scan(&waits); err != nil || waits != 1 {
		t.Fatalf("recheck dropped a held decision: %d %v", waits, err)
	}
	// A poller turn finds the link gone, calls no gh, and sweeps.
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.startPoller(link{Project: m.p.ID, PollSeconds: 1})
	s.wg.Wait()
	s.cancel()
	if _, err := s.linkRow(ctx, m.p.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the moved project's row was not swept: %v", err)
	}
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM ghlink_waits WHERE project = ?`, m.p.ID).Scan(&waits); err != nil || waits != 0 {
		t.Fatalf("swept link kept its waits: %d %v", waits, err)
	}
	if s.tokenLive(ctx, m.p.ID, l.TokenID) {
		t.Fatal("the linker's token outlived the sweep")
	}
	if _, err := os.Stat(s.tokenPath(m.p.ID)); !os.IsNotExist(err) {
		t.Fatalf("the linker's token file stayed: %v", err)
	}

	// The replica goes away (zm leave, or a move back): nothing revives.
	delete(m.team, m.p.ID)
	t.Setenv("PATH", t.TempDir()) // no gh on PATH
	_, _ = s.browseFetch(ctx, m.p.ID, "api", "x")
	if ls, _ := s.links(ctx); len(ls) != 0 {
		t.Fatalf("a swept link came back: %v", ls)
	}
	if c := m.calls(); c != "" {
		t.Fatalf("the moved project's link supplied gh: %s", c)
	}
}

// A put for a project this machine no longer hosts is refused (no token is
// minted for a link that does nothing), and it also finds nothing to repair.
func TestPutRefusesProjectNotHostedHere(t *testing.T) {
	m := newMoved(t)
	m.team[m.p.ID] = true
	before := m.tokens(t)
	if _, err := m.put(t); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("put on a replicated project: %v, want conflict", err)
	}
	if after := m.tokens(t); after != before {
		t.Fatalf("a refused put minted a token: %d -> %d", before, after)
	}
	var n int
	if err := m.s.db().QueryRow(`SELECT count(*) FROM ghlink_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused put wrote a row: %d %v", n, err)
	}
	// Delete never turns a non-ULID ref into a row lookup.
	r := httptest.NewRequest("DELETE", "/v0/github/links/nope", nil)
	r.SetPathValue("project", "nope")
	if _, err := m.s.handleDelete(r, m.owner); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("delete of an unknown ref: %v", err)
	}
}

// A moved project's webhook secret verifies nothing: 401 like any unknown.
func TestHookOfMovedProjectIs401(t *testing.T) {
	m := newMoved(t)
	ctx := context.Background()
	if _, err := m.s.db().ExecContext(ctx, `INSERT INTO ghlink_links (project, repos, on_open, on_merge, on_close, secret, created)
		VALUES (?, '["a/b"]', 'none', 'none', 'none', 's3cret', 1)`, m.p.ID); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"opened","repository":{"full_name":"a/b"},"pull_request":{"number":1}}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	post := func() int {
		r := httptest.NewRequest("POST", ReceiverPath, bytes.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", sig)
		r.Header.Set("X-GitHub-Event", "pull_request")
		w := httptest.NewRecorder()
		m.s.handleHook(w, r)
		return w.Code
	}
	m.team[m.p.ID] = true
	if code := post(); code != http.StatusUnauthorized {
		t.Fatalf("hook for a moved project: %d, want 401", code)
	}
}
