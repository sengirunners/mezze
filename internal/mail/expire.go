package mail

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// Mail nobody will read piles up (ended sessions' mail, a machine's group
// mailbox).
// Mail to an ended session goes to the live holder of its ref (box.go), else
// to the group mailbox; what nobody takes there in GroupKeep expires: zm
// takes it as the machine, with no report. An ask notice to an ended session expires
// after NoticeKeep with no report: the ask's own record holds its outcome.

// GroupKeep is how long group mail (machine:<m>) waits for a taker before it
// expires.
const GroupKeep = 24 * time.Hour

// expiredListMax bounds the mail one expiry report names.
const expiredListMax = 20

// askNoticeSQL is 1 for an ask notice (provenance ask_notice), else 0.
const askNoticeSQL = `CASE WHEN json_valid(provenance) THEN coalesce(json_extract(provenance, '$.ask_notice'), 0) ELSE 0 END`

// groupExpireWhere picks open group mail a session would take: an agent's
// note, not needs=human (the machine's human reads that in their inbox).
const groupExpireWhere = `acked_at IS NULL AND kind = 'note' AND needs != 'human' AND audience = ''`

// sessionEnded reports whether addr is an ended or gone session for mail in
// project. A replica's session registered outside a team project and still
// listed by its heartbeat is live (SPEC 9).
func (s *Service) sessionEnded(ctx context.Context, project, addr string) bool {
	ss, ok := orchcore.Sessions(s.d).Lookup(ctx, addr)
	if ok && ss.State != orchapi.StateEnded {
		return false
	}
	if pe, can := orchcore.Sessions(s.d).(orchcore.PresenceElsewhere); can && s.team(project) {
		if es, ok := pe.Elsewhere(project, addr); ok && es.Live {
			return false
		}
	}
	return true
}

// heldBy is a ref's lease holder as one sweep saw it: to, the live session
// this daemon delivers to ("" = none); held, whether anyone live holds it at
// all (a person, another machine's session, the sender itself).
type heldBy struct {
	to   string
	held bool
}

// liveHolder is the live session holding the lease on c's ref, when it can
// take c: filed in the item's own area (the lease's), not c's ended recipient
// or its sender, and a session this daemon delivers to (this machine's, or a
// team project's). "" = none. held reports whether anyone live holds the ref
// (only then is the item not "nobody's"); a lookup that fails counts as held,
// so the mail keeps its old route. Leases only: the sweep never calls a
// tracker. memo caches the answer per ref for one sweep.
func (s *Service) liveHolder(ctx context.Context, project string, c row, memo map[string]heldBy, ended func(string) bool) (to string, held bool) {
	if !itemMail(c) || orchcore.Work(s.d) == nil {
		return "", true
	}
	h, seen := memo[c.Ref]
	if !seen {
		owner, err := orchcore.Work(s.d).OwnerOf(ctx, s.d.System(), project, c.Ref)
		a, perr := api.ParseAddress(owner)
		switch {
		case err != nil:
			h.held = true
		case owner == "":
		case perr != nil || a.Kind != api.KindSession:
			h.held = true // a person (or something else) holds it
		case ended(a.String()):
		default:
			h.held = true
			if a.Machine == s.machine() || s.team(project) {
				h.to = a.String()
			}
		}
		memo[c.Ref] = h
	}
	if h.to == c.Recipient || h.to == c.From {
		return "", h.held
	}
	return h.to, h.held
}

// systemCaller is this daemon acting as its machine (the system subject):
// the taker of what zm expires.
func (s *Service) systemCaller() caller {
	sys := s.d.System()
	c := caller{sub: sys, machine: s.machine()}
	c.addr, _ = api.ParseAddress(sys.Address)
	return c
}

// expireGroupMail expires group mail nobody took in GroupKeep: zm takes
// each as the machine (settling its orphaned original too). Mail about a work
// item first goes onto the item (unheld: a comment and one triage item); only
// item mail that also fails there is named in one short report to the owners,
// so nothing about a bead vanishes silently. Other mail expires with no
// report: the daily summary counts it and zm mail --to <box> still reads it.
func (s *Service) expireGroupMail(ctx context.Context, db *sql.DB, project string, ended func(string) bool) error {
	recip, args := s.groupRecip(project)
	old, err := listRows(ctx, db, project, groupExpireWhere+` AND `+recip+` AND created <= ? ORDER BY id LIMIT 500`,
		append(args, store.Millis(s.now().Add(-GroupKeep)))...)
	if err != nil || len(old) == 0 {
		return err
	}
	c := s.systemCaller()
	var lost []row
	for _, m := range old {
		if itemMail(m) && m.Via != unheldVia { // a triage item's item already holds its comment
			box, _ := api.ParseAddress(m.Recipient)
			if done, err := s.unheld(ctx, db, project, box.Machine, m, ended); err != nil {
				return err
			} else if done {
				continue
			}
			lost = append(lost, m)
		}
		if _, err := s.ack(ctx, c, db, m); err != nil {
			return err
		}
	}
	if len(lost) > 0 {
		ref := api.ProjectRef{ID: project, Slug: s.d.Projects.Slug(project)}
		if _, err := s.send(ctx, s.d.System(), ref, "", sendOpts{}, orchapi.SendMailRequest{To: "owners", Kind: orchapi.MailKindReport, Body: lostItemReport(lost)}); err != nil {
			return err
		}
	}
	s.d.Log.Info("mail: expired group mail nobody took", "project", project, "n", len(old), "item_mail_reported", len(lost))
	return nil
}

// lostItemReport names item mail that expired without reaching its item.
func lostItemReport(ms []row) string {
	var b strings.Builder
	fmt.Fprintf(&b, "zm: %d mail about work items reached nobody in %s and could not be put on the item, so zm closed it. Read one: zm thread <id>.\n", len(ms), shortDur(GroupKeep))
	for i, m := range ms {
		if i == expiredListMax {
			fmt.Fprintf(&b, "...and %d more\n", len(ms)-i)
			break
		}
		fmt.Fprintf(&b, "- %s re %s: %s\n", short(m.ID), m.Ref, firstLine(m.Body))
	}
	return strings.TrimRight(b.String(), "\n")
}

// expireAskNotices takes ask notices left unacked for NoticeKeep on an
// ended session (recip/rargs select the sessions this sweep covers): the
// ask's record holds the outcome, and a machine cannot act on it (SPEC 9
// rule 4), so nobody would ever read them.
func (s *Service) expireAskNotices(ctx context.Context, db *sql.DB, project, recip string, rargs []any, ended func(string) bool) error {
	old, err := listRows(ctx, db, project, `acked_at IS NULL AND kind = 'note' AND `+askNoticeSQL+` != 0 AND `+recip+` AND created <= ? ORDER BY id LIMIT 500`,
		append(rargs, store.Millis(s.now().Add(-NoticeKeep)))...)
	if err != nil {
		return err
	}
	c := s.systemCaller()
	for _, m := range old {
		if !ended(m.Recipient) {
			continue
		}
		if _, err := s.ack(ctx, c, db, m); err != nil {
			return err
		}
	}
	return nil
}

// expiredCounts counts, for the daily summary (noticeCounts), what this
// daemon expired in [lo, hi) ms: group mail nobody took, and ask notices to
// ended sessions.
func (s *Service) expiredCounts(ctx context.Context, db *sql.DB, lo, hi int64) (group, asks int, err error) {
	taker := s.d.System().Address
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE kind = 'note' AND needs != 'human'
		AND substr(recipient, 1, 8) = 'machine:' AND acked_by = ? AND acked_at >= ? AND acked_at < ? AND acked_at - created >= ?`,
		taker, lo, hi, GroupKeep.Milliseconds()).Scan(&group); err != nil {
		return 0, 0, err
	}
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE kind = 'note' AND `+askNoticeSQL+` != 0
		AND instr(recipient, '/') > 0 AND acked_by = ? AND acked_at >= ? AND acked_at < ?`, taker, lo, hi).Scan(&asks)
	return group, asks, err
}
