package orchapi

import (
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// SessionState is a session's state.
type SessionState string

const (
	StateRunning    SessionState = "running"     // the agent is working
	StateNeedsHuman SessionState = "needs_human" // blocked on a person (a question, a permission prompt, needs=human mail it sent)
	StateIdle       SessionState = "idle"        // waiting at its prompt
	StateEnded      SessionState = "ended"       // final; see Outcome
)

// OutcomeKind is a typed result. Anything not positively recognised as ok is
// never reported as ok.
type OutcomeKind string

const (
	OutcomeOK     OutcomeKind = "ok"     // exited 0 (or stopped by a human) and no detector fired
	OutcomeQuota  OutcomeKind = "quota"  // usage / rate limit
	OutcomeOOM    OutcomeKind = "oom"    // killed for memory
	OutcomeAuth   OutcomeKind = "auth"   // not logged in / bad credentials
	OutcomeInfra  OutcomeKind = "infra"  // zm or the machine failed it (daemon restarted, spawn failed)
	OutcomeKilled OutcomeKind = "killed" // stopped by a signal (zm session stop, kill)
	OutcomeFailed OutcomeKind = "failed" // anything else that is not ok
)

// Outcome is how a session (or task) ended.
type Outcome struct {
	Kind     OutcomeKind `json:"kind"`
	ExitCode *int        `json:"exit_code,omitempty"`
	Signal   string      `json:"signal,omitempty"`
	// Evidence is the matched output line (or the reason) behind Kind.
	Evidence string `json:"evidence,omitempty"`
	// Confidence is "high" (exit status, explicit runtime signal) or "low" (text match).
	Confidence string `json:"confidence,omitempty"`
	Note       string `json:"note,omitempty"` // e.g. "daemon restarted"
}

// Session is one agent in one PTY (spawned by zm, Managed=true) or one agent
// registered by its hooks (running in someone's own terminal, Managed=false).
// Every session has a project ("none" = visible only to the machine's owner).
type Session struct {
	Address string `json:"address"` // <machine>/<session8>
	ID      string `json:"id"`      // session8
	// Name is an editable alias (ValidSessionName), unique on its machine
	// among live sessions, and resolved only while the session is live
	// (SPEC 8 "Names"). Mail and commands store the address it resolves to.
	Name    string `json:"name,omitempty"`
	Machine string `json:"machine"`
	Project string `json:"project"` // ULID or "none"
	Area    string `json:"area,omitempty"`
	// Launcher is the identity that started or registered it; a session never
	// acts above its launcher's grants.
	Launcher string `json:"launcher,omitempty"`
	// Parent is the address of the session that started this one, when zm
	// knows it (SPEC 8 "Parents"). It is set on reads, and left out when the
	// reader cannot see that session; the parent may have ended.
	Parent string `json:"parent,omitempty"`
	// What ran is what was asked: these are recorded at spawn/register, never guessed later.
	Agent    string   `json:"agent"`             // claude, codex, opencode, gemini, shell, or a custom name
	Command  []string `json:"command,omitempty"` // argv actually executed
	Version  string   `json:"version,omitempty"` // runtime version (e.g. `claude --version`), when known
	Model    string   `json:"model,omitempty"`   // model asked for, when known
	Cwd      string   `json:"cwd,omitempty"`
	Worktree string   `json:"worktree,omitempty"` // git worktree zm created for it (never deleted by zm)
	Branch   string   `json:"branch,omitempty"`   // the worktree's branch (zm/<session8>)
	Scratch  string   `json:"scratch,omitempty"`  // its scratch dir
	// Sandbox is the srt settings file the agent runs under (zm run --sandbox); "" = not sandboxed.
	Sandbox string       `json:"sandbox,omitempty"`
	Title   string       `json:"title,omitempty"`
	State   SessionState `json:"state"`
	Note    string       `json:"note,omitempty"` // why it is in its state ("asked: deploy to prod?")
	// Work is the work item it holds a lease on, else the one it last held
	// (kept after the release, so a session still says what it worked on).
	Work string `json:"work,omitempty"`
	// RuntimeID is the agent's own session id (Claude/Codex UUID), when known.
	RuntimeID string `json:"runtime_id,omitempty"`
	// Usage attribution (SPEC 8 "Usage"), recorded at spawn/register, never
	// guessed later: Account is the Claude login active then, For the identity
	// it works for, JobKind one of the JobKind* values. Tokens are the whole
	// session's, summed from its own transcript while it runs and at its end.
	Account string         `json:"account,omitempty"`
	For     string         `json:"for,omitempty"`
	JobKind string         `json:"job_kind,omitempty"`
	Tokens  *SessionTokens `json:"tokens,omitempty"`
	Managed bool           `json:"managed"`
	PID     int            `json:"pid,omitempty"`
	Outcome *Outcome       `json:"outcome,omitempty"` // set when State is ended
	Started time.Time      `json:"started"`
	Updated time.Time      `json:"updated"`
	Ended   *time.Time     `json:"ended,omitempty"`
	// Seen is set on a session of another machine that a team authority
	// knows from that replica's presence (SPEC 17.3): its last heartbeat.
	// A heartbeat older than 60 s shows the session idle ("presence stale").
	Seen *time.Time `json:"seen,omitempty"`

	// Activity is the agent's latest step, as its hooks report it (one line,
	// at most MaxActivityLen characters). It goes to everyone who can read the
	// session, teammates included, so hooks never put a command, a prompt or
	// file contents in it: "Editing store.go", "Searching", "Web: go.dev".
	// A hook reporting idle with no activity (Claude's Stop and SessionStart,
	// an idle Notification, Codex notify, `zm session state idle`) clears it.
	Activity string `json:"activity,omitempty"`
	// ActivityAt is when Activity was last reported (refreshed at most every
	// 30 s while it repeats).
	ActivityAt *time.Time `json:"activity_at,omitempty"`
	// TermTitle is the latest terminal title a managed session's program set
	// (OSC 0 or 2); Claude Code sets the conversation's topic. It leaves the
	// machine (presence) only for a known agent runtime (SharesTermTitle).
	TermTitle string `json:"term_title,omitempty"`
	// Who is whose session it is, resolved by the daemon answering: a local
	// session's launcher, a replica's session the identity its machine joined
	// as (SPEC 17.3).
	Who *SessionWho `json:"who,omitempty"`
	// Fg is the foreground process of a managed session's terminal (the
	// PTY's foreground process group leader), Busy reports that it is not
	// the session's own process (a child runs: a build, vim under a shell),
	// and Ports are the TCP ports in LISTEN state of any process in the
	// session's process tree. Jobs counts the processes in that tree other
	// than the session's own (its shell or agent): a shell's background (&)
	// and suspended (^Z) jobs, which leave Busy false because the shell holds
	// the terminal, and an agent's helpers (MCP servers, tool runs); a
	// process that reparented itself away (a double fork, "(cmd &)") is not
	// counted, an unreaped zombie is. Sampled at most every 2 s, and only
	// while a client is attached or the session was read recently (else fg,
	// busy and jobs are dropped as stale; ?sample=1 samples on the spot);
	// this machine's owner only (never to a token or tailnet reader, never
	// in presence). Jobs is set (0 included) whenever the foreground process
	// was sampled, and absent otherwise (no sample yet, an unmanaged session,
	// an older daemon), so "0 jobs" is never confused with "unknown".
	Fg    *SessionFg `json:"fg,omitempty"`
	Busy  bool       `json:"busy,omitempty"`
	Ports []int      `json:"ports,omitempty"`
	Jobs  *int       `json:"jobs,omitempty"`
	// Shared: project members may send it prompts (SPEC 17.10); set at
	// spawn, in a team project only. LastPrompt is the latest one, InUse
	// who is using it now (a UI warns before a prompt only then).
	Shared     bool               `json:"shared,omitempty"`
	LastPrompt *SessionLastPrompt `json:"last_prompt,omitempty"`
	InUse      *SessionInUse      `json:"in_use,omitempty"`
	// Reach is how far the session's launcher can act: ReachAll (this
	// machine's owner on its socket), ReachProject or ReachArea (a replica's
	// token, capped to the project or to the session's area).
	Reach string `json:"reach,omitempty"`
	// Mode is "pipe" for a structured session (SPEC 8 "Structured
	// sessions"), whose Capabilities its adapter declared; "" a PTY session.
	Mode         string             `json:"mode,omitempty"`
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`
	// Unreachable says why this machine's daemon cannot reach a live managed
	// session's terminal (its holder speaks another protocol version, SPEC 8
	// "Daemon restarts"): it keeps running, but attach, input and stop are
	// refused until a daemon of a matching version adopts it. Owner only.
	Unreachable string `json:"unreachable,omitempty"`
	// RuntimeAgent is the agent running inside this managed shell (a
	// `claude` typed at a managed shell's prompt): its hooks name the shell's
	// ZM_SESSION, so its state and activity go to this row and no second
	// session is registered (SPEC 8 "Agents in a shell").
	RuntimeAgent *RuntimeAgent `json:"runtime_agent,omitempty"`
	// Subagents are the session's live subagents (Claude Code's Task/Agent
	// tool), oldest first, at most MaxSessionSubagents; GET
	// /v0/sessions/{id}/subagents has every one with its activity lines.
	Subagents []Subagent `json:"subagents,omitempty"`
	// Needs says why a needs_human session waits; absent in any other state.
	Needs *SessionNeeds `json:"needs,omitempty"`
	// SafeOutputs are the caps a start's safe_outputs set, every kind
	// (SafeOutputKinds), on the answer to that start only: a daemon that
	// sends none for a start that asked for caps did not apply them.
	SafeOutputs map[string]int `json:"safe_outputs,omitempty"`
}

// SessionTokens is a session's token use, summed over its whole transcript
// (subagents included). Input excludes cache reads and writes.
type SessionTokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

// Job kinds (Session.JobKind): what started a session.
const (
	JobKindInteractive = "interactive" // a person at a terminal
	JobKindStanding    = "standing"    // a standing automation's run
	JobKindFarm        = "farm"        // a farm runner's lane
	JobKindTimer       = "timer"       // a scheduled job
)

// ValidJobKind reports whether k is "" or one of the JobKind values.
func ValidJobKind(k string) bool {
	switch k {
	case "", JobKindInteractive, JobKindStanding, JobKindFarm, JobKindTimer:
		return true
	}
	return false
}

// MaxForLen bounds Session.For (bytes).
const MaxForLen = 128

// RuntimeAgent is an agent runtime bound to a managed shell (Session.RuntimeAgent).
type RuntimeAgent struct {
	Agent     string `json:"agent"` // claude, codex
	RuntimeID string `json:"runtime_id"`
	Model     string `json:"model,omitempty"`
	// Task is the agent's latest activity (Session.Activity's rules), kept
	// while it is idle so the shell's row still says what it last did.
	Task    string    `json:"task,omitempty"`
	PID     int       `json:"pid,omitempty"`
	Started time.Time `json:"started"`
}

// Subagent is one subagent of a session (Claude Code SubagentStart to
// SubagentStop). State is running or ended.
type Subagent struct {
	ID          string       `json:"id"`             // the runtime's agent_id
	Type        string       `json:"type,omitempty"` // agent_type: Explore, general-purpose, ...
	Description string       `json:"description,omitempty"`
	State       SessionState `json:"state"`
	Activity    string       `json:"activity,omitempty"` // its latest step (Session.Activity's rules)
	Started     time.Time    `json:"started"`
	Updated     time.Time    `json:"updated"`
	Ended       *time.Time   `json:"ended,omitempty"`
}

// MaxSessionSubagents bounds Session.Subagents (the live ones a read carries).
const MaxSessionSubagents = 20

// SessionNeeds is why a session waits on a person.
type SessionNeeds struct {
	Kind string    `json:"kind"` // NeedsPermission, NeedsInput, NeedsDecision, NeedsManual
	At   time.Time `json:"at"`
	// Subagent is the subagent whose step came last before a hook's wait
	// (its prompt, most likely): its next step or its stop ends the wait.
	Subagent string `json:"subagent,omitempty"`
}

// SessionNeeds kinds. permission and input come from the agent's own hooks
// (authoritative), decision from mail the session sent needing a person,
// manual from `zm session state needs_human` (POST /v0/sessions/{id}/state).
const (
	NeedsPermission = "permission"
	NeedsInput      = "input"
	NeedsDecision   = "decision"
	NeedsManual     = "manual"
)

// HookSubagent is a hook's subagent part (HookRequest.Subagent). Event:
// SubagentStart, SubagentStop, SubagentSpawn (the parent's Task/Agent tool
// about to start one: Type and Description, no ID), or "" (a tool step
// inside subagent ID).
type HookSubagent struct {
	Event       string `json:"event,omitempty"`
	ID          string `json:"id,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// HookSubagent events.
const (
	SubagentStart = "start"
	SubagentStop  = "stop"
	SubagentSpawn = "spawn"
)

// ActivityLine is one recorded activity of a session or of one of its subagents.
type ActivityLine struct {
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
	Subagent string    `json:"subagent,omitempty"` // the subagent's id; "" = the session's own
}

// ActivityList is GET /v0/sessions/{id}/activity: newest first.
type ActivityList struct {
	Lines []ActivityLine `json:"lines"`
}

// SubagentDetail is a subagent with its recent activity lines, newest first.
type SubagentDetail struct {
	Subagent
	Lines []ActivityLine `json:"lines"`
}

// SubagentList is GET /v0/sessions/{id}/subagents.
type SubagentList struct {
	Subagents []SubagentDetail `json:"subagents"`
}

// AgentRuntimes are the agent runtimes zm knows (their own CLIs set
// terminal titles to the conversation's topic, never a command line).
var AgentRuntimes = map[string]bool{"claude": true, "codex": true, "gemini": true, "opencode": true}

// SharesTermTitle reports whether a session's term_title may leave this
// machine (presence, so teammates): only a known agent runtime's, never a
// shell's or an unknown program's, whose titles can carry command lines,
// paths and hosts. command, when known, must run that runtime (directly or
// under the srt sandbox).
func SharesTermTitle(agent string, command []string) bool {
	if !AgentRuntimes[agent] {
		return false
	}
	if len(command) == 0 {
		return true
	}
	bin := command[0]
	if i := strings.LastIndexByte(bin, '/'); i >= 0 {
		bin = bin[i+1:]
	}
	return bin == agent || bin == "srt"
}

// SessionLastPrompt is a shared session's latest prompt.
type SessionLastPrompt struct {
	Who  string    `json:"who"` // the sender's identity
	At   time.Time `json:"at"`
	Text string    `json:"text"` // at most MaxActivityLen characters
}

// SessionInUse says a person is using a shared session on its machine: By
// (the machine's owner) has Attached writable attaches now, or typed into
// it at TypedAt (within the last 5 minutes).
type SessionInUse struct {
	By       string     `json:"by"`
	Attached int        `json:"attached"`
	TypedAt  *time.Time `json:"typed_at,omitempty"`
}

// PromptRequest is POST /v0/sessions/{id}/prompt.
type PromptRequest struct {
	Text string `json:"text"`
}

// SessionPrompt is one entry of a shared session's prompt log.
type SessionPrompt struct {
	ID          string     `json:"id"`
	Session     string     `json:"session"` // the session's address
	From        string     `json:"from"`    // the sender's address (redacted for a reader who may not see it)
	Who         string     `json:"who"`     // the sender's identity
	Text        string     `json:"text"`
	At          time.Time  `json:"at"`
	State       string     `json:"state"` // PromptPending, PromptDelivered, PromptExpired
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// PromptList is GET /v0/sessions/{id}/prompts.
type PromptList struct {
	Prompts []SessionPrompt `json:"prompts"`
}

// Prompt states and limits (SPEC 17.10).
const (
	PromptPending   = "pending"
	PromptDelivered = "delivered"
	PromptExpired   = "expired"
	MaxPromptBytes  = 8 << 10
)

// SessionFg is the process in the foreground of a session's terminal.
type SessionFg struct {
	PID  int    `json:"pid"`
	Name string `json:"name"` // executable name: "zsh", "claude", "vim", "npm"
}

// OwnerOnlySessionKeys are the Session fields only this machine's owner
// sees (events and reads strip them for any other reader).
var OwnerOnlySessionKeys = []string{"fg", "busy", "ports", "jobs", "unreachable"}

// SessionWho names a session's person or machine.
type SessionWho struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // human | machine | agent
}

// Session reach values.
const (
	ReachAll     = "all"
	ReachProject = "project"
	ReachArea    = "area"
)

// MaxActivityLen bounds Session.Activity and Session.TermTitle (characters).
const MaxActivityLen = 120

// SpawnRequest is POST /v0/sessions: start an agent in a new PTY (local callers only).
// The project comes from resolution; no project = "none".
type SpawnRequest struct {
	Agent    string   `json:"agent"`             // claude, codex, opencode, gemini, shell, or custom with Command
	Command  []string `json:"command,omitempty"` // overrides the agent's default command
	Model    string   `json:"model,omitempty"`
	Cwd      string   `json:"cwd"` // the client's cwd; required
	Area     string   `json:"area,omitempty"`
	Title    string   `json:"title,omitempty"`
	Name     string   `json:"name,omitempty"`     // a session name (ValidSessionName), free among live sessions here
	Work     string   `json:"work,omitempty"`     // a work item id; the session takes a bound lease on it
	Prompt   string   `json:"prompt,omitempty"`   // initial CLI prompt for known interactive Codex launches; otherwise typed input
	Worktree bool     `json:"worktree,omitempty"` // create a git worktree + branch for it
	// Sandbox runs the agent under Anthropic's sandbox runtime (`srt` on PATH),
	// allowed to write only the project's repos, its worktree and scratch dir.
	// Refused when srt is missing or the session has no project.
	Sandbox bool              `json:"sandbox,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // the client's environment (PATH etc.); ZM_* are replaced
	// Shared lets project members send it prompts (a team project only;
	// SPEC 17.10).
	Shared bool `json:"shared,omitempty"`
	Rows   int  `json:"rows,omitempty"`
	Cols   int  `json:"cols,omitempty"`
	// Mode "pipe" starts a structured session (SPEC 8 "Structured
	// sessions"): the agent's adapter on pipes, driven by turns; "" a PTY.
	Mode string `json:"mode,omitempty"`
	// Approvals, for a pipe session: "ask" (approval events), "never"
	// (nothing waits on a person: what would ask is declined), "" the
	// runtime's own settings.
	Approvals string `json:"approvals,omitempty"`
	// Resume, for a pipe session whose adapter has the resume capability:
	// the runtime's own id (a Claude session, a Codex thread) to go on from.
	Resume string `json:"resume,omitempty"`
	// For and JobKind are Session.For and Session.JobKind (zm run --for,
	// --job-kind; the CLI falls back to $ZM_FOR and $ZM_JOB_KIND).
	For     string `json:"for,omitempty"`
	JobKind string `json:"job_kind,omitempty"`
	// MCP, for a pipe session whose adapter has the mcp capability: remote
	// MCP servers the runtime starts with (header values may be
	// credentials: zm hands them to the runtime and keeps them nowhere).
	MCP []AgentMCP `json:"mcp,omitempty"`
	// Machine starts the structured session on another machine of its team
	// project, through the authority (SPEC 8 "Sessions on other machines"):
	// Cwd then only resolves the project here, and the session runs in the
	// project's checkout there. ClientRequestID makes a retry answer the
	// session the first try started. Machine naming this machine is a local
	// start, and ClientRequestID is then ignored: a retry starts again.
	Machine         string `json:"machine,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	// ContinueFrom, for a pipe session: a pipe session whose history the
	// first turn carries as zm's budgeted handoff, before Prompt (SPEC 8
	// "Pipe handoffs").
	ContinueFrom string `json:"continue_from,omitempty"`
	// AllowedTools and PermissionMode, for a pipe session whose adapter has
	// the tools capability: AgentStart's tool grant.
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	// SafeOutputs caps the writes the session may make through zm, per
	// kind (SafeOutputKinds): the daemon the session runs on counts them
	// and refuses one past its cap. Set at the start only.
	SafeOutputs map[string]int `json:"safe_outputs,omitempty"`
}

// RegisterRequest is POST /v0/sessions/register: an agent hook announcing a
// session zm did not spawn. Idempotent per (agent, runtime_id). Never fails
// for lack of a project (it registers with project "none").
type RegisterRequest struct {
	Agent     string   `json:"agent"`
	RuntimeID string   `json:"runtime_id"`
	Cwd       string   `json:"cwd,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Title     string   `json:"title,omitempty"`
	Command   []string `json:"command,omitempty"`
	Version   string   `json:"version,omitempty"`
	Model     string   `json:"model,omitempty"`
	// Session is the managed session this hook runs inside ($ZM_SESSION):
	// the runtime id is recorded on it instead of creating a second session.
	Session string `json:"session,omitempty"`
	// State and Note, when set, are applied in the same call (hooks: one round trip).
	State SessionState `json:"state,omitempty"`
	Note  string       `json:"note,omitempty"`
	// End marks an unmanaged session ended with outcome ok (the runtime's own
	// session-end hook; Note says why). Ignored for managed sessions.
	End bool `json:"end,omitempty"`
	// For and JobKind come from the hook's $ZM_FOR and $ZM_JOB_KIND;
	// AgentHome is its $CLAUDE_CONFIG_DIR or $CODEX_HOME. Each is sent only
	// when set and read only when the session is first registered.
	For       string `json:"for,omitempty"`
	JobKind   string `json:"job_kind,omitempty"`
	AgentHome string `json:"agent_home,omitempty"`
}

// HookRequest is POST /v0/sessions/hook: what an agent hook reports, in one
// round trip. It registers like POST /v0/sessions/register (idempotent per
// (agent, runtime_id); Session names the managed session the hook runs in)
// and applies State/Note; Notice also returns the session's notice line.
type HookRequest struct {
	RegisterRequest
	Notice bool `json:"notice,omitempty"`
	// Activity, when set, is the session's latest step (Session.Activity):
	// never a command, a prompt or file contents.
	Activity string `json:"activity,omitempty"`
	// Subagent, when set, says the hook is about a subagent: its start or
	// stop (no state change for the session), a step inside it (Activity is
	// the subagent's, and the session's cwd is left alone), or the parent
	// spawning one.
	Subagent *HookSubagent `json:"subagent,omitempty"`
	// Needs is why a needs_human State waits: NeedsPermission or NeedsInput.
	Needs string `json:"needs,omitempty"`
}

// HookResponse is the session after the hook and, when asked, its notice.
type HookResponse struct {
	Session Session `json:"session"`
	Notice  *Notice `json:"notice,omitempty"`
}

// InputRequest is POST /v0/sessions/{id}/input. Data is written as-is;
// Enter=true appends a carriage return.
type InputRequest struct {
	Data  string `json:"data"`
	Enter bool   `json:"enter,omitempty"`
}

// ResizeRequest is POST /v0/sessions/{id}/resize.
type ResizeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// StateRequest is POST /v0/sessions/{id}/state (hooks report what the agent is doing).
type StateRequest struct {
	State SessionState `json:"state"`
	Note  string       `json:"note,omitempty"`
}

// NameRequest is POST /v0/sessions/{id}/name: set the session's name, or
// clear it with "".
type NameRequest struct {
	Name string `json:"name"`
}

// MoveRequest is POST /v0/sessions/{id}/project: put the session in
// another project (a slug, ULID, "slug/area" or "none") and area. Refused
// (conflict) while the session holds a lease.
type MoveRequest struct {
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
}

// StopRequest is POST /v0/sessions/{id}/stop.
type StopRequest struct {
	Signal string `json:"signal,omitempty"` // TERM (default), INT, HUP, KILL
	// WaitMS is how long to wait for the process to exit before answering
	// (default 3000, at most 10000). The answer is the session as it is then.
	WaitMS int `json:"wait_ms,omitempty"`
	// Escalate does what `zm kill` does: TERM and up to 3 s, then HUP (an
	// interactive shell ignores TERM) and up to 1 s; with Force, then KILL
	// and up to 3 s. Each wait ends as soon as the session does. Not with
	// Signal or WaitMS. The response header Zm-Stop-Signals lists the
	// signals sent ("TERM, HUP").
	Escalate bool `json:"escalate,omitempty"`
	Force    bool `json:"force,omitempty"` // only with Escalate: KILL after HUP
}

// HeaderStopSignals is the response header of POST /v0/sessions/{id}/stop:
// the signals sent, in order, comma separated.
const HeaderStopSignals = "Zm-Stop-Signals"

// SessionQuery is GET /v0/sessions's query string. No project = every visible project.
type SessionQuery struct {
	Project string
	State   SessionState
	Agent   string
	All     bool // include ended
	// Before is a page cursor: the Next of the previous page. Rows come most
	// recently updated first (then by state within the page).
	Before string
	Limit  int // rows per page, 1..500 (default 500)
}

// SessionList is GET /v0/sessions.
type SessionList struct {
	Sessions []Session `json:"sessions"`
	// Next, when set, is the cursor for the following page (?before=Next):
	// this page was full. Opaque; never a count.
	Next string `json:"next,omitempty"`
	// Skipped names unavailable shares; Behind names shares that are not current.
	// A list carrying either may omit sessions or contain older data.
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
	Behind  []WorkBehind      `json:"behind,omitempty"`
}

// Output is GET /v0/sessions/{id}/output: recent scrollback.
type Output struct {
	Data   string `json:"data"` // terminal output from Offset to Next (UTF-8-lossy)
	Offset int64  `json:"offset"`
	Next   int64  `json:"next"` // pass as ?since= to continue
}

// AttachProtocol is the HTTP Upgrade token for GET /v0/sessions/{id}/attach.
const AttachProtocol = "zm-pty"

// Admission reasons: why a spawn was refused.
const (
	AdmitMemory   = "memory"
	AdmitDisk     = "disk"
	AdmitSessions = "sessions"
)

// AdmissionProbe is what the machine looks like now.
type AdmissionProbe struct {
	// MemoryFreePercent is macOS kern.memorystatus_level (Linux: MemAvailable/MemTotal).
	MemoryFreePercent int   `json:"memory_free_percent"`
	SwapUsedMB        int64 `json:"swap_used_mb"`
	DiskFreeMB        int64 `json:"disk_free_mb"` // min of $ZM_HOME's and the cwd's volume
	ManagedSessions   int   `json:"managed_sessions"`
}

// Admission is GET /v0/sessions/admission and the error data of a refused
// spawn (code "unavailable"). Refusals never kill anything.
type Admission struct {
	OK     bool           `json:"ok"`
	Reason string         `json:"reason,omitempty"` // memory | disk | sessions
	Detail string         `json:"detail,omitempty"`
	Probe  AdmissionProbe `json:"probe"`
}

// GCCandidate is one row of GET /v0/sessions/gc: an ended session's worktree
// that MAY be deletable. zm never deletes it; a human does.
type GCCandidate struct {
	Session  string   `json:"session"`
	Outcome  *Outcome `json:"outcome,omitempty"`
	Worktree string   `json:"worktree"`
	Branch   string   `json:"branch,omitempty"`
	Dirty    bool     `json:"dirty"`   // git status shows changes
	Ahead    int      `json:"ahead"`   // commits on the branch not on its upstream (or main)
	Reasons  []string `json:"reasons"` // why it is (or is not) safe
	Safe     bool     `json:"safe"`    // ended and clean; task branches are retained
	// Repo is the main checkout the worktree belongs to; Remove is the shell
	// command that deletes it (only when Safe), runnable from any directory.
	Repo             string `json:"repo,omitempty"`
	Remove           string `json:"remove,omitempty"`
	Transcript       bool   `json:"transcript,omitempty"`
	TranscriptRemove string `json:"transcript_remove,omitempty"`
}

// GCList is GET /v0/sessions/gc.
type GCList struct {
	Candidates []GCCandidate `json:"candidates"`
}

// SessionLeft is the data of dev.zm.session.left, published to the
// project/area a session moved out of: only where it was, never where it
// went.
type SessionLeft struct {
	Address string `json:"address"`
	Project string `json:"project"` // the project it left
	Area    string `json:"area,omitempty"`
}

// Session event types. Data is the Session (admission_refused: the
// Admission; left: SessionLeft).
const (
	EventSessionStarted          = "dev.zm.session.started"
	EventSessionRegistered       = "dev.zm.session.registered"
	EventSessionStateChanged     = "dev.zm.session.state_changed"
	EventSessionUpdated          = "dev.zm.session.updated" // activity or term_title changed (state did not)
	EventSessionLeft             = "dev.zm.session.left"    // moved out of this project/area; data SessionLeft
	EventSessionEnded            = "dev.zm.session.ended"
	EventSessionAdmissionRefused = "dev.zm.session.admission_refused"
)
