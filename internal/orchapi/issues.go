package orchapi

import (
	"encoding/json"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Native issue statuses (beads' built-in set plus in_review). ZmStatus maps
// each onto the four zm work statuses.
const (
	IssueOpen       = "open"
	IssueInProgress = "in_progress"
	IssueInReview   = "in_review"
	IssueBlocked    = "blocked"
	IssueDeferred   = "deferred"
	IssueClosed     = "closed"
	IssuePinned     = "pinned"
	IssueHooked     = "hooked"
)

// IssueStatuses lists every valid native status.
var IssueStatuses = []string{IssueOpen, IssueInProgress, IssueInReview, IssueBlocked, IssueDeferred, IssueClosed, IssuePinned, IssueHooked}

// ValidIssueStatus reports whether s is a native status.
func ValidIssueStatus(s string) bool {
	for _, x := range IssueStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// ZmStatus maps a native status onto zm's four work statuses (SPEC 10):
// in_progress and hooked are claimed, in_review is in_review, closed is
// done, everything else (open, blocked, deferred, pinned) is open.
func ZmStatus(s string) WorkStatus {
	switch s {
	case IssueInProgress, IssueHooked:
		return WorkClaimed
	case IssueInReview:
		return WorkInReview
	case IssueClosed:
		return WorkDone
	}
	return WorkOpen
}

// Dependency types (beads' set). Only blocks and conditional-blocks gate
// ready work; parent-child makes the child blocked only while its parent is.
const (
	DepBlocks            = "blocks"
	DepConditionalBlocks = "conditional-blocks"
	DepParentChild       = "parent-child"
	DepRelated           = "related"
	DepRelatesTo         = "relates-to"
	DepDiscoveredFrom    = "discovered-from"
)

// Issue is one native issue. Times are RFC 3339 UTC.
type Issue struct {
	ID string `json:"id"` // <prefix>-<hash>[.N...]: acme-ofoqu, acme-ofoqu.2
	// Number is the issue's per-project sequence number (1, 2, ...; Linear's
	// Issue.number), in creation order, never reused. The id stays the
	// identifier; imported issues are numbered by their creation time.
	Number int `json:"number"`
	// Score is the issue's relevance in a sort=rank search (bm25, higher is
	// better; 0 elsewhere).
	Score              float64    `json:"score,omitempty"`
	Project            string     `json:"project"`
	Area               string     `json:"area,omitempty"`
	Title              string     `json:"title"`
	Description        string     `json:"description,omitempty"` // markdown
	Design             string     `json:"design,omitempty"`
	AcceptanceCriteria string     `json:"acceptance_criteria,omitempty"`
	Notes              string     `json:"notes,omitempty"`
	SpecID             string     `json:"spec_id,omitempty"`
	Status             string     `json:"status"`
	ZmStatus           WorkStatus `json:"zm_status"`
	Priority           int        `json:"priority"` // 0 (highest) .. 4
	IssueType          string     `json:"issue_type"`
	// Assignee is a display name (beads keeps names, e.g. "Judy Example");
	// ClaimedBy is the zm address that claimed it through zm (redacted like
	// every address: "someone" when the caller cannot see that session).
	Assignee         string    `json:"assignee,omitempty"`
	ClaimedBy        string    `json:"claimed_by,omitempty"`
	Owner            string    `json:"owner,omitempty"`
	EstimatedMinutes *int      `json:"estimated_minutes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedBy        string    `json:"created_by,omitempty"`
	// CreatedByAddress is the zm address that created it through zm (empty
	// for imported issues; redacted).
	CreatedByAddress string          `json:"created_by_address,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	ClosedAt         *time.Time      `json:"closed_at,omitempty"`
	CloseReason      string          `json:"close_reason,omitempty"`
	ClosedBySession  string          `json:"closed_by_session,omitempty"`
	DueAt            *time.Time      `json:"due_at,omitempty"`
	DeferUntil       *time.Time      `json:"defer_until,omitempty"`
	ExternalRef      string          `json:"external_ref,omitempty"`
	SourceSystem     string          `json:"source_system,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	Pinned           bool            `json:"pinned,omitempty"`
	IsTemplate       bool            `json:"is_template,omitempty"`
	Ephemeral        bool            `json:"ephemeral,omitempty"`
	Labels           []string        `json:"labels,omitempty"`
	// Blocked: an open blocks/conditional-blocks dependency, or a blocked
	// parent (beads' is_blocked). Ready work is open and not blocked.
	Blocked bool `json:"blocked,omitempty"`
	// Parent is the parent-child target, if any.
	Parent string `json:"parent,omitempty"`
	// Counts (lists and gets): blocks edges out and in (beads' list
	// semantics), and comments. Edges to issues the caller cannot read are
	// not counted.
	DependencyCount int `json:"dependency_count"`
	DependentCount  int `json:"dependent_count"`
	CommentCount    int `json:"comment_count"`
	// Version is the id of the issue's last change (its history row): send
	// it back as Base on a field update to refuse a lost update (SPEC
	// "Native issues", replicas).
	Version string `json:"version,omitempty"`
	// Extra holds source columns zm does not model (beads' wisp, molecule,
	// compaction and agent fields), kept verbatim by the importer.
	Extra map[string]any `json:"extra,omitempty"`
	// Dependencies are this issue's own edges (lists: raw records;
	// GET /v0/issues/{id}: see IssueDetail).
	Dependencies []IssueDep `json:"dependencies,omitempty"`
	// Incoming are edges onto this issue from issues the caller reads, and
	// Comments its comments; lists fill them on request (incoming=1,
	// comments=1), so a page needs no call per issue.
	Incoming []IssueDep     `json:"incoming,omitempty"`
	Comments []IssueComment `json:"comments,omitempty"`
	// Attachments are the issue's live images, metadata only (bytes: GET
	// /v0/issues/{id}/attachments/{aid}). A single issue's read fills them
	// (GET /v0/issues/{id}, the native work item, bd show); lists do not.
	Attachments []IssueAttachment `json:"attachments,omitempty"`
}

// IssueAttachment is one image or HTML page on an issue (SPEC "Native issues",
// attachments): kept at the authority only, removed when it expires or by
// its attacher or an area admin. The field names are a contract (the Mezze app reads
// them).
type IssueAttachment struct {
	ID        string    `json:"id"`   // a ULID
	Type      string    `json:"type"` // image/png, image/jpeg, image/webp or text/html
	Size      int64     `json:"size"` // bytes as stored: images re-encoded, HTML verbatim
	SHA256    string    `json:"sha256"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"` // the attacher's address (redacted)
	Expires   time.Time `json:"expires"`
}

// IssueAttachmentList answers GET /v0/issues/{id}/attachments.
type IssueAttachmentList struct {
	Attachments []IssueAttachment `json:"attachments"`
}

// Attachment limits (SPEC "Native issues", attachments).
const (
	MaxIssueAttachments      = 8                   // live per issue
	IssueAttachmentTTL       = 14 * 24 * time.Hour // default expiry
	MaxIssueAttachmentTTL    = 90 * 24 * time.Hour // the longest
	MaxIssueAttachmentUpload = 8 << 20             // an attach request's body, before re-encoding
)

// IssueDep is one dependency edge: IssueID depends on DependsOnID (for
// blocks: DependsOnID blocks IssueID; for parent-child: DependsOnID is the
// parent). DependsOnID may be "external:..." text kept from beads.
type IssueDep struct {
	IssueID     string    `json:"issue_id"`
	DependsOnID string    `json:"depends_on_id"`
	Type        string    `json:"type"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	Metadata    string    `json:"metadata,omitempty"`
	ThreadID    string    `json:"thread_id,omitempty"`
	External    bool      `json:"external,omitempty"`
	// SourceID is the source tracker's row id (beads' dependency uuid), kept
	// so edges list in the source's order.
	SourceID string `json:"-"`
}

// IssueLink is a related issue as GET /v0/issues/{id} shows it: the other
// issue (without its own edges) and the edge type.
type IssueLink struct {
	Issue
	DependencyType string `json:"dependency_type"`
}

// IssueComment is one comment. Author is a display name; AuthorAddress the
// zm address that wrote it (empty for imported comments; redacted).
type IssueComment struct {
	ID            string    `json:"id"`
	IssueID       string    `json:"issue_id"`
	Project       string    `json:"project,omitempty"` // set by a search across projects
	Author        string    `json:"author"`
	AuthorAddress string    `json:"author_address,omitempty"`
	Text          string    `json:"text"` // "" on a deleted comment, but for the area's admins
	CreatedAt     time.Time `json:"created_at"`
	// EditedAt is the author's last edit (nil: never edited).
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// Deleted is a tombstone; DeletedBy (who deleted it) is shown to the
	// area's admins only, so readers do not learn which moderator acted.
	Deleted   bool       `json:"deleted,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string     `json:"deleted_by,omitempty"`
}

// EditCommentRequest is PATCH /v0/issues/{id}/comments/{cid}. Base, when
// set, is the comment's last history id the editor saw: a change since
// refuses the edit (conflict) instead of overwriting it.
type EditCommentRequest struct {
	Text string `json:"text"`
	Base string `json:"base,omitempty"`
}

// AckRedactionGapRequest is POST /v0/issues/{id}/comments/{cid}/ack-gap:
// stop is one stop as the caller's own redaction conflict names it
// ("project:issue", or the hop "old>new").
type AckRedactionGapRequest struct {
	Stop string `json:"stop"`
}

// RedactionGapAck is an operator's acknowledgement that a redaction gap was
// dealt with out of band: the stop and why it was a gap, who and when, and
// the gaps still open (named as the caller's conflict names them).
type RedactionGapAck struct {
	Comment   string    `json:"comment_id"`
	Stop      string    `json:"stop"`
	Why       string    `json:"why"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
	Remaining []string  `json:"remaining"`
}

// CommentQuery is GET /v0/issues/comments: comments across the issues the
// caller reads, oldest id first. Zero values do not filter.
type CommentQuery struct {
	Author   string // author address (exact)
	Prefix   string // text starts with
	Contains string // text contains
	After    string // a comment id: only comments after it
	Limit    int    // default 100, at most 1000
}

// IssueCommentList is GET /v0/issues/comments's answer; Next continues it.
type IssueCommentList struct {
	Comments []IssueComment `json:"comments"`
	Next     string         `json:"next,omitempty"`
}

// IssueDetail is GET /v0/issues/{id}: the issue, its edges both ways with
// the other issue embedded (edges to issues the caller cannot read are left
// out), and its comments.
type IssueDetail struct {
	Issue
	DependsOn  []IssueLink    `json:"depends_on"`
	Dependents []IssueLink    `json:"dependents"`
	Comments   []IssueComment `json:"comments"`
	// MovedFrom is the id asked for when it named an issue that moved to
	// another project (the redirect was followed).
	MovedFrom string `json:"moved_from,omitempty"`
}

// IssueChange is one history row: every change to an issue, with its actor.
// IDs are ULIDs minted in commit order, so the history doubles as the
// project's issue changelog (the replica feed, SPEC "Native issues").
type IssueChange struct {
	ID           string                     `json:"id"`
	IssueID      string                     `json:"issue_id"`
	Kind         string                     `json:"kind"`                    // created, updated, status_changed, claimed, closed, reopened, commented, label_added, label_removed, dep_added, dep_removed, imported, area_changed
	Actor        string                     `json:"actor,omitempty"`         // display name
	ActorAddress string                     `json:"actor_address,omitempty"` // zm address (redacted)
	Fields       map[string]json.RawMessage `json:"fields,omitempty"`        // field -> [old, new]
	Comment      string                     `json:"comment,omitempty"`
	AreaBefore   string                     `json:"area_before"`
	AreaAfter    string                     `json:"area_after"`
	At           time.Time                  `json:"at"`
}

// IssueQuery filters GET /v0/issues. Zero values do not filter.
type IssueQuery struct {
	Project string
	// Status: comma-separated statuses (OR), or "all". Empty = beads'
	// default list: everything but closed and pinned (and templates, gates).
	Status     string
	Priority   *int
	Type       string
	Assignee   string
	NoAssignee bool
	Labels     []string // AND
	LabelAny   []string // OR
	Parent     string
	IDs        []string
	// Text filters (substring, case-insensitive): title, description, notes.
	TitleContains string
	DescContains  string
	NotesContains string
	// Q is full-text search (FTS5: words and prefixes over title,
	// description, notes); Substr is beads' search: substring of the title
	// or the id.
	Q      string
	Substr string
	Ready  bool // ready work: open, not blocked, not deferred
	// Blocked: open issues waiting on an open blocker (or a blocked parent).
	Blocked bool
	// Number: the issue with this per-project number.
	Number *int
	// Sort: priority (default), created, updated, closed, id, title, oldest;
	// rank (with Q): bm25 relevance over title, description and notes, best
	// first, each issue's Score set.
	Sort         string
	Reverse      bool
	Limit        int // default 50, 0 with All; max 5000
	All          bool
	UpdatedAfter *time.Time
	// Keyset paging and narrower filters:
	//   - Areas: only issues in these areas ("" = project-level).
	//   - Contains: substring (case-insensitive) of the title or description.
	//   - ClaimedBy: claimed through zm by this address.
	//   - After: the Next of a previous page (sort created or updated only).
	Areas                                      []string
	Contains                                   string
	ClaimedBy                                  string
	CreatedByAddr                              string // filed by this zm address (created_by_addr)
	CreatedAfter, CreatedBefore, UpdatedBefore *time.Time
	After                                      string
	IDAfter                                    string // only ids after this one (with sort=id: paging by id)
	// Deps adds each issue's own edges (Issue.Dependencies).
	Deps bool
	// Incoming and Comments fill Issue.Incoming and Issue.Comments.
	Incoming, Comments bool
	// Count asks for the exact Total even when the page is cut at Limit
	// (else Total is the page size and More says more matched).
	Count bool
}

// IssueList is GET /v0/issues. More: more issues matched than Limit.
// Total is the number of matching issues the caller may read when the
// query asked for Count (or the page was not cut), else the page size.
type IssueList struct {
	Issues []Issue `json:"issues"`
	More   bool    `json:"more,omitempty"`
	Total  int     `json:"total"`
	// Next continues a keyset page (sort created or updated) when More.
	Next string `json:"next,omitempty"`
	// Skipped names joined team projects whose authority did not answer a
	// list that named no project (a replica).
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
	// Unranked: a sort=rank query too common to rank within the 16 ms
	// budget: the newest matches come back with no Score, and Total is an
	// upper bound (0: unknown). Add words or filters.
	Unranked bool `json:"unranked,omitempty"`
}

// NewIssueRequest is POST /v0/issues. ID is optional (an explicit id with
// the project's prefix); Parent makes the id <parent>.<N>. Deps are
// "id" or "type:id" ("blocks:X" = the new issue blocks X).
type NewIssueRequest struct {
	ID                 string          `json:"id,omitempty"`
	Area               string          `json:"area,omitempty"`
	Title              string          `json:"title"`
	Description        string          `json:"description,omitempty"`
	Design             string          `json:"design,omitempty"`
	AcceptanceCriteria string          `json:"acceptance_criteria,omitempty"`
	Notes              string          `json:"notes,omitempty"`
	SpecID             string          `json:"spec_id,omitempty"`
	Status             string          `json:"status,omitempty"`
	Priority           *int            `json:"priority,omitempty"`
	IssueType          string          `json:"issue_type,omitempty"`
	Assignee           string          `json:"assignee,omitempty"`
	Owner              string          `json:"owner,omitempty"`
	EstimatedMinutes   *int            `json:"estimated_minutes,omitempty"`
	DueAt              *time.Time      `json:"due_at,omitempty"`
	DeferUntil         *time.Time      `json:"defer_until,omitempty"`
	ExternalRef        string          `json:"external_ref,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Labels             []string        `json:"labels,omitempty"`
	Parent             string          `json:"parent,omitempty"`
	NoInheritLabels    bool            `json:"no_inherit_labels,omitempty"` // a child takes its parent's labels unless set
	Deps               []string        `json:"deps,omitempty"`
}

// UpdateIssueRequest is PATCH /v0/issues/{id}. Nil fields are unchanged; an
// empty string clears a text field. Base, when set, is a Version the caller
// had: the update is refused (conflict) if a field it changes changed after
// it (no silent last-writer-wins).
type UpdateIssueRequest struct {
	Base               string          `json:"base,omitempty"`
	Title              *string         `json:"title,omitempty"`
	Description        *string         `json:"description,omitempty"`
	Design             *string         `json:"design,omitempty"`
	AcceptanceCriteria *string         `json:"acceptance_criteria,omitempty"`
	Notes              *string         `json:"notes,omitempty"`
	AppendNotes        *string         `json:"append_notes,omitempty"`
	SpecID             *string         `json:"spec_id,omitempty"`
	Status             *string         `json:"status,omitempty"`
	Priority           *int            `json:"priority,omitempty"`
	IssueType          *string         `json:"issue_type,omitempty"`
	Assignee           *string         `json:"assignee,omitempty"`
	EstimatedMinutes   *int            `json:"estimated_minutes,omitempty"`
	DueAt              *time.Time      `json:"due_at,omitempty"`
	ClearDue           bool            `json:"clear_due,omitempty"`
	DeferUntil         *time.Time      `json:"defer_until,omitempty"`
	ClearDefer         bool            `json:"clear_defer,omitempty"`
	ExternalRef        *string         `json:"external_ref,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"` // merged into the existing object
	AddLabels          []string        `json:"add_labels,omitempty"`
	RemoveLabels       []string        `json:"remove_labels,omitempty"`
	SetLabels          []string        `json:"set_labels,omitempty"`
	Parent             *string         `json:"parent,omitempty"`  // "" removes the parent
	Session            string          `json:"session,omitempty"` // closed_by_session when Status is closed
	// Fence, on this and the close, reopen, claim and comment requests, makes
	// the write one under the caller's lease on the issue (Lease.Fence; a
	// project binding native issues): refused unless it is still live.
	Fence int64 `json:"fence,omitempty"`
}

// ClaimIssueRequest is POST /v0/issues/{id}/claim: beads' --claim. The
// assignee becomes the caller and the status in_progress, atomically, only
// from open and unassigned (or already the caller's).
type ClaimIssueRequest struct {
	// TTLSeconds is the lease's TTL when the project binds native issues
	// (0 = bound for a live session, else default; a named TTL always wins).
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Assignee is a routing label for the claim (the bd door sends bd's
	// actor, such as a hub address "host-a/58565660"): it becomes the
	// issue's assignee instead of the caller's name. It is not an identity:
	// who claimed is claimed_by (the caller's zm address), and authz is the
	// caller's. "" = the caller's name.
	Assignee string `json:"assignee,omitempty"`
	// Fence renews only the caller's live lease with this fence (ClaimRequest.Fence).
	Fence int64 `json:"fence,omitempty"`
}

// CloseIssueRequest is POST /v0/issues/{id}/close. Force closes an epic
// with open children or an issue with open blockers.
type CloseIssueRequest struct {
	Reason  string `json:"reason,omitempty"`
	Session string `json:"session,omitempty"`
	Force   bool   `json:"force,omitempty"`
	Fence   int64  `json:"fence,omitempty"` // UpdateIssueRequest.Fence
}

// ReopenIssueRequest is POST /v0/issues/{id}/reopen; a reason is added as a comment.
type ReopenIssueRequest struct {
	Reason string `json:"reason,omitempty"`
	Fence  int64  `json:"fence,omitempty"` // UpdateIssueRequest.Fence
}

// IssueCommentRequest is POST /v0/issues/{id}/comments. ID is optional: a
// ULID the client minted, so a replayed write never duplicates.
type IssueCommentRequest struct {
	ID    string `json:"id,omitempty"`
	Text  string `json:"text"`
	Fence int64  `json:"fence,omitempty"` // UpdateIssueRequest.Fence
}

// IssueDepRequest is POST /v0/issues/{id}/deps: {id} depends on Target.
type IssueDepRequest struct {
	Target string `json:"target"`
	Type   string `json:"type,omitempty"` // default blocks
}

// IssueLabelRequest is POST /v0/issues/{id}/labels.
type IssueLabelRequest struct {
	Label string `json:"label"`
}

// IssueStats is GET /v0/issues/stats (beads' stats summary), over the
// issues the caller may read.
type IssueStats struct {
	TotalIssues      int `json:"total_issues"`
	OpenIssues       int `json:"open_issues"`
	InProgressIssues int `json:"in_progress_issues"`
	ClosedIssues     int `json:"closed_issues"`
	BlockedIssues    int `json:"blocked_issues"`
	DeferredIssues   int `json:"deferred_issues"`
	ReadyIssues      int `json:"ready_issues"`
	PinnedIssues     int `json:"pinned_issues"`
}

// IssueRestoreRequest is POST /v0/issues/restore: make a sqlite backup's copy
// a project's native store (SPEC 17.9).
type IssueRestoreRequest struct {
	Prefix string `json:"prefix,omitempty"` // the native tracker; default: the project's one
	From   string `json:"from,omitempty"`   // a backup id, file:///path or a path; default: every sqlite backup
	At     string `json:"at,omitempty"`     // a copy's time stamp, or the start of one; default: the newest
	DryRun bool   `json:"dry_run,omitempty"`
	// Force restores although the current store could not be copied first.
	Force bool `json:"force,omitempty"`
}

// IssueRestoreResult is what a restore did, or with DryRun would do.
type IssueRestoreResult struct {
	Project string `json:"project"`
	Copy    string `json:"copy"`
	DryRun  bool   `json:"dry_run,omitempty"`
	// Counts is what the store holds now, or with DryRun would hold.
	Counts map[string]int `json:"counts,omitempty"`
	// Redactions is how many listed comments the copy still held in clear.
	Redactions int `json:"redactions"`
	// Kept is where the store it replaced was copied to first.
	Kept  string   `json:"kept,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// IssueMoveRequest is POST /v0/issues/move: move open issues of the resolved
// project into project To on the same authority (SPEC "Native issues":
// moves). New ids are To's prefix plus the old id without the old prefix.
type IssueMoveRequest struct {
	To  string   `json:"to"`
	IDs []string `json:"ids"`
	// Children adds every open descendant of the ids.
	Children bool `json:"children,omitempty"`
	// TransferClaims moves claimed issues with their claim (else refused).
	TransferClaims bool `json:"transfer_claims,omitempty"`
	DryRun         bool `json:"dry_run,omitempty"`
}

// IssueMoveItem is one issue of a move: what it would do, or did.
type IssueMoveItem struct {
	ID     string `json:"id"`
	NewID  string `json:"new_id,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	Claim  string `json:"claim,omitempty"` // the claim's holder (lease holder, else claimed_by, else assignee)
	// Cross are its edges that become cross-project: "type:id" it depends
	// on, "id:type" depending on it.
	Cross []string `json:"cross,omitempty"`
	// Result: "move" (dry run), "moved", "already moved", "refused", or
	// "half moved" (copied but not removed: run the move again).
	Result string `json:"result"`
	Note   string `json:"note,omitempty"`
}

// IssueMoveResult is a move's plan (DryRun) or outcome, in move order
// (parents first).
type IssueMoveResult struct {
	From    string          `json:"from"`
	To      string          `json:"to"`
	DryRun  bool            `json:"dry_run,omitempty"`
	Items   []IssueMoveItem `json:"items"`
	Moved   int             `json:"moved"`
	Refused int             `json:"refused"`
}

// IssueProjectInfo is GET /v0/issues/projects: projects with native issues
// the caller sees, and their id prefix.
type IssueProjectInfo struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Prefix  string `json:"prefix"`
}

// IssueProjects wraps the list.
type IssueProjects struct {
	Projects []IssueProjectInfo `json:"projects"`
	// Skipped names joined team projects whose authority did not answer
	// (a replica; never empty-looking).
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
}

// IssueEvent is the data of every dev.zm.issue.* event: the issue as the
// daemon sees it after the change (without edges), and the change.
// dev.zm.issue.moved carries only {id, gone: true} to readers of the area
// the issue left.
type IssueEvent struct {
	Issue  *Issue       `json:"issue,omitempty"`
	Change *IssueChange `json:"change,omitempty"`
	ID     string       `json:"id,omitempty"`
	Gone   bool         `json:"gone,omitempty"`
	// Imported: one event per committed import batch and area (deprecated
	// with EventIssueImported).
	Imported []string `json:"imported,omitempty"`
}

// Issue event types.
const (
	EventIssueCreated   = "dev.zm.issue.created"
	EventIssueUpdated   = "dev.zm.issue.updated" // any field, status, label, dependency, claim, close or reopen
	EventIssueCommented = "dev.zm.issue.commented"
	// A comment edited by its author, or deleted (a tombstone) by its author
	// or a moderator (whose name the event then leaves out).
	EventIssueCommentEdited  = "dev.zm.issue.comment_edited"
	EventIssueCommentDeleted = "dev.zm.issue.comment_deleted"
	// A comment's text made to stop existing by a moderator: every reader
	// that keeps a copy overwrites it by the comment's id.
	EventIssueCommentRedacted = "dev.zm.issue.comment_redacted"
	EventIssueMoved           = "dev.zm.issue.moved" // to readers of the area it left: {id, gone}
	// Deprecated: nothing publishes it since the beads import was retired
	// (2026-11-10); older event logs still carry it.
	EventIssueImported = "dev.zm.issue.imported"
)

// Memory is one bd memory (beads' kv row "kv.memory.<key>"): PUT
// /v0/issues/memories/{key} answers it with action "remembered" or
// "updated", DELETE with action "deleted" and the value it had.
type Memory struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Action string `json:"action,omitempty"`
}

// MemoryList is GET /v0/issues/memories: user key -> text.
type MemoryList struct {
	Memories map[string]string `json:"memories"`
}

// SQLRequest is POST /v0/issues/sql: one read-only SELECT over the
// project's bd-shaped views.
type SQLRequest struct {
	Query string `json:"query"`
}

// SQLResult is bd sql's answer: values are null, numbers or strings
// (DATETIME columns as "YYYY-MM-DDTHH:MM:SSZ").
type SQLResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}
