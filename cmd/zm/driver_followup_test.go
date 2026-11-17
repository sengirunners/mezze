package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/runtarget"
)

// A driver changed by hand (refused) stays refused through an upgrade
// that rolls back: the swap trusts back only a file that matched its pin.
func TestUpgradeRollbackKeepsAHandChangedDriverRefused(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	installDriver(t, r.inst, "zm-run-ref", r.script+"# changed by hand\n")
	if err := r.acquire(); err == nil || !strings.Contains(err.Error(), "zm driver pin ref") {
		t.Fatalf("a hand-changed driver: %v", err)
	}
	bad := filepath.Join(r.home, "bad")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"daemon preflight\" ]; then ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"; fi\n" +
		"if [ \"$1\" = daemon ]; then echo 'zm: boom: cannot serve' >&2; exit 1; fi\n" +
		"ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"\n"
	installDriver(t, bad, "zm", script)
	installDriver(t, bad, "zm-run-ref", r.script+"# build 2, rolled back\n")
	out, e, code := r.runBin(r.zm, "daemon", "upgrade", filepath.Join(bad, "zm"))
	if code != 1 || !strings.Contains(e, "rolled back to the previous binary") {
		t.Fatalf("bad upgrade: %d\n%s\n%s", code, out, e)
	}
	if err := r.acquire(); err == nil || !strings.Contains(err.Error(), "zm driver pin ref") {
		t.Fatalf("the hand-changed driver after the rollback: %v\n%s\n%s", err, out, e)
	}
}

// The plan keeps a replaced file's sum only when that file is the one
// pinned: not a hand-changed one, not an unpinned one.
func TestPlanPinsTrustsBackOnlyThePinnedFile(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	dir, stage := filepath.Join(base, "inst"), filepath.Join(base, "stage")
	installDriver(t, dir, "zm-run-x", "#!/bin/sh\nexit 1\n")
	installDriver(t, dir, "zm-run-y", "#!/bin/sh\nexit 1\n") // never pinned
	installDriver(t, stage, "zm-run-x", "#!/bin/sh\nexit 2\n")
	installDriver(t, stage, "zm-run-y", "#!/bin/sh\nexit 2\n")
	if _, err := runtarget.PinDriver(home, []string{dir}, "x"); err != nil {
		t.Fatal(err)
	}
	names := []string{"zm-run-x", "zm-run-y"}
	if p, err := planPins(home, stage, dir, "", names); err != nil || len(p.Prev) != 1 || p.Prev["zm-run-x"] == "" {
		t.Fatalf("pinned x: %+v %v", p, err)
	}
	installDriver(t, dir, "zm-run-x", "#!/bin/sh\nexit 9\n") // changed by hand
	if p, err := planPins(home, stage, dir, "", names); err != nil || len(p.Prev) != 0 {
		t.Fatalf("hand-changed x: %+v %v", p, err)
	}
}

// stubDriverProbe makes the verify's driver probe answer from answers in
// turn (the last again), with a short grace.
func stubDriverProbe(t *testing.T, answers ...wiring.DriverCheck) *int {
	t.Helper()
	oldP, oldD, oldG, oldR := probe, driverProbe, swapRemoteGrace, swapRemoteRetry
	t.Cleanup(func() { probe, driverProbe, swapRemoteGrace, swapRemoteRetry = oldP, oldD, oldG, oldR })
	swapRemoteGrace, swapRemoteRetry = 600*time.Millisecond, 20*time.Millisecond
	probe = func(context.Context, *client.Client) api.ServeCheck { return api.ServeCheck{} }
	n := new(int)
	driverProbe = func(context.Context, *client.Client) wiring.DriverCheck {
		a := answers[min(*n, len(answers)-1)]
		*n++
		return a
	}
	return n
}

func driverItem(name, fail string) api.ServeItem {
	return api.ServeItem{Kind: wiring.ItemDriver, Name: name, OK: fail == "", Error: fail}
}

// The verify's driver regressions wait out the same grace as a remote
// item: a driver that fails, then answers, rolls nothing back; one that
// keeps failing does, once the grace has passed; a binding whose listing
// failed is unknown, said and never counted.
func TestVerifyDriverGraceAndUnknown(t *testing.T) {
	before := []api.ServeItem{driverItem("acme/box", ""), driverItem("machine/h-c1m2", "")}
	ok := wiring.DriverCheck{Items: before}
	failing := wiring.DriverCheck{Items: []api.ServeItem{driverItem("acme/box", "driver busy"), driverItem("machine/h-c1m2", "")}}
	s := &swapper{ctx: context.Background(), driversBefore: before}

	n := stubDriverProbe(t, failing, failing, ok)
	if why := s.serves(api.ServeCheck{}); why != "" || *n != 3 {
		t.Fatalf("a flake: %q after %d probes", why, *n)
	}
	stubDriverProbe(t, failing)
	start := time.Now()
	if why := s.serves(api.ServeCheck{}); !strings.Contains(why, "driver acme/box: served before, now driver busy") || time.Since(start) < swapRemoteGrace {
		t.Fatalf("a lasting failure: %q after %s", why, time.Since(start))
	}
	unknown := wiring.DriverCheck{Items: []api.ServeItem{driverItem("machine/h-c1m2", "")}, AllProjects: "projects: timeout"}
	stubDriverProbe(t, unknown)
	if why := s.serves(api.ServeCheck{}); why != "" {
		t.Fatalf("an unknown binding counted: %q", why)
	}
	noMachine := wiring.DriverCheck{Items: []api.ServeItem{driverItem("acme/box", "")}, Unknown: map[string]string{"machine": "forbidden"}}
	if regs, unk := wiring.DriverRegressions(before, noMachine); len(regs) != 0 || len(unk) != 1 || !strings.Contains(unk[0], "machine/h-c1m2") {
		t.Fatalf("machine scope unknown: %v %v", regs, unk)
	}
	gone := wiring.DriverCheck{Items: []api.ServeItem{driverItem("machine/h-c1m2", "")}}
	if regs, _ := wiring.DriverRegressions(before, gone); len(regs) != 1 || !strings.Contains(regs[0], "missing now") {
		t.Fatalf("a binding gone from a listed scope: %v", regs)
	}
}

// A plan file that does not parse is moved aside, said once, and the swap
// goes on.
func TestCorruptPinsPlanMovedAside(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := os.WriteFile(filepath.Join(home, fileSwapPins), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := loadSwapPins(home); p != nil || err == nil || !strings.Contains(err.Error(), ".corrupt") {
		t.Fatalf("corrupt: %v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(home, fileSwapPins+".corrupt")); err != nil {
		t.Fatal("not moved aside")
	}
	if p, err := loadSwapPins(home); p != nil || err != nil {
		t.Fatalf("after: %v %v", p, err)
	}
}

// pinTo for a moved folder: the file changes between its check and the
// pin, so PinDriver pins the wrong bytes; the previous pin is put back.
func TestPinToRaceRestoresThePreviousPin(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	oldDir, newDir := filepath.Join(base, "lib"), filepath.Join(base, "bin")
	old := installDriver(t, oldDir, "zm-run-x", "#!/bin/sh\nexit 1\n")
	placed := installDriver(t, newDir, "zm-run-x", "#!/bin/sh\nexit 2\n")
	before, err := runtarget.PinDriver(home, []string{oldDir}, "x")
	if err != nil {
		t.Fatal(err)
	}
	orig := pinDriver
	t.Cleanup(func() { pinDriver = orig })
	pinDriver = func(home string, dirs []string, d string) (runtarget.DriverPin, error) {
		if dirs[0] == newDir {
			installDriver(t, newDir, "zm-run-x", "#!/bin/sh\nexit 3\n") // swapped under it
		}
		return orig(home, dirs, d)
	}
	err = pinTo(home, newDir, "x", map[string]string{"zm-run-x": sumOf(t, placed)})
	if err == nil || !strings.Contains(err.Error(), "previous pin") {
		t.Fatalf("pinTo: %v", err)
	}
	pins, _, _ := readPins(home)
	if pins["x"].Path != old || pins["x"].SHA256 != before.SHA256 {
		t.Fatalf("the previous pin is not back: %+v", pins["x"])
	}
}

// A stand-in an earlier check left behind (killed midway) is removed.
func TestDriverCheckRemovesALeftoverStandIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	left := installDriver(t, dir, "potluck-driver-"+dirCheckPrefix+"abc", "#!/bin/sh\n")
	if err := checkDriverDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover stand-in stays: %v", err)
	}
}

// The driver folder's hold: set once (never over someone's own hold),
// lifted after autoHoldRetry, for a newer release, or by an upgrade that
// lands; a hold set by hand is never lifted.
func TestDriverFolderHold(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	none := func() string { return "" }
	holdForDrivers(home, "main-aaaaaaaaaaaa (aaaa)", "drivers in /x would be refused")
	if ownAutoHold(home) == nil {
		t.Fatal("no hold")
	}
	if why := expireAutoHold(home, time.Now(), none); why != "" {
		t.Fatalf("lifted at once: %s", why)
	}
	if why := expireAutoHold(home, time.Now(), func() string { return "main-aaaaaaaaaaaa" }); why != "" {
		t.Fatalf("lifted for the same release: %s", why)
	}
	if why := expireAutoHold(home, time.Now().Add(7*time.Hour), none); why == "" || ownAutoHold(home) != nil {
		t.Fatal("not lifted after 6h")
	}
	if _, held, _ := release.ReadHold(release.HoldPath(home)); held {
		t.Fatal("the hold file stays")
	}
	holdForDrivers(home, "main-aaaaaaaaaaaa (aaaa)", "x")
	if why := expireAutoHold(home, time.Now(), func() string { return "main-bbbbbbbbbbbb" }); why == "" {
		t.Fatal("not lifted for a newer release")
	}
	holdForDrivers(home, "b", "x")
	if !liftAutoHold(home) || ownAutoHold(home) != nil {
		t.Fatal("an upgrade that landed did not lift it")
	}
	// Someone's own hold: never replaced, never lifted.
	if err := release.WriteHold(release.HoldPath(home), release.Hold{Reason: "by hand", Since: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	holdForDrivers(home, "c", "x")
	if h, _, _ := release.ReadHold(release.HoldPath(home)); h.Reason != "by hand" {
		t.Fatalf("a hand hold was replaced: %+v", h)
	}
	if expireAutoHold(home, time.Now().Add(100*time.Hour), none) != "" || liftAutoHold(home) {
		t.Fatal("a hand hold was lifted")
	}
	if _, held, _ := release.ReadHold(release.HoldPath(home)); !held {
		t.Fatal("the hand hold is gone")
	}
}

// Potluck's refusal text, which isFolderRefusal matches (Potluck exports
// no typed error): a change there must change the helper.
func TestFolderRefusalText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	installDriver(t, dir, "potluck-driver-x", "#!/bin/sh\n")
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	_, err = runtarget.PinDriver(home, []string{dir}, "x")
	if !isFolderRefusal(err) || !strings.Contains(err.Error(), dir+" is writable by group or others (fix: chmod go-w "+dir+")") {
		t.Fatalf("Potluck's refusal changed: %v", err)
	}
	if _, err := runtarget.PinDriver(home, []string{dir}, "missing"); isFolderRefusal(err) {
		t.Fatalf("a missing driver read as a folder refusal: %v", err)
	}
	if isFolderRefusal(errors.New("write /x: no space left on device")) || isFolderRefusal(nil) {
		t.Fatal("a passing failure read as a refusal")
	}
}

// A failure that is not Potluck refusing the folder (it cannot be made, or
// the stand-in cannot be written: a full disk, no write access) refuses
// this attempt but holds nothing: the next tick tries again. Potluck's
// refusal holds.
func TestDriverFolderHoldsOnlyOnARefusal(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	file := installDriver(t, base, "afile", "")
	var out bytes.Buffer
	why, _, refused := driverFolderProblem(filepath.Join(file, "bin"), base, &out) // under a file: cannot be made
	if why == "" || refused || !strings.Contains(why, "not upgrading this time") {
		t.Fatalf("cannot make it: %q refused=%v", why, refused)
	}
	stage := filepath.Join(base, "stage")
	installDriver(t, stage, "zm", "#!/bin/sh\n")
	installDriver(t, stage, "zm-run-x", "#!/bin/sh\n")
	upgrade := func(dir string) (*swapper, string) {
		home, err := os.MkdirTemp("", "zm")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(home) })
		s := &swapper{cfg: &config.Config{Home: home}, o: swapOpts{what: "upgrade", dir: dir}, pf: api.Preflight{OK: true, Build: "b"}}
		s.checkDriverFolder(filepath.Join(stage, "zm"))
		return s, home
	}
	ro := filepath.Join(base, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	if os.Geteuid() != 0 { // root writes a 0555 folder
		s, home := upgrade(ro) // the stand-in cannot be written
		if s.pf.OK || len(s.pf.Problems) != 1 {
			t.Fatalf("a passing failure did not refuse the attempt: %+v", s.pf)
		}
		if _, held, _ := release.ReadHold(release.HoldPath(home)); held {
			t.Fatal("held for a passing failure")
		}
	}
	gw := filepath.Join(base, "gw")
	if err := os.MkdirAll(filepath.Join(gw, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gw, 0o775); err != nil { // above HOME (base is not home): never repaired
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(gw, "bin"))
	s, home := upgrade(filepath.Join(gw, "bin"))
	if _, held, _ := release.ReadHold(release.HoldPath(home)); s.pf.OK || !held {
		t.Fatalf("Potluck's refusal did not hold: %+v held=%v", s.pf, held)
	}
}

// pinTo's restore checks the previous file too: one that changed as well
// is not pinned back, and the error says what stays pinned.
func TestPinToRaceWithTheOldFileChangedToo(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	oldDir, newDir := filepath.Join(base, "lib"), filepath.Join(base, "bin")
	installDriver(t, oldDir, "zm-run-x", "#!/bin/sh\nexit 1\n")
	placed := installDriver(t, newDir, "zm-run-x", "#!/bin/sh\nexit 2\n")
	if _, err := runtarget.PinDriver(home, []string{oldDir}, "x"); err != nil {
		t.Fatal(err)
	}
	orig := pinDriver
	t.Cleanup(func() { pinDriver = orig })
	calls := 0
	pinDriver = func(home string, dirs []string, d string) (runtarget.DriverPin, error) {
		calls++
		installDriver(t, newDir, "zm-run-x", "#!/bin/sh\nexit 3\n")
		installDriver(t, oldDir, "zm-run-x", "#!/bin/sh\nexit 4\n")
		return orig(home, dirs, d)
	}
	err = pinTo(home, newDir, "x", map[string]string{"zm-run-x": sumOf(t, placed)})
	if err == nil || !strings.Contains(err.Error(), "changed too") || calls != 1 {
		t.Fatalf("pinTo: %v (PinDriver called %d times)", err, calls)
	}
}
