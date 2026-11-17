package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runners"
)

// fakeGH puts a gh on PATH that answers GET repos/<repo> from vis (repo ->
// "public", "internal" or "private", pushable) and fails anything else:
// tests never reach GitHub.
func fakeGH(t *testing.T, vis map[string]string) {
	t.Helper()
	dir := t.TempDir()
	var cases strings.Builder
	for repo, v := range vis {
		priv := "true"
		if v == "public" {
			priv = "false"
		}
		cases.WriteString("  repos/" + repo + `) echo '{"private":` + priv + `,"visibility":"` + v + `","permissions":{"push":true}}' ;;` + "\n")
	}
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\ncase \"$last\" in\n" + cases.String() + "  *) echo 'gh: HTTP 404' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir()) // the probe's script
}

func ciDoctorRun(t *testing.T, jsonMode bool, args ...string) (string, error) {
	t.Helper()
	oldJSON := flagJSON
	flagJSON = jsonMode
	t.Cleanup(func() { flagJSON = oldJSON })
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	return runCLI(t, r, append([]string{"ci", "doctor"}, args...)...)
}

func probeCheckOf(t *testing.T, out ciDoctorOut) runners.DoctorCheck {
	t.Helper()
	for _, r := range out.Reports {
		for _, c := range r.Checks {
			if c.Number == probeCheck {
				return c
			}
		}
	}
	t.Fatalf("no check 18: %+v", out)
	return runners.DoctorCheck{}
}

// TestCIDoctorProbeIsPotlucks: zm ci doctor --probe runs Potluck's probe
// (check 18) on zm's daemon: a private repository gets the script to run;
// a public or internal one gets an ask, with no command for an agent, until
// the owner's own --public.
func TestCIDoctorProbeIsPotlucks(t *testing.T) {
	newFakeBroker(t)
	fakeGH(t, map[string]string{"judy/app": "private", "judy/site": "public", "corp/tool": "internal"})
	t.Setenv(cli.EnvNoAutostart, "")

	raw, err := ciDoctorRun(t, true, "judy/app", "--probe")
	var out ciDoctorOut
	if err != nil || json.Unmarshal([]byte(raw), &out) != nil {
		t.Fatalf("private: %v\n%s", err, raw)
	}
	c := probeCheckOf(t, out)
	if c.Status != "warn" || !strings.HasPrefix(c.Fix, "sh ") || !strings.Contains(c.Message, "Probe ready, not run") ||
		out.Ask == nil || out.Ask.Kind != "ask" || out.Ask.Command != c.Fix {
		t.Fatalf("private probe: %+v ask %+v", c, out.Ask)
	}
	if os.Getenv(cli.EnvNoAutostart) != "1" {
		t.Fatal("Potluck's autostart was left on")
	}
	text, err := ciDoctorRun(t, false, "judy/app", "--probe")
	checks, body, shown := strings.Cut(text, "The probe script (pushing a workflow file needs gh's workflow scope: gh auth refresh -s workflow):\n")
	if err != nil || !shown || !strings.Contains(checks, "18 warn") || strings.Contains(checks, "potluck ci") {
		t.Fatalf("text: %v\n%s", err, text)
	}
	// Text mode shows the script Potluck wrote, as it is on disk, then the ask.
	_, next, _ := strings.Cut(checks, "   Next: sh ")
	next, _, _ = strings.Cut(next, "\n")
	disk, rerr := os.ReadFile(shellUnquote(next))
	script, ask, asked := strings.Cut(body, "ask: ")
	if rerr != nil || !strings.HasPrefix(script, "#!") || script != string(disk) || !asked || !strings.Contains(ask, "gh auth refresh -s workflow") {
		t.Fatalf("text shows not the script on disk (%v):\n%s", rerr, text)
	}

	for _, repo := range []string{"judy/site", "corp/tool"} {
		raw, err := ciDoctorRun(t, true, repo, "--probe", "--size", "c4")
		var out ciDoctorOut
		if err == nil || json.Unmarshal([]byte(raw), &out) != nil {
			t.Fatalf("%s: a probe waiting on a yes must fail: %v\n%s", repo, err, raw)
		}
		c := probeCheckOf(t, out)
		if c.Status != "fail" || strings.Contains(c.Fix, "--public") || strings.Contains(c.Fix, "zm ") {
			t.Fatalf("%s: the owner's yes rode on the fix as a command: %+v", repo, c)
		}
		if out.Ask == nil || out.Ask.Kind != "ask" || out.Ask.Command != "" || strings.Join(out.Ask.Choices, ",") != "yes,no" ||
			!strings.Contains(out.Ask.Say, "zm ci doctor "+repo+" --probe --size c4 --public themselves") || strings.Contains(out.Ask.Say, "potluck") {
			t.Fatalf("%s: ask %+v", repo, out.Ask)
		}
		if !strings.Contains(raw, `"ask"`) {
			t.Fatalf("%s: --json has no ask: %s", repo, raw)
		}
	}
	// --labels and --os reach Potluck: its rerun command, in the ask, says them again.
	raw, _ = ciDoctorRun(t, true, "judy/site", "--probe", "--labels", "self-hosted,linux,home", "--os", "linux")
	out = ciDoctorOut{}
	if json.Unmarshal([]byte(raw), &out) != nil || out.Ask == nil || !strings.Contains(out.Ask.Say, "--labels self-hosted,linux,home --os linux --public themselves") {
		t.Fatalf("labels and os: %s", raw)
	}
	// Potluck's repository rule (jobrule.ValidRepo), and never a flag.
	for _, bad := range []string{"--public", "-x/app", "judy/a b", "judy/app/x", "judy/..", "judy/", "/app", "a&b/c"} {
		if _, err := ciDoctorProbe(context.Background(), bad, ciProbeOpts{}); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Fatalf("%q as the repository: %v", bad, err)
		}
	}
	// The owner's own yes: the script to run, no yes/no ask.
	raw, err = ciDoctorRun(t, true, "judy/site", "--probe", "--public")
	out = ciDoctorOut{}
	if err != nil || json.Unmarshal([]byte(raw), &out) != nil || out.Ask == nil || out.Ask.Choices != nil || !strings.HasPrefix(probeCheckOf(t, out).Fix, "sh ") {
		t.Fatalf("--public: %v\n%s", err, raw)
	}
}

// TestCIDoctorProbeFlags: --probe needs one repository, and the probe's
// flags need --probe; without --probe the JSON is the broker's as it was.
func TestCIDoctorProbeFlags(t *testing.T) {
	newFakeBroker(t)
	fakeGH(t, nil)
	if _, err := ciDoctorRun(t, false, "--probe"); err == nil || !strings.Contains(err.Error(), "needs the repository") {
		t.Fatalf("no repo: %v", err)
	}
	for _, f := range []string{"--public", "--labels=self-hosted,macos,home", "--size=c4", "--os=linux"} {
		if _, err := ciDoctorRun(t, false, "judy/app", f); err == nil || !strings.Contains(err.Error(), "only go with --probe") {
			t.Fatalf("%s without --probe: %v", f, err)
		}
	}
	raw, err := ciDoctorRun(t, true, "judy/app")
	if err != nil || strings.Contains(raw, `"ask"`) || strings.Contains(raw, `"number": 18`) {
		t.Fatalf("plain doctor: %v\n%s", err, raw)
	}
}

// TestComputeDoctorProbeIsCLIJSON: compute_doctor's probe answers what
// zm ci doctor --probe --json prints, ask included, and cannot give the
// owner's yes.
func TestComputeDoctorProbeIsCLIJSON(t *testing.T) {
	newFakeBroker(t)
	fakeGH(t, map[string]string{"judy/site": "public"})
	t.Setenv(cli.EnvNoAutostart, "")
	raw, _ := ciDoctorRun(t, true, "judy/site", "--probe")
	c, _, err := newClientContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Autostart = nil
	v, err := computeProbe(context.Background(), c, "judy/site", mcp.ProbeRequest{Probe: true})
	if err != nil {
		t.Fatal(err)
	}
	var a, m any
	mb, _ := json.Marshal(v)
	if json.Unmarshal([]byte(raw), &a) != nil || json.Unmarshal(mb, &m) != nil {
		t.Fatalf("not JSON:\ncli %s\nmcp %s", raw, mb)
	}
	ab, _ := json.Marshal(a)
	mb, _ = json.Marshal(m)
	if string(ab) != string(mb) || !strings.Contains(string(mb), `"kind":"ask"`) {
		t.Fatalf("compute_doctor probe differs from zm ci doctor --probe --json, or gave the yes:\ncli %s\nmcp %s", ab, mb)
	}
}

// TestProbeScript: only the first report's check 18, only Potluck's
// probe.sh, unquoted.
func TestProbeScript(t *testing.T) {
	rep := func(checks ...runners.DoctorCheck) runners.DoctorReport {
		return runners.DoctorReport{Repo: "judy/app", Checks: checks}
	}
	fix := func(n int, f string) runners.DoctorCheck {
		return runners.DoctorCheck{Number: n, Status: "warn", Fix: f}
	}
	for _, tc := range []struct {
		res  runners.DoctorResult
		want string
	}{
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(fix(18, "sh '/tmp/a b/probe.sh'"))}}, "/tmp/a b/probe.sh"},
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(fix(17, "sh /tmp/a/probe.sh"))}}, ""},
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(fix(18, "sh /etc/passwd"))}}, ""},
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(), rep(fix(18, "sh /tmp/a/probe.sh"))}}, ""},
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(fix(1, ""), fix(18, "sh /tmp/a/probe.sh"))}}, "/tmp/a/probe.sh"},
		{runners.DoctorResult{Reports: []runners.DoctorReport{rep(fix(18, "/tmp/a/probe.sh"))}}, ""},
		{runners.DoctorResult{}, ""},
	} {
		if got := probeScript(tc.res); got != tc.want {
			t.Fatalf("%+v: %q, want %q", tc.res, got, tc.want)
		}
	}
}

func TestShellUnquote(t *testing.T) {
	for in, want := range map[string]string{"/tmp/a/probe.sh": "/tmp/a/probe.sh", "'/tmp/a b/probe.sh'": "/tmp/a b/probe.sh", `'/tmp/it'\''s/probe.sh'`: "/tmp/it's/probe.sh"} {
		if got := shellUnquote(in); got != want {
			t.Fatalf("%s: %q, want %q", in, got, want)
		}
	}
}

// TestPotluckError: Potluck's --json error keeps its code, in zm's words;
// plain text and silence are errors too, never an empty pass.
func TestPotluckError(t *testing.T) {
	var ae *api.Error
	err := potluckError([]byte(`{"error":{"code":"unavailable","message":"the potluck daemon is not running","hint":"start it with: potluck daemon --detach"}}`+"\n"), 3)
	if !errors.As(err, &ae) || ae.Code != api.CodeUnavailable || ae.Message != "the zm daemon is not running" || ae.Hint != "start it with: zm daemon --detach" {
		t.Fatalf("json: %#v", err)
	}
	if err := potluckError([]byte("potluck: unknown flag: --x\n"), 2); err == nil || err.Error() != "unknown flag: --x" {
		t.Fatalf("text: %v", err)
	}
	if err := potluckError(nil, 1); err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("silence: %v", err)
	}
}
