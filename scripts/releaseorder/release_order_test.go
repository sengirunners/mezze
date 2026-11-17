// Package releaseorder tests release-order.py, the script that sets or unsets a machine's own order of release
// repositories (config update_repos) from the order its own zm prints. A fake zm on PATH stands in for the
// machine's zm: it answers doctor --json with the home and daemon update status with the order zm would read
// from that home's config.json.
package releaseorder

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZM reads FAKE_* from its environment: FAKE_HOME (the home doctor names), FAKE_HOME_STATUS (its status),
// FAKE_DEFAULT (the built-in order, ", "-joined), FAKE_OLD (a build without the setting: no order line),
// FAKE_IGNORE (status ignores config.json, as another home's daemon would), FAKE_FORCE_SET (status reports this
// order as config.json's whatever the file holds). It fails if ZM_HOST reaches it or
// ZM_NO_AUTOSTART is not 1, and logs each call to FAKE_LOG.
const fakeZM = `#!/usr/bin/env python3
import json, os, sys
e = os.environ
with open(e["FAKE_LOG"], "a") as f:
    f.write(" ".join(sys.argv[1:]) + "\n")
if "ZM_HOST" in e or e.get("ZM_NO_AUTOSTART") != "1":
    print("zm: wrong environment", file=sys.stderr); sys.exit(9)
args = sys.argv[1:]
if args == ["doctor", "--json"]:
    print(json.dumps([{"check": "home", "detail": e["FAKE_HOME"], "status": e.get("FAKE_HOME_STATUS", "ok")},
                      {"check": "daemon", "detail": "x", "status": "fail"}]))
    sys.exit(1)
if args == ["daemon", "update", "status"]:
    print("m1                   main-000000000000  ok 1m ago")
    if e.get("FAKE_OLD"):
        sys.exit(0)
    try:
        doc = json.load(open(os.path.join(e["FAKE_HOME"], "config.json")))
    except (OSError, ValueError):
        doc = {}
    if e.get("FAKE_FORCE_SET"):
        print("release sources on m1: " + e["FAKE_FORCE_SET"] + " (config update_repos)")
    elif "update_repos" in doc and not e.get("FAKE_IGNORE"):
        print("release sources on m1: " + ", ".join(doc["update_repos"]) + " (config update_repos)")
    else:
        print("release sources on m1: " + e.get("FAKE_DEFAULT", "pub-org/pub, own-org/own") + " (default)")
    sys.exit(0)
print("zm: unexpected " + " ".join(args), file=sys.stderr); sys.exit(8)
`

// config is a config.json with keys of each JSON kind, so a rewrite that loses or respells one shows.
const config = `{"machine":"m1","update_keys":["ed25519:AAAA"],"tailnet":{"enabled":true,"port":7433},` +
	`"share":0.5,"big":12345678901234567890,"exp":1e3,"note":"café <&>","none":null}` + "\n"

type env struct {
	t               *testing.T
	home, bin, logf string
	extra           []string
}

func setup(t *testing.T, body string) *env {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH")
	}
	root, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	e := &env{t: t, home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin"), logf: filepath.Join(root, "log")}
	for _, d := range []string{e.home, e.bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.bin, "zm"), []byte(fakeZM), 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(e.cfg(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) cfg() string { return filepath.Join(e.home, "config.json") }

func (e *env) read() string {
	b, err := os.ReadFile(e.cfg())
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// run runs the script with only the fake zm's directory and the system's on PATH, ZM_HOST set (it must not
// reach zm) and HOME in the temp root.
func (e *env) run(args ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	cmd := exec.Command("./release-order.py", args...) // as zm exec runs it: the file itself, executable
	cmd.Env = append([]string{"PATH=" + e.bin + ":/usr/bin:/bin", "HOME=" + filepath.Dir(e.home), "ZM_HOST=http://remote.invalid",
		"FAKE_HOME=" + e.home, "FAKE_LOG=" + e.logf}, e.extra...)
	var o, er bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &er
	err := cmd.Run()
	if x, ok := err.(*exec.ExitError); ok {
		return x.ExitCode(), o.String(), er.String()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return 0, o.String(), er.String()
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("%v in %s", err, s)
	}
	return m
}

func TestSetPutsTheBuildRepoFirstAndKeepsEveryOtherKey(t *testing.T) {
	e := setup(t, config)
	code, out, errs := e.run("set")
	if code != 0 {
		t.Fatalf("set: exit %d, %s%s", code, out, errs)
	}
	if !strings.HasSuffix(out, "release sources on m1: own-org/own, pub-org/pub (config update_repos)\n") {
		t.Fatalf("set must end with the order zm reads now:\n%s", out)
	}
	before, after := decode(t, config), decode(t, e.read())
	got, _ := json.Marshal(after["update_repos"])
	if string(got) != `["own-org/own","pub-org/pub"]` {
		t.Fatalf("update_repos = %s", got)
	}
	delete(after, "update_repos")
	for k, v := range before {
		a, _ := json.Marshal(after[k])
		b, _ := json.Marshal(v)
		if k == "exp" { // 1e3 may be respelled; it must stay the same number
			if f, err := after[k].(json.Number).Float64(); err != nil || f != 1000 {
				t.Errorf("exp = %v (%v)", after[k], err)
			}
			continue
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: %s, was %s", k, a, b)
		}
	}
	if len(after) != len(before) {
		t.Errorf("keys: %v, was %v", after, before)
	}
	if st, err := os.Stat(e.cfg()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v (%v), want 0600", st.Mode().Perm(), err)
	}
	left, _ := filepath.Glob(filepath.Join(e.home, ".config.json.*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	// Again: nothing to do, the file is untouched.
	was := e.read()
	if code, out, errs := e.run("set"); code != 0 || !strings.Contains(out, "unchanged") || e.read() != was {
		t.Fatalf("second set: exit %d, %s%s", code, out, errs)
	}

	// unset takes the key out and nothing else; again is a no-op.
	if code, out, errs := e.run("unset"); code != 0 || !strings.HasSuffix(out, "pub-org/pub, own-org/own (default)\n") {
		t.Fatalf("unset: exit %d, %s%s", code, out, errs)
	}
	if _, ok := decode(t, e.read())["update_repos"]; ok {
		t.Fatal("unset left update_repos")
	}
	was = e.read()
	if code, out, _ := e.run("unset"); code != 0 || !strings.Contains(out, "unchanged") || e.read() != was {
		t.Fatalf("second unset: exit %d, %s", code, out)
	}
}

func TestShowWritesNothing(t *testing.T) {
	e := setup(t, config)
	code, out, _ := e.run("show")
	if code != 0 || !strings.HasSuffix(out, "release sources on m1: pub-org/pub, own-org/own (default)\n") || e.read() != config {
		t.Fatalf("show: exit %d, %s", code, out)
	}
	log, _ := os.ReadFile(e.logf)
	if strings.Contains(string(log), "update --release") {
		t.Fatalf("show ran an update: %s", log)
	}
}

// Every refusal exits 1 and leaves config.json byte for byte as it was.
func TestRefusesAndWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name, body string
		extra      []string
		verb, want string
	}{
		{"a build without the setting", config, []string{"FAKE_OLD=1"}, "set", "predates"},
		{"a build from the public repo reads one repository", config, []string{"FAKE_DEFAULT=pub-org/pub"}, "set", "no build repository"},
		{"a name that is not owner/name", config, []string{"FAKE_DEFAULT=pub-org/pub, own-org/own.git"}, "set", "not a GitHub repository"},
		{"a home doctor does not pass", config, []string{"FAKE_HOME_STATUS=warn"}, "set", "home check is warn"},
		{"a key twice", `{"machine":"m1","machine":"m2"}`, nil, "set", "twice"},
		{"not an object", `["x"]`, nil, "set", "not a JSON object"},
		{"not JSON", `{"machine":`, nil, "set", "not JSON"},
		{"NaN is not JSON", `{"share":NaN}`, nil, "set", "not JSON"},
		{"a lone surrogate UTF-8 cannot hold", `{"note":"\ud800"}`, nil, "set", "cannot be written back"},
		{"a key zm reads in another case", `{"Update_Repos":["a/b"]}`, nil, "set", "fix it by hand"},
		{"zm reads another home than it names", config, []string{"FAKE_IGNORE=1"}, "set", "put back"},
		{"unset where zm reads an order the file does not hold", `{"machine":"m1"}`, []string{"FAKE_FORCE_SET=x/y, z/w"}, "unset", "has no update_repos"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, c.body)
			e.extra = c.extra
			code, out, errs := e.run(c.verb)
			if code != 1 || !strings.Contains(errs, c.want) {
				t.Fatalf("exit %d, want 1 and %q:\n%s%s", code, c.want, out, errs)
			}
			if e.read() != c.body {
				t.Fatalf("config.json changed:\n%s", e.read())
			}
		})
	}
}

func TestSymlinkIsRefused(t *testing.T) {
	e := setup(t, "")
	target := filepath.Join(filepath.Dir(e.home), "real.json")
	if err := os.WriteFile(target, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.cfg()); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := e.run("set"); code != 1 || !strings.Contains(errs, "symbolic link") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if fi, err := os.Lstat(e.cfg()); err != nil || fi.Mode()&os.ModeSymlink == 0 || e.read() != config {
		t.Fatal("the link or its file changed")
	}
}

func TestNoConfigFileIsRefused(t *testing.T) {
	e := setup(t, "")
	if code, _, errs := e.run("set"); code != 1 || !strings.Contains(errs, "cannot read") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(e.cfg()); err == nil {
		t.Fatal("set made a config.json")
	}
}

func TestUsage(t *testing.T) {
	e := setup(t, config)
	for _, args := range [][]string{nil, {"put"}, {"set", "extra"}} {
		if code, _, _ := e.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// The fake prints the lines zm's own status prints: if zm's wording changes, this fails before the script
// silently stops reading it (release-order.py parses exactly these).
func TestFakeLinesAreZMs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "zm", "daemon_update.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`const updateReposKey = "update_repos"`,
		`"release sources on %s: %s (config %s)\n", machine, strings.Join(repos, ", "), updateReposKey)`,
		`"release sources on %s: %s (default)\n", machine, strings.Join(repos, ", "))`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("cmd/zm/daemon_update.go no longer has %s: update release-order.py and this test's fake", want)
		}
	}
}
