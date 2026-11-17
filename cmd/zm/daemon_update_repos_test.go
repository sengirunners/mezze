package main

// A machine's own order of release repositories (config update_repos, SPEC 2
// "Release channel"). The repositories here are made up.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
)

const (
	repoA = "example-org/zm-a"
	repoB = "example-org/zm-b"
	repoC = "example-org/zm-c"
	// runningFull is the running build's commit in these tests.
	runningFull = "6ace86b7c0ffee000000000000000000000000aa"
)

// orderHome is a temp home whose config.json is body ("" writes none).
func orderHome(t *testing.T, body string) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if body != "" {
		if err := os.WriteFile(filepath.Join(home, config.FileConfig), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// The setting is a strict list; anything else is an error naming the key,
// never the default order in disguise. No key is the built-in order.
func TestReleaseReposSetting(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"update_keys":["ed25519:x"],"machine":"m"}`} {
		repos, set, err := releaseRepos(orderHome(t, body))
		if err != nil || set || !slices.Equal(repos, updateRepos) {
			t.Errorf("%q: %q %v %v, want the built-in order", body, repos, set, err)
		}
	}
	repos, set, err := releaseRepos(orderHome(t, `{"update_repos":["`+repoB+`","`+repoA+`"]}`))
	if err != nil || !set || !slices.Equal(repos, []string{repoB, repoA}) {
		t.Fatalf("set: %q %v %v", repos, set, err)
	}
	nine := `["a/1","a/2","a/3","a/4","a/5","a/6","a/7","a/8","a/9"]`
	for value, want := range map[string]string{
		`null`:                   "must be a list",
		`"example-org/zm-a"`:     "must be a list",
		`[1]`:                    "must be a list",
		`{"a":"b"}`:              "must be a list",
		`[]`:                     "is empty",
		nine:                     "at most 8",
		`["zm-a"]`:               `"zm-a", not a GitHub repository`,
		`["a/b/c"]`:              "not a GitHub repository",
		`["-a/b"]`:               "not a GitHub repository",
		`["a_b/c"]`:              "not a GitHub repository",
		`["a/."]`:                "not a GitHub repository",
		`["a/.."]`:               "not a GitHub repository",
		`["../b"]`:               "not a GitHub repository",
		`["a/b.git"]`:            "not a GitHub repository",
		`["a/b?per_page=1"]`:     "not a GitHub repository",
		`["a/b c"]`:              "not a GitHub repository",
		`[""]`:                   "not a GitHub repository",
		`["a/b","A/B"]`:          `lists "A/B" twice`,
		`["example-org/zm-a",2]`: "must be a list",
	} {
		repos, set, err := releaseRepos(orderHome(t, `{"update_repos":`+value+`}`))
		if err == nil || repos != nil || !set || !strings.HasPrefix(err.Error(), "config update_repos ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %q %v %v, want an error with %q", value, repos, set, err, want)
		}
	}
	// A near miss of the key is not silently the default.
	if _, _, err := releaseRepos(orderHome(t, `{"Update_Repos":["a/b"]}`)); err == nil || !strings.Contains(err.Error(), "lower case") {
		t.Errorf("near miss: %v", err)
	}
	if _, _, err := releaseRepos(orderHome(t, `{`)); err == nil {
		t.Error("an unreadable config.json")
	}
}

// Rule 5: a machine on its own order says so in each check's reason and in
// status; the default order adds nothing to a check's reason.
func TestReleaseSourcesReport(t *testing.T) {
	if n := releaseSourcesNote(updateRepos, false); n != "" {
		t.Errorf("default note %q", n)
	}
	note := releaseSourcesNote([]string{repoB, repoA}, true)
	if note != "release sources: "+repoB+", "+repoA+" (config update_repos)" {
		t.Errorf("note %q", note)
	}
	for reason, want := range map[string]string{"": note, "held: x": "held: x; " + note} {
		if got := withSources(reason, note); got != want {
			t.Errorf("withSources(%q) = %q", reason, got)
		}
		if got := withSources(reason, ""); got != reason {
			t.Errorf("default changed %q to %q", reason, got)
		}
	}
	for body, want := range map[string]string{
		"":                                   strings.Join(updateRepos, ", ") + " (default)",
		`{"update_repos":["` + repoA + `"]}`: repoA + " (config update_repos)",
		`{"update_repos":[]}`:                "config update_repos is empty",
	} {
		if got := releaseSourcesLine(orderHome(t, body), "m1"); !strings.HasPrefix(got, "release sources on m1: ") || !strings.Contains(got, want) {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}

// Rule 6: the setting is read in one place (the release source names it only
// in a message) and written by nothing in this module: no route, flag or
// command handles the key.
func TestUpdateReposHasOneReader(t *testing.T) {
	var where []string
	_ = filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != "../.." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), `"update_repos"`) || strings.Contains(string(b), "updateReposKey") {
			where = append(where, filepath.ToSlash(p))
		}
		return nil
	})
	if !slices.Equal(where, []string{"../../cmd/zm/daemon_release.go", "../../cmd/zm/daemon_update.go"}) {
		t.Errorf("update_repos appears in %q, want only the resolver's file", where)
	}
}

// ghCalls is a fake gh api answering by path (gh) that records each call.
func ghCalls(calls *[]string, answers map[string]string, errs map[string]error) func(string, ...string) ([]byte, error) {
	f := gh(answers, errs)
	return func(name string, a ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(a, " "))
		return f(name, a...)
	}
}

// ghFiles lists and places releases through a githubSource and reads the
// release's files from a directory.
type ghFiles struct {
	dirSource
	g *githubSource
}

func (s ghFiles) list() ([]listed, error) { return s.g.list() }
func (s ghFiles) ahead(running, commit string) (bool, string, error) {
	return s.g.ahead(running, commit)
}

// listing is GitHub's release listing holding the test release's tag.
func listing(published time.Time) string {
	r := ghRelease{Tag: releaseTagPrefix + relCommit[:12], Published: published}
	r.Assets = append(r.Assets, struct {
		Name string `json:"name"`
	}{release.ManifestFile})
	b, _ := json.Marshal([]ghRelease{r})
	return string(b)
}

func releasesPath(repo string) string { return "repos/" + repo + "/releases?per_page=100" }
func comparePath(repo, base string) string {
	return "repos/" + repo + "/compare/" + base + "..." + runningFull
}

type fetchOut struct {
	install        bool
	result, reason string
	err            string
	calls          []string
}

// fetchVia runs one release check through g (its run replaced by a recording
// fake) over the signed release in dir.
func fetchVia(t *testing.T, g *githubSource, dir string, keys []string, answers map[string]string, errs map[string]error) fetchOut {
	t.Helper()
	var o fetchOut
	g.run = ghCalls(&o.calls, answers, errs)
	var st release.State
	_, install, result, reason, err := fetchRelease(ghFiles{dirSource{dir}, g}, keys, "main-"+runningFull[:12], runningFull, &st, false, 0, time.Now(), nil, t.TempDir())
	o.install, o.result, o.reason = install, result, reason
	if err != nil {
		o.err = err.Error()
	}
	return o
}

// Rule 1: with no setting, a check makes the same gh calls and ends with the
// same result, reason or error as the built-in source, in every branch.
func TestNoSettingIsToday(t *testing.T) {
	pub, priv := twoRepos(t)
	now := time.Now()
	dir, key, m := releaseDir(t, now.Add(-time.Hour), "zm")
	repos, set, err := releaseRepos(orderHome(t, `{"machine":"m"}`))
	if err != nil || set {
		t.Fatal(repos, set, err)
	}
	notThere := errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	commit := "repos/" + priv + "/commits/" + m.Commit
	for name, c := range map[string]struct {
		answers map[string]string
		errs    map[string]error
	}{
		"falls to the build repo, running newer": {map[string]string{releasesPath(priv): listing(now), comparePath(priv, m.Commit): "ahead"}, nil},
		"takes it":                               {map[string]string{releasesPath(pub): listing(now), comparePath(pub, m.Commit): "behind"}, nil},
		"nothing places it":                      {map[string]string{releasesPath(priv): listing(now), commit: "x"}, nil},
		"rate limit stops":                       {nil, map[string]error{releasesPath(pub): errors.New("gh: HTTP 403: API rate limit exceeded")}},
		"no repo has one":                        {map[string]string{releasesPath(priv): "[]"}, map[string]error{releasesPath(pub): notThere}},
	} {
		today := fetchVia(t, &githubSource{}, dir, []string{key}, c.answers, c.errs)
		got := fetchVia(t, &githubSource{repos: repos, set: set}, dir, []string{key}, c.answers, c.errs)
		if !slices.Equal(got.calls, today.calls) || got.install != today.install || got.result != today.result || got.reason != today.reason || got.err != today.err {
			t.Errorf("%s:\n got %+v\ntoday %+v", name, got, today)
		}
		if len(today.calls) == 0 {
			t.Errorf("%s: no gh call", name)
		}
		if name == "takes it" {
			want := []string{"api " + releasesPath(pub), "api " + comparePath(pub, m.Commit) + " --jq .status"}
			if !slices.Equal(got.calls, want) || !got.install {
				t.Errorf("default calls %q, want %q", got.calls, want)
			}
		}
	}
}

// Rule 2: the order never changes which keys a release must verify with: a
// release on a listed repo signed by a key this machine does not trust is
// refused with today's error, in either order.
func TestOrderNeverWidensTrust(t *testing.T) {
	now := time.Now()
	dir, key, _ := releaseDir(t, now.Add(-time.Hour), "zm")
	other, _, _ := ed25519.GenerateKey(nil)
	untrusted := []string{release.FormatKey(other)}
	var st release.State
	_, _, _, _, want := fetchRelease(dirSource{dir}, untrusted, "main-"+runningFull[:12], runningFull, &st, false, 0, now, nil, t.TempDir())
	if want == nil {
		t.Fatal("an untrusted key verified")
	}
	for _, order := range [][]string{{repoA, repoB}, {repoB, repoA}} {
		answers := map[string]string{releasesPath(repoB): listing(now), comparePath(repoB, relCommit): "behind " + relCommit + " " + runningFull}
		o := fetchVia(t, &githubSource{repos: order, set: true}, dir, untrusted, answers, nil)
		if o.install || o.err != want.Error() {
			t.Errorf("%q: %+v, want %v", order, o, want)
		}
		if o := fetchVia(t, &githubSource{repos: order, set: true}, dir, []string{key}, answers, nil); !o.install {
			t.Errorf("%q with the trusted key: %+v", order, o)
		}
	}
}

// Rule 3: on its own order a machine still never goes backwards and never
// takes a release it cannot place; an answer about other commits than asked
// (a branch or tag named like a commit) holds.
func TestOrderNeverTakesWhatItCannotPlace(t *testing.T) {
	now := time.Now()
	dir, key, m := releaseDir(t, now.Add(-time.Hour), "zm")
	const elsewhere = "0dd0000000000000000000000000000000000ddd"
	const src = "0b5e1ec7000000000000000000000000000000bb"
	order := []string{repoB, repoA}
	list := releasesPath(repoB)
	for name, c := range map[string]struct {
		answers map[string]string
		install bool
		why     string
	}{
		"running newer":              {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "ahead " + m.Commit + " " + m.Commit}, false, "running build is newer"},
		"running newer on the other": {map[string]string{list: listing(now), comparePath(repoA, m.Commit): "ahead " + m.Commit + " " + m.Commit}, false, "running build is newer"},
		"diverged":                   {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "diverged " + m.Commit + " " + elsewhere}, false, "diverged"},
		"no listed repo places it":   {map[string]string{list: listing(now)}, false, "no update repo can place"},
		"behind: takes it":           {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind " + m.Commit + " " + runningFull}, true, ""},
		"a short merge base":         {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind " + m.Commit + " " + runningFull[:7]}, false, "other commits than asked"},
		"base is another commit":     {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind " + elsewhere + " " + runningFull}, false, "other commits than asked"},
		"merge base is another":      {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind " + m.Commit + " " + elsewhere}, false, "other commits than asked"},
		"bare status":                {map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind"}, false, "other commits than asked"},
		"source of another commit": {map[string]string{list: listing(now),
			"repos/" + repoB + "/commits/" + m.Commit: elsewhere + " v\n\nSource: " + src, comparePath(repoA, src): "behind " + src + " " + runningFull}, false, "no update repo can place"},
		"source of this commit": {map[string]string{list: listing(now),
			"repos/" + repoB + "/commits/" + m.Commit: m.Commit + " v\n\nSource: " + src, comparePath(repoA, src): "behind " + src + " " + runningFull}, true, ""},
	} {
		o := fetchVia(t, &githubSource{repos: order, set: true}, dir, []string{key}, c.answers, nil)
		if o.err != "" || o.install != c.install || (!o.install && (o.result != "held" || !strings.Contains(o.reason, c.why))) {
			t.Errorf("%s: %+v", name, o)
		}
	}
	// What this home installed already is never replaced by an older release.
	var o fetchOut
	g := &githubSource{repos: order, set: true, run: ghCalls(&o.calls, map[string]string{list: listing(now), comparePath(repoB, m.Commit): "behind " + m.Commit + " " + runningFull}, nil)}
	st := release.State{Version: "main-" + runningFull[:12], Commit: runningFull, Created: now}
	if _, install, result, _, err := fetchRelease(ghFiles{dirSource{dir}, g}, []string{key}, "x", "0f0f0f0f0f0f", &st, true, 0, now, nil, t.TempDir()); install || result != "held" || err != nil {
		t.Errorf("older than installed: %v %q %v", install, result, err)
	}
}

// A running build named by a short commit is placed by GitHub's full one.
func TestComparedAskedShortRunning(t *testing.T) {
	if !comparedAsked("behind "+relCommit+" "+runningFull, relCommit, runningFull[:7]) || comparedAsked("behind "+relCommit+" "+runningFull, relCommit, "0dd0000") {
		t.Fatal("comparedAsked")
	}
}

// A yank is made on one repo: a machine's own order never takes a release
// another listed repo it can read yanked. A repo it cannot read says nothing;
// any other failure stops the check.
func TestOrderKeepsYanks(t *testing.T) {
	now := time.Now()
	dir, key, m := releaseDir(t, now.Add(-time.Hour), "zm")
	yank := func() string {
		r := ghRelease{Tag: releaseTagPrefix + relCommit[:12], Published: now, Prerelease: true}
		b, _ := json.Marshal([]ghRelease{r})
		return string(b)
	}()
	notThere := errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	order := []string{repoB, repoA}
	behind := map[string]string{releasesPath(repoB): listing(now), comparePath(repoB, m.Commit): "behind " + m.Commit + " " + runningFull}
	with := func(k, v string) map[string]string {
		a := map[string]string{k: v}
		for x, y := range behind {
			a[x] = y
		}
		return a
	}
	for name, c := range map[string]struct {
		answers map[string]string
		errs    map[string]error
		install bool
		fail    string
	}{
		"yanked on the other repo": {with(releasesPath(repoA), yank), nil, false, ""},
		"yanked on a built-in one": {with(releasesPath(publicRepo), yank), nil, false, ""},
		"other repo has it":        {with(releasesPath(repoA), listing(now)), nil, true, ""},
		"other repo unreadable":    {behind, map[string]error{releasesPath(repoA): notThere}, true, ""},
		"other repo refuses":       {behind, map[string]error{releasesPath(repoA): errors.New("gh: Forbidden (HTTP 403)")}, false, "HTTP 403"},
	} {
		o := fetchVia(t, &githubSource{repos: order, set: true}, dir, []string{key}, c.answers, c.errs)
		if o.install != c.install || (c.fail == "") != (o.err == "") || !strings.Contains(o.err, c.fail) || (!o.install && c.fail == "" && o.result != "held") {
			t.Errorf("%s: %+v", name, o)
		}
	}
	// A machine's own order with no repos never reads the built-in ones.
	var calls []string
	g := &githubSource{set: true, run: ghCalls(&calls, nil, nil)}
	if _, err := g.list(); err == nil || len(calls) != 0 || !strings.Contains(err.Error(), "lists no repository") {
		t.Errorf("empty own order: %v %q", err, calls)
	}
}

// Rule 4: fall-through on a machine's order is as on the built-in one: a 404
// or no release passes to the next listed repo, anything else stops there.
func TestOrderFallsThroughAsToday(t *testing.T) {
	now := time.Now()
	notThere := errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	order := []string{repoA, repoB, repoC}
	for name, c := range map[string]struct {
		answers map[string]string
		errs    map[string]error
		repo    string
		asked   int
		fail    string
	}{
		"404, empty, then releases": {map[string]string{releasesPath(repoB): "[]", releasesPath(repoC): listing(now)}, map[string]error{releasesPath(repoA): notThere}, repoC, 3 + 2 + len(updateRepos), ""}, // + the yank check of the others and the built-in ones
		"first has releases":        {map[string]string{releasesPath(repoA): listing(now), releasesPath(repoC): listing(now)}, nil, repoA, 1 + 2 + len(updateRepos), ""},
		"forbidden stops":           {map[string]string{releasesPath(repoC): listing(now)}, map[string]error{releasesPath(repoA): errors.New("gh: Forbidden (HTTP 403)")}, "", 1, "HTTP 403"},
		"network stops at the 2nd":  {map[string]string{releasesPath(repoC): listing(now)}, map[string]error{releasesPath(repoA): notThere, releasesPath(repoB): errors.New("gh: dial tcp: i/o timeout")}, "", 2, "i/o timeout"},
		"none has one":              {map[string]string{releasesPath(repoB): "[]"}, map[string]error{releasesPath(repoA): notThere}, "", 3, "no update repo has zm's releases"},
	} {
		var calls []string
		g := &githubSource{repos: order, set: true, run: ghCalls(&calls, c.answers, c.errs)}
		_, err := g.list()
		if len(calls) != c.asked {
			t.Errorf("%s: asked %q", name, calls)
		}
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: %v, want %q", name, err, c.fail)
			}
			continue
		}
		if err != nil || g.from() != c.repo {
			t.Errorf("%s: from %s: %v", name, g.from(), err)
		}
	}
}
