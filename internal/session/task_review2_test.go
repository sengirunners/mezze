package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestReview2UnmanagedParentStartsTask(t *testing.T) {
	h, _ := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"ok\"}"}}' '{"type":"turn.completed"}'; sleep .2`)
	parent, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "unmanaged-parent", Cwd: h.home, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Finished != "done" {
		t.Fatalf("unmanaged parent: %+v", done.Result)
	}
}

func TestReview2NoControllingTerminal(t *testing.T) {
	script := `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"forged\"}"}}' '{"type":"turn.completed"}' > /dev/tty 2>/dev/null || :; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"real\"}"}}' '{"type":"turn.completed"}'; sleep .2`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Finished != "not_done" || done.Result.Text != "real" {
		t.Fatalf("forged result: %+v", done.Result)
	}
}

func TestReview2StderrCannotForgeEvents(t *testing.T) {
	script := `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}' >&2; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"real\"}"}}' '{"type":"turn.completed"}'; sleep .2`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Finished != "not_done" || done.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("stderr forged result: %+v", done.Result)
	}
}

func TestReview2RawAndPrefixedScrub(t *testing.T) {
	secret := "private secret marker"
	for _, prefix := range []string{"12 ", "[1] ", `"x" `, `{"x":1} `} {
		input := prefix + secret + "\n"
		if got := scrubTaskTranscript(input, []string{secret}, true); strings.Contains(got, secret) {
			t.Errorf("prefix %q leaked: %q", prefix, got)
		}
	}
	secret = "private first line\nprivate second line"
	if got := scrubTaskTranscript("before "+secret+" after", []string{secret}, true); strings.Contains(got, secret) {
		t.Fatalf("multiline leaked: %q", got)
	}
	if got := scrubTaskTranscript(`12 "private \u0073ecret marker" trailing`, []string{"private secret marker"}, true); strings.Contains(got, `\u0073ecret`) {
		t.Fatalf("escaped string leaked: %q", got)
	}
}

func TestReview2PlainChildSurvivesParentFailure(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	child, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Stop(context.Background(), h.c, parent.ID, orchapi.StopRequest{Escalate: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	got, err := orchclient.Session(context.Background(), h.c, child.ID)
	if err != nil || got.State == orchapi.StateEnded {
		t.Fatalf("plain child killed: %+v %v", got, err)
	}
}

func TestReview2RegisterInheritsDepth(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	a, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = a.Session
	b, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "b", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h.c, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := core.WithPeerPID(core.WithSubject(context.Background(), h.d.Projects.Local(parent.Address)), child.PID)
	reg, _, err := h.s.Register(ctx, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "review2-registration", Cwd: h.home, PID: child.PID})
	if err != nil {
		t.Fatal(err)
	}
	var depth int
	err = h.d.Store.Registry().QueryRow(`SELECT depth FROM session_lineage WHERE id=?`, reg.ID).Scan(&depth)
	if err != nil || depth < 2 {
		t.Fatalf("registration depth %d: %v", depth, err)
	}
}

func TestReview2HoldPreservesUnrelatedResult(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"useful result"}}' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	a, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Work: "other", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, a.ID)
	if done.Result.Text != "useful result" {
		t.Fatal(done.Result)
	}
	b, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "b", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"secret marker"}})
	got, err := orchclient.TaskStatus(context.Background(), h.c, a.ID, 0)
	if err != nil || got.Result.Text != "useful result" || got.Result.TextWithheld {
		t.Fatalf("unrelated result changed: %+v %v", got.Result, err)
	}
	_, _ = orchclient.TaskCancel(context.Background(), h.c, b.ID)
}

func TestReview2DeliveryRetries(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	attempts := 0
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind == "mail" {
			attempts++
			if attempts < 2 {
				return fmt.Errorf("transient")
			}
		}
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	for i := 0; i < 4; i++ {
		h.s.deliverTask(v.ID)
		time.Sleep(10 * time.Millisecond)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "sent" || attempts < 2 {
		t.Fatalf("no retry: %+v attempts=%d err=%v", got, attempts, err)
	}
}

func TestReview2DeliveryRetriesAreBounded(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	attempts := 0
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind == "mail" {
			attempts++
			return fmt.Errorf("unavailable")
		}
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	for i := 0; i < 5; i++ {
		h.s.deliverTask(v.ID)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "failed" || attempts != 3 {
		t.Fatalf("retry bound: %+v attempts=%d err=%v", got, attempts, err)
	}
}

func TestReview2LargeBurstKeepsTerminalEvent(t *testing.T) {
	h, parent := taskHarness(t, `head -c 16777216 /dev/zero | tr '\000' x; printf '\n%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"after burst\"}"}}' '{"type":"turn.completed"}'`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "burst", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Finished != "not_done" || done.Result.Text != "after burst" {
		t.Fatalf("burst lost result: %+v", done.Result)
	}
}

func TestReview2SpoolLimitIsReported(t *testing.T) {
	h, parent := taskHarness(t, `head -c 34603008 /dev/zero | tr '\000' x; printf '\n%s\n' '{"type":"turn.completed"}'`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "overflow", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeFailed || !strings.Contains(done.Result.Outcome.Evidence, "32 MiB") {
		t.Fatalf("overflow: %+v", done.Result)
	}
}

func TestReview2HoldEventuallyScrubsAndKeepsText(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"before secret marker after"}}'; sleep 1; printf '%s\n' '{"type":"turn.completed"}'`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(task.ID, "secret marker")
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"secret marker"}}
	if err := h.d.ScrubAll(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("hold: %v", err)
	}
	_ = waitTask(t, h, task.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.s.recoverTaskHolds(context.Background(), task.ID)
		got, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		if err == nil && got.Result != nil && !got.Result.TextWithheld {
			if got.Result.Text != "before [redacted] after" {
				t.Fatalf("lost preserved text: %+v", got.Result)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hold was never scrubbed and released")
}

func TestReview2HoldEventuallySendsWithheldMail(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"secret marker"}}'; exec sleep 30`)
	taskClient(h, parent)
	no := false
	var body string
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, text string) error {
		if kind == "mail" {
			body = text
		}
		return nil
	}
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(task.ID, "secret marker")
	if err = h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"secret marker"}}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatal(err)
	}
	h.s.taskMu.Lock()
	defer h.s.taskMu.Unlock()
	if _, err = orchclient.TaskCancel(context.Background(), h.c, task.ID); err != nil {
		t.Fatal(err)
	}
	_, err = h.d.Store.Registry().Exec(`UPDATE session_task_holds SET created=? WHERE task_id=?`, time.Now().Add(-time.Minute).UnixMilli(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.s.deliverTask(task.ID)
	if !strings.Contains(body, "withheld") || strings.Contains(body, "secret marker") {
		t.Fatalf("held mail %q", body)
	}
	if err := h.c.Do(context.Background(), "DELETE", "/v0/tasks/"+task.ID+"/transcript", nil, nil); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("cleanup while held: %v", err)
	}
}

func TestReview2HoldRecoveryAfterRestart(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"before secret marker after\"}}'\nsleep 2\nprintf '%s\\n' '{\"type\":\"turn.completed\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h1.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(child.ID))
	h1.waitOutput(task.ID, "secret marker")
	if err = h1.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"secret marker"}}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatal(err)
	}
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	_ = waitTask(t, h2, task.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h2.s.recoverTaskHolds(context.Background(), task.ID)
		got, err := orchclient.TaskStatus(context.Background(), h2.c, task.ID, 0)
		if err == nil && got.Result != nil && !got.Result.TextWithheld {
			if got.Result.Text != "before [redacted] after" {
				t.Fatalf("result: %+v", got.Result)
			}
			for _, p := range []string{h2.s.scrollbackPath(task.ID), holder.TaskOutputPath(home, task.ID)} {
				b, e := os.ReadFile(p)
				if e == nil && strings.Contains(string(b), "secret marker") {
					t.Fatalf("secret remains in %s", p)
				}
			}
			// A compaction whose checkpoint is busy (the daemon's own taskTick may be
			// compacting or reading at the same moment) stays queued and is retried;
			// the files are clean once the queue is empty, not after any one attempt.
			compactBy := time.Now().Add(10 * time.Second)
			for pending := 1; pending > 0; {
				if time.Now().After(compactBy) {
					t.Fatal("scrub compaction never completed")
				}
				h2.s.compactTaskScrubs(context.Background())
				if err := h2.s.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_compactions`).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if pending > 0 {
					time.Sleep(20 * time.Millisecond)
				}
			}
			for _, p := range []string{filepath.Join(home, "registry.db"), filepath.Join(home, "registry.db-wal")} {
				b, e := os.ReadFile(p)
				if e == nil && strings.Contains(string(b), "secret marker") {
					t.Fatalf("secret remains after compaction in %s", p)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("restart did not scrub and release hold")
}
