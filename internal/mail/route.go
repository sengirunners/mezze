package mail

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

// sendOpts tunes one send.
type sendOpts struct {
	// areaExplicit: the caller named the area (it beats the ref's area).
	areaExplicit bool
	// parent is the mail this answers (reply endpoint or reply_to): the area
	// is the parent's, and needs defaults to any (a reply is an answer, not a
	// question; only fresh mail to a human defaults to needs=human).
	parent *row
	// daemonTo: the recipient was chosen by the daemon from the parent (the
	// reply endpoint), not supplied by the caller, so it skips the session
	// visibility check; output is redacted. A caller-supplied `to` (POST
	// /v0/mail with reply_to) is always checked.
	daemonTo bool
	// direct: a client's own POST /v0/mail. A fresh agent note to a person
	// is refused there (asks only); zm's in-process sends still
	// file one as a report.
	direct bool
	// group is the request of a send to several people, which every
	// copy's client_request_id is checked against.
	group *orchapi.SendMailRequest
}

// routed is one mail routed and checked, not yet stored. done: m is final
// already (a replayed request, or mail forwarded to another machine).
type routed struct {
	m          row
	done       bool
	db         *sql.DB
	actor, key string
	hash       string
	lim        counted
}

// Send implements core.MailService: route and store mail in project p as
// sub, exactly like POST /v0/mail after resolution (area "" = the ref's area,
// else project-level), then wake the recipient.
func (s *Service) Send(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, req orchapi.SendMailRequest) (orchapi.Mail, error) {
	if sub == nil {
		return orchapi.Mail{}, api.Errorf(api.CodeUnauthenticated, "no caller")
	}
	m, err := s.send(ctx, sub, p, area, sendOpts{areaExplicit: area != ""}, req)
	if err != nil {
		return orchapi.Mail{}, err
	}
	s.nudge(ctx, m)
	return s.sentAnswer(ctx, sub, m), nil
}

// Reply implements core.MailService: answer mail id in project as sub,
// with the same checks as POST /v0/mail/{id}/reply.
func (s *Service) Reply(ctx context.Context, sub *authz.Subject, project, id string, req orchapi.ReplyRequest) (orchapi.Mail, error) {
	if sub == nil {
		return orchapi.Mail{}, api.Errorf(api.CodeUnauthenticated, "no caller")
	}
	parent, _, err := s.find(ctx, sub.Scope.Only(project), sub.Address, id)
	if err != nil {
		return orchapi.Mail{}, err
	}
	if err := sub.Check(authz.Write, authz.Object{Project: parent.Project, Area: parent.Area}, "mail", parent.ID); err != nil {
		return orchapi.Mail{}, err
	}
	c := s.callerOf(ctx, sub)
	if !s.party(c, parent) {
		return orchapi.Mail{}, api.Errorf(api.CodeForbidden, "you are not a party to mail %s", short(parent.ID))
	}
	m, err := s.reply(ctx, c, parent, req)
	if err != nil {
		return orchapi.Mail{}, err
	}
	return s.redact(ctx, sub, m), nil
}

// send validates, routes (SPEC section 9), stores and publishes one mail,
// or one copy per person for mail to several people (sendGroup).
// It does not nudge; callers do, once everything else is settled.
func (s *Service) send(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, o sendOpts, req orchapi.SendMailRequest) (row, error) {
	if len(req.ClientRequestID) > 128 {
		return row{}, api.Errorf(api.CodeBadRequest, "client_request_id exceeds 128 bytes")
	}
	if orchapi.IsMailGroup(req.To) {
		return s.sendGroup(ctx, sub, p, area, o, req)
	}
	r, err := s.route(ctx, sub, p, area, o, req)
	if err != nil || r.done {
		return r.m, err
	}
	ms, err := s.store(ctx, []routed{r})
	if err != nil {
		return row{}, err
	}
	m := ms[0]
	if m.Needs == orchapi.NeedsHuman && m.HeldFor == "" && m.Kind != orchapi.MailKindReport && sub.Session != "" && m.From == sub.Session {
		note := "waiting on mail"
		if m.Audience == "" {
			note += " " + short(m.ID)
		}
		s.setState(ctx, sub.Session, orchapi.StateNeedsHuman, note)
	}
	return m, nil
}

// sendGroup sends one private two-party copy to each person req.To names
// (SPEC 9 "Mail to several people"). Every copy is routed and checked
// before any is stored, then all are stored together: one refusal sends
// nothing. Each copy carries the members and the first copy's id (batch);
// fresh copies share that id as their thread too.
func (s *Service) sendGroup(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, o sendOpts, req orchapi.SendMailRequest) (row, error) {
	recipients, members, err := orchapi.MailGroup(sub.Address, req.To)
	if err != nil {
		return row{}, err
	}
	o.group = &req
	rs := make([]routed, 0, len(recipients))
	for i, to := range recipients {
		cp := req
		cp.To = to
		if i > 0 {
			cp.ID = "" // a replica's write id names the first copy
		}
		if req.ClientRequestID != "" {
			cp.ClientRequestID = req.ClientRequestID + "/" + to
		}
		r, err := s.route(ctx, sub, p, area, o, cp)
		switch {
		case err != nil:
			return row{}, err
		case r.done && i == 0:
			return r.m, nil // stored before, all copies together
		case r.done:
			return row{}, api.Errorf(api.CodeConflict, "client_request_id already used with a different payload or operation")
		}
		rs = append(rs, r)
	}
	batch := rs[0].m.ID
	for i := range rs {
		rs[i].m.Members, rs[i].m.Batch = members, batch
		if rs[i].m.ReplyTo == "" {
			rs[i].m.Thread = batch
		}
	}
	ms, err := s.store(ctx, rs)
	if err != nil {
		return row{}, err
	}
	return ms[0], nil
}

// store commits routed mail (one project) in one transaction with its
// client_request_ids, and publishes it. A request stored meanwhile answers
// the mail stored then.
func (s *Service) store(ctx context.Context, rs []routed) ([]row, error) {
	out := make([]row, len(rs))
	for i, r := range rs {
		out[i] = r.m
	}
	err := s.commit(ctx, rs[0].db, rs[0].m.Project, func(tx *sql.Tx) ([]pub, error) {
		for i, r := range rs {
			if r.key == "" {
				continue
			}
			ex, found, err := requestReplay(ctx, tx, r.m.Project, r.actor, r.key, r.hash)
			switch {
			case err != nil:
				return nil, err
			case found && i == 0:
				out = []row{ex}
				return nil, nil
			case found:
				return nil, api.Errorf(api.CodeConflict, "client_request_id already used with a different payload or operation")
			}
		}
		// Send limits (limits.go): counted here, in the transaction that
		// stores the mail, so a replay above never counts twice.
		now, l := s.now(), s.limits(rs[0].m.Project)
		evs := make([]pub, 0, len(rs))
		if err := s.checkSends(ctx, tx, rs, l, now); err != nil {
			return nil, err
		}
		for _, r := range rs {
			if err := recordSend(ctx, tx, r.m, r.lim, now); err != nil {
				return nil, err
			}
			if r.key != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO mail_requests(actor,request_id,payload_hash,mail_id) VALUES (?,?,?,?)`, r.actor, r.key, r.hash, r.m.ID); err != nil {
					return nil, err
				}
			}
			if err := insertRow(ctx, tx, r.m); err != nil {
				return nil, err
			}
			evs = append(evs, pub{orchapi.EventMailSent, r.m})
		}
		return evs, nil
	})
	return out, err
}

// route validates and routes one mail (SPEC section 9) without storing it.
func (s *Service) route(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, o sendOpts, req orchapi.SendMailRequest) (routed, error) {
	db, err := s.d.Store.Project(sub.Scope, authz.Write, p.ID)
	if err != nil {
		return routed{}, err
	}
	hashed := req
	if o.group != nil {
		hashed = *o.group
	}
	hash := payloadHash(struct {
		Request orchapi.SendMailRequest
		Area    string
		Parent  string
	}{hashed, area, func() string {
		if o.parent != nil {
			return o.parent.ID
		}
		return ""
	}()})
	actor := requestActor(sub)
	if req.ClientRequestID != "" {
		if ex, found, err := requestReplay(ctx, db, p.ID, actor, req.ClientRequestID, hash); err != nil {
			return routed{}, err
		} else if found {
			if err := sub.Check(authz.Write, authz.Object{Project: p.ID, Area: ex.Area}, "mail", ex.ID); err != nil {
				return routed{}, err
			}
			return routed{m: ex, done: true}, nil
		}
	}
	req.Body = textclean.Clean(req.Body)
	if textclean.Blank(req.Body) {
		return routed{}, api.Errorf(api.CodeBadRequest, "empty mail body")
	}
	kind, needs, err := normalise(req.Kind, req.Needs)
	if err != nil {
		return routed{}, err
	}
	if kind == orchapi.MailKindNotice {
		if sub.Via != authz.ViaSystem {
			return routed{}, api.Errorf(api.CodeBadRequest, "kind %q: want note or announce", kind)
		}
		o.areaExplicit = true // filed exactly where zm says: "" is project level, not the session's area
	}
	to, err := s.parseTo(ctx, sub, req.To)
	if err != nil {
		return routed{}, err
	}
	if kind == orchapi.MailKindNudge && to.Kind != api.KindHuman && to.Kind != api.KindOwners {
		return routed{}, api.Errorf(api.CodeBadRequest, "a nudge goes to a person: human:<name> or owners")
	}
	if kind == orchapi.MailKindNudge {
		// A nudge informs; a decision is an ask, so a nudge never holds its sender at needs_human.
		if needs == orchapi.NeedsHuman {
			return routed{}, api.Errorf(api.CodeBadRequest, "a nudge needs nothing; a decision only a person can make is an ask").
				WithHint("zm ask new")
		}
		needs = orchapi.NeedsAny
	}
	if req.Digest && to.Kind != api.KindHuman {
		return routed{}, api.Errorf(api.CodeBadRequest, "only mail to a person (human:<name>) waits for a digest")
	}
	if to.Kind == api.KindHuman && req.Audience == "" && kind != orchapi.MailKindNotice {
		// For you (SPEC 9): mail to a person is theirs alone. An ask's mail
		// (recorded under its id first) keeps the restricted-mail rule.
		ask, err := s.isAsk(ctx, p.ID, req.ID)
		if err != nil {
			return routed{}, err
		}
		if !ask {
			req.Audience = orchapi.MailAudienceParticipants
		}
	}
	if (to.Kind == api.KindBroadcast) != (kind == orchapi.MailKindAnnounce) {
		return routed{}, api.Errorf(api.CodeBadRequest, "only announcements go to all, and announcements only go to all").
			WithHint("zm send all --kind announce <body>")
	}
	from := sub.Address
	if from == "" {
		return routed{}, api.Errorf(api.CodeBadRequest, "the caller has no mail address")
	}
	ref := ""
	if strings.TrimSpace(req.Ref) != "" {
		ra, err := api.ParseAddress(req.Ref)
		if err != nil || ra.Kind != api.KindWork {
			return routed{}, api.Errorf(api.CodeBadRequest, "ref %q is not a work item id (<source>:<ref>)", req.Ref)
		}
		ref = ra.String()
	}

	id := newID()
	if req.ID != "" {
		if !ids.Valid(req.ID) {
			return routed{}, api.Errorf(api.CodeInternal, "bad caller mail id %q", req.ID)
		}
		id = req.ID
		// A team replica's replayed write (SPEC 17.6): the mail it already
		// stored under this id, never a second copy.
		if ex, err := getRow(ctx, db, p.ID, id); err == nil && req.ClientRequestID == "" {
			if ex.From != from {
				return routed{}, api.Errorf(api.CodeConflict, "write id %s is already used", id)
			}
			return routed{m: ex, done: true}, nil
		}
	}
	m := row{Mail: orchapi.Mail{ID: id, Project: p.ID, From: from, To: to.String(), Kind: kind, Needs: needs,
		Body: req.Body, Created: s.now()}, AskNotice: orchcore.IsAskNotice(ctx)}
	m.Thread = m.ID

	// Threading: reply_to must be mail in this project the caller is a party to.
	if o.parent == nil && strings.TrimSpace(req.ReplyTo) != "" {
		sc := sub.Scope.Only(p.ID)
		par, _, err := s.find(ctx, sc, sub.Address, req.ReplyTo)
		if err != nil {
			return routed{}, err
		}
		if !s.party(s.callerOf(ctx, sub), par) {
			return routed{}, api.Errorf(api.CodeForbidden, "you are not a party to mail %s", short(par.ID))
		}
		if par.Origin != nil {
			return routed{}, api.Errorf(api.CodeConflict, "imported mail has no verified reply transport")
		}
		if par.Kind == orchapi.MailKindNotice {
			return routed{}, noticeNoReply(par.ID)
		}
		o.parent = &par
	}
	if o.parent != nil {
		if o.parent.Origin != nil {
			return routed{}, api.Errorf(api.CodeConflict, "imported mail has no verified reply transport")
		}
		if o.parent.Audience != "" {
			if !slices.Contains(o.parent.Participants, to.String()) && !slices.Contains(o.parent.Members, to.String()) {
				return routed{}, api.Errorf(api.CodeForbidden, "private thread recipient must be a participant")
			}
			req.Audience = o.parent.Audience
		}
		m.ReplyTo, m.Thread = o.parent.ID, o.parent.Thread
		m.ParentRestricted = restricted(*o.parent)
		if ref == "" {
			ref, m.RefArea = o.parent.Ref, o.parent.RefArea
		}
		area = o.parent.Area
		if needs == "" {
			needs = orchapi.NeedsAny
		}
	} else if ref != "" && ref != to.String() {
		// The ref must be an item the sender can see; its area is the default area.
		_, _, ra, err := s.mailTarget(ctx, sub, p.ID, ref)
		switch {
		case err == nil:
		case api.IsCode(err, api.CodeConflict): // visible, but nobody to route to: fine for a ref
			// ra is the item's area as work located it (labels included);
			// AreaForWork from the id alone missed label areas.
		default:
			return routed{}, err
		}
		m.RefArea = ra
		if !o.areaExplicit {
			area = ra
		}
	}

	// Routing reads only `to` and `ref`; the body is never scanned.
	switch to.Kind {
	case api.KindBroadcast:
		m.Recipient, m.Via = "all", "announce"
	case api.KindOwners:
		m.Recipient, m.Via = "owners", "owners"
		if needs == "" {
			needs = orchapi.NeedsHuman
		}
	case api.KindHuman:
		m.Recipient, m.Via = to.String(), "direct"
		if needs == "" {
			needs = orchapi.NeedsHuman
		}
	case api.KindSession:
		if to.Machine != s.machine() && !s.team(p.ID) {
			if req.ClientRequestID != "" || req.Audience != "" {
				return routed{}, api.Errorf(api.CodeConflict, "private or retry-safe mail requires native authority delivery")
			}
			return forwarded(s.forward(ctx, sub, db, m, area, to))
		}
		// A team project's authority stores mail for its replicas' sessions
		// (known by presence) like its own; they fetch it through the proxy.
		// Hidden and missing answer the same not_found, naming the caller's
		// own input (never a redacted placeholder).
		nf := api.NotFound("session", req.To)
		ss, known := s.lookup(ctx, to.String())
		switch {
		case o.daemonTo:
			// A reply reaches the parent's sender wherever that session is
			// registered now, even one this daemon never knew (SPEC 9): the
			// parent proves the address. Filed in the parent's area.
		case known && ss.Project == p.ID:
			if !s.d.Visible(ctx, sub.Scope, to.String()) {
				return routed{}, nf
			}
			// Mail to a session is filed in the session's area unless the
			// sender named one (or a ref did): a member mailing a guest's
			// session from the repo root would otherwise file it at project
			// level, where the guest can never read it (the sender saw
			// "sent", the guest's inbox stayed empty).
			if !o.areaExplicit && ref == "" && ss.Area != "" {
				area = ss.Area
			}
		default:
			// Registered in another project or none (SPEC 9 "Across projects").
			a, err := s.acrossProjects(ctx, sub, p.ID, to, ss, known, area, o.areaExplicit || ref != "", nf)
			if err != nil {
				return routed{}, err
			}
			area = a
		}
		m.Recipient, m.Via = to.String(), "direct"
	case api.KindWork:
		target, via, itemArea, err := s.mailTarget(ctx, sub, p.ID, to.String())
		if err != nil {
			return routed{}, err
		}
		ta, err := api.ParseAddress(target)
		if err != nil || ta.Kind == api.KindWork || ta.Kind == api.KindBroadcast {
			return routed{}, api.Errorf(api.CodeInternal, "work item %s routes to a bad address", to.String())
		}
		if via == "" {
			via = "holder of " + to.String()
		}
		area, m.RefArea = itemArea, itemArea
		if ref == "" {
			ref = to.String()
		}
		m.Recipient, m.Via = ta.String(), via
		if (ta.Kind == api.KindSession || ta.Kind == api.KindMachine) && ta.Machine != s.machine() && !s.team(p.ID) {
			if req.ClientRequestID != "" || req.Audience != "" {
				return routed{}, api.Errorf(api.CodeConflict, "private or retry-safe mail requires native authority delivery")
			}
			return forwarded(s.forward(ctx, sub, db, withRef(m, ref, needs), area, ta))
		}
	case api.KindMachine:
		if to.Machine != s.machine() {
			if !s.team(p.ID) {
				if req.ClientRequestID != "" || req.Audience != "" {
					return routed{}, api.Errorf(api.CodeConflict, "private or retry-safe mail requires native authority delivery")
				}
				return forwarded(s.forward(ctx, sub, db, m, area, to))
			}
			if !s.groupOK(ctx, sub, p.ID, to.Machine) {
				return routed{}, api.NotFound("machine", req.To)
			}
		}
		m.Recipient, m.Via = to.String(), "group"
		if ref != "" { // a ref held by a session on that machine goes straight to it
			if target, via, _, err := s.mailTarget(ctx, sub, p.ID, ref); err == nil && strings.HasPrefix(via, "holder of") {
				if ta, err := api.ParseAddress(target); err == nil && ta.Kind == api.KindSession && ta.Machine == to.Machine {
					m.Recipient, m.Via = ta.String(), via
				}
			}
		}
	default:
		return routed{}, api.Errorf(api.CodeBadRequest, "cannot send mail to %s", to.String())
	}
	if needs == "" {
		needs = orchapi.NeedsAny
	}
	if needs == orchapi.NeedsAgent && (to.Kind == api.KindHuman || to.Kind == api.KindOwners || strings.HasPrefix(m.Recipient, "human:")) {
		return routed{}, api.Errorf(api.CodeBadRequest, "needs=agent mail cannot go to %s", m.Recipient).
			WithHint("send it to a session, a work item or machine:<m>")
	}
	// People take decisions only (SPEC 9 "Reports"): an agent's mail that
	// would reach a person, other than an ask's own mail, is filed as a
	// report: readable, never in an inbox, Decisions, a push or the notice.
	switch {
	case m.Kind == orchapi.MailKindReport:
		if !orchapi.ToPerson(m.Recipient, needs) {
			return routed{}, api.Errorf(api.CodeBadRequest, "a report goes to a person (human:<name> or owners), not %s", m.Recipient)
		}
		if o.direct && !realReply(o.parent, from, to.String()) {
			// Mail the send limits refuse anyway costs no judge call.
			pm := withRef(m, ref, needs)
			pm.Area = area
			c, err := s.countedAs(ctx, sub, pm, req.ID)
			if err != nil {
				return routed{}, err
			}
			c.to = to.String()
			if err := s.checkSends(ctx, db, []routed{{m: pm, lim: c}}, s.limits(p.ID), s.now()); err != nil {
				return routed{}, err
			}
			// A step in a report is lost: it goes on an ask.
			if err := orchapi.RefuseChoreMail(ctx, s.judgeCaller(), from, m.Kind, m.Recipient, needs, m.Body); err != nil {
				return routed{}, err
			}
		}
		needs, req.Digest = orchapi.NeedsHuman, false
	case m.Kind == orchapi.MailKindNote && orchapi.AgentSender(from) && orchapi.ToPerson(m.Recipient, needs):
		ask, err := s.isAsk(ctx, p.ID, req.ID)
		if err != nil {
			return routed{}, err
		}
		if !ask && o.direct && !realReply(o.parent, from, to.String()) {
			if err := orchapi.RefusePersonMail(from, m.Kind, m.Recipient, needs); err != nil {
				return routed{}, err
			}
		}
		if !ask {
			m.Kind, needs, req.Digest, m.Note = orchapi.MailKindReport, orchapi.NeedsHuman, false, orchapi.ReportNote
		}
	}
	if req.Audience != "" {
		if req.Audience != orchapi.MailAudienceParticipants || (to.Kind != api.KindHuman && to.Kind != api.KindSession) {
			return routed{}, api.Errorf(api.CodeBadRequest, "participants audience requires a direct human or session recipient")
		}
		m.Audience, m.Participants = req.Audience, []string{from, m.Recipient}
		if pa := orchcore.People(s.d); pa != nil && to.Kind == api.KindHuman {
			// The person's other accounts, when this one shares its mail.
			for _, a := range pa.ShareMailWith(ctx, m.Recipient) {
				if !slices.Contains(m.Participants, a) {
					m.Participants = append(m.Participants, a)
				}
			}
		}
	}
	m = withRef(m, ref, needs)
	m.Area = area
	if req.Digest {
		held := nextDigest(m.Created)
		m.HeldUntil, m.ForYou = &held, orchapi.ForYouDigest
	} else if personal(m) && m.From != m.Recipient {
		m.ForYou = orchapi.ForYouKnow
	}
	if err := sub.Check(authz.Write, authz.Object{Project: p.ID, Area: area}, "area", area); err != nil {
		return routed{}, err
	}
	if m.Needs == orchapi.NeedsHuman && req.ID != "" {
		// An ask's mail (its row is in before it sends): held for an outside
		// party, the row and its mail.sent event say so.
		m.HeldFor = s.heldAsks(ctx, m.Project)[m.ID]
	}
	if m.Recipient != "owners" && strings.HasPrefix(m.Recipient, "human:") {
		if err := s.checkHuman(ctx, p.ID, area, m.Recipient); err != nil {
			return routed{}, err
		}
	}
	c, err := s.countedAs(ctx, sub, m, req.ID)
	if err != nil {
		return routed{}, err
	}
	if !o.daemonTo {
		c.to = to.String()
	}
	return routed{m: m, db: db, actor: actor, key: req.ClientRequestID, hash: hash, lim: c}, nil
}

// forwarded is route's answer for mail forward stored itself.
func forwarded(m row, err error) (routed, error) {
	return routed{m: m, done: true}, err
}

// groupOK reports whether sub may mail machine's group mailbox in team
// project: machine is a replica joined to it, and sub reads the whole
// project or is on that machine itself. An area-only reader (a guest)
// learns nothing about which other machines joined: refused like a
// machine that never did.
func (s *Service) groupOK(ctx context.Context, sub *authz.Subject, project, machine string) bool {
	if !(sub.Scope.WholeProject(project) || (sub.Node != "" && sub.Machine == machine)) {
		return false
	}
	for _, p := range s.d.Projects.MachineProjects(ctx, machine) {
		if p == project {
			return true
		}
	}
	return false
}

// acrossProjects applies SPEC 9 "Across projects" to mail in project to
// session to, which is registered outside it (known: this daemon has a row
// for it, in another project or none). It returns the area the mail is
// filed in, or nf (exactly a missing session's answer). This machine's own
// session: a reader of the whole project who may also see the session where
// it is registered. A replica's: one joined to the project whose latest
// heartbeat for it listed the session, addressed by the readers who see it
// (orchcore.ElsewhereShared, with what the replica's token reads now).
func (s *Service) acrossProjects(ctx context.Context, sub *authz.Subject, project string, to api.Address, ss orchapi.Session, known bool,
	area string, chosen bool, nf error) (string, error) {
	if to.Machine == s.machine() {
		// The owner's session (the owner reads every area of P). One the
		// sender cannot see where it is registered (its project, or its area
		// there) is not confirmed by an answer.
		if !sub.Scope.WholeProject(project) || !known ||
			(ss.Project != "" && ss.Project != api.ProjectNone && !s.d.Visible(ctx, sub.Scope, to.String())) {
			return "", nf
		}
		return area, nil
	}
	pe, ok := orchcore.Sessions(s.d).(orchcore.PresenceElsewhere)
	if !ok || !s.team(project) || !slices.Contains(s.d.Projects.MachineProjects(ctx, to.Machine), project) {
		return "", nf
	}
	es, ok := pe.Elsewhere(project, to.String())
	if !ok || !es.Live {
		return "", nf
	}
	// What the replica reads of P now (its token's scope, never the one a
	// heartbeat recorded: a downgrade or revocation counts at once).
	rsc, ok := s.d.Projects.TokenScope(ctx, es.Token)
	if !ok {
		return "", nf
	}
	whole, areas := rsc.Areas(project)
	if !whole && len(areas) == 0 {
		return "", nf
	}
	all, shared := orchcore.ElsewhereShared(sub.Scope, project, whole, areas)
	if !all {
		if len(shared) == 0 {
			return "", nf
		}
		areas = shared // an area reader files it in an area it shares with the replica
	}
	switch {
	case whole || slices.Contains(areas, area):
		return area, nil
	case !chosen && len(areas) == 1:
		return areas[0], nil // a guest's replica reads its one area
	}
	return "", api.Errorf(api.CodeBadRequest, "%s reads only some areas of this project, and %s is not one of them", to.Machine, areaName(area)).
		WithHint("name an area " + to.Machine + " reads: zm send --area <area>")
}

func areaName(area string) string {
	if area == "" {
		return "the project level"
	}
	return "area " + area
}

func withRef(m row, ref string, needs orchapi.Needs) row {
	m.Ref = ref
	if needs == "" {
		needs = orchapi.NeedsAny
	}
	m.Needs = needs
	return m
}

// realReply: a reply_to that answers its parent's other party, as the reply
// endpoint would address it. Naming any other recipient is fresh mail, so a
// reply_to cannot carry a note past the asks-only rule.
func realReply(parent *row, from, to string) bool {
	if parent == nil {
		return false
	}
	want := parent.From
	if want == from {
		want = parent.To
	}
	return to == want
}

func normalise(kind string, needs orchapi.Needs) (string, orchapi.Needs, error) {
	switch kind = strings.ToLower(strings.TrimSpace(kind)); kind {
	case "":
		kind = orchapi.MailKindNote
	case orchapi.MailKindNote, orchapi.MailKindAnnounce, orchapi.MailKindNotice, orchapi.MailKindNudge, orchapi.MailKindReport:
	default:
		return "", "", api.Errorf(api.CodeBadRequest, "kind %q: want note, announce, nudge or report", kind)
	}
	switch needs = orchapi.Needs(strings.ToLower(strings.TrimSpace(string(needs)))); needs {
	case "", orchapi.NeedsAny, orchapi.NeedsAgent, orchapi.NeedsHuman:
	default:
		return "", "", api.Errorf(api.CodeBadRequest, "needs %q: want any, agent or human", needs)
	}
	return kind, needs, nil
}

// mailTarget asks the work module where mail about id goes. Mail never
// starts work: no holder and no creator is a refusal.
func (s *Service) mailTarget(ctx context.Context, sub *authz.Subject, project, id string) (to, via, area string, err error) {
	if orchcore.Work(s.d) == nil {
		return "", "", "", api.Errorf(api.CodeUnavailable, "the work module is not running")
	}
	to, via, area, err = orchcore.Work(s.d).MailTarget(ctx, sub, project, id)
	var e *api.Error
	if err != nil && errors.As(err, &e) && e.Code == api.CodeConflict && e.Hint == "" {
		err = e.WithHint("nobody holds " + id + "; see `zm who " + id + "`, or mail machine:" + s.machine())
	}
	return to, via, area, err
}

func (s *Service) lookup(ctx context.Context, addr string) (orchapi.Session, bool) {
	if orchcore.Sessions(s.d) == nil {
		return orchapi.Session{}, false
	}
	return orchcore.Sessions(s.d).Lookup(ctx, addr)
}

// checkHuman refuses a human recipient who could not read the mail: the
// identity must exist and be able to read (project, area). Missing and
// unable answer the same not_found.
func (s *Service) checkHuman(ctx context.Context, project, area, addr string) error {
	a, _ := api.ParseAddress(addr)
	nf := api.NotFound("human", addr)
	idents, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return err
	}
	for _, id := range idents {
		if id.Name != a.Human {
			continue
		}
		tuples, err := s.d.Projects.Tuples(ctx, id.ID)
		if err != nil {
			return err
		}
		if authz.FromTuples(tuples, nil).Can(authz.Read, authz.Object{Project: project, Area: area}) {
			return nil
		}
		return nf
	}
	return nf
}

// forward hands mail for another machine to the Forwarder, keeping the
// sender's record here. Without a Forwarder (v0) it is not_found. It does
// not pass store, so whoever wires a Forwarder applies the send limits
// (limits.go) here too.
func (s *Service) forward(ctx context.Context, sub *authz.Subject, db *sql.DB, m row, area string, to api.Address) (row, error) {
	f := s.forwarder()
	if f == nil {
		kind := "session"
		if to.Kind == api.KindMachine {
			kind = "machine"
		}
		return row{}, api.NotFound(kind, to.String()).WithHint("mail to another machine is not part of zm v0")
	}
	if err := sub.Check(authz.Write, authz.Object{Project: m.Project, Area: area}, "area", area); err != nil {
		return row{}, err
	}
	m.Area = area
	if m.Recipient == "" {
		m.Recipient = to.String()
	}
	if m.Needs == "" {
		m.Needs = orchapi.NeedsAny
	}
	m.Via = strings.TrimSpace(m.Via + " forwarded to " + to.Machine)
	if _, err := f.Forward(ctx, to.Machine, m.Mail); err != nil {
		var e *api.Error
		if !errors.As(err, &e) {
			err = api.Errorf(api.CodeUnavailable, "could not reach %s", to.Machine)
		}
		return row{}, err
	}
	if err := s.commit(ctx, db, m.Project, func(tx *sql.Tx) ([]pub, error) {
		return []pub{{orchapi.EventMailSent, m}}, insertRow(ctx, tx, m)
	}); err != nil {
		return row{}, err
	}
	return m, nil
}
