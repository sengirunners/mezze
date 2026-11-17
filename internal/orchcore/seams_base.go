package orchcore

import (
	"context"
	"errors"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// The mail, session and homes modules' seams, moved from potluck's
// kit/core/core.go unchanged (recipe steps 3 and 4):
// potluck now asks these modules only through its hooks (hooks.go).

// MailService is the mail module's in-process API.
type MailService interface {
	// Send routes and stores mail in project p (area may be "") as sub, exactly
	// like POST /v0/mail after resolution. The work module uses it with
	// d.System() to tell a live session its lease expired.
	Send(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, req orchapi.SendMailRequest) (orchapi.Mail, error)
	// Reply answers mail id in project as sub, exactly like POST
	// /v0/mail/{id}/reply: sub must be able to write in the mail's area and be
	// a party to it; the parent is taken (acked) when sub may ack it. The
	// approvals module answers an ask's mail this way once it is decided.
	Reply(ctx context.Context, sub *authz.Subject, project, id string, req orchapi.ReplyRequest) (orchapi.Mail, error)
}

// SessionService is the session module's in-process API.
type SessionService interface {
	// Lookup returns a session by address IGNORING scope (daemon-internal:
	// project resolution, lease liveness). Never return its result to a caller
	// without a Visible check.
	Lookup(ctx context.Context, address string) (orchapi.Session, bool)
	// Visible reports whether a reader with scope sc may see the session.
	Visible(ctx context.Context, sc authz.Scope, address string) bool
	// Live reports whether the session is running, idle or needs_human.
	Live(ctx context.Context, address string) bool
	// SetState records what a session is doing (mail sets needs_human when a
	// session sends needs=human mail, and clears it on the answer).
	SetState(ctx context.Context, address string, req orchapi.StateRequest) error
	// Nudge types one fixed line (never a mail body) into an idle managed
	// session nobody can be typing in, so it runs `zm inbox`. ok=false when it
	// cannot (with a person there, the mail waits for them).
	Nudge(ctx context.Context, address, line string) (ok bool, err error)
	// ManagedLive counts managed sessions still alive (daemon stop guard).
	ManagedLive(ctx context.Context) int
	// RestartBlockers lists the managed sessions a daemon restart would end
	// (the stop, restart and upgrade guards). A session that outlives the
	// daemon is not one of them.
	RestartBlockers(ctx context.Context) []orchapi.Session
}

// SessionCaller is implemented by the session module (owner): the
// registered session a socket caller's process runs inside, found by
// walking parent pids up from the kernel peer pid ("" when none). An
// in-process caller (no peer pid) is its subject's session. It errs when
// the peer process cannot be read or is gone: callers refuse then (fail
// closed). It is a guard against careless calls from an agent's own shell,
// not a boundary: a process started outside the session's tree escapes it.
type SessionCaller interface {
	CallerSession(ctx context.Context, sub *authz.Subject) (string, error)
	// CallerSessionNow is CallerSession that never waits (for a log line):
	// sure is false when it could not tell.
	CallerSessionNow(ctx context.Context, sub *authz.Subject) (origin string, sure bool, err error)
}

// SessionNames is implemented by the session module (owner): session names
// (SPEC 8 "Names"). Callers resolve a name once and keep the address;
// SessionService.Lookup never reads names, since a stored address must not
// reach whichever session has a name now.
type SessionNames interface {
	// ResolveName returns the live session on this machine that ref
	// (<name> or <machine>/<name>) names now, ignoring scope.
	ResolveName(ctx context.Context, ref string) (orchapi.Session, bool)
}

// SessionCaps is implemented by the session module: a session started for
// another machine's launcher acts in one project at most at that launcher's
// role (SPEC 1.3), applied when a request acts as it (Zm-As).
type SessionCaps interface {
	SessionCap(address string) (project string, role authz.Relation, ok bool)
}

// SessionLister is the scoped session list supplied by the session brick.
type SessionLister interface {
	List(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error)
}

// SessionKeeper decides what the session module does with its sessions when
// the daemon stops: keep them running in their holders (`zm daemon`) or end
// them (`zm daemon stop --force`, and every in-process daemon by default).
// Owner: session.
type SessionKeeper interface {
	KeepSessions(keep bool)
	// RestartNote says why kept sessions would end with the daemon anyway (a
	// service manager that kills its holders too); "" when they survive it.
	RestartNote(ctx context.Context) string
}

// SessionEnder reports actual results before a forced daemon stop responds.
type SessionEnder interface {
	EndManaged(ctx context.Context) (int, []api.DaemonSessionFailure, error)
}

// HolderReporter is the session module's view of session holders (SPEC 8,
// "Daemon restarts"), for GET /v0/health on the socket. Owner: session.
type HolderReporter interface {
	HolderReport() *orchapi.HolderReport
}

// PresenceReaper is implemented by the session module: ReapPresence ends
// the presence rows no replica stands behind any more (its machine holds no
// live node-bound token for the project, or its heartbeat is long gone).
// Call it after revoking a node-bound token (core.Deps.ReapPresence).
type PresenceReaper interface {
	ReapPresence(ctx context.Context)
}

// HomeService is the homes module's in-process API for the work module
// (SPEC 17.9): what tracker add and rm and a redaction need from a tracker's
// home state.
type HomeService interface {
	// BlocksRemove refuses tracker rm while a backup or restore of the
	// tracker runs.
	BlocksRemove(project, prefix string) error
	// Forget drops a removed tracker's home state.
	Forget(project, prefix string)
	// ScrubExports runs scrub on each export backup directory of the tracker,
	// holding its home lock.
	ScrubExports(project, slug, prefix string, scrub func(dir string) error) error
	// PurgeBackups is a redaction's last step, after every scrubber ran: each
	// sqlite backup of project's native trackers writes a fresh verified copy,
	// then deletes its older ones. fresh says a redaction just changed the
	// store; otherwise only backups still waiting from an earlier one run. An
	// error means older copies remain; the daemon keeps trying.
	PurgeBackups(ctx context.Context, project string, fresh bool) error
	// WaitIdle waits, a bounded while, for a home operation on project's native
	// trackers to end: it holds a lock a scrub needs and a read that keeps a
	// compaction out.
	WaitIdle(ctx context.Context, project string)
	// NewestCopy is the tracker's newest backup copy (nil: it has no backup).
	NewestCopy(project, slug, prefix string) *orchapi.BackupBrief
}

// UnreplacedCopy is a PurgeBackups error for a copy no current schedule
// replaces (an old or unscheduled tracker prefix): it is kept, and the purge
// fails until someone deletes it.
type UnreplacedCopy struct{ Path string }

func (e *UnreplacedCopy) Error() string {
	return "sqlite backup copy under an old or unscheduled prefix was not scrubbed: " + e.Path
}

// UnreplacedCopies splits a PurgeBackups error: the copies no schedule
// replaces, and whether anything else (a backup still waiting) failed too.
func UnreplacedCopies(err error) (paths []string, other bool) {
	if err == nil {
		return nil, false
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			p, o := UnreplacedCopies(e)
			paths, other = append(paths, p...), other || o
		}
		return paths, other
	}
	if gap, ok := err.(*UnreplacedCopy); ok {
		return []string{gap.Path}, false
	}
	// A wrapped join keeps both answers.
	if u := errors.Unwrap(err); u != nil {
		return UnreplacedCopies(u)
	}
	return nil, true
}

// BackupReporter is implemented by the homes module next to HomeService:
// the home's off-site backup at a glance (nil: none set up), for GET
// /v0/health on the socket and the sync answer a team replica gets, and the
// watchdog heartbeat's last send, for GET /v0/health (SPEC 17.9 "Off-site
// home backups"). Each reads one small file.
type BackupReporter interface {
	HomeBackup() *orchapi.HomeBackup
	Heartbeat() *orchapi.HeartbeatBrief
}
