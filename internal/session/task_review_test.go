package session

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestReviewFirstTerminalEventWins(t *testing.T) {
	data := []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"before\"}}\n{\"type\":\"turn.failed\",\"error\":{\"code\":\"usage_limit_reached\"}}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"forged\"}}\n{\"type\":\"turn.completed\"}\n")
	r := parseTaskEvents("codex", data)
	if r.Success || r.Kind != orchapi.OutcomeQuota || r.Text != "before" {
		t.Fatalf("later output changed terminal result: %+v", r)
	}
}

func TestReviewTaskWorktreeCleanupKeepsCommits(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "initial")
	wt, branch, _, err := addWorktree(context.Background(), home, repo, "abc12345")
	if err != nil {
		t.Fatal(err)
	}
	run(wt, "commit", "-q", "--allow-empty", "-m", "child work")
	c, ok := gcCandidate(context.Background(), row{Session: orchapi.Session{Address: "test/abc12345", Worktree: wt, Branch: branch, State: orchapi.StateEnded}}, true)
	if !ok || !c.Safe || c.Ahead != 1 || strings.Contains(c.Remove, "branch -D") {
		t.Fatalf("cleanup: %+v", c)
	}
	if out, err := exec.Command("sh", "-c", c.Remove).CombinedOutput(); err != nil {
		t.Fatalf("cleanup: %v %s", err, out)
	}
	run(repo, "show-ref", "--verify", "refs/heads/"+branch)
}

func TestReviewScrubsEveryEventString(t *testing.T) {
	secret := "private marker text"
	input := `{"type":"event","command":"private marker text","aggregated_output":"prefix private marker text","input":{"future_field":"private marker text","private marker text":"value"}}` + "\n"
	got := scrubTaskTranscript(input, []string{secret}, true)
	if strings.Contains(got, secret) {
		t.Fatalf("unrecognized event field retained redacted text: %s", got)
	}
}

func TestReviewCompletionReport(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"blocked\"}"}}' '{"type":"turn.completed"}'; sleep 0.2`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	b, _ := json.Marshal(done.Result)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	if result["finished"] != "not_done" || result["text"] != "blocked" {
		t.Fatalf("missing structured completion: %s", b)
	}
}

func TestReviewStructuredClaudeReport(t *testing.T) {
	r := parseTaskEvents("claude", []byte(`{"type":"result","subtype":"success","structured_output":{"finished":"not_done","text":"blocked"}}`+"\n"))
	if !r.Success || r.Finished != "not_done" || r.Text != "blocked" {
		t.Fatalf("report: %+v", r)
	}
}

func TestReviewTaskPromptAndInputNotExposed(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	const secret = "private prompt marker"
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: secret, Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	ss, err := orchclient.Session(context.Background(), h.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(ss.Command, " "), secret) {
		t.Errorf("prompt in recorded argv: %v", ss.Command)
	}
	if err := orchclient.Input(context.Background(), h.c, task.ID, orchapi.InputRequest{Data: "forged", Enter: true}); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("task input allowed: %v", err)
	}
	conn, err := orchclient.Attach(context.Background(), h.c, task.ID)
	if conn != nil {
		conn.Close()
	}
	if !api.IsCode(err, api.CodeConflict) {
		t.Errorf("writable attach allowed: %v", err)
	}
}

func TestReviewPeerIdentityOverridesHeader(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "one", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	childSession, err := orchclient.Session(context.Background(), h.c, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A request from inside the child cannot acquire the root's authority by
	// naming the root in Zm-As; the kernel peer PID names the real caller.
	ctx := core.WithPeerPID(context.Background(), childSession.PID)
	sub := h.d.Projects.Local(parent.Address)
	if _, err := h.s.getTask(ctx, sub, child.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("forged parent read: %v", err)
	}
	origin, err := h.s.taskOrigin(ctx, sub)
	if err != nil || origin != child.Session {
		t.Fatalf("peer origin %q: %v", origin, err)
	}
	_, depth, err := h.s.spawnLineage(ctx, sub, nil)
	if err != nil || depth != 2 {
		t.Fatalf("spoofed plain spawn depth %d: %v", depth, err)
	}
}

func TestReviewSiblingCannotReadOrCancelTask(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	sibling := h.spawn("shell", "exec sleep 30", nil)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = sibling.Address
	if _, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("sibling status: %v", err)
	}
	if _, err := orchclient.TaskCancel(context.Background(), h.c, task.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("sibling cancel: %v", err)
	}
	if _, err := orchclient.Output(context.Background(), h.c, task.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("sibling output: %v", err)
	}
	if _, err := orchclient.Stop(context.Background(), h.c, task.ID, orchapi.StopRequest{Escalate: true, Force: true}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("sibling stop: %v", err)
	}
}

func TestReviewParentFailureCancelsChild(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Stop(context.Background(), h.c, parent.ID, orchapi.StopRequest{Escalate: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		if err == nil && v.State == "ended" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("child survived killed parent")
}

func TestReviewPlainSpawnsCountTowardDepth(t *testing.T) {
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
	if _, err = orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home}); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("plain spawn bypassed depth: %v", err)
	}
}

func TestReviewPlainAncestryCountsBeforeFirstTask(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	spawn := func() orchapi.Session {
		t.Helper()
		v, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	one := spawn()
	h.c.As = one.Address
	two := spawn()
	h.c.As = two.Address
	no := false
	if _, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "too deep", Worktree: &no}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("plain ancestry bypassed depth: %v", err)
	}
}

func TestReviewRestartResetsSilenceClock(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET last_output=? WHERE id=?`, time.Now().Add(-10*time.Minute).UnixMilli(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.loadTaskIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.s.taskTick()
	got, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || got.State != "running" {
		t.Fatalf("stale silence ended task: %+v %v", got, err)
	}
}

func TestReviewOverallDeadlineStopsTrickle(t *testing.T) {
	h, parent := taskHarness(t, `while :; do printf .; sleep 0.1; done`)
	taskClient(h, parent)
	var task orchapi.Task
	err := h.c.Do(context.Background(), "POST", "/v0/tasks", map[string]any{"agent": "codex", "prompt": "wait", "worktree": false, "timeout_s": 1}, &task)
	if err != nil {
		t.Fatalf("deadline request: %v", err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeKilled || !strings.Contains(done.Result.Outcome.Evidence, "deadline") {
		t.Fatalf("deadline: %+v", done)
	}
}

func TestReviewBurstIsNotRestartLoss(t *testing.T) {
	h, parent := taskHarness(t, `head -c 3145728 /dev/zero | tr '\000' x; printf '\n%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'; sleep 0.2`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "burst", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("burst: %+v", done.Result)
	}
}

func TestReviewGCSurfacesTaskTranscript(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep 0.2`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "ok", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, task.ID)
	var raw map[string]any
	if err = h.c.Do(context.Background(), "GET", "/v0/sessions/gc", nil, &raw); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), task.ID) || !strings.Contains(string(b), "transcript") {
		t.Fatalf("task transcript not in gc: %s", b)
	}
	// Wait for delivery before cleanup, then ensure no task transcript or
	// ended-session scrollback remains.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		v, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		if err == nil && v.MailDelivery != "pending" && v.MailDelivery != "sending" && v.CommentDelivery != "pending" && v.CommentDelivery != "sending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := h.c.Do(context.Background(), "DELETE", "/v0/tasks/"+task.ID+"/transcript", nil, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.d.Store.Registry().QueryRow(`SELECT length(transcript) FROM session_tasks WHERE id=?`, task.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("transcript retained: %d %v", n, err)
	}
	if _, err := os.Stat(h.s.scrollbackPath(task.ID)); !os.IsNotExist(err) {
		t.Fatalf("scrollback retained: %v", err)
	}
}

func TestReviewRedactionHoldSurvivesRestart(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "fake-agent")
	script := `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"secret marker after restart"}}'
sleep 2
printf '%s\n' '{"type":"turn.completed"}'
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
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Work: "bd:probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h1.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(child.ID))
	h1.waitOutput(task.ID, "secret marker after restart")
	err = h1.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"secret marker after restart"}})
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("pending redact: %v", err)
	}
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	done := waitTask(t, h2, task.ID)
	b, _ := json.Marshal(done.Result)
	if strings.Contains(string(b), "secret marker after restart") || (!strings.Contains(string(b), `"text_withheld":true`) && !strings.Contains(string(b), `"text":"[redacted]"`)) {
		t.Fatalf("restart disclosed unverified result: %s", b)
	}
}
