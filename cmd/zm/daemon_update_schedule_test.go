package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
)

// fakeSchedule is an updateSchedule over a temp home whose systemctl and
// launchctl calls are recorded, never run.
func fakeSchedule(t *testing.T, goos string) (*updateSchedule, *[]string) {
	t.Helper()
	var calls []string
	return &updateSchedule{goos: goos, userHome: t.TempDir(), uid: 501, run: func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}}, &calls
}

func testSpec(t *testing.T) scheduleSpec {
	dir := t.TempDir()
	return scheduleSpec{Exe: filepath.Join(dir, "zm"), Dir: dir, Home: filepath.Join(t.TempDir(), "zm home"), Path: dir + ":/usr/bin:/bin", MinAge: defaultUpdateMinAge, Minute: 17}
}

// The daily job runs the farm's update: signed releases only, at the farm's
// ring age, with the hold switch of $ZM_HOME.
func TestScheduleRunsTheSignedReleaseUpdate(t *testing.T) {
	sp := testSpec(t)
	got := sp.args()
	want := []string{sp.Exe, "daemon", "update", "--release", "github", "--dir", sp.Dir, "--min-age", "6h0m0s", "--no-canary"}
	if !slices.Equal(got, want) {
		t.Fatalf("args %q, want %q", got, want)
	}
	for _, bad := range []string{"--force", "--end-sessions", "--repo", "--key"} {
		if slices.Contains(got, bad) {
			t.Errorf("the scheduled update passes %s", bad)
		}
	}
}

func TestScheduleLinuxIdempotentNoRoot(t *testing.T) {
	s, calls := fakeSchedule(t, "linux")
	sp := testSpec(t)
	for i := range 2 {
		r, err := s.install(sp)
		if err != nil || r.foreign != "" {
			t.Fatalf("install %d: %+v %v", i, r, err)
		}
		if r.replaced != (i == 1) {
			t.Errorf("install %d replaced=%v", i, r.replaced)
		}
	}
	units := filepath.Join(s.userHome, ".config", "systemd", "user")
	names, _ := filepath.Glob(filepath.Join(units, "*"))
	if len(names) != 2 {
		t.Fatalf("unit files after two installs: %v", names)
	}
	svc, _ := os.ReadFile(filepath.Join(units, "zm-update.service"))
	tmr, _ := os.ReadFile(filepath.Join(units, "zm-update.timer"))
	for _, w := range []string{"KillMode=process", "Type=oneshot", "--release github", "--no-canary", `Environment="ZM_HOME=` + sp.Home + `"`, "TimeoutStartSec=35min"} {
		if !strings.Contains(string(svc), w) {
			t.Errorf("service lacks %q:\n%s", w, svc)
		}
	}
	if strings.Contains(string(svc), "ZM_NO_AUTOSTART") {
		t.Errorf("the service passes ZM_NO_AUTOSTART on to the daemon it starts:\n%s", svc)
	}
	for _, w := range []string{"OnCalendar=*-*-* 03:17:00", "Persistent=true", "WantedBy=timers.target"} {
		if !strings.Contains(string(tmr), w) {
			t.Errorf("timer lacks %q:\n%s", w, tmr)
		}
	}
	for _, c := range *calls {
		if !strings.HasPrefix(c, "systemctl --user ") {
			t.Errorf("ran %q: only systemctl --user, never root", c)
		}
	}
	if !slices.Contains(*calls, "systemctl --user enable zm-update.timer") {
		t.Errorf("calls %q", *calls)
	}

	*calls = nil
	removed, foreign, err := s.remove()
	if err != nil || !removed || foreign != "" {
		t.Fatalf("remove: %v %q %v", removed, foreign, err)
	}
	if names, _ := filepath.Glob(filepath.Join(units, "*")); len(names) != 0 {
		t.Errorf("left %v", names)
	}
	if !slices.Contains(*calls, "systemctl --user disable --now zm-update.timer") {
		t.Errorf("remove ran %q", *calls)
	}
	if removed, _, err := s.remove(); removed || err != nil {
		t.Errorf("a second remove: %v %v", removed, err)
	}
}

func TestScheduleMacLaunchAgentIdempotent(t *testing.T) {
	s, calls := fakeSchedule(t, "darwin")
	sp := testSpec(t)
	for range 2 {
		if r, err := s.install(sp); err != nil || r.foreign != "" {
			t.Fatalf("install: %+v %v", r, err)
		}
	}
	agents := filepath.Join(s.userHome, "Library", "LaunchAgents")
	names, _ := filepath.Glob(filepath.Join(agents, "*"))
	if len(names) != 1 || filepath.Base(names[0]) != "dev.zm.update.plist" {
		t.Fatalf("agents: %v", names)
	}
	b, _ := os.ReadFile(names[0])
	for _, w := range []string{"<string>--no-canary</string>", "<key>AbandonProcessGroup</key><true/>", "<key>Hour</key><integer>3</integer>", "<key>Minute</key><integer>17</integer>", "<key>ZM_HOME</key><string>" + sp.Home + "</string>"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("plist lacks %q:\n%s", w, b)
		}
	}
	if strings.Contains(string(b), "RunAtLoad") {
		t.Errorf("the job runs at load:\n%s", b)
	}
	want := []string{"launchctl bootout gui/501/dev.zm.update", "launchctl bootstrap gui/501 " + names[0]}
	if !slices.Equal(*calls, append(slices.Clone(want), want...)) {
		t.Errorf("calls %q", *calls)
	}
	if removed, _, err := s.remove(); !removed || err != nil {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, err := os.Stat(names[0]); !os.IsNotExist(err) {
		t.Errorf("plist left: %v", err)
	}
}

// The farm's updater (a unit its fleet tooling links into the
// user's units) or one an owner wrote by hand stays the only one: zm never
// adds a second, never rewrites or removes it.
func TestScheduleLeavesAnotherUpdaterAlone(t *testing.T) {
	farm := "[Service]\nExecStart=%h/.local/bin/zm daemon update --release github --dir %h/.local/bin --min-age ${ZM_UPDATE_MIN_AGE}\n"
	for _, c := range []struct {
		goos, name, body string
		link             bool
	}{
		{"linux", "zm-update.service", farm, true},
		{"linux", "zm-update.timer", "[Timer]\nOnUnitInactiveSec=30min\n", false},
		{"linux", "pull-zm.service", farm, false},
		{"darwin", "local.zm.update.plist", "<array>\n    <string>/x/zm</string><string>daemon</string>\n    <string>update</string>\n</array>", false},
		{"darwin", "dev.zm.update.plist", "<plist>someone else's</plist>", false},
	} {
		t.Run(c.goos+"/"+c.name, func(t *testing.T) {
			s, calls := fakeSchedule(t, c.goos)
			dir := s.unitDir()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, c.name)
			if c.link {
				src := filepath.Join(t.TempDir(), c.name)
				if err := os.WriteFile(src, []byte(c.body), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(src, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			before, _ := filepath.Glob(filepath.Join(dir, "*"))
			r, err := s.install(testSpec(t))
			if err != nil || r.foreign != path {
				t.Fatalf("install: %+v %v", r, err)
			}
			removed, foreign, err := s.remove()
			if err != nil || removed || foreign != path {
				t.Fatalf("remove: %v %q %v", removed, foreign, err)
			}
			after, _ := filepath.Glob(filepath.Join(dir, "*"))
			if !slices.Equal(before, after) || len(*calls) != 0 {
				t.Errorf("touched another updater: %v -> %v, ran %q", before, after, *calls)
			}
			if b, _ := os.ReadFile(path); string(b) != c.body {
				t.Errorf("rewrote %s", path)
			}
		})
	}
}

// zm's other units (a tracker home's server) are not updaters.
func TestScheduleIgnoresOtherZmUnits(t *testing.T) {
	s, _ := fakeSchedule(t, "linux")
	dir := s.unitDir()
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "dev.zm.dolt.ab12.service"), []byte("ExecStart=/x/zm daemon --tracker\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "zm.service"), []byte("ExecStart=%h/.local/bin/zm daemon\n"), 0o644)
	if r, err := s.install(testSpec(t)); err != nil || r.foreign != "" {
		t.Fatalf("install: %+v %v", r, err)
	}
}

func TestScheduleOtherOSRefusesPlainly(t *testing.T) {
	s, _ := fakeSchedule(t, "freebsd")
	if _, err := s.install(testSpec(t)); err == nil || !strings.Contains(err.Error(), "freebsd") {
		t.Fatalf("%v", err)
	}
}

// --dir defaults to the directory of the running binary, its symlinks
// resolved; a directory this user cannot write is refused in one sentence.
func TestUpdateDirDefaultsToTheRunningBinary(t *testing.T) {
	real := t.TempDir()
	exe := filepath.Join(real, "zm")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	link := filepath.Join(bin, "zm")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	old := executable
	t.Cleanup(func() { executable = old })
	executable = func() (string, error) { return link, nil }
	gotExe, dir, err := runningInstall()
	want, _ := filepath.EvalSymlinks(real)
	if err != nil || dir != want || gotExe != filepath.Join(want, "zm") {
		t.Fatalf("runningInstall: %q %q %v (want dir %q)", gotExe, dir, err, want)
	}
	if err := writableDir(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the write check left %v", entries)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	err = writableDir(ro)
	if err == nil || !strings.Contains(err.Error(), "this user cannot write "+ro) || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("read-only dir: %v", err)
	}
}

// enable pins the channel's key and installs the job once; a farm updater
// makes it change nothing, not even config.json.
func TestEnableAutoUpdatePinsTheKeyOnce(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Path(config.FileConfig), []byte(`{"machine":"mini","future_key":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.LoadFrom(home)
	s, _ := fakeSchedule(t, "linux")
	sp := testSpec(t)
	var out bytes.Buffer
	for range 2 {
		out.Reset()
		if err := enableAutoUpdate(cfg, s, sp, &out); err != nil {
			t.Fatal(err)
		}
	}
	if l := strings.Count(out.String(), "\n"); l != 1 || !strings.Contains(out.String(), "zm daemon update disable") {
		t.Errorf("enable said:\n%s", out.String())
	}
	again, _ := config.LoadFrom(home)
	if !slices.Equal(again.File.UpdateKeys, []string{release.ChannelKey}) {
		t.Errorf("update_keys %q", again.File.UpdateKeys)
	}
	if b, _ := os.ReadFile(cfg.Path(config.FileConfig)); !strings.Contains(string(b), `"future_key"`) {
		t.Errorf("config lost a key:\n%s", b)
	}

	// The farm's box: nothing written, the farm's unit named.
	home2, _ := os.MkdirTemp("", "zm")
	t.Cleanup(func() { os.RemoveAll(home2) })
	cfg2, _ := config.LoadFrom(home2)
	s2, calls := fakeSchedule(t, "linux")
	_ = os.MkdirAll(s2.unitDir(), 0o755)
	farm := filepath.Join(s2.unitDir(), "zm-update.service")
	_ = os.WriteFile(farm, []byte("ExecStart=zm daemon update --release github\n"), 0o644)
	out.Reset()
	if err := enableAutoUpdate(cfg2, s2, sp, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg2.Path(config.FileConfig)); !errors.Is(err, os.ErrNotExist) || len(*calls) != 0 || !strings.Contains(out.String(), farm) {
		t.Errorf("with the farm's updater: config %v, calls %q, said %q", err, *calls, out.String())
	}
}

// The channel key zm pins is the one the farm pins (its fleet
// tooling's setup script) and parses as a key.
func TestChannelKeyParses(t *testing.T) {
	if _, err := release.ParseKey(release.ChannelKey); err != nil {
		t.Fatal(err)
	}
}

// While the repository is private, GitHub answers a stranger's gh with a 404:
// the update says why in a sentence, never the raw HTTP error.
func TestReleaseAccessErrorsArePlain(t *testing.T) {
	for _, c := range []struct {
		in   error
		want string
	}{
		{errors.New("gh: exit status 1: gh: Not Found (HTTP 404)"), "while github.com/example-org/zm-private is private"},
		{errors.New("gh: exit status 1: HTTP 404: Not Found (https://api.github.com/repos/example-org/zm-private/releases?per_page=100)"), "only accounts with read access"},
		{errors.New("gh: exit status 4: To get started with GitHub CLI, please run:  gh auth login"), "run gh auth login"},
		{fmt.Errorf("gh: %w: ", &exec.Error{Name: "gh", Err: exec.ErrNotFound}), "gh is not installed"},
		{errors.New("gh: exit status 1: HTTP 403: API rate limit exceeded for user"), "rate limit"},
	} {
		got := releaseAccess(c.in, "example-org/zm-private")
		if got == nil || !strings.Contains(got.Error(), c.want) || strings.Contains(got.Error(), "HTTP 4") {
			t.Errorf("%v -> %v (want %q)", c.in, got, c.want)
		}
	}
	other := errors.New("gh: exit status 1: connection reset")
	if got := releaseAccess(other, "a/b"); got != other {
		t.Errorf("an unknown error changed: %v", got)
	}
	if releaseAccess(nil, "a/b") != nil {
		t.Error("nil")
	}
}

// The call site: the channel's listing, refused by GitHub, fails the update
// check with the sentence.
func TestGithubSourceSaysWhyGitHubRefused(t *testing.T) {
	g := &githubSource{run: func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: exit status 1: gh: Not Found (HTTP 404)")
	}}
	_, err := g.list()
	if err == nil || !strings.Contains(err.Error(), "is private") || strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("%v", err)
	}
	// Past the listing access is proven: a later 404 (a commit GitHub does
	// not have, a missing asset) keeps its own words.
	if _, err := g.file(release.ManifestFile); err == nil || strings.Contains(err.Error(), "is private") {
		t.Fatalf("%v", err)
	}
}

// disable removes zm's own job even when another updater appeared beside it,
// and leaves that one alone.
func TestScheduleDisableRemovesOnlyItsOwn(t *testing.T) {
	s, _ := fakeSchedule(t, "linux")
	if _, err := s.install(testSpec(t)); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(s.unitDir(), "pull-zm.service")
	_ = os.WriteFile(other, []byte("ExecStart=/x/zm daemon update --repo /src\n"), 0o644)
	removed, foreign, err := s.remove()
	if err != nil || !removed || foreign != other {
		t.Fatalf("remove: %v %q %v", removed, foreign, err)
	}
	names, _ := filepath.Glob(filepath.Join(s.unitDir(), "*"))
	if !slices.Equal(names, []string{other}) {
		t.Errorf("left %v", names)
	}
}

// On Linux without lingering the timer stops at logout: the line says so.
func TestEnableSaysWhenTheTimerNeedsLinger(t *testing.T) {
	home, _ := os.MkdirTemp("", "zm")
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, _ := config.LoadFrom(home)
	s, _ := fakeSchedule(t, "linux")
	s.linger = func() (bool, error) { return false, nil }
	var out bytes.Buffer
	if err := enableAutoUpdate(cfg, s, testSpec(t), &out); err != nil || !strings.Contains(out.String(), "loginctl enable-linger") {
		t.Fatalf("%v %q", err, out.String())
	}
	s.linger = func() (bool, error) { return true, nil }
	out.Reset()
	if err := enableAutoUpdate(cfg, s, testSpec(t), &out); err != nil || strings.Contains(out.String(), "linger") {
		t.Fatalf("%v %q", err, out.String())
	}
}

// A daemon whose executable is a symlink into the install directory runs
// from it: the upgrade restarts it rather than reporting a swap it did not do.
func TestRunsFromResolvesSymlinks(t *testing.T) {
	real, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.WriteFile(filepath.Join(real, "zm"), nil, 0o755)
	link := filepath.Join(t.TempDir(), "zm")
	if err := os.Symlink(filepath.Join(real, "zm"), link); err != nil {
		t.Fatal(err)
	}
	if !runsFrom(link, real) || !runsFrom(filepath.Join(real, "zm"), real) || runsFrom(link, t.TempDir()) {
		t.Fatal("runsFrom")
	}
}

func TestLentNeedsABoundSize(t *testing.T) {
	if lent(hostprobe.ShareResult{Sizes: []hostprobe.SizeResult{{}}}) || !lent(hostprobe.ShareResult{Sizes: []hostprobe.SizeResult{{}, {Bound: true}}}) {
		t.Fatal("lent")
	}
}
