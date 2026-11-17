// Package codex is the zm-agent/1 driver for `codex app-server`: JSON-RPC
// without the "jsonrpc" member, one object per line (client request ids
// from 1; the server's own requests have their own ids). Shapes follow the
// schema `codex app-server generate-json-schema` prints (checked against
// codex-cli 0.166.0), and T3 Code's CodexAdapterV2 (MIT) as a reading aid;
// no code is copied.
package codex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/agentpipe"
	"github.com/sengirunners/potluck/kit/api"
)

func init() { agentpipe.Register("codex", func() agentpipe.Driver { return &driver{} }) }

var caps = agentpipe.Capabilities{Steer: true, Interrupt: true, Approvals: true, Questions: true, Resume: true,
	Images: true, Partial: true, Usage: true, Plan: true, ToolOutput: true, Model: true, MCP: true, Tools: true}

type pending struct {
	id       json.RawMessage // the server's request id, as sent
	method   string
	params   json.RawMessage
	turn     string
	item     string
	kind     string
	title    string
	options  []string
	question []string // requestUserInput: question ids
}

type driver struct {
	st      agentpipe.Start
	nextID  int
	calls   map[int]call // our requests in flight
	thread  string
	turn    string // the running turn ("" when none)
	turnOps []string
	waiting map[string]*pending // by request id
	order   []string            // pending ids, oldest first
	titles  map[string]string   // item -> title (for approvals of an item)
	queued  []agentpipe.Op      // turn ops sent before the thread was ready
	failure *agentpipe.Error    // the turn's last error notification
	usage   agentpipe.Usage
	ready   bool
	state   string
	open    map[string]agentpipe.Event // running tool items, by id
	openIDs []string                   // their order
}

// tool emits a tool event and tracks it while it runs.
func (d *driver) tool(io agentpipe.IO, ev agentpipe.Event) {
	if d.open == nil {
		d.open = map[string]agentpipe.Event{}
	}
	if ev.Status == agentpipe.ItemRunning {
		if _, ok := d.open[ev.Item]; !ok {
			d.openIDs = append(d.openIDs, ev.Item)
		}
		d.open[ev.Item] = ev
	} else if _, ok := d.open[ev.Item]; ok {
		delete(d.open, ev.Item)
		for i, id := range d.openIDs {
			if id == ev.Item {
				d.openIDs = append(d.openIDs[:i], d.openIDs[i+1:]...)
				break
			}
		}
	}
	io.Emit(ev)
}

// closeTools ends a turn's tools still running (an interrupt leaves them).
func (d *driver) closeTools(io agentpipe.IO, turn string) {
	for _, id := range append([]string(nil), d.openIDs...) {
		if ev := d.open[id]; ev.Turn == turn || turn == "" {
			ev.Status = agentpipe.ItemFailed
			d.tool(io, ev)
		}
	}
}

type call struct {
	method string
	op     string // the op id it answers
	turnOp *agentpipe.Op
}

func (d *driver) Describe() agentpipe.Describe {
	return agentpipe.Describe{Proto: agentpipe.Proto, Adapter: "codex", Capabilities: caps}
}

// Argv is the configured codex command with the app-server subcommand;
// model and approvals travel in thread/start.
func (d *driver) Argv(cli []string, st agentpipe.Start) ([]string, error) {
	if len(cli) == 0 {
		cli = []string{"codex"}
	}
	d.st = st
	out := append([]string(nil), cli...)
	return append(out, "app-server"), nil
}

func (d *driver) request(io agentpipe.IO, method string, params any, c call) {
	d.nextID++
	if d.calls == nil {
		d.calls = map[int]call{}
	}
	c.method = method
	d.calls[d.nextID] = c
	io.Send(map[string]any{"id": d.nextID, "method": method, "params": params})
}

func (d *driver) setState(io agentpipe.IO, s string) {
	if d.state == s {
		return
	}
	d.state = s
	io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: s, RuntimeID: d.thread})
}

func (d *driver) Begin(io agentpipe.IO) {
	d.waiting, d.titles = map[string]*pending{}, map[string]string{}
	d.request(io, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "zm", "title": "zm", "version": api.Version},
		"capabilities": map[string]any{"experimentalApi": true, "optOutNotificationMethods": []string{"turn/diff/updated"}},
	}, call{})
}

func (d *driver) threadParams() map[string]any {
	p := map[string]any{"cwd": d.st.Cwd}
	if d.st.Model != "" {
		p["model"] = d.st.Model
	}
	grant, shell := d.st.PermissionMode == "acceptEdits", false
	for _, t := range d.st.AllowedTools {
		name, _, _ := strings.Cut(t, "(")
		switch name {
		case "Bash":
			grant, shell = true, true
		case "Edit", "Write", "MultiEdit", "NotebookEdit":
			grant = true
		}
	}
	switch d.st.Approvals {
	case "never":
		p["approvalPolicy"] = "never"
	case "ask":
		p["approvalPolicy"] = "untrusted"
		if grant {
			p["approvalPolicy"] = "on-request"
		}
	}
	cfg := map[string]any{}
	if grant {
		// Codex has no per-tool list: a grant that writes is its
		// workspace-write sandbox, with the network for a shell (Claude's Bash
		// has it); a read-only grant leaves the sandbox as it was.
		p["sandbox"] = "workspace-write"
		if shell {
			cfg["sandbox_workspace_write.network_access"] = true
		}
	}
	if len(d.st.MCP) > 0 {
		// Config overrides travel in the request, never `-c` on argv: header
		// values may be credentials.
		for _, m := range d.st.MCP {
			cfg["mcp_servers."+m.Name+".url"] = m.URL
			if len(m.Headers) > 0 {
				cfg["mcp_servers."+m.Name+".http_headers"] = m.Headers
			}
		}
	}
	if len(cfg) > 0 {
		p["config"] = cfg
	}
	return p
}

func input(op agentpipe.Op) []map[string]any {
	in := []map[string]any{}
	if op.Text != "" {
		in = append(in, map[string]any{"type": "text", "text": op.Text})
	}
	for _, p := range op.Images {
		in = append(in, map[string]any{"type": "localImage", "path": p})
	}
	return in
}

func (d *driver) Op(io agentpipe.IO, op agentpipe.Op) {
	switch op.Op {
	case agentpipe.OpTurn:
		if op.Text == "" && len(op.Images) == 0 {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseBadRequest, "a turn needs text or images"))
			return
		}
		if d.turn != "" || len(d.turnOps) > 0 {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "a turn is running: steer it or interrupt it"))
			return
		}
		if !d.ready {
			d.queued = append(d.queued, op)
			d.turnOps = append(d.turnOps, op.ID)
			return
		}
		d.startTurn(io, op)
	case agentpipe.OpSteer:
		if d.turn == "" || (op.Turn != "" && op.Turn != d.turn) {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "no such running turn"))
			return
		}
		if op.Text == "" && len(op.Images) == 0 {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseBadRequest, "steer needs text or images"))
			return
		}
		d.request(io, "turn/steer", map[string]any{"threadId": d.thread, "expectedTurnId": d.turn, "input": input(op)}, call{op: op.ID})
	case agentpipe.OpInterrupt:
		if d.turn == "" || (op.Turn != "" && op.Turn != d.turn) {
			io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseConflict, "no such running turn"))
			return
		}
		d.request(io, "turn/interrupt", map[string]any{"threadId": d.thread, "turnId": d.turn}, call{op: op.ID})
	case agentpipe.OpAnswer:
		d.answer(io, op)
	default:
		io.Emit(agentpipe.Refuse(op.ID, agentpipe.RefuseUnsupported, "unknown operation "+strconv.Quote(op.Op)))
	}
}

func (d *driver) startTurn(io agentpipe.IO, op agentpipe.Op) {
	p := map[string]any{"threadId": d.thread, "input": input(op)}
	if op.Model != "" {
		p["model"] = op.Model
	}
	if len(d.turnOps) == 0 {
		d.turnOps = append(d.turnOps, op.ID)
	}
	o := op
	d.request(io, "turn/start", p, call{op: op.ID, turnOp: &o})
}

func (d *driver) Stop(io agentpipe.IO) {}

type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (d *driver) Line(io agentpipe.IO, line []byte) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		io.Logf("codex: not JSON: %s", agentpipe.Head(string(line), 200))
		return
	}
	switch {
	case f.Method != "" && len(f.ID) > 0:
		d.serverRequest(io, f)
	case f.Method != "":
		d.notification(io, f.Method, f.Params)
	case len(f.ID) > 0:
		d.response(io, f)
	}
}

func (d *driver) response(io agentpipe.IO, f frame) {
	id, err := strconv.Atoi(string(f.ID))
	if err != nil {
		return
	}
	c, ok := d.calls[id]
	if !ok {
		return
	}
	delete(d.calls, id)
	if f.Error != nil {
		msg := string(d.st.ScrubMCP([]byte(f.Error.Message))) // a config error may quote an MCP url or header
		switch c.method {
		case "initialize", "thread/start", "thread/resume":
			io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateError,
				Error: &agentpipe.Error{Kind: classify(msg, ""), Message: c.method + ": " + msg}})
			d.state = agentpipe.StateError
			for _, op := range d.queued {
				io.Emit(agentpipe.Refuse(op.ID, agentpipe.ErrInfra, "the runtime did not start"))
			}
			d.queued, d.turnOps = nil, nil
		case "turn/start":
			d.turnOps = nil
			io.Emit(agentpipe.Refuse(c.op, classify(msg, ""), msg))
		default:
			if c.op != "" {
				io.Emit(agentpipe.Refuse(c.op, agentpipe.RefuseConflict, msg))
			}
		}
		return
	}
	switch c.method {
	case "initialize":
		io.Send(map[string]any{"method": "initialized"})
		if d.st.Resume != "" {
			p := d.threadParams()
			p["threadId"] = d.st.Resume
			p["excludeTurns"] = true
			d.request(io, "thread/resume", p, call{})
		} else {
			d.request(io, "thread/start", d.threadParams(), call{})
		}
	case "thread/start", "thread/resume":
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model string `json:"model"`
		}
		_ = json.Unmarshal(f.Result, &r)
		d.thread, d.ready = r.Thread.ID, true
		d.state = agentpipe.StateReady
		io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateReady, RuntimeID: d.thread, Model: r.Model})
		q := d.queued
		d.queued = nil
		for _, op := range q {
			d.startTurn(io, op)
		}
	case "turn/start":
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(f.Result, &r)
		d.beginTurn(io, r.Turn.ID)
		io.Emit(agentpipe.OpResult(c.op, nil))
	case "turn/steer", "turn/interrupt":
		io.Emit(agentpipe.OpResult(c.op, nil))
	}
}

func (d *driver) beginTurn(io agentpipe.IO, id string) {
	if id == "" || d.turn == id {
		return
	}
	d.turn, d.turnOps, d.failure = id, nil, nil
	io.Emit(agentpipe.Event{Type: agentpipe.EvTurn, Turn: id, Status: agentpipe.TurnRunning})
	d.setState(io, agentpipe.StateRunning)
}

func (d *driver) notification(io agentpipe.IO, method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		d.beginTurn(io, p.Turn.ID)
	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message        string          `json:"message"`
					CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		d.endTurn(io, p.Turn.ID, p.Turn.Status, p.Turn.Error)
	case "item/started", "item/completed":
		var p struct {
			Item   json.RawMessage `json:"item"`
			TurnID string          `json:"turnId"`
		}
		_ = json.Unmarshal(params, &p)
		d.item(io, p.TurnID, p.Item, method == "item/completed")
	case "item/agentMessage/delta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Delta != "" {
			io.Emit(agentpipe.Event{Type: agentpipe.EvMessageDelta, Turn: p.TurnID, Item: p.ItemID, Role: "assistant", Text: p.Delta})
		}
	case "item/commandExecution/outputDelta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Delta != "" {
			io.Emit(agentpipe.Event{Type: agentpipe.EvOutput, Turn: p.TurnID, Item: p.ItemID, Text: agentpipe.Clip(p.Delta, agentpipe.MaxChunk)})
		}
	case "turn/plan/updated":
		var p struct {
			TurnID string `json:"turnId"`
			Plan   []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		_ = json.Unmarshal(params, &p)
		ev := agentpipe.Event{Type: agentpipe.EvPlan, Turn: p.TurnID}
		for _, s := range p.Plan {
			ev.Steps = append(ev.Steps, agentpipe.Step{Text: s.Step, Status: planStatus(s.Status)})
		}
		io.Emit(ev)
	case "thread/tokenUsage/updated":
		var p struct {
			TurnID     string `json:"turnId"`
			TokenUsage struct {
				Total struct {
					Input     int64 `json:"inputTokens"`
					Cached    int64 `json:"cachedInputTokens"`
					Output    int64 `json:"outputTokens"`
					Reasoning int64 `json:"reasoningOutputTokens"`
				} `json:"total"`
				Last struct {
					Total int64 `json:"totalTokens"`
				} `json:"last"`
				Window int64 `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		_ = json.Unmarshal(params, &p)
		t := p.TokenUsage
		d.usage = agentpipe.Usage{InputTokens: t.Total.Input, OutputTokens: t.Total.Output, CachedInputTokens: t.Total.Cached,
			ReasoningTokens: t.Total.Reasoning, ContextTokens: t.Last.Total, ContextWindow: t.Window}
		u := d.usage
		io.Emit(agentpipe.Event{Type: agentpipe.EvUsage, Turn: p.TurnID, Usage: &u})
	case "error":
		var p struct {
			TurnID    string `json:"turnId"`
			WillRetry bool   `json:"willRetry"`
			Error     struct {
				Message        string          `json:"message"`
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		}
		_ = json.Unmarshal(params, &p)
		e := &agentpipe.Error{Kind: classify(p.Error.Message, string(p.Error.CodexErrorInfo)), Message: p.Error.Message}
		if p.WillRetry {
			io.Emit(agentpipe.Event{Type: agentpipe.EvNotice, Turn: p.TurnID, Level: "warn", Error: e})
		} else {
			d.failure = e
		}
	case "serverRequest/resolved":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(params, &p)
		if w, ok := d.waiting[reqKey(p.RequestID)]; ok {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	case "warning", "configWarning", "deprecationNotice":
		var p struct {
			Message string `json:"message"`
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(params, &p)
		msg := p.Message
		if msg == "" {
			msg = p.Summary
		}
		if msg != "" {
			io.Emit(agentpipe.Event{Type: agentpipe.EvNotice, Level: "warn", Text: agentpipe.Head(msg, 2000)})
		}
	}
}

func planStatus(s string) string {
	switch s {
	case "inProgress", "in_progress":
		return "in_progress"
	case "completed":
		return "completed"
	}
	return "pending"
}

func (d *driver) endTurn(io agentpipe.IO, id, status string, terr *struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}) {
	if id == "" {
		id = d.turn
	}
	for _, k := range append([]string(nil), d.order...) {
		if w := d.waiting[k]; w != nil && w.turn == id {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	}
	d.closeTools(io, id)
	ev := agentpipe.Event{Type: agentpipe.EvTurn, Turn: id}
	switch status {
	case "completed":
		ev.Status = agentpipe.TurnCompleted
	case "interrupted":
		ev.Status = agentpipe.TurnInterrupted
	default:
		ev.Status = agentpipe.TurnFailed
		e := d.failure
		if terr != nil {
			e = &agentpipe.Error{Kind: classify(terr.Message, string(terr.CodexErrorInfo)), Message: terr.Message}
		}
		if e == nil {
			e = &agentpipe.Error{Kind: agentpipe.ErrFailed, Message: "the turn failed (status " + strconv.Quote(status) + ")"}
		}
		ev.Error = e
	}
	if d.usage.InputTokens > 0 || d.usage.OutputTokens > 0 {
		u := d.usage
		ev.Usage = &u
	}
	io.Emit(ev)
	if id == d.turn {
		d.turn, d.failure = "", nil
	}
	d.setState(io, agentpipe.StateReady)
}

// classify maps Codex's error info, else its words, to an error kind.
func classify(msg, info string) string {
	switch {
	case strings.Contains(info, "usageLimitExceeded"), strings.Contains(info, "rateLimitExceeded"), strings.Contains(info, "sessionBudgetExceeded"):
		return agentpipe.ErrQuota
	case strings.Contains(info, "unauthorized"):
		return agentpipe.ErrAuth
	}
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "usage limit"), strings.Contains(l, "rate limit"):
		return agentpipe.ErrQuota
	case strings.Contains(l, "401 unauthorized"), strings.Contains(l, "not logged in"), strings.Contains(l, "invalid_api_key"):
		return agentpipe.ErrAuth
	}
	return agentpipe.ErrFailed
}

type item struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Path string `json:"path"`
		URL  string `json:"url"`
	} `json:"content"`
	Summary          []string        `json:"summary"`
	Command          string          `json:"command"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Changes          []change        `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
}

type change struct {
	Path string `json:"path"`
	Kind struct {
		Type string `json:"type"`
	} `json:"kind"`
}

func toolStatus(s string, done bool) string {
	switch s {
	case "completed":
		return agentpipe.ItemCompleted
	case "failed":
		return agentpipe.ItemFailed
	case "declined":
		return agentpipe.ItemDeclined
	}
	if done {
		return agentpipe.ItemCompleted
	}
	return agentpipe.ItemRunning
}

func (d *driver) item(io agentpipe.IO, turn string, raw json.RawMessage, done bool) {
	var it item
	if json.Unmarshal(raw, &it) != nil || it.ID == "" {
		return
	}
	status := agentpipe.ItemStreaming
	if done {
		status = agentpipe.ItemCompleted
	}
	switch it.Type {
	case "userMessage":
		var parts []string
		for _, c := range it.Content {
			switch c.Type {
			case "text":
				parts = append(parts, c.Text)
			case "localImage":
				parts = append(parts, "[image "+c.Path+"]")
			case "image":
				parts = append(parts, "[image]")
			}
		}
		if !done {
			return // a user message is whole when it completes
		}
		io.Emit(agentpipe.Event{Type: agentpipe.EvMessage, Turn: turn, Item: it.ID, Role: "user", Status: agentpipe.ItemCompleted, Text: strings.Join(parts, "\n")})
	case "agentMessage":
		io.Emit(agentpipe.Event{Type: agentpipe.EvMessage, Turn: turn, Item: it.ID, Role: "assistant", Status: status, Text: it.Text})
	case "reasoning":
		if !done {
			return
		}
		text := strings.Join(it.Summary, "\n\n")
		if text == "" {
			return // nothing visible to show
		}
		io.Emit(agentpipe.Event{Type: agentpipe.EvReasoning, Turn: turn, Item: it.ID, Status: agentpipe.ItemCompleted, Text: text})
	case "commandExecution":
		ev := agentpipe.Event{Type: agentpipe.EvTool, Turn: turn, Item: it.ID, Kind: agentpipe.ToolCommand, Name: "shell",
			Title: agentpipe.OneLine(it.Command), Status: toolStatus(it.Status, false), ExitCode: it.ExitCode}
		if in, err := json.Marshal(map[string]string{"command": it.Command}); err == nil {
			ev.Input = agentpipe.BoundJSON(in)
		}
		if it.AggregatedOutput != nil {
			ev.Output = agentpipe.Clip(*it.AggregatedOutput, agentpipe.MaxOutput)
		}
		d.titles[it.ID] = ev.Title
		d.tool(io, ev)
	case "fileChange":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		ev := agentpipe.Event{Type: agentpipe.EvTool, Turn: turn, Item: it.ID, Kind: agentpipe.ToolFileChange, Name: "apply_patch",
			Title: agentpipe.OneLine(strings.Join(paths, ", ")), Status: toolStatus(it.Status, false)}
		if in, err := json.Marshal(it.Changes); err == nil {
			ev.Input = agentpipe.BoundJSON(in)
		}
		d.titles[it.ID] = ev.Title
		d.tool(io, ev)
	case "mcpToolCall":
		ev := agentpipe.Event{Type: agentpipe.EvTool, Turn: turn, Item: it.ID, Kind: agentpipe.ToolMCP, Name: it.Server + "." + it.Tool,
			Title: it.Server + "." + it.Tool, Status: toolStatus(it.Status, false), Input: agentpipe.BoundJSON(it.Arguments)}
		if it.Error != nil {
			ev.Output = agentpipe.Clip(it.Error.Message, agentpipe.MaxOutput)
		} else if len(it.Result) > 0 && string(it.Result) != "null" {
			ev.Output = agentpipe.Clip(mcpText(it.Result), agentpipe.MaxOutput)
		}
		d.tool(io, ev)
	case "webSearch":
		d.tool(io, agentpipe.Event{Type: agentpipe.EvTool, Turn: turn, Item: it.ID, Kind: agentpipe.ToolWeb, Name: "web_search",
			Title: agentpipe.OneLine(it.Query), Status: toolStatus(it.Status, done)})
	case "plan", "contextCompaction", "hookPrompt":
		// Shown through other events (turn/plan/updated) or not at all.
	default:
		d.tool(io, agentpipe.Event{Type: agentpipe.EvTool, Turn: turn, Item: it.ID, Kind: agentpipe.ToolOther, Name: it.Type,
			Title: it.Type, Status: toolStatus(it.Status, done)})
	}
}

func mcpText(raw json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		var parts []string
		for _, c := range r.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func reqKey(id json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return "r" + n.String()
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return "r" + s
	}
	return "r" + string(id)
}

var allDecisions = []string{agentpipe.DecisionAccept, agentpipe.DecisionAcceptSession, agentpipe.DecisionDecline, agentpipe.DecisionCancel}

func (d *driver) serverRequest(io agentpipe.IO, f frame) {
	var p struct {
		TurnID    string `json:"turnId"`
		ItemID    string `json:"itemId"`
		Command   string `json:"command"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(f.Params, &p)
	w := &pending{id: f.ID, method: f.Method, params: f.Params, turn: p.TurnID, item: p.ItemID}
	switch f.Method {
	case "item/commandExecution/requestApproval":
		w.kind, w.options = agentpipe.ApprovalCommand, allDecisions
		w.title = p.Command
		if w.title == "" {
			w.title = d.titles[p.ItemID]
		}
	case "execCommandApproval":
		var lp struct {
			Command []string `json:"command"`
		}
		_ = json.Unmarshal(f.Params, &lp)
		w.kind, w.options, w.title = agentpipe.ApprovalCommand, allDecisions, strings.Join(lp.Command, " ")
	case "item/fileChange/requestApproval", "applyPatchApproval":
		w.kind, w.options, w.title = agentpipe.ApprovalFileChange, allDecisions, d.titles[p.ItemID]
	case "item/permissions/requestApproval":
		w.kind, w.options, w.title = agentpipe.ApprovalPermissions, []string{agentpipe.DecisionAccept, agentpipe.DecisionAcceptSession, agentpipe.DecisionDecline}, "more permissions"
	case "item/tool/requestUserInput":
		w.kind, w.options = agentpipe.ApprovalQuestion, []string{agentpipe.DecisionAccept, agentpipe.DecisionDecline}
		var qs []string
		for _, q := range p.Questions {
			w.question = append(w.question, q.ID)
			qs = append(qs, q.Question)
		}
		if len(qs) > 0 {
			w.title = qs[0]
		}
		p.Reason = strings.Join(qs, "\n")
	case "mcpServer/elicitation/request":
		w.kind, w.options, w.title = agentpipe.ApprovalTool, []string{agentpipe.DecisionAccept, agentpipe.DecisionDecline, agentpipe.DecisionCancel}, p.Message
	default:
		// Requests zm does not serve (dynamic tools, token refresh, attestation).
		io.Send(map[string]any{"id": f.ID, "error": map[string]any{"code": -32601, "message": "zm does not serve " + f.Method}})
		return
	}
	if w.turn == "" {
		w.turn = d.turn
	}
	key := reqKey(f.ID)
	if d.st.Approvals == "never" {
		// Nobody answers: decline at once, so a headless session never waits.
		d.waiting[key], d.order = w, append(d.order, key)
		d.reply(io, w, agentpipe.DecisionDecline, "")
		d.resolve(io, w, agentpipe.ItemAnswered, agentpipe.DecisionDecline)
		return
	}
	d.waiting[key], d.order = w, append(d.order, key)
	io.Emit(agentpipe.Event{Type: agentpipe.EvApproval, Turn: w.turn, Item: w.item, Request: key, Kind: w.kind, Status: agentpipe.ItemPending,
		Title: agentpipe.OneLine(w.title), Detail: agentpipe.Head(p.Reason, 4000), Options: w.options})
	d.setState(io, agentpipe.StateWaiting)
}

func (d *driver) resolve(io agentpipe.IO, w *pending, status, decision string) {
	key := reqKey(w.id)
	if _, ok := d.waiting[key]; !ok {
		return
	}
	delete(d.waiting, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	if d.st.Approvals != "never" || status != agentpipe.ItemAnswered {
		io.Emit(agentpipe.Event{Type: agentpipe.EvApproval, Turn: w.turn, Item: w.item, Request: key, Kind: w.kind, Status: status, Decision: decision})
	}
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
	d.reply(io, w, op.Decision, op.Answer)
	io.Emit(agentpipe.OpResult(op.ID, nil))
	d.resolve(io, w, agentpipe.ItemAnswered, op.Decision)
}

// reply answers a server request in its own method's terms.
func (d *driver) reply(io agentpipe.IO, w *pending, decision, answer string) {
	var result any
	switch w.method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		v := map[string]string{agentpipe.DecisionAccept: "accept", agentpipe.DecisionAcceptSession: "acceptForSession",
			agentpipe.DecisionDecline: "decline", agentpipe.DecisionCancel: "cancel"}[decision]
		result = map[string]any{"decision": v}
	case "execCommandApproval", "applyPatchApproval":
		var v any
		switch decision {
		case agentpipe.DecisionAccept:
			v = "approved"
		case agentpipe.DecisionAcceptSession:
			v = "approved_for_session"
		case agentpipe.DecisionCancel:
			v = "abort"
		default:
			reason := answer
			if reason == "" {
				reason = "declined"
			}
			v = map[string]any{"denied": map[string]any{"rejection": reason}}
		}
		result = map[string]any{"decision": v}
	case "item/permissions/requestApproval":
		var p struct {
			Permissions json.RawMessage `json:"permissions"`
		}
		_ = json.Unmarshal(w.params, &p)
		switch decision {
		case agentpipe.DecisionAccept:
			result = map[string]any{"permissions": p.Permissions, "scope": "turn"}
		case agentpipe.DecisionAcceptSession:
			result = map[string]any{"permissions": p.Permissions, "scope": "session"}
		default:
			result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		}
	case "item/tool/requestUserInput":
		answers := map[string]any{}
		if decision == agentpipe.DecisionAccept {
			for i, q := range w.question {
				a := []string{}
				if i == 0 && answer != "" {
					a = []string{answer}
				}
				answers[q] = map[string]any{"answers": a}
			}
		}
		result = map[string]any{"answers": answers}
	case "mcpServer/elicitation/request":
		v := map[string]string{agentpipe.DecisionAccept: "accept", agentpipe.DecisionDecline: "decline", agentpipe.DecisionCancel: "cancel"}[decision]
		if v == "" {
			v = "decline"
		}
		result = map[string]any{"action": v}
	}
	io.Send(map[string]any{"id": w.id, "result": result})
}

func (d *driver) Exit(io agentpipe.IO, code int, stopped bool) {
	for _, op := range d.queued {
		io.Emit(agentpipe.Refuse(op.ID, agentpipe.ErrInfra, "the runtime exited"))
	}
	for _, k := range append([]string(nil), d.order...) {
		if w := d.waiting[k]; w != nil {
			d.resolve(io, w, agentpipe.ItemCancelled, "")
		}
	}
	d.closeTools(io, "")
	if d.turn != "" {
		ev := agentpipe.Event{Type: agentpipe.EvTurn, Turn: d.turn, Status: agentpipe.TurnCancelled}
		if !stopped {
			ev.Status = agentpipe.TurnFailed
			ev.Error = &agentpipe.Error{Kind: agentpipe.ErrInfra, Message: fmt.Sprintf("codex exited (code %d) during the turn", code)}
		}
		io.Emit(ev)
		d.turn = ""
	}
	if code == 0 || stopped {
		io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped, RuntimeID: d.thread})
		return
	}
	io.Emit(agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateError, RuntimeID: d.thread,
		Error: &agentpipe.Error{Kind: agentpipe.ErrInfra, Message: fmt.Sprintf("codex exited with code %d", code)}})
}
