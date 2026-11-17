package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// zeroEnv is a fresh machine: HOME is an empty temp dir, ZM_HOME is unset
// (so the default ~/.zm is used), nothing from the caller's agent session,
// shell trackers or real home leaks in.
type zeroEnv struct {
	t    *testing.T
	home string
	env  []string
}

func newZeroEnv(t *testing.T) *zeroEnv {
	t.Helper()
	home, err := os.MkdirTemp("", "pz") // short: ~/.zm/zm.sock must fit in 103 bytes
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	z := &zeroEnv{t: t, home: home}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZM_") || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") ||
			strings.HasPrefix(k, "DOLT_") || k == "HOME" || k == "POTLUCK_BOX_CREDENTIAL" || k == "CLAUDE_CODE_SESSION_ID" || k == "CODEX_THREAD_ID" || k == "CLAUDECODE" {
			continue
		}
		z.env = append(z.env, kv)
	}
	z.env = append(z.env, "HOME="+home, "ZM_TEST_MAIN=1", "ZM_MACHINE=zero", "ZM_HUMAN=ana")
	t.Cleanup(func() {
		z.stopDaemon()
		os.RemoveAll(home)
	})
	return z
}

func (z *zeroEnv) zmHome() string { return filepath.Join(z.home, ".zm") }

// run runs zm (this test binary) in dir with extra env.
func (z *zeroEnv) run(dir string, extra []string, args ...string) (stdout, stderr string, code int) {
	z.t.Helper()
	c := exec.Command(os.Args[0], args...)
	c.Dir = dir
	c.Env = append(append([]string(nil), z.env...), extra...)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		z.t.Fatalf("zm %v: %v", args, err)
	}
	return o.String(), e.String(), code
}

// daemonPID is the pid in daemon.lock, 0 when no daemon process lives.
func (z *zeroEnv) daemonPID() int {
	b, err := os.ReadFile(filepath.Join(z.zmHome(), "daemon.lock"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func (z *zeroEnv) stopDaemon() {
	pid := z.daemonPID()
	if pid == 0 {
		return
	}
	_, _, _ = z.run(z.home, nil, "daemon", "stop", "--force")
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// gitRepo makes a repository (without running git) with an origin remote.
func gitRepo(t *testing.T, dir, origin string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n"
	if origin != "" {
		cfg += "[remote \"origin\"]\n\turl = " + origin + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const startedLine = "zm: started the daemon (~/.zm)\n"

// TestAutostartRules: which commands may start the daemon (SPEC 17.1).
func TestAutostartRules(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	never := [][]string{{"help"}, {"completion"}, {"completion", "zsh"}, {"status"}, {"daemon"}, {"daemon", "stop"},
		{"mcp"}, {"hook"}, {"notice"}, {"session", "hook"}, {"session", "register"}, {"session", "state"}}
	for _, args := range never {
		cmd, _, err := rootCmd.Find(args)
		if err != nil || cmd == rootCmd {
			// cobra adds help and completion lazily; build them like Execute does
			rootCmd.InitDefaultHelpCmd()
			rootCmd.InitDefaultCompletionCmd()
			if cmd, _, err = rootCmd.Find(args); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		}
		if autostartAllowed(cmd, env(nil)) {
			t.Errorf("zm %s may autostart; it must never", strings.Join(args, " "))
		}
	}
	for _, args := range [][]string{{"init"}, {"work", "new"}, {"work", "ls"}, {"inbox"}, {"send"}, {"ls"}, {"project", "why"}, {"run"}} {
		cmd, _, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !autostartAllowed(cmd, env(nil)) {
			t.Errorf("zm %s does not autostart", strings.Join(args, " "))
		}
		for _, v := range []string{"1", "true", "yes"} {
			if autostartAllowed(cmd, env(map[string]string{EnvNoAutostart: v})) {
				t.Errorf("zm %s autostarts with %s=%s", strings.Join(args, " "), EnvNoAutostart, v)
			}
		}
		if !autostartAllowed(cmd, env(map[string]string{EnvNoAutostart: "0"})) {
			t.Errorf("zm %s: %s=0 must not turn autostart off", strings.Join(args, " "), EnvNoAutostart)
		}
	}
	if autostartAllowed(nil, env(nil)) {
		t.Error("no running command (a test calling newClient) must not autostart")
	}
}

// TestNeverAutostarts: on a fresh machine, the excluded commands start no
// daemon and create no ~/.zm; a hook with no daemon is silent and quick.
func TestNeverAutostarts(t *testing.T) {
	z := newZeroEnv(t)
	cases := []struct {
		args  []string
		extra []string
		code  int
	}{
		{[]string{"--version"}, nil, 0},
		{[]string{"help"}, nil, 0},
		{[]string{"completion", "bash"}, nil, 0},
		{[]string{"status"}, nil, 3},
		{[]string{"daemon", "stop"}, nil, 3},
		{[]string{"notice"}, nil, 0},
		{[]string{"hook", "claude", "prompt", `{"session_id":"zero-test"}`}, []string{"CLAUDE_CODE_SESSION_ID=zero-test"}, 0},
		{[]string{"hook", "claude", "session-start", `{"session_id":"zero-test"}`}, []string{"CLAUDE_CODE_SESSION_ID=zero-test"}, 0},
		{[]string{"work", "ls"}, []string{EnvNoAutostart + "=1"}, 3},
		{[]string{"work", "new", "x"}, []string{EnvNoAutostart + "=1"}, 3},
		{[]string{"work", "ls"}, []string{"ZM_HOST=127.0.0.1:1", "ZM_TOKEN=zm_x"}, 1}, // remote: never this machine's daemon
	}
	for _, tc := range cases {
		start := time.Now()
		_, stderr, code := z.run(z.home, tc.extra, tc.args...)
		d := time.Since(start)
		if code != tc.code {
			t.Errorf("zm %v: exit %d, want %d (stderr %q)", tc.args, code, tc.code, stderr)
		}
		if strings.Contains(stderr, "started the daemon") {
			t.Errorf("zm %v started the daemon", tc.args)
		}
		if tc.args[0] == "hook" && (stderr != "" || d > time.Second) {
			t.Errorf("zm %v with no daemon: %s, stderr %q (must be silent and within its budget)", tc.args, d, stderr)
		}
		if _, err := os.Stat(z.zmHome()); err == nil {
			if z.daemonPID() != 0 {
				t.Fatalf("zm %v started a daemon", tc.args)
			}
			t.Errorf("zm %v created %s", tc.args, z.zmHome())
			os.RemoveAll(z.zmHome())
		}
	}
}

// TestZeroInstallStart: a fresh HOME goes from nothing to a tracked work item
// with two commands; the first starts the daemon and says so in one line.
func TestZeroInstallStart(t *testing.T) {
	z := newZeroEnv(t)
	repo := gitRepo(t, filepath.Join(z.home, "code", "MyApp"), "git@github.com:acme/my-app.git")
	sub := filepath.Join(repo, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, stderr, code := z.run(sub, nil, "init")
	if code != 0 || stderr != startedLine {
		t.Fatalf("init: exit %d, stderr %q (want exactly %q), out %q", code, stderr, startedLine, out)
	}
	for _, want := range []string{"created project my-app (slug from the repo name; path " + repo + ", remote github.com/acme/my-app)",
		"zm work new", "zm run", "zm ls"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output lacks %q:\n%s", want, out)
		}
	}
	out, stderr, code = z.run(sub, nil, "work", "new", "first", "task")
	t.Logf("fresh HOME to first work item: %s", time.Since(start))
	if code != 0 || stderr != "" || !strings.HasPrefix(out, "created my-app/local:") {
		t.Fatalf("work new: exit %d, stderr %q, out %q", code, stderr, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".zm")); err == nil {
		t.Error("init without --pin wrote into the repo")
	}

	// Again: names the project, exits 0, starts nothing.
	out, stderr, code = z.run(repo, nil, "init")
	if code != 0 || stderr != "" || !strings.Contains(out, "is already project my-app (rule cwd-path)") || !strings.Contains(out, "zm ls") {
		t.Errorf("init again: exit %d, stderr %q, out %q", code, stderr, out)
	}
	// $ZM_PROJECT does not make a directory "already resolve".
	plain := filepath.Join(z.home, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _, code = z.run(plain, []string{"ZM_PROJECT=my-app"}, "init")
	if code != 0 || !strings.Contains(out, "created project notes (slug from the directory name; path "+plain+")") || strings.Contains(out, "remote") {
		t.Errorf("init outside git: exit %d, out %q", code, out)
	}

	// The same repo name elsewhere: refused with a hint to pass --slug, never my-app-2.
	other := gitRepo(t, filepath.Join(z.home, "fork", "my-app"), "https://github.com/someone/my-app")
	out, stderr, code = z.run(other, nil, "init")
	if code != 1 || !strings.Contains(stderr, `project "my-app" already exists`) || !strings.Contains(stderr, "--slug") {
		t.Errorf("collision: exit %d, stderr %q, out %q", code, stderr, out)
	}
	if ls, _, _ := z.run(z.home, nil, "project", "ls"); strings.Contains(ls, "my-app-") {
		t.Errorf("a suffixed slug was created:\n%s", ls)
	}
	out, _, code = z.run(other, nil, "init", "--slug", "fork", "--pin")
	pin, err := os.ReadFile(filepath.Join(other, ".zm", "project.toml"))
	if code != 0 || !strings.Contains(out, "created project fork (slug from --slug") || err != nil || !strings.Contains(string(pin), `project = "fork"`) {
		t.Errorf("--slug --pin: exit %d, out %q, pin %q %v", code, out, pin, err)
	}
	if why, _, _ := z.run(other, nil, "project", "why"); !strings.HasPrefix(why, "fork (rule: cwd-file)") {
		t.Errorf("after --pin: %s", why)
	}
	// A nested project with an explicit --slug: only for a directory deeper
	// than the rule that claims it (its longer path wins); a subdirectory of
	// the same repo (same root) is refused.
	deeper := filepath.Join(plain, "drafts")
	_ = os.MkdirAll(deeper, 0o755)
	if out, _, code = z.run(deeper, nil, "init", "--slug", "drafts"); code != 0 || !strings.Contains(out, "created project drafts") || strings.Contains(out, "note:") {
		t.Errorf("nested --slug: exit %d, out %q", code, out)
	}
	if why, _, _ := z.run(deeper, nil, "project", "why"); !strings.HasPrefix(why, "drafts (rule: cwd-path)") {
		t.Errorf("nested, then why: %s", why)
	}
	if _, stderr, code = z.run(sub, nil, "init", "--slug", "tools"); code != 1 || !strings.Contains(stderr, "already belongs to project my-app") {
		t.Errorf("same-root --slug: exit %d, stderr %q", code, stderr)
	}
	// Bad slugs and the home directory are refused.
	if _, stderr, code = z.run(plain, nil, "init", "--slug", "Bad Slug"); code != 1 || !strings.Contains(stderr, "bad slug") {
		t.Errorf("bad --slug: exit %d, %q", code, stderr)
	}
	if _, stderr, code = z.run(z.home, nil, "init"); code != 1 || !strings.Contains(stderr, "refusing") {
		t.Errorf("init in HOME: exit %d, %q", code, stderr)
	}
	// A teammate's pinned repo whose project this machine lacks: init uses the pinned slug.
	team := gitRepo(t, filepath.Join(z.home, "team-checkout"), "")
	_ = os.MkdirAll(filepath.Join(team, ".zm"), 0o755)
	_ = os.WriteFile(filepath.Join(team, ".zm", "project.toml"), []byte("project = \"teamproj\"\n"), 0o644)
	if out, _, code = z.run(team, nil, "init"); code != 0 || !strings.Contains(out, "created project teamproj (slug from .zm/project.toml") {
		t.Errorf("pinned missing: exit %d, out %q", code, out)
	}
	if why, _, _ := z.run(team, nil, "project", "why"); !strings.HasPrefix(why, "teamproj (rule: cwd-file)") {
		t.Errorf("pinned missing, then why: %s", why)
	}

	// A second clone of a repo another project claims by remote, given its
	// own --slug: the new project gets the clone's path only; the remote
	// stays with the original, so a third clone still lands there.
	clone := func(name string) string {
		return gitRepo(t, filepath.Join(z.home, "clones", name), "git@github.com:acme/app.git")
	}
	if out, _, code = z.run(clone("app"), nil, "init"); code != 0 || !strings.Contains(out, "created project app (slug from the repo name; path "+filepath.Join(z.home, "clones", "app")+", remote github.com/acme/app)") {
		t.Fatalf("first clone: exit %d, out %q", code, out)
	}
	app2 := clone("app2")
	if why, _, _ := z.run(app2, nil, "project", "why"); !strings.HasPrefix(why, "app (rule: cwd-remote)") {
		t.Fatalf("second clone before init: %s", why)
	}
	out, stderr, code = z.run(app2, nil, "init", "--slug", "app2")
	if code != 0 || !strings.Contains(out, "created project app2 (slug from --slug; path "+app2+"; remote github.com/acme/app stays with project app)") ||
		strings.Contains(out, ", remote") || strings.Contains(out, "note:") {
		t.Errorf("second clone --slug: exit %d, stderr %q, out %q", code, stderr, out)
	}
	if why, _, _ := z.run(app2, nil, "project", "why"); !strings.HasPrefix(why, "app2 (rule: cwd-path)") {
		t.Errorf("second clone after init: %s", why)
	}
	if why, _, _ := z.run(clone("app3"), nil, "project", "why"); !strings.HasPrefix(why, "app (rule: cwd-remote)") {
		t.Errorf("a third clone no longer resolves to the original project: %s", why)
	}

	// Concurrent first commands after a stop start exactly one daemon.
	z.stopDaemon()
	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, stderr, code := z.run(repo, nil, "work", "ls")
			mu.Lock()
			defer mu.Unlock()
			started += strings.Count(stderr, "started the daemon")
			if code != 0 {
				t.Errorf("concurrent work ls: exit %d, %q", code, stderr)
			}
		}()
	}
	wg.Wait()
	if started != 1 {
		t.Errorf("4 concurrent commands said %d times that they started the daemon, want 1", started)
	}
	if z.daemonPID() == 0 {
		t.Error("no daemon after the concurrent start")
	}
}

// TestInitBroadRoots: a git repo at $HOME (dotfiles) is not a project root:
// init there is refused, a plain directory under it gets a path rule only
// (never the dotfiles remote), and --pin writes into that directory, never
// a file at $HOME that every directory under it would find. A stranger whose
// first command is work new is told to run zm init.
func TestInitBroadRoots(t *testing.T) {
	z := newZeroEnv(t)
	gitRepo(t, z.home, "git@github.com:ana/dotfiles.git")
	notes := filepath.Join(z.home, "notes")
	other := filepath.Join(z.home, "other")
	for _, d := range []string{notes, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, stderr, code := z.run(other, nil, "work", "new", "x")
	if code != 1 || !strings.HasPrefix(stderr, startedLine) || !strings.Contains(stderr, "no project for this call") || !strings.Contains(stderr, "zm init") {
		t.Errorf("work new before init: exit %d, stderr %q (want the init hint)", code, stderr)
	}
	if _, stderr, code = z.run(z.home, nil, "init"); code != 1 || !strings.Contains(stderr, "refusing to make "+z.home) {
		t.Errorf("init in a git $HOME: exit %d, %q", code, stderr)
	}
	out, stderr, code := z.run(notes, nil, "init", "--pin")
	if code != 0 || !strings.Contains(out, "created project notes (slug from the directory name; path "+notes+")") || strings.Contains(out, "remote") {
		t.Errorf("init under a git $HOME: exit %d, stderr %q, out %q", code, stderr, out)
	}
	if _, err := os.Stat(filepath.Join(notes, ".zm", "project.toml")); err != nil {
		t.Errorf("--pin did not write into %s: %v", notes, err)
	}
	if _, err := os.Stat(filepath.Join(z.home, ".zm", "project.toml")); err == nil {
		t.Error("--pin wrote ~/.zm/project.toml, which every directory under $HOME finds")
	}
	if why, _, code := z.run(other, nil, "project", "why"); code != 1 || strings.HasPrefix(why, "notes") {
		t.Errorf("a sibling of the project resolves to it: exit %d, %s", code, why)
	}
}

// TestDaemonEnv: a daemon never inherits the starter's identity; an
// autostarted one also drops the shell's bd and Dolt settings (a live
// BEADS_DOLT_SERVER_HOST must not reach bd through a daemon nobody started on
// purpose), while daemon --detach keeps them for existing bindings.
func TestDaemonEnv(t *testing.T) {
	in := []string{"PATH=/bin", "ZM_HOME=/old", "ZM_AS=agent:x", "CLAUDECODE=1", "ZM_MACHINE=m",
		"CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_MESSAGING_TOKEN=t", "CMUX_SURFACE_ID=s", "CLAUDE_CONFIG_DIR=/c",
		"BEADS_DOLT_SERVER_HOST=leakhost", "BEADS_DIR=/x/.beads", "BD_ACTOR=a", "DOLT_ROOT_PATH=/d"}
	has := func(env []string, k string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				return true
			}
		}
		return false
	}
	for _, auto := range []bool{true, false} {
		env := daemonEnv(in, "/new", auto)
		if !has(env, "PATH") || !has(env, "ZM_MACHINE") || env[len(env)-1] != "ZM_HOME=/new" || has(env, "ZM_AS") || has(env, "CLAUDECODE") ||
			has(env, "CLAUDE_CODE_CHILD_SESSION") || has(env, "CLAUDE_CODE_MESSAGING_TOKEN") || has(env, "CMUX_SURFACE_ID") ||
			!has(env, "CLAUDE_CONFIG_DIR") {
			t.Errorf("auto=%v: %q", auto, env)
		}
		for _, k := range []string{"BEADS_DOLT_SERVER_HOST", "BEADS_DIR", "BD_ACTOR", "DOLT_ROOT_PATH"} {
			if has(env, k) == auto {
				t.Errorf("auto=%v: %s kept=%v", auto, k, has(env, k))
			}
		}
	}
}
