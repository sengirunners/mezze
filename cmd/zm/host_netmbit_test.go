package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
)

// --net-mbit is tri-state for a re-run, on zm host share and
// zm pool host: given, it decides; 0 removes the cap; left out, hostprobe
// keeps the cap the network's unit has.
func TestShareNetMbitFlag(t *testing.T) {
	for _, pool := range []bool{true, false} {
		for _, c := range []struct {
			args []string
			mbit int
			set  bool
		}{{nil, 0, false}, {[]string{"--net-mbit", "48"}, 48, true}, {[]string{"--net-mbit", "0"}, 0, true}} {
			t.Setenv("ZM_HOME", t.TempDir())
			t.Setenv("ZM_NO_AUTOSTART", "1")
			var sf shareFlags
			cmd := &cobra.Command{Use: "share"}
			sf.add(cmd, pool)
			if err := cmd.ParseFlags(append([]string{"--name-prefix", "h", "--dry-run"}, c.args...)); err != nil {
				t.Fatal(err)
			}
			cmd.SetContext(context.Background())
			o, err := sf.options(cmd)
			if err != nil || o.NetMbit != c.mbit || o.NetMbitSet != c.set {
				t.Errorf("%v: net-mbit %d set %v (%v), want %d %v", c.args, o.NetMbit, o.NetMbitSet, err, c.mbit, c.set)
			}
		}
	}
}

// zm host share --net-mbit runs the cap's root step itself and says what it
// is for; without the flag a capped network is left alone; doctor shows the cap.
func TestShareSetsAndRemovesTheDownloadCap(t *testing.T) {
	capped := func(mbit int) hostprobe.Report {
		r := ociHost(true)
		r.Layers[0].NetworkMbit = mbit
		return r
	}
	// A machine set up under zm's names keeps them: its cap steps rewrite zm-box-net.service.
	old := func(mbit int) hostprobe.Report {
		r := capped(mbit)
		r.Layers[0].Network, r.Layers[0].NetworkLegacy = hostprobe.LegacyBoxNet, true
		return r
	}
	oldSteps := func(mbit int) []string { return hostprobe.BoxNetCapStepsFor(old(0).Layers[0], mbit) }
	if slices.Equal(oldSteps(48), hostprobe.BoxNetCapSteps(48)) {
		t.Fatal("the old names' cap steps are the new names'")
	}
	for _, c := range []struct {
		name   string
		before hostprobe.Report
		args   []string
		steps  []string
		why    string
	}{
		{"old names, set", old(0), []string{"--net-mbit", "48"}, oldSteps(48), "zm needs admin rights once to cap its VMs' downloads at 48 Mbit/s"},
		{"old names, removed", old(48), []string{"--net-mbit", "0"}, oldSteps(0), "zm needs admin rights once to remove the download cap from its VMs' network"},
		{"set", capped(0), []string{"--net-mbit", "48"}, hostprobe.BoxNetCapSteps(48), "zm needs admin rights once to cap its VMs' downloads at 48 Mbit/s"},
		{"removed", capped(48), []string{"--net-mbit", "0"}, hostprobe.BoxNetCapSteps(0), "zm needs admin rights once to remove the download cap from its VMs' network"},
		{"with the network", ociHost(false), []string{"--net-mbit", "48"}, hostprobe.BoxNetCapSteps(48), "zm needs admin rights once to set up the firewall its VMs run behind, with downloads capped at 48 Mbit/s"},
		{"kept", capped(48), nil, nil, ""},
		{"as it is", capped(48), []string{"--net-mbit", "48"}, nil, ""},
	} {
		h := &hostRig{reports: []hostprobe.Report{c.before, capped(48)}}
		h.install(t, true, true)
		errOut, _ := hostShare(t, append([]string{"--runner-dir", "/home/runner"}, c.args...)...)
		switch {
		case c.steps == nil && len(h.scripts) != 0:
			t.Errorf("%s: share ran %q, want no root step", c.name, h.scripts)
		case c.steps != nil && (len(h.scripts) != 1 || h.scripts[0] != hostprobe.RootScript(c.steps)):
			t.Errorf("%s: share ran %q", c.name, h.scripts)
		case c.steps != nil && !strings.Contains(errOut, c.why):
			t.Errorf("%s: share said %q, want %q", c.name, errOut, c.why)
		}
	}

	h := &hostRig{reports: []hostprobe.Report{capped(48)}}
	h.install(t, true, true)
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_NO_AUTOSTART", "1")
	cmd := hostDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--state-dir", t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "filtered network "+hostprobe.BoxNet+" (internet, never this machine or its LAN; downloads capped at 48 Mbit/s for all of them together)\n") {
		t.Errorf("doctor:\n%s", out.String())
	}
}

// --net-mbit caps zm's own oci network: on a tart layer potluck refuses it, a
// cap or its removal alike, and share shows that refusal as a plain error
// with no root step run.
func TestShareNetMbitRefusedOnTart(t *testing.T) {
	tart := hostprobe.Report{OS: "darwin", Arch: "arm64", Cores: 16, RAMMB: 48 << 10, FreeDiskGB: 500,
		Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true}}}
	for _, mbit := range []string{"48", "0"} {
		h := &hostRig{reports: []hostprobe.Report{tart}}
		h.install(t, true, true)
		_, err := hostShare(t, "--layer", "tart", "--net-mbit", mbit)
		if err == nil || !strings.Contains(err.Error(), "--net-mbit is for the oci layer") || !strings.Contains(err.Error(), "tart") {
			t.Errorf("--net-mbit %s on tart: %v", mbit, err)
		}
		if len(h.scripts) != 0 {
			t.Errorf("--net-mbit %s on tart ran root steps %q", mbit, h.scripts)
		}
	}
}
