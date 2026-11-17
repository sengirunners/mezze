package main

// zm daemon update --release (SPEC 2 "Release channel"): a machine pulls a
// signed release and installs it through the ordinary guarded upgrade.
// Nothing is pushed to it. It walks the channel newest first and takes the
// newest release that is --min-age old (its ring), did not fail here, and,
// behind the canary, that another machine vouches for; never one older than
// what this home installed. The manifest must verify with a key in config
// update_keys and the tarball must match it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/spf13/cobra"
)

// releaseTagPrefix starts every release tag the channel publishes:
// zm-main-<sha12>, whose version is main-<sha12>.
const releaseTagPrefix = "zm-main-"

// releaseDecision says what to do with verified manifest m on a daemon
// running commit, given what this home installed last and the minimum age.
// It returns "" to install, else the held/unchanged reason. It re-checks
// with signed data what the chooser read from the unsigned listing.
func releaseDecision(m release.Manifest, running string, st release.State, haveState bool, minAge time.Duration, now time.Time) (result, reason string) {
	if release.SameCommit(running, m.Commit) {
		return "unchanged", ""
	}
	if haveState && !m.Created.After(st.Created) {
		return "held", fmt.Sprintf("release %s is not newer than %s, installed from the channel already", m.Version, st.Version)
	}
	if age := now.Sub(m.Created); age < minAge {
		return "held", fmt.Sprintf("release %s is %s old; this machine takes releases at %s", m.Version, age.Truncate(time.Minute), minAge)
	}
	return "", ""
}

// listed is one release in the channel's listing (GitHub's: not signed).
type listed struct {
	Tag       string
	Version   string
	Published time.Time
}

// commitOf is the commit (abbreviated) a channel version names.
func commitOf(version string) string { return strings.TrimPrefix(version, "main-") }

// gate says whether another machine vouches for a release and none vetoes
// it: "" when it does, else why not (the canary gate).
type gate func(version string) string

// choice is the chooser's answer: install pick, or result and reason. pick
// is also set when the walk stopped at the running build's release.
type choice struct {
	pick           listed
	install        bool
	result, reason string
}

// chooseRelease walks the listing newest first. It stops at the running
// build's release (unchanged) or at what this home installed (never a
// rollback), and otherwise takes the first release that is minAge old,
// did not fail here and passes the gate. When it skipped newer releases
// before stopping, the result is held, with the first one's reason.
func chooseRelease(rs []listed, running string, st release.State, have bool, minAge time.Duration, now time.Time, g gate) choice {
	skipped := ""
	stop := func(pick listed, result, why string) choice {
		if skipped != "" {
			return choice{pick: pick, result: "held", reason: skipped}
		}
		return choice{pick: pick, result: result, reason: why}
	}
	for _, r := range rs {
		switch {
		case release.SameCommit(commitOf(r.Version), running):
			return stop(r, "unchanged", "")
		case have && (r.Version == st.Version || r.Published.Before(st.Created)):
			return stop(listed{}, "held", fmt.Sprintf("release %s is not newer than %s, installed from the channel already", r.Version, st.Version))
		}
		why := ""
		if age := now.Sub(r.Published); age < minAge {
			why = fmt.Sprintf("release %s is %s old; this machine takes releases at %s", r.Version, age.Truncate(time.Minute), minAge)
		} else if f, ok := st.FailedHere(r.Version); ok {
			why = fmt.Sprintf("release %s failed here at %s (%s); waiting for a newer one", r.Version, f.At.Format(time.RFC3339), f.Result)
		} else if g != nil {
			why = g(r.Version)
		}
		if why == "" {
			return choice{pick: r, install: true}
		}
		if skipped == "" {
			skipped = why
		}
	}
	return stop(listed{}, "held", "no release in the channel is newer than this build")
}

// vouch is the canary gate over machine numbers (GET /v0/machines): a
// release passes when another machine lists it as stable (it ran it for
// release.Soak with no daemon restart) and no machine lists it as failed.
func vouch(ms []api.Machine, self, version string) string {
	by := ""
	for _, m := range ms {
		z := m.Stats.Build()
		if z == nil {
			continue
		}
		if slices.Contains(z.Failed, version) {
			return fmt.Sprintf("release %s failed on %s; waiting for a newer one", version, m.Machine)
		}
		if by == "" && m.Machine != self && slices.Contains(z.Stable, version) {
			by = m.Machine
		}
	}
	if by == "" {
		return fmt.Sprintf("no other machine vouches for release %s yet (one must run it %s with no restart)", version, release.Soak)
	}
	return ""
}

// releaseSource fetches release files: from GitHub through gh ("github"),
// or from a directory holding one release's files (staging, tests).
type releaseSource interface {
	// list is the channel's releases, newest first.
	list() ([]listed, error)
	// open fetches tag's manifest and signature; file then reads that
	// release.
	open(tag string) (manifest, sig []byte, err error)
	// file fetches one file of the opened release.
	file(name string) ([]byte, error)
	// ahead reports whether the release may not replace the running build,
	// and why: the build is newer than (or beside) the release's commit, so
	// taking it would go backwards, or nothing can place the two.
	ahead(running, commit string) (bool, string, error)
}

type dirSource struct{ dir string }

// list reads the directory's manifest without verifying it, only to list
// it: open's caller verifies it.
func (d dirSource) list() ([]listed, error) {
	b, err := os.ReadFile(filepath.Join(d.dir, release.ManifestFile))
	if err != nil {
		return nil, err
	}
	var m release.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return []listed{{Version: m.Version, Published: m.Created}}, nil
}

func (d dirSource) open(string) ([]byte, []byte, error) {
	m, err := os.ReadFile(filepath.Join(d.dir, release.ManifestFile))
	if err != nil {
		return nil, nil, err
	}
	s, err := os.ReadFile(filepath.Join(d.dir, release.SignatureFile))
	return m, s, err
}

// ahead: a directory carries no history; its release is taken as given.
func (dirSource) ahead(string, string) (bool, string, error) { return false, "", nil }

func (d dirSource) file(name string) ([]byte, error) {
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("release file %q", name)
	}
	return os.ReadFile(filepath.Join(d.dir, name))
}

// githubSource reads the channel's releases with the machine's own gh login
// (read access to one of its repos is all it needs). list picks the repo
// (the first that has releases); open and file then stay on it, so a
// release's manifest and tarball never come from two repos. ahead asks that
// repo first and may ask the others: history only vetoes, it never supplies
// a file.
type githubSource struct {
	run  func(name string, args ...string) ([]byte, error)
	tmp  string
	tag  string
	repo string
	// repos is the order list walks; nil is updateRepos, the built-in one.
	repos []string
	// set: repos is this machine's config update_repos. Its repositories
	// are not the built-in ones, so their answers about history are checked
	// against the commits asked (a branch or tag may carry a commit's name):
	// one that does not name them holds.
	set bool
}

// order is the repositories list walks, in order. A machine's own order is
// never the built-in one in disguise: set with no repos lists nothing.
func (g *githubSource) order() []string {
	if g.set || g.repos != nil {
		return g.repos
	}
	return updateRepos
}

// from is the repo list chose, or the first one before it has.
func (g *githubSource) from() string {
	if g.repo != "" {
		return g.repo
	}
	if o := g.order(); len(o) > 0 {
		return o[0]
	}
	return ""
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Created    time.Time `json:"created_at"` // GitHub: the release commit's date
	Published  time.Time `json:"published_at"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// channelReleases is the channel in GitHub's listing, newest published
// first. Drafts, prereleases (a yanked release), other tags (app previews)
// and releases without a manifest are never taken.
func channelReleases(data []byte, repo string) ([]listed, error) {
	var rs []ghRelease
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, err
	}
	var out []listed
	for _, r := range rs {
		sha, ok := strings.CutPrefix(r.Tag, releaseTagPrefix)
		if r.Draft || r.Prerelease || !ok || !updateCommit.MatchString(sha) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == release.ManifestFile {
				at := r.Published
				if at.IsZero() {
					at = r.Created
				}
				out = append(out, listed{Tag: r.Tag, Version: "main-" + sha, Published: at})
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no signed release on %s yet", repo)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	return out, nil
}

// releaseAccess turns gh's refusals into one plain sentence: no gh, no
// login, or (while the repository is private) an account without read
// access, which GitHub answers with a 404. Any other error is kept.
func releaseAccess(err error, repo string) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("zm updates itself from its signed releases on GitHub, which it reads with the gh command, and gh is not installed here: install it (https://cli.github.com), then run gh auth login")
	case strings.Contains(s, "gh auth login"):
		return errors.New("gh is not logged in to GitHub on this machine, so zm cannot read its releases: run gh auth login")
	case strings.Contains(strings.ToLower(s), "rate limit"):
		return errors.New("GitHub's rate limit stopped this update check; the next one tries again")
	case repoUnreadable(err):
		return fmt.Errorf("GitHub did not let this machine's gh login read zm's releases: while github.com/%s is private, only accounts with read access to it can download them (ask the pool's owner for access)", repo)
	case strings.Contains(s, "HTTP 403"):
		return fmt.Errorf("GitHub refused this machine's gh login on github.com/%s (HTTP 403, not the rate limit: an organization's SSO may need the login authorized, gh auth refresh); the next check tries again: %w", repo, err)
	}
	return err
}

// repoUnreadable is GitHub's answer for a repository this login cannot see
// (private, or not there yet) or a commit the repository does not hold: a
// 404. A 403 (an SSO the login is not authorized for, a rate limit) is a
// refusal that may pass, never a sign the repo lacks something.
func repoUnreadable(err error) bool {
	return strings.Contains(err.Error(), "HTTP 404")
}

// list walks its repos in order and takes the first repo that answers with
// channel releases. A repo that cannot be read (404), or has none yet, passes
// to the next; any other failure (no gh, no login, a 403, a rate limit, the
// network) stops here, so a repo that is only unreachable now is never skipped
// for an older one. If none has a release, the error names each repo's answer.
func (g *githubSource) list() ([]listed, error) {
	if len(g.order()) == 0 {
		return nil, errors.New("config " + updateReposKey + " lists no repository")
	}
	var whys []error
	for _, repo := range g.order() {
		data, err := g.run("gh", "api", "repos/"+repo+"/releases?per_page=100")
		if err != nil {
			// the first call: past it, access is proven and a 404 means something else
			if !repoUnreadable(err) {
				return nil, releaseAccess(err, repo)
			}
			err = releaseAccess(err, repo)
		} else {
			var rs []listed
			if rs, err = channelReleases(data, repo); err == nil {
				g.repo = repo
				if g.set {
					return g.unyanked(rs)
				}
				return rs, nil
			}
		}
		whys = append(whys, err)
	}
	if len(whys) == 1 {
		return nil, whys[0]
	}
	s := make([]string, len(whys))
	for i, e := range whys {
		s[i] = e.Error()
	}
	return nil, errors.New("no update repo has zm's releases for this machine: " + strings.Join(s, "; "))
}

// unyanked drops, on a machine's own order, a release that another listed
// repo or a built-in one yanked (marked prerelease, or holds as a draft): a
// yank is made on one repo, and the order must not undo it. A yank is per
// tag. A repo this login cannot read (404) says nothing; any other failure
// stops the check, as in list.
func (g *githubSource) unyanked(rs []listed) ([]listed, error) {
	yanked := map[string]bool{}
	var asked []string
	for _, repo := range append(slices.Clone(g.order()), updateRepos...) {
		if strings.EqualFold(repo, g.repo) || slices.ContainsFunc(asked, func(a string) bool { return strings.EqualFold(a, repo) }) {
			continue
		}
		asked = append(asked, repo)
		data, err := g.run("gh", "api", "repos/"+repo+"/releases?per_page=100")
		if err != nil {
			if repoUnreadable(err) {
				continue
			}
			return nil, releaseAccess(err, repo)
		}
		var all []ghRelease
		if err := json.Unmarshal(data, &all); err != nil {
			return nil, fmt.Errorf("the releases of %s: %w", repo, err)
		}
		for _, r := range all {
			if r.Draft || r.Prerelease {
				yanked[r.Tag] = true
			}
		}
	}
	return slices.DeleteFunc(rs, func(r listed) bool { return yanked[r.Tag] }), nil
}

func (g *githubSource) open(tag string) ([]byte, []byte, error) {
	g.tag = tag
	m, err := g.file(release.ManifestFile)
	if err != nil {
		return nil, nil, err
	}
	s, err := g.file(release.SignatureFile)
	return m, s, err
}

// sourceTrailer is the line in a public release commit's message naming the
// private commit scripts/publish copied it from.
var sourceTrailer = regexp.MustCompile(`(?m)^Source: ([0-9a-f]{40})\s*$`)

// ahead asks GitHub how the running commit relates to the release's: a
// build ahead of it (a hand install) or off its line is never replaced. It
// asks the release's repo first, then its other repos: a repo published
// from one commit does not hold the history the running build came from, and
// GitHub answers a compare it cannot place with a 404. Then, when the release
// commit names the commit it was copied from (its Source: line), it compares
// that one the same way, so a public release is placed on the private repo and
// one copied from an older snapshot never replaces a newer build. When nothing
// places the two, it holds: build times say nothing about whose code is newer.
func (g *githubSource) ahead(running, commit string) (bool, string, error) {
	if !updateCommit.MatchString(running) {
		return true, fmt.Sprintf("the running build's commit %q is not a commit; install a release by hand once", running), nil
	}
	placed, newer, why, err := g.compare(commit, running)
	if placed || err != nil {
		return newer, why, err
	}
	src, err := g.source(commit)
	if err != nil {
		return false, "", err
	}
	if src != "" {
		if placed, newer, why, err = g.compare(src, running); placed || err != nil {
			if newer {
				why += fmt.Sprintf(" (release commit %s is a copy of %s)", commit[:min(12, len(commit))], src[:12])
			}
			return newer, why, err
		}
	}
	return true, fmt.Sprintf("no update repo can place the running build %s against the release's commit %s or a source it names; held: install a release by hand (zm daemon upgrade) to rejoin the channel", running, commit[:min(12, len(commit))]), nil
}

// compare places running against base on the release's repo, then its other
// repos; a 404 (a repo without one of them) passes to the next. placed
// is false when every repo answered 404.
func (g *githubSource) compare(base, running string) (placed, newer bool, why string, err error) {
	repos := append([]string{g.from()}, slices.DeleteFunc(slices.Clone(g.order()), func(r string) bool { return r == g.from() })...)
	jq := ".status"
	if g.set {
		jq = `.status+" "+.base_commit.sha+" "+.merge_base_commit.sha`
	}
	for _, repo := range repos {
		out, err := g.run("gh", "api", "repos/"+repo+"/compare/"+base+"..."+running, "--jq", jq)
		if err == nil {
			st := strings.TrimSpace(string(out))
			if g.set && !comparedAsked(st, base, running) {
				return true, true, fmt.Sprintf("github.com/%s answered the compare of the release with the running build for other commits than asked; held", repo), nil
			}
			st, _, _ = strings.Cut(st, " ")
			newer, why := compareAhead(st)
			return true, newer, why, nil
		}
		if !repoUnreadable(err) {
			return false, false, "", fmt.Errorf("compare the running build with the release: %w", releaseAccess(err, repo))
		}
	}
	return false, false, "", nil
}

// comparedAsked reports whether a compare answer (status, base commit, merge
// base) is about the commits asked: an answer that takes the release
// ("behind") must have compared base itself and found running as the merge
// base. Any other status holds anyway.
func comparedAsked(answer, base, running string) bool {
	f := strings.Fields(answer)
	if len(f) != 3 {
		return false
	}
	if f[0] != "behind" {
		return true
	}
	return f[1] == base && updateSHA.MatchString(f[2]) && release.SameCommit(f[2], running)
}

// source is the commit the release's commit was copied from, read from its
// Source: line on the release's repo; "" when it names none. On a machine's
// own order the answer must be about that commit, else it names none.
func (g *githubSource) source(commit string) (string, error) {
	jq := ".commit.message"
	if g.set {
		jq = `.sha+" "+.commit.message`
	}
	out, err := g.run("gh", "api", "repos/"+g.from()+"/commits/"+commit, "--jq", jq)
	if err != nil {
		if repoUnreadable(err) {
			return "", nil
		}
		return "", fmt.Errorf("read the release's commit: %w", releaseAccess(err, g.from()))
	}
	if g.set {
		sha, msg, _ := strings.Cut(string(out), " ")
		if sha != commit {
			return "", nil
		}
		out = []byte(msg)
	}
	m := sourceTrailer.FindSubmatch(out)
	if m == nil || release.SameCommit(string(m[1]), commit) {
		return "", nil
	}
	return string(m[1]), nil
}

// compareAhead reads GitHub's compare status of release...running.
func compareAhead(st string) (bool, string) {
	switch st {
	case "behind":
		return false, ""
	case "ahead", "identical":
		return true, "the running build is newer than the release; waiting for a newer one"
	default:
		return true, fmt.Sprintf("the running build is %s from the release's line; install by hand", st)
	}
}

func (g *githubSource) file(name string) ([]byte, error) {
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("release file %q", name)
	}
	if _, err := g.run("gh", "release", "download", g.tag, "--repo", g.from(), "--pattern", name, "--dir", g.tmp, "--clobber"); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(g.tmp, name))
}

// fetchRelease chooses a release from src and, when it is to be installed,
// verifies it and writes its programs into stage. install is false with
// the held/unchanged result and reason. A hand install of a channel
// release joins the channel: st records it (the caller writes st), unless
// that release failed here.
func fetchRelease(src releaseSource, keys []string, version, running string, st *release.State, have bool, minAge time.Duration, now time.Time, g gate, stage string) (m release.Manifest, install bool, result, reason string, err error) {
	rs, err := src.list()
	if err != nil {
		return m, false, "", "", err
	}
	c := chooseRelease(rs, running, *st, have, minAge, now, g)
	if !c.install {
		if f, failed := st.FailedHere(c.pick.Version); failed && c.result == "unchanged" {
			// The running build's release failed here (an unverified swap):
			// it never joins as installed, so never as stable.
			return m, false, c.result, fmt.Sprintf("release %s failed here at %s (%s); not recorded as installed", c.pick.Version, f.At.Format(time.RFC3339), f.Result), nil
		}
		if c.pick.Version != "" && !have {
			if body, sig, e := src.open(c.pick.Tag); e == nil {
				if v, e := release.Verify(body, string(sig), keys); e == nil && v.RunBy(version, running) {
					m, *st = v, st.Installing(v, now)
				}
			}
		}
		return m, false, c.result, c.reason, nil
	}
	body, sig, err := src.open(c.pick.Tag)
	if err != nil {
		return m, false, "", "", err
	}
	if m, err = release.Verify(body, string(sig), keys); err != nil {
		return m, false, "", "", err
	}
	if m.Version != c.pick.Version {
		return m, false, "", "", fmt.Errorf("release %s carries the manifest of %s", c.pick.Version, m.Version)
	}
	if result, reason = releaseDecision(m, running, *st, have, minAge, now); result != "" {
		return m, false, result, reason, nil
	}
	if newer, why, err := src.ahead(running, m.Commit); err != nil || newer {
		return m, false, "held", why, err
	}
	name := release.Tarball(m.Version, runtime.GOOS, runtime.GOARCH)
	tb, err := src.file(name)
	if err != nil {
		return m, false, "", "", err
	}
	if err = m.CheckFile(name, tb); err != nil {
		return m, false, "", "", err
	}
	if _, err = release.Extract(tb, m.Version, runtime.GOOS, runtime.GOARCH, stage); err != nil {
		return m, false, "", "", err
	}
	return m, true, "", "", nil
}

// noteStable records what the running daemon proves before anything is
// installed: the release it runs is stable once it has run release.Soak
// with no restart since its install. A state from before installed times
// starts counting now. Whether it runs that release is Potluck's one
// comparison (release.State.Runs): a source build at its commit is not it.
func noteStable(st *release.State, have bool, h api.Health, now time.Time) bool {
	if !have || st.Runs(true, h.Version, h.Commit) != release.RunsInstalled {
		return false
	}
	dirty := false
	if st.Installed.IsZero() {
		st.Installed, dirty = now.UTC(), true
	}
	if st.StableNow(h.Version, h.Commit, h.Started, now) && st.MarkStable(st.Version) {
		dirty = true
	}
	return dirty
}

// forgiveFailure is the way back from a failed-here record:
// when the daemon's last swap is an upgrade to the build it runs that
// ended ok after the failure (a person reinstalled that release by hand,
// zm daemon upgrade, and it verified), the release is no longer failed
// here; this tick may then join it as installed.
func forgiveFailure(st *release.State, h api.Health) bool {
	w := h.LastSwap
	if w == nil || w.What != "upgrade" || w.Result != "ok" || h.Version == "" {
		return false
	}
	f, failed := st.FailedHere(h.Version)
	if !failed || !w.At.After(f.At) {
		return false
	}
	v, c, _ := strings.Cut(w.To, " (")
	if v != h.Version || !release.SameCommit(strings.TrimSuffix(c, ")"), h.Commit) {
		return false
	}
	return st.Forgive(h.Version)
}

// installFailure is how the upgrade that began at start failed, when that
// is the release's doing: it attempted a swap that did not end ok (its
// record), the daemon restarted or changed build with no record (a swap
// whose record never reached the daemon), or no daemon answers after it.
// restarted reports a daemon restart since before. A refusal before
// anything moved (preflight, managed sessions, swap.lock) is the host's,
// retried next tick: false.
func installFailure(c *client.Client, start time.Time, version string, before api.Health) (f release.Failure, failed, restarted bool) {
	var err error
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var h api.Health
		h, err = c.Health(ctx)
		cancel()
		if err == nil {
			return judgeInstall(h, start, version, before)
		}
		time.Sleep(2 * time.Second)
	}
	return release.Failure{Version: version, At: time.Now().UTC(), Result: "failed", Reason: "no daemon answers after the upgrade: " + err.Error()}, true, true
}

// judgeInstall is installFailure's verdict from the daemon's health after
// a failed upgrade.
func judgeInstall(h api.Health, start time.Time, version string, before api.Health) (release.Failure, bool, bool) {
	restarted := h.Started.After(before.Started)
	if w := h.LastSwap; w != nil && w.What == "upgrade" && !w.At.Before(start) && w.Result != "ok" {
		return release.Failure{Version: version, At: w.At, Result: w.Result, Reason: w.Reason}, true, restarted
	}
	if restarted || h.Commit != before.Commit {
		return release.Failure{Version: version, At: time.Now().UTC(), Result: "failed",
			Reason: fmt.Sprintf("the upgrade failed with no swap record and the daemon restarted (now %s)", h.Commit)}, true, true
	}
	return release.Failure{}, false, false
}

// releaseRunner runs a command with the update's clean environment.
func releaseRunner(env []string) func(name string, args ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		p := exec.Command(name, args...)
		p.Env = env
		b, err := p.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(b)))
		}
		return b, nil
	}
}

// updateFromRelease is `zm daemon update --release`: choose, verify, then
// the same staged swap the source path runs (preflight, swap lock, session
// and KillMode guards, verify, rollback or roll-forward). Behind the canary
// (minAge above 0, unless noCanary) a release needs another machine's vouch.
func updateFromRelease(cmd *cobra.Command, c *client.Client, home string, keys []string, from string, repos []string, reposSet bool, dir string, minAge time.Duration, noCanary bool, rec *api.SwapRecord, held func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Minute)
	defer cancel()
	health, err := c.Health(ctx)
	if err != nil {
		return err
	}
	rec.From = buildOf(health)
	now := time.Now()
	st, have, err := release.LoadState(home)
	if err != nil {
		rec.Result, rec.Reason = "held", err.Error()
		return nil
	}
	forgiven := forgiveFailure(&st, health)
	if noteStable(&st, have, health, now) || forgiven {
		if err := release.WriteState(home, st); err != nil {
			return err
		}
	}
	tmp, err := updateScratch(home, "zm-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	stage := filepath.Join(tmp, "stage")
	if err = os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	env := updateEnv(os.Environ())
	var src releaseSource
	if from == "github" {
		src = &githubSource{run: releaseRunner(env), tmp: tmp, repos: repos, set: reposSet}
	} else {
		abs, err := filepath.Abs(from)
		if err != nil {
			return err
		}
		src = dirSource{dir: abs}
	}
	rl := &listRecorder{releaseSource: src}
	src = rl
	var g gate
	if minAge > 0 && !noCanary {
		ml, err := c.Machines(ctx)
		if err != nil {
			why := "the canary gate cannot read machine numbers (" + err.Error() + "); --no-canary relies on --min-age alone"
			g = func(string) string { return why }
		} else {
			g = func(v string) string {
				why := vouch(ml.Machines, health.Machine, v)
				if why != "" && len(ml.Skipped) > 0 {
					why += "; machine numbers incomplete: " + ml.Skipped[0].Error
				}
				return why
			}
		}
	}
	joined := st
	m, install, result, reason, err := fetchRelease(src, keys, health.Version, health.Commit, &joined, have, minAge, now, g, stage)
	// The canary rollout's dry run: a record of what it would
	// have done; the tick below is unchanged.
	dryRunRollout(ctx, cmd.ErrOrStderr(), c, home, dir, health, rl, st, have, result, reason, err, now)
	if m.Commit != "" {
		rec.To = m.Version + " (" + m.Commit + ")"
	}
	if err != nil {
		return err
	}
	if !install {
		if !have && joined.Version != "" {
			_ = release.WriteState(home, joined) // a hand install of the same build joins the channel
		}
		rec.Result, rec.Reason = result, reason
		return nil
	}
	if yes, e := held(); yes || e != nil {
		return e
	}
	if err = checkMovedIssues(ctx, home); err != nil {
		return err
	}
	latest, err := c.Health(ctx)
	if err != nil {
		return err
	}
	if latest.Commit != health.Commit {
		return fmt.Errorf("daemon build changed during the release download; retry on the next timer tick")
	}
	ictx, icancel := updateInstallContext(cmd.Context())
	defer icancel()
	start := time.Now()
	p := swapperCommand(ictx, filepath.Join(stage, "zm"), "daemon", "upgrade", filepath.Join(stage, "zm"), "--dir", dir)
	p.Env = env
	out, err := p.CombinedOutput()
	if len(out) > 0 {
		fmt.Fprint(cmd.OutOrStdout(), string(out))
	}
	if err != nil {
		if f, ok, restarted := installFailure(c, start, m.Version, health); ok {
			st.MarkFailed(f)
			if have && (f.Result == "rolled_back" || restarted) && st.Runs(true, health.Version, health.Commit) == release.RunsInstalled {
				st.Installed = time.Now().UTC() // the rollback restarted the release this home runs: its soak starts over
			}
			if e := release.WriteState(home, st); e != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "zm: could not record the failed release:", e)
			}
		}
		return fmt.Errorf("zm daemon upgrade: %w%s", err, swapperWhy(out))
	}
	if err = release.WriteState(home, st.Installing(m, time.Now())); err != nil {
		return fmt.Errorf("installed %s but could not record it: %w", m.Version, err)
	}
	rec.Result = "ok"
	return nil
}

// updateScratch makes an update's scratch directory under home, on disk, not
// in the system temp: a shared machine's /tmp may be a small RAM tmpfs with a
// per-user quota that other jobs fill, and a release's staged programs are
// hundreds of megabytes (a full quota fails the staging with EDQUOT). The caller
// holds update.lock, so a scratch directory already there is a killed earlier
// run's leftover and goes first.
func updateScratch(home, prefix string) (string, error) {
	root := filepath.Join(home, "update-tmp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadDir(root); err == nil {
		for _, e := range old {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	return os.MkdirTemp(root, prefix)
}
