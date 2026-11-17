package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func maintenanceRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	repo, _ := snapshotRepo(t)
	maintenanceWrite(t, filepath.Join(repo, "file.txt"), "a")
	maintenanceGit(t, repo, "add", ".")
	maintenanceGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "file")
	return repo
}

func maintenanceWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
}

func maintenanceGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), repo, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func maintenanceNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository program executed: marker=%s err=%v", marker, err)
	}
}

func TestWorktreeMaintenanceDisablesRepositoryPrograms(t *testing.T) {
	repo := maintenanceRepo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	hooks := filepath.Join(t.TempDir(), "hooks")
	for _, hook := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		maintenanceWrite(t, filepath.Join(hooks, hook), "#!/bin/sh\necho ran >> "+shellQuote(marker)+"\n")
	}
	maintenanceGit(t, repo, "config", "core.hooksPath", hooks)
	maintenanceGit(t, repo, "config", "core.fsmonitor", "echo ran >> "+shellQuote(marker))
	maintenanceGit(t, repo, "config", "core.pager", "echo ran >> "+shellQuote(marker))
	wt, branch, _, err := addWorktreeFrom(context.Background(), t.TempDir(), repo, "abc12345", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "file.txt"))
	if err != nil || string(got) != "a" {
		t.Fatalf("checkout content=%q err=%v", got, err)
	}
	c, ok := gcCandidate(context.Background(), row{Session: orchapi.Session{Address: "mini/abc12345", Worktree: wt, Branch: branch, State: orchapi.StateEnded}}, true)
	if !ok || !c.Safe || c.Dirty {
		t.Fatalf("ordinary clean checkout lost GC safety: %+v", c)
	}
	removeWorktree(context.Background(), wt, branch)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("rollback left worktree: %v", err)
	}
	maintenanceNoMarker(t, marker)
}

func TestWorktreeMaintenanceRejectsBaseFiltersAndRollsBack(t *testing.T) {
	repo := maintenanceRepo(t)
	plain := maintenanceGit(t, repo, "rev-parse", "HEAD")
	maintenanceWrite(t, filepath.Join(repo, ".gitattributes"), "*.txt filter=trap\n")
	maintenanceGit(t, repo, "add", ".gitattributes")
	maintenanceGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "filter")
	filtered := maintenanceGit(t, repo, "rev-parse", "HEAD")
	maintenanceGit(t, repo, "checkout", "--detach", plain)
	marker := filepath.Join(t.TempDir(), "executed")
	maintenanceGit(t, repo, "config", "filter.trap.smudge", "echo ran >> "+shellQuote(marker)+"; cat")
	maintenanceGit(t, repo, "config", "filter.trap.clean", "echo ran >> "+shellQuote(marker)+"; cat")
	maintenanceGit(t, repo, "config", "filter.trap.process", "echo ran >> "+shellQuote(marker)+"; exit 1")
	maintenanceGit(t, repo, "config", "filter.trap.required", "true")
	home := t.TempDir()
	if wt, _, _, err := addWorktreeFrom(context.Background(), home, repo, "abc12345", filtered); err == nil || wt != "" || !strings.Contains(err.Error(), "filter attribute") {
		t.Fatalf("filtered base should refuse before checkout: wt=%q err=%v", wt, err)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "abc12345")); !os.IsNotExist(err) {
		t.Fatalf("partially created worktree remains: %v", err)
	}
	if ref := maintenanceGit(t, repo, "branch", "--list", "zm/abc12345"); ref != "" {
		t.Fatalf("partially created branch remains: %s", ref)
	}
	if head := maintenanceGit(t, repo, "rev-parse", "HEAD"); head != plain {
		t.Fatalf("source HEAD changed: %s", head)
	}
	maintenanceNoMarker(t, marker)
}

func TestWorktreeMaintenanceGCRefusesFalseCleanFilters(t *testing.T) {
	for _, source := range []string{"cached", "working", "global"} {
		t.Run(source, func(t *testing.T) {
			repo := maintenanceRepo(t)
			attrs := "*.txt filter=trap\n"
			switch source {
			case "cached":
				maintenanceWrite(t, filepath.Join(repo, ".gitattributes"), attrs)
				maintenanceGit(t, repo, "add", ".gitattributes")
				maintenanceGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "attrs")
				maintenanceWrite(t, filepath.Join(repo, ".gitattributes"), "*.txt -filter\n")
			case "working":
				maintenanceWrite(t, filepath.Join(repo, ".gitattributes"), attrs)
			case "global":
				path := filepath.Join(t.TempDir(), "attributes")
				maintenanceWrite(t, path, attrs)
				maintenanceWrite(t, filepath.Join(os.Getenv("HOME"), ".gitconfig"), "[core]\n attributesFile = "+path+"\n")
			}
			marker := filepath.Join(t.TempDir(), "executed")
			// Raw index and working bytes are both 'a'. A normal clean filter
			// changes them to 'b'; disabling it must not claim this is clean.
			maintenanceGit(t, repo, "config", "filter.trap.clean", "echo ran >> "+shellQuote(marker)+"; tr a b")
			old := time.Unix(100, 0)
			if err := os.Chtimes(filepath.Join(repo, "file.txt"), old, old); err != nil {
				t.Fatal(err)
			}
			c, ok := gcCandidate(context.Background(), row{Session: orchapi.Session{Worktree: repo, Branch: "main", State: orchapi.StateEnded}}, true)
			if !ok || c.Safe || c.Remove != "" || !strings.Contains(strings.Join(c.Reasons, " "), "filter attribute") {
				t.Fatalf("GC advertised safety with effective %s filters: %+v", source, c)
			}
			maintenanceNoMarker(t, marker)
		})
	}
}

func TestWorktreeMaintenanceIgnoresInheritedGitRedirection(t *testing.T) {
	repo := maintenanceRepo(t)
	other := maintenanceRepo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", "echo ran >> "+shellQuote(marker))
	wt, branch, _, err := addWorktreeFrom(context.Background(), t.TempDir(), repo, "abc12345", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	g, err := newDaemonWorktreeGit(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	root, err := g.run("rev-parse", "--show-toplevel")
	realWT, realErr := filepath.EvalSymlinks(wt)
	if err != nil || realErr != nil || root != realWT {
		t.Fatalf("repository redirected: root=%q want=%q err=%v", root, wt, err)
	}
	removeWorktree(context.Background(), wt, branch)
	maintenanceNoMarker(t, marker)
}

func TestWorktreeMaintenanceRefusesLazyFetch(t *testing.T) {
	repo := maintenanceRepo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	maintenanceGit(t, repo, "config", "remote.origin.promisor", "true")
	maintenanceGit(t, repo, "config", "remote.origin.url", "ext::sh -c 'echo ran >> "+marker+"'")
	maintenanceGit(t, repo, "config", "credential.helper", "!echo ran >> "+shellQuote(marker))
	if _, _, _, err := addWorktreeFrom(context.Background(), t.TempDir(), repo, "abc12345", "HEAD"); err == nil || !strings.Contains(err.Error(), "partial clone") {
		t.Fatalf("partial clone was not refused before object access: %v", err)
	}
	maintenanceNoMarker(t, marker)
}

func TestWorktreeMaintenancePreservesPreexistingData(t *testing.T) {
	repo := maintenanceRepo(t)
	home := t.TempDir()
	path := filepath.Join(home, "worktrees", "abc12345")
	maintenanceWrite(t, filepath.Join(path, "user-data"), "keep")
	if _, _, _, err := addWorktreeFrom(context.Background(), home, repo, "abc12345", "HEAD"); err == nil {
		t.Fatal("preexisting path was accepted")
	}
	if b, err := os.ReadFile(filepath.Join(path, "user-data")); err != nil || string(b) != "keep" {
		t.Fatalf("preexisting data changed: %q %v", b, err)
	}
	maintenanceGit(t, repo, "branch", "zm/def12345")
	if _, _, _, err := addWorktreeFrom(context.Background(), home, repo, "def12345", "HEAD"); err == nil {
		t.Fatal("preexisting branch was accepted")
	}
	if ref := maintenanceGit(t, repo, "branch", "--list", "zm/def12345"); ref == "" {
		t.Fatal("preexisting branch was removed")
	}
}

func TestWorktreeMaintenanceOverallDeadline(t *testing.T) {
	repo := maintenanceRepo(t)
	fifo := filepath.Join(t.TempDir(), "excludes")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	maintenanceGit(t, repo, "config", "core.excludesFile", fifo)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, ok := gcCandidate(ctx, row{Session: orchapi.Session{Worktree: repo, Branch: "main", State: orchapi.StateEnded}}, true)
	if !ok || c.Safe || time.Since(start) > 2*time.Second {
		t.Fatalf("blocked Git was not bounded: candidate=%+v elapsed=%s", c, time.Since(start))
	}
}

func TestWorktreeMaintenanceRollsBackDestinationConfigFailure(t *testing.T) {
	for _, failure := range []string{"partial clone", "blocked include"} {
		t.Run(failure, func(t *testing.T) {
			repo := maintenanceRepo(t)
			realRepo, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			included := filepath.Join(t.TempDir(), "destination-config")
			if failure == "partial clone" {
				maintenanceWrite(t, included, "[remote \"blocked\"]\n promisor = true\n")
			} else if err := syscall.Mkfifo(included, 0600); err != nil {
				t.Fatal(err)
			}
			// A trailing slash matches descendants, not the gitdir itself.
			condition := "includeIf.gitdir:" + filepath.Join(realRepo, ".git", "worktrees", "abc12345") + ".path"
			maintenanceGit(t, repo, "config", condition, included)
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			wt, _, _, err := addWorktreeFrom(ctx, home, repo, "abc12345", "HEAD")
			if err == nil || wt != "" {
				t.Fatalf("destination config failure succeeded: wt=%q err=%v", wt, err)
			}
			if failure == "partial clone" && !strings.Contains(err.Error(), "partial clone") {
				t.Fatalf("destination include did not trigger the intended refusal: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, "worktrees", "abc12345")); !os.IsNotExist(err) {
				t.Fatalf("destination config prevented rollback: %v", err)
			}
			if ref := maintenanceGit(t, repo, "branch", "--list", "zm/abc12345"); ref != "" {
				t.Fatalf("destination config left branch: %s", ref)
			}
		})
	}
}

// Reproduce the on-disk state before Git 2.43's symbolic-ref subprocess
// finishes. The integration FIFO test above reaches this phase on that Git.
func TestWorktreeMaintenanceInitializingRollback(t *testing.T) {
	for _, mutation := range []string{"", "real HEAD", "wrong object format", "different lock", "wrong backlink", "wrong common", "wrong gitdir", "user data", "extra admin file", "metadata symlink", "metadata FIFO", "moved branch", "cancelled"} {
		t.Run(mutation, func(t *testing.T) {
			repo := maintenanceRepo(t)
			g, err := newDaemonWorktreeGit(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			common := maintenanceGit(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
			common, err = filepath.EvalSymlinks(common)
			if err != nil {
				t.Fatal(err)
			}
			wt := filepath.Join(t.TempDir(), "fresh")
			if err := os.Mkdir(wt, 0700); err != nil {
				t.Fatal(err)
			}
			wt, err = filepath.EvalSymlinks(wt)
			if err != nil {
				t.Fatal(err)
			}
			admin := filepath.Join(common, "worktrees", "fresh")
			commit := maintenanceGit(t, repo, "rev-parse", "HEAD")
			branch := "zm/fresh"
			maintenanceGit(t, repo, "branch", branch, commit)
			maintenanceWrite(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
			maintenanceWrite(t, filepath.Join(admin, "gitdir"), filepath.Join(wt, ".git")+"\n")
			maintenanceWrite(t, filepath.Join(admin, "commondir"), "../..\n")
			maintenanceWrite(t, filepath.Join(admin, "HEAD"), strings.Repeat("0", len(commit))+"\n")
			maintenanceWrite(t, filepath.Join(admin, "locked"), "initializing\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mutation {
			case "real HEAD":
				maintenanceWrite(t, filepath.Join(admin, "HEAD"), commit+"\n")
			case "wrong object format":
				maintenanceWrite(t, filepath.Join(admin, "HEAD"), strings.Repeat("0", 64)+"\n")
			case "different lock":
				maintenanceWrite(t, filepath.Join(admin, "locked"), "user locked\n")
			case "wrong backlink":
				maintenanceWrite(t, filepath.Join(wt, ".git"), "gitdir: "+common+"\n")
			case "wrong common":
				maintenanceWrite(t, filepath.Join(admin, "commondir"), "../../other\n")
			case "wrong gitdir":
				maintenanceWrite(t, filepath.Join(admin, "gitdir"), repo+"/.git\n")
			case "user data":
				maintenanceWrite(t, filepath.Join(wt, "keep"), "user data")
			case "extra admin file":
				maintenanceWrite(t, filepath.Join(admin, "config.worktree"), "keep")
			case "metadata symlink", "metadata FIFO":
				path := filepath.Join(admin, "locked")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mutation == "metadata symlink" {
					target := filepath.Join(t.TempDir(), "target")
					maintenanceWrite(t, target, "initializing\n")
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "moved branch":
				maintenanceGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "next")
				maintenanceGit(t, repo, "branch", "-f", branch, "HEAD")
			case "cancelled":
				cancel()
			}
			start := time.Now()
			removed := g.removeInitializing(ctx, wt, admin, common, branch, commit)
			if time.Since(start) > 2*time.Second {
				t.Fatal("initialization metadata blocked rollback")
			}
			if mutation == "" {
				if !removed {
					t.Fatal("proven initialization was preserved")
				}
				for _, path := range []string{wt, admin} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("initialization remains: %s %v", path, err)
					}
				}
				if ref := maintenanceGit(t, repo, "branch", "--list", branch); ref != "" {
					t.Fatalf("created branch remains: %s", ref)
				}
			} else {
				if removed {
					t.Fatalf("unproven initialization removed: %s", mutation)
				}
				for _, path := range []string{filepath.Join(wt, ".git"), admin} {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("unproven data changed: %s %v", path, err)
					}
				}
				if ref := maintenanceGit(t, repo, "branch", "--list", branch); ref == "" {
					t.Fatal("unproven branch removed")
				}
			}
		})
	}
}
