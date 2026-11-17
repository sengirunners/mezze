package session

// Script runs (SPEC 8 "Script runs"): a task of agent "run" runs a script
// from a pushed commit on another machine of the team project, through the
// same authority path as a remote task (queue, idempotency, machine choice,
// status, cancel, read). The code, not the caller, decides what runs: the
// target machine resolves the ref on its checkout's remote with a live
// lookup, makes a clean checkout of exactly that commit (never a working
// tree), runs the script there with SPEC 20's environment allowlist, and
// removes the checkout when the task ends. Output, stop, cancel and
// redaction holds are the task machinery's own. A run may carry Command,
// one shell command line, instead of a script (an approved command action,
// SPEC 16): a fixed /bin/sh wrapper runs it in the same checkout.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

const (
	runArgsMax   = 64
	runArgMax    = 4096
	runScriptMax = 1024
	// runCommandMax bounds a command run's line (an action's is shorter).
	runCommandMax = 4096
	runTextTail   = 4000
	// runOutputMax is the most of a run's output task_read serves (its
	// tail), in runPage pages: a remote read is one heartbeat each.
	runOutputMax = 1 << 20
	runPage      = 64 << 10
)

// runStarts is read-held by every script-run start from before its
// checkout exists until its task row is written; the sweep runs only when it
// can take it outright, so it never sees a checkout between the two
// (a start that ended between the sweep's directory read and its
// task query lost its checkout while the script ran).
var runStarts sync.RWMutex

var runCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// runEnvAllow is SPEC 20's environment allowlist for what a run target
// runs; the daemon adds only zm's own session variables.
var runEnvAllow = []string{"PATH", "HOME", "LANG", "TERM", "TZ"}

// isRun reports a script-run request.
func isRun(req orchapi.TaskStartRequest) bool { return req.Agent == orchapi.TaskAgentRun }

// checkRun validates a script run's request: no agent fields, a plain ref,
// and either a repository-relative script with literal arguments or one
// command line.
func checkRun(req orchapi.TaskStartRequest) error {
	r := req.Run
	if r == nil {
		return api.Errorf(api.CodeBadRequest, "agent run needs run: {ref, script}")
	}
	if strings.TrimSpace(req.Prompt) != "" || req.Model != "" || req.OnLimit != "" || req.ContinueFrom != "" || req.Worktree != nil && *req.Worktree {
		return api.Errorf(api.CodeBadRequest, "a script run takes no prompt, model, on_limit, continue_from or worktree")
	}
	if why := orchapi.RunRefProblem(r.Ref); why != "" {
		return api.Errorf(api.CodeBadRequest, "%s", why)
	}
	if r.Commit != "" && !orchapi.RunCommitOK(r.Commit) {
		return api.Errorf(api.CodeBadRequest, "run.commit must be a full 40-character commit id")
	}
	if r.CommitOnRef && r.Commit == "" {
		return api.Errorf(api.CodeBadRequest, "run.commit_on_ref needs run.commit")
	}
	if r.Command != "" {
		if r.Script != "" || len(r.Args) > 0 {
			return api.Errorf(api.CodeBadRequest, "run.command goes alone: no run.script or run.args")
		}
		if r.Command == "" || len(r.Command) > runCommandMax || strings.IndexFunc(r.Command, func(c rune) bool { return c < 0x20 || c > 0x7e }) >= 0 {
			return api.Errorf(api.CodeBadRequest, "run.command is one line of printable ASCII, at most %d bytes", runCommandMax)
		}
		return nil
	}
	sc := r.Script
	if sc == "" || len(sc) > runScriptMax || strings.ContainsRune(sc, 0) || filepath.IsAbs(sc) || filepath.Clean(sc) != sc ||
		sc == "." || sc == ".." || strings.HasPrefix(sc, "../") || strings.HasPrefix(sc, "-") {
		return api.Errorf(api.CodeBadRequest, "run.script must be a clean path inside the repository, such as scripts/test.sh")
	}
	if len(r.Args) > runArgsMax {
		return api.Errorf(api.CodeBadRequest, "run.args takes at most %d arguments", runArgsMax)
	}
	for _, a := range r.Args {
		if len(a) > runArgMax || strings.ContainsRune(a, 0) {
			return api.Errorf(api.CodeBadRequest, "each run argument is at most %d bytes, with no NUL", runArgMax)
		}
	}
	return nil
}

// runGit runs git with a bounded time and no prompts.
func runGitT(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// No hooks, and no GIT_* from the daemon's environment (a stray GIT_DIR
	// must not point the checkout at another repository); no replace refs
	// or grafts, which would put another tree or history under a commit's id.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE="+os.DevNull)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// resolveRunRef is ref's commit on url by a live lookup (git ls-remote),
// never a local tracking ref: a branch first, then a tag (peeled). full is
// the remote ref that matched, the one to fetch. repo "" asks from no
// repository at all (a git dir that cannot exist), so no repository's own
// config bends the lookup.
func resolveRunRef(ctx context.Context, repo, url, ref string) (commit, full string, err error) {
	want := []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref}
	args := []string{"ls-remote", "--", url, "refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"}
	if repo == "" {
		args = append([]string{"--git-dir", filepath.Join(os.DevNull, "no-repository")}, args...)
	}
	out, err := runGitT(ctx, 30*time.Second, repo, args...)
	if err != nil {
		return "", "", api.Errorf(api.CodeUnavailable, "resolve %s on the remote: %v", ref, err)
	}
	found := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(line, "\t")
		if ok && runCommitPattern.MatchString(sha) {
			found[name] = sha
		}
	}
	for _, w := range want {
		if sha := found[w]; sha != "" {
			return sha, strings.TrimSuffix(w, "^{}"), nil
		}
	}
	return "", "", api.Errorf(api.CodeBadRequest, "the remote has no branch or tag %s", ref).
		WithHint("push it first: a run needs a pushed commit")
}

// runsDir holds script runs' checkouts on this machine.
func (s *Service) runsDir() string { return filepath.Join(s.d.Config.Home, "runs") }

// runMirrorsDir holds bare clones of projects this machine has no checkout
// of, made for script runs (outside runsDir: the sweep never touches them).
func (s *Service) runMirrorsDir() string { return filepath.Join(s.d.Config.Home, "run-mirrors") }

var (
	runMirrorLock    = make(chan struct{}, 1)
	runRemotePattern = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z]{2,}/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// runMirrorURL is where a mirror clones a project's remote from (a test
// points it at a local repository).
var runMirrorURL = func(remote string) string { return "https://" + remote + ".git" }

const (
	mirrorAbsent = iota
	mirrorGood
	mirrorBad     // definitely not a whole mirror of the remote: clone again
	mirrorUnknown // the check itself failed (timeout, cancel): leave it alone
)

// mirrorState checks dir as a bare mirror of url, asking git about dir alone
// (--git-dir: never a repository above it).
func mirrorState(ctx context.Context, dir, url string) int {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return mirrorAbsent
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(err) {
		return mirrorBad
	}
	got, err := runGitT(ctx, 10*time.Second, "", "--git-dir", dir, "remote", "get-url", "origin")
	var ee *exec.ExitError
	switch {
	case err == nil && got == url:
		return mirrorGood
	case err == nil, errors.As(err, &ee) && ee.Exited() && ctx.Err() == nil:
		return mirrorBad
	}
	return mirrorUnknown
}

// projectRemote is proj's first listed remote: this machine's own rules,
// else, for a joined team project, the rules its authority last sent (a farm
// box that joined never sets its own).
func (s *Service) projectRemote(proj string) string {
	if rs := s.projectRemotes(proj); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// projectRemotes are proj's listed remotes, from the same rules as
// projectRemote.
func (s *Service) projectRemotes(proj string) []string {
	if s.d.Projects != nil {
		if p, err := s.d.Projects.Get(authz.All(), proj); err == nil && len(p.Remotes) > 0 {
			return p.Remotes
		}
	}
	if tr, ok := s.d.Team.(core.TeamRepos); ok {
		for _, j := range tr.JoinedRepos() {
			if j.Project.ID == proj && len(j.Repos.Remotes) > 0 {
				return j.Repos.Remotes
			}
		}
	}
	return nil
}

// originServes reports whether a checkout whose origin is url may serve a
// run of a project listing remotes (normalised "host/owner/repo"): any
// origin when it lists none, else only one of them, in any URL form.
func originServes(url string, remotes []string) bool {
	if url == "" {
		return false
	}
	got := project.NormalizeRemote(url)
	return len(remotes) == 0 || slices.ContainsFunc(remotes, func(w string) bool { return strings.EqualFold(got, w) })
}

// runMirror is a bare clone of proj's first listed remote, made once under
// runMirrorsDir, for a machine where no session of the project ever ran (a
// farm box): a run needs only the remote, never a working tree. It refuses
// a project that lists no remote.
func (s *Service) runMirror(ctx context.Context, proj string) (string, error) {
	remote := s.projectRemote(proj)
	if !runRemotePattern.MatchString(remote) || strings.Contains(remote, "..") {
		return "", api.Errorf(api.CodeBadRequest, "%s has no checkout of the project, and the project lists no remote to clone", s.d.Config.Machine).
			WithHint("list the repository in the project's remotes, or start a session of the project on that machine once")
	}
	// '@' is outside the pattern, so distinct remotes never share a name.
	dir := filepath.Join(s.runMirrorsDir(), strings.ReplaceAll(remote, "/", "@")+".git")
	url := runMirrorURL(remote)
	if mirrorState(ctx, dir, url) == mirrorGood {
		return dir, nil
	}
	select { // one clone at a time; a cancelled start stops waiting
	case runMirrorLock <- struct{}{}:
		defer func() { <-runMirrorLock }()
	case <-ctx.Done():
		return "", api.Errorf(api.CodeUnavailable, "waiting to clone %s: %v", remote, ctx.Err())
	}
	switch mirrorState(ctx, dir, url) {
	case mirrorGood: // made while this start waited
		return dir, nil
	case mirrorUnknown: // never remove a mirror on a timeout or a cancel
		return "", api.Errorf(api.CodeUnavailable, "could not check the mirror of %s on %s; try again", remote, s.d.Config.Machine)
	case mirrorBad:
		if es, _ := os.ReadDir(filepath.Join(dir, "worktrees")); len(es) > 0 {
			return "", api.Errorf(api.CodeUnavailable, "the mirror of %s on %s is damaged but runs still use it", remote, s.d.Config.Machine)
		}
	}
	if err := os.MkdirAll(s.runMirrorsDir(), 0o700); err != nil {
		return "", err
	}
	// Under the lock no clone is in progress: any .clone-* is a crash's
	// leftover, and a mirror found bad is cloned again.
	if es, err := os.ReadDir(s.runMirrorsDir()); err == nil {
		for _, e := range es {
			if strings.HasPrefix(e.Name(), ".clone-") {
				_ = os.RemoveAll(filepath.Join(s.runMirrorsDir(), e.Name()))
			}
		}
	}
	_ = os.RemoveAll(dir)
	tmp, err := os.MkdirTemp(s.runMirrorsDir(), ".clone-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := runGitT(ctx, 10*time.Minute, "", "clone", "--bare", "--quiet", "--filter=blob:none", "--", url, tmp); err != nil {
		return "", api.Errorf(api.CodeUnavailable, "clone %s on %s: %v", remote, s.d.Config.Machine, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// runDir is op's checkout directory.
func (s *Service) runDir(opID string) string {
	return filepath.Join(s.runsDir(), strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			return c
		}
		return '_'
	}, strings.ToLower(opID)))
}

// sweepRunCheckouts removes run checkouts no task needs: left by a crash
// between checkout and task end, or by an end that could not remove it. A
// checkout stays while its task runs, while its process group may still be
// running (an operator inspects it), and while a start is making it.
func (s *Service) sweepRunCheckouts(ctx context.Context) {
	runs := s.runsDir()
	if !runStarts.TryLock() {
		return // a start is making its checkout: next tick
	}
	defer runStarts.Unlock()
	es, err := os.ReadDir(runs)
	if err != nil || len(es) == 0 {
		return
	}
	keep := map[string]bool{}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT request_json FROM session_tasks WHERE state='running' OR failure LIKE '%may still be running%'`)
	if err != nil {
		return
	}
	for rows.Next() {
		var js string
		var req orchapi.TaskStartRequest
		if rows.Scan(&js) == nil && json.Unmarshal([]byte(js), &req) == nil && isRun(req) {
			keep[req.Cwd] = true
		}
	}
	rows.Close()
	for _, e := range es {
		dir := filepath.Join(runs, e.Name())
		if keep[dir] {
			continue
		}
		removeRunCheckout(ctx, runs, dir)
	}
}

// prepareRunHook, when a test sets it, runs first in every prepareRun (a
// slow checkout).
var prepareRunHook atomic.Pointer[func(ctx context.Context, opID string)]

// SetPrepareRunHook is for tests only: f (nil: none) runs first in every
// script run's checkout here, with the start's context.
func SetPrepareRunHook(f func(ctx context.Context, opID string)) {
	if f == nil {
		prepareRunHook.Store(nil)
		return
	}
	prepareRunHook.Store(&f)
}

// prepareRun makes the clean checkout for op's script run of project proj
// and answers the command and directory to run it in, and the commit.
func (s *Service) prepareRun(ctx context.Context, proj, opID string, r orchapi.TaskRun) (cmd []string, dir, commit string, err error) {
	if h := prepareRunHook.Load(); h != nil {
		(*h)(ctx, opID)
	}
	// The project's checkout here serves only when its origin is one of the
	// project's listed remotes (a farm worktree may have no origin, and a
	// session filed under the project may sit in another repository);
	// otherwise the mirror of its first remote does.
	repo, url := s.projectCheckout(ctx, proj), ""
	if repo != "" {
		url, _ = runGitT(ctx, 10*time.Second, repo, "remote", "get-url", "origin")
		if !originServes(url, s.projectRemotes(proj)) {
			repo = ""
		}
	}
	if repo == "" {
		if repo, err = s.runMirror(ctx, proj); err != nil {
			return nil, "", "", err
		}
		if url, err = runGitT(ctx, 10*time.Second, repo, "remote", "get-url", "origin"); err != nil || url == "" {
			return nil, "", "", api.Errorf(api.CodeUnavailable, "the mirror on %s has no origin remote", s.d.Config.Machine)
		}
	}
	head, full, err := resolveRunRef(ctx, repo, url, r.Ref)
	if err != nil {
		return nil, "", "", err
	}
	if r.Commit != "" && r.Commit != head && !r.CommitOnRef {
		return nil, "", "", api.Errorf(api.CodeForbidden, "run.commit %s is not what %s resolves to on the remote (%s)", r.Commit, r.Ref, head)
	}
	if _, err := runGitT(ctx, 5*time.Minute, repo, "fetch", "--no-tags", "--quiet", "origin", full); err != nil {
		return nil, "", "", api.Errorf(api.CodeUnavailable, "fetch %s: %v", r.Ref, err)
	}
	if _, err := runGitT(ctx, 10*time.Second, repo, "cat-file", "-e", head+"^{commit}"); err != nil {
		return nil, "", "", api.Errorf(api.CodeUnavailable, "commit %s is gone from the remote (the ref was rewritten); try again", head)
	}
	// The checkout is the pinned commit, never the ref's newer head. With
	// commit_on_ref (an approved ask whose commit was the ref's head when
	// asked) the ref may have moved on since: the commit must still be on it
	// on the remote, as the head or an ancestor of the head just fetched.
	commit = head
	if r.Commit != "" && r.Commit != head {
		if err := runOnRef(ctx, repo, r.Commit, head); err != nil {
			return nil, "", "", api.Errorf(api.CodeForbidden, "run.commit %s is not on %s on the remote (its head is %s): %v", r.Commit, r.Ref, head, err)
		}
		commit = r.Commit
	}
	if err := os.MkdirAll(s.runsDir(), 0o700); err != nil {
		return nil, "", "", err
	}
	dir = s.runDir(opID)
	if _, err := os.Stat(dir); err == nil {
		removeRunCheckout(ctx, s.runsDir(), dir) // a start interrupted before its task existed
	}
	_, _ = runGitT(ctx, time.Minute, repo, "worktree", "prune") // records of checkouts removed by hand
	if _, err := runGitT(ctx, 2*time.Minute, repo, "worktree", "add", "--detach", "--quiet", dir, commit); err != nil {
		return nil, "", "", api.Errorf(api.CodeUnavailable, "check out %s: %v", commit, err)
	}
	fail := func(e error) ([]string, string, string, error) {
		removeRunCheckout(ctx, s.runsDir(), dir)
		return nil, "", "", e
	}
	if head, err := runGitT(ctx, 10*time.Second, dir, "rev-parse", "HEAD"); err != nil || head != commit {
		return fail(api.Errorf(api.CodeUnavailable, "checkout is not at %s", commit))
	}
	if r.Command != "" {
		// The line is /bin/sh's program as a person types it at a prompt
		// (no positional arguments), after one fixed line that puts its
		// stderr in the output as a script's is. The line has no newline,
		// so it cannot reach into the first.
		return []string{"/bin/sh", "-c", "exec 2>&1\n" + r.Command}, dir, commit, nil
	}
	script := filepath.Join(dir, r.Script)
	real, err := filepath.EvalSymlinks(script)
	if err != nil {
		return fail(api.Errorf(api.CodeBadRequest, "%s is not in the repository at %s", r.Script, commit[:12]))
	}
	root, _ := filepath.EvalSymlinks(dir)
	fi, err := os.Stat(real)
	if err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return fail(api.Errorf(api.CodeBadRequest, "%s must be an executable file inside the repository", r.Script))
	}
	// A fixed wrapper (never a caller's string) puts stderr in the one
	// output the task records; the script and arguments stay literal.
	cmd = append([]string{"/bin/sh", "-c", `exec "$0" "$@" 2>&1`, script}, r.Args...)
	return cmd, dir, commit, nil
}

// runOnRef is nil when commit is an ancestor of head in repo (both
// commits, head fetched from the remote just now), else why not. git
// merge-base exits 1 for "not an ancestor"; any other failure (a shallow
// or partial history that cannot tell) refuses too.
func runOnRef(ctx context.Context, repo, commit, head string) error {
	if _, err := runGitT(ctx, 10*time.Second, repo, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return errors.New("that commit is not in the history fetched from it")
	}
	if got, err := runGitT(ctx, 10*time.Second, repo, "rev-parse", "--verify", "--quiet", commit+"^{commit}"); err != nil || got != commit {
		return errors.New("that id is not a commit")
	}
	if _, err := runGitT(ctx, time.Minute, repo, "merge-base", "--is-ancestor", commit, head); err != nil {
		return errors.New("it is not an ancestor of the head")
	}
	return nil
}

// CheckRunPin implements orchcore.RunPinChecker: when an ask carrying a
// run is made, whether commit is what ref resolves to on project's remote
// now, by the same live lookup a run makes (git ls-remote only: no fetch,
// no clone, nothing written here). checked is false, with no error, when
// this machine has no way to reach the remote (no checkout of the project
// here and no listed remote) or the remote did not answer in time: the run
// machine's own check after the yes decides then. A definite no is an
// error a person can act on.
func (s *Service) CheckRunPin(ctx context.Context, proj, ref, commit string) (checked bool, err error) {
	if why := orchapi.RunRefProblem(ref); why != "" {
		return false, api.Errorf(api.CodeBadRequest, "%s", why)
	}
	if !orchapi.RunCommitOK(commit) {
		return false, api.Errorf(api.CodeBadRequest, "run.commit must be a full 40-character commit id")
	}
	ctx, cancel := context.WithTimeout(ctx, runPinWithin)
	defer cancel()
	url := ""
	if repo := s.projectCheckout(ctx, proj); repo != "" {
		if u, err := runGitT(ctx, 10*time.Second, repo, "remote", "get-url", "origin"); err == nil && originServes(u, s.projectRemotes(proj)) {
			url = u
		}
	}
	if remote := s.projectRemote(proj); url == "" && runRemotePattern.MatchString(remote) && !strings.Contains(remote, "..") {
		url = runMirrorURL(remote)
	}
	if url == "" {
		return false, nil
	}
	head, _, err := resolveRunRef(ctx, "", url, ref) // from no repository
	if err != nil {
		if api.IsCode(err, api.CodeBadRequest) {
			return false, err // the remote answered: no such branch or tag
		}
		return false, nil
	}
	if head != commit {
		return false, api.Errorf(api.CodeBadRequest, "commit %s is not what %s resolves to on the remote now (%s)", commit, ref, head).
			WithHint("fetch, then give the commit git rev-parse " + ref + " prints (for a tag, the commit it points at)")
	}
	return true, nil
}

// runPinWithin bounds an ask's live check of its run's ref.
var runPinWithin = 15 * time.Second

// removeRunCheckout removes a run's checkout under runs (and its worktree
// record). Anything outside runs is left alone.
func removeRunCheckout(ctx context.Context, runs, dir string) {
	if dir == "" || filepath.Dir(dir) != runs {
		return
	}
	ctx = context.WithoutCancel(ctx) // cleanup outlives a cancelled request
	if common, err := runGitT(ctx, 10*time.Second, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && common != "" {
		_, _ = runGitT(ctx, time.Minute, "", "--git-dir", common, "worktree", "remove", "--force", dir)
		_ = os.RemoveAll(dir)
		_, _ = runGitT(ctx, time.Minute, "", "--git-dir", common, "worktree", "prune")
		return
	}
	_ = os.RemoveAll(dir)
}

// identityEnv sets a child's zm identity in m: its session address and
// scratch directory. An approved ask's run on its approver's own machine
// (taskStart.asApprover) acts as them instead (SPEC 16): as this
// machine's human, with no session to fall back to.
func (s *Service) identityEnv(m map[string]string, addr, scratch string, task *taskStart) {
	m[config.EnvAs], m["ZM_SESSION"], m["ZM_SCRATCH"] = addr, addr, scratch
	if task == nil || !task.asApprover {
		return
	}
	delete(m, config.EnvAs)
	delete(m, "ZM_SESSION")
	if s.d.Config.Human != "" {
		m[config.EnvAs] = s.d.Config.HumanAddress()
	}
}

// runFilterEnv keeps only the allowlisted variables.
func runFilterEnv(env map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range runEnvAllow {
		if v, ok := env[k]; ok {
			out[k] = v
		}
	}
	return out
}

// runOutcome is a script run's outcome: only a script's own exit reports
// success, and a run cut off at its deadline reports exit 124.
func runOutcome(failure string, process *orchapi.Outcome) orchapi.Outcome {
	o := orchapi.Outcome{Kind: orchapi.OutcomeFailed, Confidence: "high", Evidence: "script exit status unknown"}
	if process != nil {
		o.ExitCode, o.Signal = process.ExitCode, process.Signal
	}
	switch {
	case strings.Contains(failure, "may still be running"):
		o.Evidence = failure
	case failure == "cancelled":
		o.Kind, o.Evidence = orchapi.OutcomeKilled, "cancelled"
	case failure == "overall deadline exceeded":
		code := 124
		o.Kind, o.Evidence, o.ExitCode = orchapi.OutcomeKilled, failure, &code
	case process != nil && process.Signal != "":
		o.Kind, o.Evidence = orchapi.OutcomeKilled, "killed by SIG"+process.Signal
		if failure != "" {
			o.Evidence = failure + "; " + o.Evidence
		}
	case failure != "":
		o.Evidence = failure
	case process != nil && process.Kind == orchapi.OutcomeInfra:
		o = *process
	case process != nil && process.ExitCode != nil && *process.ExitCode == 0:
		o.Kind, o.Evidence = orchapi.OutcomeOK, "script exited 0"
	case process != nil && process.ExitCode != nil:
		o.Evidence = fmt.Sprintf("script exited %d", *process.ExitCode)
	}
	return o
}

// runResult is a script run's parsed result: its output's tail as text.
func runResult(transcript []byte, outcome orchapi.Outcome) taskEventResult {
	r := taskEventResult{Complete: true, Success: outcome.Kind == orchapi.OutcomeOK, Kind: outcome.Kind, Evidence: outcome.Evidence, Finished: "not_done"}
	if r.Success {
		r.Finished = "done"
	}
	r.Text = runText(transcript, runTextTail)
	return r
}

// runText is the last n bytes of out as valid UTF-8.
func runText(out []byte, n int) string {
	if len(out) > n {
		out = out[len(out)-n:]
		for len(out) > 0 && !utf8.RuneStart(out[0]) {
			out = out[1:]
		}
	}
	return strings.ToValidUTF8(string(out), "�")
}

// runMessages is a script run's one history item: its output, the last
// runOutputMax bytes of it after a note of what was left out.
func runMessages(transcript []byte) []orchapi.TaskHistoryItem {
	text := runText(transcript, runOutputMax)
	if len(transcript) > runOutputMax {
		text = fmt.Sprintf("[zm: the first %d bytes of output are not kept]\n", len(transcript)-len(text)) + text
	}
	return []orchapi.TaskHistoryItem{{ID: orchapi.TaskRunOutput, Text: text}}
}
