package repoguard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The guard that keeps a test from rewriting a shared checkout's origin (zm-jmep): it passes while the config is
// untouched, names the change when a remote URL is rewritten (also from a worktree, which shares the config),
// and ignores a directory outside any repository.
func TestWatchSeesTheEnclosingRepoConfigChange(t *testing.T) {
	root := t.TempDir()
	repo, wt := root+"/repo", root+"/wt"
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "base")
	git(t, repo, "remote", "add", "origin", "https://example.com/real/repo.git")
	git(t, repo, "worktree", "add", "-q", "-b", "lane", wt)

	check := Watch(wt)
	if err := check(); err != nil {
		t.Fatalf("nothing changed: %v", err)
	}
	git(t, wt, "remote", "set-url", "origin", "https://example.com/a b/c")
	err := check()
	if err == nil || !strings.Contains(err.Error(), "https://example.com/a b/c") {
		t.Fatalf("a rewritten origin from a worktree went unseen: %v", err)
	}

	if err := Watch(t.TempDir())(); err != nil {
		t.Fatalf("outside a repository: %v", err)
	}
}
