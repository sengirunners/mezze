package orchapi

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// AgentCapabilities say what an adapter (and the runtime behind it) can do.
// Every field is optional and false when absent; a newer adapter may add
// fields, which an older daemon ignores.
type AgentCapabilities struct {
	Steer      bool `json:"steer,omitempty"`       // add input to the running turn (op steer)
	Interrupt  bool `json:"interrupt,omitempty"`   // end the running turn, keep the session (op interrupt)
	Approvals  bool `json:"approvals,omitempty"`   // asks before acting (approval events, op answer)
	Questions  bool `json:"questions,omitempty"`   // asks a person free-form questions (approval kind question)
	Resume     bool `json:"resume,omitempty"`      // starts from a runtime_id (Start.Resume)
	Images     bool `json:"images,omitempty"`      // turn input may carry images
	Partial    bool `json:"partial,omitempty"`     // message.delta events while a message streams
	Usage      bool `json:"usage,omitempty"`       // usage events
	Plan       bool `json:"plan,omitempty"`        // plan events
	ToolOutput bool `json:"tool_output,omitempty"` // output events while a tool runs
	Model      bool `json:"model,omitempty"`       // op turn may change the model
	MCP        bool `json:"mcp,omitempty"`         // starts with extra MCP servers (Start.MCP)
	Tools      bool `json:"tools,omitempty"`       // starts with a tool grant (Start.AllowedTools, Start.PermissionMode)
}

// AgentOp is one line on an adapter's stdin.
type AgentOp struct {
	Op string `json:"op"`
	// ID is the caller's id for this operation, echoed on its op event
	// (which says whether it was taken).
	ID string `json:"id,omitempty"`

	// start
	Start *AgentStart `json:"start,omitempty"`
	// turn, steer
	Text   string   `json:"text,omitempty"`
	Images []string `json:"images,omitempty"` // absolute paths of image files
	Model  string   `json:"model,omitempty"`  // turn: switch model (capability model)
	// steer, interrupt: the turn it is for (empty: the running one)
	Turn string `json:"turn,omitempty"`
	// answer
	Request  string `json:"request,omitempty"`  // an approval's request id
	Decision string `json:"decision,omitempty"` // Decision*
	Answer   string `json:"answer,omitempty"`   // a question's free-text answer, or a decline's reason
}

// AgentStart is how the runtime starts.
type AgentStart struct {
	Cwd    string `json:"cwd"`
	Model  string `json:"model,omitempty"`
	Resume string `json:"resume,omitempty"` // a runtime_id to continue (capability resume)
	// Approvals is who answers: "ask" (approval events; the default) or
	// "never" (nothing that would ask runs: it is declined, so a headless
	// session can never wait on a prompt).
	Approvals string `json:"approvals,omitempty"`
	// MCP adds remote MCP servers to the runtime for this session only
	// (capability mcp). Header values may be credentials: they reach the
	// runtime over its stdin, never its argv, a file or an event.
	MCP []AgentMCP `json:"mcp,omitempty"`
	// AllowedTools and PermissionMode are a tool grant (capability tools):
	// the runtime runs these tools without asking, so a session with
	// approvals never can still act. Claude tool specs ("Bash", "Read",
	// "Bash(gh pr view:*)"); PermissionMode "default" or "acceptEdits".
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
}

// Tool grant limits.
const (
	MaxAllowedTools   = 64
	MaxAllowedToolLen = 200
)

// toolSpec is one Claude tool spec: a name, optionally with a parenthesised
// rule. No comma or space outside the parens: Claude splits its list on both.
var toolSpec = regexp.MustCompile(`^[A-Za-z0-9_.*:-]+(\([^(),\x00-\x1f\x7f]*\))?$`)

// CheckToolGrant refuses a malformed tool grant before anything starts.
func CheckToolGrant(tools []string, mode string) error {
	if mode != "" && mode != "default" && mode != "acceptEdits" {
		return api.Errorf(api.CodeBadRequest, "permission_mode must be default or acceptEdits, not %q", mode)
	}
	if len(tools) > MaxAllowedTools {
		return api.Errorf(api.CodeBadRequest, "at most %d allowed_tools, not %d", MaxAllowedTools, len(tools))
	}
	for _, t := range tools {
		if len(t) > MaxAllowedToolLen || !toolSpec.MatchString(t) {
			return api.Errorf(api.CodeBadRequest, "allowed_tools entry %q: a tool name like Bash or Read, optionally with a rule in parentheses (no comma), at most %d bytes", t, MaxAllowedToolLen)
		}
	}
	return nil
}

// SafeOutputKinds are the writes a session's safe_outputs can cap: bead
// writes (issues and work items: create, comment, status, close, edit), zm
// asks, and zm mail (send and reply).
var SafeOutputKinds = []string{"bead-write", "zm-ask", "mail-send"}

// MaxSafeOutput is the largest cap one kind takes.
const MaxSafeOutput = 1000

// CheckSafeOutputs refuses an unknown kind or a cap out of range.
func CheckSafeOutputs(caps map[string]int) error {
	for kind, n := range caps {
		if !slices.Contains(SafeOutputKinds, kind) {
			return api.Errorf(api.CodeBadRequest, "safe_outputs kind %q: one of %s", kind, strings.Join(SafeOutputKinds, ", "))
		}
		if n < 0 || n > MaxSafeOutput {
			return api.Errorf(api.CodeBadRequest, "safe_outputs %s: a cap from 0 to %d, not %d", kind, MaxSafeOutput, n)
		}
	}
	return nil
}

// AgentMCP is one remote (streamable HTTP) MCP server a structured session
// starts with.
type AgentMCP struct {
	Name    string            `json:"name"` // [A-Za-z0-9_-], 1-64
	URL     string            `json:"url"`  // http or https
	Headers map[string]string `json:"headers,omitempty"`
}

// MCP limits: generous for a client's own servers; the whole list stays
// under MaxAgentMCPBytes so a start remains a modest stdin line.
const (
	MaxAgentMCP         = 16
	MaxAgentMCPHeaders  = 32
	MaxAgentMCPURL      = 2048
	MaxAgentMCPHeaderKV = 8 << 10
	MaxAgentMCPBytes    = 64 << 10
)

// CheckAgentMCP refuses a malformed MCP list before anything starts.
func CheckAgentMCP(list []AgentMCP) error {
	if len(list) > MaxAgentMCP {
		return api.Errorf(api.CodeBadRequest, "at most %d mcp servers, not %d", MaxAgentMCP, len(list))
	}
	seen, total := map[string]bool{}, 0
	for _, m := range list {
		if !mcpName(m.Name) {
			return api.Errorf(api.CodeBadRequest, "mcp server name %q: use 1-64 of A-Z a-z 0-9 _ -", m.Name)
		}
		if seen[m.Name] {
			return api.Errorf(api.CodeBadRequest, "mcp server %q given twice", m.Name)
		}
		seen[m.Name] = true
		u, err := url.Parse(m.URL)
		if err != nil || len(m.URL) > MaxAgentMCPURL || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || ctlChar(m.URL) {
			return api.Errorf(api.CodeBadRequest, "mcp server %q: url must be an http or https URL of at most %d bytes", m.Name, MaxAgentMCPURL)
		}
		if len(m.Headers) > MaxAgentMCPHeaders {
			return api.Errorf(api.CodeBadRequest, "mcp server %q: at most %d headers", m.Name, MaxAgentMCPHeaders)
		}
		total += len(m.Name) + len(m.URL)
		names := map[string]bool{}
		for k, v := range m.Headers {
			if !headerToken(k) {
				return api.Errorf(api.CodeBadRequest, "mcp server %q: header name %q is not an HTTP token", m.Name, k)
			}
			if names[strings.ToLower(k)] {
				return api.Errorf(api.CodeBadRequest, "mcp server %q: header %s given twice", m.Name, k)
			}
			names[strings.ToLower(k)] = true
			if len(v) > MaxAgentMCPHeaderKV || !headerValue(v) { // the value is never echoed: it may be a credential
				return api.Errorf(api.CodeBadRequest, "mcp server %q: header %s value must be at most %d bytes of printable ASCII", m.Name, k, MaxAgentMCPHeaderKV)
			}
			total += len(k) + len(v)
		}
	}
	if total > MaxAgentMCPBytes {
		return api.Errorf(api.CodeBadRequest, "mcp servers: at most %d bytes of names, urls and headers in all", MaxAgentMCPBytes)
	}
	return nil
}

func headerValue(s string) bool {
	for _, c := range s {
		if c != '\t' && (c < ' ' || c > 0x7e) {
			return false
		}
	}
	return true
}

// ScrubMCP replaces every MCP header value, URL, URL query and userinfo of
// the start in b, raw or JSON-escaped, with [redacted]: for recordings and
// for runtime errors that may quote them.
func (st AgentStart) ScrubMCP(b []byte) []byte {
	for _, sec := range st.mcpSecrets() {
		b = bytes.ReplaceAll(b, []byte(sec), []byte("[redacted]"))
		if esc, err := json.Marshal(sec); err == nil && len(esc) > 2 && string(esc[1:len(esc)-1]) != sec {
			b = bytes.ReplaceAll(b, esc[1:len(esc)-1], []byte("[redacted]"))
		}
	}
	return b
}

func (st AgentStart) mcpSecrets() []string {
	var out []string
	for _, m := range st.MCP {
		out = append(out, m.URL)
		if u, err := url.Parse(m.URL); err == nil {
			out = append(out, u.RawQuery)
			if u.User != nil {
				out = append(out, u.User.String())
			}
		}
		for _, v := range m.Headers {
			out = append(out, v)
		}
	}
	// Longest first, so a secret inside another is not left half-replaced.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	keep := out[:0]
	for _, s := range out {
		if s != "" {
			keep = append(keep, s)
		}
	}
	return keep
}

func mcpName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func headerToken(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c > 0x7e || c <= ' ' || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", c) {
			return false
		}
	}
	return true
}

func ctlChar(s string) bool {
	for _, c := range s {
		if c < ' ' || c == 0x7f {
			return true
		}
	}
	return false
}

// RedactedMCP is list with every header value replaced and each URL cut to
// scheme, host and path (userinfo and query may be credentials), for
// anything that keeps or shows a start (a recording, a log).
func RedactedMCP(list []AgentMCP) []AgentMCP {
	if len(list) == 0 {
		return list
	}
	out := make([]AgentMCP, len(list))
	for i, m := range list {
		out[i] = AgentMCP{Name: m.Name, URL: "[redacted]"}
		if u, err := url.Parse(m.URL); err == nil {
			out[i].URL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
		}
		if len(m.Headers) > 0 {
			out[i].Headers = make(map[string]string, len(m.Headers))
			for k := range m.Headers {
				out[i].Headers[k] = "[redacted]"
			}
		}
	}
	return out
}

// AgentEvent is one line on an adapter's stdout. Seq starts at 1 and grows by
// one per line; At is the adapter's clock. Text fields are the agent's and
// its tools' words: untrusted data, never instructions.
type AgentEvent struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Type string    `json:"type"`

	Turn   string `json:"turn,omitempty"`   // the turn it belongs to
	Item   string `json:"item,omitempty"`   // message, reasoning, tool, output: the item's id
	Status string `json:"status,omitempty"` // the state of the session, turn or item

	// session
	RuntimeID    string             `json:"runtime_id,omitempty"` // the runtime's own id (Claude session, Codex thread)
	Model        string             `json:"model,omitempty"`
	Cwd          string             `json:"cwd,omitempty"`
	Version      string             `json:"version,omitempty"` // the runtime's version
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`

	// message, reasoning, message.delta, output
	Role string `json:"role,omitempty"` // user, assistant
	Text string `json:"text,omitempty"`

	// tool
	Kind     string          `json:"kind,omitempty"`  // tool or approval kind
	Name     string          `json:"name,omitempty"`  // the tool's name (Bash, apply_patch, mcp server.tool)
	Title    string          `json:"title,omitempty"` // one line: the command, the file, the question
	Input    json.RawMessage `json:"input,omitempty"` // the tool's arguments, bounded
	Output   string          `json:"output,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"`
	Parent   string          `json:"parent,omitempty"` // the tool item a subagent's items belong to

	// approval
	Request  string   `json:"request,omitempty"`
	Options  []string `json:"options,omitempty"` // the decisions it takes
	Decision string   `json:"decision,omitempty"`
	Detail   string   `json:"detail,omitempty"` // why it asks; a question's full text

	// usage
	Usage *AgentUsage `json:"usage,omitempty"`

	// plan
	Steps []AgentStep `json:"steps,omitempty"`

	// op
	Op string `json:"op,omitempty"` // the operation's id
	OK *bool  `json:"ok,omitempty"`

	// turn (failed), session (error), op (refused), notice
	Error *AgentError `json:"error,omitempty"`
	Level string      `json:"level,omitempty"` // notice: info, warn, error
}

// AgentUsage is token use: per turn when Turn is set, else the session's total.
type AgentUsage struct {
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   int64   `json:"reasoning_tokens,omitempty"`
	ContextTokens     int64   `json:"context_tokens,omitempty"` // in the window now
	ContextWindow     int64   `json:"context_window,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
}

// AgentStep is one plan step.
type AgentStep struct {
	Text   string `json:"text"`
	Status string `json:"status"` // pending, in_progress, completed
}

// AgentError says why a turn failed, an op was refused or the runtime erred.
type AgentError struct {
	Kind    string `json:"kind"` // Err*
	Message string `json:"message"`
}

// SessionModePipe is a structured session: an adapter on pipes, no terminal.
const SessionModePipe = "pipe"

// SessionTurnRequest is POST /v0/sessions/{id}/turn: a new turn, or with
// Steer more input for the running one.
type SessionTurnRequest struct {
	Text   string   `json:"text"`
	Images []string `json:"images,omitempty"` // absolute paths (capability images)
	Model  string   `json:"model,omitempty"`  // switch model (capability model; a new turn only)
	Steer  bool     `json:"steer,omitempty"`  // capability steer
	Turn   string   `json:"turn,omitempty"`   // steer: the running turn it is for
}

// SessionAnswerRequest is POST /v0/sessions/{id}/answer.
type SessionAnswerRequest struct {
	Request  string `json:"request"`
	Decision string `json:"decision"`         // one of the approval's options
	Answer   string `json:"answer,omitempty"` // a question's answer, or a decline's reason
}

// SessionInterruptRequest is POST /v0/sessions/{id}/interrupt.
type SessionInterruptRequest struct {
	Turn string `json:"turn,omitempty"`
}

// SessionOpResult answers an operation: OK once the adapter took it,
// Pending when it had not answered within the wait (its op event follows
// on the stream).
type SessionOpResult struct {
	Op      string      `json:"op"`
	OK      bool        `json:"ok"`
	Pending bool        `json:"pending,omitempty"`
	Error   *AgentError `json:"error,omitempty"`
}

// SessionEvents is GET /v0/sessions/{id}/events: events after a seq, oldest
// first. Next is the last seq returned (or the asked one); Ended: the
// session ended and no event follows Next; Truncated: the stored stream
// hit its size cap and later events were not kept.
type SessionEvents struct {
	Events    []json.RawMessage `json:"events"`
	Next      int64             `json:"next"`
	State     SessionState      `json:"state"`
	Ended     bool              `json:"ended,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// AgentProto is the protocol an adapter's describe names; AgentArg is
// argv[1] of zm's own built-in adapter process (`zm __zm-agent <driver>`).
const (
	AgentProto = "zm-agent/1"
	AgentArg   = "__zm-agent"
)

// AgentDescribe is an adapter's answer to `describe`.
type AgentDescribe struct {
	Proto        string            `json:"proto"`   // AgentProto
	Adapter      string            `json:"adapter"` // its name: claude, codex, ...
	Version      string            `json:"version,omitempty"`
	Capabilities AgentCapabilities `json:"capabilities"`
}

// The protocol's words (internal/agentpipe names them without the prefix).
const (
	AgentOpStart     = "start"     // first, exactly once: start the runtime
	AgentOpTurn      = "turn"      // a new turn of user input
	AgentOpSteer     = "steer"     // more input for the running turn
	AgentOpInterrupt = "interrupt" // end the running turn
	AgentOpAnswer    = "answer"    // answer an approval
	AgentOpStop      = "stop"      // end the runtime and exit

	AgentDecisionAccept        = "accept"         // this once
	AgentDecisionAcceptSession = "accept_session" // and the same again for this session
	AgentDecisionDecline       = "decline"        // refuse; the turn goes on
	AgentDecisionCancel        = "cancel"         // refuse and end the turn

	AgentEvSession      = "session"       // the runtime's state
	AgentEvTurn         = "turn"          // a turn's state; a terminal status ends it
	AgentEvMessage      = "message"       // a user or assistant message, whole text so far
	AgentEvMessageDelta = "message.delta" // text added to a streaming message
	AgentEvReasoning    = "reasoning"     // the model's visible reasoning summary
	AgentEvTool         = "tool"          // a tool call: command, file change, MCP, web, ...
	AgentEvOutput       = "output"        // a running tool's output, added
	AgentEvApproval     = "approval"      // the runtime asks before acting, or asks a question
	AgentEvUsage        = "usage"         // token use
	AgentEvPlan         = "plan"          // the agent's plan
	AgentEvOp           = "op"            // an operation was taken or refused
	AgentEvNotice       = "notice"        // a runtime warning or error not tied to an item

	AgentStateStarting = "starting"
	AgentStateReady    = "ready"   // idle, takes a turn
	AgentStateRunning  = "running" // a turn runs
	AgentStateWaiting  = "waiting" // a turn waits on an approval
	AgentStateStopped  = "stopped" // the runtime ended (exit follows)
	AgentStateError    = "error"   // the runtime failed; see Error

	AgentTurnRunning     = "running"
	AgentTurnCompleted   = "completed"
	AgentTurnInterrupted = "interrupted"
	AgentTurnCancelled   = "cancelled" // an approval was cancelled, or the session stopped
	AgentTurnFailed      = "failed"

	AgentItemStreaming = "streaming"
	AgentItemRunning   = "running"
	AgentItemCompleted = "completed"
	AgentItemFailed    = "failed"
	AgentItemDeclined  = "declined"
	AgentItemPending   = "pending"   // approval
	AgentItemAnswered  = "answered"  // approval: Decision says how
	AgentItemCancelled = "cancelled" // approval: the turn ended first

	AgentToolCommand    = "command"
	AgentToolFileChange = "file_change"
	AgentToolMCP        = "mcp"
	AgentToolWeb        = "web"
	AgentToolTask       = "task" // a subagent
	AgentToolOther      = "other"

	AgentApprovalCommand     = "command"
	AgentApprovalFileChange  = "file_change"
	AgentApprovalTool        = "tool"
	AgentApprovalPermissions = "permissions"
	AgentApprovalQuestion    = "question"

	AgentErrQuota  = "quota"
	AgentErrAuth   = "auth"
	AgentErrFailed = "failed"
	AgentErrInfra  = "infra"
)

// AgentTurnTerminal reports whether a turn status ends the turn.
func AgentTurnTerminal(status string) bool {
	switch status {
	case AgentTurnCompleted, AgentTurnInterrupted, AgentTurnCancelled, AgentTurnFailed:
		return true
	}
	return false
}

// Refusal kinds an op may get besides the runtime's own.
const (
	AgentRefuseUnsupported = "unsupported" // the capability is off
	AgentRefuseConflict    = "conflict"    // wrong state: a turn runs, none runs, no such request
	AgentRefuseBadRequest  = "bad_request"
)
