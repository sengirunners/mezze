package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/release"
)

const relCommit = "1eacf23e722f2df10a113480c8496e29607ba005"

// releaseDir writes one signed release for this platform into a new
// directory and returns it, the trusted key and the manifest.
func releaseDir(t *testing.T, created time.Time, zmBody string) (string, string, release.Manifest) {
	t.Helper()
	version := "main-" + relCommit[:12]
	name := release.Tarball(version, runtime.GOOS, runtime.GOARCH)
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	prefix := strings.TrimSuffix(name, ".tar.gz")
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755})
	for f, body := range map[string]string{"zm": zmBody, "bd-zm": "bd", "README.md": "docs"} {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + f, Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	m, err := release.FromSums(version, relCommit, created, []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	body, sig, err := release.Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, release.ManifestFile), body, 0o644)
	_ = os.WriteFile(filepath.Join(dir, release.SignatureFile), []byte(sig+"\n"), 0o644)
	return dir, release.FormatKey(pub), m
}

func TestReleaseInstallsOnlyWhatVerifies(t *testing.T) {
	dir, key, m := releaseDir(t, time.Now().Add(-time.Hour), "new zm")
	stage := t.TempDir()
	fetch := func(keys []string, stage string) (release.Manifest, bool, string, error) {
		var st release.State
		got, install, result, _, err := fetchRelease(dirSource{dir}, keys, "main-080d8123", "080d8123", &st, false, 0, time.Now(), nil, stage)
		return got, install, result, err
	}
	got, install, result, err := fetch([]string{key}, stage)
	if err != nil || !install || result != "" || got.Commit != m.Commit {
		t.Fatalf("install: %v %v %q %v", got.Version, install, result, err)
	}
	if b, _ := os.ReadFile(filepath.Join(stage, "zm")); string(b) != "new zm" {
		t.Fatalf("staged zm: %q", b)
	}
	if _, err := os.Stat(filepath.Join(stage, "README.md")); err == nil {
		t.Fatal("docs staged")
	}

	// An untrusted key, a changed tarball, a changed manifest: nothing staged.
	_, other, _ := releaseDir(t, time.Now(), "x")
	if _, _, _, err := fetch([]string{other}, t.TempDir()); err == nil {
		t.Fatal("a release signed by another key installed")
	}
	if _, _, _, err := fetch(nil, t.TempDir()); err == nil {
		t.Fatal("installed with no keys configured")
	}
	tb := filepath.Join(dir, release.Tarball(m.Version, runtime.GOOS, runtime.GOARCH))
	orig, _ := os.ReadFile(tb)
	_ = os.WriteFile(tb, append(append([]byte{}, orig...), 0), 0o644)
	if _, _, _, err := fetch([]string{key}, t.TempDir()); err == nil {
		t.Fatal("a changed tarball installed")
	}
	_ = os.WriteFile(tb, orig, 0o644)
	mf := filepath.Join(dir, release.ManifestFile)
	body, _ := os.ReadFile(mf)
	_ = os.WriteFile(mf, bytes.Replace(body, []byte(`"created"`), []byte(`"created" `), 1), 0o644)
	if _, _, _, err := fetch([]string{key}, t.TempDir()); err == nil {
		t.Fatal("a changed manifest installed")
	}
	_ = os.WriteFile(mf, body, 0o644)

	// Behind the canary, a release no machine vouches for stages nothing.
	var st release.State
	empty := t.TempDir()
	no := func(string) string { return "no vouch" }
	if _, install, result, why, err := fetchRelease(dirSource{dir}, []string{key}, "main-080d8123", "080d8123", &st, false, time.Minute, time.Now(), no, empty); err != nil || install || result != "held" || why != "no vouch" {
		t.Fatalf("unvouched: install=%v %q %q %v", install, result, why, err)
	}
	if fs, _ := os.ReadDir(empty); len(fs) > 0 {
		t.Fatal("an unvouched release was staged")
	}
	// A hand install of the release joins the channel.
	if _, install, result, _, err := fetchRelease(dirSource{dir}, []string{key}, m.Version, relCommit, &st, false, 0, time.Now(), nil, t.TempDir()); err != nil || install || result != "unchanged" || st.Version != m.Version || st.Installed.IsZero() {
		t.Fatalf("join: install=%v %q %+v %v", install, result, st, err)
	}
	// A source build at the release's commit (make install: a short commit)
	// is not the release (release.State.Runs): it never joins as installed.
	var src release.State
	if _, install, result, _, err := fetchRelease(dirSource{dir}, []string{key}, "zm-main-1eacf23-dirty", relCommit[:7], &src, false, 0, time.Now(), nil, t.TempDir()); err != nil || install || result != "unchanged" || src.Version != "" {
		t.Fatalf("source build joined: install=%v %q %+v %v", install, result, src, err)
	}
}

func TestReleaseDecision(t *testing.T) {
	now := time.Now()
	m := release.Manifest{Version: "main-1eacf23e722f", Commit: relCommit, Created: now.Add(-2 * time.Hour)}
	older := release.State{Version: "main-old", Created: now.Add(-48 * time.Hour)}
	same := release.State{Version: m.Version, Created: m.Created}
	newer := release.State{Version: "main-new", Created: now.Add(-time.Hour)}
	for _, c := range []struct {
		name    string
		running string
		st      release.State
		have    bool
		minAge  time.Duration
		want    string
	}{
		{"fresh home takes it", "080d8123", release.State{}, false, 0, ""},
		{"newer than installed", "080d8123", older, true, 0, ""},
		{"already running it", relCommit[:8], older, true, 0, "unchanged"},
		{"same release again", "080d8123", same, true, 0, "held"},
		{"older than installed: never a rollback", "080d8123", newer, true, 0, "held"},
		{"too new for this ring", "080d8123", older, true, 6 * time.Hour, "held"},
		{"old enough for this ring", "080d8123", older, true, time.Hour, ""},
	} {
		if got, _ := releaseDecision(m, c.running, c.st, c.have, c.minAge, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChannelReleasesSkipDraftsYanksAndPreviews(t *testing.T) {
	mk := func(tag string, draft, pre bool, at time.Time, manifest bool) ghRelease {
		r := ghRelease{Tag: tag, Draft: draft, Prerelease: pre, Created: at.Add(-time.Hour), Published: at}
		if manifest {
			r.Assets = append(r.Assets, struct {
				Name string `json:"name"`
			}{release.ManifestFile})
		}
		return r
	}
	now := time.Now()
	rs := []ghRelease{
		mk("zm-main-aaaaaaaaaaaa", false, false, now.Add(-2*time.Hour), true),
		mk("zm-main-bbbbbbbbbbbb", true, false, now, true),                       // draft
		mk("zm-main-eeeeeeeeeeee", false, true, now.Add(-10*time.Minute), true),  // yanked
		mk("app-preview-20261101", false, false, now.Add(time.Hour), true),       // another channel
		mk("zm-main-cccccccccccc", false, false, now.Add(-time.Hour), false),     // no manifest
		mk("zm-main-dddddddddddd", false, false, now.Add(-90*time.Minute), true), // newest eligible
	}
	b, _ := json.Marshal(rs)
	got, err := channelReleases(b, "x/y")
	if err != nil || len(got) != 2 || got[0].Tag != "zm-main-dddddddddddd" || got[0].Version != "main-dddddddddddd" || got[1].Tag != "zm-main-aaaaaaaaaaaa" {
		t.Fatalf("listed %+v: %v", got, err)
	}
	if _, err := channelReleases([]byte("[]"), "x/y"); err == nil {
		t.Fatal("listed from no releases")
	}
}

// channel is a release every step, newest first, the newest at now.
func channel(now time.Time, n int, step time.Duration) []listed {
	var rs []listed
	for i := 0; i < n; i++ {
		v := fmt.Sprintf("main-%012x", 0xa00000000000+n-i)
		rs = append(rs, listed{Tag: "zm-" + v, Version: v, Published: now.Add(-time.Duration(i) * step)})
	}
	return rs
}

func TestChooseReleaseNeverStarvesARing(t *testing.T) {
	now := time.Now()
	rs := channel(now, 16, 30*time.Minute) // a busy day: a release every 30 minutes for 8 hours
	running := commitOf(rs[15].Version)
	installed := release.State{Version: rs[15].Version, Created: rs[15].Published.Add(-time.Minute)}
	c := chooseRelease(rs, running, installed, true, 2*time.Hour, now, nil)
	if !c.install || c.pick.Version != rs[4].Version {
		t.Fatalf("2h ring took %+v, want the newest 2h old (%s)", c, rs[4].Version)
	}
	// The canary takes the newest; the running release is unchanged.
	if c := chooseRelease(rs, running, installed, true, 0, now, nil); !c.install || c.pick.Version != rs[0].Version {
		t.Fatalf("canary took %+v", c)
	}
	if c := chooseRelease(rs, commitOf(rs[0].Version), installed, true, 0, now, nil); c.install || c.result != "unchanged" {
		t.Fatalf("running the newest: %+v", c)
	}
	// Newer ones too young, running the newest old enough: held, saying why.
	if c := chooseRelease(rs, commitOf(rs[4].Version), installed, true, 2*time.Hour, now, nil); c.install || c.result != "held" || !strings.Contains(c.reason, rs[0].Version) {
		t.Fatalf("waiting: %+v", c)
	}
	// Never one older than what this home installed.
	if c := chooseRelease(rs, "080d8123", release.State{Version: rs[2].Version, Created: rs[2].Published}, true, 2*time.Hour, now, nil); c.install {
		t.Fatalf("took an older release: %+v", c)
	}
}

func TestChooseReleaseSkipsFailedAndUnvouched(t *testing.T) {
	now := time.Now()
	rs := channel(now, 6, time.Hour)
	st := release.State{Version: rs[5].Version, Created: rs[5].Published.Add(-time.Minute)}
	st.MarkFailed(release.Failure{Version: rs[1].Version, At: now, Result: "rolled_back"})
	running := commitOf(rs[5].Version)
	if c := chooseRelease(rs, running, st, true, time.Hour, now, nil); !c.install || c.pick.Version != rs[2].Version {
		t.Fatalf("after a failed release: %+v, want %s", c, rs[2].Version)
	}
	ok := map[string]bool{rs[3].Version: true}
	g := func(v string) string {
		if ok[v] {
			return ""
		}
		return "no vouch for " + v
	}
	if c := chooseRelease(rs, running, st, true, time.Hour, now, g); !c.install || c.pick.Version != rs[3].Version {
		t.Fatalf("gated: %+v, want the newest vouched %s", c, rs[3].Version)
	}
	none := func(v string) string { return "no vouch for " + v }
	if c := chooseRelease(rs, running, st, true, time.Hour, now, none); c.install || c.result != "held" || !strings.Contains(c.reason, rs[0].Version) {
		t.Fatalf("nothing vouched: %+v", c)
	}
}

func TestVouchNeedsAnotherMachinesStableRunAndNoFailure(t *testing.T) {
	v := "main-1eacf23e722f"
	row := func(name string, stable, failed []string) api.Machine {
		return api.Machine{Machine: name, Stats: api.MachineStats{Zm: &api.MachineZm{Version: v, Stable: stable, Failed: failed}}}
	}
	old := api.Machine{Machine: "dev-03"} // older than machine zm state
	if why := vouch([]api.Machine{old, row("dev-01", nil, nil), row("dev-04", []string{"main-x", v}, nil)}, "dev-01", v); why != "" {
		t.Fatalf("vouched by dev-04: %s", why)
	}
	if why := vouch([]api.Machine{row("dev-01", []string{v}, nil), old}, "dev-01", v); why == "" {
		t.Fatal("a machine vouched for itself")
	}
	if why := vouch([]api.Machine{row("dev-04", []string{v}, nil), row("dev-02", nil, []string{v})}, "dev-01", v); !strings.Contains(why, "failed on dev-02") {
		t.Fatalf("a failure did not veto: %q", why)
	}
	// Rename step 1: a machine that reports its build under the new
	// "potluck" field (step 2) vouches and vetoes the same.
	potluck := func(name string, stable, failed []string) api.Machine {
		return api.Machine{Machine: name, Stats: api.MachineStats{Potluck: &api.MachineZm{Version: v, Stable: stable, Failed: failed}}}
	}
	if why := vouch([]api.Machine{old, potluck("dev-04", []string{v}, nil)}, "dev-01", v); why != "" {
		t.Fatalf("a new-field machine did not vouch: %s", why)
	}
	if why := vouch([]api.Machine{row("dev-04", []string{v}, nil), potluck("dev-02", nil, []string{v})}, "dev-01", v); !strings.Contains(why, "failed on dev-02") {
		t.Fatalf("a new-field failure did not veto: %q", why)
	}
}

func TestNoteStableAfterSoakWithNoRestart(t *testing.T) {
	now := time.Now()
	inst := now.Add(-release.Soak - time.Minute)
	st := release.State{Version: "main-1eacf23e722f", Commit: relCommit, Created: inst.Add(-time.Hour), Installed: inst}
	h := api.Health{Version: st.Version, Commit: relCommit, Started: inst.Add(-time.Minute)}
	if src := (api.Health{Version: "zm-main-1eacf23-dirty", Commit: relCommit[:7], Started: h.Started}); noteStable(&release.State{Version: st.Version, Commit: relCommit, Created: st.Created, Installed: inst}, true, src, now) {
		t.Fatal("a source build at the release's commit counted as the release")
	}
	if !noteStable(&st, true, h, now) || len(st.Stable) != 1 {
		t.Fatalf("not stable after the soak: %+v", st)
	}
	restarted := release.State{Version: st.Version, Commit: relCommit, Created: st.Created, Installed: inst}
	h.Started = inst.Add(time.Minute) // a restart after the install
	if noteStable(&restarted, true, h, now) || len(restarted.Stable) != 0 {
		t.Fatalf("stable after a restart: %+v", restarted)
	}
	adopted := release.State{Version: st.Version, Commit: relCommit, Created: st.Created}
	if !noteStable(&adopted, true, h, now) || adopted.Installed.IsZero() || len(adopted.Stable) != 0 {
		t.Fatalf("an old state did not start counting: %+v", adopted)
	}
	other := release.State{Version: "main-old", Commit: "080d8123aaaa", Created: st.Created, Installed: inst}
	if noteStable(&other, true, api.Health{Version: st.Version, Commit: relCommit, Started: inst.Add(-time.Minute)}, now) {
		t.Fatal("stable for a release the daemon does not run")
	}
}

func TestReleaseNeverReplacesANewerRunningBuild(t *testing.T) {
	for status, want := range map[string]bool{"behind": false, "ahead": true, "identical": true, "diverged": true} {
		g := &githubSource{run: func(name string, args ...string) ([]byte, error) { return []byte(status + "\n"), nil }}
		if newer, _, err := g.ahead("6ace86b7", relCommit); err != nil || newer != want {
			t.Errorf("%s: newer=%v %v, want %v", status, newer, err, want)
		}
	}
	g := &githubSource{run: func(string, ...string) ([]byte, error) { t.Fatal("asked GitHub about a non-commit"); return nil, nil }}
	if newer, _, _ := g.ahead("app-preview-822", relCommit); !newer {
		t.Error("a build with no commit was replaced")
	}
}

// A failed upgrade with no swap record still counts as the release's
// failure when the daemon restarted or changed build (a swap whose record
// never reached the daemon); a refusal before anything moved does not.
func TestJudgeInstall(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	start := time.Now()
	before := api.Health{Commit: "aaaaaaaaaaaa", Started: t0}
	for _, tc := range []struct {
		name            string
		h               api.Health
		failed, restart bool
		result          string
	}{
		{"refused before anything moved", api.Health{Commit: before.Commit, Started: t0}, false, false, ""},
		{"rolled back, recorded", api.Health{Commit: before.Commit, Started: start.Add(time.Second),
			LastSwap: &api.SwapRecord{What: "upgrade", At: start.Add(time.Second), Result: "rolled_back"}}, true, true, "rolled_back"},
		{"rolled back, record lost", api.Health{Commit: before.Commit, Started: start.Add(time.Second)}, true, true, "failed"},
		{"rejected build still runs", api.Health{Commit: "bbbbbbbbbbbb", Started: start.Add(time.Second)}, true, true, "failed"},
		{"older record ignored", api.Health{Commit: before.Commit, Started: t0,
			LastSwap: &api.SwapRecord{What: "upgrade", At: t0, Result: "rolled_back"}}, false, false, ""},
	} {
		f, failed, restarted := judgeInstall(tc.h, start, "main-x", before)
		if failed != tc.failed || restarted != tc.restart || f.Result != tc.result {
			t.Errorf("%s: failed=%v restarted=%v result=%q", tc.name, failed, restarted, f.Result)
		}
	}
}

// twoRepos makes updateRepos what a private build's binary has, the public repo then the repo it was built from,
// for the test.
func twoRepos(t *testing.T) (pub, built string) {
	t.Helper()
	old := updateRepos
	updateRepos = sourceRepos(publicRepo, "example-org/zm-private")
	t.Cleanup(func() { updateRepos = old })
	return updateRepos[0], updateRepos[1]
}

// The channel reads updateRepos in order and takes the first repo that has
// releases: a repo not public yet (404) or empty passes to the next, one
// repo's listing is never mixed with another's, and open/file/ahead stay on
// the chosen repo.
func TestGithubSourceTakesFirstRepoWithReleases(t *testing.T) {
	rel := func(tag string) []byte {
		r := ghRelease{Tag: tag, Published: time.Now()}
		r.Assets = append(r.Assets, struct {
			Name string `json:"name"`
		}{release.ManifestFile})
		b, _ := json.Marshal([]ghRelease{r})
		return b
	}
	notThere := errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	first, second := twoRepos(t)
	for name, c := range map[string]struct {
		first, second           func() ([]byte, error)
		wantRepo, wantTag, fail string
	}{
		"new repo has releases": {func() ([]byte, error) { return rel("zm-main-aaaaaaaaaaaa"), nil }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, first, "zm-main-aaaaaaaaaaaa", ""},
		"new repo not public":   {func() ([]byte, error) { return nil, notThere }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, second, "zm-main-bbbbbbbbbbbb", ""},
		"new repo no releases":  {func() ([]byte, error) { return []byte("[]"), nil }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, second, "zm-main-bbbbbbbbbbbb", ""},
		"rate limit stops":      {func() ([]byte, error) { return nil, errors.New("gh: HTTP 403: API rate limit exceeded") }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, "", "", "rate limit"},
		"forbidden stops":       {func() ([]byte, error) { return nil, errors.New("gh: Resource not accessible (HTTP 403)") }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, "", "", "HTTP 403"},
		"network error stops":   {func() ([]byte, error) { return nil, errors.New("gh: dial tcp: no such host") }, func() ([]byte, error) { return rel("zm-main-bbbbbbbbbbbb"), nil }, "", "", "no such host"},
		"neither readable":      {func() ([]byte, error) { return nil, notThere }, func() ([]byte, error) { return []byte("[]"), nil }, "", "", "github.com/" + first + " is private"},
	} {
		g := &githubSource{run: func(_ string, args ...string) ([]byte, error) {
			if strings.Contains(args[1], first) {
				return c.first()
			}
			return c.second()
		}}
		rs, err := g.list()
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: err %v, want %q", name, err, c.fail)
			}
			if name == "neither readable" && (err == nil || !strings.Contains(err.Error(), "no signed release on "+second)) {
				t.Errorf("%s: err %v does not name %s's answer", name, err, second)
			}
			continue
		}
		if err != nil || len(rs) != 1 || rs[0].Tag != c.wantTag || g.repo != c.wantRepo || g.from() != c.wantRepo {
			t.Errorf("%s: %+v repo %q: %v", name, rs, g.repo, err)
		}
	}
	// The download and the history check stay on the repo list chose.
	var args []string
	g := &githubSource{repo: second, tmp: t.TempDir(), tag: "zm-main-bbbbbbbbbbbb", run: func(_ string, a ...string) ([]byte, error) {
		args = append(args, strings.Join(a, " "))
		return []byte("behind\n"), nil
	}}
	_, _, _ = g.ahead("6ace86b7", relCommit)
	_, _ = g.file(release.ManifestFile)
	for _, a := range args {
		if !strings.Contains(a, second) || strings.Contains(a, first) {
			t.Errorf("stayed on %s? %q", second, a)
		}
	}
	if len(args) != 2 {
		t.Errorf("calls %q", args)
	}
}

// ahead asks the release's repo first, then the others: a repo published from
// one commit holds none of the running build's history (GitHub answers 404).
// A compare no repo can place, with no source the release names, holds. A 403
// (an SSO, the rate limit) or the network is an error, retried next tick.
func TestGithubSourceAheadAcrossRepos(t *testing.T) {
	first, second := twoRepos(t)
	unknown := errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	type answer struct {
		out string
		err error
	}
	for name, c := range map[string]struct {
		first, second answer
		ahead         bool
		calls         int
		fail          string
	}{
		"release repo places it":      {answer{"behind", nil}, answer{"ahead", nil}, false, 1, ""},
		"release repo: running ahead": {answer{"ahead", nil}, answer{"behind", nil}, true, 1, ""},
		"other repo places it":        {answer{"", unknown}, answer{"ahead", nil}, true, 2, ""},
		"other repo: running behind":  {answer{"", unknown}, answer{"behind", nil}, false, 2, ""},
		"no repo places it: held":     {answer{"", unknown}, answer{"", unknown}, true, 3, ""},
		"forbidden (SSO) stops":       {answer{"", errors.New("gh: Forbidden (HTTP 403)")}, answer{"behind", nil}, false, 1, "HTTP 403"},
		"network error stops":         {answer{"", errors.New("gh: dial tcp: i/o timeout")}, answer{"behind", nil}, false, 1, "i/o timeout"},
		"rate limit stops":            {answer{"", errors.New("gh: HTTP 403: API rate limit exceeded")}, answer{"behind", nil}, false, 1, "rate limit"},
	} {
		var asked []string
		g := &githubSource{repo: first, run: func(_ string, a ...string) ([]byte, error) {
			asked = append(asked, a[1])
			r := c.second
			if strings.Contains(a[1], "repos/"+first+"/") {
				r = c.first
			}
			return []byte(r.out + "\n"), r.err
		}}
		ahead, why, err := g.ahead("6ace86b7", relCommit)
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: err %v, want %q", name, err, c.fail)
			}
		} else if err != nil || ahead != c.ahead || ahead != (why != "") {
			t.Errorf("%s: ahead %v %q: %v", name, ahead, why, err)
		}
		if len(asked) != c.calls || !strings.Contains(asked[0], first) {
			t.Errorf("%s: asked %q", name, asked)
		}
	}
	// The repo list chose is asked first even when it is not updateRepos[0].
	var asked []string
	g := &githubSource{repo: second, run: func(_ string, a ...string) ([]byte, error) {
		asked = append(asked, a[1])
		return nil, unknown
	}}
	if ahead, _, err := g.ahead("6ace86b7", relCommit); !ahead || err != nil || len(asked) != 3 || !strings.Contains(asked[0], second) || !strings.Contains(asked[1], first) || !strings.Contains(asked[2], "repos/"+second+"/commits/") {
		t.Errorf("chosen %s: ahead %v %v, asked %q", second, ahead, err, asked)
	}
}

// gh is a fake gh api: answers by path, a 404 for any other.
func gh(answers map[string]string, errs map[string]error) func(string, ...string) ([]byte, error) {
	return func(_ string, a ...string) ([]byte, error) {
		if err := errs[a[1]]; err != nil {
			return nil, err
		}
		if out, ok := answers[a[1]]; ok {
			return []byte(out + "\n"), nil
		}
		return nil, errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	}
}

// A public release commit is a copy of a private one (its Source: line): when
// no repo can compare the release's own commit with the running build, the
// copy's source is placed on the private repo instead.
func TestGithubSourcePlacesACopyByItsSource(t *testing.T) {
	pub, priv := twoRepos(t)
	const running = "6ace86b7c0ffee000000000000000000000000aa"
	const source = "0b5e1ec7000000000000000000000000000000bb"
	commit := "repos/" + pub + "/commits/" + relCommit
	msg := "Mezze v0.1.0\n\nSource: " + source
	onPriv := "repos/" + priv + "/compare/" + source + "..." + running
	for name, c := range map[string]struct {
		answers map[string]string
		errs    map[string]error
		held    bool
		why     string
		fail    string
	}{
		"source older than the running build": {map[string]string{commit: msg, onPriv: "ahead"}, nil, true, "copy of " + source[:12], ""},
		"source newer: take it":               {map[string]string{commit: msg, onPriv: "behind"}, nil, false, "", ""},
		"source diverged":                     {map[string]string{commit: msg, onPriv: "diverged"}, nil, true, "diverged", ""},
		"no source line: held":                {map[string]string{commit: "Mezze v0.1.0"}, nil, true, "no update repo can place", ""},
		"source no repo holds: held":          {map[string]string{commit: msg}, nil, true, "no update repo can place", ""},
		"source line not a full commit: held": {map[string]string{commit: "x\n\nSource: 0b5e1ec7"}, nil, true, "no update repo can place", ""},
		"commit unreadable: error":            {nil, map[string]error{commit: errors.New("gh: dial tcp: i/o timeout")}, false, "", "i/o timeout"},
		"source compare 403: error":           {map[string]string{commit: msg}, map[string]error{onPriv: errors.New("gh: Forbidden (HTTP 403)")}, false, "", "HTTP 403"},
	} {
		g := &githubSource{repo: pub, run: gh(c.answers, c.errs)}
		held, why, err := g.ahead(running, relCommit)
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: err %v, want %q", name, err, c.fail)
			}
			continue
		}
		if err != nil || held != c.held || !strings.Contains(why, c.why) {
			t.Errorf("%s: held %v %q: %v", name, held, why, err)
		}
	}
}

// githubDir is a release directory whose history GitHub answers (fake gh).
type githubDir struct {
	dirSource
	g *githubSource
}

func (d githubDir) ahead(running, commit string) (bool, string, error) {
	return d.g.ahead(running, commit)
}

// fetchRelease never installs a release it cannot place over the running
// build: not over a hand build (newer or not), not over the release this home
// installed from the channel, however much later the release was built. A
// public release copied from an older private snapshot than the channel's
// install (the cutover) holds; one copied from a newer snapshot installs.
func TestFetchReleaseHoldsWhatItCannotPlace(t *testing.T) {
	now := time.Now()
	dir, key, m := releaseDir(t, now.Add(-time.Hour), "public zm")
	pub, priv := twoRepos(t)
	const x = "0c0ffee0000000000000000000000000000000cc" // the private release the fleet runs
	const w = "0b5e1ec7000000000000000000000000000000bb" // the private snapshot the public release copies
	installedX := release.State{Version: "main-" + x[:12], Commit: x, Created: now.Add(-2 * time.Hour)}
	commit := "repos/" + pub + "/commits/" + m.Commit
	fetch := func(version, running string, st release.State, have bool, answers map[string]string) (bool, string, string, string) {
		t.Helper()
		stage := t.TempDir()
		src := githubDir{dirSource{dir}, &githubSource{repo: pub, run: gh(answers, nil)}}
		_, install, result, why, err := fetchRelease(src, []string{key}, version, running, &st, have, 0, now, nil, stage)
		if err != nil {
			t.Fatalf("fetch over %s: %v", running, err)
		}
		fs, _ := os.ReadDir(stage)
		if install != (len(fs) > 0) {
			t.Fatalf("install=%v staged %d files", install, len(fs))
		}
		return install, result, why, stage
	}
	for name, c := range map[string]struct {
		version, running string
		st               release.State
		have             bool
		answers          map[string]string
		install          bool
		why              string
	}{
		"hand build, nothing places it":          {"v0.0.0-20261113120000-6ace86b7c0ff", "6ace86b7c0ffee000000000000000000000000aa", installedX, true, nil, false, "no update repo can place"},
		"hand build, fresh home":                 {"v0.0.0-20261113120000-6ace86b7c0ff", "6ace86b7c0ffee000000000000000000000000aa", release.State{}, false, nil, false, "no update repo can place"},
		"channel install, no source named":       {installedX.Version, x, installedX, true, map[string]string{commit: "Mezze v0.1.0"}, false, "no update repo can place"},
		"cutover: copy of an older snapshot":     {installedX.Version, x, installedX, true, map[string]string{commit: "v\n\nSource: " + w, "repos/" + priv + "/compare/" + w + "..." + x: "ahead"}, false, "newer than the release"},
		"cutover: copy of a newer snapshot":      {installedX.Version, x, installedX, true, map[string]string{commit: "v\n\nSource: " + w, "repos/" + priv + "/compare/" + w + "..." + x: "behind"}, true, ""},
		"public build placed on the public repo": {installedX.Version, x, installedX, true, map[string]string{"repos/" + pub + "/compare/" + m.Commit + "..." + x: "behind"}, true, ""},
	} {
		install, result, why, _ := fetch(c.version, c.running, c.st, c.have, c.answers)
		if install != c.install || (!install && (result != "held" || !strings.Contains(why, c.why))) {
			t.Errorf("%s: install=%v %q %q", name, install, result, why)
		}
	}
}
