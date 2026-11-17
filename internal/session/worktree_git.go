package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// daemonWorktreeGit keeps repository semantics (including global attributes and
// conversions) but forbids repository-selected programs in daemon maintenance.
// Ordinary git callers and an agent's own Git commands retain their semantics.
type daemonWorktreeGit struct {
	ctx context.Context
	dir string
	env []string
}

type worktreeGitSetting struct{ key, value string }

func newDaemonWorktreeGit(ctx context.Context, dir string) (*daemonWorktreeGit, error) {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=", "GIT_TERMINAL_PROMPT=0")
	settings := []worktreeGitSetting{
		{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"},
		{"core.pager", ""}, {"maintenance.auto", "false"}, {"gc.auto", "0"},
		{"log.showSignature", "false"}, {"diff.external", ""},
		{"diff.renames", "false"}, {"status.renames", "false"},
		{"submodule.recurse", "false"}, {"fetch.recurseSubmodules", "false"},
		{"protocol.allow", "never"}, {"credential.helper", ""},
	}
	g := &daemonWorktreeGit{ctx: ctx, dir: dir, env: worktreeGitEnv(env, settings)}
	// Reading metadata does not invoke hooks, filters, or object transports.
	// Keep normal global/system config: ignoring it could hide a clean filter
	// or conversion and incorrectly report an ended worktree as safe to remove.
	keys, err := g.raw("config", "--null", "--name-only", "--list")
	if err != nil {
		return nil, err
	}
	for _, key := range strings.Split(keys, "\x00") {
		lower := strings.ToLower(key)
		if lower == "extensions.partialclone" || (strings.HasPrefix(lower, "remote.") && (strings.HasSuffix(lower, ".promisor") || strings.HasSuffix(lower, ".partialclonefilter"))) {
			return nil, fmt.Errorf("repository uses partial clone configuration; daemon worktree operations cannot fetch missing objects")
		}
		if strings.HasPrefix(lower, "protocol.") && strings.HasSuffix(lower, ".allow") {
			settings = append(settings, worktreeGitSetting{key, "never"})
		}
		if strings.HasPrefix(lower, "filter.") && (strings.HasSuffix(lower, ".clean") || strings.HasSuffix(lower, ".smudge") || strings.HasSuffix(lower, ".process") || strings.HasSuffix(lower, ".required")) {
			value := ""
			if strings.HasSuffix(lower, ".required") {
				value = "false"
			}
			settings = append(settings, worktreeGitSetting{key, value})
		}
		if strings.HasPrefix(lower, "hook.") && (strings.HasSuffix(lower, ".command") || strings.HasSuffix(lower, ".event") || strings.HasSuffix(lower, ".enabled")) {
			settings = append(settings, worktreeGitSetting{key[:strings.LastIndexByte(key, '.')] + ".enabled", "false"})
		}
	}
	g.env = worktreeGitEnv(env, settings)
	return g, nil
}

func worktreeGitEnv(env []string, settings []worktreeGitSetting) []string {
	out := append([]string(nil), env...)
	out = append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(settings)))
	for i, setting := range settings {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, setting.key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, setting.value))
	}
	return out
}

func (g *daemonWorktreeGit) raw(args ...string) (string, error) {
	out, err := runGitEnvBounded(g.ctx, g.dir, nil, g.env, 8<<20, args...)
	if err == nil && len(out) > 8<<20 {
		return "", fmt.Errorf("daemon worktree Git metadata exceeds 8 MiB")
	}
	return out, err
}

func (g *daemonWorktreeGit) run(args ...string) (string, error) {
	out, err := g.raw(args...)
	return strings.TrimSpace(out), err
}

// refuseFilters checks every tracked path, not merely paths status reports as
// dirty. Otherwise disabling a clean driver could produce a false clean result.
// Inspect the indexed attributes for creation; GC also inspects working attrs.
func (g *daemonWorktreeGit) refuseFilters(working bool) error {
	index, err := g.raw("ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	var paths []string
	for _, entry := range strings.Split(index, "\x00") {
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "160000 ") {
			return fmt.Errorf("worktree contains a submodule or nested repository; inspect it manually")
		}
		_, path, ok := strings.Cut(entry, "\t")
		if !ok {
			return fmt.Errorf("invalid worktree index metadata")
		}
		paths = append(paths, path)
		if len(paths) > 100000 {
			return fmt.Errorf("worktree has too many tracked paths to inspect safely")
		}
	}
	for _, cached := range []bool{true, false} {
		if !cached && !working {
			break
		}
		for first := 0; first < len(paths); {
			last, size := first, 0
			for last < len(paths) && last-first < 1000 && size+len(paths[last]) < 64<<10 {
				size += len(paths[last]) + 1
				last++
			}
			if last == first {
				return fmt.Errorf("worktree path exceeds inspection argument budget")
			}
			args := []string{"check-attr", "-z"}
			if cached {
				args = append(args, "--cached")
			}
			args = append(args, "filter", "--")
			args = append(args, paths[first:last]...)
			attrs, err := g.raw(args...)
			if err != nil {
				return err
			}
			parts := strings.Split(attrs, "\x00")
			if len(parts) != (last-first)*3+1 {
				return fmt.Errorf("invalid worktree attributes")
			}
			for i := 0; i < len(parts)-1; i += 3 {
				if parts[i+2] != "unspecified" && parts[i+2] != "unset" {
					return fmt.Errorf("tracked path %q has a filter attribute; daemon cannot inspect or check it out without running repository programs", parts[i])
				}
			}
			first = last
		}
	}
	return nil
}

// removeCreated uses the established source policy even if destination-only
// config caused creation to fail. Rereading that config would prevent rollback.
func (g *daemonWorktreeGit) removeCreated(ctx context.Context, wt, branch string) {
	cleanup := *g
	cleanup.ctx = ctx
	listing, err := cleanup.raw("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return
	}
	realWT, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return
	}
	owned := false
	for _, record := range strings.Split(listing, "\x00\x00") {
		var path, ref string
		for _, field := range strings.Split(record, "\x00") {
			if strings.HasPrefix(field, "worktree ") {
				path = strings.TrimPrefix(field, "worktree ")
			}
			if strings.HasPrefix(field, "branch ") {
				ref = strings.TrimPrefix(field, "branch ")
			}
		}
		if path == realWT && ref == "refs/heads/"+branch {
			owned = true
			break
		}
	}
	if !owned {
		return
	}
	if _, err := cleanup.run("worktree", "remove", "--force", wt); err == nil {
		_, _ = cleanup.run("branch", "-D", branch)
	}
}

// readWorktreeInitialization reads only small regular metadata files. A FIFO or
// symlink in an interrupted checkout must not block rollback or redirect reads.
func readWorktreeInitialization(path string) (string, bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	return string(b), err == nil && len(b) <= 8192
}

// removeInitializing is restricted to an invocation whose destination, branch,
// and exact administration slot were absent before add. Git can be interrupted
// before symbolic-ref replaces its provisional zero HEAD; normal branch-based
// ownership checks deliberately do not accept that state.
func (g *daemonWorktreeGit) removeInitializing(ctx context.Context, wt, admin, common, branch, commit string) bool {
	if ctx.Err() != nil {
		return false
	}
	for _, path := range []string{wt, admin} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	realWT, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return false
	}
	realAdmin, err := filepath.EvalSymlinks(admin)
	if err != nil || realAdmin != admin || filepath.Dir(admin) != filepath.Join(common, "worktrees") {
		return false
	}
	// No checked-out or user-created files may be discarded by this fallback.
	for path, allowed := range map[string]map[string]bool{
		wt:    {".git": true},
		admin: {"HEAD": true, "locked": true, "gitdir": true, "commondir": true},
	} {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return false
		}
		dir := os.NewFile(uintptr(fd), path)
		entries, readErr := dir.ReadDir(6)
		dir.Close()
		if readErr != nil && readErr != io.EOF {
			return false
		}
		for _, entry := range entries {
			if !allowed[entry.Name()] {
				return false
			}
		}
	}
	metadata := map[string]string{}
	for _, name := range []string{"HEAD", "locked", "gitdir", "commondir"} {
		value, ok := readWorktreeInitialization(filepath.Join(admin, name))
		if !ok {
			return false
		}
		metadata[name] = value
	}
	if (len(commit) != 40 && len(commit) != 64) || metadata["HEAD"] != strings.Repeat("0", len(commit))+"\n" {
		return false
	}
	if metadata["locked"] != "initializing\n" || metadata["commondir"] != "../..\n" || metadata["gitdir"] != filepath.Join(realWT, ".git")+"\n" {
		return false
	}
	link, ok := readWorktreeInitialization(filepath.Join(wt, ".git"))
	if !ok || link != "gitdir: "+admin+"\n" {
		return false
	}
	cleanup := *g
	cleanup.ctx = ctx
	// The branch was absent before add, and may have been created before HEAD
	// initialization. Refuse cleanup if it no longer points to our commit.
	ref, err := cleanup.run("for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil || (ref != "" && ref != commit) || ctx.Err() != nil {
		return false
	}
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		return false
	}
	if err := os.Remove(wt); err != nil {
		return false
	}
	for _, name := range []string{"HEAD", "locked", "gitdir", "commondir"} {
		if err := os.Remove(filepath.Join(admin, name)); err != nil {
			return false
		}
	}
	if err := os.Remove(admin); err != nil {
		return false
	}
	if ref != "" {
		// Preserve a branch another process advanced after our proof. The
		// expected old OID makes deletion atomic with that identity check.
		_, _ = cleanup.run("update-ref", "-d", "refs/heads/"+branch, commit)
	}
	return true
}
