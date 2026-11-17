// Package agentpipe is the structured-session adapter brick (SPEC 8
// "Structured sessions"): a
// process protocol, zm-agent/1, between zm and one agent runtime driven over
// pipes instead of a terminal. An adapter reads operations (one JSON object
// per line) on stdin and writes normalized session events (one per line) on
// stdout; it starts the agent's own CLI as its child and translates.
//
// Drivers built in: claude (`claude --input-format stream-json
// --output-format stream-json`) and codex (`codex app-server`), run as
// `zm __zm-agent <driver> -- <cli argv>`. A third party supplies one as an
// executable `zm-agent-<name>` speaking the same protocol (ARCHITECTURE rule
// 3); `describe` comes first. Conformance: agentpipe/conformance replays
// recorded real transcripts (cmd/zm-agent-conformance).
//
// The daemon never checks an agent's name to decide what a session can do:
// it reads the adapter's Capabilities.
package agentpipe

import (
	"github.com/sengirunners/mezze/internal/orchapi"
)

// Proto is the protocol name an adapter's describe answers with.
const Proto = orchapi.AgentProto

// Capabilities say what an adapter can do (api.AgentCapabilities).
type Capabilities = orchapi.AgentCapabilities

// Describe is an adapter's answer to `describe`.
type Describe = orchapi.AgentDescribe

// Operation kinds: what zm asks an adapter to do.
const (
	OpStart     = orchapi.AgentOpStart
	OpTurn      = orchapi.AgentOpTurn
	OpSteer     = orchapi.AgentOpSteer
	OpInterrupt = orchapi.AgentOpInterrupt
	OpAnswer    = orchapi.AgentOpAnswer
	OpStop      = orchapi.AgentOpStop
)

// Op is one line on an adapter's stdin; Start how the runtime starts.
type (
	Op    = orchapi.AgentOp
	Start = orchapi.AgentStart
	MCP   = orchapi.AgentMCP
)

// Decisions for an approval.
const (
	DecisionAccept        = orchapi.AgentDecisionAccept
	DecisionAcceptSession = orchapi.AgentDecisionAcceptSession
	DecisionDecline       = orchapi.AgentDecisionDecline
	DecisionCancel        = orchapi.AgentDecisionCancel
)

// Event types: what an adapter reports. Every event is a whole state of the
// thing it names (an upsert keyed by its id), except the *.delta events and
// output, which add to one.
const (
	EvSession      = orchapi.AgentEvSession
	EvTurn         = orchapi.AgentEvTurn
	EvMessage      = orchapi.AgentEvMessage
	EvMessageDelta = orchapi.AgentEvMessageDelta
	EvReasoning    = orchapi.AgentEvReasoning
	EvTool         = orchapi.AgentEvTool
	EvOutput       = orchapi.AgentEvOutput
	EvApproval     = orchapi.AgentEvApproval
	EvUsage        = orchapi.AgentEvUsage
	EvPlan         = orchapi.AgentEvPlan
	EvOp           = orchapi.AgentEvOp
	EvNotice       = orchapi.AgentEvNotice
)

// Session states (EvSession).
const (
	StateStarting = orchapi.AgentStateStarting
	StateReady    = orchapi.AgentStateReady
	StateRunning  = orchapi.AgentStateRunning
	StateWaiting  = orchapi.AgentStateWaiting
	StateStopped  = orchapi.AgentStateStopped
	StateError    = orchapi.AgentStateError
)

// Turn statuses (EvTurn). Completed, interrupted, cancelled and failed are
// terminal: exactly one per turn, and the last event of that turn.
const (
	TurnRunning     = orchapi.AgentTurnRunning
	TurnCompleted   = orchapi.AgentTurnCompleted
	TurnInterrupted = orchapi.AgentTurnInterrupted
	TurnCancelled   = orchapi.AgentTurnCancelled
	TurnFailed      = orchapi.AgentTurnFailed
)

// Item statuses (message, reasoning, tool, approval).
const (
	ItemStreaming = orchapi.AgentItemStreaming
	ItemRunning   = orchapi.AgentItemRunning
	ItemCompleted = orchapi.AgentItemCompleted
	ItemFailed    = orchapi.AgentItemFailed
	ItemDeclined  = orchapi.AgentItemDeclined
	ItemPending   = orchapi.AgentItemPending
	ItemAnswered  = orchapi.AgentItemAnswered
	ItemCancelled = orchapi.AgentItemCancelled
)

// Tool kinds.
const (
	ToolCommand    = orchapi.AgentToolCommand
	ToolFileChange = orchapi.AgentToolFileChange
	ToolMCP        = orchapi.AgentToolMCP
	ToolWeb        = orchapi.AgentToolWeb
	ToolTask       = orchapi.AgentToolTask
	ToolOther      = orchapi.AgentToolOther
)

// Approval kinds.
const (
	ApprovalCommand     = orchapi.AgentApprovalCommand
	ApprovalFileChange  = orchapi.AgentApprovalFileChange
	ApprovalTool        = orchapi.AgentApprovalTool
	ApprovalPermissions = orchapi.AgentApprovalPermissions
	ApprovalQuestion    = orchapi.AgentApprovalQuestion
)

// Error kinds, the same words as api.OutcomeKind where they overlap.
const (
	ErrQuota  = orchapi.AgentErrQuota
	ErrAuth   = orchapi.AgentErrAuth
	ErrFailed = orchapi.AgentErrFailed
	ErrInfra  = orchapi.AgentErrInfra
)

// Event is one line on an adapter's stdout; Usage, Step and Error its parts.
type (
	Event = orchapi.AgentEvent
	Usage = orchapi.AgentUsage
	Step  = orchapi.AgentStep
	Error = orchapi.AgentError
)

// Terminal reports whether a turn status ends the turn.
func Terminal(status string) bool { return orchapi.AgentTurnTerminal(status) }
