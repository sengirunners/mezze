package orchapi

import "strings"

// TaskStartRequest starts a one-shot child on this machine.
type TaskStartRequest struct {
	Prompt          string `json:"prompt"`
	Agent           string `json:"agent"`
	Model           string `json:"model,omitempty"`
	Work            string `json:"work,omitempty"`
	Worktree        *bool  `json:"worktree,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	TimeoutSeconds  int    `json:"timeout_s,omitempty"`
	ContinueFrom    string `json:"continue_from,omitempty"`
	OnLimit         string `json:"on_limit,omitempty"`
	// Machine starts the task on another zm machine of the parent's team
	// project through its authority (SPEC 8 "Machine choice"): "auto" or a
	// machine name; empty or this machine's name starts it here.
	Machine string `json:"machine,omitempty"`
	// For and JobKind are the task session's (Session.For, Session.JobKind);
	// unset, it takes its parent's.
	For     string `json:"for,omitempty"`
	JobKind string `json:"job_kind,omitempty"`
	// Run, with Agent "run", runs a script instead of an agent (SPEC 8
	// "Script runs"): no prompt, and only on another machine.
	Run *TaskRun `json:"run,omitempty"`
	// Lookup, with machine and client_request_id, answers the task an
	// earlier start with this request made (not_found when none) and never
	// starts one (zm exec --cancel; SPEC 8 "Script runs").
	Lookup bool `json:"lookup,omitempty"`
	// Front, with machine, puts the start before every non-front start
	// waiting in the authority's line; front starts keep their own order
	// (SPEC 8 "Waiting line").
	Front bool `json:"front,omitempty"`
	// Sandbox asks for the session to run in a pool box through a machine's
	// task driver instead of on the host. A sandbox request may not also
	// carry Run or ContinueFrom (the server enforces this in a later change).
	Sandbox *TaskSandbox `json:"sandbox,omitempty"`
}

// TaskSandbox is a sandbox task's box request (SPEC 8 "Waiting line"). Size
// names a pool target size such as c2m8 or c4m16; empty means the driver's
// default. Not SpawnRequest.Sandbox, which is the srt filesystem sandbox of a
// session.
type TaskSandbox struct {
	Size string `json:"size,omitempty"`
	// Egress names hosts the box's egress allowlist adds for this task
	// (name.tld or *.name.tld); the driver enforces the list and its own
	// policy. Empty: the driver's default list.
	Egress []string `json:"egress,omitempty"`
}

// TaskRun is a script run: the machine resolves Ref on its checkout's
// remote, runs Script (a path in the repository) from a clean checkout of
// exactly that commit with Args as literal arguments, and removes the
// checkout after. Commit, when sent, must equal what Ref resolves to,
// unless CommitOnRef: then Commit may also be an ancestor of it, and the
// checkout is exactly Commit (an approved ask whose commit was Ref's head
// when asked, run after Ref moved on). Command, instead of Script and Args,
// is one shell command line /bin/sh runs in that checkout (an approved
// command action, SPEC 16); a machine that does not know it sees no script
// and refuses the run.
type TaskRun struct {
	Ref         string   `json:"ref"`
	Commit      string   `json:"commit,omitempty"`
	CommitOnRef bool     `json:"commit_on_ref,omitempty"`
	Script      string   `json:"script"`
	Args        []string `json:"args,omitempty"`
	Command     string   `json:"command,omitempty"`
}

// RunRefProblem says what is wrong with ref as a run's ref ("" when it is
// fine): a branch or tag name as a person types it (main, v1.2), never a
// full refs/... name, a path or an option. The run machine and an ask's
// action check the same rule.
func RunRefProblem(ref string) string {
	ok := ref != "" && len(ref) <= 200 && !strings.Contains(ref, "..") && !strings.HasPrefix(ref, "-") &&
		!strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/") && !strings.HasPrefix(ref, "refs/")
	for _, c := range ref {
		ok = ok && (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-", c))
	}
	if ok {
		return ""
	}
	if short, cut := strings.CutPrefix(ref, "refs/heads/"); cut {
		return "run.ref must be a branch or tag name such as main: use " + short
	}
	if short, cut := strings.CutPrefix(ref, "refs/tags/"); cut {
		return "run.ref must be a branch or tag name such as main: use " + short
	}
	return "run.ref must be a branch or tag name such as main"
}

// RunCommitOK reports whether c is a run's commit: a full 40-character
// lower-case commit id (the run machine takes no other form).
func RunCommitOK(c string) bool {
	if len(c) != 40 {
		return false
	}
	for _, r := range c {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// TaskAgentRun is the Agent of a script run.
const TaskAgentRun = "run"

// TaskRunOutput is the task_read item holding a script run's output.
const TaskRunOutput = "output"

type TaskUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

type TaskResult struct {
	Finished     string    `json:"finished"`
	TextWithheld bool      `json:"text_withheld,omitempty"`
	Text         string    `json:"text"`
	Outcome      Outcome   `json:"outcome"`
	Usage        TaskUsage `json:"usage"`
	Branch       string    `json:"branch,omitempty"`
}

type Task struct {
	ID                string      `json:"id"`
	Session           string      `json:"session"`
	Parent            string      `json:"parent"`
	Agent             string      `json:"agent"`
	Model             string      `json:"model,omitempty"`
	Work              string      `json:"work,omitempty"`
	State             string      `json:"state"` // running or ended
	RedactionPending  bool        `json:"redaction_pending,omitempty"`
	Result            *TaskResult `json:"result,omitempty"`
	ContinuedFrom     string      `json:"continued_from,omitempty"`
	Continuation      string      `json:"continuation,omitempty"`
	ContinuationError string      `json:"continuation_error,omitempty"`
	MailDelivery      string      `json:"mail_delivery"`
	CommentDelivery   string      `json:"comment_delivery"`
	MailError         string      `json:"mail_error,omitempty"`
	CommentError      string      `json:"comment_error,omitempty"`
	// Machine is where a task started through the authority runs (SPEC 8
	// "Machine choice"); its ID then starts with RemoteTaskPrefix and State
	// may also be "pending" (not taken yet) or "refused" (Error says why).
	Machine string `json:"machine,omitempty"`
	Error   string `json:"error,omitempty"`
	// Tried is set on a remote task that handed off on a limit (on_limit
	// next_agent): each earlier agent of its chain and how it ended. The
	// task's Agent, Session and State are then the newest continuation's.
	Tried []TaskTry `json:"tried,omitempty"`
	// The waiting line (SPEC 8 "Waiting line"), on a remote task only:
	// Position is a waiting start's place (1 is next; 0 once placed),
	// Requeued how often it went back after its machine was lost, Acted
	// the outward actions zm saw it take (mail, comment, status, push, ...),
	// and Lost, on an ended one, why it ended with its machine lost instead
	// of going back to the line.
	Position int      `json:"position,omitempty"`
	Requeued int      `json:"requeued,omitempty"`
	Acted    []string `json:"acted,omitempty"`
	Lost     string   `json:"lost,omitempty"`
	// Sandbox is the request's sandbox, set on a sandbox task only.
	Sandbox *TaskSandbox `json:"sandbox,omitempty"`
}

// RemoteTaskList is GET /v0/team/tasks: a team project's remote tasks, the
// waiting line first in its order, then the placed ones, then the newest
// settled (SPEC 8 "Waiting line").
type RemoteTaskList struct {
	Project string `json:"project"`
	Tasks   []Task `json:"tasks"`
}

// TaskTry is one agent a task's chain ran before the current one.
type TaskTry struct {
	Agent   string      `json:"agent"`
	Session string      `json:"session"`
	Outcome OutcomeKind `json:"outcome"`
}

// RemoteTaskPrefix starts the ID of a task started on another machine.
const RemoteTaskPrefix = "rt-"

// Remote task states besides running and ended.
const (
	TaskPending = "pending"
	TaskRefused = "refused"
)

// RemoteTaskRequest is POST /v0/team/tasks at a team authority: the parent's
// machine asks it to start a task on Machine (SPEC 8 "Machine choice").
type RemoteTaskRequest struct {
	Project string           `json:"project"` // the authority's ULID
	Machine string           `json:"machine"` // a member machine, the authority's own, or "auto"
	Parent  string           `json:"parent"`  // the parent session's address (on the caller's machine)
	Depth   int              `json:"depth"`   // the task's depth (the parent's plus one)
	Request TaskStartRequest `json:"request"`
}

// TaskStartOp is a task_start op's payload: the parent session (its
// address on the asking machine), the task's depth and the request. Ask
// marks an approved ask's action (SPEC 16), set by the authority only: its
// parent, the asking session, may be a session on the target itself.
// AsApprover, also the authority's only and only with Ask, marks a command
// or script run on its approver's own machine: its zm calls act as that
// machine's human, the approver, not as its task session.
type TaskStartOp struct {
	Parent     string           `json:"parent"`
	Depth      int              `json:"depth"`
	Request    TaskStartRequest `json:"request"`
	Ask        bool             `json:"ask,omitempty"`
	AsApprover bool             `json:"as_approver,omitempty"`
}

// SessionStartOp is a session_start op's payload: the request, its area
// and the launcher's role in the project, which caps the session (SPEC 1.3).
type SessionStartOp struct {
	Request SpawnRequest `json:"request"`
	Area    string       `json:"area,omitempty"`
	Role    string       `json:"role,omitempty"`
}

// T3EnsureOp is what a t3_ensure op asks of its machine: the credential's
// label ("zm: <requesting machine>") and scopes (none: T3's standard set).
type T3EnsureOp struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes,omitempty"`
}

// SessionOp is one operation on a structured session relayed by the
// authority: exactly one of Turn, Interrupt and Answer.
type SessionOp struct {
	Turn      *SessionTurnRequest      `json:"turn,omitempty"`
	Interrupt *SessionInterruptRequest `json:"interrupt,omitempty"`
	Answer    *SessionAnswerRequest    `json:"answer,omitempty"`
	// Stop ends a session started from another machine (SPEC 8 "Sessions on
	// other machines"); no other shared session is stopped from afar.
	Stop *SessionStopOp `json:"stop,omitempty"`
}

// SessionStopOp is a relayed stop: it carries nothing.
type SessionStopOp struct{}

// RemoteSessionRequest is POST /v0/team/sessions at a team authority: a
// member machine asks it to start a structured session on Machine.
type RemoteSessionRequest struct {
	Project string       `json:"project"` // the authority's ULID
	Machine string       `json:"machine"` // a member machine, the authority's own, or "auto"
	Area    string       `json:"area,omitempty"`
	Request SpawnRequest `json:"request"`
}

type TaskCapabilities struct {
	Structured bool `json:"structured"`
	Delegation bool `json:"delegation"`
	Resume     bool `json:"resume"`
}

type TaskAgent struct {
	Agent         string           `json:"agent"`
	Available     bool             `json:"available"`
	Authenticated *bool            `json:"authenticated,omitempty"`
	Capabilities  TaskCapabilities `json:"capabilities"`
	Reason        string           `json:"reason,omitempty"`
	Models        []string         `json:"models"` // empty: runtime chooses its configured default
}

type TaskAgents struct {
	Agents []TaskAgent `json:"agents"`
}

// TaskHistoryItem is a whole message from a task transcript.
type TaskHistoryItem struct {
	ID   string `json:"id"`
	Role string `json:"role,omitempty"` // a pipe session's: user | assistant
	Text string `json:"text"`
}

type TaskHistoryPage struct {
	Items []TaskHistoryItem `json:"items"`
	Next  string            `json:"next,omitempty"`
}
