package main

// Safe swaps (SPEC 2, "Swaps"): zm daemon preflight, stop, restart and
// upgrade. One swap at a time (swap.lock); the binary that will run next
// is preflighted against the running daemon; an upgrade verifies the new
// daemon and rolls back to the previous binary when it fails.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
)

// Swap timings (tests shorten them).
var (
	swapStartWait = 15 * time.Second // a new daemon must answer health within this
	swapStopWait  = 10 * time.Second // an old one must exit within this
	// swapSupervisorStopWait bounds a service manager's stop (systemd's
	// default TimeoutStopSec).
	swapSupervisorStopWait = 90 * time.Second
	// preflightTimeout bounds a binary's preflight: a hung one must not
	// hold swap.lock for ever.
	preflightTimeout = 60 * time.Second
	// swapRemoteGrace bounds how long verify waits for regressions that can
	// be the tailnet still coming up (swap.SplitRegressions: a joined
	// project's reads, which go to its authority). A daemon's tsnet node
	// takes 30-60 s after a start (2026-11-03). A local regression refuses
	// at once, unless every one fails `unavailable` (swapLocalGrace).
	swapRemoteGrace = 90 * time.Second
	swapRemoteRetry = 2 * time.Second // between probes within the grace
	// swapLocalGrace bounds the wait for local regressions that all fail
	// `unavailable`: a store still busy at the new daemon's first start
	// (2026-11-17 on dev-05: the native tracker failed while the bd mirror's
	// first sync held its database, SQLITE_BUSY, and answered seconds later;
	// the swap had already rolled forward on a one-way migration and exited
	// 1). Missing items and any other code still refuse at once.
	swapLocalGrace = 30 * time.Second
	// swapCallWait bounds one health, session or record call: a daemon that
	// takes the connection and then wedges must not stop a wait's deadline.
	swapCallWait = 3 * time.Second
	// swapSettleWait bounds a health or manifest call whose answer decides
	// what the swap does (is a daemon running, which pid, which build): a
	// slow but healthy daemon must not read as gone. Only a refused or
	// missing socket is "not running"; a call that times out is unsure,
	// and the swap reports that and stops rather than act on a guess.
	swapSettleWait = 10 * time.Second
	// swapStopCallWait bounds one stop call (with --end-sessions it ends
	// every managed session before it answers).
	swapStopCallWait = 60 * time.Second
	// swapFinishBudget bounds a swap from its commit to its end: past the
	// commit it ignores SIGINT/SIGTERM, so this is the bound.
	// swapRollbackReserve of it is kept for a rollback: the forward steps
	// (stop, start or supervisor restart, verify) get the rest. A supervised
	// swap's worst case: the restart and waitNew 105 s each, serves 90 s
	// plus probes, then the rollback's settled health (10 s), supervisor
	// call (30 s), stop (60 + 10 s), restart and waitNew (105 s each):
	// 325 s (TestSwapperFinishWaitAboveBudget).
	swapFinishBudget    = 13 * time.Minute
	swapRollbackReserve = 5*time.Minute + 30*time.Second
)

// preflightLocal is `zm daemon preflight`: this build against the daemon
// running on cfg's home (or, with none, against its databases).
func preflightLocal(ctx context.Context, cfg *config.Config) (api.Preflight, error) {
	build, err := server.ThisBuild(ctx, wiring.Full)
	if err != nil {
		return api.Preflight{}, fmt.Errorf("describe this build: %w", err)
	}
	applied, aerr := swap.Applied(ctx, cfg.Home)
	var running *swap.Running
	c := client.NewUnix(cfg.SocketPath())
	if h, err := c.Health(ctx); err == nil {
		running = &swap.Running{Health: h}
		if m, err := c.Manifest(ctx); err == nil {
			running.Manifest = &m
		} else if !isNoRoute(err) {
			return api.Preflight{}, fmt.Errorf("the running daemon's manifest: %w", err)
		} else if rs, err := c.Remotes(ctx); err == nil {
			for _, r := range rs {
				if r.State == "" || r.State == "ok" {
					running.Joined = append(running.Joined, r.Project.Slug)
				}
			}
		}
	}
	pf := swap.Check(build, running, applied, swap.Databases(cfg.Home))
	if aerr != nil {
		pf.Problems = append(pf.Problems, "read the databases: "+aerr.Error())
		pf.OK = false
	}
	return pf, nil
}

// isNoRoute: a daemon older than the route answered 404.
func isNoRoute(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == api.CodeNotFound
}

// preflightBinary runs bin's own preflight against cfg's home. A binary
// without one (older than safe swaps) fails it.
func preflightBinary(ctx context.Context, cfg *config.Config, bin string) api.Preflight {
	if _, err := os.Stat(bin); err != nil {
		return api.Preflight{Build: bin, Problems: []string{fmt.Sprintf("%s: %v", bin, err)}}
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon", "preflight", "--json")
	cmd.Env = append(daemonEnv(os.Environ(), cfg.Home, true), EnvNoAutostart+"=1")
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return api.Preflight{Build: bin, Problems: []string{fmt.Sprintf("%s daemon preflight did not finish within %s", bin, preflightTimeout)}}
	}
	var pf api.Preflight
	if jerr := json.Unmarshal(out, &pf); jerr != nil || pf.Build == "" {
		msg := fmt.Sprintf("%s has no working `zm daemon preflight` (older than safe swaps, or broken)", bin)
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg += ": " + strings.TrimSpace(lastOf(string(ee.Stderr)))
		}
		return api.Preflight{Build: bin, Problems: []string{msg}}
	}
	return pf
}

func lastOf(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func printPreflight(pf api.Preflight, forced bool) {
	for _, n := range pf.Notes {
		fmt.Fprintln(os.Stderr, "note:", n)
	}
	for _, w := range pf.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if len(pf.Problems) == 0 {
		return
	}
	if forced {
		fmt.Fprintln(os.Stderr, "!!!!!!!! --force: overriding the preflight. This build may not serve what the daemon serves now:")
	} else {
		fmt.Fprintf(os.Stderr, "preflight refused %s:\n", pf.Build)
	}
	for _, p := range pf.Problems {
		fmt.Fprintln(os.Stderr, "  -", p)
	}
}

func daemonPreflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Can this zm take over from the running daemon? (exit 1 if not)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			pf, err := preflightLocal(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if flagJSON {
				_ = printJSON(pf)
			} else {
				printPreflight(pf, false)
				if pf.OK {
					fmt.Printf("preflight ok: %s can take over from %s\n", pf.Build, firstNonEmpty(pf.Running, "no running daemon"))
				}
			}
			if !pf.OK {
				return exitSilently(1)
			}
			return nil
		},
	}
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// swapOpts is one stop, restart or upgrade.
type swapOpts struct {
	what        string // stop | restart | upgrade
	newBin      string // upgrade: the staged zm
	dir         string // upgrade: where to install (default: the daemon's directory)
	force       bool   // override the preflight
	endSessions bool
}

// runSwap does it under swap.lock.
func runSwap(ctx context.Context, o swapOpts) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := client.NewUnix(cfg.SocketPath())
	if _, err := os.Stat(cfg.Home); err != nil {
		// No home: no daemon ever ran here, nothing to check against.
		if o.what != "upgrade" {
			return api.Errorf(api.CodeUnavailable, "zm daemon not running at %s", cfg.SocketPath())
		}
		// A first install (the README's make install on a new machine): the swap records its drivers' pins and
		// any hold in the home, so it makes the home, then goes on as on a home no daemon ever used.
		if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
			return err
		}
	}
	by := cfg.HumanAddress()
	if as := client.DefaultAs(cfg, os.Getenv); as != "" {
		by = as
	}
	if w, err := c.Whoami(ctx); err == nil && w.As != "" {
		by = w.As
	}
	lock, err := swap.Acquire(cfg.Home, api.SwapHolder{By: by, What: o.what})
	if err != nil {
		return err
	}
	defer lock.Release()
	settleLeftPins(ctx, cfg.Home, c, os.Stderr) // an earlier swapper died before it settled its drivers' pins
	s := &swapper{ctx: ctx, cfg: cfg, c: c, o: o, lock: lock}
	defer s.done()
	err = s.run()
	if err == nil && o.what == "upgrade" && liftAutoHold(cfg.Home) {
		fmt.Fprintln(os.Stderr, "zm: the driver folder's hold on updates is lifted: this upgrade landed")
	}
	return err
}

type swapper struct {
	ctx  context.Context
	cfg  *config.Config
	c    *client.Client
	o    swapOpts
	lock *swap.Lock

	health   *api.Health // the running daemon, nil when none answers
	manifest *api.DaemonManifest
	exe      string
	args     []string
	pf       api.Preflight
	rec      api.SwapRecord
	sup      *supervisor    // the service manager running the daemon, nil: none
	child    *swap.Detached // the daemon this swap started (its saved pid)
	unhook   func()         // stops commit's signal watch, nil: not committed
	pins     *swapPins      // an upgrade's driver pins plan, nil: it places no driver
	// driversBefore: the old daemon's run targets, described before the
	// upgrade placed anything (the verify's driver baseline).
	driversBefore []api.ServeItem

	base    context.Context // past commit: the signal context without its cancel
	end     time.Time       // past commit: swapFinishBudget from it
	cancels []context.CancelFunc
	rolling bool // rollingBack ran

	mu    sync.Mutex // guards step and since (the signal watch reads them)
	step  string     // what the swap waits on now
	since time.Time  // when step began
	// pending: commit wrote swap.FilePending (an upgrade under swap.lock);
	// untouched: the swap then put everything back as it was.
	pending, untouched bool
}

func healthWithin(ctx context.Context, c *client.Client) (api.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, swapCallWait)
	defer cancel()
	return c.Health(ctx)
}

// healthBy asks the daemon within a wait that ends at deadline (at least
// swapCallWait from now): a start or supervisor wait's own deadline, so a
// slow but healthy daemon still comes up, and a wedged one still ends it.
func (s *swapper) healthBy(deadline time.Time) (api.Health, error) {
	if min := time.Now().Add(swapCallWait); deadline.Before(min) {
		deadline = min
	}
	ctx, cancel := context.WithDeadline(s.ctx, deadline)
	defer cancel()
	return s.c.Health(ctx)
}

// settledHealth asks the daemon within swapSettleWait, for a decision.
// running is false only when nothing listens on the socket (refused, or
// no socket). Any other failure (a timeout, a cut-off) is unsure: err
// says so, and the caller reports and stops.
func (s *swapper) settledHealth() (h orchapi.Health, running bool, err error) {
	ctx, cancel := context.WithTimeout(s.ctx, swapSettleWait)
	defer cancel()
	h, err = orchclient.Health(ctx, s.c)
	switch {
	case err == nil:
		return h, true, nil
	case client.NotRunning(err):
		return orchapi.Health{}, false, nil
	}
	return orchapi.Health{}, true, s.unsure("its health check", err)
}

// unsure is a call that neither answered within swapSettleWait nor found
// the socket refused or missing.
func (s *swapper) unsure(what string, err error) error {
	if s.ctx.Err() != nil {
		return s.outOfTime(s.ctx.Err())
	}
	return fmt.Errorf("the daemon at %s neither answered %s within %s nor refused the connection (%v): not sure it runs, so not acting on a guess",
		s.cfg.SocketPath(), what, swapSettleWait, err)
}

// runs reports whether the daemon answering health is pid; err when it
// cannot tell (settledHealth).
func (s *swapper) runs(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	h, up, err := s.settledHealth()
	if err != nil {
		return false, err
	}
	return up && h.PID == pid, nil
}

// stopDaemon calls the stop route within swapStopCallWait.
func (s *swapper) stopDaemon(req api.DaemonStopRequest) (api.DaemonStopResponse, error) {
	ctx, cancel := context.WithTimeout(s.ctx, swapStopCallWait)
	defer cancel()
	return s.c.StopDaemon(ctx, req)
}

func (s *swapper) nonce() string {
	if s.lock == nil {
		return ""
	}
	return s.lock.Holder.Nonce
}

func (s *swapper) run() error {
	ctx, o := s.ctx, s.o
	h, up, err := s.settledHealth()
	if err != nil {
		return fmt.Errorf("%w: not swapping", err)
	}
	if up {
		s.health = &h.Health
		mctx, cancel := context.WithTimeout(ctx, swapSettleWait)
		m, err := s.c.Manifest(mctx)
		cancel()
		var ae *api.Error
		switch {
		case err == nil:
			s.manifest = &m
		case errors.As(err, &ae) && ae.HTTPStatus != 0:
			// It answered without one: a build older than manifests.
		default:
			return fmt.Errorf("%w: not swapping", s.unsure("its manifest", err))
		}
	}
	s.exe, s.args = s.daemonExe()
	s.rec = api.SwapRecord{What: o.what, Exe: s.exe}
	if s.health != nil && o.what != "stop" {
		sup, err := detectSupervisor(ctx, s.health.PID)
		if err == nil && sup == nil {
			// A daemon beside a loaded job: the swap hands the home back to the job.
			sup, err = detectJobBeside(ctx, s.exe, s.cfg.Home)
		}
		if err != nil {
			return fmt.Errorf("%w: not swapping (zm would start a second daemon beside it)", err)
		}
		s.sup = sup
	}
	if s.health != nil {
		s.rec.From = buildOf(*s.health)
	}

	// What will run next: the staged build, else the binary on disk (for a
	// stop whose daemon's file is gone, this CLI: the next autostart's).
	target := s.exe
	if _, err := os.Stat(target); err != nil && o.what == "stop" {
		target, _ = os.Executable()
	}
	if o.what == "upgrade" {
		abs, err := filepath.Abs(o.newBin)
		if err != nil {
			return err
		}
		target = abs
	}
	if s.health != nil || o.what == "upgrade" {
		s.pf = preflightBinary(ctx, s.cfg, target)
		if o.what == "upgrade" && s.pf.Build != "" {
			s.checkDriverFolder(target)
		}
		forced := o.force && !s.pf.OK
		printPreflight(s.pf, forced)
		if !s.pf.OK && !o.force {
			return exitSilently(1)
		}
		if forced {
			s.rec.Forced, s.rec.Overridden = true, s.pf.Problems
		}
	}
	if s.health == nil && o.what == "stop" {
		return api.Errorf(api.CodeUnavailable, "the daemon is not running")
	}

	switch o.what {
	case "stop":
		if err := s.dryStop(); err != nil {
			return err
		}
		if err := s.commit(); err != nil {
			return err
		}
		s.record() // before: the record lives on in swap-last.json
		r, err := s.stop()
		if err != nil {
			return err
		}
		switch {
		case r.Ended:
			fmt.Printf("zm daemon stopped (%d managed sessions ended)\n", r.ManagedSessions)
		case r.ManagedSessions > 0:
			fmt.Printf("zm daemon stopped (%d managed sessions keep running; the next daemon adopts them)\n", r.ManagedSessions)
		default:
			fmt.Println("zm daemon stopped")
		}
		return nil
	case "restart":
		return s.restart(nil)
	default:
		return s.upgrade(target)
	}
}

// daemonExe is how the daemon runs: its manifest, else this home's
// default (the CLI's own binary) with plain `daemon`.
func (s *swapper) daemonExe() (string, []string) {
	if m := s.manifest; m != nil && m.Exe != "" {
		args := m.Args
		if len(args) == 0 || args[0] != "daemon" {
			args = []string{"daemon"}
		}
		return m.Exe, args
	}
	// A daemon older than manifests: its command line, from ps.
	if s.health != nil && s.health.PID > 0 {
		if out, err := exec.Command("ps", "-o", "args=", "-p", fmt.Sprint(s.health.PID)).Output(); err == nil {
			f := strings.Fields(string(out))
			if len(f) >= 2 && f[1] == "daemon" && filepath.IsAbs(f[0]) {
				if _, err := os.Stat(f[0]); err == nil {
					return f[0], f[1:]
				}
			}
		}
	}
	self, _ := os.Executable()
	if s.o.what == "upgrade" && s.o.dir != "" {
		self = filepath.Join(s.o.dir, "zm")
	}
	return self, []string{"daemon"}
}

// installDir is where an upgrade places its files: --dir, else the
// daemon's directory.
func (s *swapper) installDir() string {
	dir := s.o.dir
	if dir == "" {
		dir = filepath.Dir(s.exe)
	}
	dir, _ = filepath.Abs(dir)
	return dir
}

func (s *swapper) upgrade(target string) error {
	src := filepath.Dir(target)
	dir := s.installDir()
	if src == dir {
		return fmt.Errorf("%s is already in %s: stage the build in another directory (make install does)", target, dir)
	}
	names, err := swap.Installable(src)
	if err != nil {
		return err
	}
	// A supervisor restarts the binary its unit names: placing the build
	// anywhere else would swap nothing (and report success).
	if s.sup != nil {
		prog, err := supervisorProgram(s.ctx, s.sup)
		if err != nil {
			return fmt.Errorf("cannot tell which binary the %s runs (%w); nothing was placed", s.sup, err)
		}
		if prog == "" {
			prog = s.exe
		}
		if want := filepath.Join(dir, "zm"); !samePath(prog, want) {
			return fmt.Errorf("the %s runs %s, not %s; nothing was placed: install where it runs (make install INSTALL_DIR=%s), or point the unit at %s",
				s.sup, prog, want, filepath.Dir(prog), want)
		}
	}
	if s.sup != nil && s.sup.Kind == "launchd" {
		if err := prepareOwnedLaunchAgent(s.ctx, s.sup, filepath.Join(dir, "zm"), s.cfg.Home); err != nil {
			return err
		}
	}
	defer s.settlePins()
	if s.health == nil {
		if err := s.commit(); err != nil {
			return err
		}
		if err := s.planPins(src, dir, names, false); err != nil {
			s.untouched = true
			return err
		}
		placed, err := swap.Place(src, dir, names)
		if err != nil {
			s.untouched = !errors.Is(err, swap.ErrNotRestored) // its files are back: the marker goes
			return err
		}
		s.pinsTo(true)
		s.rec.To, s.rec.Result = s.pf.Build, "ok"
		s.rec.Reason = "no daemon was running: installed only"
		s.record()
		fmt.Printf("installed %s into %s (previous files kept as *.prev); no daemon was running\n", strings.Join(placed.Names, ", "), dir)
		return nil
	}
	if err := s.dryStop(); err != nil {
		return err
	}
	if err := s.commit(); err != nil {
		return err
	}
	restarts := runsFrom(s.exe, dir) // before Place: it may replace a symlinked zm
	if restarts && placedDrivers(names) {
		s.stepTo("asking the old daemon to describe its run targets")
		s.driversBefore = driverProbe(s.ctx, s.c).Items
	}
	if err := s.planPins(src, dir, names, restarts); err != nil {
		s.untouched = true
		return err
	}
	s.stepTo("placing the new files in %s", dir)
	placed, err := swap.Place(src, dir, names)
	if err != nil {
		s.untouched = !errors.Is(err, swap.ErrNotRestored) // the old daemon runs on its own files
		return err
	}
	fmt.Printf("placed %s in %s (previous files kept as *.prev)\n", strings.Join(placed.Names, ", "), dir)
	// The new daemon runs the drivers it brought during its verify; a
	// rollback pins the restored ones again (restart).
	s.pinsTo(true)
	if !restarts {
		s.rec.To, s.rec.Result = s.pf.Build, "ok"
		s.rec.Reason = "installed; the daemon runs " + s.exe + " and was not restarted"
		s.record()
		fmt.Println("the daemon runs", s.exe, "(not in", dir+"): not restarted")
		return nil
	}
	return s.restart(placed)
}

// runsFrom reports whether the daemon's executable exe lives in dir, its
// symlinks resolved: on macOS os.Executable keeps a symlink's path, while
// zm daemon update places the build where the link points.
func runsFrom(exe, dir string) bool {
	if filepath.Dir(exe) == dir {
		return true
	}
	r, err := filepath.EvalSymlinks(exe)
	return err == nil && samePath(filepath.Dir(r), dir)
}

// dryStop refuses early (before any file moves) what the stop would. Only
// a daemon with a manifest knows dry_run; an older one refuses at the real
// stop, and the files go back then.
func (s *swapper) dryStop() error {
	if s.manifest == nil {
		return nil
	}
	_, err := s.stopDaemon(api.DaemonStopRequest{Force: s.o.endSessions, Swap: s.nonce(), What: s.o.what, DryRun: true})
	return err
}

// probe is swap.Probe plus the task agents (tests replace it): verify
// treats an agent the old daemon could start and the new one cannot like
// any other regression, and rolls back.
var probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
	return wiring.ProbeAgents(ctx, c, swap.Probe(ctx, c, wiring.Probes...))
}

// restart replaces the running daemon (if any) with s.exe and verifies it:
// it answers, it is the build the preflight described, and it serves what
// the old one served. With placed files, a failure stops the rejected
// daemon, puts the previous files back and brings the previous binary up.
// Under a service manager every start goes through it (supervisor.go).
func (s *swapper) restart(placed *swap.Placed) error {
	if err := s.commit(); err != nil {
		return err
	}
	var before api.ServeCheck
	var appliedBefore map[string][]string // a daemon with a manifest has its schema there
	oldPID := 0
	if s.health != nil {
		oldPID = s.health.PID
		s.stepTo("probing what the old daemon (pid %d) serves", oldPID)
		before = probe(s.ctx, s.c)
		if placed != nil && s.manifest == nil {
			appliedBefore, _ = swap.Applied(s.ctx, s.cfg.Home)
		}
	}
	h, err := s.bringUp(oldPID, placed != nil)
	held := "" // the socket was held but did not answer (supervised: rolled back)
	if err != nil {
		s.rollingBack() // the forward steps' time may be up: what follows is the recovery's
	}
	if err != nil && s.health != nil {
		// Nothing was stopped (a refused stop or supervisor call), or the
		// old daemon is too slow to say: the previous files go back and
		// nothing else starts (a second daemon would be refused). Under a
		// service manager an unsure answer is no guess: the manager replaces
		// its own process (a wedged new daemon, say), so the rollback below
		// restarts through it on the previous files.
		still, uerr := s.runs(oldPID)
		if uerr != nil && s.sup != nil && placed != nil && s.ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "zm: %v; rolling back through the %s\n", uerr, s.sup)
			held = fmt.Sprintf("; the daemon at %s did not answer within %s", s.cfg.SocketPath(), swapSettleWait)
		} else if still || uerr != nil {
			if placed != nil {
				if rerr := placed.Restore(); rerr != nil {
					return fmt.Errorf("%w; putting the previous files back also failed: %v", err, rerr)
				}
				fmt.Fprintln(os.Stderr, "zm: the previous files are back")
				s.pinsTo(false)
			}
			if uerr != nil {
				s.rec.Result, s.rec.Reason = "failed", err.Error()+"; then "+uerr.Error()
				s.recordLog()
				return fmt.Errorf("%w; then %v (see zm daemon status)", err, uerr)
			}
			fmt.Fprintln(os.Stderr, "zm: the daemon still runs")
			s.untouched = true
			return err
		}
	}
	why := ""
	if err != nil {
		why = s.outOfTime(err).Error() + held
	} else {
		s.stepTo("verifying the new daemon (pid %d)", h.PID)
		why = s.verify(h, before)
	}
	if why == "" {
		s.rec.To, s.rec.Result = buildOf(h), "ok"
		s.record()
		fmt.Printf("zm daemon %s: %s -> %s (pid %d)\n", map[string]string{"restart": "restarted", "upgrade": "upgraded"}[s.o.what],
			firstNonEmpty(s.rec.From, "not running"), s.rec.To, h.PID)
		return nil
	}
	fmt.Fprintln(os.Stderr, "zm: the new daemon failed:", why)
	s.rollingBack()
	s.rec.Reason = why
	answered := err == nil
	if placed == nil {
		s.rec.Result = "failed"
		if answered {
			s.record()
			return fmt.Errorf("the restarted daemon (pid %d) runs, but %s (no previous binary to roll back to; see %s)", h.PID, why, s.cfg.Path(config.FileLog))
		}
		s.recordLog()
		return fmt.Errorf("the daemon is down: %s (no previous binary to roll back to; see %s)", why, s.cfg.Path(config.FileLog))
	}
	// The new build may have migrated the databases before it failed: the
	// previous build would then run on data newer than it, worse than
	// either (2026-11-03). A new daemon that answers stays; one that does
	// not is rolled back anyway (the previous build serving some is better
	// than none), loudly.
	newer := s.dataNewer(appliedBefore)
	if answered && len(newer) > 0 {
		return s.rollForward(h, why, newer)
	}
	// Stop the rejected daemon before the previous binary starts: a second
	// daemon on this home would be refused, and this one must not stay.
	if answered && s.sup == nil {
		s.stepTo("rolling back: stopping the rejected daemon (pid %d)", h.PID)
		if err := s.stopRejected(h.PID); err != nil {
			s.rec.Result = "failed"
			s.recordLog()
			return fmt.Errorf("the rejected daemon (pid %d) did not stop: %w; it still runs, on the new files", h.PID, err)
		}
	}
	s.stepTo("rolling back: putting the previous files back")
	if err := placed.Restore(); err != nil {
		s.rec.Result = "failed"
		s.recordLog()
		if answered && s.sup != nil {
			return fmt.Errorf("putting the previous files back failed: %w; the rejected daemon (pid %d) still runs", err, h.PID)
		}
		return fmt.Errorf("putting the previous files back failed: %w; the daemon is down", err)
	}
	s.pinsTo(false) // before it starts: the previous daemon runs the drivers that came back
	fmt.Fprintln(os.Stderr, "zm: rolled back to the previous binary; starting it")
	rejected := 0
	if answered {
		rejected = h.PID
	}
	s.stepTo("rolling back: starting the previous binary")
	h2, err := s.bringUp(rejected, true) // a rollback is not refused for sessions
	if err != nil {
		s.rec.Result = "failed"
		s.recordLog()
		still, uerr := s.runs(rejected)
		if uerr != nil {
			return fmt.Errorf("rolled back the files, but %w; then %v", s.outOfTime(err), uerr)
		}
		if still {
			return fmt.Errorf("rolled back the files, but %w; the rejected daemon (pid %d) still runs", s.outOfTime(err), rejected)
		}
		return fmt.Errorf("rolled back the files, but the previous binary did not start: %w; the daemon is down", s.outOfTime(err))
	}
	s.rec.To, s.rec.Result = buildOf(h2), "rolled_back"
	s.record()
	if regs := swap.Regressions(before, probe(s.ctx, s.c)); len(regs) > 0 {
		fmt.Fprintln(os.Stderr, "zm: after the rollback these still fail:\n  "+strings.Join(regs, "\n  "))
	}
	if len(newer) > 0 {
		fmt.Fprintln(os.Stderr, "!!!!!!!! zm: the previous build runs on data newer than it (the new build migrated it, then never answered):\n  "+
			strings.Join(newer, "\n  ")+"\nzm: install a build that knows these migrations (make install FORCE=1)")
	}
	return exitWith{code: 1, err: fmt.Errorf("%s refused: %s; rolled back to %s (pid %d)", s.o.what, why, s.rec.To, h2.PID)}
}

// rollForward keeps the new daemon h that failed its verify because the
// previous build cannot read the data it migrated (newer). The placed
// files stay, each previous one as <name>.prev; the record says
// rolled_forward and the swap exits 1.
func (s *swapper) rollForward(h api.Health, why string, newer []string) error {
	s.rec.To, s.rec.Result = buildOf(h), "rolled_forward"
	s.rec.Reason = why + "; not rolled back: " + strings.Join(newer, "; ")
	s.record()
	fmt.Fprintf(os.Stderr, "!!!!!!!! zm: NOT rolled back: the new build migrated this home's data and the previous build cannot read it:\n  %s\n"+
		"zm: the new daemon keeps running (pid %d); fix forward with a newer build (the previous files are kept as *%s)\n",
		strings.Join(newer, "\n  "), h.PID, swap.PrevSuffix)
	return exitWith{code: 1, err: fmt.Errorf("%s verify failed: %s; not rolled back, the data is newer than the previous build (%s): %s keeps running (pid %d)",
		s.o.what, why, strings.Join(newer, "; "), s.rec.To, h.PID)}
}

// bringUp replaces the daemon running as pid (0: none) with s.exe and
// waits until a daemon other than pid answers: through the supervisor
// when there is one, else by stopping pid and starting s.exe ourselves.
func (s *swapper) bringUp(pid int, guarded bool) (api.Health, error) {
	if s.sup != nil {
		if pid > 0 && !guarded {
			if err := s.guard(); err != nil {
				return api.Health{}, err
			}
		}
		// The manager replaces only its own process: a daemon beside it would
		// keep the home, and the manager's would exit "already runs".
		up, err := s.runs(pid)
		if err != nil {
			return api.Health{}, err
		}
		if up {
			main, err := supervisorMain(s.ctx, s.sup)
			if err != nil {
				return api.Health{}, fmt.Errorf("cannot tell which process the %s runs: %w", s.sup, err)
			}
			if main != pid {
				fmt.Fprintf(os.Stderr, "zm: the daemon (pid %d) runs beside the %s: stopping it first\n", pid, s.sup)
				if s.health != nil && pid == s.health.PID {
					_, err = s.stop()
				} else {
					err = s.stopRejected(pid)
				}
				if err != nil {
					return api.Health{}, err
				}
			}
		}
		fmt.Fprintf(os.Stderr, "zm: restarting through the %s\n", s.sup)
		s.stepTo("restarting the daemon through the %s", s.sup)
		if err := supervisorRestart(s.ctx, s.sup); err != nil {
			return api.Health{}, err
		}
		return s.waitNew(pid)
	}
	up, err := s.runs(pid)
	if err != nil {
		return api.Health{}, err
	}
	if up {
		if _, err := s.stop(); err != nil {
			return api.Health{}, err
		}
	}
	return s.start()
}

// guard refuses what stopping the daemon would (sessions, another swap)
// when a supervisor, not the stop route, is about to stop it.
func (s *swapper) guard() error {
	if s.manifest != nil {
		return s.dryStop()
	}
	if s.o.endSessions {
		return nil
	}
	sctx, cancel := context.WithTimeout(s.ctx, swapCallWait)
	defer cancel()
	ss, err := orchclient.Sessions(sctx, s.c, orchapi.SessionQuery{})
	if err != nil {
		return fmt.Errorf("cannot list the sessions a restart would end: %w", err)
	}
	var live []string
	for _, x := range ss {
		if x.Managed && x.Machine == s.cfg.Machine && x.State != orchapi.StateEnded {
			live = append(live, x.Address+" ("+x.Agent+")")
		}
	}
	if len(live) > 0 {
		return api.Errorf(api.CodeConflict, "%d managed sessions are running; restarting the daemon ends them: %s", len(live), strings.Join(live, ", ")).
			WithHint("stop them first (zm session stop), or pass --end-sessions")
	}
	return nil
}

// waitNew waits for a daemon other than pid to answer (a supervisor's).
// The start wait counts from pid's exit: the manager stops it first, and
// a slow exit (launchd took 15 s) must not use up the new daemon's time.
func (s *swapper) waitNew(pid int) (api.Health, error) {
	s.stepTo("waiting for the %s's new daemon to answer (within %s of the old one's exit)", s.sup, swapStartWait)
	stop, deadline := time.Now().Add(swapSupervisorStopWait), time.Now().Add(swapStartWait)
	for time.Now().Before(deadline) {
		if h, err := s.healthBy(deadline); err == nil && h.PID > 0 && h.PID != pid {
			return h, nil
		}
		if pid > 0 && syscall.Kill(pid, 0) == nil { // the old one still exits
			if time.Now().After(stop) {
				return api.Health{}, fmt.Errorf("the %s did not stop the daemon (pid %d) within %s", s.sup, pid, swapSupervisorStopWait)
			}
			deadline = time.Now().Add(swapStartWait)
		}
		select {
		case <-s.ctx.Done():
			return api.Health{}, s.outOfTime(s.ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return api.Health{}, fmt.Errorf("the %s brought no new daemon up within %s", s.sup, swapStartWait)
}

// verify is "" when h is the build the preflight described, serves what
// before did and can reach its sessions, else why not. Every path (a daemon
// this swap started, or a supervisor's) comes through here.
func (s *swapper) verify(h api.Health, before api.ServeCheck) string {
	if want := s.pf.Build; want != "" && s.pf.OK && want != buildOf(h) && !s.rec.Forced {
		return fmt.Sprintf("the daemon reports %s, the preflight described %s", buildOf(h), want)
	}
	if why := s.serves(before); why != "" {
		return why
	}
	// h is too early to say: GET /v0/health skips the start gate, so it can
	// answer before the session module adopts anything. The probe's other
	// calls waited for the modules to start: ask again now.
	now, up, err := s.settledHealth()
	switch {
	case err != nil:
		return "it did not answer its health check within " + swapSettleWait.String()
	case !up:
		return "it stopped answering its health check"
	}
	return holdersProblem(now)
}

// dataNewer lists the migrations now applied on this home that the build
// a rollback would start does not know: the running daemon's schema from
// its manifest, else (a daemon older than manifests) what was applied
// since before in files that existed then. Non-empty: a rollback would
// leave that build on data newer than it.
func (s *swapper) dataNewer(before map[string][]string) []string {
	after, _ := swap.Applied(s.ctx, s.cfg.Home) // a file it cannot read: judge the rest
	if s.manifest != nil && len(s.manifest.Build.Schema) > 0 {
		return swap.Newer(s.manifest.Build.Schema, after, swap.Databases(s.cfg.Home))
	}
	if before == nil {
		return nil
	}
	var out []string
	for f, names := range after {
		had, ok := before[f]
		if !ok {
			continue
		}
		old := map[string]bool{}
		for _, n := range had {
			old[n] = true
		}
		for _, n := range names {
			if !old[n] {
				out = append(out, fmt.Sprintf("%s has migration %s, applied by the new build", f, n))
			}
		}
	}
	sort.Strings(out)
	return out
}

// holdersProblem is why a new daemon must not stay: sessions alive in
// their holders that this build cannot adopt (another holder protocol
// major; SPEC 8). Rolling back gives them a daemon that can.
func holdersProblem(h orchapi.Health) string {
	if h.Holders == nil || len(h.Holders.Unreachable) == 0 {
		return ""
	}
	ids := make([]string, 0, len(h.Holders.Unreachable))
	for _, u := range h.Holders.Unreachable {
		ids = append(ids, u.Session)
	}
	return fmt.Sprintf("%d sessions are alive but this build cannot reach their holders (%s): %s",
		len(ids), strings.Join(ids, ", "), h.Holders.Unreachable[0].Why)
}

// stopRejected stops a daemon this swap started and then rejected: by the
// stop route as the lock's holder, else by its saved pid (TERM, then KILL).
func (s *swapper) stopRejected(pid int) error {
	_, _ = s.stopDaemon(api.DaemonStopRequest{Swap: s.nonce(), What: "rollback"})
	if waitGone(s.ctx, s.c, pid, swapStopWait) {
		return nil
	}
	if s.child == nil || s.child.Pid != pid {
		return fmt.Errorf("pid %d is not a daemon this swap started", pid)
	}
	_ = s.child.Signal(syscall.SIGTERM)
	if waitGone(s.ctx, s.c, pid, 5*time.Second) {
		return nil
	}
	_ = s.child.Signal(syscall.SIGKILL)
	if waitGone(s.ctx, s.c, pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d did not exit", pid)
}

// stop asks the running daemon to stop as the lock's holder and waits
// for its process to exit.
func (s *swapper) stop() (api.DaemonStopResponse, error) {
	pid := 0
	if s.health != nil {
		pid = s.health.PID
	}
	req := api.DaemonStopRequest{Force: s.o.endSessions, Swap: s.nonce(), What: s.o.what}
	if s.manifest == nil {
		req = api.DaemonStopRequest{Force: s.o.endSessions} // an older daemon refuses fields it does not know
	}
	s.stepTo("stopping the daemon (pid %d)", pid)
	r, err := s.stopDaemon(req)
	if err != nil {
		return r, err
	}
	for _, f := range r.NotEnded {
		fmt.Printf("session %s was not ended: %s\n", f.Session, f.Reason)
	}
	if !waitGone(s.ctx, s.c, pid, swapStopWait) {
		return r, fmt.Errorf("the daemon (pid %d) did not exit within %s", pid, swapStopWait)
	}
	return r, nil
}

// waitGone waits until the daemon stops answering and pid has exited.
func waitGone(ctx context.Context, c *client.Client, pid int, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		_, herr := healthWithin(ctx, c)
		if herr != nil && (pid <= 0 || syscall.Kill(pid, 0) != nil) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// start runs s.exe (s.args) detached and waits for it to answer. A daemon
// that does not answer in time is stopped by its pid.
func (s *swapper) start() (api.Health, error) {
	p, err := swap.StartDetached(s.ctx, swap.Detach{Argv: append([]string{s.exe}, s.args...),
		Env: daemonEnv(os.Environ(), s.cfg.Home, true), Log: s.cfg.Path(config.FileLog), Home: s.cfg.Home})
	if err != nil {
		return api.Health{}, err
	}
	exited := p.Exited
	s.child = p
	s.stepTo("starting %s (pid %d, %s) and waiting up to %s for its health", s.exe, p.Pid, p.Via, swapStartWait)
	deadline := time.Now().Add(swapStartWait)
	for {
		if h, err := s.healthBy(deadline); err == nil && h.PID == p.Pid {
			return h, nil
		}
		select {
		case <-exited:
			return api.Health{}, fmt.Errorf("it exited: %s", lastLine(s.cfg.Path(config.FileLog)))
		case <-s.ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
		// Out of its start wait, or of the swap's budget: it does not stay.
		if late := time.Now().After(deadline); late || s.ctx.Err() != nil {
			_ = p.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				_ = p.Signal(syscall.SIGKILL)
				<-exited
			}
			if !late {
				return api.Health{}, fmt.Errorf("it did not answer: %w", s.outOfTime(s.ctx.Err()))
			}
			return api.Health{}, fmt.Errorf("it did not answer within %s", swapStartWait)
		}
	}
}

// record sends the swap record to the daemon now running (it logs it,
// publishes dev.zm.daemon.swapped and keeps swap-last.json); a daemon
// without the route, or none, gets the line in daemon.log instead.
func (s *swapper) record() {
	// Even past the budget: the record is the report.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), swapCallWait)
	defer cancel()
	if _, err := s.c.RecordSwap(ctx, s.rec); err == nil {
		return
	}
	s.recordLog()
}

func (s *swapper) recordLog() {
	if s.rec.At.IsZero() { // the daemon stamps the records it takes
		s.rec.At = time.Now().UTC()
	}
	b, _ := json.Marshal(s.rec)
	f, err := os.OpenFile(s.cfg.Path(config.FileLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "time=%s level=WARN msg=\"daemon swap\" record=%s\n", time.Now().UTC().Format(time.RFC3339), b)
}

func buildOf(h api.Health) string {
	if h.Commit == "" {
		return h.Version
	}
	return h.Version + " (" + h.Commit + ")"
}

func daemonStopCmd() *cobra.Command {
	var force, endSessions bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon; managed sessions keep running in their holders (refuses during another swap, or when the zm on disk cannot take over)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSwap(cmd.Context(), swapOpts{what: "stop", force: force, endSessions: force || endSessions})
		},
	}
	stop.Flags().BoolVar(&force, "force", false, "stop anyway: end managed sessions and override the preflight of the zm that starts next")
	stop.Flags().BoolVar(&endSessions, "end-sessions", false, "also end every managed session (outcome infra)")
	return stop
}

func daemonRestartCmd() *cobra.Command {
	var force, endSessions bool
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon with the zm it runs from, after its preflight; verify it serves what it served",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSwap(cmd.Context(), swapOpts{what: "restart", force: force, endSessions: endSessions})
		},
	}
	c.Flags().BoolVar(&force, "force", false, "restart even when the preflight refuses (loud; recorded)")
	c.Flags().BoolVar(&endSessions, "end-sessions", false, "restart even while managed sessions run (they end)")
	return c
}

func daemonUpgradeCmd() *cobra.Command {
	var force, endSessions bool
	var dir string
	c := &cobra.Command{
		Use:   "upgrade <zm binary>",
		Short: "Install a staged zm build and swap the daemon to it; roll back if it does not serve what it served",
		Long: `Runs the staged binary's preflight against the running daemon, places it (with bd-zm and the
zm-* files beside it) in --dir (default: the daemon's directory), keeping each previous file as
<name>.prev, restarts the daemon and checks it answers, is that build and serves what the old one
served. On a failure the previous files go back and the previous daemon starts again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSwap(cmd.Context(), swapOpts{what: "upgrade", newBin: args[0], dir: dir, force: force, endSessions: endSessions})
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "install directory (default: the running daemon's)")
	c.Flags().BoolVar(&force, "force", false, "swap even when the preflight refuses (loud; recorded)")
	c.Flags().BoolVar(&endSessions, "end-sessions", false, "swap even while managed sessions run (they end)")
	return c
}
