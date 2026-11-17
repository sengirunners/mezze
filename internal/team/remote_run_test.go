package team_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// git runs git in dir for a test, failing it on an error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A script run (SPEC 8 "Script runs") from a member Mac on a farm box: the
// box resolves the ref on its checkout's remote (not its local branch, which
// is ahead), runs the script from a clean checkout of exactly that commit
// (not its dirty working tree) with literal arguments and only the
// allowlisted environment, reports the script's own exit code, serves the
// whole output as task_read item "output", and removes the checkout. A
// caller's commit that is not the ref's is refused; a deadline ends the run
// with exit 124; a run with no machine is refused.
func TestScriptRunOnAnotherMachine(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	t.Setenv("EXEC_CANARY_SECRET", "leaked")
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The project's remote at a pushed commit, and the box's clone of it:
	// dirty, and with a local main ahead of the remote.
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitT(t, root, "init", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "clone", "--quiet", remote, seed)
	script := "#!/bin/sh\necho \"pushed $#:$1|$2\"\necho \"secret=${EXEC_CANARY_SECRET:-none}\"\nread line && echo \"stdin=$line\"\necho oops >&2\ncase \"$1\" in sleep) exec sleep 30;; bg) sleep 30 & echo started-bg;; exit*) exit ${1#exit};; esac\n"
	if err := os.MkdirAll(filepath.Join(seed, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "scripts", "hello.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/echo", filepath.Join(seed, "scripts", "evil")); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "--quiet", "-m", "hello")
	gitT(t, seed, "push", "--quiet", "origin", "main")
	pushed := gitT(t, seed, "rev-parse", "HEAD")
	boxClone := filepath.Join(root, "box-clone")
	gitT(t, root, "clone", "--quiet", remote, boxClone)
	if err := os.WriteFile(filepath.Join(boxClone, "scripts", "hello.sh"), []byte("#!/bin/sh\necho local-unpushed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, boxClone, "commit", "--quiet", "-am", "local only")
	if err := os.WriteFile(filepath.Join(boxClone, "scripts", "hello.sh"), []byte("#!/bin/sh\necho dirty-tree\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The authority takes remote tasks too: a lookup naming it must not run one there.
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: "member"}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	mac := join("mac", "ivogh", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)
	// The box knows its checkout from a session there.
	mustf(orchclient.Spawn(ctx, at(box.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: boxClone}))(t)
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	pc := at(mac.owner, "harbor", ss.Address)
	heartbeat(t, box)
	heartbeat(t, mac)

	run := func(c *client.Client, machine string, timeout int, r orchapi.TaskRun) (orchapi.Task, error) {
		return orchclient.TaskStart(ctx, c, orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: machine, TimeoutSeconds: timeout, Run: &r})
	}
	output := func(id string) string {
		t.Helper()
		var page orchapi.TaskHistoryPage
		eventually(t, 15*time.Second, func() error {
			heartbeat(t, box)
			var err error
			page, err = orchclient.TaskRead(ctx, pc, id, orchapi.TaskRunOutput, 0)
			return err
		})
		if len(page.Items) != 1 {
			t.Fatalf("output: %+v", page)
		}
		return page.Items[0].Text
	}
	runsLeft := func() []string {
		es, _ := os.ReadDir(filepath.Join(box.cfg.Home, "runs"))
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}

	// A good run: the pushed script, literal args, allowlisted env, empty stdin.
	t1 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"a b", "$HOME"}}))(t)
	done := waitRemoteTask(t, pc, box, t1.ID)
	if done.State != "ended" || done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Result.Outcome.ExitCode == nil || *done.Result.Outcome.ExitCode != 0 {
		t.Fatalf("run: %+v %+v", done, done.Result)
	}
	out := output(t1.ID)
	for _, want := range []string{"pushed 2:a b|$HOME", "secret=none", "oops"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"local-unpushed", "dirty-tree", "leaked", "stdin="} {
		if strings.Contains(out, not) {
			t.Fatalf("output has %q:\n%s", not, out)
		}
	}
	if !strings.Contains(done.Result.Text, "pushed 2") {
		t.Fatalf("result text: %q", done.Result.Text)
	}
	eventually(t, 10*time.Second, func() error {
		if left := runsLeft(); len(left) != 0 {
			return &api.Error{Code: api.CodeConflict, Message: "checkouts left: " + strings.Join(left, ",")}
		}
		return nil
	})

	// The script's own exit code.
	t2 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"exit3"}}))(t)
	if st := waitRemoteTask(t, pc, box, t2.ID); st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeFailed || st.Result.Outcome.ExitCode == nil || *st.Result.Outcome.ExitCode != 3 {
		t.Fatalf("exit 3: %+v", st.Result)
	}

	// A commit the ref does not resolve to is refused on the box.
	t3 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Commit: strings.Repeat("0", 40), Script: "scripts/hello.sh"}))(t)
	if st := waitRemoteTask(t, pc, box, t3.ID); st.State != orchapi.TaskRefused || !strings.Contains(st.Error, pushed) {
		t.Fatalf("wrong commit: %+v", st)
	}
	// An unpushed ref, and a script outside the repository.
	t4 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "nope", Script: "scripts/hello.sh"}))(t)
	if st := waitRemoteTask(t, pc, box, t4.ID); st.State != orchapi.TaskRefused {
		t.Fatalf("unpushed ref: %+v", st)
	}
	if _, err := run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "../etc/passwd"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("script outside the repository: %v", err)
	}
	if _, err := run(pc, "box", 0, orchapi.TaskRun{Ref: "refs/pull/1/head", Script: "scripts/hello.sh"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a full ref name: %v", err)
	}
	if _, err := orchclient.TaskStart(ctx, pc, orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Run: &orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a run with no machine: %v", err)
	}
	if _, err := orchclient.TaskStart(ctx, pc, orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "box", Prompt: "hi", Run: &orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a run with a prompt: %v", err)
	}

	// A symlink out of the checkout is refused on the box.
	t6 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "scripts/evil"}))(t)
	if st := waitRemoteTask(t, pc, box, t6.ID); st.State != orchapi.TaskRefused || !strings.Contains(st.Error, "inside the repository") {
		t.Fatalf("symlink out of the checkout: %+v", st)
	}

	// Cancel ends a running script as killed and removes its checkout.
	t7 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"sleep"}}))(t)
	eventually(t, 15*time.Second, func() error {
		heartbeat(t, box)
		if st, err := orchclient.TaskStatus(ctx, pc, t7.ID, 0); err != nil || st.State != "running" {
			return &api.Error{Code: api.CodeConflict, Message: "not running yet"}
		}
		return nil
	})
	mustf(orchclient.TaskCancel(ctx, pc, t7.ID))(t)
	if st := waitRemoteTask(t, pc, box, t7.ID); st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("cancelled run: %+v", st.Result)
	}

	// A background child the script leaves behind: the run still ends, and
	// its group is stopped with it.
	t8 := mustf(run(pc, "box", 0, orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"bg"}}))(t)
	if st := waitRemoteTask(t, pc, box, t8.ID); st.State != "ended" || st.Result == nil {
		t.Fatalf("run with a background child: %+v", st)
	}

	running := func(id string) {
		t.Helper()
		eventually(t, 15*time.Second, func() error {
			heartbeat(t, box)
			if st, err := orchclient.TaskStatus(ctx, pc, id, 0); err != nil || st.State != "running" {
				return &api.Error{Code: api.CodeConflict, Message: "not running yet"}
			}
			return nil
		})
	}

	// bc is the parent's client with the box beating while a cancel waits
	// for its confirmation (no background beat in these tests).
	bc := &beatingClient{Client: pc, beat: func() { heartbeat(t, box) }}

	// zm exec --cancel never starts a run: an unknown
	// request id is not_found, on the box and on the authority's own
	// machine (where a start runs at once), and a second lookup still finds
	// nothing.
	for _, m := range []string{"mini", "box"} {
		unknown := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: m, ClientRequestID: "exec-never-started-" + m,
			Run: &orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"sleep"}}}
		for i := 0; i < 2; i++ {
			if c, err := orchclient.CancelRun(ctx, bc, unknown); !api.IsCode(err, api.CodeNotFound) {
				t.Fatalf("--cancel of an unknown request on %s (%d): %+v %v", m, i, c, err)
			}
		}
	}

	// zm exec SIGKILLed mid-run: nothing in it cancels, so the run goes on;
	// zm exec --cancel <request-id> (client.CancelRun) with the same request
	// finds it by its request id and ends it killed, confirmed.
	ra := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "box", ClientRequestID: "exec-killed-mid-run",
		Run: &orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"sleep"}}}
	ta := mustf(orchclient.StartRun(ctx, orchclient.Tasks(pc), ra))(t)
	running(ta.ID)
	other := ra
	other.Run = &orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"exit3"}}
	if _, err := orchclient.CancelRun(ctx, bc, other); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("cancel with a different run's request: %v", err)
	}
	if st := mustf(orchclient.TaskStatus(ctx, pc, ta.ID, 0))(t); st.State != "running" {
		t.Fatalf("the run before --cancel: %+v", st)
	}
	if c := mustf(orchclient.CancelRun(ctx, bc, ra))(t); c.ID != ta.ID || c.Result == nil || c.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("cancel of %s: %+v %+v", ta.ID, c, c.Result)
	}
	if c := mustf(orchclient.CancelRun(ctx, bc, ra))(t); c.ID != ta.ID || c.State != "ended" {
		t.Fatalf("a second cancel: %+v", c)
	}

	// A cancel that reaches the box while its start is still in the
	// checkout (a slow first mirror clone): the start refuses, and the
	// script never runs.
	entered := make(chan string, 1)
	session.SetPrepareRunHook(func(hctx context.Context, opID string) {
		select {
		case entered <- opID:
		default:
		}
		select {
		case <-hctx.Done():
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { session.SetPrepareRunHook(nil) })
	rp := ra
	rp.ClientRequestID = "exec-cancel-while-preparing"
	tp := mustf(orchclient.TaskStart(ctx, pc, rp))(t)
	eventually(t, 15*time.Second, func() error {
		heartbeat(t, box)
		select {
		case id := <-entered:
			entered <- id
			return nil
		default:
			return &api.Error{Code: api.CodeConflict, Message: "the box has not begun its checkout"}
		}
	})
	if c, err := orchclient.CancelRun(ctx, bc, rp); err != nil || c.ID != tp.ID || c.State != orchapi.TaskRefused || c.Result != nil || !strings.Contains(c.Error, "cancelled before it started") {
		t.Fatalf("cancel while the box prepares: %+v %v", c, err)
	}
	session.SetPrepareRunHook(nil)
	for i := 0; i < 3; i++ { // later beats change nothing: the script never started
		heartbeat(t, box)
		if st := mustf(orchclient.TaskStatus(ctx, pc, tp.ID, 0))(t); st.State != orchapi.TaskRefused || st.Session != "" {
			t.Fatalf("after a cancel while preparing: %+v", st)
		}
	}

	// A signal while the start is in flight, after the authority queued it
	// (here: after the box took it): the run is cancelled, not left going.
	sctx, signalled := context.WithCancel(ctx)
	race := &signalDuringStart{beatingClient: bc, signal: signalled, started: running}
	rb := ra
	rb.ClientRequestID = "exec-signal-during-start"
	if _, err := orchclient.StartRun(sctx, race, rb); err == nil || sctx.Err() == nil {
		t.Fatalf("start after a signal: %v", err)
	}
	if race.id == "" {
		t.Fatal("the start never reached the authority")
	}
	if st := waitRemoteTask(t, pc, box, race.id); st.State != orchapi.TaskRefused && (st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled) {
		t.Fatalf("run after a signal during its start: %+v %+v", st, st.Result)
	}

	// On the authority's own machine a start runs at once (runHere): its
	// checkout runs inside the start request.
	miniClone := filepath.Join(root, "mini-clone")
	gitT(t, root, "clone", "--quiet", remote, miniClone)
	mustf(orchclient.Spawn(ctx, at(a.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: miniClone}))(t)
	slowCheckout := func() chan string {
		ch := make(chan string, 1)
		session.SetPrepareRunHook(func(hctx context.Context, opID string) {
			select {
			case ch <- opID:
			default:
			}
			select {
			case <-hctx.Done():
			case <-time.After(20 * time.Second):
			}
		})
		return ch
	}
	neverRan := func(what, rid string) {
		t.Helper()
		lr := ra
		lr.Machine, lr.ClientRequestID, lr.Lookup = "mini", rid, true
		for i := 0; i < 3; i++ {
			if st, err := orchclient.TaskStart(ctx, pc, lr); err != nil || st.State != orchapi.TaskRefused || st.Session != "" || st.Result != nil || !strings.Contains(st.Error, "cancelled before it started") {
				t.Fatalf("%s: %+v %v", what, st, err)
			}
		}
	}

	// A cancel during that checkout reaches the start's gate on the
	// authority: the start refuses, and the script never runs.
	entered = slowCheckout()
	rm := ra
	rm.Machine, rm.ClientRequestID = "mini", "exec-cancel-while-mini-prepares"
	startDone := make(chan orchapi.Task, 1)
	go func() {
		st, _ := orchclient.TaskStart(ctx, pc, rm)
		startDone <- st
	}()
	<-entered
	if c, err := orchclient.CancelRun(ctx, orchclient.Tasks(pc), rm); err != nil || c.State != orchapi.TaskRefused || !strings.Contains(c.Error, "cancelled before it started") {
		t.Fatalf("cancel while the authority prepares: %+v %v", c, err)
	}
	if st := <-startDone; st.State != orchapi.TaskRefused || st.Session != "" {
		t.Fatalf("the start cancelled while the authority prepared: %+v", st)
	}
	neverRan("after a cancel while the authority prepared", rm.ClientRequestID)

	// A signal (Ctrl-C) during that checkout cuts the start request: the
	// start is still recorded, found and cancelled; it never runs.
	entered = slowCheckout()
	sctx, signalled = context.WithCancel(ctx)
	go func() {
		<-entered
		signalled()
	}()
	rs := ra
	rs.Machine, rs.ClientRequestID = "mini", "exec-signal-while-mini-prepares"
	if _, err := orchclient.StartRun(sctx, orchclient.Tasks(pc), rs); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("a signal while the authority prepares: %v", err)
	}
	neverRan("after a signal while the authority prepared", rs.ClientRequestID)

	// The resend after a signal overtook the first start and became it: its
	// request runs the checkout past its bound (here 1 s). Lookups find the
	// run, and it is cancelled.
	entered = slowCheckout()
	sctx, signalled = context.WithCancel(ctx)
	over := &resendOvertakes{Client: pc, signal: signalled}
	ro := ra
	ro.Machine, ro.ClientRequestID = "mini", "exec-resend-overtakes-on-mini"
	if _, err := orchclient.StartRun(sctx, over, ro); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("a signal whose resend ran the authority's checkout: %v", err)
	}
	if over.lookups == 0 {
		t.Fatal("the run was found without a lookup")
	}
	neverRan("after a signal whose resend ran the authority's checkout", ro.ClientRequestID)
	session.SetPrepareRunHook(nil)

	// An orphaned checkout (a crash between checkout and task end) is swept.
	orphan := filepath.Join(box.cfg.Home, "runs", "rt-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	// The deadline ends it with exit 124 and removes the checkout.
	t5 := mustf(run(pc, "box", 1, orchapi.TaskRun{Ref: "main", Script: "scripts/hello.sh", Args: []string{"sleep"}}))(t)
	st := waitRemoteTask(t, pc, box, t5.ID)
	if st.Result == nil || st.Result.Outcome.Kind != orchapi.OutcomeKilled || st.Result.Outcome.ExitCode == nil || *st.Result.Outcome.ExitCode != 124 {
		t.Fatalf("deadline: state=%s error=%q result=%+v", st.State, st.Error, st.Result)
	}
	eventually(t, 10*time.Second, func() error {
		if left := runsLeft(); len(left) != 0 {
			return &api.Error{Code: api.CodeConflict, Message: "checkouts left: " + strings.Join(left, ",")}
		}
		return nil
	})
	if wt := gitT(t, boxClone, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Fatalf("worktrees left on the box's clone:\n%s", wt)
	}
}

// signalDuringStart is a client whose first start reaches the authority and
// then loses to a signal: the caller sees its context's error, not the task.
type signalDuringStart struct {
	*beatingClient
	signal  context.CancelFunc
	started func(id string)
	id      string
}

func (s *signalDuringStart) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if s.id != "" {
		return orchclient.TaskStart(ctx, s.Client, r)
	}
	t, err := orchclient.TaskStart(context.WithoutCancel(ctx), s.Client, r)
	if err != nil {
		return t, err
	}
	s.id = t.ID
	s.started(t.ID)
	s.signal()
	return orchapi.Task{}, ctx.Err()
}

// resendOvertakes is a client whose first start is lost to a signal before
// it leaves, and whose resend times out after 1 s (on the authority's own
// machine it is the start, running its checkout); lookups go through.
type resendOvertakes struct {
	*client.Client
	signal  context.CancelFunc
	sent    bool
	lookups int
}

func (s *resendOvertakes) TaskStatus(ctx context.Context, id string, wait int) (orchapi.Task, error) {
	return orchclient.TaskStatus(ctx, s.Client, id, wait)
}

func (s *resendOvertakes) TaskCancel(ctx context.Context, id string) (orchapi.Task, error) {
	return orchclient.TaskCancel(ctx, s.Client, id)
}

func (s *resendOvertakes) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	switch {
	case r.Lookup:
		s.lookups++
		return orchclient.TaskStart(ctx, s.Client, r)
	case !s.sent:
		s.sent = true
		s.signal()
		return orchapi.Task{}, ctx.Err()
	}
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return orchclient.TaskStart(rctx, s.Client, r)
}

// beatingClient is a parent's client that beats the box before each status read,
// so a cancel's confirmation sees the box's reports.
type beatingClient struct {
	*client.Client
	beat func()
}

func (b *beatingClient) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	return orchclient.TaskStart(ctx, b.Client, r)
}

func (b *beatingClient) TaskCancel(ctx context.Context, id string) (orchapi.Task, error) {
	return orchclient.TaskCancel(ctx, b.Client, id)
}

func (b *beatingClient) TaskStatus(ctx context.Context, id string, wait int) (orchapi.Task, error) {
	b.beat()
	return orchclient.TaskStatus(ctx, b.Client, id, wait)
}
