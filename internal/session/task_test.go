package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestLateTaskExitAfterStoreShutdownKeepsProcessOutcome(t *testing.T) {
	st, err := storetest.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Store: st}}
	exit := 3
	want := orchapi.Outcome{Kind: orchapi.OutcomeFailed, ExitCode: &exit, Evidence: "process exited with status 3"}
	got := s.taskExitOutcome(context.Background(), "finished-task", "codex", want, 0)
	if got.Kind != want.Kind || got.ExitCode == nil || *got.ExitCode != 3 || got.Evidence != want.Evidence {
		t.Fatalf("late exit lost process evidence: %+v", got)
	}
}

func taskHarness(t *testing.T, script string) (*harness, orchapi.Session) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}} })
	parent := h.spawn("shell", "exec sleep 30", nil)
	return h, parent
}
func taskClient(h *harness, parent orchapi.Session) { h.c.As = parent.Address; h.c.Cwd = h.home }
func waitTask(t *testing.T, h *harness, id string) orchapi.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, err := orchclient.TaskStatus(context.Background(), h.c, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == "ended" {
			return v
		}
	}
	t.Fatalf("task did not end: %s", taskCaptureDiagnostic(h, id))
	return orchapi.Task{}
}
func TestTaskLifecycleAndIdempotency(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":4,"output_tokens":2,"cached_input_tokens":1}}'; sleep 0.2`)
	taskClient(h, parent)
	no := false
	req := orchapi.TaskStartRequest{Agent: "codex", Prompt: "do it", Worktree: &no, ClientRequestID: "retry"}
	first, err := orchclient.TaskStart(context.Background(), h.c, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := orchclient.TaskStart(context.Background(), h.c, req)
	if err != nil || again.ID != first.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	req.Prompt = "different"
	if _, err = orchclient.TaskStart(context.Background(), h.c, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	done := waitTask(t, h, first.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Result.Text != "done" || done.Result.Usage.InputTokens != 4 {
		t.Fatalf("result: %+v", done.Result)
	}
}
func TestTaskCancelAndTimeout(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "timeout"}[cancel], func(t *testing.T) {
			h, parent := taskHarness(t, `exec sleep 30`)
			taskClient(h, parent)
			no := false
			if !cancel {
				h.s.TaskNoOutput = 300 * time.Millisecond
			}
			v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "wait", Worktree: &no})
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				if _, err = orchclient.TaskCancel(context.Background(), h.c, v.ID); err != nil {
					t.Fatal(err)
				}
			}
			done := waitTask(t, h, v.ID)
			want := orchapi.OutcomeKilled
			if done.Result == nil || done.Result.Outcome.Kind != want {
				t.Fatalf("result: %+v", done.Result)
			}
		})
	}
}
func TestTaskParentLimit(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	for i := 0; i < taskParentLimit; i++ {
		if _, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "extra", Worktree: &no}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("limit: %v", err)
	}
}

func TestTaskDeliveryReportsQueued(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}' ; sleep 0.2`)
	var mu sync.Mutex
	var calls []string
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, to, body string) error {
		mu.Lock()
		calls = append(calls, kind)
		mu.Unlock()
		if kind == "comment" {
			return api.Errorf(api.CodeQueued, "queued")
		}
		return nil
	}

	taskClient(h, parent)
	no := false
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "do it", Work: "bd:fake-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err = orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v.MailDelivery == "sent" && v.CommentDelivery == "queued" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v.MailDelivery != "sent" || v.CommentDelivery != "queued" {
		t.Fatalf("delivery %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("delivery calls: %v", calls)
	}
}

func TestTaskDepthLimit(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "one", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = child.Session
	grandchild, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "two", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = grandchild.Session
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "three", Worktree: &no}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("depth: %v", err)
	}
}

func TestTaskSurvivesDaemonRestart(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "fake-agent")
	script := `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"before restart"}}'
sleep 2
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":3}}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "restart", Worktree: &no, ClientRequestID: "restart-key"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h1.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(child.ID))
	h1.waitOutput(task.ID, "before restart")
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	done := waitTask(t, h2, task.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Result.Text != "before restart" || done.Result.Usage.InputTokens != 11 {
		t.Fatalf("recovered: %+v", done)
	}
	retry, err := orchclient.TaskStart(context.Background(), h2.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "restart", Worktree: &no, ClientRequestID: "restart-key"})
	if err != nil || retry.ID != task.ID {
		t.Fatalf("retry: %+v %v", retry, err)
	}
}

type fakeTaskIssues struct {
	orchcore.IssueService
	got string
}

func (f *fakeTaskIssues) NativeCall(_ context.Context, _ string, req orchapi.AdapterRequest, _ any) error {
	f.got = req.ID
	return nil
}

func TestTaskDeliveryFallsBackToNativeIssue(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}' ; sleep 0.2`)
	native := &fakeTaskIssues{}
	orchcore.Wire(h.d).Issues = native
	taskClient(h, parent)
	no := false
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "do it", Work: "probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err = orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v.CommentDelivery == "sent" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v.CommentDelivery != "sent" || native.got != "probe-1" {
		t.Fatalf("delivery: %+v, native id %q", v, native.got)
	}
}

func TestTaskScrubsRedactedIssueText(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"sensitive text example"}'; sleep 0.2`)
	taskClient(h, parent)
	no := false
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "sensitive text example", Work: "bd:probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"sensitive text example"}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.d.ScrubAll(context.Background(), sc)
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := orchclient.Output(context.Background(), h.c, v.ID, 0)
	if err != nil || strings.Contains(out.Data, "sensitive text example") {
		t.Fatalf("scrollback not scrubbed: %+v %v", out, err)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.Result == nil || got.Result.Text != core.Redacted {
		t.Fatalf("result: %+v %v", got, err)
	}
	var transcript, request, command string
	err = h.d.Store.Registry().QueryRowContext(context.Background(), `SELECT t.transcript,t.request_json,s.command FROM session_tasks t JOIN sessions s ON s.id=t.id WHERE t.id=?`, v.ID).Scan(&transcript, &request, &command)
	if err != nil || strings.Contains(transcript+request+command, "sensitive text example") {
		t.Fatalf("copies not scrubbed: %v", err)
	}
}

func TestTaskScrubShortTextOnlyInLinkedEvents(t *testing.T) {
	source := `{"type":"result","result":"no"}` + "\n" + `{"type":"system","message":"no","other":"no"}` + "\n"
	got := scrubTaskTranscript(source, []string{"no"}, true)
	if !strings.Contains(got, `"result":"[redacted]"`) || !strings.Contains(got, `"other":"[redacted]"`) {
		t.Fatalf("scrub: %s", got)
	}
	if got := scrubTaskTranscript(source, []string{"no"}, false); got != source {
		t.Fatalf("unlinked scrub: %s", got)
	}
}

func TestTaskRedactionWaitsForLiveOutput(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	var mu sync.Mutex
	calls := 0
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind != "mail" {
			return nil
		}
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Work: "probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"sensitive text"}}
	if err := h.d.ScrubAll(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("live task: %v", err)
	}
	if _, err := orchclient.TaskCancel(context.Background(), h.c, v.ID); err != nil {
		t.Fatal(err)
	}
	h.s.deliverTask(v.ID)
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 0 {
		t.Fatalf("delivered during pending redaction: %d", before)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.d.ScrubAll(context.Background(), sc)
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatalf("ended task: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.s.deliverTask(v.ID)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != 1 {
		t.Fatalf("delivery after redaction: %d", after)
	}
}

func TestTaskDeliveryWaitsForAllPendingRedactions(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	var mu sync.Mutex
	calls := 0
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind != "mail" {
			return nil
		}
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Work: "probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	first := core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"first secret"}}
	second := core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"second secret"}}
	for _, sc := range []core.Scrub{first, second} {
		if err := h.d.ScrubAll(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
			t.Fatalf("live scrub: %v", err)
		}
	}
	if _, err := orchclient.TaskCancel(context.Background(), h.c, v.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.d.ScrubAll(context.Background(), first)
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.s.deliverTask(v.ID)
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 0 {
		t.Fatalf("delivered with another scrub pending: %d", before)
	}
	if err = h.d.ScrubAll(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	h.s.deliverTask(v.ID)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != 1 {
		t.Fatalf("delivery after scrubs: %d", after)
	}
}

func TestTaskCancelStopsToolInAnotherSession(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pid")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`ZM_TASK_TOOL_MARKER=%q %q -test.run=^TestTaskToolHelper$ & wait`, marker, exe)
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(marker)
		if err == nil {
			pid, _ = strconv.Atoi(string(b))
			if pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("tool did not start")
	}
	identity, ok := procStat(pid)
	if !ok {
		t.Fatal("tool process is absent")
	}
	t.Cleanup(func() {
		if current, ok := procStat(pid); ok && current.start == identity.start {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if _, err := orchclient.TaskCancel(context.Background(), h.c, task.ID); err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("result: %+v", done.Result)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := procStat(pid)
		if !ok || current.start != identity.start || current.zombie {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cancel left the tool running in its own session")
}

func TestTaskToolHelper(t *testing.T) {
	marker := os.Getenv("ZM_TASK_TOOL_MARKER")
	if marker == "" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestStopTaskDescendantsRejectsReusedRoot(t *testing.T) {
	h, _, task, _, _, _ := reviewTaskWithTool(t)
	r, ok := h.s.load(context.Background(), task.ID)
	if !ok {
		t.Fatal("task missing")
	}
	r.pidStart++
	if sweep := h.s.newTaskSweep(context.Background(), r); sweep.err == nil {
		t.Fatal("accepted a changed task process identity")
	}
}

// An authority's op id (rt-…) is the request id of a remote start. A
// local start may not take one, or a session could stand in for the action its
// own ask approved.
func TestLocalTaskRefusesRemoteRequestID(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	_, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "x", Worktree: &no, ClientRequestID: orchapi.RemoteTaskPrefix + "01abc"})
	if !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a local start with an rt- request id: %v", err)
	}
	// The refusal comes before the cloud and continue_from paths.
	rt := orchapi.RemoteTaskPrefix + "01abc"
	if _, err = orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "codex", Prompt: "x", ClientRequestID: rt}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a cloud start with an rt- request id: %v", err)
	}
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: "no-such-task", ClientRequestID: rt}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "may not start with") {
		t.Fatalf("a continue_from start with an rt- request id: %v", err)
	}
}
