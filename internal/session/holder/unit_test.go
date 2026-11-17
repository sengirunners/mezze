package holder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeUnit replaces what UnitKillsSessions reads; show nil fails the test if asked.
func fakeUnit(t *testing.T, cgroup string, cgroupErr error, show func() ([]byte, error)) {
	t.Helper()
	oldC, oldS := readCgroup, systemctlShow
	t.Cleanup(func() { readCgroup, systemctlShow = oldC, oldS })
	readCgroup = func() ([]byte, error) { return []byte(cgroup), cgroupErr }
	systemctlShow = func(context.Context, ...string) ([]byte, error) {
		if show == nil {
			t.Fatal("systemctl was asked on a daemon in no unit")
		}
		return show()
	}
}

// TestUnitKillsSessionsCannotRead: under a unit, no answer from systemctl
// refuses (it fails closed); it must say why.
func TestUnitKillsSessionsCannotRead(t *testing.T) {
	fakeUnit(t, "0::/system.slice/zm.service\n", nil, func() ([]byte, error) {
		return nil, errors.New(`exec: "systemctl": executable file not found`)
	})
	got := UnitKillsSessions(context.Background())
	for _, want := range []string{"systemd unit zm.service", "cannot read its KillMode", "not found", "KillMode=process"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// TestUnitKillsSessionsNoSystemd: no cgroup file (macOS) or a cgroup that is
// no service passes, and never asks systemctl.
func TestUnitKillsSessionsNoSystemd(t *testing.T) {
	for _, tc := range []struct {
		cgroup string
		err    error
	}{
		{"", os.ErrNotExist},
		{"0::/user.slice/user-1000.slice/session-3.scope\n", nil},
		{"0::/\n", nil},
		{"12:pids:/docker/abc\n", nil},
	} {
		fakeUnit(t, tc.cgroup, tc.err, nil)
		if got := UnitKillsSessions(context.Background()); got != "" {
			t.Errorf("%q: %q", tc.cgroup, got)
		}
	}
}

// TestUnitKillsSessionsReadsTheUnit: an answer decides, for a user unit too.
func TestUnitKillsSessionsReadsTheUnit(t *testing.T) {
	const cg = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/zm.service\n"
	self := os.Getpid()
	fakeUnit(t, cg, nil, func() ([]byte, error) { return fmt.Appendf(nil, "MainPID=%d\nKillMode=control-group\n", self), nil })
	if got := UnitKillsSessions(context.Background()); !strings.Contains(got, "systemd --user unit zm.service") {
		t.Errorf("control-group: %q", got)
	}
	fakeUnit(t, cg, nil, func() ([]byte, error) { return fmt.Appendf(nil, "MainPID=%d\nKillMode=process\n", self), nil })
	if got := UnitKillsSessions(context.Background()); got != "" {
		t.Errorf("process: %q", got)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	cg := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/zm.service\n"
	if u, user := UnitFromCgroup(cg); u != "zm.service" || !user {
		t.Fatalf("%q %v", u, user)
	}
	if u, user := UnitFromCgroup("0::/system.slice/zm.service\n"); u != "zm.service" || user {
		t.Fatalf("%q %v", u, user)
	}
	if u, _ := UnitFromCgroup("0::/user.slice/user-1000.slice/session-3.scope\n"); u != "" {
		t.Fatalf("%q", u)
	}
}

func TestKillModeProblem(t *testing.T) {
	for _, tc := range []struct {
		show string
		bad  bool
	}{
		{"MainPID=7\nKillMode=control-group\n", true},
		{"KillMode=mixed\nMainPID=7\n", true},
		{"MainPID=7\nKillMode=process\n", false},
		{"MainPID=7\nKillMode=none\n", false},
		{"MainPID=9\nKillMode=control-group\n", false}, // inside a unit, not its main process
	} {
		got := killModeProblem("systemd unit zm.service", tc.show, 7)
		if tc.bad != (got != "") || (tc.bad && !strings.Contains(got, "KillMode=process")) {
			t.Errorf("%q: %q", tc.show, got)
		}
	}
}
