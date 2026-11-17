package issues

import (
	"context"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// An operator acknowledges that a redaction gap (a stop zm cannot prove
// clean) was dealt with out of band: its copies were checked or destroyed
// by hand, or the gap is the false alarm a crash between a good purge and
// the stop's clean mark leaves. The acknowledgement marks that one stop
// clean, so a later redaction of the comment no longer reports it, and
// keeps who, when, the stop and why it was a gap in issue_redaction_acks.
// It searches no text and deletes no backup: it only clears the record.

var redactionAckMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "issues/0003_redaction_acks", Script: `
CREATE TABLE issue_redaction_acks (
	comment_id TEXT NOT NULL,
	project    TEXT NOT NULL,
	issue_id   TEXT NOT NULL,
	why        TEXT NOT NULL, -- the gap it was
	who        TEXT NOT NULL,
	at         INTEGER NOT NULL
);
CREATE INDEX issue_redaction_acks_comment ON issue_redaction_acks (comment_id);`},
}

// ackGap is POST /v0/issues/{id}/comments/{cid}/ack-gap: whoever may
// redact the comment names one gap as their own conflict names it. A stop
// in a project the caller cannot read is never matched, so it answers like
// a stop that is not there; one in another project they read but do not
// moderate is refused (403).
func (s *Service) ackGap(ctx context.Context, sub *authz.Subject, l located, cid, stop string) (any, error) {
	if err := s.redactable(ctx, sub, l, cid); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(stop)
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		name = name[:i] // pasted with its "(why)"
	}
	if name == "" {
		return nil, api.Errorf(api.CodeBadRequest, "stop is required: one stop as the redaction's conflict names it (project:issue)")
	}
	gaps, err := s.redactionGaps(ctx, cid)
	if err != nil {
		return nil, err
	}
	var g *redactionGap
	for i := range gaps {
		if n, ok := gapName(sub, gaps[i]); ok && n == name {
			g = &gaps[i]
			break
		}
	}
	if g == nil {
		return nil, api.NotFound("redaction gap", name)
	}
	// Clearing another project's stop takes its moderator, as redacting there would.
	if g.project != "" && g.project != l.project && !moderates(sub, g.project, "") {
		return nil, api.Errorf(api.CodeForbidden, "only an admin of %s, or the machine owner, acknowledges its stop", g.project)
	}
	at := s.now()
	reg := s.d.Store.Registry()
	tx, err := reg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Only the gap read above: a concurrent acknowledgement finds none.
	res, err := tx.ExecContext(ctx, `UPDATE issue_redaction_stops SET why = ?, at = ? WHERE comment_id = ? AND project = ? AND issue_id = ? AND why = ?`,
		stopClean, at.UnixMilli(), cid, g.project, g.id, g.why)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, api.NotFound("redaction gap", name)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_redaction_acks (comment_id, project, issue_id, why, who, at) VALUES (?, ?, ?, ?, ?, ?)`,
		cid, g.project, g.id, g.why, sub.Address, at.UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	left, err := s.redactionGaps(ctx, cid)
	if err != nil {
		return nil, err
	}
	out := orchapi.RedactionGapAck{Comment: cid, Stop: name, Why: g.why, By: s.d.RedactFor(ctx, sub, sub.Address), At: at.UTC().Truncate(time.Millisecond), Remaining: []string{}}
	for _, r := range left {
		n, _ := gapName(sub, r)
		out.Remaining = append(out.Remaining, n+" ("+r.why+")")
	}
	return out, nil
}

// gapName is a gap as sub's conflict names it: "project:issue", the hop
// "old>new", or, for a stop in a project sub cannot read, a placeholder
// (ok false: it names nothing sub may act on).
func gapName(sub *authz.Subject, g redactionGap) (string, bool) {
	switch {
	case g.project == "":
		return g.id, true
	case !sub.Scope.SeesProject(g.project):
		return "a stop in another project", false
	default:
		return g.project + ":" + g.id, true
	}
}
