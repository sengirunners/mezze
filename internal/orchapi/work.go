package orchapi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// WorkStatus is a work item's status.
type WorkStatus string

const (
	WorkOpen     WorkStatus = "open"
	WorkClaimed  WorkStatus = "claimed"
	WorkInReview WorkStatus = "in_review"
	WorkDone     WorkStatus = "done"
)

// WorkItem is one unit of work from any source, inside one project.
type WorkItem struct {
	ID      string     `json:"id"` // "<source>:<ref>": local:k3x9qa, bd:abc-1 (unique inside its project)
	Project string     `json:"project"`
	Area    string     `json:"area,omitempty"`
	Source  string     `json:"source"` // "local", "bd", ...
	Title   string     `json:"title"`
	Body    string     `json:"body,omitempty"`
	Status  WorkStatus `json:"status"`
	// Owner is the address holding the item's live lease ("" when unleased,
	// "someone" when the caller cannot see that session).
	Owner string `json:"owner,omitempty"`
	// CreatedBy is the address that created or dispatched it; mail to the
	// item reaches it when nobody holds a lease.
	CreatedBy string    `json:"created_by,omitempty"`
	URL       string    `json:"url,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Links     []Link    `json:"links,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	Updated   time.Time `json:"updated,omitzero"`
	// Extra holds source-specific fields, passed through (e.g. extra.status, the tracker's own status).
	Extra map[string]any `json:"extra,omitempty"`
	// Comments is the item's history; only GET /v0/work/{id} fills it.
	Comments []Comment `json:"comments,omitempty"`
	// Stale is set on a tracker item served from a mirror that is not
	// current (the tracker is failing or far behind): why, sanitised for the
	// caller. The item is still the last state zm saw.
	Stale string `json:"stale,omitempty"`
	// Write is set on a replica's answer to a status write: the id of the
	// outbox entry it queued (GET /v0/team/outbox), until the authority answers.
	Write string `json:"write,omitempty"`
}

// QueuedStatus answers a status write (zm work status --json, MCP
// work_set_status): the item, plus Queued while a replica's write waits for
// the authority, so a script never reads a queued write as final.
type QueuedStatus struct {
	WorkItem
	Queued string `json:"queued,omitempty"`
}

// StatusQueuedNote is QueuedStatus.Queued for a write still in the outbox.
const StatusQueuedNote = "queued: it applies when the authority is reachable (zm outbox)"

// ValidStatus reports whether s is one of the four zm statuses.
func ValidStatus(s WorkStatus) bool {
	switch s {
	case WorkOpen, WorkClaimed, WorkInReview, WorkDone:
		return true
	}
	return false
}

// LinkRels are the allowed Link.Rel values.
var LinkRels = []string{"pr", "blocks", "blocked_by", "parent", "child", "session", "related", "url"}

// ValidLinkRel reports whether rel is one of LinkRels.
func ValidLinkRel(rel string) bool {
	for _, r := range LinkRels {
		if r == rel {
			return true
		}
	}
	return false
}

// Link relates a work item to something else. A link to an item the caller
// cannot read is shown as {"rel": ..., "target": "", "hidden": true}; hidden
// links are collapsed into one entry per rel so their count does not leak.
type Link struct {
	Rel    string `json:"rel"`              // pr, blocks, blocked_by, parent, child, session, related, url
	Target string `json:"target,omitempty"` // a work item id, a session address or a URL
	Hidden bool   `json:"hidden,omitempty"` // "linked to an item you can't access"
	// Project is the target item's project ULID when it is not the linking
	// item's project (a cross-project link). Ignored on requests.
	Project string `json:"project,omitempty"`
}

// Lease is an atomic claim with a server-enforced deadline on a key: a work
// item id or any other string ("lock:migrations"), inside one project.
// Owned by the exact session address, never the machine.
type Lease struct {
	Key     string `json:"key"`
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
	Holder  string `json:"holder"` // session or human address ("someone" when hidden)
	// Bound leases belong to a managed session: the daemon's ticker renews
	// them (Expires = now + bound_lease_ttl) only while the session is
	// running, so an idle or needs_human holder expires like a TTL lease; an ended session's leases are released at once. A replica
	// serves a bound lease with no Expires: renewals stay at the authority.
	Bound   bool       `json:"bound"`
	Note    string     `json:"note,omitempty"`
	Claimed time.Time  `json:"claimed"`
	Renewed time.Time  `json:"renewed"`
	Expires *time.Time `json:"expires,omitempty"` // the effective deadline; always set (bound leases too)
	// Fence is the lease's fencing token: a new claim gets a larger one than
	// any earlier lease of its project (random steps), a renewal keeps it.
	// Writes and releases made under the lease send it back (Fence on their
	// requests); one that no longer names the caller's live lease is refused
	// (conflict, data Refusal{ReasonStaleLease}). 0: a lease from before fences.
	Fence int64 `json:"fence,omitempty"`
}

// ReasonStaleLease (a Refusal's reason, with code conflict): a write or
// release named a lease fence that is not the caller's live lease on the
// key any more (it expired, was released, or was taken again since).
const ReasonStaleLease = "stale-lease"

// StaleLease is the refusal of a write or release that named fence on key
// when that is not the caller's live lease any more. It names nothing about
// whoever holds the key now, so it says no more than the caller's own fence.
func StaleLease(key string, fence int64) *api.Error {
	return api.Errorf(api.CodeConflict, "your lease on %s (fence %d) is no longer live: it expired, was released or was taken since", key, fence).
		WithHint("claim it again and use the new lease's fence; check what changed meanwhile").
		WithData(Refusal{Reason: ReasonStaleLease})
}

// IsStaleLease reports whether err is a StaleLease refusal.
func IsStaleLease(err error) bool {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict || len(e.Data) == 0 {
		return false
	}
	var r Refusal
	return json.Unmarshal(e.Data, &r) == nil && r.Reason == ReasonStaleLease
}

// Claim is the old name of Lease.
type Claim = Lease

// ClaimRequest is POST /v0/work/{id}/claim (also renews) and POST /v0/leases.
type ClaimRequest struct {
	Key        string `json:"key,omitempty"`         // POST /v0/leases only
	TTLSeconds int    `json:"ttl_seconds,omitempty"` // named TTL wins for any session; zero = bound TTL or default 3600; max 7 days
	Note       string `json:"note,omitempty"`
	// Renew only extends a lease the caller already holds; it never takes a
	// free or someone else's key (conflict instead).
	Renew bool `json:"renew,omitempty"`
	// Fence, when set, renews only the caller's live lease with this fence
	// (else conflict, ReasonStaleLease); it never takes a key afresh.
	Fence int64 `json:"fence,omitempty"`
}

// ClaimResponse is the result of a claim. On conflict the error's Data is
// the current Lease, redacted for the caller.
type ClaimResponse struct {
	Lease Lease     `json:"lease"`
	Item  *WorkItem `json:"item,omitempty"`
}

// ReleaseRequest is POST /v0/work/{id}/release and DELETE /v0/leases/{key}.
type ReleaseRequest struct {
	Force bool `json:"force,omitempty"` // owners/members on the local socket only
	// Fence, when set, releases only the lease with this fence: another lease
	// on the key is refused (conflict, ReasonStaleLease); none is released: false.
	Fence int64 `json:"fence,omitempty"`
}

// ReleaseResponse reports the release.
type ReleaseResponse struct {
	Key      string `json:"key"`
	Released bool   `json:"released"`
}

// HolderResponse is GET /v0/work/{id}/holder: who mail to this item reaches.
type HolderResponse struct {
	ID     string `json:"id"`
	Holder string `json:"holder,omitempty"`
	Lease  *Lease `json:"lease,omitempty"`
	// MailTo is where mail to the item goes now (holder, else creator); "" = refused.
	MailTo string `json:"mail_to,omitempty"`
	Via    string `json:"via,omitempty"`
	// TrackerOwner is who the item's tracker says has it (e.g. a beads
	// assignee) when no zm lease does; it may not be a zm address.
	TrackerOwner string `json:"tracker_owner,omitempty"`
	// Session is the holding session when the caller may see it: its
	// state, and for a session on a team replica its presence (Seen).
	Session *Session `json:"session,omitempty"`
}

// CreateWorkRequest is POST /v0/work (local items only). Project from resolution.
type CreateWorkRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Area   string   `json:"area,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// CommentRequest is POST /v0/work/{id}/comment.
type CommentRequest struct {
	Body string `json:"body"`
	// Fence, when set, makes the comment a write under the caller's lease:
	// refused (conflict, ReasonStaleLease) unless that lease is still live.
	Fence int64 `json:"fence,omitempty"`
}

// Comment is one entry in a work item's history.
type Comment struct {
	ID      string    `json:"id"`
	Item    string    `json:"item"`
	Author  string    `json:"author"`
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
}

// CommentList is GET /v0/work/{id}/comments.
type CommentList struct {
	Comments []Comment `json:"comments"`
}

// StatusRequest is POST /v0/work/{id}/status.
type StatusRequest struct {
	Status WorkStatus `json:"status"`
	Fence  int64      `json:"fence,omitempty"` // as CommentRequest.Fence
}

// LinkRequest is POST /v0/work/{id}/link.
type LinkRequest = Link

// WorkQuery is GET /v0/work's query string. No project = every visible project.
type WorkQuery struct {
	Project string
	Area    string
	Source  string
	Status  WorkStatus
	Owner   string
	Q       string
	Limit   int
}

// WorkList is a list of items. Skipped names every tracker that could not
// be listed (missing, broken, timed out): the list is then incomplete, and
// clients must say so rather than show a shorter list as if it were whole.
type WorkList struct {
	Items   []WorkItem        `json:"items"`
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
	// Behind names every tracker whose items came from a mirror that is not
	// current: still loading its first sync (the list may be missing items),
	// or stale (the tracker is failing; the items are as last seen).
	// Clients must say so, like Skipped.
	Behind []WorkBehind `json:"behind,omitempty"`
}

// Node kinds in a rollup.
const (
	RollupTheme = "theme" // an epic labelled theme, with no open epic above it
	RollupEpic  = "epic"
)

// WorkRollupList is GET /v0/work/rollup: one rollup per project, with the
// same skipped and behind as GET /v0/work.
type WorkRollupList struct {
	Rollups []WorkRollup      `json:"rollups"`
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
	Behind  []WorkBehind      `json:"behind,omitempty"`
}

// WorkRollup is one project's counts per theme and epic (SPEC 10 "Rollup").
type WorkRollup struct {
	Project string `json:"project"`
	Slug    string `json:"slug,omitempty"`
	// Totals is every item but epics; NoEpic those with no open epic above them.
	Totals RollupCounts `json:"totals"`
	NoEpic RollupCounts `json:"no_epic"`
	// Nodes is a flat list, most urgent first; Parent and Children are the tree.
	Nodes     []RollupNode     `json:"nodes"`
	Claimed   []RollupClaim    `json:"claimed"`
	Deadlines []RollupDeadline `json:"deadlines"`
}

// RollupCounts counts items. Total = Open + Working + Blocked + Deferred +
// Done; Urgent is the open and blocked ones at priority 0 or 1.
type RollupCounts struct {
	Total    int       `json:"total"`
	Open     int       `json:"open"`
	Urgent   int       `json:"urgent"`
	Working  int       `json:"working"` // claimed or in review
	Blocked  int       `json:"blocked"`
	Deferred int       `json:"deferred"`
	Done     int       `json:"done"`
	Changed  time.Time `json:"changed,omitzero"` // the latest change among them
}

// RollupNode is one theme or epic, with every item beneath it counted.
type RollupNode struct {
	ID       string   `json:"id"` // a work id
	Title    string   `json:"title"`
	Kind     string   `json:"kind"` // RollupTheme or RollupEpic
	Parent   string   `json:"parent,omitempty"`
	Children []string `json:"children,omitempty"`
	RollupCounts
	// Claims counts the live leases on the items beneath it (as its counts do).
	Claims RollupClaimCounts `json:"claims"`
}

// RollupClaimCounts counts a node's claims by state (RollupClaim.State); a
// claim in neither state (a person's, or a holder not known here) is in neither.
type RollupClaimCounts struct {
	Live    int `json:"live"`
	Blocked int `json:"blocked"`
}

// Rollup claim states (RollupClaim.State).
const (
	ClaimLive    = "live"    // the holding session is running or idle
	ClaimBlocked = "blocked" // the item is blocked, or its holder waits on a person or has ended
)

// RollupClaim is one live lease on a work item.
type RollupClaim struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Status   string     `json:"status"`
	Priority *int       `json:"priority,omitempty"`
	Holder   string     `json:"holder"` // "someone" when hidden
	Mine     bool       `json:"mine"`   // the caller's session, or a session the caller started
	Bound    bool       `json:"bound"`
	Claimed  time.Time  `json:"claimed"`
	Expires  *time.Time `json:"expires,omitempty"`
	// Epic is the node that counts the item: its nearest open epic or theme
	// ("" under no_epic, or an epic out of sight).
	Epic string `json:"epic,omitempty"`
	// State is ClaimLive, ClaimBlocked or "" (neither).
	State string `json:"state,omitempty"`
}

// RollupDeadline is an item not done with a due time (DueAt), or a deferred
// one coming back within a week (ReturnsAt).
type RollupDeadline struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	Priority  *int       `json:"priority,omitempty"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	ReturnsAt *time.Time `json:"returns_at,omitempty"`
	Day       string     `json:"day"` // the calendar day that was typed, YYYY-MM-DD
	Overdue   bool       `json:"overdue,omitempty"`
}

// WorkBehind is one tracker whose mirror is not current.
type WorkBehind struct {
	Project string    `json:"project"`         // project ULID
	Source  string    `json:"source"`          // tracker prefix ("bd")
	State   string    `json:"state"`           // SyncLoading or SyncStale
	Synced  time.Time `json:"synced,omitzero"` // last successful sync
	Error   string    `json:"error,omitempty"` // sanitised for the caller
}

// Tracker mirror states (WorkSync.State, WorkBehind.State).
const (
	SyncLoading = "loading" // the first full sync has not finished
	SyncOK      = "ok"      // synced recently
	SyncStale   = "stale"   // the last sync failed or is overdue; reads serve what was last seen
)

// LeaseList is GET /v0/leases.
type LeaseList struct {
	Leases []Lease `json:"leases"`
	// Skipped names a joined team project whose authority did not answer
	// (source "authority"): the list is missing its leases (SPEC 17.3).
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
}

// LeaseQuery is GET /v0/leases's query string.
type LeaseQuery struct {
	Project string
	Holder  string
}

// WorkSource is one source of work in a project: "local" or a tracker's adapter.
type WorkSource struct {
	Project string   `json:"project"`
	Name    string   `json:"name"`           // "local", "beads"
	Prefix  string   `json:"prefix"`         // "local", "bd"
	Path    string   `json:"path,omitempty"` // adapter executable (local callers only)
	Verbs   []string `json:"verbs"`
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	// Sync is the tracker's mirror freshness (trackers only; local is always current).
	Sync *WorkSync `json:"sync,omitempty"`
	// Backup: on a replica's authority row, the authority's off-site home
	// backup as of its last sync answer (SPEC 17.9).
	Backup *HomeBackup `json:"backup,omitempty"`
	// Started: on the same row, when the authority's daemon started.
	Started time.Time `json:"started,omitzero"`
}

// WorkSync is how current a tracker's local mirror is. Reads are served
// from the mirror; a background sync keeps it current.
type WorkSync struct {
	State      string    `json:"state"`           // SyncLoading, SyncOK or SyncStale
	Synced     time.Time `json:"synced,omitzero"` // last successful sync
	LagSeconds float64   `json:"lag_seconds"`     // seconds since Synced (0 when never)
	FullSync   time.Time `json:"full_sync,omitzero"`
	Error      string    `json:"error,omitempty"` // the last sync's error, sanitised for the caller
	// FailingSince is when the current run of failed syncs began (absent
	// when the last sync worked); Failures counts them.
	FailingSince *time.Time `json:"failing_since,omitempty"`
	Failures     int        `json:"failures,omitempty"`
	// Items is the number of mirrored items (shown to whole-project readers only).
	Items int `json:"items,omitempty"`
	// Mode is how the mirror syncs: "changes" (incremental by cursor) or
	// "list" (the full-list fallback for adapters without changes). Serve:
	// the adapter runs as one long-lived process. Local callers only.
	Mode  string `json:"mode,omitempty"`
	Serve bool   `json:"serve,omitempty"`
}

// WorkSourceList is GET /v0/work/sources.
type WorkSourceList struct {
	Sources []WorkSource `json:"sources"`
}

// Inflight is one row of GET /v0/work/inflight: a read-only join of live
// leases, their work items and the holding sessions.
type Inflight struct {
	Lease   Lease     `json:"lease"`
	Item    *WorkItem `json:"item,omitempty"`
	Session *Session  `json:"session,omitempty"`
	// Unheld: no live lease, but the item is claimed or in review with a pr
	// link (its lease lapsed while the PR is open); Lease names only the key.
	Unheld bool `json:"unheld,omitempty"`
}

// InflightList is GET /v0/work/inflight.
type InflightList struct {
	Inflight []Inflight        `json:"inflight"`
	Skipped  []api.WorkSkipped `json:"skipped,omitempty"` // as LeaseList.Skipped
}

// ProjectHint is a content-free hint's data (dev.zm.work.refreshed,
// dev.zm.summary.schedule_changed): the project to read again.
type ProjectHint struct {
	Project string `json:"project"`
}

// WorkMoved is dev.zm.work.moved: the work id a native issue had here. It does
// not say where it went: this project's readers may not see that project.
type WorkMoved struct {
	ID string `json:"id"`
}

// Work event types. Data is the WorkItem (claimed/released/lease_expired: the Lease; commented: the Comment).
const (
	EventWorkCreated       = "dev.zm.work.created"
	EventWorkClaimed       = "dev.zm.work.claimed"
	EventWorkReleased      = "dev.zm.work.released"
	EventWorkLeaseExpired  = "dev.zm.work.lease_expired"
	EventWorkStatusChanged = "dev.zm.work.status_changed"
	EventWorkCommented     = "dev.zm.work.commented"
	EventWorkLinked        = "dev.zm.work.linked"
	EventWorkMoved         = "dev.zm.work.moved"     // a native issue left the project (zm issues move): data WorkMoved
	EventWorkChanged       = "dev.zm.work.changed"   // an adapter's watch saw a change
	EventWorkRefreshed     = "dev.zm.work.refreshed" // tracker freshness changed; refetch scoped read metadata
	EventWorkImported      = "dev.zm.work.imported"  // deprecated 2026-11-01: nothing publishes it; data is WorkImported
)

// WorkImported is a `zm work import` batch that committed, one event per
// area it touched (subject: the job id): Items are the work ids created or
// updated (phase items) or given links (phase links) in that area.
//
// Deprecated: `zm work import` was retired on 2026-11-01 and nothing
// publishes this event now. The type stays because it is contract: a team
// replica still applies the event when it replays an older log.
type WorkImported struct {
	Job   string   `json:"job"`
	Phase string   `json:"phase"`
	Items []string `json:"items"`
}

// Adapter verbs. describe, list, get, claim, release, comment, set_status are required.
const (
	VerbDescribe  = "describe"
	VerbList      = "list"
	VerbGet       = "get"
	VerbClaim     = "claim"
	VerbRelease   = "release"
	VerbComment   = "comment"
	VerbSetStatus = "set_status"
	VerbLink      = "link"    // optional
	VerbWatch     = "watch"   // optional; streams NDJSON
	VerbChanges   = "changes" // optional; what changed since a cursor (the mirror's sync)
	// VerbSetup is optional: zm tracker add calls it with dry_run before
	// binding (the adapter checks its config and may return defaults to
	// merge) and again after binding (the adapter does its one-time setup).
	// describe.setup says what it touches.
	VerbSetup = "setup"
	// VerbCheck is optional and read-only: checks only the adapter knows how
	// to make, run by zm tracker add and test after the conformance checks.
	VerbCheck = "check"
	// AdapterNative is the built-in adapter that serves a project's native
	// issues (SPEC "Native issues"): no executable, the issues module answers.
	AdapterNative = "native"
)

// ServeFlag starts an adapter whose describe says "serve": true in serve
// mode: one long-lived process reading JSON lines (AdapterRequest with RID)
// on stdin and answering JSON lines (AdapterResponse with the same RID) on
// stdout, possibly out of order, until stdin closes.
const ServeFlag = "--serve"

// RequiredVerbs every adapter must support.
var RequiredVerbs = []string{VerbDescribe, VerbList, VerbGet, VerbClaim, VerbRelease, VerbComment, VerbSetStatus}

// WriteVerbs change the tracker; a read-only caller (zm-work-conformance
// without --write) never sends them.
var WriteVerbs = []string{VerbClaim, VerbRelease, VerbComment, VerbSetStatus, VerbLink}

// AdapterRequest is the one JSON object an adapter reads on stdin (one
// line per request in serve mode).
type AdapterRequest struct {
	Spec string `json:"zm"` // "zm/v0"
	// RID names the request in serve mode; the response carries it back.
	RID string `json:"rid,omitempty"`
	// Cancel (serve mode only) is a control line, not a request: stop the
	// request with this RID (its answer may still arrive, or not at all).
	Cancel string          `json:"cancel,omitempty"`
	Verb   string          `json:"verb"`
	ID     string          `json:"id,omitempty"` // full id incl. prefix, e.g. "bd:abc-1"
	Args   json.RawMessage `json:"args,omitempty"`
	// Config is the project's tracker config (project.toml [[trackers]] config).
	Config map[string]string `json:"config,omitempty"`
	// Actor is the zm identity name a write verb is made for (the caller,
	// else the claim holder's launcher); empty for the daemon's own writes
	// (expiry, repair) and on reads. Adapters may attribute the write to it.
	Actor string `json:"actor,omitempty"`
}

// AdapterResponse is the one JSON object an adapter writes on stdout (exit 0).
type AdapterResponse struct {
	RID    string          `json:"rid,omitempty"` // serve mode: the request's RID
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *api.Error      `json:"error,omitempty"`
}

// AdapterDescribe is describe's result.
type AdapterDescribe struct {
	IdempotentComments bool     `json:"idempotent_comments,omitempty"` // delivery_id is atomically hash-bound at final sink
	Spec               string   `json:"zm"`                            // "zm/v0"
	Name               string   `json:"name"`
	Prefix             string   `json:"prefix"`
	Version            string   `json:"version,omitempty"`
	Verbs              []string `json:"verbs"`
	// Serve: the adapter also runs as one long-lived process (ServeFlag).
	Serve bool `json:"serve,omitempty"`
	// PollSeconds is the least pause between incremental syncs the adapter
	// asks for (0 = the daemon's default, a few seconds), e.g. to spare an
	// API rate limit.
	PollSeconds int `json:"poll_seconds,omitempty"`
	// Backup lists native backup kinds (BackupSqlite). Every adapter also
	// gets BackupExport without saying so.
	Backup []string `json:"backup,omitempty"`
	// Setup is set when the adapter offers the setup verb: what its
	// one-time setup touches outside zm, shown to whoever adds the tracker.
	Setup *AdapterSetupInfo `json:"setup,omitempty"`
	// Env is what the adapter needs in its environment beyond the base
	// (PATH, HOME, LANG, TERM, TZ); absent, the base only (SPEC 17.2).
	Env *AdapterEnv `json:"env,omitempty"`
}

// AdapterEnv is describe.env: the only variables zm adds to the base.
type AdapterEnv struct {
	// Vars are passed from the daemon's environment when set; the zm-filled
	// names (EnvDataDir, EnvToolsDir) come from zm, never from there.
	Vars []string `json:"vars,omitempty"`
	// Secrets zm reads through the Secrets brick, never from its environment.
	Secrets []AdapterSecret `json:"secrets,omitempty"`
}

// AdapterSecret is one secret variable: Name, filled from From, which is
// SecretSlot (the tracker's secret slot, tracker add --password-file) or
// SecretConfig+key (the file a config key names). Unset when it is empty.
type AdapterSecret struct {
	Name string `json:"name"`
	From string `json:"from"`
}

// AdapterSecret.From values.
const (
	SecretSlot   = "slot"
	SecretConfig = "config:"
)

// Variables zm fills itself for an adapter declaring them in describe.env.
const (
	EnvDataDir  = "ZM_DATA_DIR"  // the tracker's own directory, $ZM_HOME/trackers/<project>/<prefix>
	EnvToolsDir = "ZM_TOOLS_DIR" // where zm provisions pinned CLIs, $ZM_HOME/tools
)

// AdapterListArgs is list's args. Result: WorkList.
type AdapterListArgs struct {
	Status WorkStatus `json:"status,omitempty"`
	Owner  string     `json:"owner,omitempty"`
	Q      string     `json:"q,omitempty"`
	Limit  int        `json:"limit,omitempty"`
	// LabelsAny, when set, asks for items carrying at least one of these
	// labels (an optional filter: zm filters the answer again either way).
	LabelsAny []string `json:"labels_any,omitempty"`
}

// AdapterClaimArgs is claim's args. Result: WorkItem. Refusal: error code "conflict".
type AdapterClaimArgs struct {
	Holder string `json:"holder"`
	Note   string `json:"note,omitempty"`
	// TTLSeconds is the zm lease's TTL, for trackers that record a deadline.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// AdapterReleaseArgs is release's args. Result: WorkItem.
type AdapterReleaseArgs struct {
	Holder string `json:"holder"`
}

// AdapterCommentArgs is comment's args. Result: {}.
type AdapterCommentArgs struct {
	DeliveryID string `json:"delivery_id,omitempty"` // stable internal effect ID; only idempotent adapters may replay
	Body       string `json:"body"`
	Author     string `json:"author"`
}

// AdapterSetStatusArgs is set_status's args. Result: WorkItem.
type AdapterSetStatusArgs struct {
	Status WorkStatus `json:"status"`
}

// AdapterLinkArgs is link's args. Result: WorkItem.
type AdapterLinkArgs = Link

// AdapterChangesArgs is changes's args. Since "" asks for a full listing:
// every item the tracker has, done ones included, possibly in pages (Page
// is the previous answer's Next). Since = a previous answer's Cursor asks
// for what changed after it (an overlap is fine: items are upserted).
type AdapterChangesArgs struct {
	Since string `json:"since,omitempty"`
	Page  string `json:"page,omitempty"`
}

// AdapterChanges is changes's result.
type AdapterChanges struct {
	Items []WorkItem `json:"items"`
	// Next continues this listing (call again with Page = Next); "" = done.
	Next string `json:"next,omitempty"`
	// Cursor is what to pass as Since next time; set on the last page.
	Cursor string `json:"cursor,omitempty"`
	// Deleted ids the tracker knows were removed since the cursor (optional).
	Deleted []string `json:"deleted,omitempty"`
	// Count is how many items the tracker has now (optional): a mirror
	// holding a different number runs a full listing to drop deleted ones.
	Count *int `json:"count,omitempty"`
	// Partial says this page of a full listing may leave items out (cut at
	// a limit, or a search that may miss some): nothing the listing did
	// not return is dropped from the mirror as gone.
	Partial bool `json:"partial,omitempty"`
}

// AdapterWatchArgs is watch's args. Output: one WorkItem per line until stdin closes or the process is killed.
type AdapterWatchArgs struct {
	Since string `json:"since,omitempty"` // adapter-defined cursor
}

// AdapterSetupInfo is describe.setup: what the setup verb touches outside
// zm (for example "Linear workflow states of the bound teams").
type AdapterSetupInfo struct {
	Touches []string `json:"touches"`
	// Options are the setup option keys the adapter takes (SetupOpt*). zm
	// sends exactly these (those with a value), refuses a tracker add flag
	// whose key is not among them, and refuses a --config key that is one.
	Options []string `json:"options,omitempty"`
	// Password: the adapter takes --password-file. zm copies it into the
	// tracker's secret slot before setup and says so in zm.has_password;
	// the adapter gets the password only in its environment.
	Password bool `json:"password,omitempty"`
	// BeforeBind: setup (not dry run) runs before the tracker is bound; its
	// config is merged, its failure refuses the add, and its undo runs if a
	// later step fails. Otherwise it runs once bound, and a failure is a note.
	BeforeBind bool `json:"before_bind,omitempty"`
}

// Setup option keys (SPEC 11): tracker add's flags as given. What zm
// fills itself is AdapterSetupZM, never an option.
const (
	SetupOptMode         = "mode"
	SetupOptDir          = "dir"
	SetupOptServer       = "server"
	SetupOptDatabase     = "database"
	SetupOptUser         = "user"
	SetupOptAllowMigrate = "allow_migrate" // "true"; never sent when frozen
)

// AdapterSetupZM is what zm itself tells setup (args.zm): never from a
// caller's options or config, so nothing a caller types can set it.
type AdapterSetupZM struct {
	// DataDir is the tracker's own directory, $ZM_HOME/trackers/<project>/<prefix>.
	DataDir string `json:"data_dir,omitempty"`
	// IssuePrefix is the project's slug (beads makes its bd prefix from it).
	IssuePrefix string `json:"issue_prefix,omitempty"`
	// Siblings are the prefixes (nothing else) of the same project's other
	// trackers of this adapter. beads refuses a bare add on a project that
	// already has one (a second new database is then likely a mistake).
	Siblings []string `json:"siblings,omitempty"`
	// HasPassword: zm stored --password-file in the tracker's secret slot.
	HasPassword bool `json:"has_password,omitempty"`
}

// UndoRemoveDir is the one undo op: remove a directory setup made.
const UndoRemoveDir = "remove_dir"

// AdapterUndo is one step that takes back what a setup call made.
type AdapterUndo struct {
	Op   string `json:"op"`
	Path string `json:"path"`
}

// AdapterSetupArgs is setup's args. DryRun checks the config and changes
// nothing outside zm. The request's actor is the identity adding the
// tracker (the machine's human).
type AdapterSetupArgs struct {
	DryRun bool `json:"dry_run,omitempty"`
	// Options are the declared setup options that have a value.
	Options map[string]string `json:"options,omitempty"`
	// ZM is what zm fills itself.
	ZM *AdapterSetupZM `json:"zm,omitempty"`
	// Frozen: the tracker's home is frozen. The adapter only probes: it
	// opens its store read-only and creates, migrates and writes nothing.
	Frozen bool `json:"frozen,omitempty"`
}

// AdapterSetupResult is setup's result: notes for the person adding the
// tracker, and (dry run) config defaults zm merges in before binding (only
// keys the config does not already set).
type AdapterSetupResult struct {
	Notes  []string          `json:"notes,omitempty"`
	Config map[string]string `json:"config,omitempty"`
	// Created: this call made something new (Undo takes it back).
	Created bool `json:"created,omitempty"`
	// Schema is a beads server binding's schema check.
	Schema *SchemaCheck `json:"schema,omitempty"`
	// Undo is what zm runs if a later step of the add fails: only when
	// Created, only for paths this call made, only inside data_dir.
	Undo []AdapterUndo `json:"undo,omitempty"`
}

// AdapterCheckResult is check's result.
type AdapterCheckResult struct {
	Checks []TrackerCheck `json:"checks,omitempty"`
	Notes  []string       `json:"notes,omitempty"`
	// CLI is the tracker CLI run exactly as the adapter runs it.
	CLI *AdapterCLI `json:"cli,omitempty"`
}

// AdapterCLI is `<bin> version` as the adapter ran it.
type AdapterCLI struct {
	Bin     string `json:"bin"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}
