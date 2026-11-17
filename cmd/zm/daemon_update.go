package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/spf13/cobra"
)

// publicRepo is the public repository releases are published to: the
// first update source, and the one both plugin manifests and the compute
// plugin's zm-release.pin name first (TestUpdateSourceOneSetting checks them).
const publicRepo = "sengirunners/mezze"

// buildRepo is the GitHub repository (owner/name) this binary was built
// from, set at link time (-X main.buildRepo): scripts/release passes the
// repository its workflow runs in, make and a source update the checkout's
// origin. Empty, as in a plain go build, means publicRepo.
var buildRepo string

// updateRepos is the one update-source setting: the repositories whose
// release tags a machine updates from, public repo first. The daemon's
// release channel takes the first that has releases. A binary built from
// another repository (a private one, until its public copy has published)
// also reads that repository's releases, after the public one.
// Releases only, never a branch.
var updateRepos = sourceRepos(publicRepo, buildRepo)

// updateBuildRepo is the repo a source build (zm daemon update --repo) must be
// a checkout of: it builds main of a clone, so it stays on the repository
// this binary came from.
var updateBuildRepo = updateRepos[len(updateRepos)-1]

var ghRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// sourceRepos is pub, then built when it names another repository; a value
// that is not owner/name is ignored.
func sourceRepos(pub, built string) []string {
	if built == "" || !ghRepoName.MatchString(built) || strings.EqualFold(built, pub) {
		return []string{pub}
	}
	return []string{pub, built}
}

// updateReposKey is the config.json key holding a machine's own order of
// release repositories (SPEC 2, "Release channel"). zm reads it itself: the
// kit's config type does not know it, ignores it on load and keeps it on save.
// Nothing writes it but a person editing that machine's config.json.
const updateReposKey = "update_repos"

// maxUpdateRepos bounds the list: each repo may cost a few GitHub calls a check.
const maxUpdateRepos = 8

// GitHub's own rules for an owner (an account or organization) and a
// repository name; a name never ends in .git and is never . or ..
var (
	ghOwner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	ghName  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// releaseRepos is the order of repositories this home's release channel
// reads: config.json's update_repos when it is there (set), else a copy of
// updateRepos, the built-in order. A value that is there but is not a
// strict list of GitHub repositories is an error that stops the check: it
// never falls back to the default.
func releaseRepos(home string) (repos []string, set bool, err error) {
	b, err := os.ReadFile(filepath.Join(home, config.FileConfig))
	if errors.Is(err, fs.ErrNotExist) {
		return slices.Clone(updateRepos), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", filepath.Join(home, config.FileConfig), err)
	}
	raw, ok := doc[updateReposKey]
	if !ok {
		for k := range doc {
			if strings.EqualFold(k, updateReposKey) {
				return nil, true, fmt.Errorf("config %q: the key is %s, in lower case", k, updateReposKey)
			}
		}
		return slices.Clone(updateRepos), false, nil
	}
	if repos, err = parseReleaseRepos(raw); err != nil {
		return nil, true, fmt.Errorf("config %s %w", updateReposKey, err)
	}
	return repos, true, nil
}

// parseReleaseRepos checks update_repos' value: syntax only.
func parseReleaseRepos(raw json.RawMessage) ([]string, error) {
	var repos []string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &repos) != nil {
		return nil, errors.New(`must be a list of repositories, ["owner/name", ...]`)
	}
	if len(repos) == 0 {
		return nil, errors.New("is empty: remove the key to use the default order")
	}
	if len(repos) > maxUpdateRepos {
		return nil, fmt.Errorf("lists %d repositories; at most %d", len(repos), maxUpdateRepos)
	}
	for i, r := range repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || !ghOwner.MatchString(owner) || !ghName.MatchString(name) || name == "." || name == ".." || strings.HasSuffix(strings.ToLower(name), ".git") {
			return nil, fmt.Errorf("has %q, not a GitHub repository (owner/name)", r)
		}
		for _, p := range repos[:i] {
			if strings.EqualFold(p, r) {
				return nil, fmt.Errorf("lists %q twice", r)
			}
		}
	}
	return repos, nil
}

// releaseSourcesNote is how a machine on its own order says so in each
// release check's reason (the stdout line and machine numbers' last check):
// "" on the default order, which reports nothing new.
func releaseSourcesNote(repos []string, set bool) string {
	if !set {
		return ""
	}
	return "release sources: " + strings.Join(repos, ", ") + " (config " + updateReposKey + ")"
}

// withSources is a check's reason with the order's note, if any.
func withSources(reason, note string) string {
	if note == "" {
		return reason
	}
	return strings.TrimPrefix(reason+"; "+note, "; ")
}

// releaseSourcesLine is the status line for this machine's order.
func releaseSourcesLine(home, machine string) string {
	repos, set, err := releaseRepos(home)
	switch {
	case err != nil:
		return fmt.Sprintf("release sources on %s: %v (release checks fail until it is fixed)\n", machine, err)
	case set:
		return fmt.Sprintf("release sources on %s: %s (config %s)\n", machine, strings.Join(repos, ", "), updateReposKey)
	}
	return fmt.Sprintf("release sources on %s: %s (default)\n", machine, strings.Join(repos, ", "))
}

var updateSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var updateCommit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

type updateRun struct {
	SHA        string `json:"head_sha"`
	Branch     string `json:"head_branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int64  `json:"run_number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// latestUpdate never selects a PR, dispatch, unfinished run or foreign fork.
func latestUpdate(data []byte) (string, error) {
	var body struct {
		Runs []updateRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", err
	}
	var best updateRun
	for _, r := range body.Runs {
		if r.Branch == "main" && r.Event == "push" && r.Status == "completed" && r.Conclusion == "success" && strings.EqualFold(r.Repository.FullName, updateBuildRepo) && updateSHA.MatchString(r.SHA) && r.Number > best.Number {
			best = r
		}
	}
	if best.SHA == "" {
		return "", fmt.Errorf("no finished successful ci.yml push on %s main", updateBuildRepo)
	}
	return best.SHA, nil
}

// updateEnv removes inherited build/Make/git overrides. There is deliberately
// no force/end-sessions flag: the staged binary uses the ordinary swap guards.
func updateEnv(env []string) []string {
	out := []string{}
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "MAKE") || strings.HasPrefix(k, "CGO_") {
			continue
		}
		switch k {
		case "GH_HOST", "GH_DEBUG", "FORCE", "END_SESSIONS", "INSTALL_DIR", "GOFLAGS", "GOENV", "GOWORK", "GOOS", "GOARCH", "GOAMD64", "GOARM", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOEXPERIMENT", "GOTOOLCHAIN", "GO", "LDFLAGS", "VERSION", "COMMIT":
			continue
		}
		out = append(out, e)
	}
	return append(out, "GOENV=off", "GOWORK=off", "GOFLAGS=", "GH_HOST=github.com")
}

// potluckModule is the public module the run-target drivers come from;
// the update build fetches it like any other dependency.
const potluckModule = "github.com/sengirunners/potluck"

// potluckCmds are built next to ./cmd/... (make install builds a newcomer's subset
// plus every extra already installed; see the Makefile):
// each driver under its Potluck name and its old one (one program; the
// daemon looks for potluck-driver-<name> first, then zm-run-<name>).
var potluckCmds = []string{
	potluckModule + "/cmd/potluck-driver-tart", potluckModule + "/cmd/potluck-driver-local", potluckModule + "/cmd/potluck-conformance",
	potluckModule + "/cmd/zm-run-tart", potluckModule + "/cmd/zm-run-local", potluckModule + "/cmd/zm-run-conformance", potluckModule + "/cmd/zm-e2b",
}

func updateHeld(now time.Time, hold, quiet string) (string, error) {
	if hold != "" {
		h, held, err := release.ReadHold(hold)
		if err != nil {
			return "", err
		}
		if held {
			why := "hold file exists: " + hold
			if h.Reason != "" {
				why += " (" + h.Reason + ")"
			}
			return why, nil
		}
	}
	if quiet == "" {
		return "", nil
	}
	a, b, ok := strings.Cut(quiet, "-")
	start, e1 := time.Parse("15:04", a)
	end, e2 := time.Parse("15:04", b)
	if !ok || e1 != nil || e2 != nil {
		return "", fmt.Errorf("quiet window must be HH:MM-HH:MM in the host's local time")
	}
	n, x, y := now.Hour()*60+now.Minute(), start.Hour()*60+start.Minute(), end.Hour()*60+end.Minute()
	if x == y || (x < y && n >= x && n < y) || (x > y && (n >= x || n < y)) {
		return "quiet window " + quiet, nil
	}
	return "", nil
}

// Every native store lives at projects/<id>/issues.db (issues/store/sqlite).
// Inspect without migrations, chmod, journal changes or writes.
func checkMovedIssues(ctx context.Context, home string) error {
	files := []string{}
	for rel, kind := range swap.Databases(home) {
		if kind == swap.KindIssues {
			files = append(files, filepath.Join(home, rel))
		}
	}
	for _, f := range files {
		u := &url.URL{Scheme: "file", Path: f}
		q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(1000)"}}
		u.RawQuery = q.Encode()
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		var tables int
		err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='moves'`).Scan(&tables)
		if err == nil && tables > 0 {
			var id string
			err = db.QueryRowContext(ctx, `SELECT id FROM issues WHERE id IN (SELECT old_id FROM moves WHERE dir='out') LIMIT 1`).Scan(&id)
			if err == nil {
				db.Close()
				return fmt.Errorf("native store %s still contains moved-out issue %s; repair before upgrading", f, id)
			}
			if err == sql.ErrNoRows {
				err = nil
			}
		}
		db.Close()
		if err != nil {
			return fmt.Errorf("native store precheck %s: %w", f, err)
		}
	}
	return nil
}

// The install gets a new budget from the invocation, never from the expired
// selection/build context. This reserves supervisor/verify AND rollback time.
func updateInstallContext(invocation context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(invocation, 10*time.Minute)
}

// unfinishedSwap judges an upgrade whose swapper died past its commit:
// swap.FilePending is left and no swapper holds swap.lock.
// The build it placed may run, never verified, and the tick would read
// running == candidate as unchanged (on the release channel: installed,
// later stable, and vouched for to other machines). It is reported failed
// instead, once; a release tick records the release failed here, so this
// home never installs it, lists it stable or vouches for it. A live swap
// holds the tick. stop: the tick ends here (rec and the error say how).
func unfinishedSwap(home string, releases bool, rec *api.SwapRecord) (stop bool, err error) {
	if _, ok, err := swap.ReadPending(home); err != nil || !ok {
		return err != nil, err
	}
	lock, err := swap.Acquire(home, api.SwapHolder{By: "zm daemon update", What: "update"})
	if busy := (*swap.BusyError)(nil); errors.As(err, &busy) {
		rec.Result, rec.Reason = "held", busy.Error()
		return true, nil
	}
	if err != nil {
		return true, err
	}
	defer lock.Release()
	// Its drivers' pins first: each to the file that is there (placed or
	// put back), whichever way the swap went.
	settleLeftPins(context.Background(), home, client.NewUnix((&config.Config{Home: home}).SocketPath()), os.Stderr)
	p, ok, err := swap.ReadPending(home) // its swap may have ended meanwhile
	if err != nil || !ok {
		return err != nil, err
	}
	if endedAfter(home, p.At) {
		// Its swapper recorded the end (ok, rolled back or forward), then
		// died before it removed the marker: nothing is unverified.
		if err := swap.ClearPending(home); err != nil {
			return true, fmt.Errorf("could not remove %s: %w", swap.FilePending, err)
		}
		return false, nil
	}
	rec.To = p.To
	why := fmt.Sprintf("the %s to %s (pid %d, at %s) never finished: its swapper died before the verify, so that build may run unverified",
		p.What, p.To, p.PID, p.At.Format(time.RFC3339))
	if releases && p.Version() != "" {
		st, _, err := release.LoadState(home)
		if err != nil {
			return true, err
		}
		st.MarkFailed(release.Failure{Version: p.Version(), At: p.At, Result: "failed", Reason: why})
		if err := release.WriteState(home, st); err != nil {
			return true, fmt.Errorf("%s; could not record it: %w", why, err)
		}
	}
	if err := swap.ClearPending(home); err != nil {
		return true, fmt.Errorf("%s; could not remove %s: %w", why, swap.FilePending, err)
	}
	return true, errors.New(why)
}

// endedAfter reports whether home's last swap record (swap-last.json, the
// daemon writes it when a swapper records its end) is an upgrade that
// ended ok, rolled back or rolled forward after at.
func endedAfter(home string, at time.Time) bool {
	b, err := os.ReadFile(filepath.Join(home, server.FileLastSwap))
	if err != nil {
		return false
	}
	var w api.SwapRecord
	if json.Unmarshal(b, &w) != nil || w.What != "upgrade" || !w.At.After(at) {
		return false
	}
	switch w.Result {
	case "ok", "rolled_back", "rolled_forward":
		return true
	}
	return false
}

// swapperFinishWait bounds how long an updater waits, after SIGTERM, for
// its swapper's verify and rollback: above the swapper's own bound
// (swapFinishBudget from its commit, plus a record and its exit).
var swapperFinishWait = swapFinishBudget + 2*time.Minute

// swapperCommand runs a staged `zm daemon upgrade`. On ctx's end (the
// updater's SIGTERM, or its install timeout) the swapper gets SIGTERM, not
// exec's SIGKILL: past its commit it finishes the verify and rollback
// first, and the updater waits for its record.
func swapperCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	p := exec.CommandContext(ctx, name, args...)
	p.Cancel = func() error { return p.Process.Signal(syscall.SIGTERM) }
	p.WaitDelay = swapperFinishWait
	return p
}

func daemonUpdateCmd() *cobra.Command {
	var repo, dir, hold, quiet, from string
	var minAge time.Duration
	var keys []string
	var noCanary bool
	cmd := &cobra.Command{Use: "update", Short: "Pull the last green main (or the newest signed release) and safely upgrade this host (one shot)", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&repo, "repo", "", "local canonical checkout of the zm repository (this or --release)")
	cmd.Flags().StringVar(&dir, "dir", "", "installed zm directory (default: the running zm's, symlinks resolved)")
	cmd.Flags().StringVar(&hold, "hold-file", "", "skip while this file exists")
	cmd.Flags().StringVar(&quiet, "quiet-window", "", "daily hold HH:MM-HH:MM, host local time; equal endpoints hold all day")
	cmd.Flags().StringVar(&from, "release", "", "install the newest signed release instead of building: github, or a directory holding one release")
	cmd.Flags().DurationVar(&minAge, "min-age", 0, "with --release: take a release only once it is this old (staged rollout)")
	cmd.Flags().StringArrayVar(&keys, "key", nil, "with --release: a trusted signing key (ed25519:<base64>) besides config update_keys")
	cmd.Flags().BoolVar(&noCanary, "no-canary", false, "with --release and --min-age: take a release without another machine's vouch (time rings only)")
	cmd.AddCommand(updateHoldCmd(), updateResumeCmd(), updateStatusCmd(), updateEnableCmd(), updateDisableCmd())
	cmd.RunE = func(cmd *cobra.Command, _ []string) (ret error) {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Minute)
		defer cancel()
		c := client.NewUnix(cfg.SocketPath())
		rec := api.SwapRecord{What: "update", Result: "failed", At: time.Now().UTC()}
		// The release channel's order, read once for the whole check: this
		// machine's config update_repos, else the built-in order. A bad value
		// fails the check below, after the holds, never falls back.
		var repos []string
		var reposSet bool
		var reposErr error
		if from == "github" {
			repos, reposSet, reposErr = releaseRepos(cfg.Home)
		}
		sources := releaseSourcesNote(repos, reposSet && reposErr == nil)
		defer func() {
			if ret != nil {
				rec.Reason = ret.Error()
			}
			rec.Reason = withSources(rec.Reason, sources)
			b, _ := json.Marshal(rec)
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			// A stopped/down daemon cannot publish: stdout remains a durable scheduler
			// log, and the next invocation can report normally. Never autostart here.
			noteCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, e := c.RecordSwap(noteCtx, rec); e != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "zm: update event unavailable:", e)
			}
		}()
		if repo == "" && from == "" {
			return fmt.Errorf("one of --repo or --release is required")
		}
		if dir == "" {
			if _, dir, err = runningInstall(); err != nil {
				return err
			}
		}
		if from == "" {
			if repo, err = filepath.Abs(repo); err != nil {
				return err
			}
		}
		dir, err = filepath.Abs(dir)
		if err != nil {
			return err
		}
		if err = writableDir(dir); err != nil {
			return err
		}
		// Do not create a home/start a daemon merely because a timer fired.
		if _, err = c.Health(ctx); err != nil {
			return fmt.Errorf("update requires the running host daemon: %w", err)
		}
		lock, err := os.OpenFile(filepath.Join(cfg.Home, "update.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("another host update is running")
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		// The driver folder's own hold lifts after a while, or for a newer
		// release than the one it refused (daemon_update_autohold.go).
		newest := func() string {
			if reposErr != nil {
				return ""
			}
			return newestRelease(from, repos, reposSet)
		}
		if why := expireAutoHold(cfg.Home, time.Now(), newest); why != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "zm:", why)
		}
		held := func() (bool, error) {
			// The hold switch (zm daemon update hold) always counts, besides --hold-file.
			why, e := updateHeld(time.Now(), release.HoldPath(cfg.Home), "")
			if why == "" && e == nil {
				why, e = updateHeld(time.Now(), hold, quiet)
			}
			if why != "" {
				rec.Result, rec.Reason = "held", why
			}
			return why != "", e
		}
		if yes, e := held(); yes || e != nil {
			return e
		}
		if stop, e := unfinishedSwap(cfg.Home, from != "", &rec); stop || e != nil {
			return e
		}
		if from == "github" && len(cfg.File.UpdateKeys) > 0 {
			// A machine on the channel trusts this zm's own channel key: after
			// a rotation, the release that carries the new key pins it here
			// (a lent machine never runs enable again).
			if _, e := cfg.AddUpdateKey(release.ChannelKey); e != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "zm: could not pin this build's release key:", e)
			}
		}
		if reposErr != nil {
			return reposErr
		}
		if from != "" {
			return updateFromRelease(cmd, c, cfg.Home, append(append([]string{}, cfg.File.UpdateKeys...), keys...), from, repos, reposSet, dir, minAge, noCanary, &rec, held)
		}
		run := func(at, name string, args ...string) ([]byte, error) {
			p := exec.CommandContext(ctx, name, args...)
			p.Dir = at
			p.Env = updateEnv(os.Environ())
			b, e := p.CombinedOutput()
			if e != nil {
				return nil, fmt.Errorf("%s: %w: %s", name, e, b)
			}
			return b, nil
		}
		remote, err := run(repo, "git", "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		r := strings.TrimSuffix(strings.TrimSpace(string(remote)), ".git")
		if !strings.EqualFold(r, "https://github.com/"+updateBuildRepo) && !strings.EqualFold(r, "git@github.com:"+updateBuildRepo) &&
			!strings.EqualFold(r, "ssh://git@github.com/"+updateBuildRepo) {
			return fmt.Errorf("origin must be the canonical github.com/%s repository", updateBuildRepo)
		}
		data, err := run(repo, "gh", "api", "repos/"+updateBuildRepo+"/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=100")
		if err != nil {
			return err
		}
		sha, err := latestUpdate(data)
		if err != nil {
			return err
		}
		rec.To = sha
		health, err := c.Health(ctx)
		if err != nil {
			return err
		}
		rec.From = buildOf(health)
		if health.Commit != "" && (health.Commit == sha || len(health.Commit) >= 7 && strings.HasPrefix(sha, health.Commit)) {
			rec.Result = "unchanged"
			return nil
		}
		if err = checkMovedIssues(ctx, cfg.Home); err != nil {
			return err
		}
		if _, err = run(repo, "git", "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
			return err
		}
		if _, err = run(repo, "git", "merge-base", "--is-ancestor", sha, "refs/remotes/origin/main"); err != nil {
			return fmt.Errorf("green SHA is not an ancestor of canonical main: %w", err)
		}

		// Never undo a newer manual/bootstrap install while its main CI catches up.
		// Resolve only an unambiguous hex commit from the fetched canonical history.
		if !updateCommit.MatchString(health.Commit) {
			return fmt.Errorf("running daemon commit %q cannot be resolved safely; bootstrap a green main build manually", health.Commit)
		}
		currentBytes, err := run(repo, "git", "rev-parse", "--verify", health.Commit+"^{commit}")
		if err != nil {
			return fmt.Errorf("cannot resolve running daemon commit; fetch its history or bootstrap green main manually: %w", err)
		}
		current := strings.TrimSpace(string(currentBytes))
		if !updateSHA.MatchString(current) {
			return fmt.Errorf("git returned no unambiguous running commit")
		}
		if current == sha {
			rec.Result = "unchanged"
			return nil
		}
		if _, err = run(repo, "git", "merge-base", "--is-ancestor", sha, current); err == nil {
			rec.Result, rec.Reason = "held", "running build is newer than the last green main; waiting for main CI to catch up"
			return nil
		}
		if _, err = run(repo, "git", "merge-base", "--is-ancestor", current, sha); err != nil {
			return fmt.Errorf("running build is not an ancestor of the green candidate; refuse an automatic downgrade or divergent upgrade: %w", err)
		}
		tmp, err := updateScratch(cfg.Home, "zm-update-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		tree := filepath.Join(tmp, "source")
		if _, err = run(repo, "git", "worktree", "add", "--detach", tree, sha); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p := exec.CommandContext(cleanup, "git", "worktree", "remove", "--force", tree)
			p.Dir = repo
			p.Env = updateEnv(os.Environ())
			if b, e := p.CombinedOutput(); e != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "zm: remove update worktree: %v: %s\n", e, b)
			}
		}()
		stage := filepath.Join(tmp, "stage")
		if err = os.Mkdir(stage, 0700); err != nil {
			return err
		}
		flags := "-X github.com/sengirunners/potluck/kit/api.Version=main-" + sha[:12] + " -X github.com/sengirunners/potluck/kit/api.Commit=" + sha +
			" -X main.buildRepo=" + updateBuildRepo
		if _, err = run(tree, "go", append([]string{"build", "-ldflags", flags, "-o", stage + string(os.PathSeparator), "./cmd/..."}, potluckCmds...)...); err != nil {
			return err
		}
		if yes, e := held(); yes || e != nil {
			return e
		}
		if err = checkMovedIssues(ctx, cfg.Home); err != nil {
			return err
		}

		latest, err := c.Health(ctx)
		if err != nil {
			return err
		}
		if latest.Commit != health.Commit {
			return fmt.Errorf("daemon build changed during update build; retry on the next timer tick")
		}
		// Give verify/rollback its own budget; a long build must not consume the
		// tailnet grace or kill the swap just as it is starting.
		ctx, installCancel := updateInstallContext(cmd.Context())
		defer installCancel()
		// This is make install's staged-binary path. It preflights and owns the
		// swap lock, session/KillMode guard, verify and rollback/roll-forward.
		up := swapperCommand(ctx, filepath.Join(stage, "zm"), "daemon", "upgrade", filepath.Join(stage, "zm"), "--dir", dir)
		up.Dir, up.Env = tree, updateEnv(os.Environ())
		output, err := up.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("zm daemon upgrade: %w%s", err, swapperWhy(output))
		}
		if len(output) > 0 {
			fmt.Fprint(cmd.OutOrStdout(), string(output))
		}
		if err != nil {
			return err
		}
		rec.Result = "ok"
		return nil
	}
	return cmd
}

// updateHoldCmd is the hold switch: while $ZM_HOME/update.hold exists, no
// update installs anything here; the machine keeps the build it runs.
func updateHoldCmd() *cobra.Command {
	return &cobra.Command{Use: "hold [reason...]", Short: "Hold this machine's automatic updates: it keeps the build it runs", Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h := release.Hold{Reason: strings.TrimSpace(strings.Join(args, " ")), Since: time.Now().UTC()}
			if err := release.WriteHold(release.HoldPath(cfg.Home), h); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updates held on %s (resume: zm daemon update resume)\n", cfg.Machine)
			return nil
		}}
}

func updateResumeCmd() *cobra.Command {
	return &cobra.Command{Use: "resume", Short: "Resume this machine's automatic updates (undo hold)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			switch err := os.Remove(release.HoldPath(cfg.Home)); {
			case err == nil:
				fmt.Fprintf(cmd.OutOrStdout(), "updates resumed on %s: the next check may install\n", cfg.Machine)
			case os.IsNotExist(err):
				fmt.Fprintf(cmd.OutOrStdout(), "%s was not held by zm daemon update hold (a --hold-file or quiet window may still hold it: zm daemon update status)\n", cfg.Machine)
			default:
				return err
			}
			return nil
		}}
}

// updateStatusCmd lists each machine's zm and release channel state from
// machine numbers (GET /v0/machines): this one, and on a team every
// member machine. On this machine's own daemon it ends with the order of
// release repositories this machine reads (its config, so only here).
func updateStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Each machine's zm build, hold and last update check", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			local := false
			return call(cmd, func(ctx context.Context, c *client.Client) (api.MachineList, error) {
				local = c.Socket != ""
				return c.Machines(ctx)
			}, func(l api.MachineList) string {
				s := updateStatusText(l)
				if cfg, err := loadConfig(); err == nil && local {
					s += releaseSourcesLine(cfg.Home, cfg.Machine)
				}
				return s
			})
		}}
}

func updateStatusText(l api.MachineList) string {
	var b strings.Builder
	for _, m := range l.Machines {
		z := m.Stats.Build()
		if z == nil {
			fmt.Fprintf(&b, "%-20s %-18s %s\n", m.Machine, "-", "(older zm: no update state)")
			continue
		}
		state := "no update check yet"
		if u := z.Last; u != nil {
			state = u.Result + " " + ago(u.At) + " ago"
			if u.Reason != "" {
				state += ": " + u.Reason
			}
		}
		if z.Held != "" {
			state = "HELD since " + z.HeldSince.Local().Format(time.DateTime) + " (" + z.Held + "); " + state
		}
		if z.Release != "" && slices.Contains(z.Stable, z.Release) {
			state += "; stable"
		}
		if len(z.Failed) > 0 {
			state += "; failed here: " + strings.Join(z.Failed, ", ")
		}
		fmt.Fprintf(&b, "%-20s %-18s %s\n", m.Machine, z.Version, state)
	}
	return b.String()
}

// swapperWhy is a staged swapper's refusal in its own words, for the
// update's record (machine numbers' last check): the preflight's problems
// ("  - " lines), else its last line. "" when it said nothing.
func swapperWhy(out []byte) string {
	var probs []string
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "  - "); ok {
			probs = append(probs, strings.TrimSpace(p))
		}
	}
	if len(probs) == 0 {
		if l := strings.TrimSpace(lastOf(string(out))); l != "" {
			probs = []string{l}
		}
	}
	if len(probs) == 0 {
		return ""
	}
	return ": " + strings.Join(probs, "; ")
}
