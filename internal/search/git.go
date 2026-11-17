package search

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// maxGitList bounds the bytes read of one git listing (a var: a test
// shortens it). 166k files take about 13 MiB.
var maxGitList = 64 << 20

// repoFiles lists the files to search under root, which sits in a git
// repository, as git itself sees them (SPEC 21): so every ignore rule is
// git's, nothing of it copied here. Nested repositories, which git lists as
// one entry, are listed in turn. Hidden paths, symlinks and paths under a
// symlink are left out: the index may name d/x after d became a symlink,
// and nothing outside the searched paths is ever read.
//
// budget bounds git, not the search: a hostile repository can hang it (a
// FIFO for a .gitignore), and the caller then falls back to the matcher in
// what time is left. ok is false when git is missing, refuses, or ran out
// of budget on root itself; cut when the listing is incomplete.
func repoFiles(ctx context.Context, root string, budget time.Duration) (files []string, cut, ok bool) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, false, false
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	known := map[string]bool{root: true} // directories checked: true when real ones
	for dirs := []string{root}; len(dirs) > 0; dirs = dirs[1:] {
		dir := dirs[0]
		names, short, err := gitList(ctx, bin, dir)
		if err != nil {
			if dir == root {
				return nil, false, false
			}
			cut = cut || ctx.Err() != nil // a nested repository git refuses is left out
			continue
		}
		cut = cut || short
		for _, name := range names {
			path := filepath.Join(dir, name)
			if hiddenPath(name) || !realDir(filepath.Dir(path), known) {
				continue
			}
			fi, err := os.Lstat(path)
			switch {
			case err != nil: // tracked, deleted from the working tree
			case fi.Mode().IsRegular():
				files = append(files, path)
			case fi.IsDir(): // a nested repository or a submodule
				known[path] = true
				dirs = append(dirs, path)
			}
		}
	}
	slices.SortFunc(files, comparePaths)    // the walk's order; git's mixes tracked and untracked
	return slices.Compact(files), cut, true // an unmerged path is listed once per stage
}

// gitList runs git ls-files in dir: argv only, a clean environment (no
// GIT_DIR from a hook that started the daemon; HOME is how git finds the
// owner's global excludes). core.fsmonitor is off because a repository's
// own config could name a command. short: the output passed maxGitList.
func gitList(ctx context.Context, bin, dir string) (names []string, short bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-c", "core.fsmonitor=false", "-C", dir,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Env = gitEnv()
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	b, _ := io.ReadAll(io.LimitReader(out, int64(maxGitList)+1))
	if short = len(b) > maxGitList; short {
		b = b[:bytes.LastIndexByte(b[:maxGitList], 0)+1] // whole names only
		cancel()
	}
	if err := cmd.Wait(); err != nil && !short {
		return nil, false, err
	}
	for _, name := range strings.Split(string(b), "\x00") {
		if name != "" {
			names = append(names, strings.TrimSuffix(name, "/")) // a nested repository: "dir/"
		}
	}
	return names, short, nil
}

func gitEnv() []string {
	env := []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0"}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// hiddenPath reports a slash-separated relative path with a hidden part.
func hiddenPath(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// realDir reports that dir and every directory up to a known one (the root
// is) is a directory itself, not a symlink.
func realDir(dir string, known map[string]bool) bool {
	if ok, seen := known[dir]; seen {
		return ok
	}
	fi, err := os.Lstat(dir)
	ok := err == nil && fi.IsDir() && realDir(filepath.Dir(dir), known)
	known[dir] = ok
	return ok
}
