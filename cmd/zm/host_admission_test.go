package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/sengirunners/potluck/computeclient"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runtarget"
)

// shareDryRun runs zm host share --dry-run with args and returns its plan.
func shareDryRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_NO_AUTOSTART", "1")
	cmd := hostShareCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(shareArgs(t, append([]string{"--dry-run"}, args...)...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// Zm host share and zm pool host take the admission flags;
// a re-run without them keeps what the earlier share bound (read without
// starting the daemon in a dry run); none removes it; doctor names it.
func TestShareAdmissionFlagsAndRerun(t *testing.T) {
	for _, c := range []*cobra.Command{hostShareCmd(), poolHostCmd()} {
		for _, f := range []string{"admission-url", "admission-token-file", "admission-ttl-s"} {
			if c.Flags().Lookup(f) == nil {
				t.Errorf("%s has no --%s", c.Name(), f)
			}
		}
	}
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &hostRig{reports: []hostprobe.Report{ociHost(true)}}
	h.install(t, false, false)
	plan, err := shareDryRun(t, "--admission-url", "https://ci.example/zm", "--admission-token-file", tok, "--admission-ttl-s", "300")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "admission: every box, conformance's too, first takes a lease from https://ci.example/zm (lease 300 s, token from "+tok+")\n") ||
		!strings.Contains(plan, "--config admission_url=https://ci.example/zm") || !strings.Contains(plan, "--config admission_ttl_s=300") {
		t.Errorf("plan with the flags:\n%s", plan)
	}
	if strings.Join(h.reads, ",") != "project" || h.starts[0] {
		t.Errorf("reads %v (start %v): want the project's, without starting the daemon, even with --admission-url", h.reads, h.starts)
	}

	h.projB = []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"admission_url": "http://10.0.0.5:9000", "admission_ttl_s": "200"}}}
	if plan, err = shareDryRun(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "first takes a lease from http://10.0.0.5:9000 (lease 200 s); kept from h-c1m2 (--admission-url none removes it)") ||
		!strings.Contains(plan, "--config admission_url=http://10.0.0.5:9000") || len(h.starts) != 2 || h.starts[1] {
		t.Errorf("re-run (reads %v):\n%s", h.starts, plan)
	}
	if plan, err = shareDryRun(t, "--admission-url", "none"); err != nil || strings.Contains(plan, "admission") {
		t.Errorf("none: %v\n%s", err, plan)
	}
	if _, err = shareDryRun(t, "--admission-url", "https://ci.example/?k=v"); err == nil || !strings.Contains(err.Error(), "no credentials or query") {
		t.Errorf("a query: %v", err)
	}

	cmd := hostDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--state-dir", t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "admission: h-c1m2 take a lease from http://10.0.0.5:9000 (lease 200 s) before each box\n") || h.starts[len(h.starts)-1] {
		t.Errorf("doctor:\n%s", out.String())
	}
}

// Fail closed (review of #552): a real share whose read of this machine's
// bindings failed refuses without --admission-url; with it, or with none,
// it goes on. zm host share without --pool reads the project's too. The
// machine needs a root step, so the refusal comes before zm asks for root.
func TestShareRefusesWhenTheBindingsAreUnread(t *testing.T) {
	h := &hostRig{reports: []hostprobe.Report{ociHost(false)}, readErr: errors.New("forbidden")}
	h.install(t, true, true)
	if _, err := hostShare(t, "--runner-dir", "/home/runner"); err == nil || !strings.Contains(err.Error(), "could not read this machine's bindings (forbidden): pass --admission-url URL or none") {
		t.Errorf("unread: %v", err)
	}
	if len(h.starts) != 1 || !h.starts[0] || strings.Join(h.reads, ",") != "project" {
		t.Errorf("reads %v (start %v)", h.reads, h.starts)
	}
	if len(h.scripts) != 0 {
		t.Errorf("asked for root, then refused: %q", h.scripts)
	}
	// The shelf and volume limits an earlier share bound are unread too
	// (Potluck #149): named, or refused as the admission keys are. (Potluck's
	// Precheck is a dry run, which skips that refusal, so root is asked first.)
	if _, err := hostShare(t, "--runner-dir", "/home/runner", "--admission-url", "none"); err == nil || !strings.Contains(err.Error(), "pass --shelf-gb, --shelf-fill and --volume-max-gb") {
		t.Errorf("none without the storage flags: %v", err)
	}
	roots := len(h.scripts)
	if _, err := hostShare(t, "--runner-dir", "/home/runner", "--admission-url", "none", "--shelf-gb", "0", "--shelf-fill", "off", "--volume-max-gb", "20"); err == nil || strings.Contains(err.Error(), "could not read") || len(h.scripts) != roots+1 {
		t.Errorf("none: %v (root %d)", err, len(h.scripts)) // stops later: no daemon in tests
	}
	if _, err := hostShare(t, "--runner-dir", "/home/runner", "--pool", "team"); err == nil || !strings.Contains(err.Error(), "could not read") || h.reads[len(h.reads)-1] != "host" {
		t.Errorf("--pool: %v (reads %v)", err, h.reads)
	}
	// With a URL too: a keyless earlier binding could stay live beside it.
	if _, err := hostShare(t, "--runner-dir", "/home/runner", "--admission-url", "https://ci.example"); err == nil || !strings.Contains(err.Error(), "could not read this machine's bindings (forbidden)") {
		t.Errorf("unread with a URL: %v", err)
	}
	if plan, err := shareDryRun(t); err != nil || strings.Contains(plan, "admission") {
		t.Errorf("a dry run notes it and goes on: %v", err)
	}
}

// shareFlagsRun is zm host share's path with a binder of the test's: the
// flags, options (which read the earlier bindings), then runShare, as RunE.
func shareFlagsRun(t *testing.T, b hostprobe.Binder, run hostprobe.Runner, args ...string) (hostprobe.ShareResult, error) {
	t.Helper()
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_NO_AUTOSTART", "1")
	var sf shareFlags
	cmd := &cobra.Command{Use: "share"}
	sf.add(cmd, true)
	if err := cmd.ParseFlags(shareArgs(t, args...)); err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.Background())
	o, err := sf.options(cmd)
	if err != nil {
		return hostprobe.ShareResult{}, err
	}
	return runShare(context.Background(), run, b, o, &bytes.Buffer{})
}

type unbinds struct {
	host, project []string
}

func (u *unbinds) BindRunTarget(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
	return computeapi.RunTargetView{}, nil
}
func (u *unbinds) UnbindRunTarget(_ context.Context, n string) error {
	u.project = append(u.project, n)
	return nil
}
func (u *unbinds) UnbindHostRunTarget(_ context.Context, n string) error {
	u.host = append(u.host, n)
	return nil
}

// Review of #552: the first switch-on (--admission-url on a host already
// lent without it) unbinds the keyless binding when the new run fails, and
// only the one in the scope the bind replaces: the project's without
// --pool, the machine-level one with it, and never one that has the keys.
func TestShareFirstSwitchOnUnbindsOnlyTheReplacedKeylessBinding(t *testing.T) {
	failing := runnerFunc(func(_ context.Context, name string, _ ...string) (hostprobe.Result, error) {
		if name == "docker" {
			return hostprobe.Result{}, nil
		}
		return hostprobe.Result{Exit: 1}, nil
	})
	keyless := []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"vm": "oci"}}}
	keyed := []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"vm": "oci", "admission_url": "https://old.example"}}}
	for _, c := range []struct {
		name         string
		hostB, projB []computeapi.RunTarget
		pool         bool
		wantHost     []string
		wantProject  []string
	}{
		{name: "project keyless", projB: keyless, hostB: keyed, wantProject: []string{"h-c1m2"}},
		{name: "project keyed, machine-level keyless", projB: keyed, hostB: keyless},
		{name: "--pool, machine-level keyless", projB: keyed, hostB: keyless, pool: true, wantHost: []string{"h-c1m2"}},
		{name: "--pool, machine-level keyed", projB: keyless, hostB: keyed, pool: true},
	} {
		h := &hostRig{reports: []hostprobe.Report{ociHost(true)}, hostB: c.hostB, projB: c.projB}
		h.install(t, false, false)
		args := []string{"--admission-url", "https://ci.example"}
		if c.pool {
			args = append(args, "--pool", "team")
		}
		u := &unbinds{}
		res, err := shareFlagsRun(t, u, failing, args...)
		if err == nil {
			t.Errorf("%s: a failed size did not fail share", c.name)
		}
		if strings.Join(u.host, ",") != strings.Join(c.wantHost, ",") || strings.Join(u.project, ",") != strings.Join(c.wantProject, ",") {
			t.Errorf("%s: unbound machine-level %v, project %v (reads %v; %+v)", c.name, u.host, u.project, h.reads, res.Sizes)
		}
	}
}

// zm pool host on the pool's own machine, through a real daemon: a lent
// keyless binding in the pool is unbound when the run that adds admission
// fails, even though no size passed and nothing new binds.
func TestPoolHostFirstSwitchOnUnbindsTheKeylessBinding(t *testing.T) {
	ctx := context.Background()
	dm, c := poolDaemon(t, tailnettest.New(), "mini", "ivo@example.com")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	p := inPool(c, "pool")
	if _, err := computeclient.BindRunTarget(ctx, p, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "h-c1m2", Driver: "ref", Kind: "box", Isolation: "ephemeral",
		Images: []string{runnerImage}, Config: map[string]string{"vm": "oci", "size": "c1m2", "cpu": "1", "memory": "2048"}}}); err != nil {
		t.Fatal(err)
	}
	o := hostprobe.ShareOptions{Report: ociHost(true), Images: []string{runnerImage}, NamePrefix: "h", Driver: "ref", AdmissionURL: "https://ci.example"}
	failing := runnerFunc(func(_ context.Context, name string, _ ...string) (hostprobe.Result, error) {
		if name == "docker" {
			return hostprobe.Result{}, nil
		}
		return hostprobe.Result{Exit: 1}, nil
	})
	var out bytes.Buffer
	if err := poolHost(ctx, c, failing, "pool", o, &out, nil, nil); err == nil || !strings.Contains(out.String(), "had no admission keys, so it was unbound") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	l, err := computeclient.RunTargets(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range l.Targets {
		if tg.Name == "h-c1m2" {
			t.Errorf("the keyless binding is still bound: %+v", tg)
		}
	}
}
