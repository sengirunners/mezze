package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/swap"
)

// fakeLaunchd is a loaded launchd job whose program is the rig's zm. Like
// `launchctl kickstart -k`, a restart TERMs only the job's own process (a
// daemon running beside the job keeps the home) and then starts the
// program; a program that finds the home taken exits 1 ("already runs").
type fakeLaunchd struct {
	r       *swapRig
	mu      sync.Mutex
	main    int           // the job's process, 0: none
	gap     time.Duration // the job's old process gone, its new one not started yet
	inGap   func()        // runs at the start of the gap
	late    time.Duration // >0: kickstart returns at once, the TERM comes this much later
	kicks   int
	running sync.WaitGroup
}

func newFakeLaunchd(t *testing.T, r *swapRig) *fakeLaunchd {
	j := &fakeLaunchd{r: r}
	oldPrepare := prepareOwnedLaunchAgent
	prepareOwnedLaunchAgent = func(context.Context, *supervisor, string, string) error { return nil }
	t.Cleanup(func() { prepareOwnedLaunchAgent = oldPrepare })
	supervisorRestart = func(context.Context, *supervisor) error {
		j.mu.Lock()
		j.kicks++
		late := j.late
		j.mu.Unlock()
		if late > 0 {
			j.running.Add(1)
			go func() { defer j.running.Done(); time.Sleep(late); j.kickstart(t) }()
			return nil
		}
		j.kickstart(t)
		return nil
	}
	supervisorProgram = func(context.Context, *supervisor) (string, error) { return r.zm, nil }
	supervisorMain = func(context.Context, *supervisor) (int, error) { return j.mainPID(), nil }
	t.Cleanup(func() {
		j.running.Wait()
		if pid := j.mainPID(); alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		detectSupervisor, supervisorRestart, supervisorProgram, probe = detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault, swapProbe
		supervisorMain, detectJobBeside = supervisorMainDefault, detectJobBesideDefault
	})
	return j
}

var supervisorMainDefault, detectJobBesideDefault = supervisorMain, detectJobBeside

var testJob = &supervisor{Kind: "launchd", Unit: "med.example.zm-daemon"}

func TestLaunchdJobParsing(t *testing.T) {
	list := "PID\tStatus\tLabel\n-\t1\tmed.example.zm-daemon\n4242\t0\tother\n"
	if pid, ok := jobPID(list, "med.example.zm-daemon"); pid != 0 || !ok {
		t.Fatalf("a loaded job between spawns: %d %v", pid, ok)
	}
	if pid, ok := jobPID(list, "other"); pid != 4242 || !ok {
		t.Fatalf("a running job: %d %v", pid, ok)
	}
	if _, ok := jobPID(list, "gone"); ok {
		t.Fatal("an unloaded job")
	}
	j := launchdJob{Label: "x", ProgramArguments: []string{"/u/.local/bin/zm", "daemon"}}
	if !j.runs("/u/.local/bin/zm", "/u/.zm", "/u") {
		t.Fatal("the default home's daemon")
	}
	if j.runs("/u/.local/bin/zm", "/u/other", "/u") || j.runs("/opt/zm", "/u/.zm", "/u") {
		t.Fatal("another home or another binary")
	}
	j.EnvironmentVariables = map[string]string{"ZM_HOME": "/u/other"}
	if !j.runs("/u/.local/bin/zm", "/u/other", "/u") {
		t.Fatal("the job's ZM_HOME")
	}
	if (launchdJob{ProgramArguments: []string{"/u/.local/bin/zm", "daemon", "stop"}}).runs("/u/.local/bin/zm", "/u/.zm", "/u") {
		t.Fatal("a job running `zm daemon stop` is not the daemon")
	}
	if !(launchdJob{ProgramArguments: []string{"/u/.local/bin/zm", "daemon", "--tailnet"}}).runs("/u/.local/bin/zm", "/u/.zm", "/u") {
		t.Fatal("daemon flags")
	}
}

func (j *fakeLaunchd) mainPID() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.main
}

func (j *fakeLaunchd) kickstart(t *testing.T) {
	if pid := j.mainPID(); alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 500 && alive(pid); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if j.inGap != nil {
		j.inGap()
	}
	time.Sleep(j.gap)
	c := exec.Command(j.r.zm, "daemon")
	c.Env = j.r.env
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Error(err)
		return
	}
	j.mu.Lock()
	j.main = c.Process.Pid
	j.mu.Unlock()
	go func() { _ = c.Wait() }()
}

// underJob makes the rig's daemon the job's process.
func (j *fakeLaunchd) underJob(t *testing.T) {
	t.Helper()
	if _, e, code := j.r.runBin(j.r.zm, "daemon", "stop"); code != 0 {
		t.Fatalf("stop: %s", e)
	}
	j.kickstart(t)
	j.waitJob(t)
}

func (j *fakeLaunchd) waitJob(t *testing.T) {
	t.Helper()
	c := client.NewUnix(j.r.cfgSocket())
	for i := 0; i < 500; i++ {
		if h, err := c.Health(context.Background()); err == nil && h.PID == j.mainPID() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job's daemon did not come up")
}

func (r *swapRig) cfgSocket() string { return r.zmHome() + "/zm.sock" }

// TestSupervisedUpgradeAutostartLosesNoGap: under a launchd
// job, an upgrade's gap (old daemon gone, the job's new one not up yet) can
// outlast an autostart's wait. The autostart must not start a daemon of its
// own there: it would take the home, the job's daemon would exit "already
// runs", and the upgrade would end on a daemon beside the job. A slow exit
// of the old daemon must not use up the new one's start time either.
func TestSupervisedUpgradeAutostartLosesNoGap(t *testing.T) {
	for _, tc := range []string{"autostart in the gap", "slow exit"} {
		t.Run(tc, func(t *testing.T) {
			r := newSwapRig(t)
			j := newFakeLaunchd(t, r)
			detectSupervisor = func(context.Context, int) (*supervisor, error) { return testJob, nil }
			j.underJob(t)
			var started, autoErr error
			done := make(chan struct{})
			if tc == "autostart in the gap" {
				saved := autostartSwapWait
				autostartSwapWait = 200 * time.Millisecond
				t.Cleanup(func() { autostartSwapWait = saved })
				j.gap = time.Second
				j.inGap = func() {
					cfg, err := loadConfig()
					if err != nil {
						started = err
						close(done)
						return
					}
					go func() { defer close(done); autoErr = autostart(context.Background(), cfg) }()
				}
			} else {
				saved := swapStartWait
				swapStartWait = time.Second
				t.Cleanup(func() { swapStartWait = saved })
				j.late = 1500 * time.Millisecond
				close(done)
			}
			err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
			<-done
			if started != nil {
				t.Fatal(started)
			}
			if err != nil {
				t.Fatalf("upgrade under launchd: %v", err)
			}
			if h := r.health(t); h.PID != j.mainPID() {
				t.Fatalf("the daemon (pid %d) is not the job's process (pid %d); autostart: %v", h.PID, j.mainPID(), autoErr)
			}
			if tc == "autostart in the gap" && (autoErr == nil || !strings.Contains(autoErr.Error(), "in progress")) {
				t.Fatalf("an autostart during the swap: %v", autoErr)
			}
		})
	}
}

// TestUpgradeBesideLaunchdJob: the daemon runs beside a
// loaded launchd job (started by hand, by an autostart or by an earlier
// rollback), so kickstart would replace nothing. The swap stops it first
// and ends with the job's daemon, after a good build and after a rollback.
func TestUpgradeBesideLaunchdJob(t *testing.T) {
	for _, tc := range []struct{ name, detect string }{
		{"found beside", "beside"},        // the daemon's pid is not the job's: found by the job's program
		{"reported as the job's", "main"}, // a detection that says the job runs it
		{"rolled back", "beside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSwapRig(t)
			j := newFakeLaunchd(t, r)
			old := r.health(t).PID
			if tc.detect == "main" {
				detectSupervisor = func(context.Context, int) (*supervisor, error) { return testJob, nil }
			} else {
				detectSupervisor = func(context.Context, int) (*supervisor, error) { return nil, nil }
				detectJobBeside = func(_ context.Context, exe, home string) (*supervisor, error) {
					if exe != r.zm || home != r.zmHome() {
						t.Errorf("job lookup for %s on %s", exe, home)
					}
					return testJob, nil
				}
			}
			if tc.name == "rolled back" {
				calls := 0
				probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
					calls++
					sc := swap.Probe(ctx, c, wiring.Probes...)
					if calls == 2 {
						sc.Items = sc.Items[:0]
					}
					return sc
				}
			}
			err := runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
			var ew exitWith
			switch {
			case tc.name == "rolled back" && !(errors.As(err, &ew) && strings.Contains(err.Error(), "rolled back")):
				t.Fatalf("a rejected build beside the job: %v", err)
			case tc.name != "rolled back" && err != nil:
				t.Fatalf("upgrade beside the job: %v", err)
			}
			if alive(old) {
				t.Fatalf("the daemon beside the job (pid %d) still runs", old)
			}
			if h := r.health(t); h.PID != j.mainPID() {
				t.Fatalf("the daemon (pid %d) is not the job's process (pid %d)", h.PID, j.mainPID())
			}
		})
	}
}
