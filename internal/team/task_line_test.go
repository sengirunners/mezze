package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// lineFarm is an authority (mini, which takes no remote tasks) and farm
// machines that do, all in one test: the waiting line's (SPEC 8 "Waiting
// line") fixture. A task's prompt is "hold <name>": its agent appends
// "<name> <machine>" to dir/started and runs until dir/go-<name> exists.
type lineFarm struct {
	t   *testing.T
	ctx context.Context
	tn  *tailnettest.Net
	a   *daemon
	dir string
	ids map[string]tailnettest.Identity
	ds  map[string]*daemon
}

func newLineFarm(t *testing.T) *lineFarm {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmline")
	if err != nil {
		t.Fatal(err)
	}
	f := &lineFarm{t: t, ctx: context.Background(), tn: tailnettest.New(), dir: dir, ids: map[string]tailnettest.Identity{}, ds: map[string]*daemon{}}
	// Every held agent exits at the end (a halted daemon's task too), then
	// the directory goes.
	t.Cleanup(func() {
		for _, n := range f.startedNames() {
			f.release(n)
		}
		time.Sleep(300 * time.Millisecond)
		os.RemoveAll(dir)
	})
	id := tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}
	f.ids["mini"] = id
	f.a = startDaemonWith(t, f.tn, "mini", id, slog.New(slog.NewTextHandler(io.Discard, nil)), roomy)
	mustf(f.a.owner.CreateProject(f.ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(f.a.owner.ProjectTeam(f.ctx, "harbor"))(t)
	return f
}

// holdAgent is a farm machine's agent (see lineFarm).
func (f *lineFarm) holdAgent(machine string) func(*config.Config) {
	bin := filepath.Join(f.t.TempDir(), "hold-agent")
	script := "#!/bin/sh\np=$(cat)\nn=$(printf '%s\\n' \"$p\" | sed -n 's/^hold \\([A-Za-z0-9_-]*\\).*/\\1/p' | head -n 1)\necho \"$n " + machine + "\" >> " + f.dir + "/started\n" +
		"while [ ! -e " + f.dir + "/go-$n ]; do sleep 0.05; done\n" +
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done from ` + machine + `"}}' '{"type":"turn.completed","usage":{"input_tokens":4,"output_tokens":2,"cached_input_tokens":1}}'` + "\nsleep 0.2\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return func(c *config.Config) {
		roomy(c)
		c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}}
		c.File.RemoteTasks = true
	}
}

// join starts machine as a member of project (as identity as); farm
// machines take remote tasks.
func (f *lineFarm) join(machine, as, project string, farm bool) *daemon {
	f.t.Helper()
	mut := roomy
	if farm {
		mut = f.holdAgent(machine)
	}
	id := tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}
	f.ids[machine] = id
	d := startDaemonWith(f.t, f.tn, machine, id, slog.New(slog.NewTextHandler(io.Discard, nil)), mut)
	inv := mustf(f.a.owner.Invite(f.ctx, api.InviteRequest{Project: project, As: as, Role: "member"}))(f.t)
	mustf(d.owner.Join(f.ctx, api.JoinRequest{Code: inv.Code}))(f.t)
	heartbeat(f.t, d) // presence and machine numbers at the authority
	f.ds[machine] = d
	return d
}

// parent is a live session on d in harbor, as a client.
func (f *lineFarm) parent(d *daemon) (*client.Client, string) {
	ss := mustf(orchclient.Spawn(f.ctx, at(d.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 600"}, Cwd: f.t.TempDir()}))(f.t)
	return at(d.owner, "harbor", ss.Address), ss.Address
}

func (f *lineFarm) start(c *client.Client, name, machine string, front bool) orchapi.Task {
	f.t.Helper()
	no := false
	st, err := orchclient.TaskStart(f.ctx, c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "hold " + name, Worktree: &no, Machine: machine, Cwd: f.t.TempDir(), Front: front})
	if err != nil || st.State == orchapi.TaskRefused {
		f.t.Fatalf("start %s: %+v %v", name, st, err)
	}
	return st
}

// started are the "<name> <machine>" lines in start order.
func (f *lineFarm) started() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "started"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "\x00"))
}

// startedNames are the started tasks' names, in order.
func (f *lineFarm) startedNames() []string {
	var out []string
	for _, l := range f.started() {
		n, _, _ := strings.Cut(l, "\x00")
		out = append(out, n)
	}
	return out
}

// startedOn is the machine name's agent started on ("" none yet; the last).
func (f *lineFarm) startedOn(name string) string {
	on := ""
	for _, l := range f.started() {
		if n, m, _ := strings.Cut(l, "\x00"); n == name {
			on = m
		}
	}
	return on
}

// lose stops machine's daemon for good (its home stays until cleanup).
func (f *lineFarm) lose(machine string) {
	d := f.ds[machine]
	d.halt()
	d.halt = func() {}
}

func (f *lineFarm) release(name string) {
	_ = os.WriteFile(filepath.Join(f.dir, "go-"+name), nil, 0o600)
}

// waitStarted waits until n agents have started, answering how long it took.
func (f *lineFarm) waitStarted(n int) time.Duration {
	f.t.Helper()
	t0 := time.Now()
	eventually(f.t, 20*time.Second, func() error {
		if got := len(f.started()); got < n {
			return fmt.Errorf("%d of %d started: %v", got, n, f.startedNames())
		}
		return nil
	})
	return time.Since(t0)
}

func (f *lineFarm) status(c *client.Client, id string) orchapi.Task {
	f.t.Helper()
	return mustf(orchclient.TaskStatus(f.ctx, c, id, 0))(f.t)
}

// Ten tasks onto four machines (one slot each) from two parents: none is
// refused; the first four run at once, the rest wait with their places and
// start in line order as slots free, each within about a second of its
// slot freeing; a front start goes first; a cancelled waiting one leaves the
// line and never runs; the list shows the line in order.
func TestWaitingLineTenOnFour(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	for i := 1; i <= 4; i++ {
		f.join(fmt.Sprintf("box%d", i), fmt.Sprintf("box%d", i), "harbor", true)
	}
	p0, _ := f.parent(f.a)
	p1, _ := f.parent(f.a)
	parents := []*client.Client{p0, p1}
	ids := map[string]string{}
	by := map[string]*client.Client{}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("t%d", i)
		c := parents[i%2]
		st := f.start(c, name, "auto", false)
		ids[name], by[name] = st.ID, c
	}
	f.waitStarted(4)
	if got := f.startedNames(); !sameSet(got, []string{"t0", "t1", "t2", "t3"}) {
		t.Fatalf("first four: %v", got)
	}
	for i := 4; i < 10; i++ {
		name := fmt.Sprintf("t%d", i)
		if st := f.status(by[name], ids[name]); st.State != orchapi.TaskPending || st.Position != i-3 || st.Machine != "" {
			t.Fatalf("%s waits: %+v", name, st)
		}
	}
	// Front jumps the line; a cancelled waiting start leaves it.
	tf := f.start(p1, "tf", "auto", true)
	ids["tf"], by["tf"] = tf.ID, p1
	if st := f.status(p1, tf.ID); st.Position != 1 {
		t.Fatalf("front: %+v", st)
	}
	tc := f.start(p0, "tc", "auto", false)
	if st := mustf(orchclient.TaskCancel(f.ctx, p0, tc.ID))(t); st.State != orchapi.TaskRefused || !strings.Contains(st.Error, "waiting in the line") {
		t.Fatalf("cancel a waiting start: %+v", st)
	}
	// The owner's list: the line in order, then the placed ones.
	l := mustf(orchclient.RemoteTasks(f.ctx, f.a.owner, "harbor"))(t)
	var order []string
	for _, x := range l.Tasks {
		if x.Position > 0 {
			order = append(order, x.ID)
		}
	}
	want := []string{ids["tf"]}
	for i := 4; i < 10; i++ {
		want = append(want, ids[fmt.Sprintf("t%d", i)])
	}
	if !slices.Equal(order, want) || len(l.Tasks) < 11 || l.Tasks[7].Machine == "" || l.Tasks[7].State == orchapi.TaskPending && l.Tasks[7].Position > 0 {
		t.Fatalf("list: %+v (want the line %v first)", l.Tasks, want)
	}
	// Free one slot at a time: the next in line starts, within about a
	// second of the slot freeing.
	next := []string{"tf", "t4", "t5", "t6", "t7", "t8", "t9"}
	free := []string{"t0", "t1", "t2", "t3", "tf", "t4", "t5"}
	var worst time.Duration
	for k, name := range next {
		f.release(free[k])
		took := f.waitStarted(5 + k)
		worst = max(worst, took)
		if got := f.startedNames(); got[len(got)-1] != name {
			t.Fatalf("after %s ended, %v started (want %s next)", free[k], got, name)
		}
	}
	t.Logf("slowest start after a slot freed (the agent's own 0.2 s exit included): %s", worst)
	if worst > 4*time.Second {
		t.Errorf("a waiting task started %s after its slot freed", worst)
	}
	for _, n := range []string{"t6", "t7", "t8", "t9"} {
		f.release(n)
	}
	for name, id := range ids {
		st := waitRemoteTask(t, by[name], nil, id)
		if st.State != "ended" || st.Result == nil || !strings.HasPrefix(st.Result.Text, "done from box") || st.Requeued != 0 {
			t.Fatalf("%s: %+v %+v", name, st, st.Result)
		}
	}
	if slices.Contains(f.startedNames(), "tc") {
		t.Fatal("the cancelled waiting start ran")
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// A pinned start waits for its machine (and holds up nothing behind it);
// a machine whose daemon joined only another project of the authority never
// gets the project's tasks, however free it is.
func TestWaitingLinePinAndProject(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	mustf(f.a.owner.CreateProject(f.ctx, api.NewProjectRequest{Slug: "side"}))(t)
	mustf(f.a.owner.ProjectTeam(f.ctx, "side"))(t)
	f.join("box1", "box1", "harbor", true)
	f.join("box2", "box2", "harbor", true)
	f.join("other", "other", "side", true) // takes remote tasks, but not in harbor
	p, _ := f.parent(f.a)

	a := f.start(p, "a", "box2", false) // box2 busy with a
	f.waitStarted(1)
	pin := f.start(p, "pin", "box2", false)
	b := f.start(p, "b", "auto", false) // not held up by pin: box1 is free
	f.waitStarted(2)
	if f.startedOn("b") != "box1" {
		t.Fatalf("b started on %q: %v", f.startedOn("b"), f.started())
	}
	if st := f.status(p, pin.ID); st.State != orchapi.TaskPending || st.Machine != "box2" || st.Position != 1 {
		t.Fatalf("pinned start waits for box2: %+v", st)
	}
	// Both harbor machines busy: an auto start waits, never onto other.
	c := f.start(p, "c", "auto", false)
	time.Sleep(1500 * time.Millisecond) // a few sweeps and beats
	if st := f.status(p, c.ID); st.State != orchapi.TaskPending || st.Position != 2 || f.startedOn("c") != "" {
		t.Fatalf("c waits: %+v %v", st, f.started())
	}
	// box2 frees: the pinned start goes there; box1 frees: c goes there.
	f.release("a")
	f.waitStarted(3)
	if f.startedOn("pin") != "box2" {
		t.Fatalf("pin started on %q", f.startedOn("pin"))
	}
	f.release("b")
	f.waitStarted(4)
	if f.startedOn("c") != "box1" {
		t.Fatalf("c started on %q", f.startedOn("c"))
	}
	for _, n := range []string{"pin", "c"} {
		f.release(n)
	}
	for _, id := range []string{a.ID, pin.ID, b.ID, c.ID} {
		if st := waitRemoteTask(t, p, nil, id); st.State != "ended" || st.Machine == "other" {
			t.Fatalf("%s: %+v", id, st)
		}
	}
	for _, l := range f.started() {
		if strings.HasSuffix(l, "\x00other") {
			t.Fatalf("other ran a task of harbor: %v", f.started())
		}
	}
	// The list is the project's owner's: a member is refused, a machine not
	// in the project gets it like a missing one.
	bea := f.join("bea", "bea", "harbor", false)
	if _, err := orchclient.RemoteTasks(f.ctx, bea.owner, "harbor"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member lists the line: %v", err)
	}
	_, hid := orchclient.RemoteTasks(f.ctx, f.ds["other"].owner, "harbor")
	_, mis := orchclient.RemoteTasks(f.ctx, f.ds["other"].owner, "nope-zz")
	if !api.IsCode(hid, api.CodeNotFound) || strings.ReplaceAll(hid.Error(), "harbor", "X") != strings.ReplaceAll(mis.Error(), "nope-zz", "X") {
		t.Fatalf("another project's machine: %v, missing: %v", hid, mis)
	}
	// A pin that can never be served is refused at once, as before.
	no := false
	if _, err := orchclient.TaskStart(f.ctx, p, orchapi.TaskStartRequest{Agent: "codex", Prompt: "hold x", Worktree: &no, Machine: "other", Cwd: t.TempDir()}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("pinned to a machine outside the project: %v", err)
	}
}

// A machine lost mid-task: its task goes back to the line once and runs on
// another machine; lost again, it ends lost. The machines' beats are the
// test's own and a machine is lost at a chosen instant on the authority's
// clock (the first one past opLostAfter), not by racing wall-clock beats
// against a short window; the real sweep's own judgement is
// TestWaitingLineSweepLosesQuietMachine.
func TestWaitingLineLostMachine(t *testing.T) {
	defer team.ReportEveryBeat()()
	t.Cleanup(team.BeatByHand()) // after the daemons stop
	f, s, project := lineRows(t)
	for _, m := range []string{"box1", "box2", "box3"} {
		f.join(m, m, "harbor", true)
	}
	p, parentAddr := f.parent(f.a)
	// beat is every machine not lost beating, as live machines do.
	down := map[string]bool{}
	beat := func() {
		for _, m := range []string{"box1", "box2", "box3"} {
			if !down[m] {
				heartbeat(t, f.ds[m])
			}
		}
	}
	// lose takes machine away, and the authority judges it lost at the first
	// instant past opLostAfter from its last beat: only its task is placed.
	lose := func(machine string) {
		t.Helper()
		f.lose(machine)
		down[machine] = true
		if n := team.LoseQuietPast(s, project, machine); n != 1 {
			t.Fatalf("the lost check judged %d placed tasks after %s went, want 1", n, machine)
		}
	}

	ta := f.start(p, "a", "auto", false)
	f.waitStarted(1)
	x := f.startedOn("a")
	var st orchapi.Task
	eventually(t, 10*time.Second, func() error {
		beat()
		if st = f.status(p, ta.ID); st.Session == "" {
			return fmt.Errorf("no session yet: %+v", st)
		}
		return nil
	})
	oldCopy := st.Session
	lose(x) // its machine goes away mid-task
	f.waitStarted(2)
	y := f.startedOn("a")
	st = f.status(p, ta.ID)
	if y == x || st.Requeued != 1 || st.Machine != y || st.State == "ended" {
		t.Fatalf("after %s was lost: started on %s, %+v", x, y, st)
	}
	// x wakes (a laptop opened): its copy of a no longer acts.
	f.ds[x].restart(f.tn, f.ids[x])
	waitReachable(t, f.ds[x], true) // a write before then would wait in its outbox (checked below)
	if _, err := orchclient.Send(f.ctx, at(f.ds[x].owner, "harbor", oldCopy), orchapi.SendMailRequest{To: parentAddr, Body: "late word from the old copy"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("the lost copy's mail: %v", err)
	}
	// Nor through its outbox: a write queued while x cannot reach the
	// authority is refused when it is replayed.
	f.tn.Cut("potluck-"+x, "potluck-mini", true)
	waitReachable(t, f.ds[x], false)
	if _, err := orchclient.Send(f.ctx, at(f.ds[x].owner, "harbor", oldCopy), orchapi.SendMailRequest{To: parentAddr, Body: "queued word from the old copy", ClientRequestID: "old-copy-queued"}); err != nil {
		t.Fatalf("queue the old copy's mail: %v", err)
	}
	f.tn.Cut("potluck-"+x, "potluck-mini", false)
	waitDrained(t, f.ds[x])
	// Find the queued write by its request id: the old copy's own process
	// may report its kill into the same outbox after the drain, newest first.
	es := mustf(at(f.ds[x].owner, "harbor", "").Outbox(f.ctx, "harbor", true))(t)
	q := slices.IndexFunc(es, func(e api.OutboxEntry) bool {
		return strings.Contains(string(e.Body), `"client_request_id":"old-copy-queued"`)
	})
	if q < 0 || es[q].State == "done" || !strings.Contains(es[q].Error, "no longer acts") {
		t.Fatalf("the old copy's queued mail: %+v", es)
	}
	// y went quiet too: it went back once already.
	beat()
	lose(y)
	eventually(t, 20*time.Second, func() error {
		st = f.status(p, ta.ID)
		if st.State != "ended" || st.Lost == "" {
			return fmt.Errorf("not lost yet: %+v", st)
		}
		return nil
	})
	if !strings.Contains(st.Lost, "once already") || st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeInfra || len(f.started()) != 2 {
		t.Fatalf("lost twice: %+v %+v %v", st, st.Result, f.started())
	}
}

// A task whose session wrote through the authority (an outward action) ends
// lost with its machine instead of running again. One machine, so the task
// can only be on it; lost at a chosen instant, as in TestWaitingLineLostMachine.
func TestWaitingLineLostAfterOutwardAction(t *testing.T) {
	defer team.ReportEveryBeat()()
	t.Cleanup(team.BeatByHand()) // after the daemons stop
	f, s, project := lineRows(t)
	z := f.join("box1", "box1", "harbor", true)
	p, _ := f.parent(f.a)
	tb := f.start(p, "b", "auto", false)
	f.waitStarted(1)
	var st orchapi.Task
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, z)
		st = f.status(p, tb.ID)
		if st.Session == "" || st.State != "running" {
			return fmt.Errorf("not running yet: %+v", st)
		}
		return nil
	})
	// Any write but a harmless one is outward: a lease too.
	if _, err := orchclient.Lease(f.ctx, at(z.owner, "harbor", st.Session), orchapi.ClaimRequest{Key: "lock:deploy", TTLSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if st = f.status(p, tb.ID); !slices.Contains(st.Acted, "claim") {
		t.Fatalf("the lease is not an outward action: %+v", st)
	}
	f.lose("box1")
	if n := team.LoseQuietPast(s, project, "box1"); n != 1 {
		t.Fatalf("the lost check judged %d placed tasks, want 1", n)
	}
	eventually(t, 20*time.Second, func() error {
		st = f.status(p, tb.ID)
		if st.State != "ended" || st.Lost == "" {
			return fmt.Errorf("not lost yet: %+v", st)
		}
		return nil
	})
	if !strings.Contains(st.Lost, "outward action (claim)") || st.Requeued != 0 {
		t.Fatalf("after an outward action: %+v", st)
	}
}

// The real sweep (sweepLine -> loseQuiet at the wall clock) sends a task
// back to the line when its machine goes quiet. The test beats for box1 until
// it carried the task, then never again, so the short window loses it for
// good and can only be late; a loss while it still beats (a stalled runner)
// ends the same way, so the test tolerates it.
func TestWaitingLineSweepLosesQuietMachine(t *testing.T) {
	defer team.ReportEveryBeat()()
	t.Cleanup(team.SetLine(300*time.Millisecond, 50*time.Millisecond)) // after the daemons stop: their sweep reads it
	t.Cleanup(team.BeatByHand())
	f := newLineFarm(t)
	box := f.join("box1", "box1", "harbor", true)
	p, _ := f.parent(f.a)
	ta := f.start(p, "a", "auto", false)
	var st orchapi.Task
	// Beat until box1 took the task and reported its session (or the sweep
	// already lost it), then no more: a loss before the machine carried the
	// task goes back without counting, to a machine now quiet.
	eventually(t, 20*time.Second, func() error {
		heartbeat(t, box)
		if st = f.status(p, ta.ID); (st.Machine != "box1" || st.Session == "") && st.Requeued == 0 {
			return fmt.Errorf("not running yet: %+v", st)
		}
		return nil
	})
	eventually(t, 20*time.Second, func() error {
		if st = f.status(p, ta.ID); st.Machine == "box1" || st.Requeued != 1 {
			return fmt.Errorf("not lost yet: %+v", st)
		}
		return nil
	})
	if st.State != orchapi.TaskPending || st.Lost != "" {
		t.Fatalf("a task sent back to the line: %+v", st)
	}
}

// The line survives an authority restart: order, front and places kept,
// and the waiting starts run in order once the machine frees.
func TestWaitingLineSurvivesRestart(t *testing.T) {
	defer team.ReportEveryBeat()()
	f := newLineFarm(t)
	f.join("box", "box", "harbor", true)
	mac := f.join("mac", "ivogh", "harbor", false)
	mustf(f.a.owner.Grant(f.ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)
	p, _ := f.parent(mac)
	heartbeat(t, mac)
	t0 := f.start(p, "t0", "auto", false)
	f.waitStarted(1)
	t1 := f.start(p, "t1", "auto", false)
	t2 := f.start(p, "t2", "auto", false)
	t3 := f.start(p, "t3", "auto", true)

	f.a.restart(f.tn, f.ids["mini"])
	for id, pos := range map[string]int{t3.ID: 1, t1.ID: 2, t2.ID: 3} {
		if st := f.status(p, id); st.State != orchapi.TaskPending || st.Position != pos {
			t.Fatalf("after the restart %s: %+v (want place %d)", id, st, pos)
		}
	}
	for k, name := range []string{"t0", "t3", "t1"} {
		f.release(name)
		f.waitStarted(2 + k)
	}
	if got := f.startedNames(); !slices.Equal(got, []string{"t0", "t3", "t1", "t2"}) {
		t.Fatalf("order after the restart: %v", got)
	}
	f.release("t2")
	for _, id := range []string{t0.ID, t1.ID, t2.ID, t3.ID} {
		if st := waitRemoteTask(t, p, nil, id); st.State != "ended" || st.Result == nil {
			t.Fatalf("%s: %+v", id, st)
		}
	}
}
