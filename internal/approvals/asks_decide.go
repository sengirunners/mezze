package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// ---- decide ----

func (s *Service) handleDecide(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var req orchapi.DecideRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	me, err := s.activeApprover(r.Context(), rm)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if s.isRelease(r.Context(), r.PathValue("id"), req.Decision) {
		a, err := s.releaseUnchecked(r.Context(), r.PathValue("id"), me)
		respond(w, a, err)
		return
	}
	d, err := s.signedAnswer(r.Context(), me, r.PathValue("id"), req, nil)
	respond(w, d, err)
}

// Decide records the calling approver's answer to ask id and signs its
// receipt. The approver check comes first, so a device that is not an
// approver learns nothing about which asks exist. One decision per ask: the
// same answer again returns the same receipt; any other is a conflict.
func (s *Service) Decide(ctx context.Context, rm *core.Remote, id, decision string) (orchapi.Decision, error) {
	me, err := s.activeApprover(ctx, rm)
	if err != nil {
		return orchapi.Decision{}, err
	}
	return s.signedAnswer(ctx, me, id, orchapi.DecideRequest{Decision: decision}, nil)
}

// normAnswer checks an answer against the ask: its decision, text and who it
// speaks for, in their stored forms.
func normAnswer(a orchapi.Ask, req orchapi.DecideRequest) (orchapi.DecideRequest, error) {
	req.Text = strings.TrimSpace(textclean.Clean(req.Text))
	if a.Subject.IsPR() {
		if req.Text != "" || req.As != "" {
			return req, api.Errorf(api.CodeBadRequest, "a pull request's ask takes a decision only: its receipt carries no text")
		}
	} else {
		if len(req.Text) > maxAnswer {
			return req, api.Errorf(api.CodeBadRequest, "an answer is at most %d bytes", maxAnswer)
		}
		if req.Decision == "" && req.Text != "" {
			req.Decision = orchapi.DecisionOther
		}
		switch {
		case req.As == "":
			req.As = orchapi.AnsweredAsOwner
		case req.As == orchapi.AnsweredAsOwner:
		case req.As == orchapi.AnsweredAsContact:
			if a.For == nil {
				return req, api.Errorf(api.CodeBadRequest, "only an ask held for an outside party has a contact to speak for")
			}
		default:
			return req, api.Errorf(api.CodeBadRequest, "as must be %s or %s", orchapi.AnsweredAsOwner, orchapi.AnsweredAsContact)
		}
		if req.Decision == orchapi.DecisionOther {
			if req.Text == "" {
				return req, api.Errorf(api.CodeBadRequest, "a free-text answer needs its text")
			}
			return req, nil
		}
		if req.Decision == orchapi.DecisionNeedsContext {
			if req.Text == "" {
				return req, api.Errorf(api.CodeBadRequest, "sending an ask back needs a note saying what context is missing")
			}
			return req, nil
		}
	}
	for _, o := range a.Options {
		if o == req.Decision {
			return req, nil
		}
	}
	return req, api.Errorf(api.CodeBadRequest, "decision must be one of %s", strings.Join(a.Options, ", "))
}

// sentBackOverDefault refuses sending back an ask whose default stands: the
// asker acts on that default, and a sent-back ask could no longer be undone.
func sentBackOverDefault(a orchapi.Ask, req orchapi.DecideRequest) error {
	if req.Decision == orchapi.DecisionNeedsContext && a.Decision == nil && a.DefaultApplied != nil && a.DefaultUndone == nil {
		return api.Errorf(api.CodeConflict, "ask %s has a default that stands", a.ID).
			WithHint("undo the default first (zm ask undo), or answer the ask with a choice").
			WithData(orchapi.Refusal{Reason: orchapi.ReasonDefaultStands})
	}
	return nil
}

// record is the one way a decision is made, after the caller proved its
// approver. check, when set, runs on the ask before anything is written.
func (s *Service) record(ctx context.Context, me approverRow, id string, req orchapi.DecideRequest, check func(orchapi.Ask) error) (orchapi.Decision, error) {
	a, err := s.ask(ctx, id)
	if err != nil {
		return orchapi.Decision{}, err
	}
	if s.isHeld(ctx, a.ID) {
		return orchapi.Decision{}, errHeldAnswer(a.ID)
	}
	if check != nil {
		if err := check(a); err != nil {
			return orchapi.Decision{}, err
		}
	}
	if err := s.onlyTheirAnswer(ctx, a, me); err != nil {
		return orchapi.Decision{}, err
	}
	if a.Withdrawn != nil {
		return orchapi.Decision{}, api.Errorf(api.CodeConflict, "ask %s was withdrawn", a.ID).WithData(orchapi.Refusal{Reason: orchapi.ReasonWithdrawn})
	}
	// Before the answer's own checks: a held card the judge joined to another
	// ask changed its choices, and a stale reader is told the card changed.
	if a.Decision == nil {
		if err := cardNotRead(req, a); err != nil {
			return orchapi.Decision{}, err
		}
	}
	req, err = normAnswer(a, req)
	if err != nil {
		return orchapi.Decision{}, err
	}
	if err := sentBackOverDefault(a, req); err != nil {
		return orchapi.Decision{}, err
	}
	if p := a.Decision; p != nil {
		if p.Decision == req.Decision && p.Approver == me.Name && p.Method == me.method() && p.Text == req.Text && p.AnsweredAs == req.As {
			return *p, nil
		}
		return orchapi.Decision{}, api.Errorf(api.CodeConflict, "ask %s was already decided", a.ID).WithData(p)
	}
	key, err := s.signer()
	if err != nil {
		return orchapi.Decision{}, err
	}
	now := store.FromMillis(ms(s.now()))
	rc := orchapi.Receipt{Project: a.Project, Ask: a.ID, Subject: a.Subject, Decision: req.Decision,
		Approver: me.Name, Node: me.Node, Time: now.Format(TimeFormat)}
	if !a.Subject.IsPR() {
		rc.V, rc.CardSHA, rc.TextSHA, rc.AnsweredAs = orchapi.ReceiptVersion2, a.CardSHA, textSHA(req.Text), req.As
		rc.Action = actionLine(a.Card)
	}
	rc = Sign(key, rc)
	d := orchapi.Decision{Ask: a.ID, Decision: req.Decision, Approver: me.Name, Method: me.method(), Time: now, Receipt: rc, Text: req.Text, AnsweredAs: req.As}
	s.answeredFrom(ctx, me, &d)
	if a.Card != nil {
		d.Answer = choiceAnswer(a.Card, req.Decision, req.Text)
	}
	notTheirs, theirs := s.notTheirsLine(ctx, a, me, req)
	d.NotTheirs = theirs
	effects, err := s.makeEffects(ctx, a, d, first(me.NodeName, me.Node), notTheirs)
	if err != nil {
		return orchapi.Decision{}, err
	}
	fresh := false
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var prev orchapi.Ask
		prev, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+askFrom+` WHERE a.id = ?`, a.ID))
		if err != nil {
			return err
		}
		if p := prev.Decision; p != nil {
			if p.Decision == req.Decision && p.Approver == me.Name && p.Method == me.method() && p.Text == req.Text && p.AnsweredAs == req.As {
				d = *p
				return nil
			}
			return api.Errorf(api.CodeConflict, "ask %s was already decided: %s by %s", a.ID, p.Decision, p.Approver).
				WithData(p)
		}
		if prev.Withdrawn != nil {
			return api.Errorf(api.CodeConflict, "ask %s was withdrawn", a.ID).WithData(orchapi.Refusal{Reason: orchapi.ReasonWithdrawn})
		}
		if err := s.stillAnswerable(ctx, tx, me, prev, a, req); err != nil {
			return err
		}
		if err := sentBackOverDefault(prev, req); err != nil {
			return err
		}
		if req.Decision == orchapi.DecisionNeedsContext {
			// Sent back, the same question may be asked again: free its dedupe slot.
			if _, err := tx.ExecContext(ctx, `UPDATE asks SET dedupe = '' WHERE id = ?`, a.ID); err != nil {
				return err
			}
		}
		if req.Decision != orchapi.DecisionNeedsContext { // sent back, it neither keeps nor replaces the default
			effects = withDefaultNote(effects, prev, req.Decision) // the default as of this commit
		}
		rb, _ := json.Marshal(rc)
		// next_try is the safety net if this daemon dies before the first
		// delivery below; a minute out, so the ticker does not race that try.
		if _, err := tx.ExecContext(ctx, `INSERT INTO decisions (ask, decision, approver, node_id, time, receipt, text, answered_as, device, next_try, method)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, req.Decision, me.Name, me.Node, ms(now), string(rb), req.Text, req.As, first(me.NodeName, me.Node), ms(now.Add(time.Minute)), me.method()); err != nil {
			return err
		}
		if err := setAnsweredFrom(ctx, tx, d); err != nil {
			return err
		}
		if err := insertEffects(ctx, tx, a.ID, effects, now); err != nil {
			return err
		}
		if err := insertApproved(ctx, tx, a, req.Decision, me, now); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	if err != nil {
		return orchapi.Decision{}, err
	}
	if fresh {
		if err := s.recorded(ctx, a, &d, me); err != nil {
			d.MailError = err.Error()
		}
	}
	return d, nil
}

// recorded runs after a decision commits: the event, the answer to the
// ask's mail (which takes it, so the asking session leaves needs_human), the
// write-back to the work item, and the GitHub side effects in the background.
// It returns the mail answer's failure, which the decision response reports
// (the decision stands).
func (s *Service) recorded(ctx context.Context, a orchapi.Ask, d *orchapi.Decision, me approverRow) error {
	ctx = context.WithoutCancel(ctx)
	s.deliver(ctx, a, d, first(me.NodeName, me.Node))
	if a.Subject.IsPR() && s.Publisher != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			pctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			if err := s.Publisher.Publish(pctx, a, *d); err != nil && !errors.Is(err, ErrNoCredential) {
				s.d.Log.Warn("approvals: post to GitHub", "ask", a.ID, "err", err)
			}
		}()
	}
	if d.MailError != "" {
		return fmt.Errorf("%s", d.MailError)
	}
	return nil
}

// handleWithdraw takes an open ask back: its asker, or anyone who may operate
// on its area. It decides nothing and signs nothing.
func (s *Service) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	a, _, err = s.withdraw(ctx, sub, a)
	respond(w, s.view(ctx, sub, a), err)
}

// withdraw withdraws a, an ask sub may read, for its asker or who may operate
// on its area, and says whether this call withdrew it. Withdrawn before is
// the same answer (false); a decided ask, or one whose default stands, is a
// conflict.
func (s *Service) withdraw(ctx context.Context, sub *authz.Subject, a orchapi.Ask) (orchapi.Ask, bool, error) {
	if a.AskedBy != sub.Address && !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
		return a, false, api.Errorf(api.CodeForbidden, "only who asked, or who may operate here, withdraws an ask")
	}
	now := ms(s.now())
	// A held ask's withdrawal is ordered against its release (holdMu): sent
	// first, it is withdrawn as any sent ask; else nothing is ever sent.
	s.holdMu.Lock()
	held := s.isHeld(ctx, a.ID)
	if !held {
		s.holdMu.Unlock()
	} else {
		defer s.holdMu.Unlock()
	}
	// A default that stands is undone or answered first: withdrawing would
	// leave the asker acting on it with no card left to undo it from.
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET withdrawn = ?, dedupe = '' WHERE id = ? AND withdrawn IS NULL
		AND (default_applied IS NULL OR default_undone IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.ask = asks.id)`, now, a.ID)
	if err != nil {
		return a, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a, err = s.ask(ctx, a.ID)
		if err == nil && a.Decision != nil {
			err = api.Errorf(api.CodeConflict, "ask %s was already decided: %s by %s", a.ID, a.Decision.Decision, a.Decision.Approver)
		} else if err == nil && a.Withdrawn == nil && a.DefaultApplied != nil && a.DefaultUndone == nil {
			err = api.Errorf(api.CodeConflict, "ask %s has a default that stands", a.ID).
				WithHint("the owner undoes it (zm ask undo) or answers it first")
		}
		return a, false, err // withdrawn before: the same answer
	}
	if held { // never sent: no mail to answer
		_ = s.unhold(context.WithoutCancel(ctx), a.ID)
		a, err = s.ask(ctx, a.ID)
		return a, true, err
	}
	// The answer takes the ask's mail. Only an agent that asked is told: who
	// withdrew it is not told what they just did, and a person gets no notice
	// from zm at all (SPEC 9 "No notice about what you just did"; at a team
	// authority the answer comes from its own human, never the asker).
	local := s.d.Projects.Local("")
	silent := a.AskedBy == sub.Address || !orchapi.AgentSender(a.AskedBy)
	if _, err := orchcore.Mail(s.d).Reply(orchcore.AsAskNotice(context.WithoutCancel(ctx)), local, a.Project, a.ID,
		orchapi.ReplyRequest{Body: "withdrawn by " + sub.Address + "; nothing was decided", Silent: silent}); err != nil {
		s.d.Log.Warn("approvals: answer a withdrawn ask's mail", "ask", a.ID, "err", err)
	}
	a, err = s.ask(ctx, a.ID)
	return a, true, err
}
