// Package claude is the zm-agent/1 driver for Claude Code's stream-json
// mode: `claude --input-format stream-json --output-format stream-json
// --verbose --permission-prompt-tool stdio`. One process lives for the
// session; each turn is a user line on stdin and ends with a result line
// carrying that line's uuid. Permission prompts arrive as can_use_tool
// control requests and are answered with control responses. Shapes are
// from Claude Code 2.1.318's own output and the Claude Agent SDK's
// declarations, read through T3 Code's ClaudeAdapterV2 (MIT); no code is
// copied.
package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

func init() { agentpipe.Register("claude", func() agentpipe.Driver { return &driver{} }) }

var caps = agentpipe.Capabilities{Steer: true, Interrupt: true, Approvals: true, Questions: true, Resume: true,
	Images: false, Partial: true, Usage: true, Plan: true, Model: true, MCP: true, Tools: true}

type tool struct {
	kind, name, title string
	input             json.RawMessage
	parent            string
	turn              string
}

type pending struct {
	id          string // the CLI's request_id
	tool        string
	toolUseID   string
	input       json.RawMessage
	suggestions json.RawMessage
	kind        string
	options     []string
	turn        string
	item        string
	questions   []string
}

type driver struct {
	st      agentpipe.Start
	runtime string
	model   string
	version string
	turns   int
	turn    string
	last    string            // the uuid of the turn's latest user line (its result ends the turn)
	uuids   map[string]string // user line uuid -> turn
	interr  bool              // an interrupt was asked for the running turn
	tools   map[string]*tool
	blocks  map[string]int // assistant message id -> frames seen
	waiting map[string]*pending
	order   []string
	ctl     map[string]string // our control request id -> op id
	ctlN    int
	usage   agentpipe.Usage
	state   string

	streamMsg string // the message the stream is in
}

func (d *driver) Describe() agentpipe.Describe {
	return agentpipe.Describe{Proto: agentpipe.Proto, Adapter: "claude", Capabilities: caps}
}

func has(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// Argv adds the stream-json flags to the configured claude command.
func (d *driver) Argv(cli []string, st agentpipe.Start) ([]string, error) {
	if len(cli) == 0 {
		cli = []string{"claude"}
	}
	d.st = st
	out := append([]string(nil), cli...)
	out = append(out, "--output-format", "stream-json", "--input-format", "stream-json", "--verbose", "--include-partial-messages")
	switch st.Approvals {
	case "never":
		out = append(out, "--permission-prompts", "none")
	default:
		out = append(out, "--permission-prompt-tool", "stdio")
	}
	switch {
	case st.PermissionMode != "":
		out = append(out, "--permission-mode", st.PermissionMode)
	case st.Approvals == "ask" && !has(cli, "--permission-mode"):
		out = append(out, "--permission-mode", "default")
	}
	if len(st.AllowedTools) > 0 {
		out = append(out, "--allowedTools", strings.Join(st.AllowedTools, ","))
	}
	if st.Model != "" && !has(cli, "--model") {
		out = append(out, "--model", st.Model)
	}
	if st.Resume != "" {
		out = append(out, "--resume", st.Resume)
	}
	return out, nil
}

func (d *driver) Begin(io agentpipe.IO) {
	d.uuids, d.tools, d.blocks = map[string]string{}, map[string]*tool{}, map[string]int{}
	d.waiting, d.ctl = map[string]*pending{}, map[string]string{}
	d.runtime = d.st.Resume
	if len(d.st.MCP) > 0 {
		// Over stdin, never --mcp-config: header values may be credentials,
		// and argv is visible to every process on the machine.
		servers := map[string]any{}
		for _, m := range d.st.MCP {
			c := map[string]any{"type": "http", "url": m.URL}
			if len(m.Headers) > 0 {
				c["headers"] = m.Headers
			}
			servers[m.Name] = c
		}
		d.control(io, "", map[string]any{"subtype": "mcp_set_servers", "servers": servers})
	}
	d.setState(io, agentpipe.StateReady)
}

func (d *driver) setState(io agentpipe.IO, s string) {
	if d.state == s {
		return
	}
	d.state = s
	io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: s, RuntimeID: d.runtime})
}

// lineUUID is a stable uuid for a user line: derived from zm's op id, so
// replays write the same bytes and a resumed session never reuses one.
func lineUUID(opID string, n int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("zm-agent/claude/%s/%d", opID, n)))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (d *driver) userLine(io agentpipe.IO, op agentpipe.Op, priority string) string {
	d.ctlN++
	id := lineUUID(op.ID, d.ctlN)
	m := map[string]any{"type": "user", "parent_tool_use_id": nil, "uuid": id,
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": op.Text}}}}
	if priority != "" {
		m["priority"] = priority
	}
	io.Send(m)
	d.uuids[id] = d.turn
	d.last = id
	io.Emit(agentpipe.Event{Type: agentpipe.EvMessage, Turn: d.turn, Item: id, Role: "user", Status: agentpipe.ItemCompleted, Text: op.Text})
	return id
}

func (d *driver) control(io agentpipe.IO, opID string, req map[string]any) {
	d.ctlN++
	id := "zm-" + strconv.Itoa(d.ctlN)
	d.ctl[id] = opID
	io.Send(map[string]any{"type": "control_request", "request_id": id, "request": req})
}

func (d *driver) Op(io agentpipe.IO, op agentpipe.Op) {
	switch op.Op {
	case agentpipe.OpTurn:
		if op.Text == "" {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseBadRequest, "a turn needs text"))
			return
		}
		if len(op.Images) > 0 {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseUnsupported, "this adapter takes no images yet"))
			return
		}
		if d.turn != "" {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "a turn is running: steer it or interrupt it"))
			return
		}
		if op.Model != "" && op.Model != d.model {
			d.control(io, "", map[string]any{"subtype": "set_model", "model": op.Model})
		}
		d.turns++
		d.turn, d.interr = "t"+strconv.Itoa(d.turns), false
		io.Emit(agentpipe.Event{Type: agentpipe.EvTurn, Turn: d.turn, Status: agentpipe.TurnRunning})
		d.userLine(io, op, "")
		io.Emit(agentpipe.OpResult(op.ID, nil))
		d.setState(io, agentpipe.StateRunning)
	case agentpipe.OpSteer:
		if d.turn == "" || (op.Turn != "" && op.Turn != d.turn) {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "no such running turn"))
			return
		}
		if op.Text == "" || len(op.Images) > 0 {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseBadRequest, "steer needs text (no images)"))
			return
		}
		d.userLine(io, op, "now")
		io.Emit(agentpipe.OpResult(op.ID, nil))
	case agentpipe.OpInterrupt:
		if d.turn == "" || (op.Turn != "" && op.Turn != d.turn) {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "no such running turn"))
			return
		}
		d.interr = true
		d.control(io, op.ID, map[string]any{"subtype": "interrupt"})
	case agentpipe.OpAnswer:
		d.answer(io, op)
	default:
		io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseUnsupported, "unknown operation "+strconv.Quote(op.Op)))
	}
}

func (d *driver) Stop(io agentpipe.IO) {}

type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Parent    *string         `json:"parent_tool_use_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`

	// system/init
	Model   string `json:"model"`
	Cwd     string `json:"cwd"`
	Version string `json:"claude_code_version"`

	// result
	IsError        bool            `json:"is_error"`
	Result         *string         `json:"result"`
	TerminalReason string          `json:"terminal_reason"`
	APIErrorStatus *int            `json:"api_error_status"`
	Errors         []string        `json:"errors"`
	UserUUIDs      []string        `json:"user_message_uuids"`
	Usage          json.RawMessage `json:"usage"`
	TotalCost      float64         `json:"total_cost_usd"`
	ModelUsage     map[string]struct {
		ContextWindow int64 `json:"contextWindow"`
	} `json:"modelUsage"`

	// rate_limit_event
	RateLimit *struct {
		Status        string `json:"status"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
	// system/api_retry
	Attempt int    `json:"attempt"`
	Error   string `json:"error"`
}

type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	ToolUse  string          `json:"tool_use_id"`
	Content  json.RawMessage `json:"content"`
	IsError  bool            `json:"is_error"`
}

func (d *driver) Line(io agentpipe.IO, raw []byte) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		io.Logf("claude: not JSON: %s", agentpipe.Head(string(raw), 200))
		return
	}
	parent := ""
	if l.Parent != nil {
		parent = *l.Parent
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			if l.SessionID != d.runtime || l.Model != d.model || l.Version != d.version {
				d.runtime, d.model, d.version = l.SessionID, l.Model, l.Version
				io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: d.state, RuntimeID: d.runtime, Model: d.model, Version: d.version})
			}
		case "api_retry":
			io.Emit(agentpipe.Event{Type: agentpipe.EvNotice, Turn: d.turn, Level: "warn",
				Error: &agentpipe.Error{Kind: agentpipe.ErrFailed, Message: "retrying the API: " + agentpipe.Head(l.Error, 500)}})
		}
	case "stream_event":
		d.stream(io, l)
	case "assistant":
		d.assistant(io, l, parent)
	case "user":
		d.toolResults(io, l)
	case "control_request":
		d.ask(io, l)
	case "control_cancel_request":
		if w, ok := d.waiting[l.RequestID]; ok {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	case "control_response":
		var r struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
			Response  struct {
				Errors map[string]json.RawMessage `json:"errors"` // mcp_set_servers: per server
			} `json:"response"`
		}
		_ = json.Unmarshal(l.Response, &r)
		op, ok := d.ctl[r.RequestID]
		if !ok {
			return
		}
		delete(d.ctl, r.RequestID)
		if op == "" {
			// Our own request (mcp_set_servers): the session goes on without
			// what failed. Names only: a message may quote a URL or header.
			if r.Subtype == "error" {
				io.Logf("control request %s refused", r.RequestID)
			} else if len(r.Response.Errors) > 0 {
				io.Logf("mcp servers not added: %s", strings.Join(sortedKeys(r.Response.Errors), ", "))
			}
			return
		}
		if r.Subtype == "error" {
			io.Emit(agentpipe.Refuse(op, agentpipe.RefuseConflict, r.Error))
			return
		}
		io.Emit(agentpipe.OpResult(op, nil))
	case "result":
		d.result(io, l)
	case "rate_limit_event":
		if l.RateLimit != nil && l.RateLimit.Status == "rejected" {
			io.Emit(agentpipe.Event{Type: agentpipe.EvNotice, Turn: d.turn, Level: "error",
				Error: &agentpipe.Error{Kind: agentpipe.ErrQuota, Message: "usage limit reached (" + l.RateLimit.RateLimitType + ")"}})
		}
	}
}

// stream turns text deltas into message.delta events; the item id is the
// one the whole message gets (message id and block index).
func (d *driver) stream(io agentpipe.IO, l line) {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(l.Event, &e) != nil {
		return
	}
	switch e.Type {
	case "message_start":
		d.streamMsg = e.Message.ID
	case "content_block_delta":
		if e.Delta.Type == "text_delta" && e.Delta.Text != "" && d.streamMsg != "" && (l.Parent == nil || *l.Parent == "") {
			io.Emit(agentpipe.Event{Type: agentpipe.EvMessageDelta, Turn: d.turn, Item: d.streamMsg + "/" + strconv.Itoa(e.Index), Role: "assistant", Text: e.Delta.Text})
		}
	}
}

func toolKind(name string) string {
	switch {
	case name == "Bash":
		return agentpipe.ToolCommand
	case name == "Edit" || name == "Write" || name == "MultiEdit" || name == "NotebookEdit":
		return agentpipe.ToolFileChange
	case name == "WebFetch" || name == "WebSearch":
		return agentpipe.ToolWeb
	case name == "Task" || name == "Agent":
		return agentpipe.ToolTask
	case strings.HasPrefix(name, "mcp__"):
		return agentpipe.ToolMCP
	}
	return agentpipe.ToolOther
}

func toolTitle(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "notebook_path", "url", "query", "pattern", "description", "prompt"} {
		if s, ok := in[k].(string); ok && s != "" {
			return agentpipe.OneLine(s)
		}
	}
	return name
}

func (d *driver) assistant(io agentpipe.IO, l line, parent string) {
	var m struct {
		ID      string  `json:"id"`
		Content []block `json:"content"`
	}
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		idx := d.blocks[m.ID]
		d.blocks[m.ID] = idx + 1
		item := m.ID + "/" + strconv.Itoa(idx)
		switch b.Type {
		case "text":
			if parent != "" || b.Text == "" {
				continue // a subagent's words stay inside its tool
			}
			io.Emit(agentpipe.Event{Type: agentpipe.EvMessage, Turn: d.turn, Item: item, Role: "assistant", Status: agentpipe.ItemCompleted, Text: b.Text})
		case "thinking":
			if parent != "" || strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			io.Emit(agentpipe.Event{Type: agentpipe.EvReasoning, Turn: d.turn, Item: item, Status: agentpipe.ItemCompleted, Text: b.Thinking})
		case "tool_use":
			t := &tool{kind: toolKind(b.Name), name: b.Name, title: toolTitle(b.Name, b.Input), input: agentpipe.BoundJSON(b.Input), parent: parent, turn: d.turn}
			d.tools[b.ID] = t
			io.Emit(agentpipe.Event{Type: agentpipe.EvTool, Turn: d.turn, Item: b.ID, Kind: t.kind, Name: t.name, Title: t.title,
				Input: t.input, Parent: parent, Status: agentpipe.ItemRunning})
			if b.Name == "TodoWrite" {
				d.todo(io, b.Input)
			}
		}
	}
}

func (d *driver) todo(io agentpipe.IO, input json.RawMessage) {
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if json.Unmarshal(input, &in) != nil {
		return
	}
	ev := agentpipe.Event{Type: agentpipe.EvPlan, Turn: d.turn}
	for _, t := range in.Todos {
		st := t.Status
		if st != "completed" && st != "in_progress" {
			st = "pending"
		}
		ev.Steps = append(ev.Steps, agentpipe.Step{Text: t.Content, Status: st})
	}
	io.Emit(ev)
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Type == "text" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func (d *driver) toolResults(io agentpipe.IO, l line) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		t, ok := d.tools[b.ToolUse]
		if !ok {
			continue
		}
		delete(d.tools, b.ToolUse)
		st := agentpipe.ItemCompleted
		if b.IsError {
			st = agentpipe.ItemFailed
		}
		io.Emit(agentpipe.Event{Type: agentpipe.EvTool, Turn: t.turn, Item: b.ToolUse, Kind: t.kind, Name: t.name, Title: t.title,
			Input: t.input, Parent: t.parent, Status: st, Output: agentpipe.Clip(resultText(b.Content), agentpipe.MaxOutput)})
	}
}

func (d *driver) ask(io agentpipe.IO, l line) {
	var r struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		Input       json.RawMessage `json:"input"`
		ToolUseID   string          `json:"tool_use_id"`
		Suggestions json.RawMessage `json:"permission_suggestions"`
		Description string          `json:"description"`
		Reason      string          `json:"decision_reason"`
		NoAlways    bool            `json:"suppress_always_allow_rule"`
	}
	_ = json.Unmarshal(l.Request, &r)
	if r.Subtype != "can_use_tool" {
		// Hooks, MCP messages, elicitations: zm registered none.
		io.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": l.RequestID,
			"error": "zm does not serve " + r.Subtype}})
		return
	}
	w := &pending{id: l.RequestID, tool: r.ToolName, toolUseID: r.ToolUseID, input: r.Input, suggestions: r.Suggestions,
		turn: d.turn, item: r.ToolUseID}
	title := toolTitle(r.ToolName, r.Input)
	detail := r.Description
	if r.Reason != "" {
		detail = strings.TrimSpace(detail + "\n" + r.Reason)
	}
	switch toolKind(r.ToolName) {
	case agentpipe.ToolCommand:
		w.kind = agentpipe.ApprovalCommand
	case agentpipe.ToolFileChange:
		w.kind = agentpipe.ApprovalFileChange
	default:
		w.kind = agentpipe.ApprovalTool
	}
	w.options = []string{agentpipe.DecisionAccept, agentpipe.DecisionAcceptSession, agentpipe.DecisionDecline, agentpipe.DecisionCancel}
	if r.NoAlways {
		w.options = []string{agentpipe.DecisionAccept, agentpipe.DecisionDecline, agentpipe.DecisionCancel}
	}
	if r.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(r.Input, &in)
		var qs []string
		for _, q := range in.Questions {
			w.questions = append(w.questions, q.Question)
			qs = append(qs, q.Question)
		}
		w.kind, w.options = agentpipe.ApprovalQuestion, []string{agentpipe.DecisionAccept, agentpipe.DecisionDecline}
		if len(qs) > 0 {
			title = qs[0]
		}
		detail = strings.Join(qs, "\n")
	}
	d.waiting[w.id], d.order = w, append(d.order, w.id)
	io.Emit(agentpipe.Event{Type: agentpipe.EvApproval, Turn: w.turn, Item: w.item, Request: w.id, Kind: w.kind, Status: agentpipe.ItemPending,
		Title: agentpipe.OneLine(title), Detail: agentpipe.Head(detail, 4000), Options: w.options})
	d.setState(io, agentpipe.StateWaiting)
}

func (d *driver) resolve(io agentpipe.IO, w *pending, status, decision string) {
	if _, ok := d.waiting[w.id]; !ok {
		return
	}
	delete(d.waiting, w.id)
	for i, k := range d.order {
		if k == w.id {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	io.Emit(agentpipe.Event{Type: agentpipe.EvApproval, Turn: w.turn, Item: w.item, Request: w.id, Kind: w.kind, Status: status, Decision: decision})
	if len(d.waiting) == 0 && d.state == agentpipe.StateWaiting {
		d.setState(io, agentpipe.StateRunning)
	}
}

func (d *driver) answer(io agentpipe.IO, op agentpipe.Op) {
	w, ok := d.waiting[op.Request]
	if !ok {
		io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "no such pending request"))
		return
	}
	valid := false
	for _, o := range w.options {
		valid = valid || o == op.Decision
	}
	if !valid {
		io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseBadRequest, fmt.Sprintf("decision must be one of %v", w.options)))
		return
	}
	var resp map[string]any
	switch op.Decision {
	case agentpipe.DecisionAccept, agentpipe.DecisionAcceptSession:
		input := w.input
		if w.kind == agentpipe.ApprovalQuestion {
			var in map[string]any
			_ = json.Unmarshal(w.input, &in)
			if in == nil {
				in = map[string]any{}
			}
			answers := map[string]string{}
			for i, q := range w.questions {
				if i == 0 {
					answers[q] = op.Answer
				}
			}
			in["answers"] = answers
			input, _ = json.Marshal(in)
		}
		resp = map[string]any{"behavior": "allow", "updatedInput": input, "toolUseID": w.toolUseID}
		if op.Decision == agentpipe.DecisionAcceptSession {
			resp["updatedPermissions"] = sessionRules(w.tool, w.suggestions)
		}
	default:
		msg := op.Answer
		if msg == "" {
			msg = "The user declined this tool use."
		}
		resp = map[string]any{"behavior": "deny", "message": msg, "toolUseID": w.toolUseID}
		if op.Decision == agentpipe.DecisionCancel {
			resp["interrupt"] = true
		}
	}
	io.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": w.id, "response": resp}})
	io.Emit(agentpipe.OpResult(op.ID, nil))
	d.resolve(io, w, agentpipe.ItemAnswered, op.Decision)
}

// sessionRules are the CLI's own suggestions scoped to this session, or a
// rule allowing the tool for the session.
func sessionRules(tool string, suggestions json.RawMessage) []map[string]any {
	var s []map[string]any
	_ = json.Unmarshal(suggestions, &s)
	for _, x := range s {
		x["destination"] = "session"
	}
	if len(s) > 0 {
		return s
	}
	return []map[string]any{{"type": "addRules", "rules": []map[string]any{{"toolName": tool}}, "behavior": "allow", "destination": "session"}}
}

func (d *driver) result(io agentpipe.IO, l line) {
	var u struct {
		Input       int64 `json:"input_tokens"`
		Output      int64 `json:"output_tokens"`
		CacheRead   int64 `json:"cache_read_input_tokens"`
		CacheCreate int64 `json:"cache_creation_input_tokens"`
	}
	_ = json.Unmarshal(l.Usage, &u)
	d.usage.InputTokens += u.Input + u.CacheRead + u.CacheCreate
	d.usage.OutputTokens += u.Output
	d.usage.CachedInputTokens += u.CacheRead
	d.usage.ContextTokens = u.Input + u.CacheRead + u.CacheCreate + u.Output
	d.usage.CostUSD = l.TotalCost
	for _, m := range l.ModelUsage {
		if m.ContextWindow > d.usage.ContextWindow {
			d.usage.ContextWindow = m.ContextWindow
		}
	}
	// A result ends the turn only for the turn's latest user line: a
	// steer's line makes the CLI end the earlier one, aborted, and go on.
	mine := false
	for _, id := range l.UserUUIDs {
		mine = mine || id == d.last
	}
	if d.turn == "" || (!mine && len(l.UserUUIDs) > 0) {
		return
	}
	usage := d.usage
	io.Emit(agentpipe.Event{Type: agentpipe.EvUsage, Turn: d.turn, Usage: &usage})
	for _, k := range append([]string(nil), d.order...) {
		if w := d.waiting[k]; w != nil && w.turn == d.turn {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	}
	for _, id := range sortedKeys(d.tools) {
		if t := d.tools[id]; t.turn == d.turn {
			delete(d.tools, id)
			io.Emit(agentpipe.Event{Type: agentpipe.EvTool, Turn: t.turn, Item: id, Kind: t.kind, Name: t.name, Title: t.title,
				Input: t.input, Parent: t.parent, Status: agentpipe.ItemFailed})
		}
	}
	ev := agentpipe.Event{Type: agentpipe.EvTurn, Turn: d.turn, Usage: &usage}
	aborted := strings.HasPrefix(l.TerminalReason, "aborted")
	switch {
	case aborted || (d.interr && l.Subtype != "success"):
		ev.Status = agentpipe.TurnInterrupted
	case l.Subtype == "success" && !l.IsError:
		ev.Status = agentpipe.TurnCompleted
	default:
		ev.Status = agentpipe.TurnFailed
		msg := strings.Join(l.Errors, "; ")
		if msg == "" && l.Result != nil {
			msg = *l.Result
		}
		if msg == "" {
			msg = l.Subtype
		}
		ev.Error = &agentpipe.Error{Kind: classify(l.APIErrorStatus, msg), Message: agentpipe.Head(msg, 2000)}
	}
	io.Emit(ev)
	d.turn, d.interr = "", false
	d.setState(io, agentpipe.StateReady)
}

func classify(status *int, msg string) string {
	if status != nil {
		switch *status {
		case 401, 403:
			return agentpipe.ErrAuth
		case 429:
			return agentpipe.ErrQuota
		}
	}
	l := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(l, "not logged in"), strings.HasPrefix(l, "invalid api key"), strings.Contains(l, "authentication_error"):
		return agentpipe.ErrAuth
	case strings.Contains(l, "usage limit"), strings.Contains(l, "rate_limit"):
		return agentpipe.ErrQuota
	}
	return agentpipe.ErrFailed
}

func (d *driver) Exit(io agentpipe.IO, code int, stopped bool) {
	for _, k := range append([]string(nil), d.order...) {
		if w := d.waiting[k]; w != nil {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	}
	for _, k := range sortedKeys(d.ctl) {
		if op := d.ctl[k]; op != "" {
			io.Emit(agentpipe.Refuse(op, agentpipe.ErrInfra, "the runtime exited"))
		}
	}
	for _, id := range sortedKeys(d.tools) {
		t := d.tools[id]
		delete(d.tools, id)
		io.Emit(agentpipe.Event{Type: agentpipe.EvTool, Turn: t.turn, Item: id, Kind: t.kind, Name: t.name, Title: t.title,
			Input: t.input, Parent: t.parent, Status: agentpipe.ItemFailed})
	}
	if d.turn != "" {
		ev := agentpipe.Event{Type: agentpipe.EvTurn, Turn: d.turn, Status: agentpipe.TurnCancelled}
		if !stopped {
			ev.Status = agentpipe.TurnFailed
			ev.Error = &agentpipe.Error{Kind: agentpipe.ErrInfra, Message: fmt.Sprintf("claude exited (code %d) during the turn", code)}
		}
		io.Emit(ev)
		d.turn = ""
	}
	if code == 0 || stopped {
		io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped, RuntimeID: d.runtime})
		return
	}
	io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateError, RuntimeID: d.runtime,
		Error: &agentpipe.Error{Kind: agentpipe.ErrInfra, Message: fmt.Sprintf("claude exited with code %d", code)}})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
