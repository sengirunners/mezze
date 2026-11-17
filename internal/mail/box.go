package mail

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// boxWhere is the inbox filter for c in project p: direct mail, the
// project's group mailbox on c's machine and announcements not yet seen
// (sessions registered in p; an unknown session gets direct mail only), the owners mailbox (owners) and needs=human mail sent to
// this machine's sessions (the machine's human). ok=false: nothing here.
//
// needs=human mail is never shown to a session (it is never surfaced inside
// an agent's terminal); the machine's human sees it instead.
//
// reports: the reports filed for a human caller instead (SPEC 9 "Reports"),
// from the same mailboxes; a report is never in anyone's inbox.
func (s *Service) boxWhere(c caller, p string, all, reports bool) (string, []any, bool) {
	if reports && !c.human() {
		return "", nil, false
	}
	open := "acked_at IS NULL"
	if all {
		open = "1=1"
	}
	me := c.addr.String()
	var parts []string
	var args []any
	switch c.addr.Kind {
	case api.KindSession:
		parts = append(parts, "(recipient = ? AND "+open+")")
		args = append(args, me)
		if c.inProject(p) { // a session on this machine, or a replica's known by presence
			parts = append(parts, "(recipient = ? AND "+open+")")
			args = append(args, "machine:"+c.addr.Machine)
			ann := "(recipient = 'all' AND created >= ? AND NOT EXISTS (SELECT 1 FROM mail_seen WHERE mail_id = mail.id AND address = ?))"
			if all {
				ann = "(recipient = 'all' AND created >= ? AND ? != '')"
			}
			parts = append(parts, ann)
			args = append(args, store.Millis(s.now().Add(-AnnounceWindow)), me)
		}
		return "needs != 'human' AND kind != 'report' AND (" + strings.Join(parts, " OR ") + ")", args, true
	case api.KindHuman:
		// A digest item waits for its 08:00 Pacific (SPEC 9 "For you").
		mine, ma := c.mineSQL()
		parts = append(parts, "("+mine+" AND "+open+" AND "+heldSQL+")")
		args = append(append(args, ma...), store.Millis(s.now()))
		if w, a, ok := ownerAreas(c.sub.Scope, p); ok {
			parts = append(parts, "(recipient = 'owners' AND "+open+" AND "+w+")")
			args = append(args, a...)
		}
		if c.machineHuman {
			lo, hi := sessionRange(c.machine)
			parts = append(parts, "(needs = 'human' AND "+open+" AND (recipient = ? OR (recipient >= ? AND recipient < ?)))")
			args = append(args, "machine:"+c.machine, lo, hi)
		}
		kind := "kind != 'report'"
		if reports {
			kind = "kind = 'report'"
		}
		return kind + " AND (" + strings.Join(parts, " OR ") + ")", args, true
	case api.KindMachine:
		return "kind != 'report' AND recipient = ? AND " + open, []any{me}, true
	}
	return "", nil, false
}

// sessionRange bounds the session addresses on machine ("<machine>/..."),
// as an index range: lo <= recipient < hi ('0' follows '/').
func sessionRange(machine string) (lo, hi string) { return machine + "/", machine + "0" }

// ownerAreas is the SQL selecting the areas of project where sc is owner.
func ownerAreas(sc authz.Scope, project string) (string, []any, bool) {
	if sc.Relation(authz.Object{Project: project}) == authz.Owner {
		return "1=1", nil, true
	}
	_, areas := sc.Areas(project)
	var own []any
	for _, a := range areas {
		if sc.Relation(authz.Object{Project: project, Area: a}) == authz.Owner {
			own = append(own, a)
		}
	}
	if len(own) == 0 {
		return "", nil, false
	}
	return "area IN (" + strings.TrimSuffix(strings.Repeat("?,", len(own)), ",") + ")", own, true
}

// inbox is the caller's mail across sc, newest first.
func (s *Service) inbox(ctx context.Context, c caller, sc authz.Scope, all bool, limit int) ([]row, error) {
	return s.box(ctx, c, sc, all, false, limit)
}

// box is inbox, or (reports) the reports filed for the caller.
func (s *Service) box(ctx context.Context, c caller, sc authz.Scope, all, reports bool, limit int) ([]row, error) {
	return s.query(ctx, sc, c.addr.String(), limit, func(p scoped) (string, []any, bool) { return s.boxWhere(c, p.id, all, reports) })
}

// markSeen records that a session has seen the announcements in rs.
func (s *Service) markSeen(ctx context.Context, c caller, rs []row) {
	if c.addr.Kind != api.KindSession {
		return
	}
	for _, r := range rs {
		if r.Recipient != "all" {
			continue
		}
		if db, err := s.d.Store.Project(authz.All(), authz.Read, r.Project); err == nil {
			_, _ = db.ExecContext(ctx, `INSERT OR IGNORE INTO mail_seen (mail_id, address) VALUES (?, ?)`, r.ID, c.addr.String())
		}
	}
}

// groupByProject orders rows by project slug, newest first inside each.
func (s *Service) groupByProject(rs []row) {
	slug := map[string]string{}
	for _, r := range rs {
		if _, ok := slug[r.Project]; !ok {
			slug[r.Project] = s.d.Projects.Slug(r.Project)
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if a, b := slug[rs[i].Project], slug[rs[j].Project]; a != b {
			return a < b
		}
		if !rs[i].Created.Equal(rs[j].Created) {
			return rs[i].Created.After(rs[j].Created)
		}
		return rs[i].ID > rs[j].ID
	})
}

// mayAck applies the ack rules to a mail the caller can already see.
func (s *Service) mayAck(ctx context.Context, c caller, m row) error {
	if err := c.sub.Check(authz.Write, authz.Object{Project: m.Project, Area: m.Area}, "mail", m.ID); err != nil {
		return err
	}
	if err := s.refuseForYou(ctx, m); err != nil {
		return err
	}
	if m.Needs == orchapi.NeedsHuman && m.Recipient != "all" && !c.human() {
		return api.Errorf(api.CodeForbidden, "mail %s needs a human; only a human may ack it", short(m.ID)).
			WithHint("tell your human; they read it with: zm inbox")
	}
	if !s.party(c, m) {
		e := api.Errorf(api.CodeForbidden, "mail %s is not addressed to you (%s)", short(m.ID), c.addr.String())
		if c.unknown && (m.Recipient == "all" || strings.HasPrefix(m.Recipient, "machine:")) {
			e = e.WithHint("this session is not registered in a project, so it takes no group mail: install the hooks (zm install claude|codex) or start it with zm run inside a project")
		}
		return e
	}
	return nil
}

// ack takes m for the caller. The first ack wins and records the taker;
// later callers get Acked=false and who took it (redacted). Announcements
// are never taken: acking one only marks it seen.
func (s *Service) ack(ctx context.Context, c caller, db *sql.DB, m row) (orchapi.AckResponse, error) {
	me := c.addr.String()
	if m.Recipient == "all" {
		_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO mail_seen (mail_id, address) VALUES (?, ?)`, m.ID, me)
		return orchapi.AckResponse{ID: m.ID, Acked: true, AckedBy: me}, err
	}
	err := s.commit(ctx, db, m.Project, func(tx *sql.Tx) ([]pub, error) {
		var evs []pub
		var err error
		_, m, evs, err = s.takeTx(ctx, tx, me, m)
		return evs, err
	})
	if err != nil {
		return orchapi.AckResponse{}, err
	}
	if m.Needs == orchapi.NeedsHuman && m.AckedAt != nil {
		if err := s.answered(ctx, m.From); err != nil {
			return orchapi.AckResponse{}, err
		}
	}
	if m.AckedBy == me {
		return orchapi.AckResponse{ID: m.ID, Acked: true, AckedBy: me, AckedAt: m.AckedAt}, nil
	}
	// AckedAt rides along so a team replica queueing this ack closes its copy
	// of the mail now rather than waiting for the taker's event.
	return orchapi.AckResponse{ID: m.ID, Acked: false, AckedBy: s.d.Redact(ctx, c.sub, m.AckedBy), AckedAt: m.AckedAt}, nil
}

// takeTx takes m for me in tx: the first taker wins, and taking a group
// copy settles the original, and the other way round. Each settled sibling
// publishes its own mail.acked: a replica learns only from events, and would
// show it unread for good. m comes back as stored; evs only when
// won.
func (s *Service) takeTx(ctx context.Context, tx *sql.Tx, me string, m row) (won bool, _ row, evs []pub, err error) {
	now := store.Millis(s.now())
	res, err := tx.ExecContext(ctx, `UPDATE mail SET acked_by = ?, acked_at = ? WHERE id = ? AND acked_at IS NULL`, me, now, m.ID)
	if err != nil {
		return false, m, nil, err
	}
	n, _ := res.RowsAffected()
	var settled []string
	if won = n == 1; won {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM mail WHERE acked_at IS NULL AND (id = ? OR orphan_of = ?)`, m.OrphanOf, m.ID)
		if err != nil {
			return false, m, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, m, nil, err
			}
			settled = append(settled, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, m, nil, err
		}
		rows.Close()
		for _, id := range settled {
			if _, err := tx.ExecContext(ctx, `UPDATE mail SET acked_by = ?, acked_at = ? WHERE id = ? AND acked_at IS NULL`, me, now, id); err != nil {
				return false, m, nil, err
			}
		}
	}
	if m, err = getRow(ctx, tx, m.Project, m.ID); err != nil || !won {
		return won, m, nil, err
	}
	evs = []pub{{orchapi.EventMailAcked, m}}
	for _, id := range settled {
		sib, err := getRow(ctx, tx, m.Project, id)
		if err != nil {
			return false, m, nil, err
		}
		evs = append(evs, pub{orchapi.EventMailAcked, sib})
	}
	return won, m, evs, nil
}

// answered clears needs_human on a session once none of the needs=human
// mail it sent is still open.
func (s *Service) answered(ctx context.Context, sender string) error {
	a, err := api.ParseAddress(sender)
	if err != nil || a.Kind != api.KindSession || a.Machine != s.machine() {
		return nil
	}
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		n, err := s.openHumanCount(ctx, p.DB, p.ID, a.String())
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	if ss, ok := s.lookup(ctx, a.String()); ok && ss.State == orchapi.StateNeedsHuman && orchcore.Sessions(s.d) != nil {
		err := orchcore.Sessions(s.d).SetState(ctx, a.String(), orchapi.StateRequest{State: orchapi.StateIdle, Note: "answered"})
		if err != nil && !api.IsCode(err, api.CodeNotImplemented) && !api.IsCode(err, api.CodeNotFound) {
			return err
		}
	}
	return nil
}

func noticeNoReply(id string) error {
	return api.Errorf(api.CodeBadRequest, "mail %s is a notice from zm: nobody reads a reply", short(id)).
		WithHint("zm ack " + short(id))
}

// reply answers parent: to its sender (or, for your own mail, to whoever you
// sent it to; mail to several people, to every other member), threaded,
// same project, area and ref; then takes the parent.
func (s *Service) reply(ctx context.Context, c caller, parent row, req orchapi.ReplyRequest) (row, error) {
	if parent.Origin != nil {
		return row{}, api.Errorf(api.CodeConflict, "imported mail has no verified reply transport")
	}
	if parent.Kind == orchapi.MailKindNotice {
		return row{}, noticeNoReply(parent.ID)
	}
	if req.Silent {
		// Nothing to tell the person who just acted: take the parent only.
		if parent.AckedAt == nil && parent.Recipient != "all" && s.mayAck(ctx, c, parent) == nil {
			db, err := s.d.Store.Project(authz.All(), authz.Read, parent.Project)
			if err != nil {
				return row{}, err
			}
			if _, err := s.ack(ctx, c, db, parent); err != nil {
				return row{}, err
			}
		}
		return parent, nil
	}
	to := parent.From
	if to == c.addr.String() {
		to = parent.To
	}
	if len(parent.Members) > 0 {
		// Mail to several people: the reply reaches every other member who
		// still reads here. One who lost access must not silence the rest.
		others := make([]string, 0, len(parent.Members))
		for _, m := range parent.Members {
			if m == c.addr.String() {
				continue
			}
			err := s.checkHuman(ctx, parent.Project, parent.Area, m)
			if api.IsCode(err, api.CodeNotFound) {
				continue
			}
			if err != nil {
				return row{}, err
			}
			others = append(others, m)
		}
		if len(others) == 0 {
			return row{}, api.Errorf(api.CodeNotFound, "nobody else in this conversation reads %s any more", s.d.Projects.Slug(parent.Project))
		}
		to = strings.Join(others, ",")
	}
	kind := ""
	if to == "all" {
		kind = orchapi.MailKindAnnounce
	}
	ref := api.ProjectRef{ID: parent.Project, Slug: s.d.Projects.Slug(parent.Project)}
	m, err := s.send(ctx, c.sub, ref, parent.Area, sendOpts{areaExplicit: true, parent: &parent, daemonTo: true},
		orchapi.SendMailRequest{To: to, Body: req.Body, Needs: req.Needs, Kind: kind, ID: req.ID, ClientRequestID: req.ClientRequestID, Audience: parent.Audience})
	if err != nil {
		return row{}, err
	}
	if parent.AckedAt == nil && parent.Recipient != "all" && s.mayAck(ctx, c, parent) == nil {
		db, err := s.d.Store.Project(authz.All(), authz.Read, parent.Project)
		if err != nil {
			return row{}, err
		}
		if _, err := s.ack(ctx, c, db, parent); err != nil {
			// The reply may already be committed. A caller with a durable
			// internal ID retries that reply, then repairs this ack.
			return row{}, err
		}
	}
	// Repair a death after the ack commit but before the session-state update.
	// The reply's durable ID prevents another send while this is retried.
	if parent.Needs == orchapi.NeedsHuman && parent.Recipient != "all" && s.mayAck(ctx, c, parent) == nil {
		if err := s.answered(ctx, parent.From); err != nil {
			return row{}, err
		}
	}
	s.nudge(ctx, m)
	return m, nil
}

// thread lists the mail in m's thread the caller may read, oldest first.
func (s *Service) thread(ctx context.Context, sc authz.Scope, me string, m row) ([]row, error) {
	for _, p := range s.readable(sc, me, authz.PersonalMailReader(core.SubjectOf(ctx))) {
		if p.id != m.Project {
			continue
		}
		return listRows(ctx, p.db, p.id, "thread = ? AND "+p.where+" ORDER BY created, id LIMIT 500", append([]any{m.Thread}, p.args...)...)
	}
	return nil, api.NotFound("mail", m.ID)
}

// visibleAddr reports whether a filter address is one the caller may name;
// filtering on a hidden session would reveal that it sent or got mail.
func (s *Service) visibleAddr(ctx context.Context, sub *authz.Subject, addr string) bool {
	return s.d.Visible(ctx, sub.Scope, addr)
}

// settleNotices acks notices older than NoticeKeep, whoever they were for.
func (s *Service) settleNotices(ctx context.Context, db *sql.DB, project string) error {
	old, err := listRows(ctx, db, project, `kind = ? AND acked_at IS NULL AND created <= ? ORDER BY id LIMIT 500`,
		orchapi.MailKindNotice, store.Millis(s.now().Add(-NoticeKeep)))
	if err != nil {
		return err
	}
	for _, m := range old {
		err := s.commit(ctx, db, project, func(tx *sql.Tx) ([]pub, error) {
			at := s.now()
			res, err := tx.ExecContext(ctx, `UPDATE mail SET acked_by = ?, acked_at = ? WHERE id = ? AND acked_at IS NULL`, m.From, store.Millis(at), m.ID)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, nil
			}
			m.AckedBy, m.AckedAt = m.From, &at
			return []pub{{orchapi.EventMailAcked, m}}, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SetClock and SweepOrphans let tests drive the sweep from another package.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) SweepOrphans(ctx context.Context) error { return s.sweepOrphans(ctx) }

// sweepOrphans moves mail left unacked for OrphanAfter on an ended (or
// gone) session, once: to the live session holding its ref (a copy, via
// "orphan of <addr>"; taking either settles both), else, about a work item,
// onto the item with one triage item (triage.go), else to its machine's
// group mailbox in the same project. It also hands untaken group mail to
// triage (triage.go) and expires what nobody will read (expire.go).
func (s *Service) sweepOrphans(ctx context.Context) error {
	if orchcore.Sessions(s.d) == nil {
		return nil
	}
	cutoff := store.Millis(s.now().Add(-OrphanAfter))
	lo, hi := sessionRange(s.machine())
	ended := map[string]bool{}
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		isEnded := func(addr string) bool {
			e, seen := ended[p.ID+" "+addr]
			if !seen {
				e = s.sessionEnded(ctx, p.ID, addr)
				ended[p.ID+" "+addr] = e
			}
			return e
		}
		if err := s.settleNotices(ctx, p.DB, p.ID); err != nil {
			return err
		}
		if err := s.settleMachineNotes(ctx, p.DB, p.ID); err != nil {
			return err
		}
		if err := s.triageGroupMail(ctx, p.DB, p.ID, isEnded); err != nil {
			return err
		}
		if err := s.expireGroupMail(ctx, p.DB, p.ID, isEnded); err != nil {
			return err
		}
		// Sessions on this machine; in a team project, its replicas' too
		// (known by presence, SPEC 17.3).
		recip, rargs := "recipient >= ? AND recipient < ?", []any{lo, hi}
		if s.team(p.ID) {
			recip, rargs = "instr(recipient, '/') > 0", nil
		}
		if err := s.expireAskNotices(ctx, p.DB, p.ID, recip, rargs, isEnded); err != nil {
			return err
		}
		cands, err := listRows(ctx, p.DB, p.ID, `orphaned = 0 AND orphan_of = '' AND acked_at IS NULL AND kind = 'note'
			AND audience = '' AND needs != 'human' AND `+askNoticeSQL+` = 0 AND `+recip+` AND created <= ? ORDER BY id LIMIT 500`, append(rargs, cutoff)...)
		if err != nil {
			return err
		}
		holders := map[string]heldBy{}
		for _, c := range cands {
			if !isEnded(c.Recipient) {
				continue
			}
			ra, _ := api.ParseAddress(c.Recipient)
			to, via := "machine:"+ra.Machine, "orphan of "+c.Recipient
			if c.Via == unheldVia { // a triage item whose taker ended: the next taker, never a holder
				done, err := s.unheld(ctx, p.DB, p.ID, ra.Machine, c, isEnded)
				if err != nil {
					return err
				}
				if done {
					continue
				}
				via = unheldVia // still the one triage item about its ref
			}
			// Held by someone it cannot go to (the sender, a person): today's route.
			h, held := "", true
			if c.Via != unheldVia {
				h, held = s.liveHolder(ctx, p.ID, c, holders, isEnded)
			}
			if h != "" {
				to = h // mail about work goes to whoever does it now
			} else if !held {
				done, err := s.unheld(ctx, p.DB, p.ID, ra.Machine, c, isEnded)
				if err != nil {
					return err
				}
				if done {
					continue // nobody does it: on the item, with one triage item
				}
			}
			cp := c
			cp.ID, cp.Recipient, cp.Via, cp.OrphanOf, cp.Orphaned = newID(), to, via, c.ID, false
			cp.Created, cp.AckedBy, cp.AckedAt = s.now(), "", nil
			if err := s.deliver(ctx, p.DB, p.ID, cp, c.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
