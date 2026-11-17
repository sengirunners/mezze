package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

func (h *harness) hook(req orchapi.HookRequest) orchapi.Session {
	h.t.Helper()
	r, err := orchclient.Hook(context.Background(), h.c, req)
	if err != nil {
		h.t.Fatalf("hook %+v: %v", req, err)
	}
	return r.Session
}

func (h *harness) live() []orchapi.Session {
	h.t.Helper()
	l, err := orchclient.Sessions(context.Background(), h.c, orchapi.SessionQuery{})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

// An agent started at a managed shell's prompt binds to the shell's row
// (runtime_agent) instead of registering a second session; its hooks, a
// completion naming no session included, reach the shell; while it runs a
// nested agent gets its own row; its end unbinds it, and the next agent binds.
func TestShellBindsAgent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	shell := h.spawn("shell", "exec sleep 30", nil)
	me := os.Getpid()
	start := func(rid string) orchapi.HookRequest {
		return orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Cwd: h.home,
			Session: shell.Address, State: orchapi.StateIdle, PID: me, Model: "opus"}, Notice: true}
	}
	got := h.hook(start("rt-shell-1"))
	if got.Address != shell.Address || got.RuntimeAgent == nil || got.RuntimeAgent.Agent != "claude" ||
		got.RuntimeAgent.RuntimeID != "rt-shell-1" || got.RuntimeAgent.PID != me || got.RuntimeAgent.Model != "opus" {
		t.Fatalf("session-start in a shell: %+v %+v", got, got.RuntimeAgent)
	}
	got = h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-shell-1",
		Session: shell.Address, State: orchapi.StateRunning}, Activity: "Editing repos.go"})
	if got.Address != shell.Address || got.Activity != "Editing repos.go" || got.RuntimeAgent.Task != "Editing repos.go" || got.State != orchapi.StateRunning {
		t.Fatalf("tool hook: %+v %+v", got, got.RuntimeAgent)
	}
	// A completion: the route drops its inherited ZM_SESSION, the runtime id finds the shell.
	got = h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-shell-1",
		Session: shell.Address, State: orchapi.StateIdle}})
	if got.Address != shell.Address || got.State != orchapi.StateIdle || got.Activity != "" || got.RuntimeAgent.Task != "Editing repos.go" {
		t.Fatalf("stop hook: %+v %+v", got, got.RuntimeAgent)
	}
	if l := h.live(); len(l) != 1 {
		t.Fatalf("one session, the shell: %+v", l)
	}
	// A `claude -p` the bound agent starts inherits ZM_SESSION: its own row.
	nestedReq := start("rt-nested")
	nestedReq.PID = shell.PID // another live process: its own pid
	nested := h.hook(nestedReq)
	if nested.Address == shell.Address || nested.Managed {
		t.Fatalf("nested agent bound to the shell: %+v", nested)
	}
	if ss, _ := orchclient.Session(ctx, h.c, shell.ID); ss.RuntimeAgent == nil || ss.RuntimeAgent.RuntimeID != "rt-shell-1" {
		t.Fatalf("binding replaced: %+v", ss.RuntimeAgent)
	}
	// Its end unbinds it.
	got = h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-shell-1",
		Session: shell.Address, End: true}})
	if got.Address != shell.Address || got.State == orchapi.StateEnded || got.RuntimeAgent != nil {
		t.Fatalf("end: %+v", got)
	}
	// The next agent binds.
	if got = h.hook(start("rt-shell-2")); got.Address != shell.Address || got.RuntimeAgent == nil || got.RuntimeAgent.RuntimeID != "rt-shell-2" {
		t.Fatalf("second agent: %+v", got)
	}
	// An agent runtime's own managed session never binds another agent.
	m := h.spawn("claude", "exec sleep 30", nil)
	other := h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: "rt-codex",
		Session: m.Address, State: orchapi.StateIdle}, Notice: true})
	if other.Address == m.Address {
		t.Fatalf("codex bound to a claude session: %+v", other)
	}
}

// A dead bound agent (no session-end hook) is unbound by the reaper.
func TestShellBindingReaped(t *testing.T) {
	h := newHarness(t, nil)
	shell := h.spawn("shell", "exec sleep 30", nil)
	got := h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: "rt-gone",
		Session: shell.Address, State: orchapi.StateIdle, PID: os.Getpid()}, Notice: true})
	if got.RuntimeAgent == nil {
		t.Fatalf("not bound: %+v", got)
	}
	// Pretend its process exited: another start for the recorded pid.
	if _, err := h.s.update(context.Background(), shell.ID, "", func(x *row) (bool, error) {
		x.rtPIDStart++
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	h.s.reapBound()
	if ss, _ := orchclient.Session(context.Background(), h.c, shell.ID); ss.RuntimeAgent != nil {
		t.Fatalf("still bound: %+v", ss.RuntimeAgent)
	}
}

// Subagents: a spawn names the next start of its type; a step inside a
// subagent is its own activity and never moves the parent's folder; stop
// ends it; the activity log keeps both.
func TestSubagentsAndActivity(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	reg := func(state orchapi.SessionState, cwd string) orchapi.RegisterRequest {
		return orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-subs", Cwd: cwd, State: state}
	}
	h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateIdle, h.home), Notice: true})
	h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning, h.home), Activity: "Find browse package",
		Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentSpawn, Type: "Explore", Description: "Find browse package"}})
	got := h.hook(orchapi.HookRequest{RegisterRequest: reg("", h.home),
		Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart, ID: "a1", Type: "Explore"}})
	if len(got.Subagents) != 1 || got.Subagents[0].ID != "a1" || got.Subagents[0].Description != "Find browse package" ||
		got.Subagents[0].State != orchapi.StateRunning || got.State != orchapi.StateRunning {
		t.Fatalf("start: %+v", got)
	}
	wt := filepath.Join(h.home, "worktree")
	got = h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning, wt), Activity: "Reading x.go",
		Subagent: &orchapi.HookSubagent{ID: "a1", Type: "Explore"}})
	if got.Cwd != h.home || got.Activity != "Find browse package" || got.Subagents[0].Activity != "Reading x.go" {
		t.Fatalf("step: cwd %q activity %q subagents %+v", got.Cwd, got.Activity, got.Subagents)
	}
	// A step of one whose start was missed shows it.
	got = h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning, wt), Activity: "Searching",
		Subagent: &orchapi.HookSubagent{ID: "a2", Type: "general-purpose"}})
	if len(got.Subagents) != 2 || got.Subagents[1].ID != "a2" {
		t.Fatalf("missed start: %+v", got.Subagents)
	}
	got = h.hook(orchapi.HookRequest{RegisterRequest: reg("", h.home), Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStop, ID: "a1"}})
	if len(got.Subagents) != 1 || got.Subagents[0].ID != "a2" || got.State != orchapi.StateRunning {
		t.Fatalf("stop: %+v", got)
	}
	list, err := orchclient.SessionSubagents(ctx, h.c, got.ID)
	if err != nil || len(list.Subagents) != 2 || list.Subagents[0].State != orchapi.StateEnded || list.Subagents[0].Ended == nil ||
		len(list.Subagents[0].Lines) != 1 || list.Subagents[0].Lines[0].Text != "Reading x.go" {
		t.Fatalf("GET subagents: %+v %v", list, err)
	}
	act, err := orchclient.SessionActivity(ctx, h.c, got.ID, 0, nil)
	if err != nil || len(act.Lines) != 3 || act.Lines[0].Text != "Searching" || act.Lines[0].Subagent != "a2" ||
		act.Lines[2].Text != "Find browse package" || act.Lines[2].Subagent != "" {
		t.Fatalf("GET activity: %+v %v", act, err)
	}
	own := ""
	if act, err = orchclient.SessionActivity(ctx, h.c, got.ID, 1, &own); err != nil || len(act.Lines) != 1 || act.Lines[0].Text != "Find browse package" {
		t.Fatalf("GET activity, own lines: %+v %v", act, err)
	}
	if _, err := orchclient.SessionActivity(ctx, h.c, got.ID, 999, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("limit 999: %v", err)
	}
	if _, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: reg("", h.home), Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("start without an id: %v", err)
	}
}

// needs says why a session waits: the hook's kind, manual by hand, decision
// by mail; any other state clears it.
func TestSessionNeeds(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	reg := orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-needs", Cwd: h.home, State: orchapi.StateNeedsHuman, Note: "pick one"}
	got := h.hook(orchapi.HookRequest{RegisterRequest: reg, Needs: orchapi.NeedsInput})
	if got.Needs == nil || got.Needs.Kind != orchapi.NeedsInput || got.Needs.At.IsZero() {
		t.Fatalf("input: %+v", got.Needs)
	}
	reg.State, reg.Note = orchapi.StateRunning, ""
	if got = h.hook(orchapi.HookRequest{RegisterRequest: reg, Activity: "Searching"}); got.Needs != nil {
		t.Fatalf("cleared by a tool: %+v", got.Needs)
	}
	reg.State = orchapi.StateNeedsHuman
	if got = h.hook(orchapi.HookRequest{RegisterRequest: reg}); got.Needs == nil || got.Needs.Kind != orchapi.NeedsPermission {
		t.Fatalf("default permission: %+v", got.Needs)
	}
	if ss, err := orchclient.SetState(ctx, h.c, got.ID, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "by hand"}); err != nil || ss.Needs == nil || ss.Needs.Kind != orchapi.NeedsManual {
		t.Fatalf("manual: %+v %v", ss.Needs, err)
	}
	if err := h.s.SetState(core.WithSubject(ctx, h.owner()), got.Address, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "asked"}); err != nil {
		t.Fatal(err)
	}
	if ss, _ := orchclient.Session(ctx, h.c, got.ID); ss.Needs == nil || ss.Needs.Kind != orchapi.NeedsDecision {
		t.Fatalf("decision: %+v", ss.Needs)
	}
	if _, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: reg, Needs: "coffee"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("unknown needs: %v", err)
	}
}

// Review of PR 294, finding 1: a permission prompt that follows a
// subagent's step (the Notification names no subagent) ends when that
// subagent steps again or stops; a prompt after the parent's own work does not.
func TestSubagentPromptWaitEnds(t *testing.T) {
	h := newHarness(t, nil)
	reg := func(state orchapi.SessionState) orchapi.RegisterRequest {
		return orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-wait", Cwd: h.home, State: state}
	}
	step := func(id string) orchapi.Session {
		return h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning), Activity: "Running tests",
			Subagent: &orchapi.HookSubagent{ID: id}})
	}
	prompt := func() orchapi.Session {
		r := reg(orchapi.StateNeedsHuman)
		r.Note = "Claude needs your permission to use Bash"
		return h.hook(orchapi.HookRequest{RegisterRequest: r, Needs: orchapi.NeedsPermission})
	}
	h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning), Activity: "Thinking"})
	h.hook(orchapi.HookRequest{RegisterRequest: reg(""), Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart, ID: "a1"}})
	step("a1")
	if got := prompt(); got.State != orchapi.StateNeedsHuman || got.Needs == nil || got.Needs.Subagent != "a1" {
		t.Fatalf("prompt after a1's step: %+v %+v", got.State, got.Needs)
	}
	if got := step("a1"); got.State != orchapi.StateRunning || got.Needs != nil {
		t.Fatalf("a1 went on: %s %+v", got.State, got.Needs)
	}
	prompt()
	if got := h.hook(orchapi.HookRequest{RegisterRequest: reg(""), Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStop, ID: "a1"}}); got.State != orchapi.StateRunning {
		t.Fatalf("a1 stopped: %s %+v", got.State, got.Needs)
	}
	// The parent's own tool, then a prompt: a subagent's step leaves it.
	h.hook(orchapi.HookRequest{RegisterRequest: reg(""), Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart, ID: "a2"}})
	step("a2")
	h.hook(orchapi.HookRequest{RegisterRequest: reg(orchapi.StateRunning), Activity: "Editing x.go"})
	if got := prompt(); got.Needs == nil || got.Needs.Subagent != "" {
		t.Fatalf("parent's prompt: %+v", got.Needs)
	}
	if got := step("a2"); got.State != orchapi.StateNeedsHuman {
		t.Fatalf("a parallel subagent cleared the parent's prompt: %s", got.State)
	}
}

func activityCount(t *testing.T, h *harness, id8 string) int {
	t.Helper()
	var n int
	if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_activity WHERE session = ?`, id8).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Review of PR 294, finding 2: every session's lines are capped, however
// writers interleave; ended sessions' lines go after the retention.
func TestActivityPrune(t *testing.T) {
	h := newHarness(t, nil)
	ids := []string{"aaaa0001", "aaaa0002", "aaaa0003", "aaaa0004", "aaaa0005"}
	now := time.Now()
	for i := 0; i < activityKeep+60; i++ {
		for _, id := range ids {
			h.s.logActivity(id, "", "step", now)
		}
	}
	for _, id := range ids {
		if n := activityCount(t, h, id); n != activityKeep {
			t.Errorf("%s keeps %d lines, want %d", id, n, activityKeep)
		}
	}
	got := h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-old", Cwd: h.home, State: orchapi.StateRunning}, Activity: "Searching"})
	h.s.end(context.Background(), got.ID, orchapi.Outcome{Kind: orchapi.OutcomeOK})
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET ended = ? WHERE id = ?`, store.Millis(now.Add(-activityLogKeep-time.Hour)), got.ID); err != nil {
		t.Fatal(err)
	}
	h.s.pruneActivity()
	if n := activityCount(t, h, got.ID); n != 0 {
		t.Errorf("an ended session past retention keeps %d lines", n)
	}
}

// Review of PR 294, finding 3: the bound agent's own new conversation
// (/clear, /resume: same pid, new runtime id) rebinds the shell; the old
// conversation's late end changes nothing and adds no row.
func TestShellRebindsSameAgent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	shell := h.spawn("shell", "exec sleep 30", nil)
	start := func(rid string) orchapi.Session {
		return h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Cwd: h.home,
			Session: shell.Address, State: orchapi.StateIdle, PID: os.Getpid()}, Notice: true})
	}
	start("rt-conv-1")
	if got := start("rt-conv-2"); got.Address != shell.Address || got.RuntimeAgent == nil || got.RuntimeAgent.RuntimeID != "rt-conv-2" {
		t.Fatalf("new conversation: %+v %+v", got, got.RuntimeAgent)
	}
	h.hook(orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-conv-1", Session: shell.Address, End: true}})
	if ss, _ := orchclient.Session(ctx, h.c, shell.ID); ss.RuntimeAgent == nil || ss.RuntimeAgent.RuntimeID != "rt-conv-2" {
		t.Fatalf("late end of the old conversation: %+v", ss.RuntimeAgent)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true}); len(l) != 1 {
		t.Fatalf("want the shell only: %+v", l)
	}
}

// Review of PR 294, finding 5: a running subagent whose stop was missed
// leaves the session's reads after subagentStale, and the column later.
func TestSubagentExpires(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	reg := orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-stale", Cwd: h.home}
	got := h.hook(orchapi.HookRequest{RegisterRequest: reg, Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart, ID: "a1"}})
	if len(got.Subagents) != 1 {
		t.Fatalf("start: %+v", got.Subagents)
	}
	age := func(d time.Duration) {
		if _, err := h.s.update(ctx, got.ID, "", func(x *row) (bool, error) {
			for i := range x.subs {
				x.subs[i].Updated = x.subs[i].Updated.Add(-d)
			}
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	age(subagentStale + time.Minute)
	if ss, _ := orchclient.Session(ctx, h.c, got.ID); len(ss.Subagents) != 0 {
		t.Fatalf("stale subagent still live: %+v", ss.Subagents)
	}
	if l, _ := orchclient.SessionSubagents(ctx, h.c, got.ID); len(l.Subagents) != 1 || l.Subagents[0].State != orchapi.StateEnded {
		t.Fatalf("GET subagents: %+v", l)
	}
	age(subagentKeep)
	h.hook(orchapi.HookRequest{RegisterRequest: reg, Subagent: &orchapi.HookSubagent{Event: orchapi.SubagentStart, ID: "a2"}})
	if l, _ := orchclient.SessionSubagents(ctx, h.c, got.ID); len(l.Subagents) != 1 || l.Subagents[0].ID != "a2" {
		t.Fatalf("stale subagent not pruned: %+v", l)
	}
}
