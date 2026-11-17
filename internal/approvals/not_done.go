package approvals

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Approved, not done (SPEC 16). A yes on a choice that needs
// work is not the end of it: the ask records APPROVED, NOT DONE on its item
// and stays in Decisions until the work reports success. Work is an action
// zm runs (its state is ask_actions', as before) and, when the card carries
// one, the asker's verify check: the asker runs it (zm ask check) after any
// action finished, and only an exit of 0 makes it done. There is no second
// state machine: a verify-only ask gets an ask_actions row of kind verify in
// state approved, and an action with a check waits in approved once it ran;
// one that failed may still be done by hand, which its check records.
// Undone work comes back to the asker, then to a fresh lane, then to the
// approver (relane.go; the times are the project's [asks]); done work
// records DONE on the item. The asker or a project owner may record it done
// with evidence when its check cannot pass (a wrong check).

var notDoneMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0009_not_done", Script: `
ALTER TABLE ask_actions ADD COLUMN checked INTEGER;
ALTER TABLE ask_actions ADD COLUMN check_exit INTEGER;
ALTER TABLE ask_actions ADD COLUMN check_output TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_actions ADD COLUMN verified INTEGER;
ALTER TABLE ask_actions ADD COLUMN reminded INTEGER;
ALTER TABLE ask_actions ADD COLUMN remind_mail TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_actions ADD COLUMN escalated INTEGER;
ALTER TABLE ask_actions ADD COLUMN escalate_mail TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_actions ADD COLUMN done_comment TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_actions ADD COLUMN done_notified INTEGER;
ALTER TABLE ask_actions ADD COLUMN checked_by TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_actions ADD COLUMN tried INTEGER;
UPDATE ask_actions SET reminded = created, escalated = created WHERE state IN ('queued', 'running');
CREATE INDEX ask_actions_undone ON ask_actions(state) WHERE state IN ('queued', 'running', 'approved') OR (state = 'done' AND done_comment != '' AND done_notified IS NULL);
`},
}

// kindVerify is the ask_actions kind of a verify check on a card with no
// action.
const kindVerify = "verify"

// gaveUp starts the reason of work its asker gave up.
const gaveUp = orchapi.ActionGaveUpPrefix

// The default not-done times, from the decision (a project's [asks] sets
// its own).
var (
	// remindAfter: approved work still not done then comes back to the asker.
	remindAfter = 24 * time.Hour
	// relaneAfter: then to a fresh lane (relane.go).
	relaneAfter = 48 * time.Hour
	// escalateAfter: and then to the approver, and on top of Decisions
	// (at relaneAfter when no lane could start).
	escalateAfter = 72 * time.Hour
	// retryAfter: a reminder or DONE record that did not land is tried
	// again then (rows that keep failing never hold up the others).
	retryAfter = 10 * time.Minute
)

// notDoneState: the approver said yes and the work is not done yet.
func notDoneState(state string) bool {
	return state == orchapi.ActionQueued || state == orchapi.ActionRunning || state == orchapi.ActionApproved
}

// verifyOf is a's card verify check, or nil.
func verifyOf(a orchapi.Ask) *orchapi.AskVerify {
	if a.Card == nil {
		return nil
	}
	return a.Card.Verify
}

// cleanVerify checks and freezes a card's verify check: one bounded line,
// on one of the card's choices (default the action's, else the first). An
// ack-mail card takes none: zm acks in the decision, so it is done then.
func cleanVerify(in orchapi.AskVerify, act *orchapi.AskAction, keys []string) (*orchapi.AskVerify, error) {
	cmd := strings.TrimSpace(in.Command)
	switch {
	case cmd == "":
		return nil, api.Errorf(api.CodeBadRequest, "verify needs a command that exits 0 once the work is done")
	case len(cmd) > orchapi.MaxVerifyCommand || !utf8.ValidString(cmd) || strings.ContainsAny(cmd, "\r\n\x00") || textclean.Clean(cmd) != cmd:
		return nil, api.Errorf(api.CodeBadRequest, "verify: one printable line of at most %d bytes", orchapi.MaxVerifyCommand)
	case act != nil && act.Kind == orchapi.ActionAckMail:
		return nil, api.Errorf(api.CodeBadRequest, "an ack-mail card takes no verify: zm acks the mails when the person answers")
	}
	on := strings.ToLower(strings.TrimSpace(in.On))
	if act != nil {
		if on != "" && on != act.On {
			return nil, api.Errorf(api.CodeBadRequest, "verify on %q: a card's check is on its action's choice (%s)", on, act.On)
		}
		on = act.On
	}
	if on == "" {
		on = keys[0]
	}
	found := false
	for _, k := range keys {
		found = found || k == on
	}
	if !found {
		return nil, api.Errorf(api.CodeBadRequest, "verify on %q: one of the card's choices (%s)", on, strings.Join(keys, ", "))
	}
	return &orchapi.AskVerify{Command: cmd, On: on}, nil
}

// needsWork reports whether decision on a approves work that is not done
// by the answer itself: a running action's choice, or the verify check's.
func needsWork(a orchapi.Ask, decision string) bool {
	if act := actionOf(a); act != nil && runsSomething(act.Kind) && act.On == decision {
		return true
	}
	v := verifyOf(a)
	return v != nil && v.On == decision
}

// insertApproved records a verify-only ask's approved, not done state in
// the decision's transaction (an action's row comes from insertAction), and
// on any row whose signed yes me gave (freezeApprover).
func insertApproved(ctx context.Context, tx *sql.Tx, a orchapi.Ask, decision string, me approverRow, now time.Time) error {
	v := verifyOf(a)
	if act := actionOf(a); v != nil && v.On == decision && (act == nil || act.Kind == orchapi.ActionHand) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ask_actions (ask, kind, state, created) VALUES (?, ?, 'approved', ?)`,
			a.ID, kindVerify, ms(now)); err != nil {
			return err
		}
	}
	return freezeApprover(ctx, tx, a.ID, me)
}

// notDoneLine is the APPROVED, NOT DONE line of a decision that approves
// work, on the item and in the asker's mail.
func notDoneLine(a orchapi.Ask) string {
	until := "the action reports success"
	if v := verifyOf(a); v != nil {
		until = fmt.Sprintf("its check passes: zm ask check %s runs %s", a.ID, quoted(v.Command))
		if act := actionOf(a); act != nil && act.Kind != orchapi.ActionHand {
			until = "the action finished and " + until
		}
	}
	return fmt.Sprintf("APPROVED, NOT DONE: zm keeps ask %s open in Decisions until %s. Do not close the work item before zm records DONE.", a.ID, until)
}

// verifyText is the card's check as the approver reads it.
func verifyText(a orchapi.Ask) string {
	v := verifyOf(a)
	if v == nil {
		return ""
	}
	return fmt.Sprintf("If you choose %s, it stays approved, not done until the asker's check passes: %s", strings.ToUpper(v.On), quoted(v.Command))
}

// notDoneText is what is left of approved work, in plain words.
func notDoneText(a orchapi.Ask) string {
	x := a.ActionState
	if x == nil || verifyOf(a) == nil || x.State != orchapi.ActionApproved {
		return ""
	}
	s := "Approved, not done: the asker's check has not passed yet."
	if st := stepOf(a); st != nil {
		who := first(strings.TrimPrefix(a.Owner, "human:"), "the person")
		s = "Approved: waiting on " + who + "'s own step (" + st.Label + " on " + st.Domain + ")."
		if x.StepDoneAt != nil {
			s = "Approved: " + who + " said the step is done" + x.StepDoneAt.UTC().Format(" (2006-01-02 15:04 UTC)") + "; not done until the check passes."
		}
	}
	if act := actionOf(a); x.EndedAt != nil && act != nil && act.Kind != orchapi.ActionHand {
		s = "Ran on " + a.Card.Action.Machine + x.EndedAt.UTC().Format(" (2006-01-02 15:04 UTC)") + "; not done until the asker's check passes."
	}
	if x.CheckedAt != nil && x.CheckExit != nil {
		s += fmt.Sprintf(" Last check %s exited %d", x.CheckedAt.UTC().Format("2006-01-02 15:04 UTC"), *x.CheckExit)
		if x.CheckOutput != "" {
			s += ": " + quoted(lastLine(x.CheckOutput))
		}
		s += "."
	}
	return s + laneText(x)
}

// laneText is the fresh lane's part of what is left, or "".
func laneText(x *orchapi.ActionState) string {
	switch {
	case x.RelanedAt == nil:
		return ""
	case x.RelaneTask == "" && x.RelaneReason == "":
		return "" // passed before fresh lanes existed
	case x.RelaneTask != "":
		return " zm handed it to a fresh lane on " + x.RelaneMachine + x.RelanedAt.UTC().Format(" (2006-01-02 15:04 UTC)") + ", task " + x.RelaneTask + "."
	}
	return " No fresh lane could take it: " + x.RelaneReason + "."
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "…"
	}
	return s
}

// actionDone records that ask's work is done (from the states in from) and
// sets the DONE record's write id; the watch writes it.
func (s *Service) actionDone(ctx context.Context, ask string, ended int64, verified bool, from ...string) (bool, error) {
	q := `UPDATE ask_actions SET state = 'done', started = coalesce(started, ?), ended = coalesce(ended, ?), done_comment = ?`
	args := []any{ended, ended, ids.New()}
	if verified {
		q += `, verified = ?`
		args = append(args, ms(s.now()))
	}
	q += ` WHERE ask = ? AND state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(from)), ",") + `)`
	args = append(args, ask)
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.d.Store.Registry().ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// handleCheck is POST /v0/asks/{id}/check: the asker ran the card's verify
// command and reports its exit, or gives the approved work up (which fails
// it, on top of Decisions until an owner confirms).
func (s *Service) handleCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.CheckRequest
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
	if err == nil {
		a, err = s.check(ctx, sub, a, req)
	}
	respond(w, s.view(ctx, sub, a), err)
}

func (s *Service) check(ctx context.Context, sub *authz.Subject, a orchapi.Ask, req orchapi.CheckRequest) (orchapi.Ask, error) {
	if a.AskedBy != sub.Address && !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
		return a, api.Errorf(api.CodeForbidden, "only its asker, or who may operate on its area, reports an ask's check")
	}
	if req.Evidence != "" {
		return s.doneWithEvidence(ctx, sub, a, req)
	}
	v := verifyOf(a)
	giveUp := req.GiveUp != ""
	if v == nil && (!giveUp || actionOf(a) == nil) {
		return a, api.Errorf(api.CodeConflict, "ask %s has no verify check", a.ID)
	}
	if req.CardSHA != a.CardSHA || cardSHA(marshal(*a.Card)) != a.CardSHA {
		return a, api.Errorf(api.CodeConflict, "ask %s: the card you read is not the one it carries", a.ID).WithHint("read it again: zm ask show " + a.ID)
	}
	x := a.ActionState
	if x == nil {
		return a, api.Errorf(api.CodeConflict, "ask %s is not approved: nobody picked the choice that needs work", a.ID)
	}
	if giveUp {
		give := strings.TrimSpace(textclean.Clean(req.GiveUp))
		switch {
		case req.Exit != nil:
			return a, api.Errorf(api.CodeBadRequest, "give_up or exit, not both")
		case give == "" || len(give) > 500 || strings.ContainsAny(give, "\r\n") || unsafePresentation(give):
			return a, api.Errorf(api.CodeBadRequest, "give_up: one plain line of at most 500 bytes saying why")
		case !notDoneState(x.State):
			return a, api.Errorf(api.CodeConflict, "ask %s's work is already %s", a.ID, x.State)
		}
		reason := gaveUp + give
		if x.State != orchapi.ActionApproved || x.RelaneTask != "" {
			reason += " (zm does not stop a task that already started, a fresh lane included; its result is no longer recorded)"
		}
		s.failAction(ctx, a.ID, reason)
		return s.ask(ctx, a.ID)
	}
	if req.Exit == nil {
		return a, api.Errorf(api.CodeBadRequest, "exit is required: the check's exit status")
	}
	if err := closable(a); err != nil {
		return a, err
	}
	now := ms(s.now())
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET checked = ?, check_exit = ?, check_output = ?, checked_by = ? WHERE ask = ? AND state = ?`,
		now, *req.Exit, checkOutput(req.Output), sub.Address, a.ID, x.State)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return a, api.Errorf(api.CodeConflict, "ask %s's work changed meanwhile", a.ID).WithHint("read it again: zm ask show " + a.ID)
	}
	if *req.Exit == 0 {
		ok, err := s.actionDone(ctx, a.ID, now, true, x.State)
		if err != nil {
			return a, err
		}
		if ok && x.State == orchapi.ActionFailed {
			s.unconfirm(ctx, a.ID)
		}
		if ok {
			s.notifyDone(ctx, a.ID)
		}
	}
	return s.ask(ctx, a.ID)
}

// closable is nil when a's state is one a check (or evidence) may close:
// approved, or failed by its machine after it started, when the asker may
// have done the work another way. Not when the action never
// started (who approved it may not: nobody did the work for them), nor
// when the asker gave it up (its task may still run); and a check that ran
// before the action finished proves nothing.
func closable(a orchapi.Ask) error {
	x := a.ActionState
	switch x.State {
	case orchapi.ActionApproved:
	case orchapi.ActionFailed:
		if x.Task == "" || strings.HasPrefix(x.Reason, gaveUp) {
			return api.Errorf(api.CodeConflict, "ask %s's work failed for good: %s", a.ID, x.Reason).
				WithHint("ask again, naming ask " + a.ID)
		}
	case orchapi.ActionQueued, orchapi.ActionRunning:
		return api.Errorf(api.CodeConflict, "ask %s's action is still %s", a.ID, x.State).WithHint("run the check once it finished: zm ask show " + a.ID)
	default:
		return api.Errorf(api.CodeConflict, "ask %s's work is already %s", a.ID, x.State)
	}
	return nil
}

// unconfirm: an owner who confirmed a failure sees the work done after all.
func (s *Service) unconfirm(ctx context.Context, ask string) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET confirmed_by = '', confirmed = NULL WHERE ask = ?`, ask)
}

// doneWithEvidence records a's approved work done without its check
// passing (a check that can never pass kept finished work
// approved, not done): only its asker or a project owner, only while the
// check is what is left, with one plain line of evidence that the DONE
// record quotes and names them for.
func (s *Service) doneWithEvidence(ctx context.Context, sub *authz.Subject, a orchapi.Ask, req orchapi.CheckRequest) (orchapi.Ask, error) {
	by := sub.Address
	person := false // the caller is a person who owns or administers the project
	if me, err := s.ownerOf(ctx, sub, a.Project); err == nil && sub.Session == "" {
		person = true
		if a.AskedBy != sub.Address {
			by = "human:" + me.Name
		}
	}
	if a.AskedBy != sub.Address && !person {
		// Not its asker: a person who owns or administers the project, never
		// another agent session (a session on the owner's socket passes
		// ProjectAdmin, and the record would name the person).
		return a, api.Errorf(api.CodeForbidden, "only its asker, or a person who owns or administers the project, records an ask done with evidence")
	}
	ev := strings.TrimSpace(textclean.Clean(req.Evidence))
	switch x := a.ActionState; {
	case req.Exit != nil || req.GiveUp != "":
		return a, api.Errorf(api.CodeBadRequest, "evidence alone: no exit, no give_up")
	case req.CardSHA != a.CardSHA || a.Card == nil || cardSHA(marshal(*a.Card)) != a.CardSHA:
		return a, api.Errorf(api.CodeConflict, "ask %s: the card you read is not the one it carries", a.ID).WithHint("read it again: zm ask show " + a.ID)
	case ev == "" || len(ev) > 500 || strings.ContainsAny(ev, "\r\n") || unsafePresentation(ev):
		return a, api.Errorf(api.CodeBadRequest, "evidence: one plain line of at most 500 bytes saying what shows the work is done")
	case x == nil:
		return a, api.Errorf(api.CodeConflict, "ask %s is not approved: nobody picked the choice that needs work", a.ID)
	case verifyOf(a) == nil && x.State != orchapi.ActionFailed:
		// With no check, zm records the action's own result; only one its
		// machine refused or failed may have been done by hand.
		return a, api.Errorf(api.CodeConflict, "ask %s has no verify check: zm records its action's own result", a.ID)
	case stepOf(a) != nil:
		// A person's step: only zm's run of its check shows it done, never
		// anyone's word (SPEC 16 "Your step").
		return a, api.Errorf(api.CodeConflict, "ask %s waits on %s's own step: only its check closes it", a.ID, first(strings.TrimPrefix(a.Owner, "human:"), "the person")).
			WithHint("run zm ask check " + a.ID + "; if the check itself is wrong, withdraw the ask and ask again")
	case verifyOf(a) == nil && !person:
		// Nothing checks it: a person vouches, never the asking agent for
		// its own failed action (it may have run partway).
		return a, api.Errorf(api.CodeForbidden, "ask %s has no verify check: a person who owns or administers the project records it done with evidence", a.ID)
	}
	if err := closable(a); err != nil {
		return a, err
	}
	x := a.ActionState
	// One statement: the evidence and DONE land together or not at all.
	now := ms(s.now())
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET checked = ?, checked_by = ?, done_evidence = ?,
		state = 'done', started = coalesce(started, ?), ended = coalesce(ended, ?), done_comment = ? WHERE ask = ? AND state = ?`,
		now, by, ev, now, now, ids.New(), a.ID, x.State)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return a, api.Errorf(api.CodeConflict, "ask %s's work changed meanwhile", a.ID).WithHint("read it again: zm ask show " + a.ID)
	}
	if x.State == orchapi.ActionFailed {
		s.unconfirm(ctx, a.ID)
	}
	s.notifyDone(ctx, a.ID)
	return s.ask(ctx, a.ID)
}

// checkOutput is a check's last lines, cleaned and bounded.
func checkOutput(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) > orchapi.MaxCheckOutput {
		s = s[len(s)-orchapi.MaxCheckOutput:]
		s = strings.ToValidUTF8(s, "")
	}
	return strings.TrimSpace(textclean.Clean(s))
}

// notifyDone comments DONE on the item under the write id fixed when the
// work became done, until it landed. With no item there is nothing to write.
func (s *Service) notifyDone(ctx context.Context, ask string) {
	ctx = context.WithoutCancel(ctx)
	a, err := s.ask(ctx, ask)
	if err != nil || a.ActionState == nil || a.ActionState.State != orchapi.ActionDone {
		return
	}
	db := s.d.Store.Registry()
	var commentID string
	if err := db.QueryRowContext(ctx, `SELECT done_comment FROM ask_actions WHERE ask = ? AND done_notified IS NULL`, ask).Scan(&commentID); err != nil || commentID == "" {
		return
	}
	if a.Subject.Work != "" {
		rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder)
		if !ok {
			return
		}
		if err := rec.RecordComment(ctx, s.d.Projects.Local(""), a.Project, a.Subject.Work, commentID, doneText(a)); err != nil {
			s.d.Log.Warn("approvals: record DONE on the item", "ask", a.ID, "err", err)
			if !api.IsCode(err, api.CodeNotFound) {
				return // tried again after retryAfter
			}
		}
	}
	_, _ = db.ExecContext(ctx, `UPDATE ask_actions SET done_notified = ? WHERE ask = ?`, ms(s.now()), a.ID)
}

// doneText is the DONE record of approved work.
func doneText(a orchapi.Ask) string {
	question := ""
	if a.Card != nil {
		question = " (" + quoted(a.Card.Question) + ")"
	}
	x := a.ActionState
	if v := verifyOf(a); v != nil && x.VerifiedAt == nil && x.DoneEvidence != "" {
		return fmt.Sprintf("DONE (recorded with evidence, not its check): the approved work of ask %s%s is done, as %s recorded: %s. Its check %s had not passed.",
			a.ID, question, first(x.CheckedBy, "its asker"), quoted(x.DoneEvidence), quoted(v.Command))
	}
	if v := verifyOf(a); v != nil && x.VerifiedAt != nil {
		how := "is done"
		if x.Reason != "" {
			how = "is done another way after its action failed (" + x.Reason + ")"
		}
		return fmt.Sprintf("DONE (verified %s): the approved work of ask %s%s %s; its check %s exited 0, as %s reported.",
			x.VerifiedAt.UTC().Format("2006-01-02 15:04 UTC"), a.ID, question, how, quoted(v.Command), first(x.CheckedBy, "its asker"))
	}
	at := ""
	if x.EndedAt != nil {
		at = x.EndedAt.UTC().Format(" (2006-01-02 15:04 UTC)")
	}
	return fmt.Sprintf("DONE%s: the approved %s action of ask %s%s ran on %s and finished ok.", at, a.Card.Action.Kind, a.ID, question, a.Card.Action.Machine)
}

// watchNotDone resurfaces approved work that is not done (the asker, a
// fresh lane, the approver, each once, at the project's times) and retries
// DONE records. A row is acted on at most once per retryAfter, least
// recently tried first, so rows that keep failing never hold up the others.
func (s *Service) watchNotDone(ctx context.Context) {
	type row struct {
		ask, project, state          string
		created                      int64
		reminded, relaned, escalated sql.NullInt64
		noLane                       bool
	}
	db := s.d.Store.Registry()
	now := s.now()
	rows, err := db.QueryContext(ctx, `SELECT x.ask, a.project, x.state, x.created, x.reminded, x.relaned, x.escalated, x.relane_task = '' FROM ask_actions x JOIN asks a ON a.id = x.ask
		WHERE ((x.state IN ('queued', 'running', 'approved') AND (x.reminded IS NULL OR x.relaned IS NULL OR x.escalated IS NULL) AND x.created <= ?)
		   OR (x.state = 'done' AND x.done_comment != '' AND x.done_notified IS NULL))
		  AND (x.tried IS NULL OR x.tried <= ?) ORDER BY coalesce(x.tried, 0), x.created LIMIT 500`,
		ms(now.Add(-minNotDoneAfter)), ms(now.Add(-retryAfter)))
	if err != nil {
		return
	}
	var xs []row
	for rows.Next() {
		var x row
		if rows.Scan(&x.ask, &x.project, &x.state, &x.created, &x.reminded, &x.relaned, &x.escalated, &x.noLane) == nil {
			xs = append(xs, x)
		}
	}
	rows.Close()
	sets := map[string]askSettings{}
	for _, x := range xs {
		set, ok := sets[x.project]
		if !ok {
			set = s.askSettingsOf(x.project)
			sets[x.project] = set
		}
		age := now.Sub(store.FromMillis(x.created))
		remind := !x.reminded.Valid && age >= set.Remind
		relane := !x.relaned.Valid && age >= set.Relane
		// Every row read is marked tried, due or not, so rows not due yet
		// take their turn behind the others instead of filling every pass.
		_, _ = db.ExecContext(ctx, `UPDATE ask_actions SET tried = ? WHERE ask = ?`, ms(now), x.ask)
		if x.state != orchapi.ActionDone && !remind && !relane && (x.escalated.Valid || (age < set.Escalate && !(x.relaned.Valid && x.noLane))) {
			continue // nothing due yet
		}
		if x.state == orchapi.ActionDone {
			s.notifyDone(ctx, x.ask)
			continue
		}
		if remind {
			s.resurface(ctx, x.ask, false)
		}
		if relane {
			s.relane(ctx, x.ask)
		}
		if !x.escalated.Valid {
			// The approver hears at escalate_after, or as soon as no fresh
			// lane could take it.
			var at sql.NullInt64
			var reason string
			_ = db.QueryRowContext(ctx, `SELECT relaned, relane_reason FROM ask_actions WHERE ask = ?`, x.ask).Scan(&at, &reason)
			if age >= set.Escalate || (at.Valid && reason != "") {
				s.resurface(ctx, x.ask, true)
			}
		}
	}
}

// WatchNotDone runs one pass (tests).
func (s *Service) WatchNotDone(ctx context.Context) { s.watchNotDone(ctx) }

// resurface mails the asker (approver false) or the approver that approved
// work is still not done, once each, under a write id fixed before sending.
func (s *Service) resurface(ctx context.Context, ask string, approver bool) {
	ctx = context.WithoutCancel(ctx)
	a, err := s.ask(ctx, ask)
	if err != nil || a.ActionState == nil || !notDoneState(a.ActionState.State) || a.Decision == nil {
		return
	}
	col, mailCol := "reminded", "remind_mail"
	if approver {
		col, mailCol = "escalated", "escalate_mail"
	}
	db := s.d.Store.Registry()
	if _, err := db.ExecContext(ctx, `UPDATE ask_actions SET `+mailCol+` = ? WHERE ask = ? AND `+mailCol+` = ''`, ids.New(), ask); err != nil {
		return
	}
	var mailID string
	if err := db.QueryRowContext(ctx, `SELECT `+mailCol+` FROM ask_actions WHERE ask = ?`, ask).Scan(&mailID); err != nil {
		return
	}
	local := s.d.Projects.Local("")
	question := ""
	if a.Card != nil {
		question = " (" + quoted(a.Card.Question) + ")"
	}
	left := first(notDoneText(a), actionStateText(a))
	approved := a.Decision.Time.UTC().Format("2006-01-02 15:04 UTC")
	var to, text string
	if approver {
		to = s.approverAddress(ctx, a)
		told := ""
		if a.ActionState.RemindedAt != nil {
			told = " zm reminded the asker;"
		}
		text = fmt.Sprintf("APPROVED, NOT DONE: you approved ask %s%s at %s, and it is still not done. %s%s it stays on top of your Decisions until it is done.",
			a.ID, question, approved, left, told)
		if notDoneText(a) == "" {
			text += laneText(a.ActionState) // else it is in left already
		}
	} else {
		if orchapi.AgentSender(a.AskedBy) {
			to = a.AskedBy
		} else if a.Subject.Work != "" {
			to, _, _, _ = orchcore.Work(s.d).MailTarget(ctx, local, a.Project, a.Subject.Work)
		}
		next := fmt.Sprintf("zm runs its action; if it is stuck, give it up: zm ask check %s --give-up \"<why>\"", a.ID)
		if v := verifyOf(a); v != nil {
			next = fmt.Sprintf("Finish it, then run zm ask check %s (or zm ask check %s --give-up \"<why>\"; if it is done but the check is wrong, zm ask check %s --evidence \"<what shows it>\")", a.ID, a.ID, a.ID)
		}
		set := s.askSettingsOf(a.Project)
		then := "it goes back to " + a.Decision.Approver
		if set.LaneMachine != "" && verifyOf(a) != nil && a.ActionState.State == orchapi.ActionApproved {
			then = "zm tries to hand it to a fresh lane on " + laneWhere(set)
		}
		text = fmt.Sprintf("APPROVED, NOT DONE: %s approved ask %s%s at %s, and it is still not done. %s %s. In %s %s.",
			a.Decision.Approver, a.ID, question, approved, left, next, plainDuration(set.Relane-set.Remind), then)
	}
	if to != "" {
		if _, err := orchcore.Mail(s.d).Send(ctx, local, api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}, a.Area,
			orchapi.SendMailRequest{ID: mailID, To: to, Body: text}); err != nil {
			s.d.Log.Warn("approvals: resurface approved work that is not done", "ask", a.ID, "err", err)
			return
		}
	}
	_, _ = db.ExecContext(ctx, `UPDATE ask_actions SET `+col+` = ? WHERE ask = ? AND `+col+` IS NULL`, ms(s.now()), a.ID)
}

// approverAddress is who approved a's work, as a person's address: the
// approver device's person, else the owner who answered, else this
// machine's human.
func (s *Service) approverAddress(ctx context.Context, a orchapi.Ask) string {
	d := a.Decision
	if d.Method == orchapi.MethodApprover {
		var id string
		_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM approvers WHERE name = ?`, d.Approver).Scan(&id)
		// approvers.identity holds the identity's ID; an address names it.
		if id != "" {
			if who, err := s.d.Projects.Identity(ctx, id); err == nil && who.Kind == api.IdentityHuman {
				if ad, err := api.ParseAddress("human:" + who.Name); err == nil && ad.Kind == api.KindHuman {
					return ad.String()
				}
			}
		}
		return s.d.Config.HumanAddress()
	}
	if ad, err := api.ParseAddress("human:" + d.Approver); d.Approver != "" && err == nil && ad.Kind == api.KindHuman {
		return ad.String()
	}
	return s.d.Config.HumanAddress()
}

// plainDuration is d as a person reads it: "24 hours", "90 minutes".
func plainDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "a moment"
	case d%time.Hour == 0:
		if h := int(d / time.Hour); h != 1 {
			return fmt.Sprintf("%d hours", h)
		}
		return "1 hour"
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}
