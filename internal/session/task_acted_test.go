package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A remote task's push (SPEC 8 "Waiting line"): its worktree branch has a
// commit, made after zm created the branch, that a remote-tracking ref
// reaches. The branch's base already on the remote, or a commit not pushed,
// is no push.
func TestBranchPushed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run := func(in string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = in
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	origin, repo, wt := filepath.Join(dir, "origin.git"), filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	run(dir, "init", "-q", "--bare", origin)
	run(dir, "init", "-q", repo)
	run(repo, "commit", "-q", "--allow-empty", "-m", "base")
	run(repo, "remote", "add", "origin", origin)
	run(repo, "push", "-q", "origin", "main")
	run(repo, "worktree", "add", "-q", "-b", "zm/abcd1234", wt, "main")
	if branchPushed(ctx, wt, "zm/abcd1234") {
		t.Fatal("a new branch at a pushed base counts as a push")
	}
	run(wt, "commit", "-q", "--allow-empty", "-m", "work")
	if branchPushed(ctx, wt, "zm/abcd1234") {
		t.Fatal("an unpushed commit counts as a push")
	}
	run(wt, "push", "-q", "origin", "HEAD:refs/heads/elsewhere")
	if !branchPushed(ctx, wt, "zm/abcd1234") {
		t.Fatal("a pushed commit (under another name) is not a push")
	}
	if branchPushed(ctx, wt, "zm/nope") || branchPushed(ctx, "", "zm/abcd1234") {
		t.Fatal("no branch or no worktree counts as a push")
	}
}
