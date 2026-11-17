package orchapi

import (
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Approver states.
const (
	// ApproverPending: the name is reserved and a one-time pairing code is
	// out; no device is bound yet.
	ApproverPending = "pending"
	// ApproverUnconfirmed: a device paired with the code, but an existing
	// approver has not confirmed it on their own device yet. No decide right.
	ApproverUnconfirmed = "unconfirmed"
	// ApproverActive: paired (and confirmed, unless it was the first). Holds
	// the decide right, bound to its tailnet node.
	ApproverActive = "active"
	// ApproverRemoved: a key that answers no more (Ended says when and
	// EndedWhy why). The row stays so old receipts still name it.
	ApproverRemoved = "removed"
)

// How a key approver got its right (Approver.Via).
const (
	// ViaLogin: bound by a GitHub sign-in of purpose key (SPEC "Key
	// approvers"): its person, account and node are the authority's view.
	ViaLogin = "login"
)

// Why a key ended (Approver.EndedWhy).
const (
	// EndedReplaced: a later sign-in of the same person on the same node
	// bound another key (ReplacedBy names it).
	EndedReplaced = "replaced"
	// EndedLegacy: a key paired before sign-in binding, not bound by a
	// sign-in within the grace period. (A sign-in that ends such a key
	// marks it EndedReplaced, with ReplacedBy.)
	EndedLegacy = "legacy"
	// EndedSignedOut: the key ended itself (DELETE /v0/approvals/key).
	EndedSignedOut = "signed-out"
)

// Refusal is an error's data when a client must tell refusals apart
// without reading the message: {"reason": ...}.
type Refusal struct {
	Reason string `json:"reason"`
	// Machine names the machine the reason is about (member-too-old).
	Machine string `json:"machine,omitempty"`
}

// Refusal reasons.
const (
	// ReasonSignedOnly: POST /v0/asks/{id}/choose picked a choice that runs
	// something, which only a signed answer may pick.
	ReasonSignedOnly = "signed-only"
	// ReasonMemberTooOld: the member machine that forwarded a key sign-in,
	// a signed answer or a key's removal runs a zm too old to check its
	// caller (orchapi.OriginChecked missing).
	ReasonMemberTooOld = "member-too-old"
	// ReasonConfirmWord: the answer picked a risky action's choice
	// (AskAction.Risk) without its ConfirmWord typed exactly.
	ReasonConfirmWord = "confirm-word"
	// The other 409s of POST /v0/asks/{id}/choose carry data {"reason": ...},
	// except an ask already answered differently (its data is the standing
	// Decision) and a write a team replica forwarded and lost or replayed (no
	// reason: read the ask again). The signed answer shares most of them; its
	// release of a held card (release_unchecked) answers its 409s with no data.
	// ReasonHeld: the card judge has not passed the ask yet; nobody was asked.
	ReasonHeld = "held"
	// ReasonJoinedAction: the ask joined another that carries an action and is
	// open; answer that one.
	ReasonJoinedAction = "joined-action-ask"
	// ReasonWithdrawn: the asker withdrew the ask.
	ReasonWithdrawn = "withdrawn"
	// ReasonDefaultStands: sending an ask back while its applied default stands.
	ReasonDefaultStands = "default-stands"
	// ReasonCardChanged: the card is not the one read (the request's
	// card_sha256, or a change while the answer was made); read the ask again.
	ReasonCardChanged = "card-changed"
)

// UnbindKeyVersion is the domain of a key's signed removal.
const UnbindKeyVersion = "zm-unbind-key/1"

// BindKeyVersion is the domain of a key login's proof: the key signs
// BindKeyVersion, the authority's receipt key id, the project (its slug as
// sent, or its id), the key, the nonce and the time, one per line.
const BindKeyVersion = "zm-bind-key/1"

// UnbindKeyRequest is DELETE /v0/approvals/key: a key ends itself (signing
// out). Proof is base64 (std) of an ASN.1 ECDSA SHA-256 signature by that
// key over UnbindKeyVersion, the authority's receipt key id, Key, Nonce and
// Time, one per line. A nonce is accepted once.
type UnbindKeyRequest struct {
	Key   string `json:"key"` // the fingerprint, p256:<16 hex>
	Nonce string `json:"nonce"`
	Time  string `json:"time"` // RFC 3339, within 5 minutes
	Proof string `json:"proof"`
}

// Approver is a paired device that may decide asks. The right is bound to
// the device's tailnet node (WhoIs stable node ID + node key) or, for a device
// that shares a machine with agents, to a key only it holds (SPEC 16 "Key
// approvers"), never to a token, a name or a Tailscale login.
type Approver struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	Node        string     `json:"node,omitempty"`      // stable node ID once paired
	NodeName    string     `json:"node_name,omitempty"` // the device's tailnet name (display only)
	Login       string     `json:"login,omitempty"`     // the device owner's login (display only; never authority)
	AddedBy     string     `json:"added_by"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"` // an approver's name, or "bootstrap" for the first
	Created     time.Time  `json:"created"`
	Paired      *time.Time `json:"paired,omitempty"`
	CodeExpires *time.Time `json:"code_expires,omitempty"` // while pending
	// Note is set on a pending or unconfirmed approver that nobody can
	// confirm (no active approver is left and this daemon already used its
	// one bootstrap): it says so and how the Mac's human recovers.
	Note string `json:"note,omitempty"`
	// Key is a key approver's fingerprint (p256:<16 hex>), for a person to
	// compare on the device that holds the key before confirming. Empty for a
	// device bound to its tailnet node.
	Key string `json:"key,omitempty"`
	// Identity is the person whose device this is (zm approver add --for),
	// empty for this machine's owner.
	Identity string `json:"identity,omitempty"`
	// A key bound by a sign-in (Via ViaLogin): the provider account it
	// answers as ("github:<numeric id>", the value of record; Login is
	// its login at the last bind), the machine it answers from and when it
	// was bound. Machine is display only; the node is what is checked.
	Account string     `json:"account,omitempty"`
	Machine string     `json:"machine,omitempty"`
	Via     string     `json:"via,omitempty"`
	Bound   *time.Time `json:"bound,omitempty"`
	// An ended key (state removed): when, why (Ended* constants) and, when
	// replaced, the fingerprint of the key that replaced it.
	Ended      *time.Time `json:"ended,omitempty"`
	EndedWhy   string     `json:"ended_why,omitempty"`
	ReplacedBy string     `json:"replaced_by,omitempty"`
}

// ApproverList is GET /v0/approvers.
type ApproverList struct {
	Approvers []Approver `json:"approvers"`
}

// NewApproverRequest is POST /v0/approvers (local).
type NewApproverRequest struct {
	Name string `json:"name"`
	// For is the person whose device this is (an identity's name; default
	// this machine's owner): the Decisions page lists that person's decisions.
	For string `json:"for,omitempty"`
}

// NewApproverResponse carries the one-time pairing code, shown once.
type NewApproverResponse struct {
	Approver Approver  `json:"approver"`
	Code     string    `json:"code"`
	Expires  time.Time `json:"expires"`
	// PairPath is the pairing page's path on the daemon's tailnet listener,
	// with the code (prefix it with the tailnet URL from GET /v0/tailnet).
	PairPath string `json:"pair_path"`
	// Confirm is true when an existing approver must confirm the new device.
	Confirm bool `json:"confirm"`
	// Note is set when nobody can confirm it (see Approver.Note).
	Note string `json:"note,omitempty"`
}

// PairRequest is POST /v0/approvals/pair (from the device, over the tailnet).
type PairRequest struct {
	Code string `json:"code"`
}

// ConfirmRequest is POST /v0/approvers/{name}/confirm: what the person compared
// (Approver.Node as the device showed it). A different device under that name
// is a conflict. Required for a key approver.
type ConfirmRequest struct {
	Node string `json:"node,omitempty"`
}

// PairKeyRequest is POST /v0/approvals/pair-key: a device on a machine that
// runs agents pairs a key it holds. It never gets the right by pairing: an
// approver on another device must confirm it.
type PairKeyRequest struct {
	Code string `json:"code"`
	// PublicKey is base64 (std) of the P-256 key's uncompressed X9.63 form
	// (65 bytes).
	PublicKey string `json:"public_key"`
	// Device is a display name, e.g. "Mezze app on the MacBook" (never authority).
	Device string `json:"device,omitempty"`
	// Proof is base64 (std) of an ASN.1 ECDSA SHA-256 signature over
	// "zm-pair-key/1\n" + the code (upper case, no dashes) + "\n" + PublicKey.
	Proof string `json:"proof"`
}

// AskSubject is what an ask is about: exactly one of a pull request (Repo,
// PR, HeadSHA, every one required), a work item (Work) or a plain question
// (Question, set by the daemon). The fields are compared
// exactly: a receipt never matches another repo, PR or head SHA.
type AskSubject struct {
	Repo    string `json:"repo"`     // canonical: host/owner/name, e.g. github.com/acme/app
	PR      int    `json:"pr"`       // pull request number
	HeadSHA string `json:"head_sha"` // the full 40 (or 64) hex commit id, lowercase
	// Work is the work item's canonical id (bd:zm-1, local:k3x9qa).
	Work string `json:"work,omitempty"`
	// Question is the sha256 (hex) of a plain question's note card.
	Question string `json:"question,omitempty"`
}

// IsPR reports whether the subject is a pull request's head commit.
func (s AskSubject) IsPR() bool { return s.Work == "" && s.Question == "" }

// Decisions an ask offers by default.
const (
	DecisionApprove = "approve"
	DecisionDecline = "decline"
)

// DecisionOther is the decision recorded for a free-text answer that picks
// none of the card's choices.
const DecisionOther = "other"

// DecisionNeedsContext sends an ask back: the approver cannot decide without
// more, and says what in the answer's text. It decides nothing.
const DecisionNeedsContext = "needs_context"

// DecisionReleaseUnchecked is what a paired device or a key approver sends to
// POST /v0/asks/{id}/decide or /answer to release a card the card judge could
// not check (Ask.NotChecked) to the people it is for, as it is. It decides
// nothing and signs no receipt; the answer is the Ask. Any other answer to
// a held card is 409.
const DecisionReleaseUnchecked = "release-unchecked"

// Who answered, as a decision records it.
const (
	AnsweredAsOwner   = "owner"   // the approver's own answer ("My answer")
	AnsweredAsContact = "contact" // the approver tapped in what the ask's contact said ("John said")
)

// How a decision was made (Decision.Method).
const (
	// MethodOwner: the project's owner or admin, from their own machine's
	// socket (or a member machine's, forwarded to the authority).
	MethodOwner = "owner"
	// MethodApprover: a paired approver device, or a key approver.
	MethodApprover = "approver"
)

// Where a decision was made (Decision.Surface).
const (
	SurfaceOwner  = "owner"  // the owner's socket (choose)
	SurfaceDevice = "device" // a paired device (decide)
	SurfaceKey    = "key"    // a key approver's signed answer (the Mezze app)
	// SurfaceAnswerToken: choose with an answer-only token (AnswerLimit)
	// from the one device it is pinned to. Method stays owner: it is
	// never a signed answer.
	SurfaceAnswerToken = "answer-token"
)

// AnswerLimit is the token route limit of an answer-only token (zm token
// mint --answer-only): it reaches only the asks, decisions, choose and
// receipt-verify routes that admit it.
const AnswerLimit = "answer"

// AnswerTokenMachine starts the machine of a decision made with an
// answer-only token (answer-token:<token id>); its receipt node is
// OwnerNodePrefix + that.
const AnswerTokenMachine = "answer-token:"

// OwnerNodePrefix starts a receipt's node when the owner decided:
// owner:<machine>.
const OwnerNodePrefix = "owner:"

// AskChoice is one option on a note card.
type AskChoice struct {
	Key   string `json:"key,omitempty"` // the decision it records; default a, b, c, d in order
	Label string `json:"label"`
	Pros  string `json:"pros,omitempty"`
	Cons  string `json:"cons,omitempty"`
	// Effect is the one line a person reads under the label: what picking
	// it changes ("The seven emails go out at the set times").
	Effect string `json:"effect,omitempty"`
}

// AskCard is a decision that fits on a note card, in plain words: a person
// reads it knowing nothing of the work. SPEC 16 "Note cards" has the limits.
type AskCard struct {
	Title      string      `json:"title,omitempty"` // default: the question, shortened
	Context    string      `json:"context"`         // explain like I'm 15
	Background string      `json:"background,omitempty"`
	Question   string      `json:"question"`
	Choices    []AskChoice `json:"choices"`
	Recommend  string      `json:"recommend"` // a choice's key
	Why        string      `json:"why"`       // the recommendation's effect, good and bad
	Searched   string      `json:"searched"`  // what the asker checked before asking (not on the card)
	// AfterPress says who acts after the press and how the person will know.
	AfterPress string `json:"after_press,omitempty"`
	// Action is what the server does as the approver when they pick its
	// choice (SPEC 16 "Asks that carry an action"). Frozen at creation:
	// card_sha256, and so the receipt, covers it.
	Action *AskAction `json:"action,omitempty"`
	// Verify is how the asker shows the approved work is done (SPEC 16
	// "Approved, not done"). Frozen at creation like Action.
	Verify *AskVerify `json:"verify,omitempty"`
	// OwnerStep is a step only the person the card is for can do (a
	// sign-in or a press on a service's own site), which no agent can do
	// for them (SPEC 16 "Your step"). Frozen at creation like Action.
	OwnerStep *AskStep `json:"owner_step,omitempty"`
}

// Bounds on a card's owner step.
const (
	MaxStepLines   = 8
	MaxStepLine    = 200 // bytes, each step and the label
	MaxStepLink    = 500 // bytes
	MaxStepMinutes = 60
)

// AskStep is a card's owner step: Label (one line, what to do), Steps (1 to
// MaxStepLines numbered lines, as the person reads them), Link (an https
// page on the service the steps name; a client's button shows its domain)
// and Minutes (about how long it takes). It belongs to the person the ask
// is for, any member of the project, on the card's verify choice; only the
// card's verify check, run by zm ask check, closes it, never evidence the
// asker reports. Domain is the link's host (daemon).
type AskStep struct {
	Label   string   `json:"label"`
	Steps   []string `json:"steps"`
	Link    string   `json:"link"`
	Domain  string   `json:"domain,omitempty"`
	Minutes int      `json:"minutes,omitempty"`
}

// StepDoneRequest is POST /v0/asks/{id}/step: the person says they did the
// card's owner step (CardSHA the card they read). zm records it and asks the
// asker to run the check now; it closes nothing. The answer is the Ask.
type StepDoneRequest struct {
	CardSHA string `json:"card_sha256"`
}

// MaxVerifyCommand bounds a card's verify command, in bytes.
const MaxVerifyCommand = 1 << 10

// AskVerify is a card's check that the approved work is done. When the
// approver picks On (default the action's choice, else the first), the ask
// is approved, not done until the asker's run of Command (zm ask check)
// exits 0 after any action finished. Command is one shell line, run by the
// asker, as the asker; the card shows it word for word.
type AskVerify struct {
	Command string `json:"command"`
	On      string `json:"on,omitempty"`
}

// Action kinds (SPEC 16 "Asks that carry an action"). One action per card,
// on one choice; any other answer runs nothing.
const (
	// ActionAckMail acks a fixed list of mails as the approver.
	ActionAckMail = "ack-mail"
	// ActionRun starts, as the approver, a script run (Script, Ref, Commit,
	// Args) or an agent task (Agent, Brief) on Machine, a named machine of
	// the project.
	ActionRun = "run"
	// ActionBrowser follows Plan in a browser on Machine, within Scope. A
	// daemon refuses it until the project has a runtime that enforces the
	// scope.
	ActionBrowser = "browser"
	// ActionAuthorizeKey adds Key, one OpenSSH public key line, to User's
	// authorized_keys on Machine.
	ActionAuthorizeKey = "authorize-key"
	// ActionCommand runs Command, one shell command line exactly as the
	// card shows it, with /bin/sh on Machine as the approver, in a clean
	// checkout of Ref at Commit (a script run whose program is the line).
	ActionCommand = "command"
)

// Bounds on one action.
const (
	// MaxActionIDs bounds an ack-mail action's list.
	MaxActionIDs = 500
	// MaxActionBrief bounds a run action's agent brief, in bytes.
	MaxActionBrief = 8 << 10
	// MaxActionPlan bounds a browser action's plan, in bytes.
	MaxActionPlan = 4 << 10
	// MaxActionCommand bounds a command action's line, in bytes.
	MaxActionCommand = 2 << 10
)

// AskAction is a typed, bounded action an ask carries. The asker sends Kind,
// On and the kind's fields; the daemon checks them, freezes them and fills
// what the reader sees (asker values for those are ignored): Count and
// Samples for ack-mail, Command and BriefSHA for run (a command action's
// Command is the asker's, frozen verbatim), KeyComment and
// KeyFingerprint for authorize-key. Every field but Kind is omitted when
// empty, so a card stored before a field existed hashes the same.
type AskAction struct {
	Kind    string   `json:"kind"`          // one of the Action kinds
	IDs     []string `json:"ids,omitempty"` // ack-mail: the mails, full ids once stored
	On      string   `json:"on,omitempty"`  // the choice that runs it; default the first
	Count   int      `json:"count,omitempty"`
	Samples []string `json:"samples,omitempty"` // ack-mail: a few of the mails' first lines, for the reader

	// Machine is where run, browser and authorize-key act: a machine's name,
	// never "auto" (the approver approves a place).
	Machine string `json:"machine,omitempty"`
	// A run's script run: Script (a path in the repository) at Ref, which
	// pointed at Commit (the full commit id) when asked, with Args as literal
	// arguments. Command is the line as a person reads it (daemon); for a
	// command action it is the asker's line itself, run at Ref and Commit.
	Script  string   `json:"script,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Commit  string   `json:"commit,omitempty"`
	Args    []string `json:"args,omitempty"`
	Command string   `json:"command,omitempty"`
	// PinChecked: when the ask was made, zm saw Commit is what Ref
	// resolves to on the project's remote (daemon). Only then may the run
	// take Commit after Ref moved on (still on Ref, never its newer head);
	// otherwise Commit must still be Ref's head when it runs.
	PinChecked bool `json:"pin_checked,omitempty"`
	// A run's agent task: Agent (claude or codex) works from Brief (at most
	// MaxActionBrief bytes). BriefSHA is the brief's sha256 (daemon).
	Agent    string `json:"agent,omitempty"`
	Brief    string `json:"brief,omitempty"`
	BriefSHA string `json:"brief_sha256,omitempty"`
	// A browser action's numbered steps (at most MaxActionPlan bytes) and
	// what it may do.
	Plan  string        `json:"plan,omitempty"`
	Scope *BrowserScope `json:"scope,omitempty"`
	// An authorize-key action's account on Machine and the key line.
	// KeyComment and KeyFingerprint (SHA256:<base64>) are the daemon's.
	User           string `json:"user,omitempty"`
	Key            string `json:"key,omitempty"`
	KeyComment     string `json:"key_comment,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// MachinePerson is the person whose own machine Machine is, when it is
	// one person's (zm machine-class <machine> person <name>): the card is
	// addressed to them and only their signed answer counts (daemon; ""
	// for a team machine, which takes either owner's).
	MachinePerson string `json:"machine_person,omitempty"`
	// Risk is set when picking On cannot be undone (RiskIrreversible) or
	// spends money (RiskSpending), or when no judge checked it
	// (RiskUnchecked): any non-empty Risk means the answer that picks On
	// must carry ConfirmWord (DecideRequest.Confirm, KeyAnswer.Confirm).
	// The asker sets it (zm ask new --risk); the card judge may raise it,
	// never lower it. Frozen with the card (card_sha256 covers it).
	Risk string `json:"risk,omitempty"`
	// ConfirmWord is the word a person types to pick a risky action's
	// choice: Machine's name, else the project's slug (daemon).
	ConfirmWord string `json:"confirm_word,omitempty"`
}

// Action risks (AskAction.Risk).
const (
	RiskIrreversible = "irreversible"
	RiskSpending     = "spending"
	// RiskUnchecked: the card was released without the card judge's check,
	// so nobody knows: treated like the others.
	RiskUnchecked = "unchecked"
)

// BrowserScope is what a browser action may do: open only Origins, sign in
// with SignIn (a saved account's name, "" = none) and press final buttons
// (send, pay, delete) only when Submit.
type BrowserScope struct {
	Origins []string `json:"origins"`
	SignIn  string   `json:"sign_in,omitempty"`
	Submit  bool     `json:"submit,omitempty"`
}

// Action states (ActionState.State).
// ActionGaveUpPrefix starts the Reason of work its asker gave up: its task may
// still run, so a check may not close it.
const ActionGaveUpPrefix = "the asker gave up: "

const (
	ActionQueued  = "queued"  // approved; no machine took it yet
	ActionRunning = "running" // a machine took it
	ActionDone    = "done"
	ActionFailed  = "failed" // Reason says why, in plain words
	// ActionApproved: approved, not done. The card's verify check has not
	// passed yet (no action ran, or the action ran and the check is still
	// to pass).
	ActionApproved = "approved"
)

// ActionState is how an answered ask's action went: kept by the daemon
// from the decision on. ConfirmedBy and ConfirmedAt are set once an owner
// confirmed it (POST /v0/asks/{id}/action/confirm), which moves it out of the
// open asks.
type ActionState struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	Task        string     `json:"task,omitempty"` // the remote task's or machine op's id
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// The card's verify check: when it last ran, its exit status and last
	// lines, and when it passed.
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	CheckExit   *int       `json:"check_exit,omitempty"`
	CheckOutput string     `json:"check_output,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	// CheckedBy is who reported the last check (the check is reported,
	// not proven: zm records who said so).
	CheckedBy string `json:"checked_by,omitempty"`
	// RemindedAt is when zm reminded the asker it is not done yet;
	// EscalatedAt when it then told the approver.
	RemindedAt  *time.Time `json:"reminded_at,omitempty"`
	EscalatedAt *time.Time `json:"escalated_at,omitempty"`
	// RelanedAt is when zm handed the work to a fresh lane:
	// RelaneTask is that lane's task on RelaneMachine, else RelaneReason
	// says why none could start.
	RelanedAt     *time.Time `json:"relaned_at,omitempty"`
	RelaneTask    string     `json:"relane_task,omitempty"`
	RelaneMachine string     `json:"relane_machine,omitempty"`
	RelaneReason  string     `json:"relane_reason,omitempty"`
	// StepDoneAt is when the person last said they did the card's owner
	// step (POST /v0/asks/{id}/step), StepDoneBy who; only a check after it
	// shows whether it took.
	StepDoneAt *time.Time `json:"step_done_at,omitempty"`
	StepDoneBy string     `json:"step_done_by,omitempty"`
	// DoneEvidence: the asker or a project owner recorded the work done with
	// this evidence instead of a passing check (CheckedBy says who).
	DoneEvidence string `json:"done_evidence,omitempty"`
	// Retries is how many times the identical approved action was started
	// again (POST /v0/asks/{id}/action/retry). Retryable: it failed before
	// it started and may start again, by its asker or a project owner,
	// until RetryUntil (SPEC 16 "Retry").
	Retries    int        `json:"retries,omitempty"`
	Retryable  bool       `json:"retryable,omitempty"`
	RetryUntil *time.Time `json:"retry_until,omitempty"`
}

// ActionRetryRequest is POST /v0/asks/{id}/action/retry: start the
// identical approved action of the card the caller read (CardSHA) again,
// after it failed before it started. Nothing in it can change the action.
// The answer is the Ask.
type ActionRetryRequest struct {
	CardSHA string `json:"card_sha256"`
}

// MaxCheckOutput bounds the last lines a check reports, in bytes.
const MaxCheckOutput = 2 << 10

// CheckRequest is POST /v0/asks/{id}/check: the asker ran the card's verify
// command (CardSHA the card it read) and reports its exit (required), or
// gives up on the approved work (GiveUp, a plain reason, and no Exit), which
// fails it. The answer is the Ask.
type CheckRequest struct {
	CardSHA string `json:"card_sha256"`
	Exit    *int   `json:"exit,omitempty"`
	Output  string `json:"output,omitempty"`
	GiveUp  string `json:"give_up,omitempty"`
	// Evidence records approved work done without its check passing (a
	// check that cannot pass, say): one plain line saying what shows it is
	// done. Only the asker or a project owner; no exit, no give_up.
	Evidence string `json:"evidence,omitempty"`
}

// ActionConfirmRequest is POST /v0/asks/{id}/action/confirm: a project owner
// saw how the action went. It changes nothing but where the ask is listed,
// so it needs no signature. The answer is the Ask.
type ActionConfirmRequest struct{}

// AskFor marks an ask held for a person outside the team (a contact at another organisation):
// it waits until they are asked, grouped by Group, and an approver records
// either their own answer or what Contact said.
type AskFor struct {
	Group      string `json:"group"`                 // the outside party's name (their organisation)
	Contact    string `json:"contact"`               // who decides, as a person reads it
	Ref        string `json:"ref,omitempty"`         // the outside party's id elsewhere
	ContactRef string `json:"contact_ref,omitempty"` // the contact's id elsewhere
}

// CardJudgeWait is how long POST /v0/asks waits for the card judge's verdict
// before the ask is stored held. A member forwarding the ask waits longer
// than this for the authority's answer (internal/team ask_forward.go).
const CardJudgeWait = 40 * time.Second

// NewAskRequest is POST /v0/asks: a needs=human mail with a subject.
type NewAskRequest struct {
	To      string     `json:"to,omitempty"`   // "owners" (default) or this machine's human:<name>; routes the mail only
	Body    string     `json:"body,omitempty"` // the question; with a card, the card's text replaces it
	Area    string     `json:"area,omitempty"`
	Subject AskSubject `json:"subject"`
	// Options are the answers the device offers; default approve, decline.
	// A card's choices set them.
	Options []string `json:"options,omitempty"`
	// Card is required unless the subject is a pull request.
	Card     *AskCard   `json:"card,omitempty"`
	Priority *int       `json:"priority,omitempty"` // 0 (first) to 4; default 2
	Topic    string     `json:"topic,omitempty"`    // one ask per subject and topic, or group and topic
	For      *AskFor    `json:"for,omitempty"`
	By       *time.Time `json:"by,omitempty"` // when the asker needs it (orders the inbox); never with For
	// Owner is the person the ask is for: an identity's name or alias at the
	// authority. Default: the person the work item's owner:<name> label names.
	Owner string `json:"owner,omitempty"`
	// SameAs joins an open card ask with the same question: this ask takes
	// its card, priority and owner, and the owner's answer to either decides
	// both. An answered one is 409 carrying it (its answer is the answer).
	SameAs string `json:"same_as,omitempty"`
	// ApplyDefault: at By, unless a person answered, zm applies the card's
	// recommended choice as DEFAULT APPLIED (never an owner decision) and
	// wakes the asker. Needs a card with recommend and By; refused when the
	// ask or its work item names an irreversible step.
	ApplyDefault bool `json:"apply_default,omitempty"`
	// Attachments are ids of live attachments on the subject work item that
	// the card shows (zm ask new --attach); the work item is required.
	Attachments []string `json:"attachments,omitempty"`
	// ThreadTitle and PRURL tell a client what the ask came from: the asking
	// thread's title and a pull request's https URL. Display fields beside
	// the card, never part of its text or its signed hash; a note card's ask only.
	ThreadTitle string `json:"thread_title,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	// RepeatKey names the thing the card is about that can break again (an
	// automation's id: syntax, never read for meaning). While an open card of
	// the project, in the same area and namespace, carries the key, this ask
	// files nothing: that card counts it (Repeat.Seen, LastSeen) and shows
	// Latest, and the answer is that card. A note card's ask only.
	RepeatKey string `json:"repeat_key,omitempty"`
	// Latest is the newest one-line state of the broken thing ("the deploy
	// still cannot sign in"), shown with the count; plain words, like the card.
	Latest string `json:"latest,omitempty"`
}

// AskRepeat is how often a card's broken thing came back while the card was
// open (NewAskRequest.RepeatKey): Seen counts the first ask too.
type AskRepeat struct {
	Key      string    `json:"key"`
	Seen     int       `json:"seen"`
	LastSeen time.Time `json:"last_seen"`
	Latest   string    `json:"latest,omitempty"`
}

// AskAttachment is an attachment the ask's card shows: the item's
// IssueAttachment without its attacher. Its bytes are read through the work
// item (GET /v0/issues/{id}/attachments/{aid}).
type AskAttachment struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// Ask is one question for an approver. Its id is the id of the needs=human
// mail that carries it.
type Ask struct {
	ID       string     `json:"id"`
	Project  string     `json:"project"`
	Area     string     `json:"area,omitempty"`
	Subject  AskSubject `json:"subject"`
	Options  []string   `json:"options"`
	Body     string     `json:"body"`
	AskedBy  string     `json:"asked_by"`
	To       string     `json:"to"`
	Created  time.Time  `json:"created"`
	Decision *Decision  `json:"decision,omitempty"`
	// Status is open, answered (decided, not yet where it belongs), recorded
	// (the answer reached the work item, or the asker when there is none) or
	// withdrawn.
	Status string   `json:"status,omitempty"`
	Card   *AskCard `json:"card,omitempty"`
	// Attachments are the images and HTML mockups the card shows, kept on
	// Subject.Work; each expires on its own.
	Attachments []AskAttachment `json:"attachments,omitempty"`
	CardSHA     string          `json:"card_sha256,omitempty"` // what a key approver signs and a receipt binds
	Priority    int             `json:"priority"`
	Topic       string          `json:"topic,omitempty"`
	For         *AskFor         `json:"for,omitempty"`
	By          *time.Time      `json:"by,omitempty"`
	Withdrawn   *time.Time      `json:"withdrawn,omitempty"`
	// Owner is the person it is for ("" = the project's owners); SameAs the
	// first ask of its group ("" = it is the first).
	Owner  string `json:"owner,omitempty"`
	SameAs string `json:"same_as,omitempty"`
	// Linked is an open ask the card judge found asking the same question
	// with a different action, so the two could not be joined (shown only to
	// who may operate on the area).
	Linked string `json:"linked,omitempty"`
	// OwnerPerson is Owner's stable person id (as GET /v0/people's Person.ID):
	// asks to any of one person's names, aliases or GitHub login carry the
	// same one. AskerHuman is the name of the person whose token or machine
	// asked. Both are hidden from an area member.
	OwnerPerson string `json:"owner_person,omitempty"`
	AskerHuman  string `json:"asker_human,omitempty"`
	// WorkTitle is the title of Subject.Work as it read when the ask was made;
	// ThreadTitle and PRURL are what the asker sent (NewAskRequest). Clients
	// render them as the "About" line and as links.
	WorkTitle   string `json:"work_title,omitempty"`
	ThreadTitle string `json:"thread_title,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	// ReceiptKey is the receipt key id of the daemon that decides this ask (its
	// project's authority): what a key approver signs as KeyAnswer.Daemon.
	ReceiptKey string `json:"receipt_key,omitempty"`
	// Note says the asker is past the daily target (POST only; never a refusal).
	Note string `json:"note,omitempty"`
	// OwnerVia says whom to "me" named and why (POST only).
	OwnerVia string `json:"owner_via,omitempty"`
	// ApplyDefault: the recommended choice applies at By unless a person
	// answers first. DefaultApplied is when it did (DEFAULT APPLIED: the ask
	// stays open, so the person may still Undo or answer); DefaultUndone when
	// the person took it back, and DefaultUndoneBy who.
	ApplyDefault    bool       `json:"apply_default,omitempty"`
	DefaultApplied  *time.Time `json:"default_applied,omitempty"`
	DefaultUndone   *time.Time `json:"default_undone,omitempty"`
	DefaultUndoneBy string     `json:"default_undone_by,omitempty"`
	// ActionText is the card's action as the approver reads it, exactly what
	// runs ("If you choose A, zm adds this key ..."); ActionState how it went
	// once answered, and ActionStateText that state in plain words. All three
	// are the daemon's: a client adds no wording of its own about what runs.
	ActionText      string       `json:"action_text,omitempty"`
	ActionState     *ActionState `json:"action_state,omitempty"`
	ActionStateText string       `json:"action_state_text,omitempty"`
	// NotDone: the approver approved work that is not done yet (an action
	// queued or running, or a verify check that has not passed). Decisions
	// lists these on their own; ActionStateText says what is left.
	NotDone bool `json:"not_done,omitempty"`
	// Whose turn it is, derived from the fields above (SPEC 16 "Whose
	// turn"), one state at a time. RanUnchecked: its action ran to done and
	// nothing checked the result (no verify, no evidence), and no owner
	// confirmed or objected yet. YourStep: the approved work waits on a step
	// only the person it is for can do. Next: NextYou (the person it is
	// for), NextAgent (its asker, a fresh lane or zm) or NextNobody.
	RanUnchecked bool   `json:"ran_unchecked,omitempty"`
	YourStep     bool   `json:"your_step,omitempty"`
	Next         string `json:"next,omitempty"`
	// Repeat: the card is keyed to a thing that can break again, and how
	// often it did while the card was open (NewAskRequest.RepeatKey).
	Repeat *AskRepeat `json:"repeat,omitempty"`
	// Updated: this POST filed nothing; it counted a repeat on this open card.
	Updated bool `json:"updated,omitempty"`
	// NotChecked: the card judge could not check this card (its model login
	// is over its limit, down or splitting on it), so it is held: it reaches
	// nobody it is for and cannot be answered. Decisions lists it for the
	// project's owners at once, with the deadline first; NotCheckedReason is
	// why the judge did not answer and NotCheckedSince when it was first
	// held. An owner releases it with DecisionReleaseUnchecked, a signed
	// answer (paired device or key approver).
	NotChecked       bool       `json:"not_checked,omitempty"`
	NotCheckedReason string     `json:"not_checked_reason,omitempty"`
	NotCheckedSince  *time.Time `json:"not_checked_since,omitempty"`
	// ReleasedUnchecked names the approver who released this card without
	// the judge's check; a person who answers it reads that, and its default
	// never applies by itself.
	ReleasedUnchecked string `json:"released_unchecked,omitempty"`
	// Labels are what a judge said about the card (SetLabels): LabelKind is
	// its kind, LabelRepeatOf the open ask it repeats. Empty until a judge has
	// labelled it; the measures show "unlabelled" rather than guess.
	Labels map[string]string `json:"labels,omitempty"`
	// Objection is an owner's later press against an answered card: the
	// harm record the measures count (POST /v0/asks/{id}/object).
	Objection *AskObjection `json:"objection,omitempty"`
}

// Label keys a judge sets on a card (Ask.Labels). Other keys are allowed.
const (
	LabelKind     = "kind"      // what sort of card it is, e.g. "lane-start"
	LabelRepeatOf = "repeat_of" // id of the open card this one repeats
)

// AskObjection is an owner saying, after the answer, that the action it set
// off should not have happened or should be undone.
type AskObjection struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Note string    `json:"note,omitempty"`
}

// ObjectRequest is POST /v0/asks/{id}/object. The answer is the Ask.
type ObjectRequest struct {
	Note string `json:"note,omitempty"` // why, in a line; optional
}

// MaxObjectionNote bounds an objection's note, in bytes.
const MaxObjectionNote = 500

// Whose turn an ask is (Ask.Next).
const (
	NextYou    = "you"
	NextAgent  = "agent"
	NextNobody = "nobody"
)

// Ask statuses.
const (
	AskOpen      = "open"
	AskAnswered  = "answered"
	AskRecorded  = "recorded"
	AskWithdrawn = "withdrawn"
)

// AskList is GET /v0/asks.
type AskList struct {
	Asks []Ask `json:"asks"`
	// Next is the cursor for the next, older page (pass it as before_id):
	// set only when this page was full, so more may follow.
	Next string `json:"next,omitempty"`
	// Skipped is a team replica's merged listing (as WorkList's): an authority
	// that did not answer.
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
}

// DecideRequest is POST /v0/asks/{id}/decide (from an approver device).
type DecideRequest struct {
	Decision string `json:"decision"`
	// Text is a free-text answer, alone (decision other) or beside a choice.
	// Never on a pull request's ask: its receipt does not carry one.
	Text string `json:"text,omitempty"`
	// As is owner (default) or, on an ask with For, contact.
	As string `json:"as,omitempty"`
	// Confirm is the word typed to pick a risky action's choice
	// (AskAction.ConfirmWord); without it that choice is a 409, reason
	// ReasonConfirmWord.
	Confirm string `json:"confirm,omitempty"`
	// CardSHA is the Ask.CardSHA the person read (optional): when it is not the
	// ask's current card the answer is a 409, reason ReasonCardChanged, and
	// nothing is recorded. Absent: the answer is for whatever card stands.
	CardSHA string `json:"card_sha256,omitempty"`
}

// KeyAnswerVersion is a key approver's signed answer format and domain.
const KeyAnswerVersion = "zm-answer/1"

// KeyAnswer is the exact JSON a key approver signs. The signature covers the
// bytes sent, so no side canonicalises anything.
type KeyAnswer struct {
	V        string `json:"v"`
	Daemon   string `json:"daemon"`      // the deciding daemon's receipt key id (GET /v0/receipts/key)
	Project  string `json:"project"`     // the ask's project ULID
	Ask      string `json:"ask"`         // the ask's id
	CardSHA  string `json:"card_sha256"` // the Ask.CardSHA the person read
	Decision string `json:"decision"`
	Text     string `json:"text,omitempty"`
	As       string `json:"as,omitempty"`
	Confirm  string `json:"confirm,omitempty"` // DecideRequest.Confirm
	Time     string `json:"time"`              // RFC 3339; accepted within 5 minutes of the daemon's clock
}

// SignedKeyAnswer is POST /v0/asks/{id}/answer.
type SignedKeyAnswer struct {
	Approver string `json:"approver"`
	Payload  string `json:"payload"` // base64 (std) of a KeyAnswer's JSON
	// Sig is base64 (std) of an ASN.1 ECDSA SHA-256 signature over
	// KeyAnswerVersion + "\n" + the payload's decoded bytes.
	Sig string `json:"sig"`
}

// InboxItem is one row of GET /v0/decisions: an ask, or a work item labelled
// as waiting on a person that has no open ask.
// DevicePushRequest is POST /v0/approvals/push: a paired device's Web Push
// subscription (the browser's PushSubscription JSON).
type DevicePushRequest struct {
	Endpoint string `json:"endpoint"`
	// Browsers always include it (usually null); accepted and ignored.
	ExpirationTime *float64 `json:"expirationTime,omitempty"`
	Keys           struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// GotItRequest is POST /v0/approvals/got-it: the device's Got it on one
// for-you mail (SPEC 9 "For you").
type GotItRequest struct {
	ID string `json:"id"`
}

type InboxItem struct {
	Kind        string    `json:"kind"` // "ask" or "work"
	ProjectSlug string    `json:"project_slug"`
	Priority    int       `json:"priority"`
	Ask         *Ask      `json:"ask,omitempty"`
	Work        *WorkItem `json:"work,omitempty"`
	// OwnerName is who an owner:<name> label names ("" = anyone).
	OwnerName string `json:"owner_name,omitempty"`
}

// PersonLinkRequest is POST /v0/person-links: account Identity belongs to
// the person whose main account is Person. ShareMail, set
// only by an account itself, turns on or off whether mail to the caller's
// own account also reaches the link's other account (private mail only).
type PersonLinkRequest struct {
	Identity  string `json:"identity"`
	Person    string `json:"person"`
	ShareMail *bool  `json:"share_mail,omitempty"`
}

// PersonLinks is GET /v0/person-links (and what POST and DELETE answer):
// the links the caller is a party to or administers.
type PersonLinks struct {
	Links []PersonLink `json:"links"`
}

// PersonLink is one account linked into a person. Live: it folds (the
// person agreed, and so did the account or an admin). ByAdmin is who recorded
// it as an admin (an address), BySelf and ByPerson each account's own say.
// SharesMail and PersonSharesMail: each account's own mail-sharing switch.
type PersonLink struct {
	Identity         string `json:"identity"`
	Person           string `json:"person"`
	Live             bool   `json:"live"`
	ByAdmin          string `json:"by_admin,omitempty"`
	BySelf           bool   `json:"by_self,omitempty"`
	ByPerson         bool   `json:"by_person,omitempty"`
	SharesMail       bool   `json:"shares_mail,omitempty"`
	PersonSharesMail bool   `json:"person_shares_mail,omitempty"`
}

// AliasRequest is POST /v0/aliases: Alias becomes one of Identity's inbox
// names ("" = the caller's own identity).
type AliasRequest struct {
	Alias    string `json:"alias"`
	Identity string `json:"identity,omitempty"`
}

// Aliases is GET /v0/aliases (and what POST and DELETE answer): the label
// names besides its own that Identity answers to in owner:<name>,
// ask:<name> and needs-<name>.
type Aliases struct {
	Identity string   `json:"identity"`
	Aliases  []string `json:"aliases"`
	// AddedBy names who set each alias (an address).
	AddedBy map[string]string `json:"added_by,omitempty"`
}

// Inbox is GET /v0/decisions. Skipped and Behind mean what they mean in
// WorkList: a partial inbox never looks complete.
type Inbox struct {
	Decisions []InboxItem       `json:"decisions"`
	Skipped   []api.WorkSkipped `json:"skipped,omitempty"`
	Behind    []WorkBehind      `json:"behind,omitempty"`
}

// Decision is the one answer an ask gets, with its signed receipt.
// DecisionDelivery exposes progress without revealing internal effect IDs or recipients.
type DecisionDelivery struct {
	Kind  string `json:"kind"`  // comment, reply, holder, event
	State string `json:"state"` // pending, succeeded, failed, unknown
}

type Decision struct {
	Deliveries []DecisionDelivery `json:"deliveries,omitempty"`
	Ask        string             `json:"ask"`
	Decision   string             `json:"decision"`
	Approver   string             `json:"approver"`
	// ApproverPerson is the stable person id (as GET /v0/people's
	// Person.ID) of whoever answered, shown to the readers Ask.OwnerPerson is
	// shown to: equal to the ask's owner_person when the person it was for
	// answered from any of their accounts.
	ApproverPerson string `json:"approver_person,omitempty"`
	// NotTheirs is the person the card was theirs to decide, set exactly
	// when the delivered answer says they did not give it (another owner,
	// a device paired for no one, or an answerer zm could not check). On a
	// card for every owner it is the asker's human, shown only where
	// Ask.AskerHuman is (not in the event).
	NotTheirs string `json:"not_theirs,omitempty"`
	// Method is how it was decided: owner (Machine names the owner's
	// machine) or approver (Device names the paired device).
	Method  string `json:"method,omitempty"`
	Machine string `json:"machine,omitempty"`
	Device  string `json:"device,omitempty"`
	// Surface is where it was answered (Surface* constants); Machine is
	// also set for a key's answer (the machine its key was bound on).
	// Account is the provider account of whoever answered, the value of
	// record: a sign-in-bound key's own, else the answerer's verified link
	// at answer time; Login is that account's login then. All come from
	// the approver row and the caller, never from a signed body.
	Surface   string    `json:"surface,omitempty"`
	Account   string    `json:"account,omitempty"`
	Login     string    `json:"login,omitempty"`
	Time      time.Time `json:"time"`
	Receipt   Receipt   `json:"receipt"`
	MailError string    `json:"mail_error,omitempty"`
	// Text and AnsweredAs: see DecideRequest. Answer is the answer as read:
	// "B) Keep last year's swaps [only these three]".
	Text       string `json:"text,omitempty"`
	AnsweredAs string `json:"answered_as,omitempty"`
	Answer     string `json:"answer,omitempty"`
	// RecordedAt: all required durable effects succeeded. Deliveries distinguishes
	// work recording from notification delivery. Unknown outcomes need reconciliation.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
	WriteError string     `json:"write_error,omitempty"`
}

// ReceiptVersion is the receipt format and the signature's domain separator.
const ReceiptVersion = "zm-receipt/1"

// ReceiptVersion2 is the format of a decision on anything but a pull request:
// the signature also covers what the person read and answered. A verifier
// built for version 1 refuses it, as a merge gate must.
const ReceiptVersion2 = "zm-receipt/2"

// Receipt is a decision signed by the daemon's ed25519 key. The signature
// covers every field but Sig (and KeyID only names the key).
type Receipt struct {
	V        string     `json:"v"`       // ReceiptVersion
	Project  string     `json:"project"` // project ULID
	Ask      string     `json:"ask"`     // the ask (mail) id
	Subject  AskSubject `json:"subject"`
	Decision string     `json:"decision"`
	Approver string     `json:"approver"`
	Node     string     `json:"node"` // the approver device's stable node ID, key:<fingerprint>, or owner:<machine>
	Time     string     `json:"time"` // RFC 3339 UTC, milliseconds
	// Version 2 only. Hashes, so a redacted card or answer leaves the receipt whole.
	CardSHA    string `json:"card_sha256,omitempty"`
	TextSHA    string `json:"text_sha256,omitempty"`
	AnsweredAs string `json:"answered_as,omitempty"`
	// Action names the card's action, when it carries one (version 2
	// only): for ack-mail its kind, count, the sha256 of its ids and the
	// choice that runs it; for any other kind "<kind> sha256=<the frozen
	// action's JSON> on=<choice>".
	Action string `json:"action,omitempty"`
	KeyID  string `json:"key_id"`
	Sig    string `json:"sig"` // base64 (std) ed25519 signature
}

// ReceiptKey is GET /v0/receipts/key: the public half of the daemon's
// receipt key, for offline verification (e.g. a CI gate).
type ReceiptKey struct {
	Alg       string `json:"alg"` // "ed25519"
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"` // base64 (std), 32 bytes
	// Line is the one-line form `zm receipt verify --key` takes: ed25519:<base64>.
	Line string `json:"line"`
}

// ReceiptExpect is what a verifier requires of a receipt. Empty fields are
// not checked; the CLI requires repo, pr and head unless --signature-only.
type ReceiptExpect struct {
	Project  string `json:"project,omitempty"` // ULID (or a slug the caller can see, on the daemon)
	Ask      string `json:"ask,omitempty"`
	Repo     string `json:"repo,omitempty"`
	PR       int    `json:"pr,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Decision string `json:"decision,omitempty"`
	Approver string `json:"approver,omitempty"`
	Work     string `json:"work,omitempty"`
	Question string `json:"question,omitempty"`
	Card     string `json:"card,omitempty"` // the card_sha256 the receipt must bind
	// Signed requires a signed answer: a paired device's or a key
	// approver's, never the owner's unsigned one (a node starting
	// OwnerNodePrefix: the socket, an owner token, an answer-only token).
	Signed bool `json:"signed,omitempty"`
}

// ReceiptCheck is GET /v0/receipts/verify. Valid is false (with Reason) for
// anything that does not verify: bad signature, other key, wrong project,
// ask, repo, PR, head SHA, work item or question, or a malformed receipt.
type ReceiptCheck struct {
	Valid   bool     `json:"valid"`
	Reason  string   `json:"reason,omitempty"`
	KeyID   string   `json:"key_id,omitempty"`
	Checked []string `json:"checked,omitempty"` // the expectations that were compared
}

// Approvals event types. Approver events are machine-level (owner only);
// decision.recorded is in the ask's project and area, for readers who may
// operate there or are a party to the ask.
const (
	EventApproverAdded    = "dev.zm.approver.added"      // data Approver (now active)
	EventApproverRemoved  = "dev.zm.approver.removed"    // data Approver
	EventDecisionRecorded = "dev.zm.decision.recorded"   // data Decision
	EventAskCreated       = "dev.zm.ask.created"         // data Ask (audience as decision.recorded)
	EventDefaultApplied   = "dev.zm.ask.default_applied" // data Ask (audience as decision.recorded)
	EventDefaultUndone    = "dev.zm.ask.default_undone"  // data Ask (audience as decision.recorded)
	EventAskRepeated      = "dev.zm.ask.repeated"        // data Ask: an open card counted a repeat (as decision.recorded)
)

// UnknownField reports a daemon that does not know a request field yet (the
// CLI was upgraded before the daemon restarted): its 400 names the field.
func UnknownField(err error, field string) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == api.CodeBadRequest && strings.Contains(e.Message, `unknown field "`+field+`"`)
}
