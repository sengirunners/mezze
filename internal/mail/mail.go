// Package mail is the mail module: routing, mailboxes (per project), acks,
// threads and the one-line notice for hooks. Owned by the mail builder. See
// SPEC.md "Mail" and API.md "/v0/mail".
//
// Mail is data, never authority: routing reads only `to` and `ref`, never the
// body, and the only text ever typed into an agent's terminal is NudgeLine.
//
// Storage: mail lives in each project's project.db (store.go). Every read
// filters with the caller's scope (ReadScope + store.Projects + AreaSQL) and
// every address returned to a caller goes through d.Redact.
package mail

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// NudgeLine is the only text ever typed into an agent's terminal for mail.
const NudgeLine = "zm: you have mail. Run: zm inbox"

// OrphanAfter is how long unacked mail to an ended session waits before it
// is copied to the live holder of its ref, else its machine's group mailbox.
const OrphanAfter = 30 * time.Minute

// NoticeKeep is how long an unacked notice stays open: it says something that
// was true then, so it does not pile up in front of real mail.
const NoticeKeep = 24 * time.Hour

// AnnounceWindow bounds how old an announcement a session is still shown.
const AnnounceWindow = 24 * time.Hour

// Forwarder delivers mail to another machine's daemon (the tailnet module
// may satisfy it; the integrator wires it with SetForwarder). v0 forwards
// nothing (SPEC section 13): without one, mail to another machine is
// not_found.
type Forwarder interface {
	// Forward delivers m (already routed: m.Recipient is on machine) and
	// returns the remote copy. An error means it was not delivered.
	Forward(ctx context.Context, machine string, m orchapi.Mail) (orchapi.Mail, error)
}

// AskChecker reports whether mail id in project is an ask (SPEC section 16)
// that no approver has decided yet. The approvals module implements it; the
// integrator wires it with SetAsks.
type AskChecker interface {
	OpenAsk(ctx context.Context, project, id string) (bool, error)
}

// IdleLister is the preferred session-module method mail uses to wake one
// idle managed session for group mail (SPEC section 9 rule 6).
type IdleLister interface {
	// IdleManaged lists the addresses of idle managed sessions of project on
	// this machine, oldest first.
	IdleManaged(ctx context.Context, project string) []string
}

// SessionLister is the session module's List (internal/session.Service has
// it); mail falls back to it when d.Sessions lacks IdleLister. A session
// module with neither cannot wake anyone for group mail, and the daemon says
// so (a Warn at Start and on the first group mail), never silently.
type SessionLister interface {
	List(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error)
}

// errNoIdleList is logged when group mail cannot wake anyone.
const errNoIdleList = "mail: the session module lists no idle sessions (neither IdleManaged nor List); group mail wakes nobody"

// Service implements orchcore.MailService and the /v0/mail routes.
type Service struct {
	d   *core.Deps
	now func() time.Time

	mu   sync.RWMutex
	fwd  Forwarder
	asks AskChecker

	noIdleOnce     sync.Once
	importLegacyMu sync.Mutex

	// judgeC is the model the mail-chore judge asks (SetJudge); nil is the
	// configured provider CLI (judge.Default).
	judgeMu sync.Mutex
	judgeC  judge.Caller

	// refused is when a work item last refused an unheld comment, by mail
	// id: the sweep asks again only after refuseRetry (triage.go).
	refusedMu sync.Mutex
	refused   map[string]time.Time
}

// SetJudge replaces the model the mail-chore judge asks (tests); nil restores the
// configured provider CLI.
func (s *Service) SetJudge(c judge.Caller) {
	s.judgeMu.Lock()
	s.judgeC = c
	s.judgeMu.Unlock()
}

// judgeCaller is the model the mail-chore judge asks.
func (s *Service) judgeCaller() judge.Caller {
	s.judgeMu.Lock()
	c := s.judgeC
	s.judgeMu.Unlock()
	if c != nil {
		return c
	}
	return judge.Default(s.d.Config.File.Agents)
}

var (
	_ orchcore.MailService = (*Service)(nil)
	_ core.Module          = (*Service)(nil)
	_ core.Starter         = (*Service)(nil)
)

// Migrations are this module's tables (core.Deps.AddMigrations); New adds them.
var Migrations = slices.Concat(storeMigrations, personalMigrations, hookNoticeMigrations, limitMigrations)

// New returns the mail service. Do not touch orchcore.Work(d) or d.Sessions
// here; they are wired after every New returns.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }}
	orchcore.AddNoticeCounter(d, s.noticeCounts)
	return s
}

// SetForwarder installs cross-machine delivery (nil = none, the v0 default).
func (s *Service) SetForwarder(f Forwarder) {
	s.mu.Lock()
	s.fwd = f
	s.mu.Unlock()
}

// SetAsks installs the open-ask check that keeps POST /v0/mail/{id}/ack
// and /reply off an undecided ask's mail (nil = no check).
func (s *Service) SetAsks(a AskChecker) {
	s.mu.Lock()
	s.asks = a
	s.mu.Unlock()
}

// refuseOpenAsk is the HTTP ack/reply guard: an ask's mail is answered only
// by an approver's decision (the approvals module replies in process), never
// by a socket or token caller, or the asking session would leave
// needs_human with nothing decided. m is mail the caller can already see,
// so saying why reveals nothing.
func (s *Service) refuseOpenAsk(ctx context.Context, m row) error {
	s.mu.RLock()
	a := s.asks
	s.mu.RUnlock()
	if a == nil || m.Needs != orchapi.NeedsHuman {
		return nil
	}
	open, err := a.OpenAsk(ctx, m.Project, m.ID)
	if err != nil {
		return err
	}
	if open {
		return api.Errorf(api.CodeForbidden, "mail %s is an open ask; only an approver's decision answers it", short(m.ID)).
			WithHint("the project's owner answers it (zm ask answer <id> <choice>, or the Mezze app's card), or a paired device; the decision answers this mail")
	}
	return nil
}

func (s *Service) forwarder() Forwarder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fwd
}

// PushOpen checks the current scoped mail, including a later acknowledgement.
func (s *Service) PushOpen(ctx context.Context, sub *authz.Subject, id string) (bool, error) {
	m, _, err := s.find(ctx, sub.Scope, sub.Address, id)
	if api.IsCode(err, api.CodeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if m.AckedAt != nil || m.HeldFor != "" || m.Kind == orchapi.MailKindReport {
		return false, nil
	}
	if m.ForYou != "" {
		// A for-you item pushes once shown: a digest item never does (it
		// waits for 08:00 with the rest), an ask's mail as a decision.
		if fy, err := s.forYou(ctx, m); err != nil || fy {
			return fy && m.HeldUntil == nil, err
		}
	}
	return m.Needs == orchapi.NeedsHuman, nil
}

// Start runs the orphan sweep once a minute until ctx ends. It warns once
// if the session module cannot list idle sessions (group mail would wake
// nobody).
func (s *Service) Start(ctx context.Context) error {
	if !s.canListIdle() {
		s.warnNoIdle()
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.sweepOrphans(ctx); err != nil && ctx.Err() == nil {
					s.d.Log.Warn("mail orphan sweep", "err", err)
				}
				if err := s.pruneSends(ctx); err != nil && ctx.Err() == nil {
					s.d.Log.Warn("mail send ledger prune", "err", err)
				}
			}
		}
	}()
	return nil
}

func (s *Service) machine() string { return s.d.Config.Machine }

// ---- callers ----

// caller is who is asking, as far as mailboxes go.
type caller struct {
	sub     *authz.Subject
	addr    api.Address
	project string // a session's own project
	// machine is the machine whose group mailbox and sessions the caller
	// acts for: this one, or, at a team authority, the replica a session
	// (or a replica's human) is on (SPEC 17.3).
	machine string
	// unknown: a session address the daemon has no row for (hooks not
	// installed) or one registered in project none. It gets direct mail
	// only: never a project's group mailbox or announcements.
	unknown bool
	// also: a person's other accounts (orchcore.People): their private mail
	// lists here too, only where this caller is also a participant.
	also []string
	// machineHuman: the machine owner as a human (sees needs=human mail sent
	// to sessions and group mailboxes on its machine). At a team authority
	// a replica's own identity is its machine's human.
	machineHuman bool
}

func (s *Service) callerOf(ctx context.Context, sub *authz.Subject) caller {
	c := caller{sub: sub, machine: s.machine()}
	c.addr, _ = api.ParseAddress(sub.Address)
	switch c.addr.Kind {
	case api.KindSession:
		c.unknown, c.machine = true, c.addr.Machine
		if orchcore.Sessions(s.d) != nil {
			// A replica's session at a team authority is known by its presence.
			if ss, ok := orchcore.Sessions(s.d).Lookup(ctx, c.addr.String()); ok && ss.Project != api.ProjectNone && ss.Project != "" {
				c.project, c.unknown = ss.Project, false
			}
		}
	case api.KindHuman:
		if pa := orchcore.People(s.d); pa != nil {
			c.also = pa.AccountsOf(ctx, c.addr.String())
		}
		if sub.Node != "" && sub.Machine != "" {
			// A team replica's node-bound token: its identity is that
			// machine's human here.
			// Never this machine's human, whatever name the token records.
			c.machine, c.machineHuman = sub.Machine, c.addr.Human == sub.Identity.Name && sub.Machine != s.machine()
			break
		}
		local := s.d.Projects.LocalIdentity()
		c.machineHuman = local.ID != "" && sub.Identity.ID == local.ID && c.addr.Human == local.Name
	case api.KindMachine:
		c.machine = c.addr.Machine
	}
	return c
}

func (c caller) human() bool { return c.addr.Kind == api.KindHuman }

// mineSQL selects mail to the caller's own address and, for a person with
// several accounts, the private mail to their other accounts (the read
// filter still requires the caller among its participants).
func (c caller) mineSQL() (string, []any) {
	args := []any{c.addr.String()}
	if len(c.also) == 0 {
		return "recipient = ?", args
	}
	for _, a := range c.also {
		args = append(args, a)
	}
	args = append(args, c.addr.String())
	return "(recipient = ? OR (audience = 'participants' AND recipient IN (" + strings.TrimSuffix(strings.Repeat("?,", len(c.also)), ",") + ") AND sender != ?))", args
}

// ownsPersonal reports whether personal mail m is the caller's: to its own
// address, or private mail to another of its person's accounts that it is a
// participant of and did not send.
func (c caller) ownsPersonal(m row) bool {
	me := c.addr.String()
	if m.Recipient == me {
		return true
	}
	return m.Audience == orchapi.MailAudienceParticipants && m.From != me && slices.Contains(c.also, m.Recipient) && slices.Contains(m.Participants, me)
}

// inProject reports whether the caller is a known session of project.
func (c caller) inProject(project string) bool {
	return c.addr.Kind == api.KindSession && !c.unknown && c.project == project
}

// onMachine reports whether recipient is a session or the group mailbox on machine.
func onMachine(recipient, machine string) bool {
	a, err := api.ParseAddress(recipient)
	return err == nil && (a.Kind == api.KindSession || a.Kind == api.KindMachine) && a.Machine == machine
}

// party reports whether the caller sent, received or may take m.
func (s *Service) party(c caller, m row) bool {
	me := c.addr.String()
	if me == "" {
		return false
	}
	if m.Audience != "" {
		for _, participant := range m.Participants {
			if participant == me {
				return true
			}
		}
		return false
	}
	if m.From == me || m.Recipient == me {
		return true
	}
	switch {
	case m.Recipient == "all":
		return c.inProject(m.Project)
	case m.Recipient == "owners":
		return c.human() && c.sub.Scope.Relation(authz.Object{Project: m.Project, Area: m.Area}) == authz.Owner
	case m.Recipient == "machine:"+c.machine:
		if c.machineHuman {
			return true
		}
		return c.inProject(m.Project) && c.addr.Machine == c.machine
	}
	return m.Needs == orchapi.NeedsHuman && c.machineHuman && onMachine(m.Recipient, c.machine)
}

// team reports whether project is a team project whose authority is this
// daemon: its mail reaches sessions and group mailboxes on its replicas
// (SPEC 17.3), which fetch it through their proxy.
func (s *Service) team(project string) bool {
	return project != "" && project != api.ProjectNone && s.d.Store.IsTeam(project)
}

// ---- output ----

// redact hides, for sub, every session address it cannot see and a ref in
// an area it cannot read.
func (s *Service) redact(ctx context.Context, sub *authz.Subject, r row) orchapi.Mail {
	m := r.Mail
	if m.ForYou != "" {
		if ask, _ := s.isAsk(ctx, r.Project, r.ID); ask {
			m.ForYou = "" // an ask's mail is a decision, answered by its decision
		}
	}
	if sub.Scope.IsAll() {
		return m
	}
	m.From = s.d.Redact(ctx, sub, m.From)
	m.To = s.d.Redact(ctx, sub, m.To)
	m.Recipient = s.d.Redact(ctx, sub, m.Recipient)
	m.AckedBy = s.d.Redact(ctx, sub, m.AckedBy)
	m.Participants = append([]string(nil), m.Participants...)
	for i, p := range m.Participants {
		m.Participants[i] = s.d.Redact(ctx, sub, p)
	}
	if m.LegacyDelivery != nil {
		delivery := *m.LegacyDelivery
		if a, err := api.ParseAddress(delivery.AckBy); err == nil && a.Kind == api.KindSession {
			delivery.AckBy = s.d.Redact(ctx, sub, delivery.AckBy)
		}
		m.LegacyDelivery = &delivery
	}
	if m.Origin != nil {
		origin := *m.Origin
		origin.ImportedBy = s.d.Redact(ctx, sub, origin.ImportedBy)
		if a, err := api.ParseAddress(origin.Sender); err == nil && a.Kind == api.KindSession {
			origin.Sender = s.d.Redact(ctx, sub, origin.Sender)
		}
		// Legacy addresses are unverified: do not reinterpret them as native aliases.
		// Only the explicitly mapped personal audience receives this provenance.
		m.Origin = &origin
	}
	if addr, ok := strings.CutPrefix(m.Via, "orphan of "); ok {
		m.Via = "orphan of " + s.d.Redact(ctx, sub, addr)
	}
	if m.Ref != "" && !sub.Can(authz.Read, authz.Object{Project: r.Project, Area: r.RefArea}) {
		m.Ref = ""
	}
	return m
}

// sentAnswer is the mail a send answers with: redacted, but a session it was
// sent to shows its address, the one the mail stores (the sender typed it or
// the session's name, SPEC 8 "Names"; it may be in another project, SPEC 9
// "Across projects": routing let the sender reach it). A work item's holder
// is still redacted.
func (s *Service) sentAnswer(ctx context.Context, sub *authz.Subject, r row) orchapi.Mail {
	m := s.redact(ctx, sub, r)
	if a, err := api.ParseAddress(r.To); err == nil && a.Kind == api.KindSession {
		m.To = r.To
		if r.Recipient == r.To {
			m.Recipient = r.Recipient
		}
	}
	return m
}

func (s *Service) redactAll(ctx context.Context, sub *authz.Subject, rs []row) []orchapi.Mail {
	out := make([]orchapi.Mail, 0, len(rs))
	for _, r := range rs {
		out = append(out, s.redact(ctx, sub, r))
	}
	return out
}

// mentions are the addresses in m a reader might not see.
func mentions(m orchapi.Mail) []string {
	out := []string{m.From, m.To, m.Recipient, m.AckedBy}
	out = append(out, m.Participants...)
	if m.Origin != nil {
		out = append(out, m.Origin.ImportedBy)
		if a, err := api.ParseAddress(m.Origin.Sender); err == nil && a.Kind == api.KindSession {
			out = append(out, m.Origin.Sender)
		}
	}
	if m.LegacyDelivery != nil {
		if a, err := api.ParseAddress(m.LegacyDelivery.AckBy); err == nil && a.Kind == api.KindSession {
			out = append(out, m.LegacyDelivery.AckBy)
		}
	}
	if addr, ok := strings.CutPrefix(m.Via, "orphan of "); ok {
		out = append(out, addr)
	}
	return out
}

// publish emits a mail event. The bus filters per subscriber by the mail's
// project/area (plus an Audience for restricted mail) and redacts Mentions,
// but knows nothing of RefArea: a ref in
// another area than the mail's is left out of the event (readers fetch
// GET /v0/mail/{id}, which redacts it per reader).
func (s *Service) publish(ctx context.Context, typ string, r row) {
	_, err := s.d.Events.Publish(context.WithoutCancel(ctx), eventOf(typ, r))
	if err != nil {
		s.d.Log.Warn("mail publish", "type", typ, "id", r.ID, "err", err)
	}
}

// eventOf is the event for mail r.
func eventOf(typ string, r row) events.Input {
	data := r.Mail
	data.Note = "" // the send's answer only, never a stored or streamed field
	if r.RefArea != r.Area {
		data.Ref = ""
	}
	in := events.Input{Type: typ, Subject: r.ID, Project: r.Project, Area: r.Area, Data: data, Mentions: mentions(r.Mail)}
	if r.Audience != "" {
		in.Audience = &events.Audience{ParticipantsOnly: true, Parties: r.Participants}
	} else if restricted(r) {
		// Same rule as readWhere: a guest of the area gets it only as a party.
		in.Audience = &events.Audience{Need: authz.Operate, Parties: []string{r.From, r.To, r.Recipient}}
	}
	return in
}

// pub is an event a mail write publishes.
type pub struct {
	typ string
	r   row
}

// commit runs fn in one transaction and publishes the events it returns.
// In a team project (SPEC 17.6) the rows and their events commit together
// (TeamTx, on the file's writer): a restart between them would leave mail
// whose event never comes, which a replica, learning only from events, would
// never see. fn must use tx only.
func (s *Service) commit(ctx context.Context, db *sql.DB, project string, fn func(tx *sql.Tx) ([]pub, error)) error {
	if s.team(project) {
		return s.d.Events.TeamTx(context.WithoutCancel(ctx), project, func(tx *sql.Tx, emit func(events.Input)) error {
			ps, err := fn(tx)
			for _, p := range ps {
				emit(eventOf(p.typ, p.r))
			}
			return err
		}, nil)
	}
	var ps []pub
	if err := store.Tx(ctx, db, func(tx *sql.Tx) error {
		var err error
		ps, err = fn(tx)
		return err
	}); err != nil {
		return err
	}
	for _, p := range ps {
		s.publish(ctx, p.typ, p.r)
	}
	return nil
}

// ---- waking sessions ----

// nudge wakes whoever should read m now: an idle managed session it was
// delivered to, or one idle managed session of the project for group mail.
// needs=human mail and announcements never type anything.
func (s *Service) nudge(ctx context.Context, m row) {
	if m.Replayed || orchcore.Sessions(s.d) == nil || m.Needs == orchapi.NeedsHuman || m.Kind == orchapi.MailKindAnnounce || m.Kind == orchapi.MailKindNotice {
		return
	}
	ctx = context.WithoutCancel(ctx)
	a, err := api.ParseAddress(m.Recipient)
	if err != nil || a.Machine != s.machine() {
		return
	}
	switch a.Kind {
	case api.KindSession:
		if _, err := orchcore.Sessions(s.d).Nudge(ctx, a.String(), NudgeLine); err != nil && !api.IsCode(err, api.CodeNotImplemented) {
			s.d.Log.Debug("mail nudge", "session", a.String(), "err", err)
		}
	case api.KindMachine:
		addrs, ok := s.idleManaged(ctx, m.Project)
		if !ok {
			s.warnNoIdle()
			return
		}
		for _, addr := range addrs {
			if ok, err := orchcore.Sessions(s.d).Nudge(ctx, addr, NudgeLine); err == nil && ok {
				return
			}
		}
	}
}

func (s *Service) canListIdle() bool {
	switch orchcore.Sessions(s.d).(type) {
	case IdleLister, SessionLister:
		return true
	}
	return false
}

func (s *Service) warnNoIdle() {
	s.noIdleOnce.Do(func() { s.d.Log.Warn(errNoIdleList) })
}

// idleManaged lists the idle managed sessions of project on this machine,
// oldest first. ok=false: the session module cannot say.
func (s *Service) idleManaged(ctx context.Context, project string) ([]string, bool) {
	switch ss := orchcore.Sessions(s.d).(type) {
	case IdleLister:
		return ss.IdleManaged(ctx, project), true
	case SessionLister:
		list, err := ss.List(ctx, authz.All().Only(project), orchapi.SessionQuery{Project: project, State: orchapi.StateIdle})
		if err != nil {
			s.d.Log.Warn("mail: list idle sessions", "err", err)
			return nil, true
		}
		var keep []orchapi.Session
		for _, x := range list {
			if x.Project == project && x.Managed && x.State == orchapi.StateIdle && x.Machine == s.machine() {
				keep = append(keep, x)
			}
		}
		sort.SliceStable(keep, func(i, j int) bool { return keep[i].Started.Before(keep[j].Started) })
		out := make([]string, len(keep))
		for i, x := range keep {
			out[i] = x.Address
		}
		return out, true
	}
	return nil, false
}

// setState records a session state change, ignoring a session module that
// cannot (yet).
func (s *Service) setState(ctx context.Context, addr string, st orchapi.SessionState, note string) {
	if orchcore.Sessions(s.d) == nil {
		return
	}
	if err := orchcore.Sessions(s.d).SetState(context.WithoutCancel(ctx), addr, orchapi.StateRequest{State: st, Note: note}); err != nil &&
		!api.IsCode(err, api.CodeNotImplemented) {
		s.d.Log.Debug("mail: session state", "session", addr, "state", st, "err", err)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}
