package session

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

func TestTaskSilenceIgnoresHeartbeatAndToolChatter(t *testing.T) {
	h, parent := taskHarness(t, `(while :; do printf '%s\n' '{"type":"tool_progress","elapsed_time_seconds":1}' 'background tool output'; sleep 0.1; done) & wait`)
	taskClient(h, parent)
	h.s.TaskNoOutput = 500 * time.Millisecond
	no := false
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("result: %+v", done)
	}
}

func TestTaskProgressEvents(t *testing.T) {
	for _, tc := range []struct {
		agent, line string
		want        bool
	}{
		{"claude", `{"type":"tool_progress"}`, false},
		{"claude", `{"type":"assistant"}`, true},
		{"claude", `{"type":"user"}`, true},
		{"codex", `{"type":"item.updated"}`, false},
		{"codex", `{"type":"item.completed"}`, true},
		{"claude", `unstructured output`, false},
	} {
		if got := taskProgress(tc.agent, []byte(tc.line)); got != tc.want {
			t.Errorf("%s %s: %v", tc.agent, tc.line, got)
		}
	}
}

func TestTaskCompletedStepsKeepItAlive(t *testing.T) {
	h, parent := taskHarness(t, `i=0; while [ "$i" -lt 6 ]; do printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result"}]}}'; sleep 0.25; i=$((i+1)); done; printf '%s\n' '{"type":"result","subtype":"success","result":"done"}'`)
	taskClient(h, parent)
	h.s.TaskNoOutput = 700 * time.Millisecond
	no := false
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "wait", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("result: %+v", done)
	}
}
