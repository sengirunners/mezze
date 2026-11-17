package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/runtarget"
)

var detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault = detectSupervisor, supervisorRestart, supervisorProgram

// runBin runs bin (a copy of this test binary, or a script) as zm.
func (z *zeroEnv) runBin(bin string, args ...string) (string, string, int) {
	z.t.Helper()
	c := exec.Command(bin, args...)
	c.Dir = z.home
	c.Env = z.env
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		z.t.Fatalf("%s %v: %v", bin, args, err)
	}
	return o.String(), e.String(), code
}

func copyExe(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func fileSum(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// TestUpgradeSwapsAndRollsBack: an upgrade to a good build swaps the
// daemon and keeps zm.prev; one to a build whose daemon never answers its
// health check is rolled back to the previous binary, which runs again.
func TestUpgradeSwapsAndRollsBack(t *testing.T) {
	z := newZeroEnv(t)
	inst, stage, bad := filepath.Join(z.home, "inst"), filepath.Join(z.home, "stage"), filepath.Join(z.home, "bad")
	zm := filepath.Join(inst, "zm")
	copyExe(t, os.Args[0], zm)
	if _, e, code := z.runBin(zm, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	pid0 := z.daemonPID()
	// A driver beside zm, pinned: the good upgrade places
	// a new build of it and re-pins it; the rolled-back one leaves the pin
	// of the file that is back.
	drv := installDriver(t, inst, "zm-run-x", "#!/bin/sh\nexit 1\n")
	if out, e, code := z.runBin(zm, "driver", "pin", "x"); code != 0 || !strings.Contains(out, "pinned x: ") {
		t.Fatalf("pin: %d\n%s\n%s", code, out, e)
	}
	pinOf := func() string {
		t.Helper()
		var f struct {
			Drivers map[string]runtarget.DriverPin
		}
		b, err := os.ReadFile(filepath.Join(z.zmHome(), runtarget.PinFile))
		if err == nil {
			err = json.Unmarshal(b, &f)
		}
		if err != nil {
			t.Fatal(err)
		}
		return f.Drivers["x"].SHA256
	}
	hexSum := func(p string) string { s := fileSum(t, p); return hex.EncodeToString(s[:]) }
	if pinOf() != hexSum(drv) {
		t.Fatal("zm driver pin did not pin the file there")
	}

	copyExe(t, os.Args[0], filepath.Join(stage, "zm"))
	installDriver(t, stage, "zm-run-x", "#!/bin/sh\nexit 2\n")
	out, e, code := z.runBin(zm, "daemon", "upgrade", filepath.Join(stage, "zm"))
	if code != 0 || !strings.Contains(out, "zm daemon upgraded") || !strings.Contains(out, "re-pinned drivers x to the files this upgrade placed") {
		t.Fatalf("good upgrade: %d\n%s\n%s", code, out, e)
	}
	if pinOf() != hexSum(drv) || hexSum(drv) != hexSum(filepath.Join(stage, "zm-run-x")) {
		t.Fatal("the upgrade did not re-pin the driver it placed")
	}
	pinned := pinOf()
	if _, err := os.Stat(zm + swap.PrevSuffix); err != nil {
		t.Fatal("the previous binary is not kept beside the new one")
	}
	pid1 := z.daemonPID()
	if pid1 == 0 || pid1 == pid0 {
		t.Fatalf("daemon not swapped: %d -> %d", pid0, pid1)
	}
	want := fileSum(t, zm)

	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"daemon preflight\" ]; then ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"; fi\n" +
		"if [ \"$1\" = daemon ]; then echo 'zm: boom: cannot serve' >&2; exit 1; fi\n" +
		"ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"\n"
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "zm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	installDriver(t, bad, "zm-run-x", "#!/bin/sh\nexit 3\n")
	out, e, code = z.runBin(zm, "daemon", "upgrade", filepath.Join(bad, "zm"))
	if code != 1 || !strings.Contains(e, "rolled back to the previous binary") || !strings.Contains(e, "boom") {
		t.Fatalf("bad upgrade: %d\n%s\n%s", code, out, e)
	}
	if fileSum(t, zm) != want {
		t.Fatal("zm is not the previous binary after the rollback")
	}
	if pinOf() != pinned || hexSum(drv) != pinned || !strings.Contains(out, "re-pinned drivers x to the files put back") {
		t.Fatalf("after the rollback the driver's pin is not the restored file's:\n%s", out)
	}
	if _, ok, err := swap.ReadPending(z.zmHome()); ok || err != nil {
		t.Fatalf("a finished upgrade left its marker (%v)", err)
	}
	out, e, code = z.runBin(zm, "status")
	if code != 0 || !strings.Contains(out, "last swap: upgrade by") || !strings.Contains(out, "rolled_back") {
		t.Fatalf("status after the rollback: %d\n%s\n%s", code, out, e)
	}
	if p := z.daemonPID(); p == 0 || p == pid1 {
		t.Fatalf("the previous binary's daemon does not run again: %d", p)
	}
	if log, _ := os.ReadFile(filepath.Join(z.zmHome(), "daemon.log")); !bytes.Contains(log, []byte(`msg="daemon swap" what=upgrade`)) {
		t.Fatal("swaps are not in the daemon log")
	}
}

// TestConcurrentRestartRefused: while one swap holds swap.lock, a restart
// or a stop is refused with who holds it, and the daemon keeps running.
func TestConcurrentRestartRefused(t *testing.T) {
	z := newZeroEnv(t)
	if _, e, code := z.run(z.home, nil, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	pid := z.daemonPID()
	l, err := swap.Acquire(z.zmHome(), api.SwapHolder{By: "zero/aaaa1111", What: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"daemon", "restart"}, {"daemon", "stop", "--force"}, {"daemon", "upgrade", os.Args[0]}} {
		_, e, code := z.run(z.home, nil, args...)
		if code != 1 || !strings.Contains(e, "upgrade of this daemon is in progress: zero/aaaa1111") {
			t.Fatalf("%v during a swap: %d %s", args, code, e)
		}
	}
	if z.daemonPID() != pid {
		t.Fatal("the daemon was stopped during another swap")
	}
	l.Release()
	if out, e, code := z.run(z.home, nil, "daemon", "restart"); code != 0 || !strings.Contains(out, "zm daemon restarted") {
		t.Fatalf("restart after the swap: %d %s %s", code, out, e)
	}
	if p := z.daemonPID(); p == 0 || p == pid {
		t.Fatal("not restarted")
	}
}

// swapRig is a daemon running from inst/zm (a copy of this test binary) on
// a temp home, for swaps driven in this process (runSwap) or as zm.
type swapRig struct {
	*zeroEnv
	inst, zm string
	sum      [32]byte
}

func newSwapRig(t *testing.T) *swapRig {
	z := newZeroEnv(t)
	r := &swapRig{zeroEnv: z, inst: filepath.Join(z.home, "inst")}
	r.zm = filepath.Join(r.inst, "zm")
	copyExe(t, os.Args[0], r.zm)
	r.sum = fileSum(t, r.zm)
	// runSwap in this process reads the same environment the zm calls get.
	for _, kv := range z.env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	for _, k := range []string{"ZM_HOME", "ZM_HOST", "ZM_AS", "ZM_SESSION", "ZM_TOKEN", "ZM_PROJECT"} {
		t.Setenv(k, "")
	}
	if _, e, code := z.runBin(r.zm, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	return r
}

func (r *swapRig) health(t *testing.T) api.Health {
	t.Helper()
	h, err := client.NewUnix(filepath.Join(r.zmHome(), "zm.sock")).Health(context.Background())
	if err != nil {
		t.Fatalf("no daemon answers: %v", err)
	}
	return h
}

// stage copies this test binary into a new directory each time: on macOS,
// writing over a file that was executed gets the next exec of it killed
// (its code signature no longer matches).
func (r *swapRig) stage(t *testing.T) string {
	dir, err := os.MkdirTemp(r.home, "stage")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "zm")
	copyExe(t, os.Args[0], p)
	return p
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// TestUpgradeRejectsBuildMismatch: the staged binary's preflight describes
// one build and its daemon is another. The daemon starts and answers, is
// rejected, is stopped, and the previous binary runs again.
func TestUpgradeRejectsBuildMismatch(t *testing.T) {
	r := newSwapRig(t)
	bad := filepath.Join(r.home, "bad", "zm")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"daemon preflight\" ]; then echo '{\"ok\":true,\"build\":\"v9 (fake)\",\"problems\":[],\"warnings\":[]}'; exit 0; fi\n" +
		"ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"\n"
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, e, code := r.runBin(r.zm, "daemon", "upgrade", bad)
	if code != 1 || !strings.Contains(e, "the daemon reports") || !strings.Contains(e, "the preflight described v9 (fake)") ||
		!strings.Contains(e, "rolled back to") || strings.Contains(e, "down") || strings.Contains(e, "still runs") {
		t.Fatalf("mismatch upgrade: %d\n%s\n%s", code, out, e)
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("zm is not the previous binary")
	}
	h := r.health(t)
	if h.PID != r.daemonPID() || h.LastSwap == nil || h.LastSwap.Result != "rolled_back" {
		t.Fatalf("after the rollback: pid %d, lock %d, last swap %+v", h.PID, r.daemonPID(), h.LastSwap)
	}
}

// TestUpgradeRejectsServedSetRegression: the new daemon answers but no
// longer serves what the old one did. It is stopped (its saved pid is
// gone), the files go back, the previous binary runs.
func TestUpgradeRejectsServedSetRegression(t *testing.T) {
	r := newSwapRig(t)
	staged := r.stage(t)
	calls, rejected := 0, 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := swap.Probe(ctx, c, wiring.Probes...)
		if calls == 2 { // the new daemon: it lost the project list
			h, _ := c.Health(ctx)
			rejected = h.PID
			sc.Items = sc.Items[:0]
		}
		return sc
	}
	t.Cleanup(func() { probe = swapProbe })
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: staged})
	var ew exitWith
	if !errors.As(err, &ew) || ew.code != 1 || !strings.Contains(err.Error(), "does not serve what the old daemon served") {
		t.Fatalf("regression upgrade: %v", err)
	}
	if rejected == 0 || alive(rejected) {
		t.Fatalf("the rejected daemon (pid %d) still runs", rejected)
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("zm is not the previous binary")
	}
	if h := r.health(t); h.PID == rejected || h.LastSwap == nil || h.LastSwap.Result != "rolled_back" {
		t.Fatalf("after the rollback: %+v", h)
	}
}

// withJoined adds a joined project harbor to sc: served (ok), or failing
// the way a joined project fails while the new daemon's tsnet node comes
// up (2026-11-03: its remote not ok yet, its reads unavailable).
func withJoined(sc api.ServeCheck, ok bool) api.ServeCheck {
	if ok {
		sc.Items = append(sc.Items, api.ServeItem{Kind: swap.ItemJoined, Name: "harbor", OK: true}, api.ServeItem{Kind: swap.ItemIssues, Name: "harbor", OK: true})
		return sc
	}
	sc.Items = append(sc.Items, api.ServeItem{Kind: swap.ItemIssues, Name: "harbor", Code: api.CodeUnavailable,
		Error: "authority zm-dev-05.tail1234.ts.net:7433 unreachable: dial tcp 100.64.0.7:7433: i/o timeout"})
	return sc
}

func shortGrace(t *testing.T, grace time.Duration) {
	g, l, r := swapRemoteGrace, swapLocalGrace, swapRemoteRetry
	swapRemoteGrace, swapLocalGrace, swapRemoteRetry = grace, grace, 10*time.Millisecond
	t.Cleanup(func() { swapRemoteGrace, swapLocalGrace, swapRemoteRetry, probe = g, l, r, swapProbe })
}

// localBusy marks sc's project list failing `unavailable`, the way a local
// store still busy at the new daemon's first start answers (2026-11-17,
// dev-05: "the native tracker failed" while the bd mirror's first sync
// held the database).
func localBusy(sc api.ServeCheck) api.ServeCheck {
	for i, it := range sc.Items {
		if it.Kind == swap.ItemProjects {
			sc.Items[i].OK, sc.Items[i].Code, sc.Items[i].Error = false, api.CodeUnavailable, "the native tracker failed"
		}
	}
	return sc
}

// TestUpgradeWaitsForBusyLocalStore: a local item failing `unavailable`
// for the new daemon's first probes is probed again and the upgrade is
// kept once it serves; one still unavailable after the local grace is
// refused, saying so.
func TestUpgradeWaitsForBusyLocalStore(t *testing.T) {
	r := newSwapRig(t)
	shortGrace(t, time.Minute)
	calls := 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := swap.Probe(ctx, c, wiring.Probes...)
		if calls == 2 || calls == 3 {
			return localBusy(sc)
		}
		return sc
	}
	if err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)}); err != nil {
		t.Fatalf("an upgrade whose local store answers within the grace: %v", err)
	}
	if calls < 4 {
		t.Fatalf("probes: %d", calls)
	}
	if h := r.health(t); h.LastSwap == nil || h.LastSwap.Result != "ok" {
		t.Fatalf("after the upgrade: %+v", h.LastSwap)
	}

	shortGrace(t, 300*time.Millisecond)
	calls = 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := swap.Probe(ctx, c, wiring.Probes...)
		if calls > 1 {
			return localBusy(sc)
		}
		return sc
	}
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	if err == nil || !strings.Contains(err.Error(), "still unavailable after 300ms") || !strings.Contains(err.Error(), "projects *: served before, now the native tracker failed") ||
		!strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("a local store still unavailable after the grace: %v", err)
	}
}

// TestUpgradeWaitsForAuthorities: the new
// daemon's joined projects fail "authority ... unreachable" for its first
// probes while tsnet comes up, then serve. The upgrade waits and keeps it.
func TestUpgradeWaitsForAuthorities(t *testing.T) {
	r := newSwapRig(t)
	shortGrace(t, time.Minute)
	calls := 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		return withJoined(swap.Probe(ctx, c, wiring.Probes...), calls == 1 || calls >= 4)
	}
	if err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)}); err != nil {
		t.Fatalf("an upgrade whose authorities come up within the grace: %v", err)
	}
	if calls < 4 {
		t.Fatalf("probes: %d", calls)
	}
	if h := r.health(t); h.LastSwap == nil || h.LastSwap.Result != "ok" {
		t.Fatalf("after the upgrade: %+v", h.LastSwap)
	}
}

// TestUpgradeRemoteGraceIsBounded: a joined project that never comes back
// is refused once the grace is over, and a local regression is refused at
// once whatever the grace.
func TestUpgradeRemoteGraceIsBounded(t *testing.T) {
	r := newSwapRig(t)
	shortGrace(t, 300*time.Millisecond)
	calls := 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		return withJoined(swap.Probe(ctx, c, wiring.Probes...), calls == 1)
	}
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	if err == nil || !strings.Contains(err.Error(), "issues harbor: served before, now authority") || !strings.Contains(err.Error(), "within 300ms") ||
		!strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("a joined project still unreachable after the grace: %v", err)
	}

	shortGrace(t, time.Hour)
	calls = 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := withJoined(swap.Probe(ctx, c, wiring.Probes...), calls == 1)
		if calls > 1 { // and a local regression: the project list is gone
			sc.Items = sc.Items[len(sc.Items)-1:]
		}
		return sc
	}
	start := time.Now()
	err = runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	if err == nil || !strings.Contains(err.Error(), "projects *: served before, missing now") || time.Since(start) > 30*time.Second {
		t.Fatalf("a local regression must refuse without the grace: %v after %s", err, time.Since(start))
	}
}

// TestUpgradeRollsForwardWhenDataIsNewer:
// the new daemon applied a registry.db migration the previous build does
// not know, then failed its verify. Rolling back would leave the previous
// build on data newer than it ("the data is newer than the build"), worse
// than either; the new daemon is kept, loudly, and the swap exits 1.
func TestUpgradeRollsForwardWhenDataIsNewer(t *testing.T) {
	r := newSwapRig(t)
	calls, kept := 0, 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := swap.Probe(ctx, c, wiring.Probes...)
		if calls == 2 { // the new daemon: it migrated, and lost the project list
			h, _ := c.Health(ctx)
			kept = h.PID
			db, err := store.OpenFile(ctx, filepath.Join(r.zmHome(), store.FileRegistry), 1, 5*time.Second, false)
			if err != nil {
				t.Errorf("open registry.db: %v", err)
				return sc
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES ('session/9999_future', 0)`); err != nil {
				t.Errorf("migrate: %v", err)
			}
			db.Close()
			sc.Items = sc.Items[:0]
		}
		return sc
	}
	t.Cleanup(func() { probe = swapProbe })
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	var ew exitWith
	if !errors.As(err, &ew) || ew.code != 1 || !strings.Contains(err.Error(), "not rolled back") ||
		!strings.Contains(err.Error(), "registry.db has migration session/9999_future") || !strings.Contains(err.Error(), "does not serve what the old daemon served") {
		t.Fatalf("want a loud roll-forward: %v", err)
	}
	h := r.health(t)
	if kept == 0 || h.PID != kept || h.LastSwap == nil || h.LastSwap.Result != "rolled_forward" {
		t.Fatalf("the new daemon (pid %d) must keep running: %+v", kept, h)
	}
	if _, err := os.Stat(r.zm + swap.PrevSuffix); err != nil {
		t.Fatal("the previous binary must stay beside the new one as zm.prev")
	}
}

// TestUpgradeUnderSupervisor: a daemon a service manager runs is
// restarted through it, twice when the new build is rejected, and zm
// never starts one of its own beside it.
func TestUpgradeUnderSupervisor(t *testing.T) {
	r := newSwapRig(t)
	restarts := 0
	detectSupervisor = func(context.Context, int) (*supervisor, error) {
		return &supervisor{Kind: "systemd", Unit: "zm.service", User: true}, nil
	}
	// The fake systemd: TERM the main pid (it was saved in daemon.lock),
	// then start the unit's binary.
	supervisorRestart = func(ctx context.Context, _ *supervisor) error {
		restarts++
		if pid := r.daemonPID(); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 500 && alive(pid); i++ {
				time.Sleep(10 * time.Millisecond)
			}
		}
		c := exec.Command(r.zm, "daemon")
		c.Env = r.env
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return c.Start()
	}
	supervisorProgram = func(context.Context, *supervisor) (string, error) { return r.zm, nil }
	supervisorMain = func(context.Context, *supervisor) (int, error) { return r.daemonPID(), nil } // the unit runs the daemon
	t.Cleanup(func() {
		detectSupervisor, supervisorRestart, supervisorProgram, probe = detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault, swapProbe
		supervisorMain = supervisorMainDefault
	})
	if err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)}); err != nil {
		t.Fatalf("upgrade under a supervisor: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("restarts through the supervisor: %d", restarts)
	}
	calls := 0
	probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		sc := swap.Probe(ctx, c, wiring.Probes...)
		if calls == 2 {
			sc.Items = sc.Items[:0]
		}
		return sc
	}
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	if err == nil || !strings.Contains(err.Error(), "rolled back") || restarts != 3 {
		t.Fatalf("a rejected build under a supervisor: %v, restarts %d", err, restarts)
	}
	if h := r.health(t); h.PID != r.daemonPID() || h.LastSwap.Result != "rolled_back" {
		t.Fatalf("after the rollback: %+v, lock pid %d", h, r.daemonPID())
	}
}

func TestSupervisorParsing(t *testing.T) {
	cg := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/zm.service\n"
	if u, user := unitFromCgroup(cg); u != "zm.service" || !user {
		t.Fatalf("user unit: %q %v", u, user)
	}
	if u, user := unitFromCgroup("12:pids:/system.slice/zm.service\n1:name=systemd:/system.slice/zm.service\n"); u != "zm.service" || user {
		t.Fatalf("system unit: %q %v", u, user)
	}
	if u, _ := unitFromCgroup("0::/user.slice/user-1000.slice/session-3.scope\n"); u != "" {
		t.Fatalf("a login session is not a supervisor: %q", u)
	}
	list := "PID\tStatus\tLabel\n-\t0\tcom.apple.x\n4242\t0\tdev.zm.daemon\n424\t0\tother\n"
	if l := labelForPID(list, 4242); l != "dev.zm.daemon" {
		t.Fatalf("launchd label: %q", l)
	}
	if l := labelForPID(list, 42); l != "" {
		t.Fatalf("no job: %q", l)
	}
}

// TestUpgradeRefusesWhenUnitRunsAnotherBinary: a supervisor whose unit
// starts a binary outside the install directory would restart the old
// build; the upgrade refuses, names that binary and places nothing.
func TestUpgradeRefusesWhenUnitRunsAnotherBinary(t *testing.T) {
	r := newSwapRig(t)
	restarts := 0
	detectSupervisor = func(context.Context, int) (*supervisor, error) {
		return &supervisor{Kind: "systemd", Unit: "zm.service", User: true}, nil
	}
	supervisorRestart = func(context.Context, *supervisor) error { restarts++; return nil }
	supervisorProgram = func(context.Context, *supervisor) (string, error) { return "/opt/zm/bin/zm", nil }
	t.Cleanup(func() {
		detectSupervisor, supervisorRestart, supervisorProgram = detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault
	})
	pid := r.daemonPID()
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
	if err == nil || !strings.Contains(err.Error(), "the systemd --user unit zm.service runs /opt/zm/bin/zm, not "+r.zm) ||
		!strings.Contains(err.Error(), "nothing was placed") || !strings.Contains(err.Error(), "INSTALL_DIR=/opt/zm/bin") {
		t.Fatalf("want a refusal naming the unit's binary: %v", err)
	}
	if restarts != 0 || r.daemonPID() != pid {
		t.Fatal("the daemon was restarted")
	}
	if _, err := os.Stat(r.zm + swap.PrevSuffix); !os.IsNotExist(err) || fileSum(t, r.zm) != r.sum {
		t.Fatal("files were placed")
	}
}

func TestSupervisorProgramParsing(t *testing.T) {
	if p := execStartPath("{ path=/usr/local/bin/zm ; argv[]=/usr/local/bin/zm daemon ; ignore_errors=no ; start_time=[n/a] ; pid=0 }\n"); p != "/usr/local/bin/zm" {
		t.Fatalf("ExecStart: %q", p)
	}
	print := "gui/501/dev.zm.daemon = {\n\tactive count = 1\n\tpath = /Users/a/Library/LaunchAgents/dev.zm.daemon.plist\n\tprogram = /Users/a/go/bin/zm\n\targuments = {\n\t\t/Users/a/go/bin/zm\n\t\tdaemon\n\t}\n"
	if p := launchdProgram(print); p != "/Users/a/go/bin/zm" {
		t.Fatalf("launchd program: %q", p)
	}
	if p := launchdProgram("\targuments = {\n\t\t/x/zm\n\t\tdaemon\n\t}\n"); p != "/x/zm" {
		t.Fatalf("launchd arguments: %q", p)
	}
}

// cancelAtFirstProbe cancels the swap's context at its first probe: the
// old daemon still runs and nothing has been stopped or started yet, the
// files are placed. That is SIGINT/SIGTERM mid-swap.
func cancelAtFirstProbe(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	probe = func(pctx context.Context, c *client.Client) api.ServeCheck {
		calls++
		if calls == 1 {
			cancel()
		}
		return swap.Probe(pctx, c, wiring.Probes...)
	}
	t.Cleanup(func() { cancel(); probe = swapProbe })
	return ctx
}

// TestUpgradeCancelledMidSwapFinishes: a signal after the files
// are placed used to return from the start at once, mark the new daemon
// "failed: context canceled", restore the old files under it and report
// "the daemon is down" while it ran unverified. The swap now finishes: the
// new daemon is verified and kept, and the report says so.
func TestUpgradeCancelledMidSwapFinishes(t *testing.T) {
	r := newSwapRig(t)
	pid0 := r.daemonPID()
	staged := r.stage(t)
	want := fileSum(t, staged)
	err := runSwap(cancelAtFirstProbe(t), swapOpts{what: "upgrade", newBin: staged})
	if err != nil {
		t.Fatalf("a cancelled upgrade did not finish: %v", err)
	}
	h := r.health(t)
	if h.PID == pid0 || h.PID != r.daemonPID() || !alive(h.PID) || alive(pid0) {
		t.Fatalf("after the upgrade: old pid %d (alive %v), health pid %d, lock pid %d", pid0, alive(pid0), h.PID, r.daemonPID())
	}
	if h.LastSwap == nil || h.LastSwap.Result != "ok" || h.LastSwap.At.IsZero() {
		t.Fatalf("last swap: %+v", h.LastSwap)
	}
	if fileSum(t, r.zm) != want {
		t.Fatal("zm on disk is not the build that runs")
	}
}

// TestUpgradeCancelledMidSwapRollsBack: cancelled mid-swap to a build its
// verify rejects, the swap still stops it, puts the files back and runs the
// previous binary, and reports a rollback (not "the daemon is down").
func TestUpgradeCancelledMidSwapRollsBack(t *testing.T) {
	r := newSwapRig(t)
	bad := filepath.Join(r.home, "bad", "zm")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"daemon preflight\" ]; then echo '{\"ok\":true,\"build\":\"v9 (fake)\",\"problems\":[],\"warnings\":[]}'; exit 0; fi\n" +
		"ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"\n"
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runSwap(cancelAtFirstProbe(t), swapOpts{what: "upgrade", newBin: bad})
	var ew exitWith
	if !errors.As(err, &ew) || ew.code != 1 || !strings.Contains(err.Error(), "rolled back to") ||
		strings.Contains(err.Error(), "down") || strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancelled bad upgrade: %v", err)
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("zm is not the previous binary")
	}
	if h := r.health(t); h.PID != r.daemonPID() || h.LastSwap == nil || h.LastSwap.Result != "rolled_back" {
		t.Fatalf("after the rollback: pid %d, lock %d, last swap %+v", h.PID, r.daemonPID(), h.LastSwap)
	}
}

// TestSwapperCommandTermsAndWaits: an updater's cancelled context sends its
// swapper SIGTERM and waits for it to finish, not exec's SIGKILL.
func TestSwapperCommandTermsAndWaits(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	done, ready := filepath.Join(dir, "done"), filepath.Join(dir, "ready")
	script := filepath.Join(dir, "swapper")
	body := "#!/bin/sh\ntrap 'sleep 0.3; echo finished > " + done + "; exit 0' TERM\ntouch " + ready + "\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := swapperFinishWait
	swapperFinishWait = 3 * time.Second // a swapper that ignores SIGTERM: killed soon, not after minutes
	t.Cleanup(func() { swapperFinishWait = saved })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := swapperCommand(ctx, script)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err = p.Wait()
	if b, _ := os.ReadFile(done); string(b) != "finished\n" {
		t.Fatalf("the swapper did not finish after the cancel (wait: %v)", err)
	}
}

// TestSwapperFinishWaitAboveBudget: the updater waits longer than its
// swapper's own bound, so it reads the swapper's record, not a kill.
func TestSwapperFinishWaitAboveBudget(t *testing.T) {
	if swapperFinishWait <= swapFinishBudget {
		t.Fatalf("swapperFinishWait %s is not above swapFinishBudget %s", swapperFinishWait, swapFinishBudget)
	}
	if swapRollbackReserve >= swapFinishBudget {
		t.Fatalf("the rollback reserve %s leaves the forward steps nothing of %s", swapRollbackReserve, swapFinishBudget)
	}
	// A supervised rollback's worst case: a settled health, the supervisor's
	// main pid, a stop of a daemon beside it, the restart and waitNew.
	worst := swapSettleWait + supervisorCallWait + swapStopCallWait + swapStopWait +
		2*(swapSupervisorStopWait+swapStartWait)
	if swapRollbackReserve < worst {
		t.Fatalf("the rollback reserve %s is under a supervised rollback's worst case %s", swapRollbackReserve, worst)
	}
}

// TestUpgradeCancelledBeforeCommitChangesNothing: a signal after the dry-run
// stop but before commit stops the swap, with no file placed and the old
// daemon still running.
func TestUpgradeCancelledBeforeCommitChangesNothing(t *testing.T) {
	r := newSwapRig(t)
	pid0 := r.daemonPID()
	staged := r.stage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beforeCommit = cancel
	t.Cleanup(func() { beforeCommit = nil })
	err := runSwap(ctx, swapOpts{what: "upgrade", newBin: staged})
	if err == nil || !strings.Contains(err.Error(), "interrupted before the swap changed anything") {
		t.Fatalf("cancelled before commit: %v", err)
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("zm on disk changed")
	}
	if _, err := os.Stat(r.zm + swap.PrevSuffix); !os.IsNotExist(err) {
		t.Fatalf("a previous file was kept, so files were placed: %v", err)
	}
	if h := r.health(t); h.PID != pid0 || r.daemonPID() != pid0 {
		t.Fatalf("the daemon changed: was pid %d, health pid %d, lock pid %d", pid0, h.PID, r.daemonPID())
	}
}

// wedge is a daemon that takes connections on sock and never answers
// (ZM_TEST_WEDGE): a build that hangs after it listens.
func wedge(sock string) {
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	if p := os.Getenv("ZM_TEST_WEDGE_PID"); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	var held []net.Conn
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(3)
		}
		held = append(held, c)
	}
}

// TestUpgradeWedgedDaemonEndsInBudget: past the commit the swap ignores the
// signal, so a new daemon that takes the socket and never answers must
// still not hold it. Bounded by the start wait (each health call has its
// own timeout) or by the swap's budget, it ends rolled back, and says why.
func TestUpgradeWedgedDaemonEndsInBudget(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		start, budget, reserve time.Duration
		within                 time.Duration // the swap ends within this
		why                    string
	}{
		// Each health call ends at the start wait, so it ends long before the budget.
		{"start wait", 2 * time.Second, 2 * time.Minute, 90 * time.Second, time.Minute, "it did not answer within 2s"},
		// The forward steps get 10 s; the rollback's cold start 30 s (loaded CI).
		{"budget", time.Hour, 40 * time.Second, 30 * time.Second, 55 * time.Second, "ran out of its 40s budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSwapRig(t)
			pidFile := filepath.Join(r.home, "wedge.pid")
			t.Cleanup(func() {
				if b, err := os.ReadFile(pidFile); err == nil {
					if pid, _ := strconv.Atoi(string(b)); alive(pid) {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			bad := filepath.Join(r.home, "bad", "zm")
			script := "#!/bin/sh\n" +
				"if [ \"$1 $2\" = \"daemon preflight\" ]; then ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"; fi\n" +
				"ZM_TEST_WEDGE=" + filepath.Join(r.zmHome(), "zm.sock") + " ZM_TEST_WEDGE_PID=" + pidFile + " exec " + os.Args[0] + "\n"
			if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bad, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			saved := []time.Duration{swapStartWait, swapFinishBudget, swapRollbackReserve, swapCallWait}
			swapStartWait, swapFinishBudget, swapRollbackReserve, swapCallWait = tc.start, tc.budget, tc.reserve, 500*time.Millisecond
			// The rollback's real daemon start gets a full wait: a cold start
			// on a loaded machine is not what this tests.
			onRollback = func() {
				if swapStartWait < time.Minute {
					swapStartWait = time.Minute
				}
			}
			t.Cleanup(func() {
				swapStartWait, swapFinishBudget, swapRollbackReserve, swapCallWait = saved[0], saved[1], saved[2], saved[3]
				onRollback = nil
			})
			ctx := cancelAtFirstProbe(t) // and a signal mid-swap: it is ignored
			begun := time.Now()
			res := make(chan error, 1)
			go func() { res <- runSwap(ctx, swapOpts{what: "upgrade", newBin: bad}) }()
			var err error
			select {
			case err = <-res:
			case <-time.After(150 * time.Second):
				t.Fatal("the swap did not end: a wedged daemon holds it")
			}
			took := time.Since(begun)
			var ew exitWith
			if !errors.As(err, &ew) || ew.code != 1 || !strings.Contains(err.Error(), "rolled back to") ||
				!strings.Contains(err.Error(), tc.why) || strings.Contains(err.Error(), "down") {
				t.Fatalf("wedged upgrade (%s): %v", took, err)
			}
			if took > tc.within {
				t.Fatalf("the swap took %s, over %s (budget %s)", took, tc.within, tc.budget)
			}
			if fileSum(t, r.zm) != r.sum {
				t.Fatal("zm is not the previous binary")
			}
			if h := r.health(t); h.PID != r.daemonPID() || h.LastSwap == nil || h.LastSwap.Result != "rolled_back" ||
				!strings.Contains(h.LastSwap.Reason, tc.why) {
				t.Fatalf("after the rollback: pid %d, lock %d, last swap %+v", h.PID, r.daemonPID(), h.LastSwap)
			}
		})
	}
}

// TestUpgradeWedgedUnderSupervisorRollsBack: a new daemon the service
// manager started takes the socket and never answers. The old one cannot
// be told apart from it (the health call times out), but under a manager
// that is no guess: the manager replaces its own process, so the swap puts
// the previous files back and restarts through it.
func TestUpgradeWedgedUnderSupervisorRollsBack(t *testing.T) {
	r := newSwapRig(t)
	bad := filepath.Join(r.home, "bad", "zm")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"daemon preflight\" ]; then ZM_TEST_MAIN=1 exec " + os.Args[0] + " \"$@\"; fi\n" +
		"ZM_TEST_WEDGE=" + filepath.Join(r.zmHome(), "zm.sock") + " exec " + os.Args[0] + "\n"
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var own *exec.Cmd // the manager's process
	restarts := 0
	stopOwn := func() {
		if own != nil {
			_ = own.Process.Kill()
			_ = own.Wait()
			own = nil
		}
	}
	detectSupervisor = func(context.Context, int) (*supervisor, error) {
		return &supervisor{Kind: "systemd", Unit: "zm.service", User: true}, nil
	}
	// The fake systemd: stop its own process (the daemon in daemon.lock at
	// first, then whatever it started), then start the unit's binary.
	supervisorRestart = func(ctx context.Context, _ *supervisor) error {
		restarts++
		stopOwn()
		if pid := r.daemonPID(); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 500 && alive(pid); i++ {
				time.Sleep(10 * time.Millisecond)
			}
		}
		c := exec.Command(r.zm, "daemon")
		c.Env = r.env
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			return err
		}
		own = c
		return nil
	}
	supervisorProgram = func(context.Context, *supervisor) (string, error) { return r.zm, nil }
	supervisorMain = func(context.Context, *supervisor) (int, error) { return r.daemonPID(), nil }
	saved := []time.Duration{swapStartWait, swapSettleWait, swapCallWait}
	swapStartWait, swapSettleWait, swapCallWait = 2*time.Second, 2*time.Second, 500*time.Millisecond
	onRollback = func() { // the rollback's real daemon start gets a full wait
		if swapStartWait < time.Minute {
			swapStartWait = time.Minute
		}
	}
	t.Cleanup(func() {
		detectSupervisor, supervisorRestart, supervisorProgram = detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault
		supervisorMain = supervisorMainDefault
		swapStartWait, swapSettleWait, swapCallWait = saved[0], saved[1], saved[2]
		onRollback = nil
	})
	t.Cleanup(stopOwn) // the wedge, if the swap left it; else the rolled-back daemon
	err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: bad})
	var ew exitWith
	if !errors.As(err, &ew) || ew.code != 1 || !strings.Contains(err.Error(), "rolled back to") ||
		!strings.Contains(err.Error(), "brought no new daemon up within 2s") || !strings.Contains(err.Error(), "did not answer within 2s") ||
		strings.Contains(err.Error(), "guess") || strings.Contains(err.Error(), "down") {
		t.Fatalf("a wedged daemon under a supervisor must be rolled back through it: %v", err)
	}
	if restarts != 2 {
		t.Fatalf("restarts through the supervisor: %d, want 2 (forward, rollback)", restarts)
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("zm is not the previous binary")
	}
	h := r.health(t)
	if h.PID != r.daemonPID() || h.LastSwap == nil || h.LastSwap.Result != "rolled_back" ||
		!strings.Contains(h.LastSwap.Reason, "brought no new daemon up") {
		t.Fatalf("after the rollback: pid %d, lock %d, last swap %+v", h.PID, r.daemonPID(), h.LastSwap)
	}
}

// TestSupervisorOutputTimesOut: a hung systemctl or launchctl call ends at
// its wait (a committed swap ignores the signal that would have ended it).
func TestSupervisorOutputTimesOut(t *testing.T) {
	begun := time.Now()
	if _, err := supervisorOutput(context.Background(), 200*time.Millisecond, true, "sleep", "30"); err == nil {
		t.Fatal("a hung command returned no error")
	}
	if took := time.Since(begun); took > 3*time.Second {
		t.Fatalf("a hung command held the swap %s", took)
	}
}

func TestSwapSeesSlowDaemon(t *testing.T) {
	z := newZeroEnv(t)
	for _, kv := range z.env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	for _, k := range []string{"ZM_HOME", "ZM_HOST", "ZM_AS", "ZM_SESSION", "ZM_TOKEN", "ZM_PROJECT"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(z.zmHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(z.home, "zm")
	pf := "#!/bin/sh\necho '{\"ok\":true,\"build\":\"v9 (fake)\",\"problems\":[],\"warnings\":[]}'\n"
	if err := os.WriteFile(exe, []byte(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true") // a pid that has exited: the stop's wait ends at once
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(z.zmHome(), "zm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	srv := &http.Server{}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch r.Method + " " + r.URL.Path {
		case "GET /v0/health":
			if !stopped.Load() {
				time.Sleep(4 * time.Second)
			}
			reply(api.Health{PID: dead.Process.Pid})
		case "GET /v0/daemon/manifest":
			reply(api.DaemonManifest{Exe: exe, Args: []string{"daemon"}})
		case "POST /v0/daemon/stop":
			var req api.DaemonStopRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			reply(api.DaemonStopResponse{})
			if !req.DryRun {
				stopped.Store(true)
				go func() { time.Sleep(50 * time.Millisecond); _ = srv.Close() }()
			}
		case "POST /v0/daemon/swaps":
			reply(api.SwapRecord{})
		default:
			http.NotFound(w, r)
		}
	})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := runSwap(context.Background(), swapOpts{what: "stop"}); err != nil {
		t.Fatalf("a slow daemon read as not running: %v", err)
	}
	if !stopped.Load() {
		t.Fatal("the swap did not stop the slow daemon")
	}
}

// syncBuf is a bytes.Buffer safe to read while it is written.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// waitFor waits up to 3 s for buf to hold s.
func (b *syncBuf) waitFor(s string) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(b.String(), s) {
			return true
		}
	}
	return false
}

// captureStderr sends os.Stderr to the returned buffer until the test ends.
func captureStderr(t *testing.T) *syncBuf {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	buf, old, copied := &syncBuf{}, os.Stderr, make(chan struct{})
	os.Stderr = w
	go func() { _, _ = io.Copy(io.MultiWriter(buf, old), r); close(copied) }()
	t.Cleanup(func() { os.Stderr = old; w.Close(); <-copied; r.Close() })
	return buf
}

// TestUpgradeSignalsMidSwapAnswer: real SIGINTs mid-swap, as
// main's signal context gets them. The first says when the swap ends, a
// second prints the step it waits on and the time left (before, it was
// swallowed), and neither stops it: the upgrade still verifies and keeps
// the new daemon.
func TestUpgradeSignalsMidSwapAnswer(t *testing.T) {
	r := newSwapRig(t)
	pid0 := r.daemonPID()
	staged := r.stage(t)
	want := fileSum(t, staged)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errOut := captureStderr(t)
	var first, second bool
	calls := 0
	probe = func(pctx context.Context, c *client.Client) api.ServeCheck {
		if calls++; calls == 1 {
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
			first = errOut.waitFor("zm: finishing the swap: it ends within ")
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
			second = errOut.waitFor("zm: the swap is probing what the old daemon (pid ")
		}
		return swap.Probe(pctx, c, wiring.Probes...)
	}
	t.Cleanup(func() { probe = swapProbe })
	err := runSwap(ctx, swapOpts{what: "upgrade", newBin: staged})
	out := errOut.String()
	if !first || !second {
		t.Fatalf("signals mid-swap: first line %v, progress line %v; stderr:\n%s", first, second, out)
	}
	if !regexp.MustCompile(`ends within 1[23]m\d+s; Ctrl-C again to see its progress\n`).MatchString(out) ||
		!regexp.MustCompile(`\(\d+s so far\); it ends within 1[23]m\d+s, verified, rolled back or rolled forward\n`).MatchString(out) {
		t.Fatalf("budget not in the lines:\n%s", out)
	}
	if strings.Count(out, "finishing the swap") != 1 {
		t.Fatalf("the first line twice:\n%s", out)
	}
	if err != nil {
		t.Fatalf("a signalled upgrade did not finish: %v", err)
	}
	h := r.health(t)
	if h.PID == pid0 || !alive(h.PID) || alive(pid0) || h.LastSwap == nil || h.LastSwap.Result != "ok" {
		t.Fatalf("after the upgrade: old pid %d, health pid %d, last swap %+v", pid0, h.PID, h.LastSwap)
	}
	if fileSum(t, r.zm) != want {
		t.Fatal("zm on disk is not the build that runs")
	}
	// The swap's watch is gone: a signal now is main's alone (the progress
	// line does not come again).
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(errOut.String(), "zm: the swap is "); n != 1 {
		t.Fatalf("%d progress lines after the swap ended:\n%s", n, errOut.String())
	}
}

// testBuildHooks (zm as this test binary): ZM_TEST_VERSION/ZM_TEST_COMMIT
// make it another build; ZM_TEST_KILL_AT_VERIFY=1 makes a swapper SIGKILL
// itself as its verify begins (the new daemon answers, unverified), as
// systemd's TimeoutStopSec or the OOM killer would; ZM_TEST_SWAP_GRACE
// shortens the verify's grace.
func testBuildHooks() {
	if v := os.Getenv("ZM_TEST_VERSION"); v != "" {
		api.Version = v
	}
	if c := os.Getenv("ZM_TEST_COMMIT"); c != "" {
		api.Commit = c
	}
	if g, err := time.ParseDuration(os.Getenv("ZM_TEST_SWAP_GRACE")); err == nil { // a short verify grace
		swapRemoteGrace, swapRemoteRetry = g, 100*time.Millisecond
	}
	if os.Getenv("ZM_TEST_KILL_AT_VERIFY") == "1" {
		onStep = func(step string) {
			if strings.HasPrefix(step, "verifying the new daemon") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	}
}

// TestUpdateTickAfterSwapperKilledBeforeVerify: a swapper
// SIGKILLed after its commit and before its verify leaves the new build
// running unverified. The next update tick used to see running == the
// release's commit, call it unchanged and, with no state file, record the
// release as installed (then stable, vouched for to other machines). It
// now finds the swap's marker with swap.lock free, reports the release
// failed and records it failed here; later ticks never install it.
func TestUpdateTickAfterSwapperKilledBeforeVerify(t *testing.T) {
	r := newSwapRig(t)
	pid0 := r.daemonPID()
	dir, key, m := releaseDir(t, time.Now().Add(-time.Hour), "new zm")
	staged := filepath.Join(r.home, "relstage", "zm")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nZM_TEST_MAIN=1 ZM_TEST_VERSION=" + m.Version + " ZM_TEST_COMMIT=" + m.Commit + " exec " + os.Args[0] + " \"$@\"\n"
	if err := os.WriteFile(staged, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	up := exec.Command(staged, "daemon", "upgrade", staged, "--dir", r.inst)
	up.Dir, up.Env = r.home, append(append([]string{}, r.env...), "ZM_TEST_KILL_AT_VERIFY=1")
	out, err := up.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the swapper was not killed mid-swap: %v\n%s", err, out)
	}
	if alive(up.Process.Pid) {
		t.Fatalf("the killed swapper (pid %d) still runs", up.Process.Pid)
	}
	h := r.health(t)
	if h.Commit != m.Commit || h.PID == pid0 || alive(pid0) {
		t.Fatalf("the new build should run unverified: commit %s pid %d (old %d alive %v)", h.Commit, h.PID, pid0, alive(pid0))
	}
	if _, held := swap.Holder(r.zmHome()); held {
		t.Fatal("swap.lock is still held after its swapper died")
	}

	tick := func() (string, string) {
		o, e, _ := r.runBin(os.Args[0], "daemon", "update", "--release", dir, "--dir", r.inst, "--key", key)
		st, have, err := release.LoadState(r.zmHome())
		if err != nil {
			t.Fatal(err)
		}
		if have || release.SameCommit(st.Commit, m.Commit) || slices.Contains(st.Stable, m.Version) {
			t.Fatalf("the unverified release was recorded as installed or stable: %+v\n%s\n%s", st, o, e)
		}
		if _, failed := st.FailedHere(m.Version); !failed {
			t.Fatalf("the unverified release is not failed here: %+v\n%s\n%s", st, o, e)
		}
		return o, e
	}
	if o, e := tick(); !strings.Contains(o, `"result":"failed"`) || !strings.Contains(o, "never finished") {
		t.Fatalf("first tick after the kill: %s\n%s", o, e)
	}
	if _, ok, _ := swap.ReadPending(r.zmHome()); ok {
		t.Fatal("the judged marker is still there")
	}
	if o, e := tick(); !strings.Contains(o, `"result":"unchanged"`) || !strings.Contains(o, "failed here") {
		t.Fatalf("second tick: %s\n%s", o, e)
	}
	if p := r.daemonPID(); p != h.PID {
		t.Fatalf("a tick touched the daemon: pid %d -> %d", h.PID, p)
	}
}

// TestUnfinishedSwapHeldWhileASwapperLives: a marker whose swapper still
// holds swap.lock is a swap in progress: the tick holds and leaves it.
func TestUnfinishedSwapHeldWhileASwapperLives(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	var rec api.SwapRecord
	if stop, err := unfinishedSwap(home, true, &rec); stop || err != nil {
		t.Fatalf("no marker: stop=%v %v", stop, err)
	}
	l, err := swap.Acquire(home, api.SwapHolder{By: "zero/aaaa1111", What: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	p := swap.Unfinished{What: "upgrade", To: "main-1eacf23e722f (" + relCommit + ")", PID: os.Getpid(), At: time.Now().UTC()}
	if err := swap.WritePending(home, p); err != nil {
		t.Fatal(err)
	}
	if stop, err := unfinishedSwap(home, true, &rec); !stop || err != nil || rec.Result != "held" {
		t.Fatalf("live swap: stop=%v %v %+v", stop, err, rec)
	}
	if _, ok, _ := swap.ReadPending(home); !ok {
		t.Fatal("a live swap's marker was dropped")
	}
	l.Release()
	if stop, err := unfinishedSwap(home, true, &rec); !stop || err == nil || !strings.Contains(err.Error(), "never finished") {
		t.Fatalf("dead swapper: stop=%v %v", stop, err)
	}
	if st, _, _ := release.LoadState(home); len(st.Failed) != 1 || st.Failed[0].Version != "main-1eacf23e722f" {
		t.Fatalf("failed here: %+v", st)
	}
}

// TestUpgradePlaceFailsLeavesNoMarker: an upgrade whose files cannot be
// placed (an unwritable install dir: EACCES, as ENOSPC or a failed rename
// would) puts everything back and changed nothing, with a daemon running
// and with none. It leaves no swap-pending.json, so the next tick never
// marks that release failed here.
func TestUpgradePlaceFailsLeavesNoMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes an unwritable directory")
	}
	r := newSwapRig(t)
	pid0 := r.daemonPID()
	staged := r.stage(t)
	if err := os.Chmod(r.inst, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.inst, 0o755) })
	noMarker := func(when string) {
		t.Helper()
		if p, ok, err := swap.ReadPending(r.zmHome()); ok || err != nil {
			t.Fatalf("%s: a failed placement left its marker %+v (%v)", when, p, err)
		}
		var rec api.SwapRecord
		if stop, err := unfinishedSwap(r.zmHome(), true, &rec); stop || err != nil {
			t.Fatalf("%s: the next tick stops: %v %+v", when, err, rec)
		}
		if st, _, err := release.LoadState(r.zmHome()); err != nil || len(st.Failed) > 0 {
			t.Fatalf("%s: failed here: %+v (%v)", when, st, err)
		}
	}

	if err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: staged}); err == nil {
		t.Fatal("an upgrade into an unwritable dir succeeded")
	}
	if p := r.daemonPID(); p != pid0 || fileSum(t, r.zm) != r.sum {
		t.Fatalf("a failed placement touched the daemon or its files: pid %d -> %d", pid0, p)
	}
	noMarker("daemon running")

	if _, e, code := r.runBin(r.zm, "daemon", "stop"); code != 0 {
		t.Fatalf("stop: %s", e)
	}
	if err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: staged, dir: r.inst}); err == nil {
		t.Fatal("an install into an unwritable dir succeeded")
	}
	if fileSum(t, r.zm) != r.sum {
		t.Fatal("a failed install changed zm")
	}
	noMarker("no daemon")
}

// TestUnfinishedSwapRecordedBeforeKill: a swapper SIGKILLed after it
// recorded its end (swap-last.json) but before it removed the marker
// finished its swap: the tick drops the marker and goes on. A record older
// than the marker is another swap's: the marker's is still unfinished.
func TestUnfinishedSwapRecordedBeforeKill(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	at := time.Now().UTC().Add(-time.Minute)
	p := swap.Unfinished{What: "upgrade", To: "main-1eacf23e722f (" + relCommit + ")", PID: 1, At: at}
	last := func(result string, when time.Time) {
		b, _ := json.Marshal(api.SwapRecord{What: "upgrade", Result: result, At: when, To: p.To})
		if err := os.WriteFile(filepath.Join(home, server.FileLastSwap), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, result := range []string{"ok", "rolled_back", "rolled_forward"} {
		if err := swap.WritePending(home, p); err != nil {
			t.Fatal(err)
		}
		last(result, at.Add(time.Second))
		var rec api.SwapRecord
		if stop, err := unfinishedSwap(home, true, &rec); stop || err != nil {
			t.Fatalf("%s recorded before the kill: stop=%v %v", result, stop, err)
		}
		if _, ok, _ := swap.ReadPending(home); ok {
			t.Fatalf("%s: the finished swap's marker is still there", result)
		}
		if st, _, _ := release.LoadState(home); len(st.Failed) > 0 {
			t.Fatalf("%s: a verified swap judged failed: %+v", result, st)
		}
	}
	if err := swap.WritePending(home, p); err != nil {
		t.Fatal(err)
	}
	last("ok", at.Add(-time.Second))
	var rec api.SwapRecord
	if stop, err := unfinishedSwap(home, true, &rec); !stop || err == nil || !strings.Contains(err.Error(), "never finished") {
		t.Fatalf("an older record: stop=%v %v", stop, err)
	}
	// A marker with no version (a build without one) is reported, never
	// recorded as a failure with an empty version.
	if err := os.Remove(release.StatePath(home)); err != nil {
		t.Fatal(err)
	}
	p.To = ""
	if err := swap.WritePending(home, p); err != nil {
		t.Fatal(err)
	}
	if stop, err := unfinishedSwap(home, true, &rec); !stop || err == nil {
		t.Fatalf("no version: stop=%v %v", stop, err)
	}
	if st, _, _ := release.LoadState(home); len(st.Failed) > 0 {
		t.Fatalf("a failure with no version: %+v", st)
	}
}

// TestForgiveFailureAfterVerifiedUpgrade: a release failed here is no
// longer failed once an upgrade to that exact build ends ok after the
// failure (a hand zm daemon upgrade that verified).
func TestForgiveFailureAfterVerifiedUpgrade(t *testing.T) {
	v, at := "main-1eacf23e722f", time.Now().UTC().Add(-time.Hour)
	h := func(result string, when time.Time, to string) api.Health {
		return api.Health{Version: v, Commit: relCommit, LastSwap: &api.SwapRecord{What: "upgrade", Result: result, At: when, To: to}}
	}
	same := v + " (" + relCommit + ")"
	for _, c := range []struct {
		name string
		h    api.Health
		want bool
	}{
		{"ok after", h("ok", at.Add(time.Minute), same), true},
		{"ok before", h("ok", at.Add(-time.Minute), same), false},
		{"rolled back after", h("rolled_back", at.Add(time.Minute), same), false},
		{"another build", h("ok", at.Add(time.Minute), "main-000000000000 (0000000000000000000000000000000000000000)"), false},
		{"no swap", api.Health{Version: v, Commit: relCommit}, false},
	} {
		st := release.State{}
		st.MarkFailed(release.Failure{Version: v, At: at, Result: "failed"})
		if got := forgiveFailure(&st, c.h); got != c.want {
			t.Errorf("%s: forgiven %v, want %v", c.name, got, c.want)
		}
		if _, failed := st.FailedHere(v); failed == c.want {
			t.Errorf("%s: failed here %v", c.name, failed)
		}
	}
}

// swapProbe is swap.Probe with no extra parts, in the shape of the probe var
// (Potluck's Probe takes variadic parts; a method value of it does not assign).
func swapProbe(ctx context.Context, c *client.Client) api.ServeCheck {
	return swap.Probe(ctx, c, wiring.Probes...)
}
