// Package store is the native issue store brick (SPEC "Native issues"): the
// interface every driver implements and the types it speaks. The SQLite
// driver is internal/issues/store/sqlite; Postgres and Dolt drivers implement
// the same interface. Drivers know nothing about zm's authz: the service
// (internal/issues) passes which areas a caller may read (Areas) and an
// Authorize callback that every write calls inside its transaction.
//
// Every write goes through one funnel, ProjectStore.Apply(Mutation), and
// records one or more history rows (api.IssueChange) in the same
// transaction. History ids are ULIDs minted in commit order, so the history
// is also the project's changelog: Changes(since) is what a round C replica
// or an outbox attaches to (see Feed).
package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// ErrNotFound is returned for a missing issue (or one outside Areas).
var ErrNotFound = errors.New("issue not found")

// Store is one authority's issue storage, across projects.
type Store interface {
	// Project returns project's store, creating it when create is true
	// (else ErrNotFound when it has none).
	Project(ctx context.Context, project string, create bool) (ProjectStore, error)
	// Existing lists the projects that have a store, with their prefixes.
	Existing(ctx context.Context) ([]ProjectMeta, error)
	Close() error
}

// ProjectMeta names a project's store.
type ProjectMeta struct {
	Project string
	Prefix  string
}

// Areas is what a reader may see in one project: every area (All), or the
// listed ones ("" = project-level objects, only with All).
type Areas struct {
	All   bool
	Names []string
}

// Has reports whether area is visible.
func (a Areas) Has(area string) bool {
	if a.All {
		return true
	}
	for _, n := range a.Names {
		if n == area {
			return true
		}
	}
	return false
}

// ProjectStore is one project's issues.
type ProjectStore interface {
	Prefix(ctx context.Context) (string, error)
	SetPrefix(ctx context.Context, prefix string) error
	// Meta and SetMeta keep small store-level facts (import state).
	Meta(ctx context.Context, key string) (string, error)
	SetMeta(ctx context.Context, key, value string) error
	// Reset deletes every issue, comment, edge, label, history row,
	// counter and kv row (not the prefix).
	Reset(ctx context.Context) error

	// Get is one issue with labels, parent and counts (edges to hidden
	// issues not counted), or ErrNotFound when missing or not in vis.
	Get(ctx context.Context, id string, vis Areas) (orchapi.Issue, error)
	// Detail adds edges both ways (only to issues in vis) and comments.
	Detail(ctx context.Context, id string, vis Areas) (orchapi.IssueDetail, error)
	// List filters, sorts and pages (api.IssueQuery), only issues in vis.
	List(ctx context.Context, q orchapi.IssueQuery, vis Areas, now time.Time) (orchapi.IssueList, error)
	Comments(ctx context.Context, id string, vis Areas) ([]orchapi.IssueComment, error)
	// Compact rewrites the store without free pages and truncates its log,
	// so no old page keeps overwritten text (after a redaction).
	Compact(ctx context.Context) error
	// SearchComments lists comments on issues in vis matching q, by id
	// (oldest first), at most q.Limit.
	SearchComments(ctx context.Context, q orchapi.CommentQuery, vis Areas) ([]orchapi.IssueComment, error)
	History(ctx context.Context, id string, vis Areas) ([]orchapi.IssueChange, error)
	Stats(ctx context.Context, vis Areas, now time.Time) (orchapi.IssueStats, error)
	// Labels is every label on an issue in vis (and in areas, when not nil), sorted.
	Labels(ctx context.Context, vis Areas, areas []string) ([]string, error)

	// Apply runs one write in one transaction (see Mutation).
	Apply(ctx context.Context, m Mutation) (Result, error)

	Feed

	// Import writes a batch of source rows as they are (ids, times,
	// authors kept), upserting by id; Finish recomputes blocked state and
	// areas once every batch is in.
	Import(ctx context.Context, b ImportBatch) error
	FinishImport(ctx context.Context, areaOf AreaFunc) error
	// Counts is what the store holds (issues, comments, deps, labels,
	// history, kv): the importer compares it with what it read.
	Counts(ctx context.Context) (map[string]int, error)

	// Rearea recomputes every issue's area (the project's area rules
	// changed) and records an area_changed history row for each that moved.
	Rearea(ctx context.Context, areaOf AreaFunc) ([]Moved, error)

	// KV is the project's key-value rows whose key starts with prefix
	// (beads' config table: settings, and memories under "kv.memory.").
	// They are project-level objects with no history: the service checks
	// project-level read and write before calling.
	KV(ctx context.Context, prefix string) (map[string]string, error)
	// SetKV writes key; old is the value it replaced ("" = none).
	SetKV(ctx context.Context, key, value string) (old string, err error)
	// DeleteKV removes key; old is the value it had ("" = none).
	DeleteKV(ctx context.Context, key string) (old string, err error)
}

// Attacher is an optional Store door: each project's image attachments
// (SPEC "Native issues", attachments). They are kept apart from the issues:
// never in Snapshot, Changes' rows (beyond OpAttachmentNote's metadata), an
// export, a move, a store copy or a restore, so removing one removes it.
type Attacher interface {
	// Attachments is project's attachment store, made when create is true
	// (else ErrNotFound when it has none).
	Attachments(ctx context.Context, project string, create bool) (AttachmentStore, error)
	// AttachmentProjects lists the projects that have one.
	AttachmentProjects(ctx context.Context) ([]string, error)
}

// Attachment is one stored image's metadata.
type Attachment struct {
	orchapi.IssueAttachment
	IssueID string
	// Identity is the attacher's identity ULID: any session of it may
	// remove the attachment.
	Identity string
}

// ErrTooMany is an Add over its limit of live attachments.
var ErrTooMany = errors.New("too many attachments")

// AttachmentStore is one project's attachments. Live means not expired at
// now: an expired one is never answered, even before Delete removes it.
type AttachmentStore interface {
	// Add stores a with its bytes, unless the issue already has max live
	// attachments (ErrTooMany), in one transaction.
	Add(ctx context.Context, a Attachment, data []byte, max int, now time.Time) error
	// List is each issue's live attachments, oldest first (issues with none
	// are absent).
	List(ctx context.Context, issueIDs []string, now time.Time) (map[string][]Attachment, error)
	// Get is one live attachment of issueID with its bytes (ErrNotFound).
	Get(ctx context.Context, issueID, id string, now time.Time) (Attachment, []byte, error)
	// Expired is every attachment expired at now; Of every one of issueID.
	Expired(ctx context.Context, now time.Time) ([]Attachment, error)
	Of(ctx context.Context, issueID string) ([]Attachment, error)
	// Delete removes ids for good and answers which it removed (an id
	// already gone is left out): their pages are overwritten and the
	// write-ahead log truncated, so no copy of the bytes stays in the file.
	// A truncation another reader holds up is left pending, not an error:
	// Settle finishes it.
	Delete(ctx context.Context, ids []string) ([]string, error)
	// Settle truncates the write-ahead log a Delete left pending (nothing
	// when none is).
	Settle(ctx context.Context) error
}

// SQLReader is an optional driver door: one read-only SELECT over the
// project's issues as bd-shaped tables (bd's table and column names:
// issues, dependencies, labels, comments, events, config). The service lets
// only a reader of the whole project call it (raw SQL cannot be scoped to
// areas). A driver without it answers that bd sql is not supported.
type SQLReader interface {
	// ReadSQL runs query (one SELECT or WITH statement) and returns at most
	// maxRows rows (more = ErrTooManyRows).
	ReadSQL(ctx context.Context, query string, maxRows int, visibleEdge func(string) (bool, error)) (SQLResult, error)
}

// Restorer is an optional driver door for a store kept in a SQLite file: a
// copy of it, and replacing it with one (SPEC 17.9). The service holds the
// project's writes around OpenCopy and Replace.
type Restorer interface {
	// Copy runs write with a handle on project's store that may run VACUUM
	// INTO and does not hold up its writer. The store is made if it has none.
	Copy(ctx context.Context, project string, write func(context.Context, *sql.DB) error) error
	// OpenCopy opens the copy at path for its last writes before Replace,
	// migrated up to this binary. It refuses a file that is not an issue
	// store, and one with migrations this binary does not know
	// (ErrSchemaNewer). done closes it and folds its log in.
	OpenCopy(ctx context.Context, project, path string) (ps ProjectStore, done func() error, err error)
	// Replace makes the closed copy at path project's store, in one rename:
	// a crash leaves the old store or the new one, never neither. A
	// ProjectStore got earlier is closed.
	Replace(ctx context.Context, project, path string) error
}

// ErrSchemaNewer is a copy written by a newer binary: it cannot be read
// safely, and migrating down does not exist.
var ErrSchemaNewer = errors.New("schema newer than this binary")

// SQLResult is a query's columns and rows (values: nil, int64, float64 or
// string; a DATETIME column is "YYYY-MM-DDTHH:MM:SSZ", as bd sql printed it).
type SQLResult struct {
	Columns []string
	Rows    [][]any
}

// ErrTooManyRows is a ReadSQL result over its row cap; ErrTooLarge one over
// its size cap.
var (
	ErrTooManyRows = errors.New("too many rows")
	ErrTooLarge    = errors.New("answer too large")
)

// ErrNotSelect is a ReadSQL query that is not one SELECT (or WITH) statement.
var ErrNotSelect = errors.New("not a single SELECT statement")

// Feed is the narrow interface round C's replica sync and outbox attach to
// (SPEC 17.6); nothing implements sync yet.
//
//   - Changes returns history rows after the change id since ("" = from the
//     start), in commit order, at most limit. Each row has AreaBefore and
//     AreaAfter, so the sync can send a full upsert to readers of AreaAfter
//     and a tombstone to readers of AreaBefore only.
//   - Snapshot calls fn for every issue (with labels and edges) from one
//     read transaction and returns the last change id that transaction saw.
//   - Writes a replica replays go through Apply: Mutation.Base is the
//     replica's last seen Version (a field changed after it refuses with
//     ErrConflict), and a comment's client-minted ID makes a replay a no-op.
type Feed interface {
	Changes(ctx context.Context, since string, limit int) ([]orchapi.IssueChange, error)
	// LastChange is the id of the newest history row ("" = none).
	LastChange(ctx context.Context) (string, error)
	Snapshot(ctx context.Context, fn func(orchapi.Issue) error) (last string, err error)
}

// ErrConflict is a Base older than a change to a field the write touches.
var ErrConflict = errors.New("conflict")

// AreaFunc is the project's area rules: the area of an issue with this id
// and these labels.
type AreaFunc func(id string, labels []string) string

// Op is a write.
type Op string

const (
	OpCreate      Op = "create"
	OpUpdate      Op = "update"
	OpClaim       Op = "claim"
	OpClose       Op = "close"
	OpReopen      Op = "reopen"
	OpComment     Op = "comment"
	OpDepAdd      Op = "dep_add"
	OpDepRemove   Op = "dep_remove"
	OpLabelAdd    Op = "label_add"
	OpLabelRemove Op = "label_remove"
	// OpCommentEdit and OpCommentDelete change a comment (CommentEdit,
	// CommentDelete): the author edits; the author or a moderator deletes.
	OpCommentEdit   Op = "comment_edit"
	OpCommentDelete Op = "comment_delete"
	// OpCommentRedact (CommentID; moderators only) overwrites a comment's
	// text and every history copy of it with "[redacted]" and tombstones it.
	// An empty ID finds the comment's issue from CommentID (ErrNotFound-coded
	// when no issue here holds it). Its comment_redacted history row names
	// the comment and is at Mutation.At, when the text left the store; a
	// repeat with nothing left to overwrite writes none.
	OpCommentRedact Op = "comment_redact"
	// OpMoveIn, OpMoveOut and OpMoveDone move one issue between two
	// projects of the same Store (Move): move in copies it under its new id
	// (pending: it refuses writes), move out removes it from its old project
	// leaving a redirect (MovedTo), and done ends the copy's pending state.
	OpMoveIn   Op = "move_in"
	OpMoveOut  Op = "move_out"
	OpMoveDone Op = "move_done"
	// OpAttachmentNote (AttachmentNote) records that an image was attached
	// to, removed from or expired off the issue: a history row with its
	// metadata (never its bytes, which live in the AttachmentStore), so the
	// change feed and events carry it.
	OpAttachmentNote Op = "attachment_note"
)

// History kinds OpAttachmentNote records.
const (
	KindAttached          = "attached"
	KindAttachmentRemoved = "attachment_removed"
	KindAttachmentExpired = "attachment_expired"
)

// AttachmentNote is OpAttachmentNote's payload.
type AttachmentNote struct {
	Kind       string // KindAttached, KindAttachmentRemoved or KindAttachmentExpired
	Attachment orchapi.IssueAttachment
}

// Actor is who writes: a display name (beads' actor, kept in created_by,
// assignee, comment author) and the zm address it authenticated as.
type Actor struct {
	Name    string
	Address string
	// Identity is the actor's identity ULID: a comment's author is its
	// identity, so any session of it may edit the comment.
	Identity string
}

// CommentEdit edits comment ID's text; Base is the comment's last history
// id the editor saw ("" = no check).
type CommentEdit struct {
	ID, Text, Base string
}

// Mutation is one write. Exactly one of the op's payload fields is set.
type Mutation struct {
	Op    Op
	ID    string // the issue (create: "" for a new id, or an explicit one)
	Actor Actor
	At    time.Time

	Create  *orchapi.NewIssueRequest
	Update  *orchapi.UpdateIssueRequest
	Close   *orchapi.CloseIssueRequest
	Reopen  *orchapi.ReopenIssueRequest
	Comment *orchapi.IssueCommentRequest
	Dep     *orchapi.IssueDepRequest
	Label   string
	// Assignee is OpClaim's routing label ("" = Actor.Name): the assignee
	// the claim writes, compared instead of the name (ClaimIssueRequest).
	Assignee string
	// CommentEdit is OpCommentEdit's payload; CommentID OpCommentDelete's.
	CommentEdit *CommentEdit
	CommentID   string
	// Move is OpMoveIn's and OpMoveOut's payload (ID is the old id).
	Move *Move
	// Note is OpAttachmentNote's payload.
	Note *AttachmentNote

	// Moderate says whether the actor moderates comments on an issue in
	// area (its admins, the machine owner): a moderator deletes any
	// comment there.
	Moderate func(area string) bool

	// AreaOf is the project's area rules (called inside the transaction).
	AreaOf AreaFunc
	// Authorize is called inside the transaction once the issue's area
	// before and after the write are known (create: before == after); an
	// error aborts the write and is returned as is.
	Authorize func(before, after string) error
	// Check, when set on an OpUpdate, is called inside the transaction with
	// the issue as it is before the write (after Authorize); an error aborts
	// the write and is returned as is: a write conditional on what it read.
	Check func(cur orchapi.Issue) error
	// CanRead says whether an issue in area may be referenced (a dependency
	// target, a parent); a hidden one answers ErrNotFound for it.
	CanRead func(area string) bool
}

// Result is what a write did.
type Result struct {
	Issue   orchapi.Issue
	Changes []orchapi.IssueChange
	Comment *orchapi.IssueComment
	// Other issues whose blocked state the write changed (their own
	// events are the caller's to publish).
	Touched []orchapi.Issue
	// Noop: nothing changed (an idempotent repeat); no history row.
	Noop bool
	// Redacted is every text OpCommentRedact overwrote (the comment's and
	// its edits'), for the other copies to be scrubbed.
	Redacted []string
}

// Move is one issue moving between projects of one Store.
//
//   - Move in (on the target): writes From under NewID: its row, labels,
//     comments (ids kept), history (new ids in commit order, times and
//     actors kept) and edges, plus a "moved" history row. An edge to an id in
//     Rename goes to its new id; an edge to an issue the target does not
//     hold becomes an external edge naming the id (a cross-project edge).
//     The parent's child counter comes along; the home area is HomeArea.
//     The copy is pending: a write to it is refused until move done. Run
//     again, it rewrites the copy from From (a resumed move); it refuses
//     when NewID is another issue or the copy has writes of its own.
//   - Move out (on the source, after move in): removes the issue, its
//     comments and history, records a "moved" history row and the redirect,
//     and turns every edge to it into an external edge naming NewID. Run
//     again it is a no-op.
//   - Move done (on the target, after move out): the copy takes writes. Run
//     again (or with nothing pending) it is a no-op.
//
// A redirect away from an id ends when the issue moves back in under it,
// and a create naming an id that moved out is refused.
type Move struct {
	Project string // the other project: the source on move in, the target on move out and done
	NewID   string
	// HomeArea is the copy's home area (the area it was filed in): the
	// service keeps the old one when the target has an area of that name.
	HomeArea string
	From     *Moving           // move in only: what Export read
	Rename   map[string]string // move in: old -> new id of every issue in the same run
}

// Moving is one issue as Export read it from its project.
type Moving struct {
	Issue    orchapi.Issue // as Get answers it, all areas
	HomeArea string        // the area it was filed in
	// Deps are its edges both ways (IssueID is the issue: its own; else an
	// edge to it), for the plan.
	Deps []orchapi.IssueDep
	// Rows is the driver's own copy of everything move in writes: only the
	// same driver reads it.
	Rows any
}

// Mover is the move doors of a ProjectStore (SPEC "Native issues": moving
// open issues between projects of one authority).
type Mover interface {
	// Export reads id for a move (ErrNotFound when missing).
	Export(ctx context.Context, id string) (Moving, error)
	// MovedTo is where id went when it moved out of this project (ok false:
	// it did not).
	MovedTo(ctx context.Context, id string) (newID, project string, ok bool, err error)
	// MovedFrom is the old id and project of an issue that moved in.
	MovedFrom(ctx context.Context, id string) (oldID, project string, ok bool, err error)
	// MoveEdges is every move record this store keeps into or out of
	// (project, id): its own redirect and arrival when project is this
	// one, and any id here that left for it or came from it.
	MoveEdges(ctx context.Context, project, id string) ([]MoveEdge, error)
}

// MoveEdge is one recorded move, from (FromProject, FromID) to (ToProject, ToID).
type MoveEdge struct {
	FromProject, FromID, ToProject, ToID string
	At                                   time.Time
}

// Moved is an issue Rearea moved.
type Moved struct {
	Issue  orchapi.Issue
	Change orchapi.IssueChange
}

// ImportBatch is source rows, as they were in the source tracker.
type ImportBatch struct {
	Issues   []orchapi.Issue // with Labels; Version ignored
	Comments []orchapi.IssueComment
	Deps     []orchapi.IssueDep
	History  []ImportedEvent
	KV       map[string]string // the source's config/memories (beads' config table)
	Counters map[string]int    // child counters: parent -> last child number
}

// ImportedEvent is one source history row. SourceID keeps a re-import
// idempotent.
type ImportedEvent struct {
	SourceID string
	IssueID  string
	Kind     string
	Actor    string
	Old, New string
	Comment  string
	At       time.Time
}

// EncodeKey and DecodeKey are a keyset cursor (api.IssueList.Next): the sort
// time (Unix ms) and the id of the last issue of a page.
func EncodeKey(ms int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ms, 10) + "|" + id))
}

// DecodeKey reads EncodeKey.
func DecodeKey(s string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", err
	}
	t, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return 0, "", errors.New("bad cursor")
	}
	ms, err := strconv.ParseInt(t, 10, 64)
	return ms, id, err
}
