package holder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// UnitFromCgroup reads /proc/<pid>/cgroup: the .service the process is in,
// and whether it is a user unit (under user@<uid>.service).
func UnitFromCgroup(s string) (unit string, user bool) {
	for _, line := range strings.Split(s, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		last := path[strings.LastIndexByte(path, '/')+1:]
		if strings.HasSuffix(last, ".service") && !strings.HasPrefix(last, "user@") {
			return last, strings.Contains(path, "/user@")
		}
	}
	return "", false
}

// readCgroup and systemctlShow are what UnitKillsSessions asks (tests replace them).
var (
	readCgroup    = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }
	systemctlShow = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "systemctl", args...).Output()
	}
)

// UnitKillsSessions says why sessions would end with this daemon although
// their holders outlive it: a holder stays in the daemon's cgroup, and when
// a systemd unit whose main process is this daemon stops or its main process
// exits, every KillMode but process and none kills the whole cgroup. "" when
// no such unit runs it (launchd kills only the job's process group, which a
// holder has left).
//
// Whether a unit runs it comes from the cgroup, never from systemctl: a daemon
// in a unit's cgroup that cannot get an answer from systemctl (missing, no
// bus, timed out) counts as killed, since "safe" would be a guess and the
// cost of a wrong one is every session. No cgroup file (not Linux) or a cgroup
// that is no .service (a login session, a container) is no unit.
func UnitKillsSessions(ctx context.Context) string {
	b, err := readCgroup()
	if err != nil {
		return ""
	}
	unit, user := UnitFromCgroup(string(b))
	if unit == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"show", "-p", "MainPID", "-p", "KillMode", unit}
	where := "systemd unit " + unit
	if user {
		args = append([]string{"--user"}, args...)
		where = "systemd --user unit " + unit
	}
	out, err := systemctlShow(ctx, args...)
	if err != nil {
		return fmt.Sprintf("this daemon runs in the %s and zm cannot read its KillMode (systemctl show: %v), so it must assume the unit kills every process in its cgroup, sessions included, when the daemon stops or crashes; make systemctl answer, and set KillMode=process in the unit (docs/runbooks/zm-upgrade.md)", where, err)
	}
	return killModeProblem(where, string(out), os.Getpid())
}

// killModeProblem reads `systemctl show -p MainPID -p KillMode`.
func killModeProblem(where, show string, self int) string {
	mode, main := "", 0
	for _, line := range strings.Split(show, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "KillMode":
			mode = v
		case "MainPID":
			main, _ = strconv.Atoi(v)
		}
	}
	if main != self || mode == "process" || mode == "none" {
		return "" // inside a unit, but not its daemon: that unit's stop is not this daemon's
	}
	return fmt.Sprintf("the %s runs this daemon with KillMode=%s, which kills every process in its cgroup, sessions included, when the daemon stops or crashes; add KillMode=process to the unit (docs/runbooks/zm-upgrade.md)", where, mode)
}
