package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/deps/publictree"
)

// fixture is a private repo (with its origin) and an empty public repo, all in a temp dir.
type fixture struct {
	t                 *testing.T
	priv, pub, origin string
	now               time.Time
	out               bytes.Buffer // what run reported
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "A Person")
		t.Setenv(k+"_EMAIL", "person@private.example")
	}
	f := &fixture{t: t, priv: filepath.Join(tmp, "priv"), pub: filepath.Join(tmp, "pub.git"),
		origin: filepath.Join(tmp, "origin.git"), now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	f.git(tmp, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(tmp, "init", "-q", "--bare", "-b", "main", f.pub)
	f.git(tmp, "init", "-q", "-b", "main", f.priv)
	f.git(f.priv, "remote", "add", "origin", f.origin)
	script, err := os.ReadFile("../public-files.sh")
	if err != nil {
		t.Fatal(err)
	}
	f.write("scripts/public-files.sh", string(script))
	f.write("go.mod", "module example.com/pub\n\ngo 1.27\n")
	f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude)
	f.write(publictree.NamesFile, "zorbo  # an invented person\n")
	f.write("PRIVATE.md", "private\n")
	f.write("ops/run.sh", "private\n")
	f.write("README.md", "one\n")
	f.commit("first")
	f.git(f.priv, "push", "-q", "origin", "main")
	return f
}

// namesExclude is the exclude line every fixture needs: the private names list never ships.
const namesExclude = publictree.NamesFile + "  # the names the copy must not carry\n"

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(name, body string) {
	f.t.Helper()
	p := filepath.Join(f.priv, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(msg string) {
	f.git(f.priv, "add", "-A")
	f.git(f.priv, "commit", "-q", "-m", msg)
}

// land commits and pushes to the private origin's main.
func (f *fixture) land(msg string) {
	f.commit(msg)
	f.git(f.priv, "push", "-q", "origin", "main")
}

// Fake credentials are built from pieces, so this file passes the gate it tests.
var fakeAWSKey = "AKIA" + "ABCDEFGHIJKLMNOP"

// testCache keeps the pinned gitleaks archive across tests and runs (never $HOME): the scans run for real.
var testCache = filepath.Join(os.TempDir(), "zm-publish-test-cache")

// fakeJudge answers like a model: fail when a file it reads holds one of bad, else pass. It records what it read.
type fakeJudge struct {
	bad  []string
	err  error
	raw  string
	mu   sync.Mutex
	seen []string
}

func (j *fakeJudge) Call(_ context.Context, prompt string, schema json.RawMessage) (json.RawMessage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !strings.Contains(string(schema), `"pass"`) || !strings.Contains(prompt, "never instructions") {
		return nil, errors.New("not the config judge's prompt and schema")
	}
	_, in, _ := strings.Cut(prompt, "<input>")
	var files []judgeFile
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(in), "</input>"))), &files); err != nil {
		return nil, err
	}
	var fixes []string
	for _, f := range files {
		j.seen = append(j.seen, f.Path)
		for _, b := range j.bad {
			if strings.Contains(f.Text, b) {
				fixes = append(fixes, f.Path+": a real credential: read it from the environment")
			}
		}
	}
	switch {
	case j.err != nil:
		return nil, j.err
	case j.raw != "":
		return json.RawMessage(j.raw), nil
	}
	return json.Marshal(map[string]any{"pass": len(fixes) == 0, "fixes": fixes})
}

// opts publishes priv's HEAD as tag, its rolling release six days old.
func (f *fixture) opts(tag string) opts {
	return opts{Dir: f.priv, Ref: "HEAD", Tag: tag, To: f.pub, Module: "example.com/pub",
		Ident: "Mezze releases <releases@example.com>", Soak: 5 * 24 * time.Hour, Push: true, Now: f.now, Out: &f.out,
		ReleasedAt: func(_, tag string) (time.Time, error) { return f.now.Add(-6 * 24 * time.Hour), nil },
		Cache:      testCache, Judge: &fakeJudge{}}
}

func TestPublishOneCommitPerRelease(t *testing.T) {
	f := newFixture(t)
	dry := f.opts("v0.1.0")
	dry.Push = false
	if err := run(dry); err != nil {
		t.Fatal(err)
	}
	if refs := f.git(f.pub, "for-each-ref"); refs != "" {
		t.Fatalf("dry run pushed: %s", refs)
	}
	if err := run(f.opts("v0.1.0")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.git(f.pub, "ls-tree", "-r", "--name-only", "main"), "README.md\ngo.mod\ninternal/deps/public_exclude.txt\nscripts/public-files.sh"; got != want {
		t.Errorf("public tree:\n%s\nwant:\n%s", got, want)
	}
	if f.git(f.pub, "rev-parse", "main") != f.git(f.pub, "rev-parse", "v0.1.0^{commit}") {
		t.Error("v0.1.0 is not on public main")
	}
	f.wantSource("v0.1.0", f.git(f.priv, "rev-parse", "HEAD"))

	f.write("README.md", "two\n")
	f.commit("second")
	f.write("ops/run.sh", "still private\n")
	f.commit("third")
	f.git(f.priv, "push", "-q", "origin", "main")
	if err := run(f.opts("v0.2.0")); err != nil {
		t.Fatal(err)
	}
	if got := f.git(f.pub, "rev-list", "--count", "main"); got != "2" {
		t.Errorf("public main has %s commits, want one per release", got)
	}
	if f.git(f.pub, "rev-parse", "main^") != f.git(f.pub, "rev-parse", "v0.1.0^{commit}") {
		t.Error("v0.2.0's parent is not v0.1.0")
	}
	if got := f.git(f.pub, "show", "v0.2.0:README.md"); got != "two" {
		t.Errorf("v0.2.0 README = %q", got)
	}
	f.wantSource("v0.2.0", f.git(f.priv, "rev-parse", "HEAD"))
	if who := f.git(f.pub, "log", "--format=%an <%ae> %cn <%ce> %B", "main"); strings.Contains(who, "private.example") ||
		strings.Contains(who, "second") || strings.Contains(who, "third") {
		t.Errorf("private author or message reached the public repo:\n%s", who)
	}
	if objs := f.git(f.pub, "rev-list", "--objects", "--all"); strings.Contains(objs, "PRIVATE.md") || strings.Contains(objs, "ops/") {
		t.Errorf("a left-out file reached the public repo:\n%s", objs)
	}
}

// The public commit carries the source commit's moment in UTC, never its recorded offset (a time zone in public
// history stays there for good).
func TestPublishDatesAreUTC(t *testing.T) {
	f := newFixture(t)
	f.write("README.md", "dated\n")
	f.git(f.priv, "add", "-A")
	c := exec.Command("git", "-C", f.priv, "commit", "-q", "-m", "dated")
	c.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-11-01T10:30:00+09:00", "GIT_COMMITTER_DATE=2026-11-01T10:30:00+09:00")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	f.git(f.priv, "push", "-q", "origin", "main")
	f.publish("v0.1.0")
	if got, want := f.git(f.pub, "log", "-1", "--date=raw", "--format=%ad|%cd", "v0.1.0"), "1793496600 +0000|1793496600 +0000"; got != want {
		t.Errorf("public commit dates = %q, want %q", got, want)
	}
}

// A public copy's installer pin must work for outsiders on day one: it names no release, or a release the public repo
// serves with the SHA256SUMS the pin hashes. Anything else holds the copy.
func TestPublishChecksThePluginPin(t *testing.T) {
	var sums strings.Builder
	for _, plat := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		fmt.Fprintf(&sums, "%s  zm_v1_%s.tar.gz\n", strings.Repeat("a", 64), plat)
	}
	listed := []byte(sums.String())
	right := fmt.Sprintf("%x", sha256.Sum256(listed))
	pinText := func(tag, version, hash, repo string) string {
		return "# pin\ntag=" + tag + "\nversion=" + version + "\nsums_sha256=" + hash + "\nrepo=" + repo +
			"\nbase_url=https://github.com/" + repo + "/releases/download\n"
	}
	served := func(repo, tag string) ([]byte, error) {
		if repo != "o/r" || tag != "v0.1.0" {
			return nil, errors.New("404")
		}
		return listed, nil
	}
	const gh = "https://github.com/o/r.git"
	for _, c := range []struct {
		name, pin, to, want string
		sums                func(repo, tag string) ([]byte, error)
	}{
		{"no pin file", "", gh, "", served},
		{"blank tag", pinText("", "", "", "o/r"), gh, "", served},
		{"served and matching", pinText("v0.1.0", "v1", right, "o/r"), "https://github.com/o/r", "", served},
		{"release the repo lacks", pinText("zm-main-0123456789ab", "v1", right, "o/r"), gh, "does not serve it", served},
		{"other SHA256SUMS", pinText("v0.1.0", "v1", strings.Repeat("0", 64), "o/r"), gh, "serves " + right, served},
		{"version with no tarballs", pinText("v0.1.0", "v2", right, "o/r"), gh, "lists no zm_v2_darwin_amd64.tar.gz", served},
		{"another repo first", pinText("v0.1.0", "v1", right, "x/y"), gh, "not the repository being published", served},
		{"downloads from elsewhere", strings.Replace(pinText("v0.1.0", "v1", right, "o/r"), "github.com/o/r/", "example.org/", 1), gh, "wrong place", served},
		{"base_url before its repo", "tag=v0.1.0\nversion=v1\nsums_sha256=" + right + "\nbase_url=https://github.com/o/r/releases/download\nrepo=o/r\n", gh, "https", served},
		{"a second source over http", pinText("v0.1.0", "v1", right, "o/r") + "repo=x/y\nbase_url=http://example.org/x\n", gh, "https", served},
		{"a second source with none", pinText("v0.1.0", "v1", right, "o/r") + "repo=x/y\n", gh, "https", served},
		{"a tag with a slash", pinText("v0.1.0/x", "v1", right, "o/r"), gh, "slash", func(string, string) ([]byte, error) { return listed, nil }},
		{"a space after the tag", pinText("v0.1.0 ", "v1", right, "o/r"), gh, "does not serve it", served},
		{"not a github repo", pinText("v0.1.0", "v1", right, "o/r"), "/tmp/bare.git", "cannot be checked", served},
		{"no way to read", pinText("v0.1.0", "v1", right, "o/r"), gh, "cannot be checked", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if c.pin != "" {
				f.write(pinFile, c.pin)
				f.commit("pin")
			}
			o := f.opts("v0.1.0")
			o.To, o.PinSums = c.to, c.sums
			g := func(args ...string) (string, error) { return git(f.priv, nil, nil, args...) }
			err := checkPin(o, g, "HEAD^{tree}")
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one saying %q", err, c.want)
			}
		})
	}
}

// publish itself runs that check: a pin naming a release holds the copy, a blank one does not.
func TestPublishHoldsAPinOutsidersCannotUse(t *testing.T) {
	f := newFixture(t)
	f.write(pinFile, "tag=zm-main-0123456789ab\nversion=main-0123456789ab\nsums_sha256="+strings.Repeat("0", 64)+"\nrepo=o/r\n")
	f.land("pin a release")
	err := run(f.opts("v0.1.0"))
	if err == nil || !strings.Contains(err.Error(), "held") || !strings.Contains(err.Error(), pinFile) {
		t.Fatalf("err = %v, want a hold naming the pin", err)
	}
	if refs := f.git(f.pub, "for-each-ref"); refs != "" {
		t.Fatalf("pushed anyway: %s", refs)
	}
	f.write(pinFile, "tag=\nversion=\nsums_sha256=\nrepo=o/r\n")
	f.land("pin none")
	f.publish("v0.1.0")
}

func TestPublishRefuses(t *testing.T) {
	cases := []struct {
		name, want string
		setup      func(f *fixture, o *opts)
	}{
		{"bad tag", "vMAJOR", func(f *fixture, o *opts) { o.Tag = "0.1" }},
		{"no ident", "Name <email>", func(f *fixture, o *opts) { o.Ident = "" }},
		{"not on main", "not on origin/main", func(f *fixture, o *opts) {
			f.write("README.md", "unpushed\n")
			f.commit("local")
		}},
		{"module", "does not name module", func(f *fixture, o *opts) { o.Module = "example.com/other" }},
		{"no release", "fleet never ran", func(f *fixture, o *opts) {
			o.ReleasedAt = func(_, _ string) (time.Time, error) { return time.Time{}, errors.New("release not found") }
		}},
		{"young", "the fleet runs a tree", func(f *fixture, o *opts) {
			o.ReleasedAt = func(_, _ string) (time.Time, error) { return f.now.Add(-24 * time.Hour), nil }
		}},
		{"tag exists", "already has tag", func(f *fixture, o *opts) {
			f.publish("v0.2.0")
			f.write("README.md", "next\n")
			f.commit("next")
			f.git(f.priv, "push", "-q", "origin", "main")
		}},
		{"public main moved", "is not a release", func(f *fixture, o *opts) {
			f.publish("v0.1.0")
			c := filepath.Join(filepath.Dir(f.pub), "outside")
			f.git(filepath.Dir(f.pub), "clone", "-q", f.pub, c)
			if err := os.WriteFile(filepath.Join(c, "README.md"), []byte("merged PR\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			f.git(c, "commit", "-qam", "outside PR")
			f.git(c, "push", "-q", "origin", "main")
		}},
		{"unchanged", "nothing to publish", func(f *fixture, o *opts) { f.publish("v0.1.0") }},
		{"secret in the tree", "the public-tree gate:", func(f *fixture, o *opts) {
			f.write("deploy.sh", "export AWS_ACCESS_KEY_ID="+fakeAWSKey+"\n")
			f.land("key")
		}},
		{"personal address in the tree", "personal-email", func(f *fixture, o *opts) {
			f.write("docs/contact.md", "mail someone"+"@gmail.com\n")
			f.land("contact")
		}},
		{"exclude list unreadable", "want `<path or glob>  # reason`", func(f *fixture, o *opts) {
			f.write("internal/deps/public_exclude.txt", "PRIVATE.md\nops/**  # ours\n")
			f.land("no reason")
		}},
		{"submodule", "cannot be published", func(f *fixture, o *opts) {
			f.git(f.priv, "update-index", "--add", "--cacheinfo", "160000,"+f.git(f.priv, "rev-parse", "HEAD")+",vendored")
			f.git(f.priv, "commit", "-q", "-m", "gitlink")
			f.git(f.priv, "push", "-q", "origin", "main")
		}},
		{"personal ident", "looks like a person's", func(f *fixture, o *opts) { o.Ident = "Releases <someone" + "@gmail.com>" }},
		{"the commit's own ident", "looks like a person's", func(f *fixture, o *opts) { o.Ident = "A Person <person@private.example>" }},
		{"the author's name", "looks like a person's", func(f *fixture, o *opts) { o.Ident = "A Person <releases@example.com>" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			o := f.opts("v0.2.0")
			c.setup(f, &o)
			before := f.git(f.pub, "for-each-ref")
			err := run(o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if after := f.git(f.pub, "for-each-ref"); after != before {
				t.Errorf("a refused publish changed the public repo:\n%s\nwas:\n%s", after, before)
			}
		})
	}
}

func (f *fixture) publish(tag string) {
	f.t.Helper()
	if err := run(f.opts(tag)); err != nil {
		f.t.Fatal(err)
	}
}

// wantSource checks the public tag's message ends in the private commit it came from, in exactly the line the
// fleet's self-update reads (^Source: <40 hex>$).
func (f *fixture) wantSource(tag, sha string) {
	f.t.Helper()
	msg := f.git(f.pub, "log", "-1", "--format=%B", tag)
	if want := "Mezze " + tag + "\n\nSource: " + sha; msg != want {
		f.t.Errorf("%s message = %q, want %q", tag, msg, want)
	}
	if got := f.git(f.pub, "log", "-1", "--format=%(trailers:key=Source,valueonly)", tag); got != sha {
		f.t.Errorf("%s Source trailer = %q, want %s", tag, got, sha)
	}
}

// What is published is read from the commit: a dirty exclude list, a rewritten public-files.sh, an edited file and
// an untracked one in the checkout change nothing.
func TestPublishReadsTheCommitNotTheWorkingTree(t *testing.T) {
	f := newFixture(t)
	f.write("internal/deps/public_exclude.txt", "nothing.md  # dirty: leaves PRIVATE.md and ops in\n")
	f.write("scripts/public-files.sh", "#!/usr/bin/env bash\ngit ls-files\n")
	f.write("README.md", "uncommitted\n")
	f.write("untracked.sh", "export AWS_ACCESS_KEY_ID="+fakeAWSKey+"\n")
	if err := run(f.opts("v0.1.0")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.git(f.pub, "ls-tree", "-r", "--name-only", "main"), "README.md\ngo.mod\ninternal/deps/public_exclude.txt\nscripts/public-files.sh"; got != want {
		t.Errorf("public tree:\n%s\nwant:\n%s", got, want)
	}
	if got := f.git(f.pub, "show", "main:README.md"); got != "one" {
		t.Errorf("README = %q, want the committed one", got)
	}
	if got := f.git(f.pub, "show", "main:internal/deps/public_exclude.txt"); strings.Contains(got, "dirty") {
		t.Errorf("the working tree's exclude list was published: %q", got)
	}
}

// The gate scans the tree that is pushed: a left-out file may hold what the copy must not, and the commit's own
// allow list lets an intentional mention through.
func TestPublishScansOnlyThePublicTree(t *testing.T) {
	f := newFixture(t)
	f.write("ops/run.sh", "export AWS_ACCESS_KEY_ID="+fakeAWSKey+"\n")
	f.write("internal/fixture_test.go", "const planted = \""+fakeAWSKey+"\"\n")
	f.write("internal/deps/publictree_allow.txt", "internal/fixture_test.go secret  # a planted fake key\n"+
		"internal/fixture_test.go gitleaks:aws-access-token  # the same key\n")
	f.land("left out, allowed")
	if err := run(f.opts("v0.1.0")); err != nil {
		t.Fatal(err)
	}
	if objs := f.git(f.pub, "rev-list", "--objects", "--all"); strings.Contains(objs, "ops/") {
		t.Errorf("a left-out file reached the public repo:\n%s", objs)
	}
}

func TestPublishRedactsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://x-access-token:" + "s3cret@github.com/a/b.git": "https://***@github.com/a/b.git",
		"git push --atomic https://tok3n@github.com/a/b x":      "git push --atomic https://***@github.com/a/b x",
		"git@github.com:a/b.git":                                "git@github.com:a/b.git",
		"https://github.com/a/b.git":                            "https://github.com/a/b.git",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	// -to carries a token; git rewrites it to the fixture's public repo, so every line that names -to is printed.
	f := newFixture(t)
	to := "https://user:" + "s3cret@example.invalid/pub.git" // in pieces: this file passes the public-tree gate
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+f.pub+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", to)
	o := f.opts("v0.1.0")
	o.To = to
	for _, push := range []bool{false, true} {
		o.Push = push
		if err := run(o); err != nil {
			t.Fatal(err)
		}
	}
	err := run(o) // the tag exists now: the refusal names -to
	if err == nil || !strings.Contains(err.Error(), "already has tag") {
		t.Fatalf("err = %v, want the tag refusal", err)
	}
	all := err.Error() + "\n" + f.out.String()
	if strings.Contains(all, "s3cret") || strings.Count(all, "https://***@example.invalid/pub.git") < 3 {
		t.Errorf("-to's credential printed, or -to not named:\n%s", all)
	}
}

// The four assignments a token's shape misses (re-review of #729). A key-like value is refused by gitleaks on its
// own. A word-like one gitleaks lets through is the judge's: it reads every config-like file, its "no" refuses, and
// the refusal never prints the value. Values are built from pieces so this file passes the scans it tests.
func TestPublishRefusesConfigCredentials(t *testing.T) {
	rows := []struct{ file, line, keyLike, wordLike string }{
		{".env", "AWS_SECRET_ACCESS_KEY=", "Zq3vR8tWm2LpX9kN4bY7" + "cH1jF6sD0gE5aU2iO8rT", "kestrel" + "-harbor"},
		{"deploy/db.env", "DATABASE_PASSWORD=", "Vt7pLq9R" + "z2mXw4Kd", "Summer" + "1987"},
		{"scripts/cf.sh", "export CLOUDFLARE_API_TOKEN=", "Hk3Pz8Qw2Lm9Rx4Tv7Ny" + "1Bc6Df0Gj5Sa2Ue8Wi3", "blue" + "-otter-lamp"},
		{"config/app.yaml", "db:\n  user: app\n  password: ", `"Tr0ub4dorQz9` + `xK2m"`, `"maple` + `syrup77"`},
	}
	try := func(t *testing.T, file, body string, j *fakeJudge) (*fixture, error) {
		f := newFixture(t)
		f.write(file, body)
		f.land("config")
		o := f.opts("v0.1.0")
		o.Judge = j
		err := run(o)
		if err != nil && strings.Contains(err.Error()+f.out.String(), strings.TrimPrefix(body, "db:")) {
			t.Errorf("the refusal printed the value: %v", err)
		}
		return f, err
	}
	for _, r := range rows {
		t.Run(r.file, func(t *testing.T) {
			f, err := try(t, r.file, r.line+r.keyLike+"\n", &fakeJudge{})
			if err == nil || !strings.Contains(err.Error(), r.file+":") || !strings.Contains(err.Error(), "gitleaks:") {
				t.Errorf("key-like value: err = %v, want gitleaks' refusal naming %s", err, r.file)
			}
			if refs := f.git(f.pub, "for-each-ref"); refs != "" {
				t.Errorf("published:\n%s", refs)
			}
			f, err = try(t, r.file, r.line+r.wordLike+"\n", &fakeJudge{bad: []string{r.wordLike}})
			if err == nil || !strings.Contains(err.Error(), "config judge found") || !strings.Contains(err.Error(), r.file) {
				t.Errorf("word-like value: err = %v, want the judge's refusal naming %s", err, r.file)
			}
			if refs := f.git(f.pub, "for-each-ref"); refs != "" {
				t.Errorf("published:\n%s", refs)
			}
			j := &fakeJudge{} // a judge that passes it: nothing else stood in the way, and the judge read the file
			if _, err = try(t, r.file, r.line+r.wordLike+"\n", j); err != nil || !slices.Contains(j.seen, r.file) {
				t.Errorf("word-like value, judge passes: err = %v, judge read %v; want only the judge deciding", err, j.seen)
			}
		})
	}
}

// No judge, a judge error and an answer outside the schema each hold the release: never a pass.
func TestPublishHoldsWithoutAJudge(t *testing.T) {
	for name, j := range map[string]Caller{
		"none":       nil,
		"error":      &fakeJudge{err: errors.New("claude: rate limited")},
		"bad answer": &fakeJudge{raw: `{"verdict":"looks fine"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write("deploy/app.yaml", "name: app\n")
			f.land("config")
			o := f.opts("v0.1.0")
			o.Judge = j
			if err := run(o); err == nil || !strings.Contains(err.Error(), "held") {
				t.Fatalf("err = %v, want held", err)
			}
			if refs := f.git(f.pub, "for-each-ref"); refs != "" {
				t.Errorf("a held release was published:\n%s", refs)
			}
		})
	}
}

// gitleaks reads a binary file's strings; nothing in the tree (.gitleaksignore, .gitleaks.toml, a gitleaks:allow
// comment, a "*" allow line) turns a rule off; only an allow line naming the gitleaks rule lets a fixture through.
func TestPublishGitleaksCannotBeTurnedOffByTheTree(t *testing.T) {
	ghp := "ghp_" + "Ab3dE5gH7jK9mN1pQ3sT5vX7zB9dF1hJ3kL5"
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"binary file": {map[string]string{"assets/logo.bin": "\x89PNG\x00\x01" + ghp + "\x00\x02"},
			"assets/logo.bin:1: gitleaks:github-pat"},
		"tree config and ignore": {map[string]string{
			"src/tok.go":         "var tok = \"" + ghp + "\" // gitleaks:allow\n",
			".gitleaksignore":    "src/tok.go:github-pat:1\n",
			".gitleaks.toml":     "[allowlist]\npaths = ['''.*''']\n",
			publictree.AllowFile: "src/** *  # every gate rule, never gitleaks'\n",
		}, "src/tok.go:1: gitleaks:github-pat"},
		"allowed by name": {map[string]string{
			"src/tok_test.go":    "var planted = \"" + ghp + "\"\n",
			publictree.AllowFile: "src/tok_test.go secret  # a planted fake token\nsrc/tok_test.go gitleaks:github-pat  # the same\n",
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			for p, body := range c.files {
				f.write(p, body)
			}
			f.land("files")
			err := run(f.opts("v0.1.0"))
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), ghp[4:]) {
				t.Errorf("the refusal printed the token: %v", err)
			}
		})
	}
}

// Gitleaks skips files by name (*.svg, *.pdf, go.sum, package-lock.json, node_modules/): every blob is scanned under a
// neutral name, so a generic key in a text file with one of those names is refused, and named by its real path.
func TestPublishScansFilesGitleaksSkipsByName(t *testing.T) {
	line := "DATABASE_PASSWORD=" + "Vt7pLq9R" + "z2mXw4Kd\n"
	for _, p := range []string{"assets/logo.svg", "docs/manual.pdf", "go.sum", "web/package-lock.json",
		"web/node_modules/pkg/index.js", "node_modules/pkg/config.txt"} {
		t.Run(p, func(t *testing.T) {
			f := newFixture(t)
			f.write(p, "<svg>\n"+line)
			f.land("files")
			o := f.opts("v0.1.0")
			o.Judge = &fakeJudge{}
			err := run(o)
			if err == nil || !strings.Contains(err.Error(), p+":2: gitleaks:") {
				t.Fatalf("err = %v, want gitleaks' refusal naming %s", err, p)
			}
			if refs := f.git(f.pub, "for-each-ref"); refs != "" {
				t.Errorf("published:\n%s", refs)
			}
		})
	}
}

// The judge reads every config-like file of the public tree and nothing else: not code, not a left-out file, not
// one an allow line hands to a person; a big file goes in parts.
func TestPublishJudgeReadsConfigLikeFiles(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{".env.example", "deploy/Dockerfile", ".github/workflows/ci.yml", "a.toml", "b.json",
		"c.ini", "run.sh", "ops/secret.yaml", "main.go", "docs/x.md", "fixtures/ok.yml"} {
		f.write(p, "x: 1\n")
	}
	f.write("big.json", strings.Repeat("\"k\": 1,\n", judgeBatch/8+100))
	f.write(publictree.AllowFile, "fixtures/ok.yml judge  # a fixture a person has read\n")
	f.land("files")
	j := &fakeJudge{}
	o := f.opts("v0.1.0")
	o.Judge = j
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	sort.Strings(j.seen)
	want := ".env.example .github/workflows/ci.yml a.toml b.json big.json (part 1 of 2) big.json (part 2 of 2) c.ini " +
		"deploy/Dockerfile run.sh scripts/public-files.sh"
	if got := strings.Join(j.seen, " "); got != want {
		t.Errorf("the judge read:\n%s\nwant:\n%s", got, want)
	}
}

// A file under the override directory is published in the place of the file at the rest of its path, and the
// override directory itself never is. The gate scans the override: it is what the public sees.
func TestPublishCarriesTheOverrideInTheFilesPlace(t *testing.T) {
	f := newFixture(t)
	f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude+
		"internal/deps/publiccopy/**  # the public copy's own versions\n")
	f.write(".github/workflows/ci.yml", "runs-on: internal-runner\n")
	f.write("internal/deps/publiccopy/.github/workflows/ci.yml", "runs-on: ubuntu-latest\n")
	f.land("override")
	if err := run(f.opts("v0.1.0")); err != nil {
		t.Fatal(err)
	}
	if got := f.git(f.pub, "show", "main:.github/workflows/ci.yml"); got != "runs-on: ubuntu-latest" {
		t.Errorf("public ci.yml = %q, want the override", got)
	}
	if objs := f.git(f.pub, "rev-list", "--objects", "--all"); strings.Contains(objs, "publiccopy") || strings.Contains(objs, "internal-runner") {
		t.Errorf("the override directory or the private version reached the public repo:\n%s", objs)
	}

	f.write("internal/deps/publiccopy/.github/workflows/ci.yml", "export AWS_ACCESS_KEY_ID="+fakeAWSKey+"\n")
	f.land("a secret in the override")
	if err := run(f.opts("v0.2.0")); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("an override with a secret published: %v", err)
	}
}

// An override must replace a file the copy keeps: one for a left-out or missing file would publish what the
// exclude list leaves out, or a file no one reads in the private tree.
func TestPublishRefusesAnOverrideForAFileTheCopyLeavesOut(t *testing.T) {
	for _, target := range []string{"PRIVATE.md", "ops/run.sh", "missing.md"} {
		f := newFixture(t)
		f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude+
			"internal/deps/publiccopy/**  # the public copy's own versions\n")
		f.write("internal/deps/publiccopy/"+target, "public\n")
		f.land("override " + target)
		if err := run(f.opts("v0.1.0")); err == nil || !strings.Contains(err.Error(), "does not keep") {
			t.Errorf("%s: an override for a file the copy does not keep: %v", target, err)
		}
		if refs := f.git(f.pub, "for-each-ref"); refs != "" {
			t.Errorf("%s: pushed: %s", target, refs)
		}
	}
}

// -export writes the public tree, overrides in place, into an empty directory, after the same scans, and needs
// nothing a release needs (tag, identity, soak, the public repo).
func TestPublishExportWritesThePublicTree(t *testing.T) {
	f := newFixture(t)
	f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude+
		"internal/deps/publiccopy/**  # the public copy's own versions\n")
	f.write("CODEOWNERS", "private\n")
	f.write("internal/deps/publiccopy/CODEOWNERS", "public\n")
	f.write("run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(f.priv, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.commit("override, not landed") // not on origin/main: export does not care
	dir := filepath.Join(t.TempDir(), "tree")
	o := opts{Dir: f.priv, Ref: "HEAD", Export: dir, Out: &f.out, Cache: testCache, Judge: &fakeJudge{},
		ReleasedAt: func(string, string) (time.Time, error) { return time.Time{}, errors.New("no release") }}
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{"CODEOWNERS", "README.md", "go.mod", "internal/deps/public_exclude.txt", "run.sh", "scripts/public-files.sh"}; !slices.Equal(got, want) {
		t.Errorf("exported %v, want %v", got, want)
	}
	if st, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh lost its executable bit: %v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CODEOWNERS")); string(b) != "public\n" {
		t.Errorf("CODEOWNERS = %q, want the override", b)
	}
	if !strings.Contains(f.out.String(), "public tree ") {
		t.Errorf("no report: %q", f.out.String())
	}
	if err := run(o); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("export into a non-empty directory: %v", err)
	}
	o.Export, o.Push = filepath.Join(t.TempDir(), "x"), true
	if err := run(o); err == nil || !strings.Contains(err.Error(), "never pushes") {
		t.Errorf("-export with -push: %v", err)
	}
}

// An override keeps its file's mode: an executable bit gained or lost, or a symlink, would change what the copy runs.
func TestPublishRefusesAnOverrideOfAnotherMode(t *testing.T) {
	f := newFixture(t)
	f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude+
		"internal/deps/publiccopy/**  # the public copy's own versions\n")
	f.write("internal/deps/publiccopy/README.md", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(f.priv, "internal/deps/publiccopy/README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.land("executable override")
	if err := run(f.opts("v0.1.0")); err == nil || !strings.Contains(err.Error(), "keeps its file's mode") {
		t.Fatalf("an override of another mode: %v", err)
	}
	if err := os.Remove(filepath.Join(f.priv, "internal/deps/publiccopy/README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../PRIVATE.md", filepath.Join(f.priv, "internal/deps/publiccopy/README.md")); err != nil {
		t.Fatal(err)
	}
	f.land("symlink override")
	if err := run(f.opts("v0.1.0")); err == nil || !strings.Contains(err.Error(), "only a regular file") {
		t.Fatalf("a symlink override: %v", err)
	}
}

// The gate's own files (its allow list, its rules, the exclude list) are read from the commit and never overridden.
func TestPublishRefusesAnOverrideOfTheGatesFiles(t *testing.T) {
	f := newFixture(t)
	f.write("internal/deps/public_exclude.txt", "PRIVATE.md  # names our people\nops/**  # ours\n"+namesExclude+
		"internal/deps/publiccopy/**  # the public copy's own versions\n")
	f.write("internal/deps/publictree_allow.txt", "# none\n")
	f.write("internal/deps/publiccopy/internal/deps/publictree_allow.txt", "** *  # everything\n")
	f.land("override the allow list")
	if err := run(f.opts("v0.1.0")); err == nil || !strings.Contains(err.Error(), "never overridden") {
		t.Fatalf("an override of the allow list: %v", err)
	}
}

// The private names list (internal/deps/private_names.txt) is read from the commit: a name in a file the copy keeps
// refuses (and is shown to the operator), one in a left-out file does not, an allow line for the rule lets one
// through, and a commit with no list, or whose copy would carry it, is held.
func TestPublishRefusesPrivateNames(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"name in a kept file":      {map[string]string{"docs/a.md": "Ask Zorbo about it.\n"}, "docs/a.md:1: private-name"},
		"name in a left-out file":  {map[string]string{"ops/b.md": "zorbo\n"}, ""},
		"allowed by name":          {map[string]string{"docs/a.md": "zorbo\n", publictree.AllowFile: "docs/a.md private-name  # a leftover\n"}, ""},
		"a list nothing can parse": {map[string]string{publictree.NamesFile: "zorbo\n"}, "want `<regular expression>  # reason`"},
		"a list that would ship": {map[string]string{
			"internal/deps/public_exclude.txt": "PRIVATE.md  # ours\nops/**  # ours\n"}, "would ship"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			for p, body := range c.files {
				f.write(p, body)
			}
			f.land("names")
			err := run(f.opts("v0.1.0"))
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	t.Run("no list", func(t *testing.T) {
		f := newFixture(t)
		f.git(f.priv, "rm", "-q", publictree.NamesFile)
		f.land("no list")
		err := run(f.opts("v0.1.0"))
		if err == nil || !strings.Contains(err.Error(), "held") || !strings.Contains(err.Error(), publictree.NamesFile) {
			t.Fatalf("err = %v, want a hold naming %s", err, publictree.NamesFile)
		}
	})
	t.Run("the list is never overridden", func(t *testing.T) {
		f := newFixture(t)
		f.write("internal/deps/publiccopy/"+publictree.NamesFile, "zorbo  # a copy of the list\n")
		f.land("override")
		if err := run(f.opts("v0.1.0")); err == nil {
			t.Fatal("an override of the names list was published")
		}
	})
}
