// Package repoguard fails a test binary that changes the git repository it
// runs in. A package's tests run with the package directory as the working
// directory, so a git command with no Dir of its own (or a test that forgot to
// make a temp repo) lands in the checkout holding the source, and every
// worktree of that checkout shares its .git/config: one stray `remote set-url`
// rewrote the shared origin for every lane (zm-jmep). Tests never write a real
// repo (AGENTS.md Never). A package opts in from its TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(repoguard.Main(m)) }
package repoguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Main runs the package's tests and fails the run when the repository around
// the working directory has a different config afterwards than before.
func Main(m *testing.M) int {
	wd, err := os.Getwd()
	if err != nil {
		return m.Run()
	}
	check := Watch(wd)
	code := m.Run()
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Watch records the config files of the repository enclosing dir (the shared
// config of every worktree, and this worktree's own) and returns a check that
// reports what changed since. Outside a repository the check always passes.
func Watch(dir string) func() error {
	files := configFiles(dir)
	before := read(files)
	return func() error {
		after := read(files)
		for _, f := range files {
			if before[f] != after[f] {
				return fmt.Errorf("a test changed the git config of the repository it runs in (%s): tests use a temp repo and never write a real one;\nwas:\n%s\nnow:\n%s", f, before[f], after[f])
			}
		}
		return nil
	}
}

// configFiles are the repository's shared config and the worktree's own, or
// nil when dir is not in a repository (or git is missing).
func configFiles(dir string) []string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, d := range strings.Fields(string(out)) {
		files = append(files, filepath.Join(d, "config"), filepath.Join(d, "config.worktree"))
	}
	return dedupe(files)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func read(files []string) map[string]string {
	m := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			m[f] = "(absent)"
			continue
		}
		m[f] = string(b)
	}
	return m
}
