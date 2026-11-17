package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// git runs git in dir with a timeout and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return runGit(ctx, dir, nil, args...)
}

func runGit(ctx context.Context, dir string, config []string, args ...string) (string, error) {
	return runGitEnv(ctx, dir, config, nil, args...)
}

func runGitEnv(ctx context.Context, dir string, config, env []string, args ...string) (string, error) {
	return runGitEnvBounded(ctx, dir, config, env, 0, args...)
}

// A nonzero outputLimit bounds metadata reads before allocating their contents.
// Existing general Git callers keep their previous unbounded-output behavior.
func runGitEnvBounded(ctx context.Context, dir string, config, env []string, outputLimit int64, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"-C", dir}, config...), args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	raw := env != nil
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, stderr []byte
	var err error
	if raw {
		// Files do not wait for descendants to close inherited pipes. This
		// keeps all snapshot commands within their shared context deadline.
		stdoutFile, e := os.CreateTemp("", "zm-git-out-*")
		if e != nil {
			return "", e
		}
		defer os.Remove(stdoutFile.Name())
		defer stdoutFile.Close()
		stderrFile, e := os.CreateTemp("", "zm-git-err-*")
		if e != nil {
			return "", e
		}
		defer os.Remove(stderrFile.Name())
		defer stderrFile.Close()
		cmd.Stdout, cmd.Stderr = stdoutFile, stderrFile
		err = cmd.Run()
		if err == nil && outputLimit > 0 {
			var info os.FileInfo
			info, err = stdoutFile.Stat()
			if err == nil && info.Size() > outputLimit {
				err = fmt.Errorf("Git metadata exceeds %d bytes", outputLimit)
			}
		}
		if err == nil {
			out, err = os.ReadFile(stdoutFile.Name())
		} else if outputLimit > 0 {
			_, _ = stderrFile.Seek(0, io.SeekStart)
			stderr, _ = io.ReadAll(io.LimitReader(stderrFile, 8<<10))
		} else {
			stderr, _ = os.ReadFile(stderrFile.Name())
		}
	} else {
		out, err = cmd.Output()
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() && ctx.Err() == nil {
			err = nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
	}
	if err != nil {
		msg := err.Error()
		if len(stderr) > 0 {
			msg = oneLine(string(stderr))
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	if raw {
		return string(out), nil
	}
	return strings.TrimSpace(string(out)), nil
}

// addWorktree creates $ZM_HOME/worktrees/<id8> on a new branch
// zm/<id8> from the HEAD of cwd's repository, and returns the directory
// matching cwd inside it.
func addWorktree(ctx context.Context, home, cwd, id8 string) (wt, branch, newCwd string, err error) {
	return addWorktreeFrom(ctx, home, cwd, id8, "HEAD")
}

func addWorktreeFrom(ctx context.Context, home, cwd, id8, base string) (wt, branch, newCwd string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	g, err := newDaemonWorktreeGit(ctx, cwd)
	if err != nil {
		return "", "", "", err
	}
	root, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", api.Errorf(api.CodeBadRequest, "--worktree needs a git repository: %s is not in one", cwd)
	}
	wt = filepath.Join(home, "worktrees", id8)
	branch = "zm/" + id8
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return "", "", "", err
	}
	g, err = newDaemonWorktreeGit(ctx, root)
	if err != nil {
		return "", "", "", err
	}
	ref, err := g.run("for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
	if err != nil {
		return "", "", "", err
	}
	_, pathErr := os.Lstat(wt)
	if ref != "" || !os.IsNotExist(pathErr) {
		return "", "", "", api.Errorf(api.CodeBadRequest, "worktree path or branch already exists; preserving it")
	}
	commit, err := g.run("rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return "", "", "", err
	}
	// Record the initialization slot before Git can create it. An interrupted
	// add may not yet have installed the requested branch in its HEAD.
	common, err := g.run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", "", err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return "", "", "", err
	}
	admin := filepath.Join(common, "worktrees", filepath.Base(wt))
	_, adminErr := os.Lstat(admin)
	initializationAbsent := os.IsNotExist(adminErr)
	cleanup := func(interruptedAdd bool) {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if interruptedAdd && initializationAbsent && g.removeInitializing(cleanupCtx, wt, admin, common, branch, commit) {
			return
		}
		g.removeCreated(cleanupCtx, wt, branch)
	}
	if _, err := g.run("worktree", "add", "--no-checkout", "-q", "-b", branch, wt, commit); err != nil {
		cleanup(true)
		return "", "", "", api.Errorf(api.CodeBadRequest, "create worktree: %v", err)
	}
	// Destination-conditional config may differ from the source. Inspect the
	// requested base's index before materializing any file or running filters.
	wg, err := newDaemonWorktreeGit(ctx, wt)
	if err == nil {
		_, err = wg.run("read-tree", commit)
	}
	if err == nil {
		err = wg.refuseFilters(false)
	}
	if err == nil {
		_, err = wg.run("checkout-index", "-a", "-u")
	}
	if err != nil {
		cleanup(false)
		return "", "", "", api.Errorf(api.CodeBadRequest, "create worktree: %v", err)
	}
	newCwd = wt
	if rroot, err := filepath.EvalSymlinks(root); err == nil {
		rcwd, _ := filepath.EvalSymlinks(cwd)
		if rel, err := filepath.Rel(rroot, rcwd); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			if fi, err := os.Stat(filepath.Join(wt, rel)); err == nil && fi.IsDir() {
				newCwd = filepath.Join(wt, rel)
			}
		}
	}
	return wt, branch, newCwd, nil
}

// mainRepo is the main checkout a linked worktree belongs to ("" if unknown).
func mainRepo(ctx context.Context, wt string) string {
	g, err := newDaemonWorktreeGit(ctx, wt)
	if err != nil {
		return ""
	}
	common, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(wt, common)
	}
	return filepath.Dir(common)
}

// removeWorktree undoes addWorktree when the spawn fails before the agent
// ever ran (nothing in it can be the agent's work).
func removeWorktree(ctx context.Context, wt, branch string) {
	if wt == "" || branch == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wg, err := newDaemonWorktreeGit(ctx, wt)
	if err != nil {
		return
	}
	// This rollback only owns the freshly created worktree/branch pair. Never
	// remove a checkout whose branch changed, or a user's preexisting branch.
	current, err := wg.run("symbolic-ref", "--short", "HEAD")
	if err != nil || current != branch {
		return
	}
	repo := mainRepo(ctx, wt)
	if repo == "" {
		return
	}
	g, err := newDaemonWorktreeGit(ctx, repo)
	if err != nil {
		return
	}
	if _, err := g.run("worktree", "remove", "--force", wt); err == nil {
		_, _ = g.run("branch", "-D", branch)
	}
}

// gcCandidate inspects an ended session's worktree. zm never deletes it;
// Safe requires an ended, clean worktree; task branches are retained even
// when they contain commits that exist nowhere else.
func gcCandidate(ctx context.Context, r row, keepBranch bool) (orchapi.GCCandidate, bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := orchapi.GCCandidate{Session: r.Address, Outcome: r.Outcome, Worktree: r.Worktree, Branch: r.Branch, Reasons: []string{}}
	if fi, err := os.Stat(r.Worktree); err != nil || !fi.IsDir() {
		return c, false // already removed: nothing to list
	}
	ended := r.State == orchapi.StateEnded
	if ended {
		kind := "ended"
		if r.Outcome != nil {
			kind += " " + string(r.Outcome.Kind)
		}
		c.Reasons = append(c.Reasons, kind)
	} else {
		c.Reasons = append(c.Reasons, "still "+string(r.State))
	}
	ok := ended
	g, err := newDaemonWorktreeGit(ctx, r.Worktree)
	if err == nil {
		err = g.refuseFilters(true)
	}
	if err != nil {
		c.Reasons = append(c.Reasons, err.Error())
		return c, true // never advertise removal under changed filter semantics
	}
	status, err := g.run("status", "--porcelain", "--untracked-files=all", "--no-renames")
	switch {
	case err != nil:
		c.Reasons, ok = append(c.Reasons, err.Error()), false
	case status != "":
		c.Dirty, ok = true, false
		c.Reasons = append(c.Reasons, "uncommitted changes")
	default:
		c.Reasons = append(c.Reasons, "clean")
	}
	// Commits reachable from HEAD but from no other branch or remote ref.
	args := []string{"rev-list", "--count", "HEAD", "--not"}
	if r.Branch != "" {
		args = append(args, "--exclude="+r.Branch)
	}
	args = append(args, "--branches", "--remotes")
	n, err := g.run(args...)
	if err == nil {
		c.Ahead, _ = strconv.Atoi(n)
	}
	switch {
	case err != nil:
		c.Reasons, ok = append(c.Reasons, err.Error()), false
	case c.Ahead > 0:
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d commits only on %s", c.Ahead, or(r.Branch, "HEAD")))
		if !keepBranch || r.Branch == "" {
			ok = false
		} else if _, e := g.run("show-ref", "--verify", "refs/heads/"+r.Branch); e != nil {
			ok = false
		}
	default:
		c.Reasons = append(c.Reasons, "no commits only on this branch")
	}
	c.Safe = ok
	c.Repo = mainRepo(ctx, r.Worktree)
	if c.Safe && c.Repo != "" {
		// git -C: the bare "git worktree remove" failed when run outside the repo (dogfood).
		c.Remove = "git -C " + shellQuote(c.Repo) + " worktree remove " + shellQuote(r.Worktree)
		if !keepBranch && r.Branch != "" {
			c.Remove += " && git -C " + shellQuote(c.Repo) + " branch -D " + shellQuote(r.Branch)
		}
	}
	return c, true
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
