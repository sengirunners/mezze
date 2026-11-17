package approvals

// Retry (SPEC 16 "Retry"): an approved run or command that failed before it
// started may start again, identical, without a new card or yes. Only one
// that provably never started: approvals never handed it to the team
// module (the retry asks again under the same request id, which the team
// module answers with any start it has, never a second), or the team
// module shows it was never handed to a machine (orchcore.AskActionStatus
// NeverStarted; the retry is a new request id, the old one refused for
// good). Its asker or a project owner asks for it, with the card's sha256;
// nothing in the request can change the action. It runs through runAction
// again: the frozen card and effect, the signed yes, the approver's grants
// now, the approving device still paired. At most maxActionRetries, and
// only until retryWithin after the yes: a yes is for now, never hours later.

import (
	"context"
	"net/http"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var actionRetryMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0022_action_retry", Script: `
ALTER TABLE ask_actions ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ask_actions ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ask_actions ADD COLUMN retryable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ask_actions ADD COLUMN attempt_at INTEGER;
ALTER TABLE ask_actions ADD COLUMN notice_owner TEXT NOT NULL DEFAULT '';
`},
}

// retryWithin is how long after the yes an action that never started may
// be started again; with the take window a retried action starts within
// about 90 minutes of the yes, or not at all.
var retryWithin = time.Hour

// maxActionRetries bounds the retries of one ask's action.
const maxActionRetries = 3

// retryState fills a's retry fields from askCols' columns.
func retryState(a *orchapi.Ask, c actionCheck) {
	x := a.ActionState
	if x == nil {
		return
	}
	x.Retries = c.retries
	if c.retryable && (a.Card != nil && a.Card.Action != nil && (a.Card.Action.Kind == orchapi.ActionRun || a.Card.Action.Kind == orchapi.ActionCommand)) && x.State == orchapi.ActionFailed && x.ConfirmedAt == nil && c.retries < maxActionRetries && a.Decision != nil {
		until := a.Decision.Time.Add(retryWithin)
		x.Retryable, x.RetryUntil = true, &until
	}
}

// handleRetryAction is POST /v0/asks/{id}/action/retry.
func (s *Service) handleRetryAction(w http.ResponseWriter, r *http.Request) {
	var req orchapi.ActionRetryRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	a, err := s.retryAction(ctx, sub, r.PathValue("id"), req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, s.view(ctx, sub, a), nil)
}

// retryAction starts ask id's failed action again for sub, its asker or a
// project owner, when it may (see the file comment).
func (s *Service) retryAction(ctx context.Context, sub *authz.Subject, id string, req orchapi.ActionRetryRequest) (orchapi.Ask, error) {
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		return a, err
	}
	// Its asker, or a person (never another session, which reaches the
	// owner's socket as every process of theirs does) who owns or
	// administers the project.
	if sub.Address == "" || sub.Address != a.AskedBy {
		_, owner := s.ownerOf(ctx, sub, a.Project)
		if pa, perr := api.ParseAddress(sub.Address); owner != nil || sub.Address != "" && (perr != nil || pa.Kind == api.KindSession) {
			return a, api.Errorf(api.CodeForbidden, "only the asker of ask %s or a person who owns or administers its project starts its action again", a.ID)
		}
	}
	if req.CardSHA == "" || req.CardSHA != a.CardSHA {
		return a, api.Errorf(api.CodeConflict, "card_sha256 is not ask %s's card", a.ID).WithHint("read the ask again (zm ask show " + a.ID + ")")
	}
	act, err := frozenAction(a)
	if err != nil {
		return a, err
	}
	x := a.ActionState
	switch {
	case act == nil || (act.Kind != orchapi.ActionRun && act.Kind != orchapi.ActionCommand):
		return a, api.Errorf(api.CodeConflict, "ask %s has no run or command action to start again", a.ID)
	case x == nil || a.Decision == nil:
		return a, api.Errorf(api.CodeConflict, "ask %s's action was not approved", a.ID)
	case x.State != orchapi.ActionFailed:
		return a, api.Errorf(api.CodeConflict, "ask %s's action is %s, not failed", a.ID, x.State)
	case x.ConfirmedAt != nil:
		return a, api.Errorf(api.CodeConflict, "ask %s's failure was confirmed by %s: it does not run again", a.ID, x.ConfirmedBy).WithHint("ask again")
	case !x.Retryable && x.Retries >= maxActionRetries:
		return a, api.Errorf(api.CodeConflict, "ask %s's action was started again %d times already", a.ID, x.Retries).WithHint("ask again")
	case !x.Retryable:
		return a, api.Errorf(api.CodeConflict, "ask %s's action may have started, or would fail the same way: it does not run again", a.ID).WithHint("fix what you can and ask again")
	case !s.now().Before(*x.RetryUntil):
		return a, api.Errorf(api.CodeConflict, "ask %s was approved more than %s ago: a yes is for now", a.ID, plainDuration(retryWithin)).WithHint("ask again")
	case a.Objection != nil:
		return a, api.Errorf(api.CodeConflict, "an owner objected to ask %s's outcome: it does not run again", a.ID).WithHint("ask again")
	}
	// A recorded start moves to a new request id only while the team
	// module still shows, now, that it was never handed to a machine.
	if x.Task != "" {
		aa := s.askActions()
		if aa == nil {
			return a, api.Errorf(api.CodeUnavailable, "this daemon runs no actions on machines")
		}
		st, err := aa.AskActionStatus(ctx, a.Project, x.Task)
		if err != nil {
			return a, err
		}
		if st.State != orchapi.ActionFailed || !st.NeverStarted {
			return a, api.Errorf(api.CodeConflict, "ask %s's action may have started: it does not run again", a.ID).WithHint("fix what you can and ask again")
		}
	}
	// One conditional update claims the retry: two at once, or one racing a
	// confirm, start at most one attempt. The attempt (its request id)
	// moves on only past an attempt the team module took and refused.
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET state = 'queued', reason = '', task = '', started = NULL, ended = NULL, notified = NULL,
		retryable = 0, retries = retries + 1, attempt = attempt + CASE WHEN task != '' THEN 1 ELSE 0 END, attempt_at = ?
		WHERE ask = ? AND state = 'failed' AND retryable = 1 AND confirmed IS NULL AND retries = ? AND retries < ?
		AND NOT EXISTS (SELECT 1 FROM ask_objections o WHERE o.ask = ask_actions.ask)`,
		ms(s.now()), a.ID, x.Retries, maxActionRetries)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return a, api.Errorf(api.CodeConflict, "ask %s's action changed meanwhile", a.ID).WithHint("read it again (zm ask show " + a.ID + ")")
	}
	s.startRetry(ctx, a.ID)
	return s.ask(ctx, a.ID)
}

// startRetry runs ask's stored action effect again through runAction (a
// retry the team module could not take yet is tried again by the watch).
func (s *Service) startRetry(ctx context.Context, ask string) {
	a, err := s.ask(ctx, ask)
	if err != nil {
		return
	}
	var e effect
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT kind, id, target, body, payload_sha, state, error FROM decision_effects WHERE ask = ? AND kind = 'action'`, a.ID).
		Scan(&e.Kind, &e.ID, &e.Target, &e.Body, &e.SHA, &e.State, &e.Error); err != nil {
		s.d.Log.Warn("approvals: read a retried action's effect", "ask", a.ID, "err", err)
		return
	}
	if textSHA(e.Kind+"\n"+e.Target+"\n"+e.Body) != e.SHA {
		s.failAction(ctx, a.ID, "its stored action changed after the yes")
		return
	}
	if err := s.runAction(ctx, a, e); err != nil {
		s.d.Log.Info("approvals: a retried action waits to start", "ask", a.ID, "err", err)
	}
}
