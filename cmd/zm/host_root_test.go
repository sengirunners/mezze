package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/runtarget"
)

// ociHost is a Linux machine whose oci layer is ready but whose filtered
// network is not set up: doctor's root line is the network's steps.
func ociHost(netReady bool) hostprobe.Report {
	l := hostprobe.Layer{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}
	if netReady {
		l.Network = hostprobe.BoxNet
	} else {
		l.NetworkProblem, l.NetworkSteps = "not set up", hostprobe.BoxNetSteps()
	}
	return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 4, RAMMB: 7680, Layers: []hostprobe.Layer{l, {Name: "tart", Supported: true}}}
}

// hostRig swaps the machine probe and the root runner for fakes: each probe
// returns the next report (the last one again), and run records the scripts.
type hostRig struct {
	reports []hostprobe.Report
	probes  int
	scripts []string
	runErr  error
	// hostB and projB are the machine's bindings now, machine-level and
	// the project's (bindingsIn); reads records each read's scope and
	// starts whether it could start the daemon.
	hostB, projB []computeapi.RunTarget
	reads        []string
	starts       []bool
	readErr      error
}

func (h *hostRig) install(t *testing.T, tty, sudo bool) {
	t.Helper()
	oldDetect, oldRoot, oldRun, oldBindings := detectHost, hostRoot, shareRunner, bindingsIn
	t.Cleanup(func() { detectHost, hostRoot, shareRunner, bindingsIn = oldDetect, oldRoot, oldRun, oldBindings })
	bindingsIn = func(_ context.Context, start, host bool) ([]computeapi.RunTarget, error) {
		h.starts = append(h.starts, start)
		if host {
			h.reads = append(h.reads, "host")
			return h.hostB, h.readErr
		}
		h.reads = append(h.reads, "project")
		return h.projB, h.readErr
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{}, errors.New("no commands in tests")
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		r := h.reports[min(h.probes, len(h.reports)-1)]
		h.probes++
		return r
	}
	hostRoot = rootStep{
		tty:  func() bool { return tty },
		sudo: func() bool { return sudo },
		run: func(_ context.Context, script string) error {
			h.scripts = append(h.scripts, script)
			return h.runErr
		},
	}
}

// runnerImage is an image zm does not pin: its boxes serve a runner (and
// need zm's network) only with --runner-dir.
var runnerImage = "x@sha256:" + strings.Repeat("a", 64)

// shareArgs are flags every case passes: no config is read for a name.
// Conformance runs the driver file the daemon would pin, so $ZM_HOME (set
// first) gets a stand-in zm-run-tart in its drivers directory; the fake
// runners never run it.
func shareArgs(t *testing.T, args ...string) []string {
	t.Helper()
	if home := os.Getenv("ZM_HOME"); home != "" {
		installDriver(t, filepath.Join(home, runtarget.DriversDir), "zm-run-tart", "#!/bin/sh\nexit 1\n")
	}
	return append([]string{"--name-prefix", "h", "--state-dir", t.TempDir(), "--image", runnerImage, "--reclaim-idle-s", "0"}, args...)
}

// hostShare runs zm host share with args (its call site); after its root
// step, the fake runner stops it at the first pull. It returns stderr.
func hostShare(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_NO_AUTOSTART", "1")
	cmd := hostShareCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(shareArgs(t, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	return errOut.String(), err
}

// zm host share and zm pool host run doctor's root step themselves at a
// terminal: one sentence of what and why, then sudo sh -c with the very
// script doctor prints (sudo asks for the password; zm never sees it), then
// the machine is read again and share goes on with the network set up.
func TestShareRunsTheRootStepItself(t *testing.T) {
	doctor := ociHost(false).RootSteps("oci") // what doctor prints, RootCommand(doctor)
	h := &hostRig{reports: []hostprobe.Report{ociHost(false), ociHost(true)}}
	h.install(t, true, true)
	errOut, _ := hostShare(t, "--runner-dir", "/home/runner")
	if len(h.scripts) != 1 || h.scripts[0] != hostprobe.RootScript(doctor) {
		t.Fatalf("share ran %q", h.scripts)
	}
	if !strings.Contains(errOut, "zm needs admin rights once to set up the firewall its VMs run behind") || !strings.Contains(errOut, "sudo may ask for your password") {
		t.Errorf("share said %q", errOut)
	}
	if h.probes != 2 {
		t.Errorf("share read the machine %d times, want again after the root step", h.probes)
	}

	// zm pool host: the same, once the pool is known; then share's checks
	// run on the machine as read again (here conformance fails: no join).
	h = &hostRig{reports: []hostprobe.Report{ociHost(false), ociHost(true)}}
	h.install(t, true, true)
	var sf shareFlags
	cmd := &cobra.Command{Use: "host"}
	sf.add(cmd, false)
	if err := cmd.ParseFlags(shareArgs(t, "--runner-dir", "/home/runner")); err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.Background())
	o, err := sf.options(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var errBuf, out bytes.Buffer
	run := runnerFunc(func(_ context.Context, name string, _ ...string) (hostprobe.Result, error) {
		if name == "docker" { // the pull
			return hostprobe.Result{}, nil
		}
		return hostprobe.Result{Exit: 1}, nil
	})
	err = poolHost(context.Background(), nil, run, "zm-mini:abcd:ABCD-EFGH", o, &out, sf.root(context.Background(), &errBuf), nil)
	if err == nil || !strings.Contains(err.Error(), "did not join") || len(h.scripts) != 1 || h.scripts[0] != hostprobe.RootScript(doctor) {
		t.Fatalf("pool host: %v, ran %q", err, h.scripts)
	}
	if !strings.Contains(errBuf.String(), "admin rights once") {
		t.Errorf("pool host said %q", errBuf.String())
	}
}

// Not at a terminal, no sudo, --no-sudo, a dry run, or sudo failing: zm
// says so in plain words and share goes on as before (it prints the root
// line and lends jobs only, or refuses); nothing is run as root.
func TestShareRootStepFallsBack(t *testing.T) {
	cases := []struct {
		name      string
		tty, sudo bool
		args      []string
		runErr    error
		ran       bool
		say       string
	}{
		{"not a terminal", false, true, nil, nil, false, "not at a terminal, so zm did not ask"},
		{"no sudo", true, false, nil, nil, false, "sudo is not installed"},
		{"--no-sudo", true, true, []string{"--no-sudo"}, nil, false, "--no-sudo"},
		{"dry run", true, true, []string{"--dry-run"}, nil, false, "a dry run"},
		{"sudo failed", true, true, nil, errors.New("exit status 1"), true, "did not finish (exit status 1)"},
	}
	for _, c := range cases {
		h := &hostRig{reports: []hostprobe.Report{ociHost(false)}, runErr: c.runErr}
		h.install(t, c.tty, c.sudo)
		errOut, _ := hostShare(t, append([]string{"--runner-dir", "/home/runner"}, c.args...)...)
		if (len(h.scripts) > 0) != c.ran || !strings.Contains(errOut, c.say) || !strings.Contains(errOut, "the root line below") {
			t.Errorf("%s: ran %d, said %q", c.name, len(h.scripts), errOut)
		}
	}
	// Nothing pending; boxes that would not use zm's network (no runner:
	// --runner-dir none, or an image zm knows no runner in; or the owner's
	// own --oci-network); or a share that refuses anyway (an explicit
	// --share 0.5 fits nothing twice): nothing is said and nothing runs.
	for _, args := range [][]string{{"--runner-dir", "/home/runner"}, nil, {"--runner-dir", "none"},
		{"--runner-dir", "/home/runner", "--oci-network", "mine", "--net-mail", "smtp.example"},
		{"--runner-dir", "/home/runner", "--share", "0.5"}} {
		h := &hostRig{reports: []hostprobe.Report{ociHost(len(args) == 2 && args[1] == "/home/runner")}}
		h.install(t, true, true)
		if errOut, err := hostShare(t, args...); strings.Contains(errOut, "admin") || len(h.scripts) > 0 {
			t.Errorf("%v: %v %q %q", args, err, errOut, h.scripts)
		}
	}
}

// Doctor's sizes line reports what share lends: on dev-01 (4 cores, 7.5
// GiB) one c1m2 at a time at the default share; an explicit --share keeps
// the rule that two fit.
func TestDoctorSizesMatchShare(t *testing.T) {
	h := &hostRig{reports: []hostprobe.Report{ociHost(true)}}
	h.install(t, false, false)
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "sizes (one at a time: none fits twice inside 50%): c1m2"},
		{[]string{"--share", "0.5"}, "sizes: none fit twice inside 50% of this machine"},
		{[]string{"--share", "0.6"}, "sizes (two fit inside 60%): c1m2"},
	} {
		cmd := hostDoctorCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetContext(context.Background())
		cmd.SetArgs(append([]string{"--state-dir", t.TempDir()}, c.args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want+"\n") {
			t.Errorf("%v: want %q in\n%s", c.args, c.want, out.String())
		}
	}
}
