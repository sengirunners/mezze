package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
)

const canaryV = "main-aaaaaaaaaaaa"

// Made-up machines: nothing here is a real box.
var canarySettings = rolloutSettings{
	Canary:        []string{"box-c"},
	Members:       []string{"box-m1", "box-m2"},
	Authority:     []string{"box-z"},
	IdleProcesses: []string{"agent-tool", "test-runner"},
}

func canaryBox(name string, held string, stable, failed bool) api.Machine {
	z := &api.MachineZm{Version: "main-old", Held: held}
	if stable {
		z.Stable = []string{canaryV}
	}
	if failed {
		z.Failed = []string{canaryV}
	}
	return api.Machine{Machine: name, Stats: api.MachineStats{Sessions: 0, Zm: z}}
}

func TestLoadRolloutSettings(t *testing.T) {
	home := t.TempDir()
	if _, ok, err := loadRolloutSettings(home); ok || err != nil {
		t.Fatalf("no file: ok=%v err=%v", ok, err)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(home, rolloutSettingsFile), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"canary":["box-c"],"members":["box-m1"],"authority":["box-z"],"idle_processes":["x"]}`)
	s, ok, err := loadRolloutSettings(home)
	if !ok || err != nil || s.roleOf("box-c") != roleCanary || s.roleOf("box-m1") != roleMember || s.roleOf("box-z") != roleAuthority || s.roleOf("other") != "" {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
	write(`{"canary":["box-c"],"authority":["box-c"]}`)
	if _, _, err := loadRolloutSettings(home); err == nil || !strings.Contains(err.Error(), "box-c") {
		t.Fatalf("one machine in two roles: %v", err)
	}
	write(`{"canary":[""]}`)
	if _, _, err := loadRolloutSettings(home); err == nil {
		t.Fatal("an empty name was accepted")
	}
	write(`not json`)
	if _, _, err := loadRolloutSettings(home); err == nil {
		t.Fatal("bad JSON was accepted")
	}
}

func TestJudgeIdle(t *testing.T) {
	if idle, why := judgeIdle(idleFacts{SessionsKnown: true}); !idle || len(why) != 0 {
		t.Fatalf("a quiet box: %v %v", idle, why)
	}
	cases := map[string]idleFacts{
		"unknown":    {},
		"sessions":   {SessionsKnown: true, Sessions: 2},
		"process":    {SessionsKnown: true, Running: []string{"test-runner"}},
		"unreadable": {SessionsKnown: true, ProcessesErr: "ps failed"},
	}
	for name, f := range cases {
		if idle, why := judgeIdle(f); idle || len(why) == 0 {
			t.Errorf("%s: judged idle (%v)", name, why)
		}
	}
}

func TestConfiguredRunningMatchesExactBaseNames(t *testing.T) {
	old := runningProcesses
	defer func() { runningProcesses = old }()
	runningProcesses = func() ([]string, error) {
		return []string{"/usr/bin/agent-tool --flag", "  test-runner-helper", "launchd", "", "python3 /opt/jobs/batch-job.py x", "a-program-name-longer-than-fifteen --x", "node --max-old-space-size=4 /srv/web-build.js"}, nil
	}
	got, errText := configuredRunning([]string{"agent-tool", "test-runner", "launchd", "batch-job.py", "a-program-name-longer-than-fifteen", "web-build.js"})
	if errText != "" || strings.Join(got, ",") != "agent-tool,launchd,batch-job.py,a-program-name-longer-than-fifteen,web-build.js" {
		t.Fatalf("got %v %q: a longer name must not match a shorter configured one", got, errText)
	}
	runningProcesses = func() ([]string, error) { return nil, errors.New("no ps") }
	if _, errText := configuredRunning([]string{"agent-tool"}); errText == "" {
		t.Fatal("a failed process list must be reported")
	}
	if got, errText := configuredRunning(nil); got != nil || errText != "" {
		t.Fatal("no configured names, nothing to read")
	}
}

func TestRolloutStageOrdersCanaryMembersAuthority(t *testing.T) {
	ms := func(c, m1, m2 bool, held string) []api.Machine {
		return []api.Machine{canaryBox("box-c", "", c, false), canaryBox("box-m1", "", m1, false), canaryBox("box-m2", held, m2, false), canaryBox("box-z", "", false, false)}
	}
	for _, tc := range []struct {
		name, role string
		ms         []api.Machine
		open       bool
	}{
		{"canary is always first", roleCanary, ms(false, false, false, ""), true},
		{"member waits for the canary", roleMember, ms(false, false, false, ""), false},
		{"member goes once the canary is stable", roleMember, ms(true, false, false, ""), true},
		{"authority waits for the canary", roleAuthority, ms(false, true, true, ""), false},
		{"authority waits for every member", roleAuthority, ms(true, true, false, ""), false},
		{"authority goes when all are stable", roleAuthority, ms(true, true, true, ""), true},
		{"a held member does not stop the authority", roleAuthority, ms(true, true, false, "held by hand"), true},
	} {
		if open, why := rolloutStage(canarySettings, tc.role, canaryV, tc.ms); open != tc.open {
			t.Errorf("%s: open=%v (%s)", tc.name, open, why)
		}
	}
	// A machine that failed the release stops it for everyone, canary included.
	failed := append(ms(true, true, true, ""), canaryBox("box-x", "", false, true))
	for _, role := range []string{roleCanary, roleMember, roleAuthority} {
		if open, why := rolloutStage(canarySettings, role, canaryV, failed); open || !strings.Contains(why, "box-x") {
			t.Errorf("%s: a failure did not stop it: %v %s", role, open, why)
		}
	}
	// A member that is there but reports no build (an older zm) cannot vouch: it holds the authority.
	older := []api.Machine{canaryBox("box-c", "", true, false), canaryBox("box-m1", "", true, false), {Machine: "box-m2"}}
	if open, why := rolloutStage(canarySettings, roleAuthority, canaryV, older); open || !strings.Contains(why, "box-m2") {
		t.Errorf("a build-less member did not hold the authority: %v %s", open, why)
	}
	// A member unreachable (not in the numbers) does not stop the authority.
	if open, _ := rolloutStage(canarySettings, roleAuthority, canaryV, []api.Machine{canaryBox("box-c", "", true, false), canaryBox("box-m1", "", true, false)}); !open {
		t.Error("an unreachable member stopped the authority")
	}
}

func canaryInputs(machine string, ms []api.Machine) dryInputs {
	s := canarySettings
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	return dryInputs{
		At: now, Machine: machine, Settings: &s,
		Running:    api.Health{OK: true, Version: "main-old", Commit: "0000000000000000000000000000000000000000", Machine: machine},
		Listing:    []listed{{Tag: "zm-main-aaaaaaaaaaaa", Version: canaryV, Published: now.Add(-time.Hour)}},
		Machines:   ms,
		Idle:       idleFacts{SessionsKnown: true},
		TickResult: "held", TickReason: "release is 1h0m0s old; this machine takes releases at 6h0m0s",
		PrevExists: true,
	}
}

func TestDryRecordSaysWhatTheRolloutWouldDo(t *testing.T) {
	stable := []api.Machine{canaryBox("box-c", "", true, false), canaryBox("box-m1", "", true, false), canaryBox("box-m2", "", true, false), canaryBox("box-z", "", false, false)}
	fresh := []api.Machine{canaryBox("box-c", "", false, false), canaryBox("box-m1", "", false, false), canaryBox("box-m2", "", false, false), canaryBox("box-z", "", false, false)}

	// The canary, idle, would install now, whatever the real tick held for.
	r := makeDryRecord(canaryInputs("box-c", fresh))
	if r.Would != "install" || r.Candidate != canaryV || r.Role != roleCanary || r.Tick.Result != "held" || r.Health == nil || r.Rollback == nil || !r.Rollback.Prev {
		t.Fatalf("canary: %+v", r)
	}
	if got := []string{r.Order[0].Machine, r.Order[1].Machine, r.Order[3].Machine}; got[0] != "box-c" || got[1] != "box-m1" || got[2] != "box-z" {
		t.Fatalf("order: %+v", r.Order)
	}

	// A listed machine missing from the numbers is shown as missing.
	in := canaryInputs("box-c", fresh[:2])
	r = makeDryRecord(in)
	missing := 0
	for _, b := range r.Order {
		if b.Missing {
			missing++
		}
	}
	if missing != 2 {
		t.Fatalf("missing boxes %d, want 2: %+v", missing, r.Order)
	}

	// A held box waits, whatever the stage.
	in = canaryInputs("box-c", fresh)
	in.Held = "hold file exists"
	if r := makeDryRecord(in); r.Would != "wait" || !strings.Contains(r.Why, "held") {
		t.Fatalf("held box: %+v", r)
	}

	// A busy canary waits, and says why.
	in = canaryInputs("box-c", fresh)
	in.Idle = idleFacts{SessionsKnown: true, Sessions: 3, Running: []string{"test-runner"}}
	if r := makeDryRecord(in); r.Would != "wait" || !strings.Contains(r.Why, "3 managed zm sessions") || !strings.Contains(r.Why, "test-runner") || r.Idle.Idle {
		t.Fatalf("busy canary: %+v", r)
	}

	// A member waits behind the canary, then goes.
	if r := makeDryRecord(canaryInputs("box-m1", fresh)); r.Would != "wait" || !strings.Contains(r.Why, "no canary") {
		t.Fatalf("member before the canary: %+v", r)
	}
	if r := makeDryRecord(canaryInputs("box-m1", stable)); r.Would != "install" {
		t.Fatalf("member after the canary: %+v", r)
	}

	// The authority goes last.
	if r := makeDryRecord(canaryInputs("box-z", fresh)); r.Would != "wait" {
		t.Fatalf("authority first: %+v", r)
	}
	if r := makeDryRecord(canaryInputs("box-z", stable)); r.Would != "install" {
		t.Fatalf("authority last: %+v", r)
	}

	// Unreadable machine numbers: the stage is closed, never assumed open.
	in = canaryInputs("box-c", nil)
	in.MachinesErr = "no route"
	if r := makeDryRecord(in); r.Would != "wait" || r.Stage.Open {
		t.Fatalf("unreadable numbers: %+v", r)
	}

	// No .prev beside the installed zm is said, not hidden.
	in = canaryInputs("box-c", fresh)
	in.PrevExists = false
	if r := makeDryRecord(in); !strings.Contains(r.Rollback.Would, "no .prev") {
		t.Fatalf("rollback: %+v", r.Rollback)
	}

	// A running daemon with a self-check problem shows in the health reading.
	in = canaryInputs("box-c", fresh)
	in.Running.SelfCheck = &api.SelfCheck{OK: false, Problems: []string{"a table is missing"}}
	if r := makeDryRecord(in); r.Health.Now.OK || len(r.Health.Now.Problems) != 1 {
		t.Fatalf("health now: %+v", r.Health)
	}
}

func TestDryRecordSkipsWhatItCannotJudge(t *testing.T) {
	in := canaryInputs("box-c", nil)
	in.Settings = nil
	if r := makeDryRecord(in); r.Would != "nothing" || !strings.Contains(r.Why, rolloutSettingsFile) {
		t.Fatalf("no settings: %+v", r)
	}
	in.SettingErr = "bad"
	if r := makeDryRecord(in); !strings.Contains(r.Why, "unusable") {
		t.Fatalf("bad settings: %+v", r)
	}
	if r := makeDryRecord(canaryInputs("laptop", nil)); r.Would != "skip" || r.Role != "" {
		t.Fatalf("a machine in no list: %+v", r)
	}
	in = canaryInputs("box-c", nil)
	in.ListErr = "gh: not logged in"
	if r := makeDryRecord(in); r.Would != "nothing" || !strings.Contains(r.Why, "listing") {
		t.Fatalf("listing failed: %+v", r)
	}
	// The running build is already the newest release: nothing to take.
	in = canaryInputs("box-c", nil)
	in.Running.Commit = "aaaaaaaaaaaa0000000000000000000000000000"
	if r := makeDryRecord(in); r.Would != "nothing" || r.Candidate != "" {
		t.Fatalf("up to date: %+v", r)
	}
	// A release that failed on this box is never a candidate.
	in = canaryInputs("box-c", nil)
	in.HaveState = true
	in.State = release.State{Version: "main-older", Created: in.At.Add(-48 * time.Hour)}
	in.State.MarkFailed(release.Failure{Version: canaryV, At: in.At, Result: "failed"})
	if r := makeDryRecord(in); r.Would != "nothing" {
		t.Fatalf("failed here: %+v", r)
	}
}

func TestAppendDryRecordKeepsTheLogBounded(t *testing.T) {
	home := t.TempDir()
	rec := makeDryRecord(canaryInputs("box-c", nil))
	if err := appendDryRecord(home, rec); err != nil {
		t.Fatal(err)
	}
	if err := appendDryRecord(home, rec); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(home, rolloutDryRunFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var back dryRecord
		if json.Unmarshal(sc.Bytes(), &back) != nil || back.Machine != "box-c" {
			t.Fatalf("line %q", sc.Text())
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d lines, want 2", n)
	}
	if fi, _ := f.Stat(); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Past the bound the file moves aside and a new one starts.
	big := filepath.Join(home, rolloutDryRunFile)
	if err := os.WriteFile(big, make([]byte, rolloutDryRunMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendDryRecord(home, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(big + ".1"); err != nil {
		t.Fatalf("old log not kept: %v", err)
	}
	if fi, _ := os.Stat(big); fi.Size() > 4096 {
		t.Fatalf("new log is %d bytes", fi.Size())
	}
}

func readDryLog(t *testing.T, home string) []dryRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, rolloutDryRunFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []dryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r dryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestDryRunRolloutLogsAndChangesNothingElse(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	var w strings.Builder
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	health := api.Health{OK: true, Version: "main-old", Commit: "0000000000000000000000000000000000000000", Machine: "box-c"}
	rl := &listRecorder{seen: []listed{{Tag: "zm-main-aaaaaaaaaaaa", Version: canaryV, Published: now.Add(-time.Hour)}}}

	// No settings file: no rollout, no line, and the box is not touched.
	dryRunRollout(context.Background(), &w, nil, home, dir, health, rl, release.State{}, false, "held", "ring", nil, now)
	if _, err := os.Stat(filepath.Join(home, rolloutDryRunFile)); !os.IsNotExist(err) {
		t.Fatalf("a home with no rollout settings logged: %v", err)
	}
	// Unusable settings do get a line: the owner must hear that they are wrong.
	if err := os.WriteFile(filepath.Join(home, rolloutSettingsFile), []byte(`{"canary":["box-c"],"authority":["box-c"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dryRunRollout(context.Background(), &w, nil, home, dir, health, rl, release.State{}, false, "held", "ring", nil, now)
	if recs := readDryLog(t, home); len(recs) != 1 || recs[0].Would != "nothing" || !strings.Contains(recs[0].Why, "unusable") || recs[0].Tick.Result != "held" {
		t.Fatalf("%+v", recs)
	}

	// Settings present but the machine numbers unreachable: the stage is
	// closed and the record says so; nothing returns an error.
	if err := os.WriteFile(filepath.Join(home, rolloutSettingsFile), []byte(`{"canary":["box-c"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newDeadClient(t)
	dryRunRollout(context.Background(), &w, c, home, dir, health, rl, release.State{}, false, "", "", errors.New("tick broke"), now)
	recs := readDryLog(t, home)
	if len(recs) != 2 || recs[1].Would != "wait" || recs[1].Tick.Error != "tick broke" || recs[1].Stage == nil || recs[1].Stage.Open {
		t.Fatalf("%+v", recs[1])
	}
	if w.Len() != 0 {
		t.Fatalf("stderr: %s", w.String())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("the dry run wrote into the install directory: %v", ents)
	}
}

// newDeadClient is a client whose socket nobody listens on.
func newDeadClient(t *testing.T) *client.Client {
	t.Helper()
	d, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return client.NewUnix(filepath.Join(d, "zm.sock"))
}

func TestDryRunRolloutContainsAPanic(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, rolloutSettingsFile), []byte(`{"canary":["box-c"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var w strings.Builder
	// A nil client with settings present panics inside; the caller must not see it.
	dryRunRollout(context.Background(), &w, nil, home, t.TempDir(), api.Health{Machine: "box-c"}, &listRecorder{}, release.State{}, false, "", "", nil, time.Now())
	if !strings.Contains(w.String(), "canary dry run") {
		t.Fatalf("stderr: %q", w.String())
	}
}

// Machine numbers the authority could not merge (Skipped) close the stage:
// a machine missing from them might hold a failure.
func TestSkippedMachineNumbersCloseTheStage(t *testing.T) {
	ml := api.MachineList{Machines: []api.Machine{canaryBox("box-c", "", false, false)}, Skipped: []api.WorkSkipped{{Error: "authority unreachable"}}}
	ms, why := usableMachines(ml, nil)
	if why == "" || !strings.Contains(why, "incomplete") || len(ms) != 1 {
		t.Fatalf("%v %q", ms, why)
	}
	if _, why := usableMachines(api.MachineList{}, errors.New("no route")); why != "no route" {
		t.Fatalf("%q", why)
	}
	if _, why := usableMachines(api.MachineList{Machines: ml.Machines}, nil); why != "" {
		t.Fatalf("complete numbers: %q", why)
	}
	in := canaryInputs("box-c", ms)
	_, in.MachinesErr = usableMachines(ml, nil)
	if r := makeDryRecord(in); r.Would != "wait" || r.Stage.Open {
		t.Fatalf("incomplete numbers opened the stage: %+v", r)
	}
}
