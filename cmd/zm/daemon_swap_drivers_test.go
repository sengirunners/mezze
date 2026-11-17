package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/runtarget"
)

// driverRig is a daemon running from inst/zm (a copy of this test binary)
// with one box binding on the reference driver, which sits in drivers
// (inst, beside zm, as a release installs it; or $ZM_HOME/drivers).
type driverRig struct {
	*zeroEnv
	inst, zm, drivers, state string
	script                   string // the reference driver's text
	c                        *client.Client
	n                        int
}

func newDriverRig(t *testing.T, inst func(home string) string, beside bool) *driverRig {
	t.Helper()
	z := newZeroEnv(t)
	r := &driverRig{zeroEnv: z, inst: inst(z.home)}
	r.zm = filepath.Join(r.inst, "zm")
	copyExe(t, os.Args[0], r.zm)
	r.drivers = r.inst
	if !beside {
		r.drivers = filepath.Join(z.zmHome(), runtarget.DriversDir)
	}
	r.state = filepath.Join(z.home, "refstate")
	scratch := t.TempDir()
	tmpl, err := refdriver.Install(scratch, r.state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(r.state) })
	b, err := os.ReadFile(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	r.script = string(b)
	if err := os.MkdirAll(z.zmHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	installDriver(t, r.drivers, "zm-run-ref", r.script)
	if beside {
		_ = os.Chmod(r.inst, 0o755)
	}
	if _, e, code := z.runBin(r.zm, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	r.c = client.NewUnix(filepath.Join(z.zmHome(), "zm.sock"))
	r.c.Project, r.c.ProjectFrom = "acme", "flag"
	ctx := context.Background()
	if _, err := r.c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, r.c, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{boxPin}}}); err != nil {
		t.Fatal(err)
	}
	return r
}

// acquire takes a box through the daemon (its driver runs) and releases it.
func (r *driverRig) acquire() error {
	r.n++
	ctx := context.Background()
	h, err := computeclient.AcquireRunTarget(ctx, r.c, "box", computeapi.AcquireRunTargetRequest{RequestID: fmt.Sprintf("d%d", r.n), TTLS: 120})
	if err != nil {
		return err
	}
	target, id, err := computeclient.SplitBoxHandle(h.Handle)
	if err != nil {
		return err
	}
	_, err = computeclient.ReleaseRunTarget(ctx, r.c, target, id)
	return err
}

// stage is a staged build: a copy of this binary and, unless driver is
// "", a zm-run-ref with that text.
func (r *driverRig) stage(t *testing.T, driver string) string {
	t.Helper()
	dir, err := os.MkdirTemp(r.home, "stage")
	if err != nil {
		t.Fatal(err)
	}
	copyExe(t, os.Args[0], filepath.Join(dir, "zm"))
	if driver != "" {
		installDriver(t, dir, "zm-run-ref", driver)
	}
	return filepath.Join(dir, "zm")
}

// staged runs the staged build's own swapper, as make install and zm
// daemon update do.
func (r *driverRig) staged(t *testing.T, bin string, extra ...string) (string, int) {
	t.Helper()
	c := exec.Command(bin, "daemon", "upgrade", bin, "--dir", r.inst)
	c.Dir, c.Env = r.home, append(append([]string{}, r.env...), extra...)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (r *driverRig) pin(t *testing.T) runtarget.DriverPin {
	t.Helper()
	pins, _, err := readPins(r.zmHome())
	if err != nil {
		t.Fatal(err)
	}
	return pins["ref"]
}

func sumOf(t *testing.T, p string) string {
	t.Helper()
	s, err := sumFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The first start after upgrading from a build without pins: drivers beside
// zm, no driver_pins.json. The upgrade lands, the restarted daemon runs the
// placed driver (a box through it), and it is pinned there.
func TestUpgradeFromNoPinsRunsDriversBesideZm(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	if err := r.acquire(); err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.Remove(filepath.Join(r.zmHome(), runtarget.PinFile)); err != nil { // as a build without pins left it
		t.Fatal(err)
	}
	pid0 := r.daemonPID()
	out, code := r.staged(t, r.stage(t, r.script+"# build 2\n"))
	if code != 0 || !strings.Contains(out, "zm daemon upgraded") {
		t.Fatalf("upgrade: %d\n%s", code, out)
	}
	if p := r.daemonPID(); p == 0 || p == pid0 {
		t.Fatalf("not swapped: %d -> %d", pid0, p)
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("a driver call through the new daemon: %v\n%s", err, out)
	}
	drv := filepath.Join(r.inst, "zm-run-ref")
	if p := r.pin(t); resolvedDir(p.Path) != resolvedDir(drv) || p.SHA256 != sumOf(t, drv) {
		t.Fatalf("pinned %+v, want %s", p, drv)
	}
	if _, err := os.Stat(filepath.Join(r.zmHome(), fileSwapPins)); !os.IsNotExist(err) {
		t.Fatalf("the swap left its pins plan: %v", err)
	}
}

// The README's first install on a machine zm never ran on: no zm home yet. The upgrade makes it (it records its
// drivers' pins there) and places every file.
func TestFirstInstallWithoutAHome(t *testing.T) {
	z := newZeroEnv(t)
	stage := filepath.Join(z.home, "stage")
	copyExe(t, os.Args[0], filepath.Join(stage, "zm"))
	installDriver(t, stage, "zm-run-ref", "#!/bin/sh\nexit 0\n")
	inst := filepath.Join(z.home, "bin")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(z.zmHome()); !os.IsNotExist(err) {
		t.Fatalf("the test needs a machine with no zm home: %v", err)
	}
	out, e, code := z.runBin(filepath.Join(stage, "zm"), "daemon", "upgrade", filepath.Join(stage, "zm"), "--dir", inst)
	if code != 0 {
		t.Fatalf("first install: %d\n%s\n%s", code, out, e)
	}
	for _, f := range []string{"zm", "zm-run-ref"} {
		if _, err := os.Stat(filepath.Join(inst, f)); err != nil {
			t.Errorf("not placed: %v\n%s\n%s", err, out, e)
		}
	}
	z.stopDaemon()
}

// A rolled-back upgrade: the previous driver comes back with its pin, and
// a box runs through the previous daemon.
func TestUpgradeRollbackThenDriverCall(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	if err := r.acquire(); err != nil {
		t.Fatalf("before: %v", err)
	}
	drv := filepath.Join(r.inst, "zm-run-ref")
	before := r.pin(t)
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
	if p := r.pin(t); p.SHA256 != before.SHA256 || sumOf(t, drv) != before.SHA256 {
		t.Fatalf("after the rollback: pinned %+v, file %s", p, sumOf(t, drv))
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("a driver call through the previous daemon: %v\n%s\n%s", err, out, e)
	}
}

// A group-writable install folder this user owns, inside home: the upgrade
// takes the write off (chmod go-w), lands, and its drivers run.
func TestUpgradeRepairsAGroupWritableDriverFolder(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	if err := os.Chmod(r.inst, 0o775); err != nil { // as umask 002 or a farm setup script leaves it
		t.Fatal(err)
	}
	pid0 := r.daemonPID()
	out, code := r.staged(t, r.stage(t, r.script+"# build 2\n"))
	if code != 0 || !strings.Contains(out, "chmod go-w") {
		t.Fatalf("upgrade: %d\n%s", code, out)
	}
	if fi, _ := os.Stat(r.inst); fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("%s still %v", r.inst, fi.Mode())
	}
	if p := r.daemonPID(); p == pid0 {
		t.Fatal("not swapped")
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("a driver call through the new daemon: %v\n%s", err, out)
	}
}

// A folder above home that others can write (a Homebrew prefix, a
// root-owned group-writable directory): the upgrade is refused before a
// file moves, says why and the fix; the old build keeps running and its
// drivers keep working; nothing above home is changed.
func TestUpgradeRefusedWhenDriversWouldBeRefused(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "gw", "inst") }, false)
	gw := filepath.Dir(r.inst)
	if err := os.Chmod(gw, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("before: %v", err)
	}
	pid0 := r.daemonPID()
	zmSum := sumOf(t, r.zm)
	// Home is inst itself: gw is above it, so never repaired.
	out, code := r.staged(t, r.stage(t, r.script+"# build 2\n"), "HOME="+r.inst, "ZM_HOME="+r.zmHome())
	if code != 1 || !strings.Contains(out, gw+" is writable by group or others") || !strings.Contains(out, "keeps its build") {
		t.Fatalf("upgrade: %d\n%s", code, out)
	}
	if why := swapperWhy([]byte(out)); !strings.Contains(why, "keeps its build") {
		t.Errorf("the update's record would say %q", why)
	}
	if p := r.daemonPID(); p != pid0 || sumOf(t, r.zm) != zmSum {
		t.Fatalf("the daemon or zm changed: pid %d -> %d", pid0, p)
	}
	if _, err := os.Stat(filepath.Join(r.inst, "zm-run-ref")); !os.IsNotExist(err) {
		t.Fatalf("a driver was placed: %v", err)
	}
	if fi, _ := os.Stat(gw); fi.Mode().Perm() != 0o775 {
		t.Fatalf("a folder above home changed: %v", fi.Mode())
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("the old build's drivers: %v", err)
	}
	// Updates are held, visibly (machine numbers' zm.held), with the fix:
	// no tick downloads a release only to be refused again.
	h, held, err := release.ReadHold(release.HoldPath(r.zmHome()))
	if !held || err != nil || !strings.Contains(h.Reason, gw) || !strings.Contains(h.Reason, "zm daemon update resume") {
		t.Fatalf("hold: %v %v %+v", held, err, h)
	}
	if ownAutoHold(r.zmHome()) == nil {
		t.Fatal("the hold is not marked as the driver folder's own")
	}
}

// zm daemon update --release: the signed release's drivers land beside zm
// and run through the new daemon.
func TestReleaseUpdateRunsItsDrivers(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	version := "main-" + relCommit[:12]
	name := release.Tarball(version, runtime.GOOS, runtime.GOARCH)
	prefix := strings.TrimSuffix(name, ".tar.gz")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	driver := r.script + "# the release's\n"
	var b bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755})
	for _, f := range []struct {
		name string
		body []byte
	}{{"zm", self}, {"zm-run-ref", []byte(driver)}} {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + f.name, Mode: 0o755, Size: int64(len(f.body))})
		_, _ = tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	m, err := release.FromSums(version, relCommit, time.Now().Add(-time.Hour), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"))
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

	pid0 := r.daemonPID()
	o, e, code := r.runBin(r.zm, "daemon", "update", "--release", dir, "--dir", r.inst, "--key", release.FormatKey(pub))
	if code != 0 || !strings.Contains(o, `"result":"ok"`) {
		t.Fatalf("update: %d\n%s\n%s", code, o, e)
	}
	if p := r.daemonPID(); p == pid0 {
		t.Fatalf("not swapped:\n%s", o)
	}
	drv := filepath.Join(r.inst, "zm-run-ref")
	if b, _ := os.ReadFile(drv); string(b) != driver {
		t.Fatal("the release's driver is not beside zm")
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("a driver call through the new daemon: %v\n%s", err, o)
	}
	if p := r.pin(t); p.SHA256 != sumOf(t, drv) {
		t.Fatalf("pinned %+v", p)
	}
}

// A swapper that died after placing (the unfinished-swap path): the next
// update tick settles the drivers' pins on the files that are there.
func TestUnfinishedSwapSettlesDriverPins(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	base, _ := filepath.EvalSymlinks(t.TempDir())
	dir, stage := filepath.Join(base, "inst"), filepath.Join(base, "stage")
	drv := installDriver(t, dir, "zm-run-x", "#!/bin/sh\nexit 1\n")
	installDriver(t, stage, "zm-run-x", "#!/bin/sh\nexit 2\n")
	if _, err := runtarget.PinDriver(home, []string{dir}, "x"); err != nil {
		t.Fatal(err)
	}
	plan, err := planPins(home, stage, dir, "", []string{"zm-run-x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.save(); err != nil {
		t.Fatal(err)
	}
	if err := swap.WritePending(home, swap.Unfinished{What: "upgrade", To: "x", PID: 1 << 30, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// Placed, then the swapper died before it re-pinned.
	if _, err := swap.Place(stage, dir, []string{"zm-run-x"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvNoAutostart, "1")
	var rec api.SwapRecord
	_, _ = unfinishedSwap(home, false, &rec)
	pins, _, _ := readPins(home)
	if pins["x"].SHA256 != sumOf(t, drv) {
		t.Fatalf("not settled on the placed file: %+v", pins["x"])
	}
	if _, err := os.Stat(filepath.Join(home, fileSwapPins)); !os.IsNotExist(err) {
		t.Fatalf("the plan stays: %v", err)
	}
}

// The verify's driver probe: a build whose daemon refuses a driver the old
// one ran (here the release's driver beside zm shadows the one pinned in
// $ZM_HOME/drivers) is rolled back, says which binding, and the old build
// runs its driver again.
func TestUpgradeRolledBackWhenItsDaemonRefusesADriver(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, false)
	if err := r.acquire(); err != nil {
		t.Fatalf("before: %v", err)
	}
	pid0 := r.daemonPID()
	out, code := r.staged(t, r.stage(t, r.script+"# shadows the pinned one\n"), "ZM_TEST_SWAP_GRACE=2s")
	if code != 1 || !strings.Contains(out, "driver acme/box") || !strings.Contains(out, "rolled back") || !strings.Contains(out, "drivers did not answer within 2s") {
		t.Fatalf("upgrade: %d\n%s", code, out)
	}
	if p := r.daemonPID(); p == pid0 || p == 0 {
		t.Logf("pid %d -> %d", pid0, p)
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("the old build's driver after the rollback: %v\n%s", err, out)
	}
}

// zm driver pin from another zm than the daemon's (another on PATH): it
// pins the file in the daemon's folder, and says the folders differ.
func TestDriverPinUsesTheDaemonsFolder(t *testing.T) {
	r := newDriverRig(t, func(h string) string { return filepath.Join(h, "inst") }, true)
	drv := filepath.Join(r.inst, "zm-run-ref")
	installDriver(t, r.inst, "zm-run-ref", r.script+"# rebuilt by hand\n")
	if err := r.acquire(); err == nil || !strings.Contains(err.Error(), "zm driver pin ref") {
		t.Fatalf("a changed driver: %v", err)
	}
	o, e, code := r.run(r.home, nil, "driver", "pin", "ref") // this test binary, not inst/zm
	if code != 0 || !strings.Contains(e, "the daemon runs "+r.zm) || !strings.Contains(o, "pinned ref: "+resolvedDir(drv)) {
		t.Fatalf("pin: %d\n%s\n%s", code, o, e)
	}
	if err := r.acquire(); err != nil {
		t.Fatalf("after the pin: %v", err)
	}
}
