package orchcore

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Seams between zm's modules (session, mail, team, approvals, issues, web)
// that potluck itself never calls, moved from potluck's kit/core/core.go
// unchanged (recipe step 2).

// IssueService is the issues module's in-process API for the work and homes
// modules: native issues served as a work adapter without an executable
// (trackers bound with adapter "native"), and the copies and restores of a
// native store (SPEC 17.9).
type IssueService interface {
	// NativeDescribe is the native adapter's describe answer.
	NativeDescribe() orchapi.AdapterDescribe
	// NativeCall runs one adapter verb on project's native issues and
	// decodes the result into out. Writes act as the caller in ctx
	// (SubjectOf), with the native authz rules; reads see every issue (the
	// work module scopes them).
	NativeCall(ctx context.Context, project string, req orchapi.AdapterRequest, out any) error
	// NativeVersion is the id of project's last native change ("" = none).
	NativeVersion(ctx context.Context, project string) (string, error)
	// HoldWrites refuses every native write in project (conflict, naming
	// why) until release is called: a restore or an issue move holds it so
	// no write lands between its checks and its swap.
	HoldWrites(project, why string) (release func(), err error)
	// SnapshotNative writes a checked copy of project's native store to dst
	// and returns its size. prev is the last good copy's (sqlbackup.Plausible).
	SnapshotNative(ctx context.Context, project, dst string, prev int64) (int64, error)
	// RestoreNative makes the checked copy at path project's native store,
	// with the redaction list applied to it first. The caller holds
	// HoldWrites. dry does all of that but the replacing.
	RestoreNative(ctx context.Context, project, path string, dry bool) (orchapi.IssueRestoreResult, error)
	// MovedTo follows id's redirect chain (zm issues move): the id it ended
	// as and that issue's project, and the project id moved out of. ok is
	// false when id did not move. It knows nothing of who is asking: the
	// caller scopes what it does with the answer.
	MovedTo(ctx context.Context, id string) (newID, from, to string, ok bool, err error)
	// MovedOut checks this project's own redirect, without following it.
	MovedOut(ctx context.Context, project, id string) (bool, error)
}

// SessionLookupErr is an optional SessionService method: Lookup telling a
// missing session (false, nil) from a store that did not answer (an error).
// The lease ticker keeps a lease through an error rather than drop it.
type SessionLookupErr interface {
	LookupErr(ctx context.Context, address string) (orchapi.Session, bool, error)
}

// LookupSession is ss.LookupErr when ss has it, else Lookup with no error.
func LookupSession(ctx context.Context, ss SessionService, address string) (orchapi.Session, bool, error) {
	if le, ok := ss.(SessionLookupErr); ok {
		return le.LookupErr(ctx, address)
	}
	sess, ok := ss.Lookup(ctx, address)
	return sess, ok, nil
}

// TeamLeases is implemented by the team module on a replica (Services.Team): a
// managed session started here in a joined project takes its bound lease at
// the project's authority (spawn --work, SPEC 10). BindAt claims
// work item id in project (the authority's ULID) for session as that
// session's own claim, answered by the authority: CodeUnavailable when it
// cannot answer in time, and then nothing stays queued to claim it later.
// ReleaseAt gives it back for a session whose start failed after BindAt (the
// authority does not drop a default-TTL lease of an ended session); queued
// while the authority is away, since it only ever drops that session's own.
type TeamLeases interface {
	BindAt(ctx context.Context, sub *authz.Subject, project, id, session string) (orchapi.Lease, error)
	ReleaseAt(ctx context.Context, project, id, session string) error
}

// PresenceStore is implemented by the session module: on a team project's
// authority it keeps each replica's sessions (presence), so Lookup, Visible,
// Live and List answer for them too. RecordPresence stores machine's
// heartbeat for project: the sessions listed are seen now; the machine's
// other sessions in project are ended. Callers check every address is on
// machine and every area is one the replica may write in.
type PresenceStore interface {
	RecordPresence(ctx context.Context, project, machine, identity string, ss []orchapi.PresenceSession) error
}

// SessionOrigins is implemented by the session module: who started a
// session, read as the daemon (no reader filter). Launcher is its identity
// (a ULID for this machine's sessions, a name for a replica's); Parent is
// the session on the same machine that started it (SPEC 8 "Parents").
type SessionOrigins interface {
	Origin(ctx context.Context, address string) (launcher, parent string, ok bool)
}

// TaskParents is implemented by the team module (Services.Team) on a
// project's authority: the parent session, on another machine, of the
// remote task that runs as session ("" when it is none).
type TaskParents interface {
	TaskParent(ctx context.Context, session string) string
}

// PresenceElsewhere is implemented by the session module (owner): on a team
// authority, the replicas' live sessions registered outside a project
// (another project or none), as each heartbeat for the project lists them
// (SPEC 9 "Across projects"), so mail in the project can reach them. In
// memory: a heartbeat replaces its machine's set for the project.
type PresenceElsewhere interface {
	// RecordElsewhere stores machine's set for project, listed by the
	// replica's token (id), which read whole or areas of project then.
	// Callers check every address is a session on machine.
	RecordElsewhere(ctx context.Context, project, machine, token string, addrs []string, whole bool, areas []string)
	// Elsewhere reports what a heartbeat for project said of address in the
	// last 7 days; ok=false when none listed it.
	Elsewhere(project, address string) (ElsewhereSession, bool)
}

// ElsewhereSession is a replica's session registered outside a project, as
// the authority of that project knows it.
type ElsewhereSession struct {
	// Live: the machine's latest heartbeat for the project, still fresh,
	// listed it.
	Live bool
	// Token is the id of the replica's token that listed it: what the
	// replica reads of the project now is that token's scope. Whole and
	// Areas are what it read at the heartbeat (for visibility).
	Token string
	Whole bool
	Areas []string
}

// ElsewhereShared is who may see and address a replica's session listed
// outside project, the replica reading whole or areas of it (SPEC 9 "Across
// projects"): a reader of the whole project (all), or, when the replica is
// a guest's (some areas), a reader of those areas, which are shared (mail
// to it is filed in one of them). Anyone else: neither.
func ElsewhereShared(sc authz.Scope, project string, whole bool, areas []string) (all bool, shared []string) {
	if sc.WholeProject(project) {
		return true, nil
	}
	if whole {
		return false, nil
	}
	for _, a := range areas {
		if sc.Can(authz.Read, authz.Object{Project: project, Area: a}) {
			shared = append(shared, a)
		}
	}
	return false, shared
}

// SharedPrompts is implemented by the session module (owner), for shared
// sessions (SPEC 17.10): on an authority, the prompts pending for a
// replica's session and marking those the replica typed; on a replica,
// typing a prompt the authority sent into a local session. The team module
// calls it from presence. MarkDelivered marks only prompts of project's
// session address, and only when address is on machine (the replica whose
// heartbeat carried the acks). DeliverPrompt's typed reports whether any of
// the prompt may have reached the terminal (false: refused before typing,
// safe to try again).
type SharedPrompts interface {
	PendingPrompts(ctx context.Context, address string) []orchapi.SessionPrompt
	MarkDelivered(ctx context.Context, project, machine, address string, ids []string)
	DeliverPrompt(ctx context.Context, address string, p orchapi.SessionPrompt) (typed bool, err error)
}

// TeamMail is implemented by the mail module: sub's unread mail as notice
// parts (one per project, what Notice counts), and how many needs=human
// mails sender sent in project that are still open.
type TeamMail interface {
	NoticeParts(ctx context.Context, sub *authz.Subject) ([]orchapi.NoticePart, error)
	OpenAsks(ctx context.Context, project, sender string) (int, error)
}

// TeamReplica is implemented by the team module on a replica (Services.Team).
// Neither method touches the network: hooks stay local and fast.
type TeamReplica interface {
	// TeamNotice is address's unread team mail as of its last presence
	// answer.
	TeamNotice(address string) []orchapi.NoticePart
	// TeamTarget is the joined team project (and area) a session started
	// or registered from this request belongs to, by the rules the proxy
	// uses for writes (an explicit project, a .zm/project.toml naming a
	// joined slug, a recorded checkout); ok=false: none, use local rules.
	TeamTarget(ctx context.Context, cwd, area string) (p api.ProjectRef, a string, ok bool, err error)
}

// LegacyMailImporter is the mail-owned bounded recovery seam. Its caller
// must be the authenticated local owner or an aware node-bound canonical human
// importing to self through an explicit joined project; never a sender override.
type LegacyMailImporter interface {
	ImportLegacy(context.Context, *authz.Subject, api.ProjectRef, string, orchapi.LegacyMailImportRequest) (orchapi.LegacyMailImportResult, error)
}

// MailActions is implemented by the mail module: the mail side of an ask
// that carries an ack-mail action (SPEC 16 "Asks that carry an action").
type MailActions interface {
	// AckPreview resolves ids to the full ids of mails in project that sub
	// can see and that an ack would take, with a few of their first lines.
	// Any missing, hidden, other-project, announcement or open-ask mail fails
	// the whole call (hidden answers like missing).
	AckPreview(ctx context.Context, sub *authz.Subject, project string, ids []string) (full, samples []string, err error)
	// AckApproved takes exactly ids (full ids) in project as taker, the
	// approver of a decided ask. It skips the needs=human rule: the decision
	// is the human's ack. Mail already taken, or gone, is skipped, so a retry
	// or a second delivery acks nothing twice. Never reachable over HTTP.
	AckApproved(ctx context.Context, project, taker string, ids []string) (acked int, err error)
}

// ForYouMail is implemented by the mail module: mail addressed to a person
// is one item in that person's Decisions (SPEC 9 "For you"), and only their
// own action there marks it read.
type ForYouMail interface {
	// ForYou lists sub's own unread for-you mail (addressed to sub's human
	// address), newest first, at most limit; a digest item only once due.
	ForYou(ctx context.Context, sub *authz.Subject, limit int) ([]orchapi.Mail, error)
	// GotIt marks for-you mail id read as sub, who must be the person it is
	// addressed to: never a session, and mail sub cannot see is not_found.
	GotIt(ctx context.Context, sub *authz.Subject, id string) (orchapi.AckResponse, error)
}

// TeamForYou is implemented by the team module (Services.Team): this machine's
// owner's for-you mail in the projects it joined, which lives at their
// authorities, for a device paired here. A joined project's
// node token speaks only for the owner, so any other person gets none and
// their Got it is not_found.
type TeamForYou interface {
	// TeamForYou lists person's unread for-you mail live from each joined
	// project's authority, newest first, at most limit; skipped names a
	// project whose authority did not answer.
	TeamForYou(ctx context.Context, person *authz.Subject, limit int) (ms []orchapi.Mail, skipped []api.WorkSkipped)
	// TeamGotIt marks for-you mail id read as person at the joined
	// project's authority holding it; not_found when none does.
	TeamGotIt(ctx context.Context, person *authz.Subject, id string) (orchapi.AckResponse, error)
}

// DevicePush is implemented by the web module: Web Push for a phone paired
// as an approver (SPEC 16 "Phone alerts"). The subscription belongs to the
// device's person and lasts while the device stays an active approver.
type DevicePush interface {
	PushPublicKey(ctx context.Context) (string, error)
	SubscribeDevice(ctx context.Context, person *authz.Subject, node string, req orchapi.DevicePushRequest) error
	UnsubscribeDevice(ctx context.Context, node, endpoint string) error
}

// PushMail reports whether a mail is still an open human decision for a
// subscriber. Implemented by mail without exposing its tables to webui.
type PushMail interface {
	PushOpen(ctx context.Context, sub *authz.Subject, id string) (bool, error)
}

// Noticer is implemented by the mail module next to MailService: sub's
// notice line across every project it sees, exactly like GET
// /v0/mail/notice. The session module's hook route (POST /v0/sessions/hook)
// returns it so an agent's prompt hook is one round trip.
type Noticer interface {
	Notice(ctx context.Context, sub *authz.Subject) (orchapi.Notice, error)
}

// HookNoticer announces fresh mail once per session across all agent surfaces.
// Mail owns the delivery records; a notice never acknowledges an inbox item.
type HookNoticer interface {
	HookNotice(ctx context.Context, sub *authz.Subject) (orchapi.Notice, error)
}

// MailNotice is already scoped/redacted mailbox metadata, never mail content.
type MailNotice struct {
	Project, ID, From, Ref string
	Created                time.Time
	Group                  bool
}

// TeamMailNotices supplies local replica metadata for exact hook deduplication,
// and cached aggregate parts only for peers whose copy is not ready yet.
type TeamMailNotices interface {
	TeamMailNotices(ctx context.Context, address string) ([]MailNotice, []orchapi.NoticePart)
}

// TaskRelay is the team module's task-notification seam. It reports handled
// only for a joined project and keeps queued writes distinct from completed ones.
type TaskRelay interface {
	DeliverTask(ctx context.Context, sub *authz.Subject, project, area, kind, to, work, body string) (handled bool, err error)
}

// RemoteTaskRunner is implemented by the session module: what the team
// module runs for a task op an authority queued for this machine, and the
// machines an authority chooses among (SPEC 8 "Machine choice"). The ID
// passed to Status, Cancel and Read is the local task's; callers are the
// daemon itself (the op came from the project's authority).
type RemoteTaskRunner interface {
	StartRemoteTask(ctx context.Context, project, id string, op orchapi.TaskStartOp) (orchapi.Task, error)
	RemoteTaskStatus(ctx context.Context, id string) (orchapi.Task, error)
	CancelRemoteTask(ctx context.Context, id string) (orchapi.Task, error)
	ReadRemoteTask(ctx context.Context, id, item string, offset int) (orchapi.TaskHistoryPage, error)
	// ProjectMachines are the machines with numbers for project: this one
	// first (no Seen), then each replica's last report.
	ProjectMachines(ctx context.Context, project string) []api.Machine
}

// RemoteSessions is implemented by the team module: a structured session
// this machine's owner starts on another machine of a team project, through
// the project's authority (SPEC 8 "Sessions on other machines"). The answer
// is the session there, shared, once the authority has its presence row.
type RemoteSessions interface {
	StartSessionOn(ctx context.Context, sub *authz.Subject, project, area string, req orchapi.SpawnRequest) (orchapi.Session, error)
}

// RemoteSessionRunner is implemented by the session module: on the target
// machine, the session a session_start op its authority queued asks for. A
// repeat of the op answers the session it started.
type RemoteSessionRunner interface {
	StartRemoteSession(ctx context.Context, project, opID, from string, op orchapi.SessionStartOp) (orchapi.Session, error)
}

// RemoteTasks is implemented by the team module: a task the parent session
// starts on another machine of its team project, through the authority, and
// its status, cancel and read from the parent's machine (SPEC 8 "Machine
// choice"). sub is the caller with Session set to the parent ("" = this
// machine's owner, who may see every one).
type RemoteTasks interface {
	StartTaskOn(ctx context.Context, sub *authz.Subject, parent orchapi.Session, depth int, req orchapi.TaskStartRequest) (orchapi.Task, error)
	RemoteTask(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error)
	CancelTaskOn(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error)
	ReadTaskOn(ctx context.Context, sub *authz.Subject, id, item string, offset int) (orchapi.TaskHistoryPage, error)
}

// MachineT3 is implemented by the team module, for the session module's
// machine numbers (SPEC 8 "Mezze app on a machine"): whether this machine's owner
// set a t3 key ("t3": {} included), and the Mezze app server a t3_ensure op
// started here, while it answers on loopback (nil otherwise).
type MachineT3 interface {
	T3Enabled() bool
	T3Stats(ctx context.Context) *api.MachineT3
}

// SessionRelay is implemented by the team module: at a team
// project's authority, a shared structured session on another machine of
// the project, as its presence row ss shows it. The session module checks
// the caller first (SPEC 17.10's rules) and calls it. RelayEvents asks the
// session's machine for its events after seq (through its heartbeat) and
// waits up to wait for a page; RelayOp queues op for that machine (run at
// most once) and waits for its result, else answers Pending.
type SessionRelay interface {
	RelayEvents(ctx context.Context, ss orchapi.Session, after int64, limit int, wait time.Duration) (orchapi.SessionEvents, error)
	RelayOp(ctx context.Context, sub *authz.Subject, ss orchapi.Session, op orchapi.SessionOp) (orchapi.SessionOpResult, error)
}

// SessionPipe is implemented by the session module: on a shared structured
// session's own machine, what the authority relays to it.
// address must be a live (or, for events, any) shared structured session of
// this machine in project; the authority already checked the member asking.
type SessionPipe interface {
	PipeEvents(ctx context.Context, project, address string, after int64, limit int) (orchapi.SessionEvents, error)
	PipeOp(ctx context.Context, project, address string, op orchapi.SessionOp) (orchapi.SessionOpResult, error)
}

// SessionWorkRecorder is implemented by the session module: a session on
// this machine holds a lease on key, which becomes its work and stays after
// the release (orchapi.Session.Work). The team module calls it for claims a
// replica makes through its authority.
type SessionWorkRecorder interface {
	RecordWork(ctx context.Context, address, key string)
}

// OwnLocalSessionLister exposes only sessions launched by one local identity.
type OwnLocalSessionLister interface {
	ListOwnLocal(ctx context.Context, identity string) ([]orchapi.Session, error)
}

// AskActions is implemented by the team module: at a team project's
// authority, the machine an action ask names, and an approved ask's frozen
// action started as its approver (SPEC 16 "Asks that carry an action").
// The approvals module reads no team tables; it keeps the ask's action
// state from AskActionStatus.
type AskActions interface {
	// AskActionMachine is machine's latest numbers when it is a trusted
	// machine of team project that takes remote tasks (and runs scripts, when
	// scripts), else the refusal a person can act on.
	AskActionMachine(ctx context.Context, project, machine string, scripts bool) (api.MachineStats, error)
	// StartAskAction starts start as sub (the approver, by its own grants
	// now). start.ID is its request id: a repeat answers the first start's
	// id and never starts a second.
	StartAskAction(ctx context.Context, sub *authz.Subject, start AskActionStart) (string, error)
	// AskActionStatus is how the start id of project went.
	AskActionStatus(ctx context.Context, project, id string) (AskActionStatus, error)
	// AskActionPin checks a run's ref and commit when the ask is made:
	// checked when commit is what ref resolves to on the project's remote
	// now; false, with no error, when that cannot be told here (the run
	// machine decides after the yes); an error when it is definitely not.
	AskActionPin(ctx context.Context, project, ref, commit string) (checked bool, err error)
}

// RunPinChecker is implemented by the session module: the live ref check
// behind AskActions.AskActionPin, by the same lookup a run makes.
type RunPinChecker interface {
	CheckRunPin(ctx context.Context, project, ref, commit string) (checked bool, err error)
}

// AskActionStart is one approved action: Kind is orchapi.ActionRun or
// orchapi.ActionCommand (Task, a remote task from the asking session Parent) or
// orchapi.ActionAuthorizeKey (User, Key). AsApprover: Machine is the
// approver's own, so a run there acts in zm as its human (TaskStartOp).
type AskActionStart struct {
	ID         string
	Kind       string
	Project    string
	Machine    string
	Parent     string
	Task       orchapi.TaskStartRequest
	User       string
	Key        string
	AsApprover bool
}

// AskActionStatus is a started action's state (orchapi.ActionQueued, Running,
// Done or Failed) and, when failed, why in plain words.
type AskActionStatus struct {
	State  string
	Reason string
	Ended  *time.Time
	// NeverStarted, on a failed run, is the authority's own proof that no
	// machine was ever handed it: refused while waiting in the line, or
	// while its placement was still unsent to a machine other than the
	// authority's own, with no copy anywhere. Only then may the identical
	// action start again (SPEC 16 "Retry").
	NeverStarted bool
}
