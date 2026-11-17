package main

// A daemon run by a service manager (a systemd unit, a launchd job) is
// restarted through it, never beside it: starting our own would put a
// second daemon next to the one the manager brings back (SPEC 2, "Swaps").

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/session/holder"
)

// supervisor is the service manager whose main process is the daemon.
type supervisor struct {
	Kind   string // systemd | launchd
	Unit   string // the systemd unit (zm.service) or the launchd label
	User   bool   // a systemd --user unit
	launch *ownedLaunchAgent
}

func (v *supervisor) String() string {
	if v.Kind == "systemd" && v.User {
		return "systemd --user unit " + v.Unit
	}
	if v.Kind == "systemd" {
		return "systemd unit " + v.Unit
	}
	return "launchd job " + v.Unit
}

// detectSupervisor finds the service manager running pid as its main
// process; nil when none does (tests replace it).
var detectSupervisor = func(ctx context.Context, pid int) (*supervisor, error) {
	if pid <= 0 {
		return nil, nil
	}
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			return nil, nil
		}
		unit, user := unitFromCgroup(string(b))
		if unit == "" {
			return nil, nil
		}
		// Inside a unit's cgroup is not enough (a daemon started from a
		// shell that a service runs): the unit must have it as its main pid.
		args := []string{"show", "-p", "MainPID", "--value", unit}
		if user {
			args = append([]string{"--user"}, args...)
		}
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "systemctl", args...)
		if err != nil {
			return nil, fmt.Errorf("the daemon runs in the systemd unit %s, and systemctl show failed: %w", unit, err)
		}
		if main, _ := strconv.Atoi(strings.TrimSpace(string(out))); main != pid {
			return nil, nil
		}
		return &supervisor{Kind: "systemd", Unit: unit, User: user}, nil
	case "darwin":
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "list")
		if err != nil {
			return nil, nil
		}
		if label := labelForPID(string(out), pid); label != "" {
			return &supervisor{Kind: "launchd", Unit: label}, nil
		}
	}
	return nil, nil
}

// unitFromCgroup reads /proc/<pid>/cgroup (holder.UnitFromCgroup).
func unitFromCgroup(s string) (unit string, user bool) { return holder.UnitFromCgroup(s) }

// labelForPID finds pid's job in `launchctl list` (PID, status, label).
func labelForPID(list string, pid int) string {
	want := strconv.Itoa(pid)
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == want {
			return f[2]
		}
	}
	return ""
}

// supervisorRestart asks the manager to restart the daemon (it stops the
// running one and starts the binary its unit names; tests replace it).
var supervisorRestart = func(ctx context.Context, v *supervisor) error {
	var cmd []string
	switch v.Kind {
	case "systemd":
		args := []string{"restart", v.Unit}
		if v.User {
			args = append([]string{"--user"}, args...)
		}
		cmd = append([]string{"systemctl"}, args...)
	case "launchd":
		if v.launch != nil {
			return restartOwnedLaunchAgent(ctx, v)
		}
		cmd = []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), v.Unit)}
	default:
		return fmt.Errorf("unknown supervisor %s", v.Kind)
	}
	// systemctl restart waits for the stop (up to TimeoutStopSec) and the start.
	if out, err := supervisorOutput(ctx, swapSupervisorStopWait+swapStartWait, true, cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// supervisorCallWait bounds one systemctl, launchctl or plutil call: a
// swap past its commit ignores signals, so a hung one must not hold it.
var supervisorCallWait = 30 * time.Second

// supervisorOutput runs name within wait: its stdout, or with combined
// its stdout and stderr.
func supervisorOutput(ctx context.Context, wait time.Duration, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second // a child holding the pipe open
	if combined {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

// supervisorProgram is the binary the supervisor starts (a systemd unit's
// ExecStart path, a launchd job's program); "" when it cannot say (tests
// replace it).
var supervisorProgram = func(ctx context.Context, v *supervisor) (string, error) {
	switch v.Kind {
	case "systemd":
		args := []string{"show", "-p", "ExecStart", "--value", v.Unit}
		if v.User {
			args = append([]string{"--user"}, args...)
		}
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "systemctl", args...)
		if err != nil {
			return "", fmt.Errorf("systemctl show -p ExecStart %s: %w", v.Unit, err)
		}
		return execStartPath(string(out)), nil
	case "launchd":
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), v.Unit))
		if err != nil {
			return "", fmt.Errorf("launchctl print %s: %w", v.Unit, err)
		}
		return launchdProgram(string(out)), nil
	}
	return "", nil
}

// execStartPath reads the path out of `systemctl show -p ExecStart
// --value`: "{ path=/usr/local/bin/zm ; argv[]=/usr/local/bin/zm daemon ; ... }".
func execStartPath(s string) string {
	for _, f := range strings.Split(s, ";") {
		f = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(f), "{"))
		if p, ok := strings.CutPrefix(f, "path="); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// launchdProgram reads `launchctl print`: its "program = X" line, else the
// first of its "arguments = {" block.
func launchdProgram(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		if p, ok := strings.CutPrefix(strings.TrimSpace(l), "program = "); ok {
			return strings.TrimSpace(p)
		}
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "arguments = {" && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

// samePath reports whether a and b name the same file (symlinks resolved).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// supervisorMain is the pid v runs as its main process now, 0 when none
// (tests replace it).
var supervisorMain = func(ctx context.Context, v *supervisor) (int, error) {
	switch v.Kind {
	case "systemd":
		args := []string{"show", "-p", "MainPID", "--value", v.Unit}
		if v.User {
			args = append([]string{"--user"}, args...)
		}
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "systemctl", args...)
		if err != nil {
			return 0, fmt.Errorf("systemctl show -p MainPID %s: %w", v.Unit, err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return pid, nil
	case "launchd":
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "list")
		if err != nil {
			return 0, fmt.Errorf("launchctl list: %w", err)
		}
		pid, _ := jobPID(string(out), v.Unit)
		return pid, nil
	}
	return 0, fmt.Errorf("unknown supervisor %s", v.Kind)
}

// jobPID finds label in `launchctl list`: loaded, and its pid (0: "-").
func jobPID(list, label string) (pid int, loaded bool) {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == label {
			pid, _ = strconv.Atoi(f[0])
			return pid, true
		}
	}
	return 0, false
}

// detectJobBeside finds, on macOS, a loaded launchd agent that runs exe as
// home's daemon, for a daemon running outside it (started by hand, by an
// autostart or by an older rollback); nil when there is none (tests
// replace it).
var detectJobBeside = func(ctx context.Context, exe, home string) (*supervisor, error) {
	uh, err := os.UserHomeDir()
	if runtime.GOOS != "darwin" || err != nil {
		return nil, nil
	}
	plists, _ := filepath.Glob(filepath.Join(uh, "Library", "LaunchAgents", "*.plist"))
	list := ""
	for _, p := range plists {
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Contains(b, []byte(filepath.Base(exe))) || !bytes.Contains(b, []byte("daemon")) {
			continue
		}
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "plutil", "-convert", "json", "-o", "-", p)
		var j launchdJob
		if err != nil || json.Unmarshal(out, &j) != nil || !j.runs(exe, home, uh) {
			continue
		}
		if list == "" {
			out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "list")
			if err != nil {
				return nil, nil
			}
			list = string(out)
		}
		if _, loaded := jobPID(list, j.Label); loaded {
			return &supervisor{Kind: "launchd", Unit: j.Label}, nil
		}
	}
	return nil, nil
}

// launchdJob is the part of a launchd plist that says what it runs.
type launchdJob struct {
	Label                string
	Program              string
	ProgramArguments     []string
	EnvironmentVariables map[string]string
}

// runs reports whether j runs `exe daemon [flags]` on home (userHome: $HOME).
func (j launchdJob) runs(exe, home, userHome string) bool {
	args, prog := j.ProgramArguments, j.Program
	if prog == "" && len(args) > 0 {
		prog = args[0]
	}
	if len(args) < 2 || args[1] != "daemon" || !samePath(prog, exe) {
		return false
	}
	for _, a := range args[2:] {
		if !strings.HasPrefix(a, "-") { // a subcommand (daemon stop), not the daemon
			return false
		}
	}
	jh := j.EnvironmentVariables["ZM_HOME"]
	if jh == "" {
		jh = filepath.Join(userHome, ".zm")
	}
	return samePath(jh, home)
}
