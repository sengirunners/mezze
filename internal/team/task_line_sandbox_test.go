package team_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// sandboxDriver is a farm machine's task driver (SPEC 8 "Where it runs"):
// like holdAgent, it appends "<name> <machine>" to dir/started and runs
// until dir/go-<name> exists; it keeps its stdin as dir/in-<name> and its
// environment as dir/env-<name>, and writes its result to ZM_TASK_RESULT.
// A name starting "fail" exits 3; on TERM it writes dir/term-<name> and
// exits, unless its name starts "stubborn" (it ignores TERM).
func (f *lineFarm) sandboxDriver(machine string) string {
	bin := filepath.Join(f.t.TempDir(), "sandbox-driver")
	d := f.dir
	script := `#!/bin/sh
in=$(cat)
n=$(printf '%s' "$in" | sed -n 's/.*"prompt":"hold \([A-Za-z0-9_-]*\).*/\1/p' | head -n 1)
printf '%s\n' "$in" > ` + d + `/in-$n
printf 'session=%s\ntask=%s\nresult=%s\ncwd=%s\nhome=%s\nleak=%s\n' "$ZM_SESSION" "$ZM_TASK_ID" "$ZM_TASK_RESULT" "$(pwd)" "${HOME:+set}" "$SANDBOX_TEST_LEAK" > ` + d + `/env-$n
case "$n" in
stubborn*) trap '' TERM ;;
*) trap 'echo term > ` + d + `/term-'"$n"'; printf released > "$ZM_TASK_RESULT"; exit 143' TERM ;;
esac
echo "$n ` + machine + `" >> ` + d + `/started
while [ ! -e ` + d + `/go-$n ]; do sleep 0.05; done
case "$n" in fail*) printf 'the box went away' > "$ZM_TASK_RESULT"; exit 3 ;; esac
printf 'sandbox done from ` + machine + `' > "$ZM_TASK_RESULT"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return bin
}

// joinSandbox joins machine as a farm machine with a task driver and slots
// sandbox slots (one plain slot).
func (f *lineFarm) joinSandbox(machine string, slots int) *daemon {
	f.t.Helper()
	drv := f.sandboxDriver(machine)
	return f.joinWith(machine, machine, "harbor", func(c *config.Config) {
		c.File.SandboxTaskCmd, c.File.SandboxTaskSlots = drv, slots
	})
}

func (f *lineFarm) startSandbox(c *client.Client, name, machine, size string, front bool) orchapi.Task {
	f.t.Helper()
	st, err := orchclient.TaskStart(f.ctx, c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "hold " + name, Machine: machine, Front: front,
		Sandbox: &orchapi.TaskSandbox{Size: size, Egress: []string{"pkg.example.com"}}, Work: ""})
	if err != nil || st.State == orchapi.TaskRefused {
		f.t.Fatalf("start sandbox %s: %+v %v", name, st, err)
	}
	return st
}

func (f *lineFarm) driverFile(kind, name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, kind+"-"+name))
	return string(b)
}

// The acceptance of the waiting line rerun with sandbox tasks: ten onto a
// machine with two sandbox slots, none refused, in order, a front start
// first. Sandbox tasks never go to a machine without a task driver, and a
// plain task runs on the sandbox machine's plain slot while both sandbox
// slots are taken. The driver gets the task on stdin and its variables;
// its result file is the result.
func TestSandboxLineTenOnTwoSlots(t *testing.T) {
	defer team.ReportEveryBeat()()
	t.Setenv("SANDBOX_TEST_LEAK", "the daemon's environment")
	f := newLineFarm(t)
	f.joinSandbox("box1", 2)
	f.join("box2", "box2", "harbor", true) // plain tasks only
	p, parentAddr := f.parent(f.a)
	ids := map[string]string{}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("s%d", i)
		size := ""
		if i == 0 {
			size = "c2m8"
		}
		ids[name] = f.startSandbox(p, name, "auto", size, false).ID
	}
	f.waitStarted(2)
	if got := f.startedNames(); !sameSet(got, []string{"s0", "s1"}) || f.startedOn("s0") != "box1" || f.startedOn("s1") != "box1" {
		t.Fatalf("first two: %v", f.started())
	}
	for i := 2; i < 10; i++ {
		name := fmt.Sprintf("s%d", i)
		if st := f.status(p, ids[name]); st.State != orchapi.TaskPending || st.Position != i-1 || st.Sandbox == nil {
			t.Fatalf("%s waits: %+v", name, st)
		}
	}
	// A plain task takes box1's plain slot (or box2): sandbox slots are apart.
	q, _ := f.parent(f.a)
	f.start(q, "plain", "box1", false)
	f.waitStarted(3)
	if f.startedOn("plain") != "box1" {
		t.Fatalf("plain: %v", f.started())
	}
	sf := f.startSandbox(p, "sf", "auto", "", true)
	ids["sf"] = sf.ID
	if st := f.status(p, sf.ID); st.Position != 1 {
		t.Fatalf("front: %+v", st)
	}
	// The driver's contract, read from s0 while it runs.
	var st orchapi.Task
	eventually(t, 10*time.Second, func() error {
		if st = f.status(p, ids["s0"]); st.Session == "" {
			return fmt.Errorf("no session yet: %+v", st)
		}
		return nil
	})
	env := f.driverFile("env", "s0")
	for _, want := range []string{"session=" + st.Session + "\n", "task=" + ids["s0"] + "\n", "home=set\n", "leak=\n"} {
		if !strings.Contains(env, want) {
			t.Fatalf("driver env lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "/worktrees/") || !strings.Contains(env, "/sandbox-tasks/") {
		t.Fatalf("driver cwd:\n%s", env)
	}
	var in struct {
		TaskID    string                   `json:"task_id"`
		OpID      string                   `json:"op_id"`
		Project   string                   `json:"project"`
		Parent    string                   `json:"parent"`
		TimeLeftS int                      `json:"time_left_s"`
		Request   orchapi.TaskStartRequest `json:"request"`
	}
	if err := json.Unmarshal([]byte(f.driverFile("in", "s0")), &in); err != nil || in.TaskID != ids["s0"] || in.OpID != ids["s0"] || in.Parent != parentAddr ||
		in.TimeLeftS <= 0 || in.Request.Prompt != "hold s0" || in.Request.Sandbox == nil || in.Request.Sandbox.Size != "c2m8" || in.Project == "" ||
		strings.Join(in.Request.Sandbox.Egress, ",") != "pkg.example.com" {
		t.Fatalf("driver stdin %+v: %v", in, err)
	}
	if st.Agent != "codex" || st.Sandbox == nil || st.Sandbox.Size != "c2m8" || st.Machine != "box1" {
		t.Fatalf("status: %+v", st)
	}
	// Free a slot at a time: the next sandbox task in line starts on box1.
	next := []string{"sf", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"}
	free := []string{"s0", "s1", "sf", "s2", "s3", "s4", "s5", "s6", "s7"}
	for k, name := range next {
		f.release(free[k])
		f.waitStarted(4 + k)
		if got := f.startedNames(); got[len(got)-1] != name || f.startedOn(name) != "box1" {
			t.Fatalf("after %s ended, %v started (want %s next, on box1)", free[k], f.started(), name)
		}
	}
	for _, n := range []string{"s8", "s9", "plain"} {
		f.release(n)
	}
	for name, id := range ids {
		st := waitRemoteTask(t, p, nil, id)
		if st.State != "ended" || st.Result == nil || st.Result.Text != "sandbox done from box1" || st.Result.Outcome.Kind != orchapi.OutcomeOK || st.Requeued != 0 {
			t.Fatalf("%s: %+v %+v", name, st, st.Result)
		}
	}
	// Its directory goes with it.
	if ents, _ := os.ReadDir(filepath.Join(f.ds["box1"].cfg.Home, "sandbox-tasks")); len(ents) != 0 {
		t.Fatalf("sandbox task directories left: %v", ents)
	}
}

// Refused at once: sandbox with run or continue_from, a size that is not a
// pool box size, sandbox with no machine, a pinned machine without a task
// driver (403, also one whose sandbox_task_cmd is not an absolute path),
// and no machine of the project taking sandbox tasks (503).
func TestSandboxLineRefusals(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	f.join("box2", "box2", "harbor", true) // no task driver
	// A task driver that is not an absolute path: the machine's zm still
	// starts, and it takes no sandbox tasks.
	f.joinWith("box3", "box3", "harbor", func(c *config.Config) { c.File.SandboxTaskCmd, c.File.SandboxTaskSlots = "farm/zm-task.mjs", 40 })
	p, _ := f.parent(f.a)
	sb := &orchapi.TaskSandbox{}
	for _, c := range []struct {
		req  orchapi.TaskStartRequest
		code string
		text string
	}{
		{orchapi.TaskStartRequest{Agent: "run", Machine: "auto", Run: &orchapi.TaskRun{Ref: "main", Script: "x.sh"}, Sandbox: sb}, api.CodeBadRequest, "not a script run"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "auto", ContinueFrom: "abcd1234", Sandbox: sb}, api.CodeBadRequest, "continue_from"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "auto", Sandbox: &orchapi.TaskSandbox{Size: "huge"}}, api.CodeBadRequest, "not a pool box size"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "auto", Sandbox: &orchapi.TaskSandbox{Egress: []string{"10.0.0.1"}}}, api.CodeBadRequest, "not a host name"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Sandbox: sb}, api.CodeBadRequest, "pass machine"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "box2", Sandbox: sb}, api.CodeForbidden, "takes no sandbox tasks"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "box3", Sandbox: sb}, api.CodeForbidden, "takes no sandbox tasks"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "auto", Sandbox: sb}, api.CodeUnavailable, "takes sandbox tasks"},
		{orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Machine: "auto", Cwd: "/tmp", Sandbox: sb}, api.CodeBadRequest, "leave cwd out"},
	} {
		st, err := orchclient.TaskStart(f.ctx, p, c.req)
		if !api.IsCode(err, c.code) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%+v: %+v %v (want %s %q)", c.req, st, err, c.code, c.text)
		}
	}
	// A plain start still goes through.
	f.start(p, "plain", "auto", false)
	f.waitStarted(1)
}

// A sandbox task whose only sandbox machine is lost ends lost at once,
// instead of waiting a day in the line for a machine that is gone. The
// machines' beats are the test's own and box1 is lost at a chosen instant on
// the authority's clock (the first one past opLostAfter), not by racing
// wall-clock beats against a short window.
func TestSandboxLineLostOnlyMachine(t *testing.T) {
	defer team.ReportEveryBeat()()
	t.Cleanup(team.BeatByHand()) // after the daemons stop
	f := newLineFarm(t)
	s := teamOf(t, f.a)
	project := mustf(orchclient.RemoteTasks(f.ctx, f.a.owner, "harbor"))(t).Project
	box1 := f.joinSandbox("box1", 1)
	box2 := f.join("box2", "box2", "harbor", true)
	p, _ := f.parent(f.a)
	heartbeat(t, box1)
	heartbeat(t, box2)
	ta := f.startSandbox(p, "a", "auto", "", false)
	f.waitStarted(1)
	f.lose("box1")
	// box2 still beats; box1 is judged lost at the first instant past
	// opLostAfter from its last beat: only its task is placed.
	heartbeat(t, box2)
	if n := team.LoseQuietPast(s, project, "box1"); n != 1 {
		t.Fatalf("the lost check judged %d placed tasks after box1 went, want 1", n)
	}
	var st orchapi.Task
	eventually(t, 20*time.Second, func() error {
		st = f.status(p, ta.ID)
		if st.State != "ended" || st.Lost == "" {
			return fmt.Errorf("not lost yet: %+v", st)
		}
		return nil
	})
	if !strings.Contains(st.Lost, "no other machine of the project takes sandbox tasks") || st.Requeued != 0 || len(f.started()) != 1 {
		t.Fatalf("lost: %+v %v", st, f.started())
	}
}

// Cancel stops the driver with TERM and gives it SandboxTermWait to
// release its box, then HUP (and KILL); a failing driver's result is its
// file's text.
func TestSandboxLineCancelAndFail(t *testing.T) {
	defer team.ReportEveryBeat()()
	old := session.SandboxTermWait
	session.SandboxTermWait = time.Second
	t.Cleanup(func() { session.SandboxTermWait = old })
	f := newLineFarm(t)
	f.joinSandbox("box1", 3)
	p, _ := f.parent(f.a)
	tc := f.startSandbox(p, "c", "auto", "", false)
	ts := f.startSandbox(p, "stubborn", "auto", "", false)
	tf := f.startSandbox(p, "fail", "auto", "", false)
	f.waitStarted(3)
	for _, id := range []string{tc.ID, ts.ID} {
		eventually(t, 10*time.Second, func() error {
			if st := f.status(p, id); st.Session == "" {
				return fmt.Errorf("no session yet: %+v", st)
			}
			return nil
		})
		if _, err := orchclient.TaskCancel(f.ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}
	st := waitRemoteTask(t, p, nil, tc.ID)
	if st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled || st.Result.Text != "released" || f.driverFile("term", "c") == "" {
		t.Fatalf("cancelled: %+v %+v", st, st.Result)
	}
	st = waitRemoteTask(t, p, nil, ts.ID)
	if st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled || st.Result.Outcome.Signal != "HUP" || f.driverFile("term", "stubborn") != "" {
		t.Fatalf("a driver that ignores TERM: %+v %+v", st, st.Result)
	}
	f.release("fail")
	st = waitRemoteTask(t, p, nil, tf.ID)
	if st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeFailed || st.Result.Text != "the box went away" ||
		st.Result.Finished != "not_done" || !strings.Contains(st.Result.Outcome.Evidence, "task driver exited 3") {
		t.Fatalf("failed: %+v %+v", st, st.Result)
	}
}

// A sandbox task answers like a missing one to a caller outside its scope
// (another parent), and an area guest's GET /v0/machines shows no more
// than before: only its own machine, not the sandbox machine's numbers.
func TestSandboxLineScope(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	f.joinSandbox("box1", 1)
	p, _ := f.parent(f.a)
	ta := f.startSandbox(p, "a", "auto", "", false)
	f.waitStarted(1)
	q, _ := f.parent(f.a)
	if st, err := orchclient.TaskStatus(f.ctx, q, ta.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("another parent reads it: %+v %v", st, err)
	}
	_, missing := orchclient.TaskStatus(f.ctx, q, "rt-01m70000000000000000000000", 0)
	if _, err := orchclient.TaskStatus(f.ctx, q, ta.ID, 0); err == nil || missing == nil || err.Error() != strings.Replace(missing.Error(), "rt-01m70000000000000000000000", ta.ID, 1) {
		t.Fatalf("hidden %v, missing %v", err, missing)
	}
	var l api.MachineList
	if err := f.a.owner.Do(f.ctx, "GET", "/v0/machines", nil, &l); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, m := range l.Machines {
		seen = seen || m.Machine == "box1" && m.Stats.SandboxTasks && m.Stats.SandboxSlots == 1
	}
	if !seen {
		t.Fatalf("the owner does not see box1's sandbox numbers: %+v", l.Machines)
	}
	mustf(f.a.owner.AddArea(f.ctx, "harbor", api.Area{Name: "solver", Work: []string{"bd:solver-*"}}))(t)
	g, _ := replica(t, f.tn, f.a, "guest-mac", "nGUEST", "harbor/solver", "vida", "guest")
	heartbeat(t, g)
	var gl api.MachineList
	if err := g.owner.Do(f.ctx, "GET", "/v0/machines", nil, &gl); err != nil {
		t.Fatal(err)
	}
	if len(gl.Machines) != 1 || gl.Machines[0].Machine != "guest-mac" || gl.Machines[0].Stats.SandboxTasks {
		t.Fatalf("the guest's machines: %+v", gl.Machines)
	}
	f.release("a")
	waitRemoteTask(t, p, nil, ta.ID)
}

// A daemon restart on the sandbox machine re-adopts the running driver (it
// is the task holder's child): the task ends with the driver's result.
func TestSandboxLineRestartReadopts(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	f.joinSandbox("box1", 1)
	p, _ := f.parent(f.a)
	ta := f.startSandbox(p, "a", "auto", "", false)
	f.waitStarted(1)
	eventually(t, 10*time.Second, func() error {
		if st := f.status(p, ta.ID); st.Session == "" {
			return fmt.Errorf("no session yet: %+v", st)
		}
		return nil
	})
	d := f.ds["box1"]
	d.dm.KeepSessions() // as cmd/zm's daemon: sessions outlive it (SPEC 8 "Daemon restarts")
	d.restart(f.tn, f.ids["box1"])
	f.release("a")
	st := waitRemoteTask(t, p, d, ta.ID)
	if st.State != "ended" || st.Result == nil || st.Result.Text != "sandbox done from box1" || st.Result.Outcome.Kind != orchapi.OutcomeOK || len(f.started()) != 1 {
		t.Fatalf("after a restart: %+v %+v %v", st, st.Result, f.started())
	}
}

// Task drivers have slots of their own: a machine whose session limit is
// full of plain tasks still starts a sandbox task in a free sandbox slot,
// and running drivers never take a plain task's place (max_managed_sessions
// 1, two sandbox slots). A model in the request reaches the driver on its
// stdin; it is not a flag zm adds to the driver's command.
func TestSandboxLineOwnSlotsAndModel(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	drv := f.sandboxDriver("box1")
	f.joinWith("box1", "box1", "harbor", func(c *config.Config) {
		c.File.SandboxTaskCmd, c.File.SandboxTaskSlots = drv, 2
		c.File.Admission.MaxManagedSessions = 1
	})
	p, _ := f.parent(f.a)
	startModel := func(name string) orchapi.Task {
		t.Helper()
		st, err := orchclient.TaskStart(f.ctx, p, orchapi.TaskStartRequest{Agent: "codex", Model: "gpt-5.5", Prompt: "hold " + name, Machine: "auto",
			Sandbox: &orchapi.TaskSandbox{}})
		if err != nil || st.State == orchapi.TaskRefused {
			t.Fatalf("start sandbox %s: %+v %v", name, st, err)
		}
		return st
	}
	s0, s1 := startModel("s0"), startModel("s1")
	f.waitStarted(2)
	var in struct {
		Request orchapi.TaskStartRequest `json:"request"`
	}
	if err := json.Unmarshal([]byte(f.driverFile("in", "s0")), &in); err != nil || in.Request.Model != "gpt-5.5" {
		t.Fatalf("driver stdin model: %+v %v", in.Request, err)
	}
	// Two drivers run; the one session the limit allows is still free.
	heartbeat(t, f.ds["box1"])
	time.Sleep(300 * time.Millisecond)
	f.start(p, "plain", "box1", false)
	f.waitStarted(3)
	if f.startedOn("plain") != "box1" {
		t.Fatalf("plain: %v", f.started())
	}
	// Now the session limit is full: a sandbox task waits only for a slot.
	heartbeat(t, f.ds["box1"])
	time.Sleep(300 * time.Millisecond)
	s2 := startModel("s2")
	f.release("s0")
	f.waitStarted(4)
	if got := f.startedNames(); got[3] != "s2" || f.startedOn("s2") != "box1" {
		t.Fatalf("s2 on the full machine: %v", f.started())
	}
	for _, n := range []string{"s1", "s2", "plain"} {
		f.release(n)
	}
	for _, id := range []string{s0.ID, s1.ID, s2.ID} {
		if st := waitRemoteTask(t, p, nil, id); st.State != "ended" || st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeOK {
			t.Fatalf("%s: %+v %+v", id, st, st.Result)
		}
	}
}
