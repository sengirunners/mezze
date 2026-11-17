package session

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestCheckRun(t *testing.T) {
	yes := true
	ok := orchapi.TaskRun{Ref: "main", Script: "scripts/test.sh", Args: []string{"a b", "$HOME", "--x"}}
	req := func(r orchapi.TaskRun, mut ...func(*orchapi.TaskStartRequest)) orchapi.TaskStartRequest {
		q := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Run: &r}
		for _, m := range mut {
			m(&q)
		}
		return q
	}
	if err := checkRun(req(ok)); err != nil {
		t.Fatalf("good run refused: %v", err)
	}
	tag := ok
	tag.Ref, tag.Commit = "release/1.4", strings.Repeat("a", 40)
	if err := checkRun(req(tag)); err != nil {
		t.Fatalf("tag with commit refused: %v", err)
	}
	if err := checkRun(req(orchapi.TaskRun{Ref: "main", Commit: strings.Repeat("b", 40), Command: `make test && echo "$HOME"`})); err != nil {
		t.Fatalf("command run refused: %v", err)
	}
	bad := map[string]orchapi.TaskStartRequest{
		"no run":        {Agent: orchapi.TaskAgentRun},
		"prompt":        req(ok, func(q *orchapi.TaskStartRequest) { q.Prompt = "hi" }),
		"model":         req(ok, func(q *orchapi.TaskStartRequest) { q.Model = "x" }),
		"worktree":      req(ok, func(q *orchapi.TaskStartRequest) { q.Worktree = &yes }),
		"continue_from": req(ok, func(q *orchapi.TaskStartRequest) { q.ContinueFrom = "abc" }),
	}
	for name, r := range map[string]orchapi.TaskRun{
		"ref option":     {Ref: "-uhack", Script: "a.sh"},
		"ref dotdot":     {Ref: "a..b", Script: "a.sh"},
		"ref absolute":   {Ref: "/main", Script: "a.sh"},
		"ref full name":  {Ref: "refs/heads/main", Script: "a.sh"},
		"ref full tag":   {Ref: "refs/tags/v1", Script: "a.sh"},
		"on ref alone":   {Ref: "main", Script: "a.sh", CommitOnRef: true},
		"long commit":    {Ref: "main", Commit: strings.Repeat("c", 64), Script: "a.sh"},
		"ref glob":       {Ref: "ma*", Script: "a.sh"},
		"ref reflog":     {Ref: "main@{1}", Script: "a.sh"},
		"ref empty":      {Ref: "", Script: "a.sh"},
		"short commit":   {Ref: "main", Commit: "abc123", Script: "a.sh"},
		"script abs":     {Ref: "main", Script: "/bin/sh"},
		"script up":      {Ref: "main", Script: "../x.sh"},
		"script unclean": {Ref: "main", Script: "scripts/../../x.sh"},
		"script option":  {Ref: "main", Script: "-c"},
		"script dot":     {Ref: "main", Script: "."},
		"script empty":   {Ref: "main"},
		"arg nul":        {Ref: "main", Script: "a.sh", Args: []string{"a\x00b"}},
		"too many args":  {Ref: "main", Script: "a.sh", Args: make([]string, runArgsMax+1)},
		"command+script": {Ref: "main", Script: "a.sh", Command: "ls"},
		"command+args":   {Ref: "main", Args: []string{"x"}, Command: "ls"},
		"command lines":  {Ref: "main", Command: "ls\nrm -rf x"},
		"command cr":     {Ref: "main", Command: "ls\rx"},
		"command nul":    {Ref: "main", Command: "ls\x00"},
		"command long":   {Ref: "main", Command: strings.Repeat("x", runCommandMax+1)},
		"command ref":    {Ref: "-uhack", Command: "ls"},
	} {
		bad[name] = req(r)
	}
	for name, r := range bad {
		if err := checkRun(r); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRunOutcome(t *testing.T) {
	code := func(n int) *int { return &n }
	for _, c := range []struct {
		name, failure string
		process       *orchapi.Outcome
		kind          orchapi.OutcomeKind
		exit          *int
	}{
		{"exit 0", "", &orchapi.Outcome{ExitCode: code(0)}, orchapi.OutcomeOK, code(0)},
		{"exit 3", "", &orchapi.Outcome{ExitCode: code(3)}, orchapi.OutcomeFailed, code(3)},
		{"deadline", "overall deadline exceeded", &orchapi.Outcome{Signal: "TERM"}, orchapi.OutcomeKilled, code(124)},
		{"cancel", "cancelled", &orchapi.Outcome{Signal: "TERM"}, orchapi.OutcomeKilled, nil},
		{"signal", "", &orchapi.Outcome{Signal: "KILL"}, orchapi.OutcomeKilled, nil},
		{"output overflow with exit 0", "task output overflow", &orchapi.Outcome{ExitCode: code(0)}, orchapi.OutcomeFailed, code(0)},
		{"may still be running", "task descendant may still be running", &orchapi.Outcome{ExitCode: code(0)}, orchapi.OutcomeFailed, code(0)},
		{"no status", "", nil, orchapi.OutcomeFailed, nil},
	} {
		o := runOutcome(c.failure, c.process)
		if o.Kind != c.kind || (c.exit == nil) != (o.ExitCode == nil) || c.exit != nil && *c.exit != *o.ExitCode {
			t.Errorf("%s: %+v", c.name, o)
		}
	}
}

func TestRunMessagesKeepTheTail(t *testing.T) {
	big := []byte(strings.Repeat("x", runOutputMax) + "the end\n")
	m := runMessages(big)
	if len(m) != 1 || m[0].ID != orchapi.TaskRunOutput || !strings.HasSuffix(m[0].Text, "the end\n") || !strings.HasPrefix(m[0].Text, "[zm: the first 8 bytes") {
		t.Fatalf("tail: %d items, %q...", len(m), m[0].Text[:60])
	}
	if small := runMessages([]byte("hi\n")); small[0].Text != "hi\n" {
		t.Fatalf("small output: %q", small[0].Text)
	}
}

// The sweep never runs while a script-run start is between making its
// checkout and writing its task row: it would see the checkout
// with no running task and remove it under the script.
func TestSweepWaitsForStartsInProgress(t *testing.T) {
	home := t.TempDir()
	s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}} // no store: the sweep must not reach it
	dir := filepath.Join(s.runsDir(), "rt-starting")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runStarts.RLock() // a start making its checkout
	s.sweepRunCheckouts(context.Background())
	runStarts.RUnlock()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the sweep removed a checkout whose start was in progress: %v", err)
	}
}

// A machine where no session of the project ever ran (a farm box) runs a
// script from a bare mirror of the project's listed remote, cloned once: no
// checkout is needed. A project with no remote is refused.
func TestRunFromMirrorWithoutCheckout(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := runGitT(ctx, time.Minute, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	git(remote, "config", "uploadpack.allowFilter", "true") // a partial clone, as from a forge
	git(remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	seed := filepath.Join(root, "seed")
	git(root, "clone", "--quiet", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "go.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", ".")
	git(seed, "commit", "--quiet", "-m", "go")
	git(seed, "push", "--quiet", "origin", "main")
	pushed := git(seed, "rev-parse", "HEAD")

	old := runMirrorURL
	runMirrorURL = func(r string) string {
		if r != "example.com/acme/app" {
			t.Errorf("mirror of %q", r)
		}
		return remote
	}
	t.Cleanup(func() { runMirrorURL = old })

	bare, err := h.d.Projects.Create(ctx, h.owner(), api.NewProjectRequest{Slug: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.prepareRun(ctx, bare.ID, "rt-none", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no checkout and no remote: %v", err)
	}
	// A joined team project: the remote comes from its authority's rules.
	h.d.Team = joinedRepos{{Project: bare.Ref(), Repos: api.RepoRules{Remotes: []string{"example.com/acme/app"}}}}
	if _, dir, _, err := h.s.prepareRun(ctx, bare.ID, "rt-joined", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); err != nil {
		t.Fatalf("a joined project's authority remote: %v", err)
	} else {
		removeRunCheckout(ctx, h.s.runsDir(), dir)
	}
	h.d.Team = nil
	if err := os.RemoveAll(h.s.runMirrorsDir()); err != nil { // the rest starts with no mirror
		t.Fatal(err)
	}
	p, err := h.d.Projects.Create(ctx, h.owner(), api.NewProjectRequest{Slug: "app", RepoRules: api.RepoRules{Remotes: []string{"example.com/acme/app"}}})
	if err != nil {
		t.Fatal(err)
	}
	// A session of the project here sits in a repository whose origin is
	// not the project's remote (a farm worktree with none, a session filed
	// under the project from elsewhere): the run uses the mirror, not it.
	stray := filepath.Join(root, "stray")
	git(root, "init", "--quiet", "--initial-branch=main", stray)
	if _, err := h.d.Projects.Update(ctx, h.owner(), p.ID, api.UpdateProjectRequest{Repos: &api.RepoRules{Paths: []string{stray}, Remotes: []string{"example.com/acme/app"}}}); err != nil {
		t.Fatal(err)
	}
	h.spawn("shell", "exec sleep 30", func(req *orchapi.SpawnRequest) { req.Cwd = stray })
	if got := h.s.projectCheckout(ctx, p.ID); got == "" {
		t.Fatal("the stray session is not the project's checkout here: the case is not set up")
	}
	if _, dir, commit, err := h.s.prepareRun(ctx, p.ID, "rt-stray", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); err != nil || commit != pushed {
		t.Fatalf("a checkout with no origin: %v %s", err, commit)
	} else {
		removeRunCheckout(ctx, h.s.runsDir(), dir)
	}
	git(stray, "remote", "add", "origin", "https://example.com/other/repo.git")
	if _, dir, commit, err := h.s.prepareRun(ctx, p.ID, "rt-other", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); err != nil || commit != pushed {
		t.Fatalf("a checkout of another repository: %v %s", err, commit)
	} else {
		removeRunCheckout(ctx, h.s.runsDir(), dir)
	}

	mirror := filepath.Join(h.s.runMirrorsDir(), "example.com@acme@app.git")
	marker := filepath.Join(mirror, "zm-test-marker") // gone if the mirror is cloned again
	for i, op := range []string{"rt-one", "rt-two"} {
		cmd, dir, commit, err := h.s.prepareRun(ctx, p.ID, op, orchapi.TaskRun{Ref: "main", Script: "go.sh", Commit: pushed})
		if err != nil {
			t.Fatal(err)
		}
		if commit != pushed || len(cmd) == 0 || !strings.HasPrefix(dir, h.s.runsDir()) {
			t.Fatalf("run %s: %v %s %s", op, cmd, dir, commit)
		}
		if _, err := os.Stat(filepath.Join(dir, "go.sh")); err != nil {
			t.Fatalf("checkout lacks the script: %v", err)
		}
		removeRunCheckout(ctx, h.s.runsDir(), dir)
		if i == 0 {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if _, err := os.Stat(marker); err != nil {
			t.Fatalf("the second run cloned the mirror again: %v", err)
		}
	}
	if got := git(mirror, "config", "remote.origin.promisor"); got != "true" {
		t.Fatalf("mirror is not a partial clone: promisor=%q", got)
	}
	// A damaged mirror (emptied by hand) and a crashed clone's leftover are
	// replaced by a fresh clone, not trusted.
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.s.runMirrorsDir(), ".clone-crashed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, dir, _, err := h.s.prepareRun(ctx, p.ID, "rt-three", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); err != nil {
		t.Fatalf("after a damaged mirror: %v", err)
	} else {
		removeRunCheckout(ctx, h.s.runsDir(), dir)
	}
	if _, err := os.Stat(filepath.Join(h.s.runMirrorsDir(), ".clone-crashed")); !os.IsNotExist(err) {
		t.Fatalf("a crashed clone's leftover stayed: %v", err)
	}
	// A mirror that is wrong while a run still uses it is refused, not
	// removed from under that run.
	git(mirror, "remote", "set-url", "origin", "https://example.com/other.git")
	if err := os.MkdirAll(filepath.Join(mirror, "worktrees", "rt-live"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.prepareRun(ctx, p.ID, "rt-four", orchapi.TaskRun{Ref: "main", Script: "go.sh"}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("a wrong mirror in use: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "worktrees", "rt-live")); err != nil {
		t.Fatalf("a mirror in use was removed: %v", err)
	}
	// A cancelled start never removes a mirror.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := mirrorState(cctx, mirror, "https://example.com/other.git"); got != mirrorUnknown && got != mirrorGood {
		t.Fatalf("a cancelled check judged the mirror: %d", got)
	}
}

// A run pinned to a commit on a busy branch (an approved ask's, asked at
// the branch's head): the ask-time check sees the head on the remote and
// refuses what could not run; after the branch moves on, only a run marked
// commit_on_ref takes the commit, and only while it is on the branch; the
// checkout is always exactly that commit, never the newer head, and a
// replace ref on the machine cannot swap its tree.
func TestRunPinOnMovingRef(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := runGitT(ctx, time.Minute, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "--quiet", remote, seed)
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(seed, "what"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("v1\n")
	git(seed, "add", ".")
	git(seed, "commit", "--quiet", "-m", "v1")
	git(seed, "push", "--quiet", "origin", "main")
	v1 := git(seed, "rev-parse", "HEAD")
	old := runMirrorURL
	runMirrorURL = func(string) string { return remote }
	t.Cleanup(func() { runMirrorURL = old })
	p, err := h.d.Projects.Create(ctx, h.owner(), api.NewProjectRequest{Slug: "app", RepoRules: api.RepoRules{Remotes: []string{"example.com/acme/app"}}})
	if err != nil {
		t.Fatal(err)
	}

	// At ask time: the head is checked; anything that could not run is a 400.
	if checked, err := h.s.CheckRunPin(ctx, p.ID, "main", v1); err != nil || !checked {
		t.Fatalf("the head: %v %v", checked, err)
	}
	for name, c := range map[string][2]string{
		"full name":    {"refs/heads/main", v1},
		"no such ref":  {"nope", v1},
		"not the head": {"main", strings.Repeat("a", 40)},
		"short commit": {"main", v1[:12]},
	} {
		if checked, err := h.s.CheckRunPin(ctx, p.ID, c[0], c[1]); checked || !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v %v", name, checked, err)
		}
	}
	none, err := h.d.Projects.Create(ctx, h.owner(), api.NewProjectRequest{Slug: "loose"})
	if err != nil {
		t.Fatal(err)
	}
	if checked, err := h.s.CheckRunPin(ctx, none.ID, "main", v1); checked || err != nil {
		t.Fatalf("no way to reach a remote is unchecked, not refused: %v %v", checked, err)
	}

	// The branch moves on.
	write("v2\n")
	git(seed, "commit", "--quiet", "-am", "v2")
	git(seed, "push", "--quiet", "origin", "main")
	run := func(op string, r orchapi.TaskRun) (string, error) {
		t.Helper()
		r.Ref, r.Command = "main", "true"
		_, dir, commit, err := h.s.prepareRun(ctx, p.ID, op, r)
		if err != nil {
			return "", err
		}
		defer removeRunCheckout(ctx, h.s.runsDir(), dir)
		b, rerr := os.ReadFile(filepath.Join(dir, "what"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if head := git(dir, "rev-parse", "HEAD"); head != commit {
			t.Fatalf("%s: checkout at %s, answered %s", op, head, commit)
		}
		return commit + " " + strings.TrimSpace(string(b)), nil
	}
	if _, err := run("rt-strict", orchapi.TaskRun{Commit: v1}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a moved ref without commit_on_ref: %v", err)
	}
	if got, err := run("rt-onref", orchapi.TaskRun{Commit: v1, CommitOnRef: true}); err != nil || got != v1+" v1" {
		t.Fatalf("a checked pin after the ref moved: %q %v (want exactly v1, never the newer head)", got, err)
	}
	// Only an approved ask's start may carry commit_on_ref.
	h.d.Config.File.RemoteTasks = true
	direct := orchapi.TaskStartOp{Parent: "far/aaaa0000", Depth: 1, Request: orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun,
		Run: &orchapi.TaskRun{Ref: "main", Commit: v1, CommitOnRef: true, Command: "true"}}}
	if _, err := h.s.StartRemoteTask(ctx, p.ID, "rt-direct", direct); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "commit_on_ref") {
		t.Fatalf("commit_on_ref on a start that is not an ask's: %v", err)
	}

	// A commit that is not on the branch is refused, with commit_on_ref too.
	git(seed, "checkout", "--quiet", "-b", "side", v1)
	write("side\n")
	git(seed, "commit", "--quiet", "-am", "side")
	git(seed, "push", "--quiet", "origin", "side")
	side := git(seed, "rev-parse", "HEAD")
	if _, err := run("rt-side", orchapi.TaskRun{Commit: side, CommitOnRef: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a commit off the branch: %v", err)
	}
	// A replace ref on this machine cannot put another tree under v1.
	mirror := filepath.Join(h.s.runMirrorsDir(), "example.com@acme@app.git")
	git(mirror, "fetch", "--quiet", "origin", "side")
	git(mirror, "replace", v1, side)
	if got, err := run("rt-replaced", orchapi.TaskRun{Commit: v1, CommitOnRef: true}); err != nil || got != v1+" v1" {
		t.Fatalf("with a replace ref for the pinned commit: %q %v", got, err)
	}
	// The branch is rewritten without v1: refused.
	git(seed, "checkout", "--quiet", "--orphan", "fresh")
	write("fresh\n")
	git(seed, "add", ".")
	git(seed, "commit", "--quiet", "-m", "fresh")
	git(seed, "push", "--quiet", "--force", "origin", "fresh:main")
	if _, err := run("rt-rewritten", orchapi.TaskRun{Commit: v1, CommitOnRef: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a branch rewritten without the commit: %v", err)
	}
}

// joinedRepos is a team module that only reports joined projects' rules.
type joinedRepos []core.JoinedRepos

func (j joinedRepos) JoinedRepos() []core.JoinedRepos { return j }
func (joinedRepos) Proxy(http.ResponseWriter, *http.Request, http.Handler) bool {
	return false
}

// A checkout serves a run only when its origin is one of the project's
// remotes, in any URL form; with no listed remote, any origin does.
func TestOriginServes(t *testing.T) {
	app := []string{"github.com/acme/app", "github.com/acme/lib"}
	for url, want := range map[string]bool{
		"https://github.com/acme/app.git":    true,
		"git@GitHub.com:acme/App.git":        true,
		"ssh://git@github.com:22/acme/lib":   true,
		"https://token@github.com/acme/app/": true,
		"https://github.com/acme/other.git":  false,
		"https://example.com/acme/app.git":   false,
		"":                                   false,
	} {
		if got := originServes(url, app); got != want {
			t.Errorf("%q: %v, want %v", url, got, want)
		}
	}
	if !originServes("/srv/mirror/app.git", nil) || originServes("", nil) {
		t.Error("a project listing no remote: any origin serves, none does not")
	}
}
