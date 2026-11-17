package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/spf13/cobra"
)

// TestCIPRIsPotlucks: zm ci pr runs Potluck's own command against zm's
// daemon (ZM_HOME's socket), with Potluck's refusals and exit code and
// Potluck's command names said as zm's. Only refusals that come before any
// gh call are run: tests never reach GitHub.
func TestCIPRIsPotlucks(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.URL.Path == "/v0/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"pool":"","label":""}`)) // GET /v0/runners/setup: lends to no pool yet
	}))
	t.Setenv(cli.EnvNoAutostart, "")
	run := func(args ...string) (string, error) { return runCIPR(t, args...) }
	// Without --job Potluck picks the one macOS job from GitHub, but only
	// after this machine's pool: the pool refusal still comes first.
	if msg, err := run("acme/worm"); err != exitSilently(1) || !strings.Contains(msg, "zm runners setup <pool> first") || strings.Contains(msg, "potluck") {
		t.Fatalf("no --job: %v %q asked %v", err, msg, seen())
	}
	if os.Getenv(cli.EnvNoAutostart) != "1" {
		t.Fatal("Potluck's autostart was left on: it would start a daemon under POTLUCK_HOME")
	}
	msg, err := run("acme/worm", "--job", "native", "--as", "mini/aaaaaaaa", "--dry-run")
	if err != exitSilently(1) || !strings.Contains(msg, "zm runners setup <pool> first") || strings.Contains(msg, "potluck") {
		t.Fatalf("no pool: %v %q", err, msg)
	}
	if got := seen(); len(got) != 2 || got[0] != "GET /v0/runners/setup" || got[1] != got[0] {
		t.Fatalf("zm's daemon was not the one asked: %v", got)
	}
	if msg, err := run("acme/worm", "--job", "native", "--json"); err != exitSilently(1) || !strings.Contains(msg, "zm runners setup <pool> first") || strings.Contains(msg, "potluck") {
		t.Fatalf("--json: Potluck's errors still say zm: %v %q", err, msg)
	}
	if _, err := run("acme/worm", "--nope"); err != exitSilently(2) {
		t.Fatalf("a usage error is Potluck's exit 2: %v", err)
	}
}

// TestCIPRWithoutZmDaemon: with no daemon (autostart off) Potluck's
// refusal says zm's daemon and keeps its exit codes; a remote client is
// refused before Potluck runs, never answered by an empty daemon.
func TestCIPRWithoutZmDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	for _, k := range []string{"ZM_AS", "ZM_SESSION", "ZM_TOKEN", "ZM_PROJECT", "ZM_HOST"} {
		t.Setenv(k, "")
	}
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_MACHINE", "mini")
	t.Setenv("ZM_NO_AUTOSTART", "1")
	t.Setenv(cli.EnvNoAutostart, "")
	if msg, err := runCIPR(t, "acme/worm", "--job", "native"); err != exitSilently(3) || !strings.Contains(msg, "the zm daemon is not running") || !strings.Contains(msg, "zm daemon --detach") || strings.Contains(msg, "potluck") {
		t.Fatalf("no daemon: %v %q", err, msg)
	}
	if _, err := runCIPR(t, "acme/worm", "--nope"); err != exitSilently(2) {
		t.Fatalf("a usage error keeps Potluck's exit 2 with no daemon: %v", err)
	}
	if _, err := runCIPR(t, "--help"); err != nil {
		t.Fatalf("help needs no daemon: %v", err)
	}
	t.Setenv("ZM_HOST", "https://studio.example")
	var e *api.Error
	if _, err := runCIPR(t, "acme/worm", "--job", "native"); !errors.As(err, &e) || e.Code != api.CodeBadRequest || !strings.Contains(e.Message, "not a remote daemon") {
		t.Fatalf("remote client: %v", err)
	}
}

// runCIPR runs zm ci pr args, returning its stderr.
func runCIPR(t *testing.T, args ...string) (string, error) {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	defer func() { os.Stderr = old }()
	oldJSON := flagJSON
	defer func() { flagJSON = oldJSON }()
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	_, err = runCLI(t, r, append([]string{"ci", "pr"}, args...)...)
	f.Close()
	b, _ := os.ReadFile(f.Name())
	return string(b), err
}

func TestPotluckArgs(t *testing.T) {
	args, jsonOut, help := potluckArgs([]string{"a/b", "--as", "mini/x", "--as=y", "--project", "home", "--project=p2", "--json=true", "--", "--as"})
	if want := []string{"a/b", "--pool", "home", "--pool=p2", "--json=true", "--", "--as"}; strings.Join(args, " ") != strings.Join(want, " ") || !jsonOut || help {
		t.Fatalf("got %q json=%v help=%v", args, jsonOut, help)
	}
	if _, jsonOut, help := potluckArgs([]string{"--json=false", "-h"}); jsonOut || !help {
		t.Fatalf("json=%v help=%v", jsonOut, help)
	}
}

func TestZmWordsLeaveDiffAndScript(t *testing.T) {
	var b strings.Builder
	w := zmWords{w: &b}
	for _, s := range []string{"potluck: no\n", "--- a/x.yml\n+  run: potluck ci doctor\n", "#!/bin/sh\n# potluck ci pr prepared\n", "next: potluck ci pr a/b\n"} {
		_, _ = w.Write([]byte(s))
	}
	if want := "zm: no\n--- a/x.yml\n+  run: potluck ci doctor\n#!/bin/sh\n# potluck ci pr prepared\nnext: zm ci pr a/b\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

// TestIsolationIsPotlucks: zm isolation is Potluck's isolation command
// against zm's daemon (ZM_HOME's socket), its words said as zm's; a remote
// client is refused before Potluck runs.
func TestIsolationIsPotlucks(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/v0/health":
			_, _ = w.Write([]byte(`{"ok":true,"machine":"mini"}`))
		case "/v0/run_targets":
			_, _ = w.Write([]byte(`{"targets":[{"name":"mac-m","kind":"box","driver":"tart","error":"driver down"}]}`))
		default:
			_, _ = w.Write([]byte(`{"projects":[]}`))
		}
	}))
	t.Setenv(cli.EnvNoAutostart, "")
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(potluckIsolationCmd())
	out, err := runCLI(t, r, "isolation")
	if err != nil || !strings.Contains(out, "mac-m (sandbox size): not checked: its driver did not answer (driver down)") || strings.Contains(out, "potluck") {
		t.Fatalf("zm isolation: %v %q", err, out)
	}
	mu.Lock()
	got := strings.Join(asked, ",")
	mu.Unlock()
	if !strings.Contains(got, "GET /v0/run_targets?host=1") {
		t.Fatalf("zm's daemon was not the one asked: %s", got)
	}
	help, err := runCLI(t, r, "isolation", "--help")
	if err != nil || !strings.Contains(help, "zm isolation test") || strings.Contains(help, "potluck isolation") || strings.Contains(help, "POTLUCK_HOME") {
		t.Fatalf("help: %v %q", err, help)
	}
	t.Setenv("ZM_HOST", "https://studio.example")
	var e *api.Error
	if _, err := runCLI(t, r, "isolation"); !errors.As(err, &e) || e.Code != api.CodeBadRequest || !strings.Contains(e.Message, "zm isolation reads this machine's own daemon") {
		t.Fatalf("remote client: %v", err)
	}
}

// TestForgetIsPotlucks: zm forget is Potluck's forget against zm's daemon:
// it shows the bound key, forgets nothing without a yes, then sends the
// shown fingerprint; its words (help, question, result) say zm and project.
func TestForgetIsPotlucks(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch {
		case r.URL.Path == "/v0/health":
			_, _ = w.Write([]byte(`{"ok":true,"machine":"mini"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v0/team/project-keys/carpool":
			_, _ = w.Write([]byte(`{"project":"carpool","via":"tailnet","fingerprint":"SHA256:old","authority":"studio","bound":"2026-11-01T10:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v0/team/project-keys/carpool/forget":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			posted = append(posted, string(b))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"key":{"project":"carpool","via":"tailnet","fingerprint":"SHA256:old"},"host_state":"drop","dropped":["share mini-c2m4"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv(cli.EnvNoAutostart, "")
	run := func(stdin string, args ...string) (out, errOut string, err error) {
		t.Helper()
		in, e := os.CreateTemp(t.TempDir(), "in")
		if e != nil {
			t.Fatal(e)
		}
		_, _ = in.WriteString(stdin)
		_, _ = in.Seek(0, 0)
		defer in.Close()
		ef, e := os.CreateTemp(t.TempDir(), "err")
		if e != nil {
			t.Fatal(e)
		}
		oldIn, oldErr := os.Stdin, os.Stderr
		os.Stdin, os.Stderr = in, ef
		defer func() { os.Stdin, os.Stderr = oldIn, oldErr }()
		r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
		r.AddCommand(potluckForgetCmd())
		out, err = runCLI(t, r, append([]string{"forget"}, args...)...)
		ef.Close()
		b, _ := os.ReadFile(ef.Name())
		return out, string(b), err
	}
	sent := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), posted...) }
	if _, e, err := run("", "carpool"); err != exitSilently(1) || !strings.Contains(e, "--keep-host-state or --drop-host-state") || len(sent()) != 0 {
		t.Fatalf("no host-state choice: %v %q %v", err, e, sent())
	}
	_, e, err := run("no\n", "carpool", "--drop-host-state")
	if err != exitSilently(1) || !strings.Contains(e, "project carpool is tied to the key of studio (SHA256:old over tailnet") ||
		!strings.Contains(e, "drops this machine's shares, the project's sandboxes here and the project's key tag") || !strings.Contains(e, "nothing was forgotten") || len(sent()) != 0 {
		t.Fatalf("answer no: %v %q %v", err, e, sent())
	}
	out, e, err := run("yes\n", "carpool", "--drop-host-state")
	if err != nil || !strings.Contains(out, "forgot key SHA256:old over tailnet for project carpool") || !strings.Contains(out, "dropped: share mini-c2m4") {
		t.Fatalf("answer yes: %v %q %q", err, out, e)
	}
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], `"host_state":"drop"`) || !strings.Contains(got[0], `"fingerprint":"SHA256:old"`) {
		t.Fatalf("forget body: %v", got)
	}
	if out, _, err := run("", "carpool", "--keep-host-state", "--yes"); err != nil || !strings.Contains(out, "for project carpool") {
		t.Fatalf("--yes: %v %q", err, out)
	}
	if got := sent(); len(got) != 2 || !strings.Contains(got[1], `"host_state":"keep"`) || !strings.Contains(got[1], `"fingerprint":"SHA256:old"`) {
		t.Fatalf("--yes body: %v", got)
	}
	help, _, err := run("", "--help")
	if err != nil || !strings.Contains(help, "zm forget <project id>") || strings.Contains(help, "potluck") || strings.Contains(help, "the pool") || strings.Contains(help, "pool id") {
		t.Fatalf("help: %v %q", err, help)
	}
}
