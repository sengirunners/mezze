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
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runtarget"
)

// installDriver writes an executable name with script in dir, both
// writable by their owner alone (whatever the umask), as the daemon runs a
// driver only then (DRIVERS.md), and answers its path.
func installDriver(t *testing.T, dir, name, script string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.Chmod(dir, 0o700), os.Chmod(p, 0o755)); err != nil {
		t.Fatal(err)
	}
	return p
}

// Drivers are pinned: the daemon finds the reference
// driver in $ZM_HOME/drivers (no SetLookPath) and pins it at bind. A
// driver changed by hand is refused, naming zm driver pin, which trusts it
// again. An upgrade that places a new driver re-pins it (it stays usable);
// one changed after it was placed stays refused.
func TestDriverPinsAndUpgradeRepins(t *testing.T) {
	first, every := wiring.DriverWatchFirst, wiring.DriverWatchEvery
	wiring.DriverWatchFirst, wiring.DriverWatchEvery = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { wiring.DriverWatchFirst, wiring.DriverWatchEvery = first, every }) // after the daemon stops
	dm, c := poolDaemon(t, tailnettest.New(), "mini", "owner@example.com")
	home := dm.Deps.Config.Home
	drivers := filepath.Join(home, runtarget.DriversDir)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	scratch := t.TempDir()
	tmpl, err := refdriver.Install(scratch, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	orig, err := os.ReadFile(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := filepath.EvalSymlinks(installDriver(t, drivers, "zm-run-ref", string(orig))) // the path pinned
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.Project, c.ProjectFrom = "acme", "flag"
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, c, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{boxPin}}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	acquire := func() error {
		n++
		h, err := computeclient.AcquireRunTarget(ctx, c, "box", computeapi.AcquireRunTargetRequest{RequestID: "p" + string(rune('a'+n)), TTLS: 120})
		if err != nil {
			return err
		}
		target, id, err := computeclient.SplitBoxHandle(h.Handle)
		if err != nil {
			t.Fatal(err)
		}
		_, err = computeclient.ReleaseRunTarget(ctx, c, target, id)
		return err
	}
	if err := acquire(); err != nil {
		t.Fatalf("pinned at bind: %v", err)
	}
	refused := func(when string) {
		t.Helper()
		if err := acquire(); err == nil || !strings.Contains(err.Error(), "zm driver pin ref") {
			t.Fatalf("%s: %v, want a refusal naming zm driver pin ref", when, err)
		}
	}

	// Changed by hand: refused until the owner pins it again.
	installDriver(t, drivers, "zm-run-ref", string(orig)+"# changed by hand\n")
	refused("changed by hand")
	// Health and zm status warn, with the reason and the fix; the
	// self-check stays clean and zm status exits 0 (a binding may stay
	// broken until its owner fixes it).
	var notice *wiring.DriverNotice
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if notice = healthDrivers(ctx, c); notice != nil {
			break
		}
	}
	if notice == nil || notice.Refused != 1 || !strings.Contains(notice.Problems[0], "driver acme/box: ") || !strings.Contains(notice.Problems[0], "zm driver pin ref") {
		t.Fatalf("health's drivers: %+v", notice)
	}
	if sc := dm.SelfCheck(ctx); !sc.OK {
		t.Fatalf("a refused driver failed the self-check: %+v", sc)
	}
	if st, err := zmCmd(home, t.TempDir(), "status").CombinedOutput(); err != nil || !strings.Contains(string(st), "warning: 1 run target(s) whose driver this daemon cannot run") {
		t.Fatalf("zm status: %v\n%s", err, st)
	}
	out, err := zmCmd(home, t.TempDir(), "driver", "pin", "ref").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "pinned ref: "+drv+" sha256 ") {
		t.Fatalf("zm driver pin ref: %v\n%s", err, out)
	}
	if err := acquire(); err != nil {
		t.Fatalf("after zm driver pin: %v", err)
	}
	for end := time.Now().Add(10 * time.Second); healthDrivers(ctx, c) != nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the warning stays after the pin: %+v", healthDrivers(ctx, c))
		}
	}

	// An upgrade places a new build of it: re-pinned at once (the new daemon
	// runs it during its verify), usable.
	stage := t.TempDir()
	installDriver(t, stage, "zm-run-ref", string(orig)+"# build 2\n")
	installDriver(t, stage, "zm-run-other", "#!/bin/sh\nexit 1\n") // placed, never pinned: left alone
	names := []string{"zm-run-ref", "zm-run-other"}
	plan, err := planPins(home, stage, drivers, "", names)
	if err != nil || plan == nil {
		t.Fatalf("plan: %v", err)
	}
	placed, err := swap.Place(stage, drivers, names)
	if err != nil {
		t.Fatal(err)
	}
	refused("placed, before the re-pin")
	if done, err := plan.apply(true); err != nil || strings.Join(done, ",") != "ref" {
		t.Fatalf("forward: %v %v", done, err)
	}
	if err := acquire(); err != nil {
		t.Fatalf("after the upgrade's re-pin: %v", err)
	}
	// Rolled back: the files come back, and so do their pins.
	if err := placed.Restore(); err != nil {
		t.Fatal(err)
	}
	refused("restored, before the re-pin")
	if done, err := plan.apply(false); err != nil || strings.Join(done, ",") != "ref" {
		t.Fatalf("backward: %v %v", done, err)
	}
	if err := acquire(); err != nil {
		t.Fatalf("after the rollback's re-pin: %v", err)
	}

	// A swap that ended no one knows how (a failed restore, a swapper
	// killed midway): settle pins each driver to whichever file is there.
	if placed, err = swap.Place(stage, drivers, names); err != nil {
		t.Fatal(err)
	}
	if left := plan.settle(""); len(left) != 0 {
		t.Fatalf("settle on the placed files: %v", left)
	}
	if err := acquire(); err != nil {
		t.Fatalf("settled on the placed files: %v", err)
	}
	if err := placed.Restore(); err != nil {
		t.Fatal(err)
	}
	if left := plan.settle(""); len(left) != 0 {
		t.Fatalf("settle on the restored files: %v", left)
	}
	if err := acquire(); err != nil {
		t.Fatalf("settled on the restored files: %v", err)
	}

	// Placed, then changed by something else before the re-pin: refused,
	// and settle says so.
	installDriver(t, stage, "zm-run-ref", string(orig)+"# build 3\n")
	if plan, err = planPins(home, stage, drivers, "", []string{"zm-run-ref"}); err != nil {
		t.Fatal(err)
	}
	if _, err = swap.Place(stage, drivers, []string{"zm-run-ref"}); err != nil {
		t.Fatal(err)
	}
	installDriver(t, drivers, "zm-run-ref", string(orig)+"# build 3, then tampered with\n")
	if done, err := plan.apply(true); err == nil || len(done) != 0 || !strings.Contains(err.Error(), "changed after it was placed") {
		t.Fatalf("tampered after placing: %v %v", done, err)
	}
	if left := plan.settle(""); len(left) != 1 || !strings.Contains(left[0], "zm driver pin ref") {
		t.Fatalf("settle names the tampered driver: %v", left)
	}
	refused("tampered after placing")
}

// A home without a pins file has nothing to re-pin, and an upgrade writes
// none there: its drivers are pinned on first use.
func TestRepinWithoutPinsWritesNothing(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	stage, dir := t.TempDir(), t.TempDir()
	installDriver(t, stage, "zm-run-ref", "#!/bin/sh\nexit 1\n")
	plan, err := planPins(home, stage, dir, "", []string{"zm-run-ref"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := swap.Place(stage, dir, []string{"zm-run-ref"}); err != nil {
		t.Fatal(err)
	}
	if done, err := plan.apply(true); err != nil || len(done) != 0 {
		t.Fatalf("no pins: %v %v", done, err)
	}
	if left := plan.settle(""); len(left) != 0 {
		t.Fatalf("no pins: %v", left)
	}
	if _, err := os.Stat(filepath.Join(home, runtarget.PinFile)); !os.IsNotExist(err) {
		t.Fatalf("a pins file was written: %v", err)
	}
}

// A --dir upgrade that moves the daemon's folder (a symlinked zm replaced
// by a real file elsewhere): a driver pinned in the old folder follows to
// the placed file, and back on a rollback.
func TestRepinFollowsAMovedFolder(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	oldDir, newDir, stage := filepath.Join(base, "lib"), filepath.Join(base, "bin"), filepath.Join(base, "stage")
	old := installDriver(t, oldDir, "zm-run-x", "#!/bin/sh\nexit 1\n")
	installDriver(t, newDir, "README", "")
	installDriver(t, stage, "zm-run-x", "#!/bin/sh\nexit 2\n")
	if _, err := runtarget.PinDriver(home, []string{oldDir}, "x"); err != nil {
		t.Fatal(err)
	}
	plan, err := planPins(home, stage, newDir, oldDir, []string{"zm-run-x"})
	if err != nil || plan.OldDir != oldDir || plan.Moved["x"].Path != old {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	placed, err := swap.Place(stage, newDir, []string{"zm-run-x"})
	if err != nil {
		t.Fatal(err)
	}
	pinOf := func() runtarget.DriverPin {
		pins, _, err := readPins(home)
		if err != nil {
			t.Fatal(err)
		}
		return pins["x"]
	}
	if done, err := plan.apply(true); err != nil || strings.Join(done, ",") != "x" || pinOf().Path != filepath.Join(newDir, "zm-run-x") {
		t.Fatalf("forward: %v %v %+v", done, err, pinOf())
	}
	if err := placed.Restore(); err != nil {
		t.Fatal(err)
	}
	if done, err := plan.apply(false); err != nil || strings.Join(done, ",") != "x" || pinOf().Path != old {
		t.Fatalf("backward: %v %v %+v", done, err, pinOf())
	}
	if left := plan.settle(oldDir); len(left) != 0 || pinOf().Path != old {
		t.Fatalf("settle with the daemon in the old folder: %v %+v", left, pinOf())
	}
}

// The upgrade's driver folder check is Potluck's rule: a folder only this
// user can write passes; one others can write is repaired when this user
// owns it and it is inside home, never above home; what stays unsafe is a
// problem naming the folder and the fix.
func TestDriverFolderCheck(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	gw, home := filepath.Join(base, "gw"), filepath.Join(base, "gw", "home")
	inst := filepath.Join(home, "bin")
	if err := os.MkdirAll(inst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkDriverDir(inst); err != nil {
		t.Fatalf("a private folder: %v", err)
	}
	if left, _ := os.ReadDir(inst); len(left) != 0 {
		t.Fatalf("the check left %v", left)
	}
	var out bytes.Buffer
	for _, d := range []string{inst, home} {
		if err := os.Chmod(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkDriverDir(inst); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("group-writable: %v", err)
	}
	if why, _, _ := driverFolderProblem(inst, home, &out); why != "" {
		t.Fatalf("repairable: %s", why)
	}
	for _, d := range []string{inst, home} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o755 {
			t.Errorf("%s not repaired: %v", d, fi.Mode())
		}
	}
	if !strings.Contains(out.String(), "chmod go-w") {
		t.Errorf("the repair was not said: %q", out.String())
	}
	// Above home: never touched, so refused.
	if err := os.Chmod(gw, 0o775); err != nil {
		t.Fatal(err)
	}
	why, detail, refused := driverFolderProblem(inst, home, &out)
	if !strings.Contains(why, gw+" is writable by group or others") || !strings.Contains(why, "keeps its build") || !strings.Contains(detail, gw) || !refused {
		t.Fatalf("above home: %q", why)
	}
	if fi, _ := os.Stat(gw); fi.Mode().Perm() != 0o775 {
		t.Errorf("a folder above home was changed: %v", fi.Mode())
	}
	if !placedDrivers([]string{"zm", "zm-run-tart"}) || placedDrivers([]string{"zm", "bd-zm", "potluck-conformance"}) {
		t.Fatal("placedDrivers")
	}
}
