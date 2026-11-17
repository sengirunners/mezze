// Package orchcore is zm's orchestration seams: the in-process APIs zm's
// modules (work, mail, sessions, issues, homes, approvals, team) call each
// other through, which left potluck's kit/core. A module
// never imports another; it asks through these interfaces, reached from
// core.Deps with the typed getters in services.go.
package orchcore

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// The work module's seams, moved from potluck's kit/core/core.go and
// kit/core/work_read.go unchanged.

// TrackerHost is the work module's in-process API for the homes module
// (SPEC 17.9): what a backup or restore needs from the tracker registry and
// the mirror. Bindings themselves are d.Projects.File.
type TrackerHost interface {
	// TrackerDir is $ZM_HOME/trackers/<project>/<prefix>.
	TrackerDir(project, prefix string) string
	// TrackerView is a binding as the tracker routes show it.
	TrackerView(ctx context.Context, project, slug string, tr api.Tracker) orchapi.TrackerView
	// Describe is the tracker's adapter's describe answer.
	Describe(ctx context.Context, project string, tr api.Tracker) (orchapi.AdapterDescribe, error)
	// Relist drops tr's mirror and lists it afresh (its source went back in
	// time: a mirror keeps the newest of every row).
	Relist(ctx context.Context, project string, tr api.Tracker) error
	// Export writes a JSON Lines snapshot of tr's mirror into dir: the items
	// written and the file's path. at names the file and is when the mirror's
	// state is read. Retention is the caller's.
	Export(ctx context.Context, project, slug string, tr api.Tracker, dir string, at time.Time) (items int, path string, err error)
}

// SessionProjectEnder is an optional WorkService method: a session's bound
// leases in one project only (its machine left the project; SPEC 17.3).
type SessionProjectEnder interface {
	SessionLeftProject(ctx context.Context, address, project string) error
}

// LeaseHolders is implemented by the work module: every key of project
// with a live lease held by holder (daemon-internal; the presence answer
// tells a replica what its own session holds).
type LeaseHolders interface {
	HeldBy(ctx context.Context, project, holder string) ([]string, error)
	// FinishedBy is the keys, of those given, whose lease on holder ended
	// because the GitHub link closed the work.
	FinishedBy(project, holder string, keys []string) []string
}

// TrackerHealth is implemented by the work module next to WorkService:
// the trackers whose mirror syncs have kept failing for at least atLeast,
// in the projects sc reads whole (in memory, no tracker call: the notice
// line asks on every prompt).
type TrackerHealth interface {
	FailingTrackers(sc authz.Scope, atLeast time.Duration) []orchapi.TrackerFailure
}

// Snapshotter is implemented by the work module (items, comments, leases)
// and the mail module (mail): everything of team project that sub may read,
// redacted for sub, for a replica's snapshot (SPEC 17.6). kind names what v
// is: "item" (orchapi.WorkItem), "comment" (orchapi.Comment), "lease" (orchapi.Lease),
// "mail" (orchapi.Mail).
type Snapshotter interface {
	Snapshot(ctx context.Context, sub *authz.Subject, project string, emit func(kind string, v any) error) error
}

// LeaseFencer is implemented by the work module: CheckFence refuses a write
// sub makes under its lease on work item id of project (fence; 0 = none
// named: nil) unless that lease is still sub's and live (conflict, data
// orchapi.Refusal{ReasonStaleLease}). Native issues' door checks its writes
// with it. Not found rules are the caller's: it has located the item.
type LeaseFencer interface {
	CheckFence(ctx context.Context, sub *authz.Subject, project, id string, fence int64) error
}

// LeaseMover is implemented by the work module: zm project team|solo moves
// a project's leases between registry.db and its project.db (SPEC 17.3).
type LeaseMover interface {
	MoveLeases(ctx context.Context, project string, toTeam bool) (int, error)
}

// WorkMoves is implemented by a work module that follows moved native issues.
type WorkMoves interface {
	// MovedItem is the work item that id became after a move, as sub sees it:
	// ok is false when id did not move or sub cannot see what it became (the
	// same answer, so it says nothing about a hidden issue).
	MovedItem(ctx context.Context, sub *authz.Subject, id string) (p api.ProjectRef, newID string, ok bool, err error)
	// ItemMoved takes a native issue that left project out of its mirror and
	// tells its readers, then mirrors it in the project to and tells those
	// (to and newID are "" when that project binds no native tracker).
	ItemMoved(ctx context.Context, project, id, to, newID string)
}

// WorkCommenter is the work module's scoped in-process comment door.
type WorkCommenter interface {
	CommentTask(ctx context.Context, sub *authz.Subject, project, id, body string) error
}

// WorkRecorder is implemented by the work module next to WorkService, for the
// approvals module (SPEC 16): find what an ask is about, record its answer on
// the item whoever asked, and read what is waiting on a person.
type WorkRecorder interface {
	// ResolveItem is work item id in project as sub may read it, following a
	// move: its canonical id, area and owning project. Hidden answers like missing.
	ResolveItem(ctx context.Context, sub *authz.Subject, project, id string) (canon, area, resolvedProject string, err error)
	// RecordComment adds body to the item's history as sub, following a move.
	// writeID (a ULID) makes a retry return the stored comment.
	RecordComment(ctx context.Context, sub *authz.Subject, project, id, writeID, body string) error
	// WaitingItems is f's items sc reads that are not done.
	WaitingItems(ctx context.Context, sub *authz.Subject, sc authz.Scope, f orchapi.WaitingFilter) (orchapi.WorkPage, error)
}

// WorkLabeler is the labels of work item id in project as sub reads it,
// following a move (approvals: an ask goes to the person owner:<name> names).
type WorkLabeler interface {
	ItemLabels(ctx context.Context, sub *authz.Subject, project, id string) ([]string, error)
}

// WorkDescriber is the title, body and labels of work item id in project as
// sub reads it, following a move (approvals: a default never applies to an
// item that names an irreversible step).
type WorkDescriber interface {
	ItemText(ctx context.Context, sub *authz.Subject, project, id string) (title, body string, labels []string, err error)
}

// WorkService is the work module's in-process API.
type WorkService interface {
	// MailTarget says where mail to work item id in project goes: its live
	// lease holder, else its creator; area is the item's area. A not_found or
	// conflict *api.Error means refuse the mail (mail never starts work).
	MailTarget(ctx context.Context, sub *authz.Subject, project, id string) (to, via, area string, err error)
	// BindLease gives a managed session a bound lease on work item id (spawn
	// --work). It has a real deadline (Config.BoundLeaseTTL) that the work
	// ticker renews only while d.Sessions.Lookup(session).State is running.
	BindLease(ctx context.Context, sub *authz.Subject, project, id, session string) (orchapi.Lease, error)
	// SessionEnded releases every bound lease the session held.
	SessionEnded(ctx context.Context, address string) error
	// OwnerOf returns who holds the live lease on work item id, redacted for
	// sub ("" when nobody does). Same not_found rules as MailTarget.
	OwnerOf(ctx context.Context, sub *authz.Subject, project, id string) (string, error)
	// ClaimItem claims work item id in project for sub, as zm work claim
	// does: a lease (bound for a live session unless TTL is named) and the source's
	// claim. Native issues' own claim door uses it, so a native claim is a
	// lease with a deadline.
	ClaimItem(ctx context.Context, sub *authz.Subject, project, id string, req orchapi.ClaimRequest) (orchapi.ClaimResponse, error)
	// EndItemLease drops the live lease on work item id in project unless
	// keep holds it (daemon-internal: the caller already authorized the
	// write that ended the claim, e.g. a native close), with a released
	// event noting why. No lease: nothing happens.
	EndItemLease(ctx context.Context, project, id, keep, note string) error
	// ItemLeased reports whether any lease row, live or expired, names work
	// item id in project (an expired one is the ticker's to release).
	ItemLeased(ctx context.Context, project, id string) (bool, error)
}

// WorkItemReader is implemented by the work module: one item as sub reads
// it now (status, created, staleness), following moves. Approvals reads it
// for standing permissions' facts.
type WorkItemReader interface {
	ReadItem(ctx context.Context, sub *authz.Subject, project, id string) (orchapi.WorkItem, error)
}

// WorkReader is an optional work-owned seam for bounded scoped derivations.
// It reads the canonical work view and its freshness metadata, never raw tables.
type WorkReader interface {
	ReadWork(context.Context, *authz.Subject, orchapi.WorkQuery) (orchapi.WorkList, error)
}
