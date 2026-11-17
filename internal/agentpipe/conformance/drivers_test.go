package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

type rig struct {
	s      *agentpipe.Session
	events []agentpipe.Event
	sent   []string
}

func newRig(t *testing.T, driver string, st agentpipe.Start) *rig {
	t.Helper()
	d, ok := agentpipe.New(driver)
	if !ok {
		t.Fatalf("no driver %s", driver)
	}
	r := &rig{}
	r.s = &agentpipe.Session{D: d, Em: &agentpipe.Emitter{Out: func(ev agentpipe.Event) { r.events = append(r.events, ev) }},
		Log: t.Logf, Send: func(b []byte) { r.sent = append(r.sent, string(b)) }}
	b, _ := json.Marshal(agentpipe.Op{ID: "s", Op: agentpipe.OpStart, Start: &st})
	if got := r.s.Op(b); got == nil {
		t.Fatal("start not taken")
	}
	if _, err := d.Argv(nil, st); err != nil {
		t.Fatal(err)
	}
	r.s.Started(nil)
	return r
}

func (r *rig) op(op agentpipe.Op) agentpipe.Event {
	b, _ := json.Marshal(op)
	r.s.Op(b)
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Type == agentpipe.EvOp && r.events[i].Op == op.ID {
			return r.events[i]
		}
	}
	return agentpipe.Event{}
}

func (r *rig) line(s string) { r.s.Line([]byte(s)) }

func refused(t *testing.T, ev agentpipe.Event, kind string) {
	t.Helper()
	if ev.OK == nil || *ev.OK || ev.Error == nil || ev.Error.Kind != kind {
		b, _ := json.Marshal(ev)
		t.Fatalf("want a %s refusal, got %s", kind, b)
	}
}

// TestDriversRefuseWrongState: steering, interrupting or answering nothing,
// and a second turn while one runs, are refused, never sent.
func TestDriversRefuseWrongState(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, name, agentpipe.Start{Cwd: "/work"})
			if name == "codex" {
				r.line(`{"id":1,"result":{}}`)
				r.line(`{"id":2,"result":{"thread":{"id":"th"},"model":"m"}}`)
			}
			before := len(r.sent)
			refused(t, r.op(agentpipe.Op{ID: "a", Op: agentpipe.OpSteer, Text: "x"}), agentpipe.RefuseConflict)
			refused(t, r.op(agentpipe.Op{ID: "b", Op: agentpipe.OpInterrupt}), agentpipe.RefuseConflict)
			refused(t, r.op(agentpipe.Op{ID: "c", Op: agentpipe.OpAnswer, Request: "nope", Decision: "accept"}), agentpipe.RefuseConflict)
			refused(t, r.op(agentpipe.Op{ID: "d", Op: agentpipe.OpTurn}), agentpipe.RefuseBadRequest)
			refused(t, r.op(agentpipe.Op{ID: "e", Op: "dance"}), agentpipe.RefuseUnsupported)
			if len(r.sent) != before {
				t.Fatalf("a refused op wrote to the runtime: %v", r.sent[before:])
			}
			r.op(agentpipe.Op{ID: "f", Op: agentpipe.OpTurn, Text: "hello"})
			if name == "codex" {
				r.line(`{"id":3,"result":{"turn":{"id":"tu"}}}`)
			}
			refused(t, r.op(agentpipe.Op{ID: "g", Op: agentpipe.OpTurn, Text: "again"}), agentpipe.RefuseConflict)
			refused(t, r.op(agentpipe.Op{ID: "h", Op: agentpipe.OpStart, Start: &agentpipe.Start{Cwd: "/x"}}), agentpipe.RefuseConflict)
		})
	}
}

// TestCodexNeverAsks: with approvals "never", a request is declined at once
// and no approval event waits on a person.
func TestCodexNeverAsks(t *testing.T) {
	r := newRig(t, "codex", agentpipe.Start{Cwd: "/work", Approvals: "never"})
	r.line(`{"id":1,"result":{}}`)
	if !strings.Contains(r.sent[len(r.sent)-1], `"approvalPolicy":"never"`) {
		t.Fatalf("thread/start: %s", r.sent[len(r.sent)-1])
	}
	r.line(`{"id":2,"result":{"thread":{"id":"th"}}}`)
	r.op(agentpipe.Op{ID: "t", Op: agentpipe.OpTurn, Text: "go"})
	r.line(`{"id":3,"result":{"turn":{"id":"tu"}}}`)
	r.line(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"th","turnId":"tu","itemId":"i","command":"rm -rf x"}}`)
	if last := r.sent[len(r.sent)-1]; last != `{"id":7,"result":{"decision":"decline"}}` {
		t.Fatalf("reply: %s", last)
	}
	for _, ev := range r.events {
		if ev.Type == agentpipe.EvApproval || ev.Status == agentpipe.StateWaiting {
			t.Fatalf("an approval surfaced: %+v", ev)
		}
	}
}

// TestClaudeNeverAsks: approvals "never" hands Claude --permission-prompts
// none, so nothing reaches stdio.
func TestClaudeNeverAsks(t *testing.T) {
	d, _ := agentpipe.New("claude")
	argv, _ := d.Argv([]string{"claude"}, agentpipe.Start{Cwd: "/w", Approvals: "never", Model: "haiku", Resume: "abc"})
	got := strings.Join(argv, " ")
	for _, want := range []string{"--permission-prompts none", "--input-format stream-json", "--model haiku", "--resume abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "permission-prompt-tool") {
		t.Errorf("argv %q asks over stdio", got)
	}
}

// TestDriversTakeToolGrant: a tool grant is Claude's --allowedTools and
// --permission-mode, and Codex's workspace-write sandbox (with the network
// for a shell) when it writes; a read-only grant or none widens nothing.
func TestDriversTakeToolGrant(t *testing.T) {
	d, _ := agentpipe.New("claude")
	grant := agentpipe.Start{Cwd: "/w", Approvals: "never", AllowedTools: []string{"Bash(gh pr view:*)", "Read"}, PermissionMode: "acceptEdits"}
	argv, _ := d.Argv([]string{"claude"}, grant)
	if got := strings.Join(argv, "\x00"); !strings.Contains(got, "--allowedTools\x00Bash(gh pr view:*),Read") || !strings.Contains(got, "--permission-mode\x00acceptEdits") {
		t.Errorf("claude argv %q lacks the grant", argv)
	}
	argv, _ = d.Argv([]string{"claude"}, agentpipe.Start{Cwd: "/w", Approvals: "never"})
	if got := strings.Join(argv, " "); strings.Contains(got, "--allowedTools") || strings.Contains(got, "--permission-mode") {
		t.Errorf("claude argv %q grants without a grant", got)
	}
	r := newRig(t, "codex", grant)
	r.line(`{"id":1,"result":{}}`)
	start := r.sent[len(r.sent)-1]
	for _, want := range []string{`"sandbox":"workspace-write"`, `"sandbox_workspace_write.network_access":true`, `"approvalPolicy":"never"`} {
		if !strings.Contains(start, want) {
			t.Errorf("codex thread/start %s lacks %s", start, want)
		}
	}
	for _, st := range []agentpipe.Start{{Cwd: "/w", Approvals: "never"}, {Cwd: "/w", Approvals: "never", AllowedTools: []string{"Read", "Grep"}}} {
		r = newRig(t, "codex", st)
		r.line(`{"id":1,"result":{}}`)
		if start := r.sent[len(r.sent)-1]; strings.Contains(start, "sandbox") || strings.Contains(start, `"config"`) {
			t.Errorf("codex thread/start %s widens the sandbox for %v", start, st.AllowedTools)
		}
	}
}

// TestClaudeSteerKeepsTheTurn: a steer's line makes Claude end the earlier
// line with an aborted result; the turn goes on until the steer's result.
func TestClaudeSteerKeepsTheTurn(t *testing.T) {
	r := newRig(t, "claude", agentpipe.Start{Cwd: "/work"})
	r.op(agentpipe.Op{ID: "t", Op: agentpipe.OpTurn, Text: "go"})
	var first map[string]any
	_ = json.Unmarshal([]byte(r.sent[0]), &first)
	r.op(agentpipe.Op{ID: "s", Op: agentpipe.OpSteer, Text: "also"})
	var second map[string]any
	_ = json.Unmarshal([]byte(r.sent[1]), &second)
	if second["priority"] != "now" {
		t.Fatalf("steer line: %s", r.sent[1])
	}
	r.line(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"aborted_tools","user_message_uuids":["` + first["uuid"].(string) + `"]}`)
	for _, ev := range r.events {
		if ev.Type == agentpipe.EvTurn && agentpipe.Terminal(ev.Status) {
			t.Fatalf("the steered turn ended early: %+v", ev)
		}
	}
	r.line(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","user_message_uuids":["` + second["uuid"].(string) + `"]}`)
	last := r.events[len(r.events)-2]
	if last.Type != agentpipe.EvTurn || last.Status != agentpipe.TurnCompleted {
		t.Fatalf("turn end: %+v", last)
	}
}

// TestClaudeFailureKinds: a result's API status types the failure.
func TestClaudeFailureKinds(t *testing.T) {
	for status, kind := range map[string]string{"429": agentpipe.ErrQuota, "401": agentpipe.ErrAuth, "500": agentpipe.ErrFailed} {
		r := newRig(t, "claude", agentpipe.Start{Cwd: "/work"})
		r.op(agentpipe.Op{ID: "t", Op: agentpipe.OpTurn, Text: "go"})
		r.line(`{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":` + status + `,"errors":["boom"]}`)
		var end agentpipe.Event
		for _, ev := range r.events {
			if ev.Type == agentpipe.EvTurn && agentpipe.Terminal(ev.Status) {
				end = ev
			}
		}
		if end.Status != agentpipe.TurnFailed || end.Error == nil || end.Error.Kind != kind {
			t.Errorf("status %s: %+v", status, end)
		}
	}
}

// TestDriversStartMCP: mcp servers reach the runtime over its stdin (Claude
// mcp_set_servers, Codex thread config), never its argv; a recording keeps
// no header value or URL query.
func TestDriversStartMCP(t *testing.T) {
	mcp := []agentpipe.MCP{{Name: "t3", URL: "http://127.0.0.1:7/mcp?token=q1", Headers: map[string]string{"Authorization": "Bearer s3cret"}}}
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			st := agentpipe.Start{Cwd: "/work", MCP: mcp}
			d, _ := agentpipe.New(name)
			if !d.Describe().Capabilities.MCP {
				t.Fatal("no mcp capability")
			}
			argv, _ := d.Argv([]string{name}, st)
			if got := strings.Join(argv, " "); strings.Contains(got, "s3cret") || strings.Contains(got, "127.0.0.1") {
				t.Fatalf("argv carries the servers: %q", got)
			}
			r := newRig(t, name, st)
			var want string
			if name == "codex" {
				r.line(`{"id":1,"result":{}}`)
				want = `"config":{"mcp_servers.t3.http_headers":{"Authorization":"Bearer s3cret"},"mcp_servers.t3.url":"http://127.0.0.1:7/mcp?token=q1"}`
			} else {
				want = `"request":{"servers":{"t3":{"headers":{"Authorization":"Bearer s3cret"},"type":"http","url":"http://127.0.0.1:7/mcp?token=q1"}},"subtype":"mcp_set_servers"}`
			}
			if !strings.Contains(strings.Join(r.sent, "\n"), want) {
				t.Fatalf("sent %v, want a line with %s", r.sent, want)
			}
			for _, ev := range r.events {
				if b, _ := json.Marshal(ev); strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "q1") {
					t.Fatalf("an event carries the servers: %s", b)
				}
			}
		})
	}
	t.Run("codex resume", func(t *testing.T) {
		r := newRig(t, "codex", agentpipe.Start{Cwd: "/work", Resume: "th", MCP: mcp})
		r.line(`{"id":1,"result":{}}`)
		last := r.sent[len(r.sent)-1]
		if !strings.Contains(last, `"thread/resume"`) || !strings.Contains(last, `"mcp_servers.t3.url"`) {
			t.Fatalf("thread/resume: %s", last)
		}
	})
	for _, name := range []string{"claude", "codex"} {
		t.Run(name+" recording and errors", func(t *testing.T) {
			path := t.TempDir() + "/r.jsonl"
			rec, err := agentpipe.CreateRecording(path, name)
			if err != nil {
				t.Fatal(err)
			}
			d, _ := agentpipe.New(name)
			var evs []agentpipe.Event
			var logs []string
			s := &agentpipe.Session{D: d, Em: &agentpipe.Emitter{Out: func(ev agentpipe.Event) { evs = append(evs, ev) }},
				Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }, Send: func([]byte) {}, Rec: rec}
			st := agentpipe.Start{Cwd: "/w", MCP: mcp}
			b, _ := json.Marshal(agentpipe.Op{ID: "s", Op: agentpipe.OpStart, Start: &st})
			if s.Op(b) == nil {
				t.Fatal("start not taken")
			}
			if _, err := d.Argv(nil, st); err != nil {
				t.Fatal(err)
			}
			s.Started(nil)
			// The runtime refuses, quoting what it was given.
			if name == "codex" {
				s.Line([]byte(`{"id":1,"result":{}}`))
				s.Line([]byte(`{"id":2,"error":{"code":-1,"message":"bad mcp_servers.t3: http://127.0.0.1:7/mcp?token=q1 header Bearer s3cret"}}`))
			} else {
				s.Line([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"zm-1","response":{"added":[],"errors":{"t3":"connect http://127.0.0.1:7/mcp?token=q1 with Bearer s3cret"}}}}`))
			}
			rec.Close()
			got, _ := os.ReadFile(path)
			all := string(got)
			for _, ev := range evs {
				eb, _ := json.Marshal(ev)
				all += string(eb)
			}
			all += strings.Join(logs, "\n")
			if strings.Contains(all, "s3cret") || strings.Contains(all, "q1") {
				t.Fatalf("a secret reached the recording, an event or a log:\n%s", all)
			}
			if !strings.Contains(string(got), "[redacted]") || !strings.Contains(string(got), `"t":"in"`) {
				t.Fatalf("recording lacks the redacted runtime line: %s", got)
			}
		})
	}
	t.Run("refusals", func(t *testing.T) {
		for _, m := range [][]agentpipe.MCP{
			{{Name: "has space", URL: "http://h/"}},
			{{Name: "a", URL: "ftp://h/"}},
			{{Name: "a", URL: "http://h/"}, {Name: "a", URL: "http://h/"}},
			{{Name: "a", URL: "http://h/", Headers: map[string]string{"Bad Name": "v"}}},
			{{Name: "a", URL: "http://h/", Headers: map[string]string{"X": "v\nInjected: 1"}}},
			{{Name: "a", URL: "http://h/", Headers: map[string]string{"X-A": "1", "x-a": "2"}}},
			{{Name: "a", URL: "http://h/", Headers: map[string]string{"X": "caf\u00e9"}}},
		} {
			d, _ := agentpipe.New("claude")
			var evs []agentpipe.Event
			s := &agentpipe.Session{D: d, Em: &agentpipe.Emitter{Out: func(ev agentpipe.Event) { evs = append(evs, ev) }}, Log: t.Logf, Send: func([]byte) {}}
			b, _ := json.Marshal(agentpipe.Op{ID: "s", Op: agentpipe.OpStart, Start: &agentpipe.Start{Cwd: "/w", MCP: m}})
			if s.Op(b) != nil || len(evs) != 1 {
				t.Fatalf("%+v: start taken (%+v)", m, evs)
			}
			refused(t, evs[0], agentpipe.RefuseBadRequest)
		}
	})
}
