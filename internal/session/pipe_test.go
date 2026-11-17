package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/builtin"     // zm __zm-agent <driver>, as cmd/zm links it
	_ "github.com/sengirunners/mezze/internal/agentpipe/conformance" // the fake runtime (__zm-agent-fake)
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// spawnPipe starts a structured codex session whose runtime is the fake
// replaying a recording (testdata of internal/agentpipe).
func (h *harness) spawnPipe(rec, approvals string) orchapi.Session {
	h.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join("..", "agentpipe", "testdata", rec))
	if err != nil {
		h.t.Fatal(err)
	}
	ss, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "codex", Command: []string{exe, agentpipe.FakeArg, abs},
		Cwd: h.home, Mode: orchapi.SessionModePipe, Approvals: approvals})
	if err != nil {
		h.t.Fatalf("spawn: %v", err)
	}
	return ss
}

// until reads the session's events until one matches, returning all read.
func (h *harness) until(id string, after *int64, what string, ok func(orchapi.AgentEvent) bool) []orchapi.AgentEvent {
	h.t.Helper()
	var seen []orchapi.AgentEvent
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		page, err := orchclient.SessionEvents(context.Background(), h.c, id, *after, 0, 2)
		if err != nil {
			h.t.Fatalf("events: %v", err)
		}
		for _, raw := range page.Events {
			var ev orchapi.AgentEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				h.t.Fatal(err)
			}
			if ev.Seq != *after+1 {
				h.t.Fatalf("event seq %d after %d", ev.Seq, *after)
			}
			*after = ev.Seq
			seen = append(seen, ev)
			if ok(ev) {
				return seen
			}
		}
		if page.Ended {
			break
		}
	}
	h.t.Fatalf("never saw %s; saw %+v", what, seen)
	return nil
}

func turnEnd(ev orchapi.AgentEvent) bool {
	return ev.Type == agentpipe.EvTurn && agentpipe.Terminal(ev.Status)
}

// TestPipeSessionTurns: a structured session started through the API takes
// two turns, streams its events by seq, records the runtime's id and goes
// idle between turns; terminal routes refuse it.
func TestPipeSessionTurns(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.spawnPipe("codex/turns.rec.jsonl", "")
	if ss.Mode != orchapi.SessionModePipe || ss.Capabilities == nil || !ss.Capabilities.Steer {
		t.Fatalf("spawned: %+v", ss)
	}
	var after int64
	h.until(ss.ID, &after, "the session ready", func(ev orchapi.AgentEvent) bool {
		return ev.Type == agentpipe.EvSession && ev.Status == agentpipe.StateReady
	})
	res, err := orchclient.Turn(context.Background(), h.c, ss.ID, orchapi.SessionTurnRequest{Text: "Reply with exactly the word: alpha"})
	if err != nil || !res.OK {
		t.Fatalf("turn: %+v %v", res, err)
	}
	evs := h.until(ss.ID, &after, "the first turn's end", turnEnd)
	var text string
	for _, ev := range evs {
		if ev.Type == agentpipe.EvMessage && ev.Role == "assistant" && ev.Status == agentpipe.ItemCompleted {
			text = ev.Text
		}
	}
	if text != "alpha" || evs[len(evs)-1].Status != agentpipe.TurnCompleted {
		t.Fatalf("first turn: %q %+v", text, evs[len(evs)-1])
	}
	got := h.wait(ss.ID, "idle with its runtime id", func(s orchapi.Session) bool { return s.State == orchapi.StateIdle && s.RuntimeID != "" })
	if got.Model == "" {
		t.Errorf("model not recorded: %+v", got)
	}
	// Terminal routes refuse a structured session.
	var ae *api.Error
	if err := orchclient.Input(context.Background(), h.c, ss.ID, orchapi.InputRequest{Data: "x"}); !errors.As(err, &ae) || ae.Code != api.CodeConflict {
		t.Fatalf("input: %v", err)
	}
	if _, err := orchclient.Turn(context.Background(), h.c, ss.ID, orchapi.SessionTurnRequest{Text: "x", Steer: true}); err == nil {
		t.Fatal("a steer with no running turn was taken")
	}
	if res, err = orchclient.Turn(context.Background(), h.c, ss.ID, orchapi.SessionTurnRequest{Text: "Now reply with exactly the word: beta"}); err != nil || !res.OK {
		t.Fatalf("second turn: %+v %v", res, err)
	}
	h.until(ss.ID, &after, "the second turn's end", turnEnd)
	if _, err := orchclient.Stop(context.Background(), h.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	h.until(ss.ID, &after, "the session stopped", func(ev orchapi.AgentEvent) bool {
		return ev.Type == agentpipe.EvSession && (ev.Status == agentpipe.StateStopped || ev.Status == agentpipe.StateError)
	})
	end := h.wait(ss.ID, "ended", ended)
	if end.Outcome.Kind != orchapi.OutcomeKilled && end.Outcome.Kind != orchapi.OutcomeOK {
		t.Errorf("outcome: %+v", end.Outcome)
	}
	// The stream stays readable after the end, from any seq.
	page, err := orchclient.SessionEvents(context.Background(), h.c, ss.ID, 3, 2, 0)
	if err != nil || len(page.Events) != 2 || page.Next != 5 {
		t.Fatalf("ended read: %+v %v", page, err)
	}
}

// TestPipeSessionApproval: a pending approval puts the session in
// needs_human; an answer through the API reaches the runtime and the
// session runs on.
func TestPipeSessionApproval(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.spawnPipe("codex/approve.rec.jsonl", "ask")
	var after int64
	h.until(ss.ID, &after, "ready", func(ev orchapi.AgentEvent) bool {
		return ev.Type == agentpipe.EvSession && ev.Status == agentpipe.StateReady
	})
	if _, err := orchclient.Turn(context.Background(), h.c, ss.ID, orchapi.SessionTurnRequest{Text: "Use the shell to run exactly this command: touch zm-approve.txt\nThen reply in one short sentence."}); err != nil {
		t.Fatal(err)
	}
	evs := h.until(ss.ID, &after, "an approval", func(ev orchapi.AgentEvent) bool {
		return ev.Type == agentpipe.EvApproval && ev.Status == agentpipe.ItemPending
	})
	req := evs[len(evs)-1]
	got := h.wait(ss.ID, "needs_human", func(s orchapi.Session) bool { return s.State == orchapi.StateNeedsHuman })
	if got.Note != "asks to run a command" || strings.Contains(got.Note, "touch") {
		t.Errorf("note: %q", got.Note)
	}
	if _, err := orchclient.Answer(context.Background(), h.c, ss.ID, orchapi.SessionAnswerRequest{Request: req.Request, Decision: "maybe"}); err == nil {
		t.Fatal("a bad decision was taken")
	}
	if res, err := orchclient.Answer(context.Background(), h.c, ss.ID, orchapi.SessionAnswerRequest{Request: req.Request, Decision: agentpipe.DecisionAccept}); err != nil || !res.OK {
		t.Fatalf("answer: %+v %v", res, err)
	}
	h.wait(ss.ID, "running again", func(s orchapi.Session) bool { return s.State != orchapi.StateNeedsHuman })
	h.until(ss.ID, &after, "the turn's end", turnEnd)
	h.wait(ss.ID, "idle", func(s orchapi.Session) bool { return s.State == orchapi.StateIdle })
}

// TestPipeSessionMCPRefusals: mcp servers and a tool grant are for a
// structured session, and a malformed list is refused before anything starts,
// without echoing a header value (it may be a credential).
func TestPipeSessionMCPRefusals(t *testing.T) {
	h := newHarness(t, nil)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	good := []orchapi.AgentMCP{{Name: "t3", URL: "http://127.0.0.1:9/mcp", Headers: map[string]string{"Authorization": "Bearer s3cret"}}}
	bad := []orchapi.AgentMCP{{Name: "t3", URL: "http://127.0.0.1:9/mcp", Headers: map[string]string{"Authorization": "Bearer s3cret\r\nX: y"}}}
	for name, req := range map[string]orchapi.SpawnRequest{
		"pty":        {Agent: "codex", Command: []string{exe, "-h"}, Cwd: h.home, MCP: good},
		"bad header": {Agent: "codex", Command: []string{exe, agentpipe.FakeArg, "none"}, Cwd: h.home, Mode: orchapi.SessionModePipe, MCP: bad},
		"bad url":    {Agent: "codex", Command: []string{exe, agentpipe.FakeArg, "none"}, Cwd: h.home, Mode: orchapi.SessionModePipe, MCP: []orchapi.AgentMCP{{Name: "t3", URL: "file:///etc/passwd"}}},
		"pty grant":  {Agent: "codex", Command: []string{exe, "-h"}, Cwd: h.home, AllowedTools: []string{"Bash"}},
		"bad mode":   {Agent: "codex", Command: []string{exe, agentpipe.FakeArg, "none"}, Cwd: h.home, Mode: orchapi.SessionModePipe, PermissionMode: "bypassPermissions"},
		"comma tool": {Agent: "codex", Command: []string{exe, agentpipe.FakeArg, "none"}, Cwd: h.home, Mode: orchapi.SessionModePipe, AllowedTools: []string{"Read,Bash"}},
		"space tool": {Agent: "codex", Command: []string{exe, agentpipe.FakeArg, "none"}, Cwd: h.home, Mode: orchapi.SessionModePipe, AllowedTools: []string{"Read Write"}},
	} {
		var ae *api.Error
		_, err := orchclient.Spawn(context.Background(), h.c, req)
		if !errors.As(err, &ae) || ae.Code != api.CodeBadRequest {
			t.Fatalf("%s: want bad_request, got %v", name, err)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("%s: the refusal echoes a header value: %v", name, err)
		}
	}
	if list, err := orchclient.Sessions(context.Background(), h.c, orchapi.SessionQuery{}); err == nil && len(list) != 0 {
		t.Fatalf("a refused spawn left a session: %+v", list)
	}
}
