package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// fakeTaskAgent is a one-shot agent: a prompt with "wait" in it hangs,
// else it reports "done from <machine>" in Codex's events.
func fakeTaskAgent(t *testing.T, machine string) func(*config.Config) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\np=$(cat)\ncase \"$p\" in *wait*) exec sleep 30;; esac\n" +
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done from ` + machine + `"}}' '{"type":"turn.completed","usage":{"input_tokens":4,"output_tokens":2,"cached_input_tokens":1}}'` + "\nsleep 0.2\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return func(c *config.Config) {
		roomy(c)
		c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}}
		c.File.RemoteTasks = true
	}
}

// waitRemoteTask beats r until c sees task id settle.
func waitRemoteTask(t *testing.T, c *client.Client, r *daemon, id string) orchapi.Task {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if r != nil {
			heartbeat(t, r)
		}
		st, err := orchclient.TaskStatus(context.Background(), c, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == "ended" || st.State == orchapi.TaskRefused {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never settled: %+v", id, st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// From a member Mac, the project's owner starts a task on another machine
// (SPEC 8 "Machine choice"): nothing connects to it, the authority queues
// the start and the machine's heartbeat carries it; status, read and cancel
// work from the Mac; the parent gets one mail when it ends. A machine that
// has not opted in, and a member who does not own the project, are refused.
func TestTaskOnAnotherMachine(t *testing.T) {
	quietPresence(t)
	defer team.ReportEveryBeat()()
	defer team.OpsPollOff()() // heartbeats alone carry ops here (task_line_test.go covers the poll)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, fakeTaskAgent(t, "mini"))
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: "member"}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", fakeTaskAgent(t, "box"))
	shy := join("shy", "shy", roomy) // never opted in
	mac := join("mac", "ivogh", roomy)
	bea := join("bea-mac", "bea", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)

	parentOn := func(d *daemon) *client.Client {
		ss := mustf(orchclient.Spawn(ctx, at(d.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
		return at(d.owner, "harbor", ss.Address)
	}
	pc := parentOn(mac)
	cc := parentOn(bea)
	for _, d := range []*daemon{box, shy, mac, bea} {
		heartbeat(t, d) // presence and machine numbers at the authority
	}
	no := false
	start := func(c *client.Client, machine, prompt string) (orchapi.Task, error) {
		return orchclient.TaskStart(ctx, c, orchapi.TaskStartRequest{Agent: "codex", Prompt: prompt, Worktree: &no, Machine: machine, Cwd: t.TempDir()})
	}

	// On box: pending until its heartbeat takes it, then it runs there.
	t1, err := start(pc, "box", "do it")
	if err != nil || !strings.HasPrefix(t1.ID, orchapi.RemoteTaskPrefix) || t1.Machine != "box" || t1.State != orchapi.TaskPending {
		t.Fatalf("start on box: %+v %v", t1, err)
	}
	if again, err := start(pc, "box", "do it"); err != nil || again.ID == t1.ID {
		// no client_request_id: a second start is a second task
		t.Fatalf("second start: %+v %v", again, err)
	}
	done := waitRemoteTask(t, pc, box, t1.ID)
	if done.State != "ended" || done.Result == nil || done.Result.Text != "done from box" || !strings.HasPrefix(done.Session, "box/") {
		t.Fatalf("task on box: %+v %+v", done, done.Result)
	}
	// The parent's mail arrives once, from box's task session.
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, box)
		ms, err := orchclient.Inbox(ctx, pc, orchapi.InboxQuery{})
		if err != nil {
			return err
		}
		n := 0
		for _, m := range ms {
			if strings.Contains(m.Body, "Task "+done.Session) {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("parent mail about %s: %d of %d", done.Session, n, len(ms))
		}
		return nil
	})
	// Another parent (the socket owner aside) never sees it.
	if _, err := orchclient.TaskStatus(ctx, at(mac.owner, "harbor", "mac/00000000"), t1.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("another session read the task: %v", err)
	}

	// Read goes to box and back (an unknown item answers like a missing one).
	readErr := make(chan error, 1)
	go func() {
		_, err := orchclient.TaskRead(ctx, pc, t1.ID, "task:nope", 0)
		readErr <- err
	}()
	for waiting := true; waiting; {
		select {
		case err := <-readErr:
			if err != nil && api.IsCode(err, api.CodeUnavailable) {
				t.Fatalf("read never reached box: %v", err)
			}
			waiting = false
		case <-time.After(100 * time.Millisecond):
			heartbeat(t, box)
		}
	}

	// Cancel: box stops it with its next heartbeat.
	t2 := mustf(start(pc, "box", "wait for it"))(t)
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, box)
		st, err := orchclient.TaskStatus(ctx, pc, t2.ID, 0)
		if err != nil || st.State != "running" {
			return fmt.Errorf("not running yet: %+v %v", st, err)
		}
		return nil
	})
	if _, err := orchclient.TaskCancel(ctx, pc, t2.ID); err != nil {
		t.Fatal(err)
	}
	if st := waitRemoteTask(t, pc, box, t2.ID); st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("cancelled: %+v", st)
	}
	// A start no heartbeat carried yet never runs.
	t3 := mustf(start(pc, "box", "wait"))(t)
	if st := mustf(orchclient.TaskCancel(ctx, pc, t3.ID))(t); st.State != orchapi.TaskRefused {
		t.Fatalf("cancel before it was taken: %+v", st)
	}

	// The authority's own machine runs it at once.
	if st := waitRemoteTask(t, pc, nil, mustf(start(pc, "mini", "do it"))(t).ID); st.Result == nil || st.Result.Text != "done from mini" {
		t.Fatalf("task on the authority: %+v", st)
	}
	// auto picks a machine that takes them (never the Mac or shy).
	if t4 := mustf(start(pc, "auto", "do it"))(t); t4.Machine != "box" && t4.Machine != "mini" {
		t.Fatalf("auto picked %q", t4.Machine)
	}

	// Refusals.
	if _, err := start(pc, "shy", "do it"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a machine that never opted in: %v", err)
	}
	if _, err := start(pc, "nowhere", "do it"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unknown machine: %v", err)
	}
	if _, err := start(cc, "box", "do it"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member who does not own the project: %v", err)
	}
	if _, err := orchclient.TaskStart(ctx, mac.owner, orchapi.TaskStartRequest{Agent: "codex", Prompt: "x", Machine: "box"}); err == nil {
		t.Fatal("a start with no parent session")
	}
}

// A parent outside any team project names one: the task
// goes through that project's authority onto its machine and is filed
// there, while the parent stays the caller's session (status, result and
// the ending mail come back to it). Left out, the parent's project still
// decides, as before. Who may is the authority's, against the named project.
func TestTaskInNamedProject(t *testing.T) {
	quietPresence(t)
	defer team.ReportEveryBeat()()
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, fakeTaskAgent(t, "mini"))
	harbor := mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: "member"}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", fakeTaskAgent(t, "box"))
	mac := join("mac", "ivogh", roomy)
	bea := join("bea-mac", "bea", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)
	// A second team project at the same authority, mac's owner owns it too.
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "pool"))(t)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "pool", As: "ivogh", Role: "member"}))(t)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "pool", Role: "owner"}))(t)
	// With project, a task runs in the machine's own checkout of it: its
	// project's most recent session there.
	checkout := func(d *daemon, project string) {
		mustf(orchclient.Spawn(ctx, at(d.owner, project, ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	}
	checkout(box, "harbor")
	checkout(a, "harbor")
	checkout(a, "pool")
	// A team project at another authority (big), mac's owner owns it.
	big := startDaemonWith(t, tn, "big", tailnettest.Identity{NodeID: "nBIG", NodeKey: "nodekey:big", Login: "big@example.com", Host: "big"}, discard, fakeTaskAgent(t, "big"))
	far := mustf(big.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "far"}))(t)
	mustf(big.owner.ProjectTeam(ctx, "far"))(t)
	finv := mustf(big.owner.Invite(ctx, api.InviteRequest{Project: "far", As: "ivogh", Role: "member"}))(t)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: finv.Code}))(t)
	mustf(big.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "far", Role: "owner"}))(t)
	checkout(big, "far")
	// Each parent sits in its own machine's solo project, in no team.
	parentIn := func(d *daemon) *client.Client {
		mustf(d.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "solo"}))(t)
		ss := mustf(orchclient.Spawn(ctx, at(d.owner, "solo", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
		return at(d.owner, "solo", ss.Address)
	}
	pc, cc := parentIn(mac), parentIn(bea)
	for _, d := range []*daemon{box, mac, bea} {
		heartbeat(t, d)
	}
	no := false
	start := func(c *client.Client, project, machine, prompt string) (orchapi.Task, error) {
		return orchclient.TaskStartIn(ctx, c, project, orchapi.TaskStartRequest{Agent: "codex", Prompt: prompt, Worktree: &no, Machine: machine})
	}

	// Left out: the parent's project, which is no team project (as before).
	if _, err := start(pc, "", "box", "do it"); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "needs a team project") {
		t.Fatalf("no project named: %v", err)
	}
	t1, err := start(pc, "harbor", "box", "do it")
	if err != nil || !strings.HasPrefix(t1.ID, orchapi.RemoteTaskPrefix) || t1.Machine != "box" {
		t.Fatalf("start in harbor on box: %+v %v", t1, err)
	}
	done := waitRemoteTask(t, pc, box, t1.ID)
	if done.State != "ended" || done.Result == nil || done.Result.Text != "done from box" || !strings.HasPrefix(done.Session, "box/") {
		t.Fatalf("task on box: %+v %+v", done, done.Result)
	}
	// Filed under harbor on box, so its members see it there.
	if ss := mustf(orchclient.Session(ctx, box.owner, done.Session))(t); ss.Project != harbor.ID {
		t.Fatalf("task session in %q, want harbor %q", ss.Project, harbor.ID)
	}
	// The parent, in solo, gets the ending mail once (filed in harbor, the
	// task's project: its whole inbox shows it, -P solo would not).
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, box)
		ms, err := orchclient.Inbox(ctx, at(mac.owner, "", pc.As), orchapi.InboxQuery{})
		if err != nil {
			return err
		}
		n := 0
		for _, m := range ms {
			if strings.Contains(m.Body, "Task "+done.Session) {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("parent mail about %s: %d of %d", done.Session, n, len(ms))
		}
		return nil
	})

	// Refusals: a project this machine does not know, a machine not of the
	// named project, a member who does not own it.
	if _, err := start(pc, "nope", "box", "do it"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unknown project: %v", err)
	}
	if _, err := start(pc, "harbor", "nowhere", "do it"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a machine not in the project: %v", err)
	}
	if _, err := start(cc, "harbor", "box", "do it"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member who does not own the project: %v", err)
	}

	// One client_request_id is one run per parent, whichever project it
	// names: another project conflicts; a lookup (zm exec --cancel) finds
	// it with no project or a wrong one.
	keyed := func(project string, lookup bool) (orchapi.Task, error) {
		return orchclient.TaskStartIn(ctx, pc, project, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no, Machine: "mini", ClientRequestID: "req-1", Lookup: lookup})
	}
	k1 := mustf(keyed("harbor", false))(t)
	if again := mustf(keyed("harbor", false))(t); again.ID != k1.ID {
		t.Fatalf("a retry started %s, not %s", again.ID, k1.ID)
	}
	if _, err := keyed("pool", false); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("the same request id in another project: %v", err)
	}
	for _, p := range []string{"", "pool", "nope"} {
		if got, err := keyed(p, true); err != nil || got.ID != k1.ID {
			t.Fatalf("lookup naming %q: %+v %v", p, got, err)
		}
	}

	// Per parent (SPEC 8): the authority asked keeps a 4th and later start
	// waiting in its line (across its projects); the parent's machine
	// refuses a start while 3 are unsettled at other authorities. Two more
	// in harbor make three; one in pool (same authority) waits; one in far
	// (another authority, which sees none of them) is refused.
	var live []string
	live = append(live, k1.ID)
	for i := 0; i < 2; i++ {
		live = append(live, mustf(start(pc, "harbor", "mini", "wait"))(t).ID)
	}
	if tk, err := start(pc, "pool", "mini", "wait"); err != nil || tk.State != orchapi.TaskPending {
		t.Fatalf("a fourth task in another project of the same authority waits: %+v %v", tk, err)
	} else {
		live = append(live, tk.ID)
	}
	if _, err := start(pc, "far", "big", "wait"); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "limit 3") {
		t.Fatalf("a fourth task at another authority: %v", err)
	}
	// A retry of a start that made its task is no new task; a request id
	// reserved by a start that made none (an earlier refusal) still counts.
	if again := mustf(keyed("harbor", false))(t); again.ID != k1.ID {
		t.Fatalf("a retry at the limit: %+v", again)
	}
	if _, err := mac.dm.Deps.Store.Registry().Exec(`INSERT INTO team_task_keys (parent, request_id, project, created) VALUES (?, 'req-stale', ?, 0)`, pc.As, far.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.TaskStartIn(ctx, pc, "far", orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no, Machine: "big", ClientRequestID: "req-stale"}); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "limit 3") {
		t.Fatalf("a reserved request id with no task at the limit: %v", err)
	}
	cancelAll := func(ids []string) {
		t.Helper()
		for _, id := range ids {
			if _, err := orchclient.TaskCancel(ctx, pc, id); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range ids {
			if st := waitRemoteTask(t, pc, nil, id); st.State != "ended" && st.State != orchapi.TaskRefused {
				t.Fatalf("cancelled: %+v", st)
			}
		}
	}
	cancelAll(live)
	// The other way round: three at far, then harbor and pool (the other
	// authority) refuse, and far itself takes a 4th to wait.
	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, mustf(start(pc, "far", "big", "wait"))(t).ID)
	}
	for _, p := range []string{"harbor", "pool"} {
		if _, err := start(pc, p, "mini", "wait"); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "limit 3") {
			t.Fatalf("a fourth task in %s with three at far: %v", p, err)
		}
	}
	got = append(got, mustf(start(pc, "far", "big", "wait"))(t).ID)
	cancelAll(got)
}

// A hostile member daemon asks the authority to start a task in a project
// its token does not reach: the same answer as for one that does not
// exist, by ID or by slug.
func TestTeamTasksRouteHidesProjects(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	secret := mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "secret"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "secret"))(t)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t)
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	ask := func(project string) string {
		st, body := rawCall(t, hc, http.MethodPost, "http://potluck-mini:7433/v0/team/tasks", token, orchapi.RemoteTaskRequest{Project: project, Machine: "mini", Parent: "bea-mac/aaaaaaaa", Depth: 1,
			Request: orchapi.TaskStartRequest{Agent: "codex", Prompt: "hi"}}, nil)
		return fmt.Sprintf("%d %s", st, strings.ReplaceAll(body, project, "NAME"))
	}
	missing := ask(strings.ToUpper(ids.New()))
	if !strings.HasPrefix(missing, "404 ") {
		t.Fatalf("a missing project: %s", missing)
	}
	for _, p := range []string{secret.ID, "secret"} {
		if got := ask(p); got != missing {
			t.Fatalf("a project outside the token (%s):\n%s\n%s", p, got, missing)
		}
	}
	if got := ask("nosuch"); got != missing {
		t.Fatalf("a missing slug:\n%s\n%s", got, missing)
	}
}
