package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/config"
)

// A task an authority queued for this machine (a farm box)
// runs the agent it asked for, and on_limit next_agent continues it on the
// fallback here although its parent lives on another machine; the parent's
// machine follows the chain from the first task's ID.
func TestRemoteTaskRunsAskedAgentAndFailsOverOnLimit(t *testing.T) {
	// codex (exec) hits its usage limit; claude finishes.
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; ` +
		`else printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"finished\":\"done\",\"text\":\"ok\"}"}'; fi`
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *config.Config) {
		c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}}
		c.File.RemoteTasks = true
	})
	dir := filepath.Join(h.home, "farm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	proj := h.project("farm", dir)
	ctx := context.Background()
	no := false
	start := func(id, agent, onLimit string) orchapi.Task {
		t.Helper()
		task, err := h.s.StartRemoteTask(ctx, proj.ID, id, orchapi.TaskStartOp{Parent: "farm-parent/0a1b2c3d", Depth: 1,
			Request: orchapi.TaskStartRequest{Agent: agent, Prompt: "go", OnLimit: onLimit, Worktree: &no, Cwd: dir}})
		if err != nil {
			t.Fatal(err)
		}
		if task.Agent != agent {
			t.Fatalf("asked for %s, runs %s", agent, task.Agent)
		}
		return task
	}
	until := func(id string, done func(orchapi.Task) bool) orchapi.Task {
		t.Helper()
		var cur orchapi.Task
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			var err error
			if cur, err = h.s.RemoteTaskStatus(ctx, id); err != nil {
				t.Fatal(err)
			}
			if done(cur) {
				return cur
			}
		}
		t.Fatalf("task %s: %+v (%s)", id, cur, taskCaptureDiagnostic(h, id))
		return cur
	}
	ended := func(x orchapi.Task) bool { return x.State == "ended" }

	// The runtime asked for is the one that runs.
	c := start("op-claude", "claude", "")
	got := until(c.ID, ended)
	if got.Agent != "claude" || got.Result == nil || got.Result.Outcome.Kind != orchapi.OutcomeOK || len(got.Tried) != 0 {
		t.Fatalf("claude task: %+v", got)
	}

	// A Codex limit continues on Claude, and the parent's view keeps the work.
	src := start("op-codex", "codex", "next_agent")
	got = until(src.ID, func(x orchapi.Task) bool { return ended(x) && x.Agent == "claude" })
	if got.ID != src.ID || got.Result == nil || got.Result.Outcome.Kind != orchapi.OutcomeOK || got.Result.Text != "ok" {
		t.Fatalf("chain view: %+v", got)
	}
	if len(got.Tried) != 1 || got.Tried[0].Agent != "codex" || got.Tried[0].Outcome != orchapi.OutcomeQuota || got.Tried[0].Session != src.Session {
		t.Fatalf("tried: %+v", got.Tried)
	}
	// From the continuation's own ID the answer is the same chain.
	direct, err := h.s.getTask(ctx, h.s.d.System(), src.ID)
	if err != nil || direct.Continuation == "" || direct.Continuation == "pending" {
		t.Fatalf("source: %+v %v", direct, err)
	}
	again := until(direct.Continuation, ended)
	if again.ID != src.ID || again.Agent != "claude" || len(again.Tried) != 1 {
		t.Fatalf("from continuation: %+v", again)
	}
	var depth int
	var parent string
	if err := h.s.d.Store.Registry().QueryRow(`SELECT depth,parent FROM session_tasks WHERE id=?`, direct.Continuation).Scan(&depth, &parent); err != nil || depth != 1 || parent != "farm-parent/0a1b2c3d" {
		t.Fatalf("continuation depth %d parent %q: %v", depth, parent, err)
	}
}

// A Claude task stopped by a bare 429 (no exhausted window) is a failure,
// not a limit: it does not hand off.
func TestBareClaude429DoesNotHandOff(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}'`)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, task.ID)
	time.Sleep(200 * time.Millisecond) // a handoff would be recorded with the end
	if done, err = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0); err != nil {
		t.Fatal(err)
	}
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeFailed || done.Continuation != "" {
		t.Fatalf("bare 429: %+v %+v", done, done.Result)
	}
}
