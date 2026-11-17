package team

import (
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// A script run goes only to a machine whose zm runs scripts: auto skips an
// older build even when it is the roomiest, and naming one is refused. Agent
// tasks still go anywhere that takes remote tasks.
func TestPickMachineSendsRunsOnlyWhereScriptsRun(t *testing.T) {
	now := time.Now()
	m := func(name string, freeMB int64, runs bool) api.Machine {
		return api.Machine{Machine: name, Seen: &now, Stats: api.MachineStats{MemoryTotalMB: freeMB, MemoryUsedMB: 0, DiskFreeMB: 100000,
			RemoteTasks: true, ScriptRuns: runs}}
	}
	ms := []api.Machine{m("old", 64000, false), m("new", 8000, true)}
	if got, err := pickMachine(ms, "auto", "mac", "auth", true); err != nil || got != "new" {
		t.Fatalf("run, auto: %q %v", got, err)
	}
	if got, err := pickMachine(ms, "auto", "mac", "auth", false); err != nil || got != "old" {
		t.Fatalf("agent task, auto: %q %v", got, err)
	}
	if _, err := pickMachine(ms, "old", "mac", "auth", true); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("run, named old build: %v", err)
	}
	if got, err := pickMachine(ms, "old", "mac", "auth", false); err != nil || got != "old" {
		t.Fatalf("agent task, named: %q %v", got, err)
	}
	if _, err := pickMachine(ms[:1], "auto", "mac", "auth", true); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("run, no machine runs scripts: %v", err)
	}
}

// A sandbox task goes only to a machine with a task driver, counted on its
// own slots: session limits and free memory do not count, disk and the
// heartbeat do; the parent's machine never; most sandbox slots first.
// lineCheck refuses a pinned machine without a driver (403) and a project
// with none (503).
func TestPickMachineSandbox(t *testing.T) {
	now, old := time.Now(), time.Now().Add(-time.Hour)
	m := func(name string, sandbox bool, slots int, seen *time.Time) api.Machine {
		return api.Machine{Machine: name, Seen: seen, Stats: api.MachineStats{MemoryTotalMB: 8000, MemoryUsedMB: 7900, DiskFreeMB: 100000,
			Sessions: 4, MaxSessions: 4, RemoteTasks: true, TaskSlots: 1, SandboxTasks: sandbox, SandboxSlots: slots}}
	}
	plain, one, two, quiet := m("plain", false, 0, &now), m("one", true, 1, &now), m("two", true, 2, &now), m("quiet", true, 8, &old)
	full := m("full", true, 9, &now)
	full.Stats.DiskFreeMB = 100
	ms := []api.Machine{plain, one, two, quiet, full}
	if got, err := pickSandboxMachine(ms, "auto", "mac", "auth"); err != nil || got != "two" {
		t.Fatalf("auto: %q %v", got, err)
	}
	if got, err := pickSandboxMachine(ms, "auto", "two", "auth"); err != nil || got != "one" {
		t.Fatalf("never the parent's machine: %q %v", got, err)
	}
	if got, err := pickSandboxMachine(ms, "one", "mac", "auth"); err != nil || got != "one" {
		t.Fatalf("named, at its session limit: %q %v", got, err)
	}
	if _, err := pickSandboxMachine(ms, "plain", "mac", "auth"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("named, no driver: %v", err)
	}
	if _, err := pickSandboxMachine(ms, "quiet", "mac", "auth"); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("named, quiet: %v", err)
	}
	if _, err := pickSandboxMachine([]api.Machine{plain, quiet, full}, "auto", "mac", "auth"); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("auto, none free: %v", err)
	}
	if err := lineCheck(ms, "plain", "mac", false, true); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("lineCheck, pinned without a driver: %v", err)
	}
	if err := lineCheck(ms, "plain", "mac", false, false); err != nil {
		t.Fatalf("lineCheck, plain task pinned there: %v", err)
	}
	if err := lineCheck([]api.Machine{plain}, "auto", "mac", false, true); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("lineCheck, no sandbox machine: %v", err)
	}
	if err := lineCheck(ms, "auto", "mac", false, true); err != nil {
		t.Fatalf("lineCheck, a quiet one counts (it may come back): %v", err)
	}
	for _, c := range []struct {
		st      api.MachineStats
		sandbox bool
		want    int
	}{{plain.Stats, true, 0}, {plain.Stats, false, 1}, {two.Stats, true, 2}, {api.MachineStats{SandboxTasks: true}, true, 1}, {two.Stats, false, 1}} {
		if got := taskSlots(c.st, c.sandbox); got != c.want {
			t.Errorf("taskSlots(%+v, %v) = %d, want %d", c.st, c.sandbox, got, c.want)
		}
	}
}
