package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// An approved action's state (SPEC 16 "Asks that carry an action"). The
// decision commits with an action effect; the effect starts the frozen
// action as the approver, checked now, through the team module; from then
// on the action's state is the remote task's or machine op's, which the
// ticker reads back (orchcore.AskActions) into ask_actions. A failure mails
// the asker and comments ACTION FAILED on the item, once. The ask stays
// with the open ones until an owner confirms how it went (a done one also
// after a day).

var askActionsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0008_ask_actions", Script: `
CREATE TABLE ask_actions (
	ask TEXT PRIMARY KEY REFERENCES asks(id),
	kind TEXT NOT NULL,
	task TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	started INTEGER,
	ended INTEGER,
	confirmed_by TEXT NOT NULL DEFAULT '',
	confirmed INTEGER,
	notice_mail TEXT NOT NULL DEFAULT '',
	notice_comment TEXT NOT NULL DEFAULT '',
	notified INTEGER
);
CREATE INDEX ask_actions_live ON ask_actions(state) WHERE state IN ('queued', 'running') OR (state = 'failed' AND notified IS NULL);
`},
}

// actionStartWithin: a start the team module cannot take yet (no machine
// heard from, the authority busy) is tried again until then, and fails after.
var actionStartWithin = 10 * time.Minute

// doneListed is how long a done action nobody confirmed stays listed with
// the open asks.
const doneListed = 24 * time.Hour

// actionBody is an action effect's payload: who approved it and the frozen
// action it starts. Identity is the approver's person ("" = this machine's
// owner), whose grants are checked when it runs.
type actionBody struct {
	Approver string `json:"approver"`
	Method   string `json:"method"`
	Identity string `json:"identity,omitempty"`
	Action   string `json:"action_sha256"`
}

// runEffect is the action effect of a decision that picked a running
// action's choice.
func (s *Service) runEffect(ctx context.Context, a orchapi.Ask, d orchapi.Decision, act *orchapi.AskAction) (*effect, error) {
	b := actionBody{Approver: d.Approver, Method: d.Method, Action: textSHA(marshal(*act))}
	if d.Method == orchapi.MethodApprover {
		err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM approvers WHERE name = ?`, d.Approver).Scan(&b.Identity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return &effect{Kind: "action", Target: a.Project, Body: marshal(b)}, nil
}

// insertAction records an action effect's state, queued, in the decision's
// transaction.
func insertAction(ctx context.Context, tx *sql.Tx, ask string, list []effect, now time.Time) error {
	for _, e := range list {
		if e.Kind != "action" {
			continue
		}
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(card, '$.action.kind'), '') FROM asks WHERE id = ?`, ask).Scan(&kind); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ask_actions (ask, kind, state, created) VALUES (?, ?, 'queued', ?)`,
			ask, kind, ms(now)); err != nil {
			return err
		}
	}
	return nil
}

// runAction delivers an action effect: it starts exactly the action frozen
// on a's card, as the approver, while the approver still owns or
// administers the project. A refusal fails the action (its state says why)
// and settles the effect; only a start the team module cannot take yet is
// tried again, for actionStartWithin of its attempt. A retry
// (action_retry.go) runs it again under the same checks.
func (s *Service) runAction(ctx context.Context, a orchapi.Ask, e effect) error {
	// One start of an ask's action at a time (the effect's delivery, a
	// retry and the watch may meet): each reads the row under it.
	unlock := s.lockActionStart(a.ID)
	defer unlock()
	var b actionBody
	if err := json.Unmarshal([]byte(e.Body), &b); err != nil {
		return api.Errorf(api.CodeConflict, "action payload: %v", err)
	}
	// Which attempt this is, and whether it still waits to start: a
	// delivery after the start (or after a failure) starts nothing.
	var state, task string
	var attempt, retries int
	var attemptAt sql.NullInt64
	var created int64
	switch err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT state, task, attempt, retries, attempt_at, created FROM ask_actions WHERE ask = ?`, a.ID).
		Scan(&state, &task, &attempt, &retries, &attemptAt, &created); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case state != orchapi.ActionQueued:
		return nil
	case task != "":
		s.refreshAction(ctx, a.Project, a.ID, task)
		return nil
	}
	if attemptAt.Valid {
		created = attemptAt.Int64
	}
	again := act0Kind(a) == orchapi.ActionRun || act0Kind(a) == orchapi.ActionCommand // only these may start again
	fail := func(reason string, retryable bool) { s.failStart(ctx, a.ID, "", attempt, reason, retryable && again) }
	act, err := frozenAction(a)
	if err != nil {
		fail("the card changed after it was asked", false)
		return err
	}
	if act == nil || e.Target != a.Project || b.Action != textSHA(marshal(*act)) {
		fail("its payload is not the action frozen on the card", false)
		return api.Errorf(api.CodeConflict, "action payload is not the action frozen on ask %s", a.ID)
	}
	if b.Method != orchapi.MethodApprover {
		fail("it was answered without a signature: a choice that runs something is answered on a paired device or in the Mezze app", false)
		return nil
	}
	if act.Kind == orchapi.ActionBrowser {
		fail("browser actions do not run yet", false)
		return nil
	}
	if retries > 0 && !s.approverPaired(ctx, a.ID, b.Approver) {
		fail("the device that approved it is no longer paired, so it does not run again: ask again", false)
		return nil
	}
	if retries > 0 && s.objected(ctx, a.ID) {
		fail("an owner objected to its outcome, so it does not run again", false)
		return nil
	}
	retry := func(err error) error {
		if s.now().Sub(store.FromMillis(created)) < actionStartWithin {
			return err
		}
		fail(err.Error(), true)
		return nil
	}
	aa := s.askActions()
	if aa == nil {
		return retry(api.Errorf(api.CodeUnavailable, "this daemon runs no actions on machines"))
	}
	sub, refused, err := s.approverSubject(ctx, a.Project, b.Approver, b.Identity)
	if err != nil {
		return retry(err)
	}
	if refused != "" {
		fail(refused, false)
		return nil
	}
	// A person's own machine takes only their yes, as its person is now. A
	// device paired for no one answers as no one unless this machine's owner
	// is the project's one answerer (approverName).
	name := sub.Identity.Name
	if b.Identity == "" && b.Method == orchapi.MethodApprover {
		if name, err = s.soleAnswerer(ctx, a.Project); err != nil {
			return retry(err)
		}
	}
	if err := s.onlyTheirYes(ctx, a, name); err != nil {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code == api.CodeUnavailable || ae.Code == api.CodeInternal {
			return retry(err)
		}
		fail(b.Approver+" may not: "+ae.Message, false)
		return nil
	}
	st := orchcore.AskActionStart{ID: startID(a.ID, attempt), Kind: act.Kind, Project: a.Project, Machine: act.Machine,
		Parent: a.AskedBy, User: act.User, Key: act.Key}
	switch {
	case act.Kind == orchapi.ActionCommand:
		st.Task = orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Work: a.Subject.Work,
			Run: &orchapi.TaskRun{Ref: act.Ref, Commit: act.Commit, CommitOnRef: act.PinChecked, Command: act.Command}}
	case act.Kind == orchapi.ActionRun && act.Brief != "":
		st.Task = orchapi.TaskStartRequest{Agent: act.Agent, Prompt: act.Brief, Work: a.Subject.Work}
	case act.Kind == orchapi.ActionRun:
		st.Task = orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Work: a.Subject.Work,
			Run: &orchapi.TaskRun{Ref: act.Ref, Commit: act.Commit, CommitOnRef: act.PinChecked, Script: act.Script, Args: act.Args}}
	}
	if st.Task.Agent == orchapi.TaskAgentRun {
		if st.AsApprover, err = s.approversMachine(ctx, a.Project, act.Machine, name); err != nil {
			return retry(err)
		}
	}
	id, err := aa.StartAskAction(ctx, sub, st)
	if err != nil {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code == api.CodeUnavailable || ae.Code == api.CodeInternal {
			return retry(err)
		}
		// The team module found no start under this id: a retry asks again
		// under the same id, so it can never be a second start.
		fail(plainReason(err), ae.Code != api.CodeConflict)
		return nil
	}
	// Recorded whatever the state: a start the team module took is followed
	// and never taken for one that did not start.
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET task = ?, retryable = 0 WHERE ask = ? AND task = '' AND attempt = ?`, id, a.ID, attempt); err != nil {
		return err
	}
	s.refreshAction(ctx, a.Project, a.ID, id)
	return nil
}

// lockActionStart takes ask's start lock; the answer releases it (and
// forgets it when no other start of the ask waits).
func (s *Service) lockActionStart(ask string) func() {
	s.actionStartsMu.Lock()
	l := s.actionStarts[ask]
	if l == nil {
		l = &startLock{}
		if s.actionStarts == nil {
			s.actionStarts = map[string]*startLock{}
		}
		s.actionStarts[ask] = l
	}
	l.users++
	s.actionStartsMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.actionStartsMu.Lock()
		if l.users--; l.users == 0 {
			delete(s.actionStarts, ask)
		}
		s.actionStartsMu.Unlock()
	}
}

// startLock is one ask's start lock and how many hold or wait for it.
type startLock struct {
	mu    sync.Mutex
	users int
}

// act0Kind is a's card action's kind, or "".
func act0Kind(a orchapi.Ask) string {
	if act := actionOf(a); act != nil {
		return act.Kind
	}
	return ""
}

// objected reports whether an owner objected to ask's outcome.
func (s *Service) objected(ctx context.Context, ask string) bool {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM ask_objections WHERE ask = ?`, ask).Scan(&n)
	return err != nil || n > 0 // unknown counts as objected: it does not run again
}

// startID is the team module's request id of an action's attempt: the
// first is ask-<id>; each attempt after one that provably never started is
// a new id (the old one stays refused for good).
func startID(ask string, attempt int) string {
	id := "ask-" + strings.ToLower(ask)
	if attempt > 0 {
		id += "-" + strconv.Itoa(attempt)
	}
	return id
}

// approverPaired reports whether the device or key that answered ask (by
// name and the node or key it answered from) is still paired and active: a
// retry runs on that yes, so one unpaired, or a new device paired under the
// same name since, gives no more yeses.
func (s *Service) approverPaired(ctx context.Context, ask, approver string) bool {
	var state, node, answered string
	db := s.d.Store.Registry()
	if err := db.QueryRowContext(ctx, `SELECT node_id FROM decisions WHERE ask = ?`, ask).Scan(&answered); err != nil || answered == "" {
		return false
	}
	err := db.QueryRowContext(ctx, `SELECT state, coalesce(node_id, '') FROM approvers WHERE name = ?`, approver).Scan(&state, &node)
	return err == nil && state == orchapi.ApproverActive && node == answered
}

// approverSubject is the approver's person (identity "" = this machine's
// owner) as a subject that still owns or administers project, else why not
// in plain words; err when that cannot be told yet.
func (s *Service) approverSubject(ctx context.Context, project, approver, identity string) (*authz.Subject, string, error) {
	sub := s.d.Projects.Local("")
	if identity != "" {
		var err error
		if sub, err = s.d.Projects.IdentitySubject(ctx, identity); err != nil {
			if api.IsCode(err, api.CodeNotFound) {
				return nil, approver + " may not: that person is no longer here", nil
			}
			return nil, "", err
		}
	}
	admin, err := s.d.ProjectAdmin(ctx, sub, project)
	if err != nil {
		return nil, "", err
	}
	if !admin {
		return nil, approver + " may not: they no longer own or administer the project", nil
	}
	return sub, "", nil
}

// approversMachine reports whether machine is approver name's own in
// project: the one person whose zm it answers as there is them (its human
// at the authority, else its one joined person: what a call without a
// session carries from it). A command or script run there acts in zm as
// them (an owner-only zm ask check --evidence on the card failed as
// its task session); anywhere else it stays its task session, never another
// person's or a shared box's account.
func (s *Service) approversMachine(ctx context.Context, project, machine, name string) (bool, error) {
	h := s.laneHost()
	if h == nil || name == "" {
		return false, nil
	}
	ts, err := h.AskRunTargets(ctx, project)
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		if t.Machine == machine {
			return len(t.People) == 1 && s.samePerson(ctx, project, name, t.People[0]), nil
		}
	}
	return false, nil
}

// plainReason is err's message with its hint, as a person reads it.
func plainReason(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		if ae.Hint != "" {
			return ae.Message + " (" + ae.Hint + ")"
		}
		return ae.Message
	}
	return err.Error()
}

// failAction marks ask's action failed with reason, once, and tells the
// asker and the approver. It is not retryable.
func (s *Service) failAction(ctx context.Context, ask, reason string) {
	s.recordFailure(ctx, ask, reason, false, "")
}

// failStart fails attempt attempt of ask's action, started as task ("" =
// never handed to the team module), and only that attempt: a late read of
// an earlier attempt never fails a later one. retryable: it provably never
// started, so the identical action may start again (action_retry.go).
func (s *Service) failStart(ctx context.Context, ask, task string, attempt int, reason string, retryable bool) {
	if task != "" { // a task id is one attempt's own
		s.recordFailure(ctx, ask, reason, retryable, ` AND task = ?`, task)
		return
	}
	s.recordFailure(ctx, ask, reason, retryable, ` AND task = '' AND attempt = ?`, attempt)
}

func (s *Service) recordFailure(ctx context.Context, ask, reason string, retryable bool, bound string, args ...any) {
	ctx = context.WithoutCancel(ctx)
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET state = 'failed', reason = ?, ended = ?, notice_mail = ?, notice_comment = ?, notice_owner = ?, retryable = ?
		WHERE ask = ? AND state IN ('queued', 'running', 'approved')`+bound, append([]any{reason, ms(s.now()), ids.New(), ids.New(), ids.New(), retryable, ask}, args...)...)
	if err != nil {
		s.d.Log.Error("approvals: record a failed action", "ask", ask, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.notifyFailure(ctx, ask)
	}
}

// refreshAction reads a started action's state back from the team module.
func (s *Service) refreshAction(ctx context.Context, project, ask, task string) {
	aa := s.askActions()
	if aa == nil || task == "" {
		return
	}
	st, err := aa.AskActionStatus(ctx, project, task)
	if err != nil {
		s.d.Log.Warn("approvals: read an action's state", "ask", ask, "err", err)
		return
	}
	now := ms(s.now())
	db := s.d.Store.Registry()
	switch st.State {
	case orchapi.ActionRunning:
		_, err = db.ExecContext(ctx, `UPDATE ask_actions SET state = 'running', started = coalesce(started, ?) WHERE ask = ? AND task = ? AND state IN ('queued', 'running')`, now, ask, task)
	case orchapi.ActionDone:
		ended := now
		if st.Ended != nil {
			ended = ms(*st.Ended)
		}
		// With a verify check on the card, a finished action is approved,
		// not done, until the asker's check passes after it.
		var verify bool
		if err = db.QueryRowContext(ctx, `SELECT json_extract(card, '$.verify.command') IS NOT NULL FROM asks WHERE id = ?`, ask).Scan(&verify); err != nil {
			break
		}
		if verify {
			_, err = db.ExecContext(ctx, `UPDATE ask_actions SET state = 'approved', started = coalesce(started, ?), ended = ? WHERE ask = ? AND task = ? AND state IN ('queued', 'running')`, ended, ended, ask, task)
			break
		}
		var cur string
		if err = db.QueryRowContext(ctx, `SELECT task FROM ask_actions WHERE ask = ?`, ask).Scan(&cur); err != nil || cur != task {
			break // a later attempt's: this read is stale
		}
		var ok bool
		if ok, err = s.actionDone(ctx, ask, ended, false, orchapi.ActionQueued, orchapi.ActionRunning); ok {
			s.notifyDone(ctx, ask)
		}
	case orchapi.ActionFailed:
		// Only a run or command the authority shows was never handed to a
		// machine may start again; anything that may have started never.
		var kind string
		_ = db.QueryRowContext(ctx, `SELECT kind FROM ask_actions WHERE ask = ?`, ask).Scan(&kind)
		retryable := st.NeverStarted && (kind == orchapi.ActionRun || kind == orchapi.ActionCommand)
		s.failStart(ctx, ask, task, 0, first(st.Reason, "it failed"), retryable)
	}
	if err != nil {
		s.d.Log.Warn("approvals: record an action's state", "ask", ask, "err", err)
	}
}

// watchActions refreshes every live action and retries failure notices.
func (s *Service) watchActions(ctx context.Context) {
	type live struct {
		project, ask, task, state string
		retries                   int
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT a.project, x.ask, x.task, x.state, x.retries FROM ask_actions x JOIN asks a ON a.id = x.ask
		WHERE x.state IN ('queued', 'running') OR (x.state = 'failed' AND x.notified IS NULL) ORDER BY x.created LIMIT 100`)
	if err != nil {
		return
	}
	var xs []live
	for rows.Next() {
		var x live
		if rows.Scan(&x.project, &x.ask, &x.task, &x.state, &x.retries) == nil {
			xs = append(xs, x)
		}
	}
	rows.Close()
	for _, x := range xs {
		switch {
		case x.state == orchapi.ActionFailed:
			s.notifyFailure(ctx, x.ask)
		case x.task == "" && x.retries > 0:
			s.startRetry(ctx, x.ask) // a retry the team module could not take yet
		default:
			s.refreshAction(ctx, x.project, x.ask, x.task)
		}
	}
}

// WatchActions runs one watch pass (tests and the ticker).
func (s *Service) WatchActions(ctx context.Context) { s.watchActions(ctx) }

// notifyFailure tells who can act that an approved action failed, under
// write ids fixed when it failed, until all landed: the asking session
// while it is live, else the work item's holder; the approver, in plain
// words, with whether it can run again; and ACTION FAILED on the item.
func (s *Service) notifyFailure(ctx context.Context, ask string) {
	a, err := s.ask(ctx, ask)
	if err != nil || a.ActionState == nil || a.ActionState.State != orchapi.ActionFailed {
		return
	}
	var mailID, commentID, ownerID string
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT notice_mail, notice_comment, notice_owner FROM ask_actions WHERE ask = ?`, ask).Scan(&mailID, &commentID, &ownerID); err != nil {
		return
	}
	question := ""
	if a.Card != nil {
		question = " (" + quoted(a.Card.Question) + ")"
	}
	what := "work"
	if act := actionOf(a); act != nil {
		what = act.Kind + " action"
	}
	again := fmt.Sprintf("Fix what you can and ask again, naming ask %s.", a.ID)
	if x := a.ActionState; x.Retryable && x.RetryUntil != nil {
		how := "It never reached the machine, so its asker"
		if x.Task == "" {
			how = "zm has no record that it reached the machine (if it did, a retry follows that start instead of making a second), so its asker"
		}
		again = fmt.Sprintf("%s or a project owner can run it again unchanged, without a new card, until %s: zm ask retry %s.",
			how, x.RetryUntil.UTC().Format("15:04 UTC"), a.ID)
	}
	text := fmt.Sprintf("ACTION FAILED: %s. The approved %s of ask %s%s did not complete. %s",
		a.ActionState.Reason, what, a.ID, question, again)
	local := s.d.Projects.Local("")
	ref := api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}
	var failure error
	to := ""
	if orchapi.AgentSender(a.AskedBy) && s.sessionLive(ctx, a.AskedBy) {
		to = a.AskedBy
	} else if a.Subject.Work != "" {
		to, _, _, _ = orchcore.Work(s.d).MailTarget(ctx, local, a.Project, a.Subject.Work)
	}
	if to == "" && orchapi.AgentSender(a.AskedBy) {
		to = a.AskedBy // nobody else holds it: its mailbox keeps it
	}
	if to != "" {
		_, failure = orchcore.Mail(s.d).Send(ctx, local, ref, a.Area, orchapi.SendMailRequest{ID: mailID, To: to, Body: text})
	}
	if owner := s.failureApprover(ctx, a); owner != "" && owner != to && ownerID != "" && failure == nil {
		_, failure = orchcore.Mail(s.d).Send(ctx, local, ref, a.Area, orchapi.SendMailRequest{ID: ownerID, To: owner, Body: ownerFailureText(a)})
	}
	if a.Subject.Work != "" && failure == nil {
		if rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder); ok {
			failure = rec.RecordComment(ctx, local, a.Project, a.Subject.Work, commentID, text)
		}
	}
	if failure != nil {
		s.d.Log.Warn("approvals: tell who can act that an action failed", "ask", a.ID, "err", failure)
		return
	}
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET notified = ? WHERE ask = ?`, ms(s.now()), a.ID)
}

// sessionLive reports whether session address is still running (its
// mail would be read now).
func (s *Service) sessionLive(ctx context.Context, address string) bool {
	ss := orchcore.Sessions(s.d)
	return ss != nil && ss.Live(ctx, address)
}

// failureApprover is the person who approved a's failed action, as an
// address: the person frozen with the signed yes (relane.go freezeApprover;
// "" there is this machine's owner), else the owner who answered; "" when
// that person cannot be resolved now, never another in their place.
func (s *Service) failureApprover(ctx context.Context, a orchapi.Ask) string {
	if a.Decision == nil {
		return ""
	}
	var id sql.NullString
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT approver_identity FROM ask_actions WHERE ask = ?`, a.ID).Scan(&id); err != nil || !id.Valid {
		if a.Decision.Method == orchapi.MethodApprover {
			return "" // a signed yes from before its person was frozen: not told who
		}
		return s.approverAddress(ctx, a)
	}
	if id.String == "" {
		return s.d.Config.HumanAddress()
	}
	who, err := s.d.Projects.Identity(ctx, id.String)
	if err != nil || who.Kind != api.IdentityHuman {
		return ""
	}
	if ad, err := api.ParseAddress("human:" + who.Name); err == nil && ad.Kind == api.KindHuman {
		return ad.String()
	}
	return ""
}

// ownerFailureText is the approver's notice of a failed action, in plain
// words: whether it ran (from the authority's own record, never the
// machine's text: "nothing ran" only where the team module proved it never
// reached the machine), the machine's own words for why, and what they can
// do.
func ownerFailureText(a orchapi.Ask) string {
	x := a.ActionState
	question := "your approved step"
	if a.Card != nil {
		question = quoted(a.Card.Question)
	}
	where := "its machine"
	if act := actionOf(a); act != nil && act.Machine != "" {
		where = act.Machine
	}
	var b strings.Builder
	switch {
	case x.Retryable && x.Task != "":
		fmt.Fprintf(&b, "Something you approved never reached %s, so nothing ran: %s (ask %s).", where, question, a.ID)
	case x.Task == "":
		fmt.Fprintf(&b, "Something you approved may not have run: zm has no record that it reached %s: %s (ask %s).", where, question, a.ID)
	default:
		fmt.Fprintf(&b, "Something you approved failed after it may have started on %s, so it may have changed things: %s (ask %s).", where, question, a.ID)
	}
	fmt.Fprintf(&b, " zm's record of why: %s.", quoted(x.Reason))
	if x.Retryable && x.RetryUntil != nil {
		fmt.Fprintf(&b, " It can run again exactly as you approved it, without a new card, until %s: zm ask retry %s", x.RetryUntil.UTC().Format("15:04 UTC"), a.ID)
		if x.Task == "" {
			b.WriteString(" (if it did reach the machine, that start is followed instead: never a second one)")
		}
		b.WriteString(". If you no longer want it, confirm it in Decisions (seen) and it never runs again.")
	} else {
		b.WriteString(" It does not run again as it is: the asker was told, and anything more needs a new card.")
	}
	return b.String()
}

// scanActionState fills a's action state from askCols' ask_actions columns.
func scanActionState(a *orchapi.Ask, state, reason, task, confirmedBy string, started, ended, confirmed sql.NullInt64, c actionCheck) {
	if state == "" {
		return
	}
	a.ActionState = &orchapi.ActionState{State: state, Reason: reason, Task: task, ConfirmedBy: confirmedBy,
		StartedAt: store.NullMillis(started), EndedAt: store.NullMillis(ended), ConfirmedAt: store.NullMillis(confirmed),
		CheckedAt: store.NullMillis(c.checked), CheckOutput: c.output, VerifiedAt: store.NullMillis(c.verified),
		RemindedAt: store.NullMillis(c.reminded), EscalatedAt: store.NullMillis(c.escalated), CheckedBy: c.by,
		RelanedAt: store.NullMillis(c.relaned), RelaneTask: c.relaneTask, RelaneMachine: c.relaneMachine, RelaneReason: c.relaneReason, DoneEvidence: c.evidence,
		StepDoneAt: store.NullMillis(c.stepDone), StepDoneBy: c.stepDoneBy}
	if c.exit.Valid {
		n := int(c.exit.Int64)
		a.ActionState.CheckExit = &n
	}
	a.NotDone = notDoneState(state)
}

// actionCheck is askCols' verify and resurfacing columns of ask_actions.
type actionCheck struct {
	checked, exit, verified, reminded, escalated, relaned, stepDone           sql.NullInt64
	output, by, relaneTask, relaneMachine, relaneReason, evidence, stepDoneBy string
	retries                                                                   int
	retryable                                                                 bool
}

// actionOf is a's card action, or nil.
func actionOf(a orchapi.Ask) *orchapi.AskAction {
	if a.Card == nil {
		return nil
	}
	return a.Card.Action
}

// actionStateText is an action's state as a person reads it.
func actionStateText(a orchapi.Ask) string {
	x := a.ActionState
	if x == nil || a.Card == nil || (a.Card.Action == nil && a.Card.Verify == nil) {
		return ""
	}
	if t := notDoneText(a); t != "" {
		return t
	}
	if x.State == orchapi.ActionDone && x.VerifiedAt == nil && x.DoneEvidence != "" {
		return "Done: " + first(x.CheckedBy, "its asker") + " recorded it with evidence, not its check: " + quoted(x.DoneEvidence) + "."
	}
	if a.Card.Action == nil {
		switch x.State {
		case orchapi.ActionDone:
			return "Done: the asker's check passed" + x.VerifiedAt.UTC().Format(" (2006-01-02 15:04 UTC)") + "."
		case orchapi.ActionFailed:
			return "Failed: " + x.Reason + "."
		}
		return ""
	}
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(" (2006-01-02 15:04 UTC)")
	}
	where := a.Card.Action.Machine
	var out string
	switch x.State {
	case orchapi.ActionQueued:
		out = "Approved; waiting for " + where + " to take it."
	case orchapi.ActionRunning:
		out = "Running on " + where + at(x.StartedAt) + "."
	case orchapi.ActionDone:
		out = "Done on " + where + at(x.EndedAt) + "."
		if x.Reason != "" && x.VerifiedAt != nil { // failed, then done another way (check)
			out = "Done: the action failed (" + x.Reason + "), and the asker's check passed" + at(x.VerifiedAt) + "."
		}
	case orchapi.ActionFailed:
		out = "Failed: " + x.Reason + "."
		if x.Retryable && x.RetryUntil != nil {
			how := " It never reached " + where
			if x.Task == "" {
				how = " zm has no record that it reached " + where
			}
			out += how + ": its asker or a project owner can run it again unchanged until " + x.RetryUntil.UTC().Format("2006-01-02 15:04 UTC") + " (zm ask retry " + a.ID + ")."
		}
	}
	switch {
	case x.Retries == 1:
		out += " Started again once after it failed."
	case x.Retries > 1:
		out += " Started again " + strconv.Itoa(x.Retries) + " times after it failed."
	}
	if x.ConfirmedBy != "" {
		out += " Confirmed by " + x.ConfirmedBy + "."
	}
	return out
}

// actionListed reports whether an answered ask still sits with the open
// ones: its action is not confirmed, and not done for over a day.
func actionListed(x *orchapi.ActionState, now time.Time) bool {
	if x == nil || x.ConfirmedAt != nil {
		return false
	}
	at := x.EndedAt
	if x.VerifiedAt != nil {
		at = x.VerifiedAt
	}
	return x.State != orchapi.ActionDone || at == nil || now.Sub(*at) < doneListed
}

// handleConfirmAction is POST /v0/asks/{id}/action/confirm: a project owner
// saw how its action went. It changes nothing but where the ask is listed.
func (s *Service) handleConfirmAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.ActionConfirmRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	me, err := s.ownerOf(ctx, sub, a.Project)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	switch x := a.ActionState; {
	case x == nil:
		err = api.Errorf(api.CodeConflict, "ask %s has no action that ran", a.ID)
	case x.ConfirmedAt != nil:
	case x.State == orchapi.ActionApproved:
		err = api.Errorf(api.CodeConflict, "ask %s is approved, not done: its check has not passed", a.ID).
			WithHint("the asker runs zm ask check " + a.ID + ", or gives it up with --give-up")
	case x.State != orchapi.ActionDone && x.State != orchapi.ActionFailed:
		err = api.Errorf(api.CodeConflict, "ask %s's action is still %s", a.ID, x.State).WithHint("confirm it once it is done or failed")
	default:
		_, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET confirmed_by = ?, confirmed = ? WHERE ask = ? AND confirmed IS NULL`,
			me.Name, ms(s.now()), a.ID)
		if err == nil {
			a, err = s.ask(ctx, a.ID)
		}
	}
	respond(w, s.view(ctx, sub, a), err)
}
