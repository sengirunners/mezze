package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Write-back (SPEC 16 "Answers land on the work"): a decision on a work item
// becomes a comment on that item, whoever asked and whether or not the asking
// session still runs. A decision is committed before this runs, and a failed
// try is retried from the row until the comment lands, across restarts. The
// comment's write ID is a random server-owned effect ID enforced at the sink.

// retryTick is how often the daemon looks for answers still to deliver.
var retryTick = 30 * time.Second

// deliverTimeout bounds one try, so a slow tracker never holds up a person's tap.
const deliverTimeout = 10 * time.Second

const maxRetryWait = time.Hour

// testHookWriteBack, when set, can fail a write-back (tests only).
var testHookWriteBack func() error

// decisionText is the answer as a person or an agent reads it, on the work
// item and in the asker's mail. The first words match what agents and agent mail
// tooling search for.
func decisionText(a orchapi.Ask, d orchapi.Decision, device string) string {
	at := d.Time.UTC().Format("2006-01-02 15:04 UTC")
	answer := quoted(choiceAnswer(a.Card, d.Decision, d.Text))
	device = quoted(device)
	question := ""
	if a.Card != nil {
		question = "\nQuestion (asker supplied): " + quoted(a.Card.Question)
	}
	switch {
	case a.For != nil && d.AnsweredAs == orchapi.AnsweredAsContact:
		return fmt.Sprintf("PROGRAM DECISION (%s, %s, heard by %s via %s, %s) on %s (%s): %s%s",
			quoted(a.For.Contact), quoted(a.For.Group), d.Approver, device, at, a.ID, a.Topic, answer, question)
	case a.For != nil:
		return fmt.Sprintf("OWNER DECISION (%s via %s, %s) on %s (%s, asked of %s): %s%s",
			d.Approver, device, at, a.ID, a.Topic, quoted(a.For.Contact), answer, question)
	}
	return fmt.Sprintf("OWNER DECISION (%s via %s, %s) on %s: %s%s", d.Approver, device, at, a.ID, answer, question)
}

// effect is an immutable server-owned delivery intent. Each sink consumes its
// own ID; completion of a different effect never suppresses a retry.
type effect struct{ Kind, ID, Target, Body, SHA, State, Error string }

// makeEffects is what a's decision delivers. notTheirs, when set, is the
// line saying the answer is not the person's it was for (notTheirsLine).
func (s *Service) makeEffects(ctx context.Context, a orchapi.Ask, d orchapi.Decision, device, notTheirs string) ([]effect, error) {
	ctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	if d.Decision == orchapi.DecisionNeedsContext {
		return s.sentBackEffects(ctx, a, d, device)
	}
	body := decisionText(a, d, device)
	if notTheirs != "" {
		body += "\n" + notTheirs
	}
	body += "\nReceipt: zm ask receipt " + a.ID
	if needsWork(a, d.Decision) {
		body += "\n" + notDoneLine(a)
	}
	if a.Card == nil {
		body = fmt.Sprintf("decision: %s, by approver %s on device %s. Receipt: zm ask receipt %s", d.Decision, d.Approver, quoted(device), a.ID)
		if notTheirs != "" {
			body += "\n" + notTheirs
		}
	}
	list := []effect{{Kind: "reply", Target: a.ID, Body: body}}
	if a.Subject.Work != "" {
		list = append(list, effect{Kind: "comment", Target: a.Subject.Work, Body: body})
		to, _, _, err := orchcore.Work(s.d).MailTarget(ctx, s.d.Projects.Local(""), a.Project, a.Subject.Work)
		// Nobody to mail (no holder, no known creator) costs only the notice;
		// the comment and the asker's reply still carry the answer.
		if err != nil && !api.IsCode(err, api.CodeConflict) {
			return nil, err
		}
		if to != "" && to != a.AskedBy {
			list = append(list, effect{Kind: "holder", Target: to, Body: body})
		}
	}
	ack, err := s.actionEffect(ctx, a, d)
	if err != nil {
		return nil, err
	}
	if ack != nil {
		list = append(list, *ack)
	}
	// Event payload is the decision as committed, never mutable delivery status.
	d.RecordedAt = nil
	d.WriteError = ""
	d.MailError = ""
	// Area operators get the event: they are not shown the asker's human.
	list = append(list, effect{Kind: "event", Target: a.ID, Body: marshal(*hideAskerHuman(a, &d))})
	for i := range list {
		list[i].ID = ids.New()
		list[i].SHA = textSHA(list[i].Kind + "\n" + list[i].Target + "\n" + list[i].Body)
	}
	return list, nil
}

// sentBackEffects is what sending an ask back delivers: the note to the asker
// and, when the asking session is gone, to the work item's holder. Nothing is
// written on the work item and no action runs: nothing was decided.
func (s *Service) sentBackEffects(ctx context.Context, a orchapi.Ask, d orchapi.Decision, device string) ([]effect, error) {
	body := fmt.Sprintf("SENT BACK, NOT DECIDED (%s via %s, %s) on %s: needs more context before deciding.\nTheir note: %s\nNothing was decided and nothing was written on the work item. Ask again with that context in the card.\nReceipt: zm ask receipt %s",
		d.Approver, quoted(device), d.Time.UTC().Format("2006-01-02 15:04 UTC"), a.ID, quoted(d.Text), a.ID)
	list := []effect{{Kind: "reply", Target: a.ID, Body: body}}
	if a.Subject.Work != "" && s.askerGone(ctx, a) {
		to, _, _, err := orchcore.Work(s.d).MailTarget(ctx, s.d.Projects.Local(""), a.Project, a.Subject.Work)
		if err != nil && !api.IsCode(err, api.CodeConflict) {
			return nil, err
		}
		if to != "" && to != a.AskedBy {
			list = append(list, effect{Kind: "holder", Target: to, Body: body})
		}
	}
	d.RecordedAt, d.WriteError, d.MailError = nil, "", ""
	list = append(list, effect{Kind: "event", Target: a.ID, Body: marshal(d)})
	for i := range list {
		list[i].ID = ids.New()
		list[i].SHA = textSHA(list[i].Kind + "\n" + list[i].Target + "\n" + list[i].Body)
	}
	return list, nil
}

// askerGone is true when the ask came from a session that has ended. An
// unknown session or a store that cannot answer counts as not gone (a replica's
// live session may be missing from presence): the asker's reply still goes out,
// and a duplicate to the holder is worse than none.
func (s *Service) askerGone(ctx context.Context, a orchapi.Ask) bool {
	addr, err := api.ParseAddress(a.AskedBy)
	ss := orchcore.Sessions(s.d)
	if err != nil || addr.Kind != api.KindSession || ss == nil {
		return false
	}
	sess, ok, err := orchcore.LookupSession(ctx, ss, a.AskedBy)
	return err == nil && ok && sess.State == orchapi.StateEnded
}

func insertEffects(ctx context.Context, tx *sql.Tx, ask string, list []effect, now time.Time) error {
	for _, e := range list {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_effects (ask,kind,id,target,body,payload_sha) VALUES (?,?,?,?,?,?)`, ask, e.Kind, e.ID, e.Target, e.Body, e.SHA); err != nil {
			return err
		}
	}
	return insertAction(ctx, tx, ask, list, now)
}

// ensureEffects upgrades pre-effects, still-pending rows. Those old rows have
// no reliable delivery evidence: existing comments/mail require reconciliation.
// Fresh decisions always persist their effects in the decision transaction.
func (s *Service) ensureEffects(ctx context.Context, a orchapi.Ask, d orchapi.Decision, device string) error {
	var n int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM decision_effects WHERE ask=?`, a.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	list, err := s.makeEffects(ctx, a, d, device, "")
	if err != nil {
		return err
	}
	return store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error { return insertEffects(ctx, tx, a.ID, list, s.now()) })
}

func (s *Service) deliver(ctx context.Context, a orchapi.Ask, d *orchapi.Decision, device string) {
	// Serialize first delivery with the retry worker. Stable sink IDs also protect
	// retries after death between a sink commit and this database's completion.
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	defer func() {
		if fresh, err := s.ask(context.WithoutCancel(ctx), a.ID); err == nil && fresh.Decision != nil {
			d.Deliveries = fresh.Decision.Deliveries
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	defer cancel()
	if err := s.ensureEffects(ctx, a, *d, device); err != nil {
		s.deliveryFailure(ctx, a, d, err.Error(), err.Error())
		return
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT kind,id,target,body,payload_sha,state,error FROM decision_effects WHERE ask=? ORDER BY kind`, a.ID)
	if err != nil {
		s.deliveryFailure(ctx, a, d, err.Error(), err.Error())
		return
	}
	var list []effect
	for rows.Next() {
		var e effect
		if err = rows.Scan(&e.Kind, &e.ID, &e.Target, &e.Body, &e.SHA, &e.State, &e.Error); err != nil {
			break
		}
		list = append(list, e)
	}
	rows.Close()
	if err != nil {
		s.deliveryFailure(ctx, a, d, err.Error(), err.Error())
		return
	}
	var writeErr, mailErr string
	for _, e := range list {
		if e.State == "succeeded" {
			continue
		}
		if e.State == "unknown" || e.State == "failed" {
			if e.Kind == "reply" || e.Kind == "holder" {
				mailErr = e.Error
			} else {
				writeErr = e.Error
			}
			continue
		}
		var failure error
		if textSHA(e.Kind+"\n"+e.Target+"\n"+e.Body) != e.SHA {
			failure = api.Errorf(api.CodeConflict, "delivery payload changed")
		} else {
			sub := s.d.Projects.Local("")
			if e.Kind == "reply" || e.Kind == "holder" {
				sub = s.d.Projects.Local(decisionSender(a, d))
			}
			switch e.Kind {
			case "comment":
				if testHookWriteBack != nil {
					failure = testHookWriteBack()
				}
				if failure == nil {
					if rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder); ok {
						failure = rec.RecordComment(ctx, sub, a.Project, e.Target, e.ID, e.Body)
					} else {
						failure = api.Errorf(api.CodeUnavailable, "work module unavailable")
					}
				}
			case "reply":
				// The receipt is for an agent that asked; a person sees the
				// decision in Decisions, never a notice from zm.
				_, failure = orchcore.Mail(s.d).Reply(orchcore.AsAskNotice(ctx), sub, a.Project, e.Target, orchapi.ReplyRequest{ID: e.ID, Body: e.Body, Silent: !orchapi.AgentSender(a.AskedBy)})
			case "holder":
				_, failure = orchcore.Mail(s.d).Send(orchcore.AsAskNotice(ctx), sub, api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}, a.Area, orchapi.SendMailRequest{ID: e.ID, To: e.Target, Body: e.Body})
			case "ack":
				failure = s.runAck(ctx, a, e)
			case "action":
				failure = s.runAction(ctx, a, e)
			case "event":
				var data orchapi.Decision
				if failure = json.Unmarshal([]byte(e.Body), &data); failure == nil {
					_, failure = s.d.Events.Publish(ctx, events.Input{DeliveryID: e.ID, Type: orchapi.EventDecisionRecorded, Subject: a.ID, Project: a.Project, Area: a.Area, Data: data, Mentions: []string{a.AskedBy}, Audience: &events.Audience{Need: authz.Operate, Parties: []string{a.AskedBy, a.To}}})
				}
			default:
				failure = api.Errorf(api.CodeInternal, "unknown decision effect")
			}
		}
		state, message := "succeeded", ""
		if failure != nil {
			state, message = "pending", failure.Error()
			if api.IsCode(failure, api.CodeNotFound) || api.IsCode(failure, api.CodeForbidden) || api.IsCode(failure, api.CodeConflict) || api.IsCode(failure, api.CodeBadRequest) {
				state = "failed"
			}
			if api.IsCode(failure, api.CodeNotImplemented) {
				state = "unknown"
			}
			if e.Kind == "reply" || e.Kind == "holder" {
				mailErr = message
			} else {
				writeErr = message
			}
		}
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE decision_effects SET state=?,error=? WHERE ask=? AND kind=?`, state, message, a.ID, e.Kind); err != nil {
			if e.Kind == "reply" || e.Kind == "holder" {
				mailErr = err.Error()
			} else {
				writeErr = err.Error()
			}
		}
	}
	if writeErr != "" || mailErr != "" {
		s.deliveryFailure(ctx, a, d, writeErr, mailErr)
		return
	}
	at := s.now().Truncate(time.Millisecond)
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE decisions SET recorded=?,mailed=?,write_error='',mail_error='',next_try=NULL WHERE ask=?`, ms(at), ms(at), a.ID); err != nil {
		d.WriteError = err.Error()
		return
	}
	d.RecordedAt = &at
	d.WriteError = ""
	d.MailError = ""
}

// decisionSender is who the decision's mail is from: the person the ask
// was put to (human:<name>), else the owner who answered. Never the
// authority machine's own user, whom the asker never asked ("" = that user).
func decisionSender(a orchapi.Ask, d *orchapi.Decision) string {
	if to, err := api.ParseAddress(a.To); err == nil && to.Kind == api.KindHuman {
		return to.String()
	}
	if d.Method == orchapi.MethodOwner && d.Approver != "" {
		if from, err := api.ParseAddress("human:" + d.Approver); err == nil && from.Kind == api.KindHuman {
			return from.String()
		}
	}
	return ""
}

func (s *Service) deliveryFailure(ctx context.Context, a orchapi.Ask, d *orchapi.Decision, writeErr, mailErr string) {
	d.WriteError = writeErr
	d.MailError = mailErr
	d.RecordedAt = nil
	var attempts int
	_ = s.d.Store.Registry().QueryRowContext(ctx, `UPDATE decisions SET attempts=attempts+1 WHERE ask=? RETURNING attempts`, a.ID).Scan(&attempts)
	wait := retryTick << min(attempts, 10)
	if wait > maxRetryWait {
		wait = maxRetryWait
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE decisions SET recorded=NULL,write_error=?,mail_error=?,next_try=? WHERE ask=?`, writeErr, mailErr, nextDelivery(s, ctx, a.ID, wait), a.ID); err != nil {
		s.d.Log.Error("approvals: persist delivery failure", "ask", a.ID, "err", err)
	}
}

func nextDelivery(s *Service, ctx context.Context, ask string, wait time.Duration) any {
	var pending int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM decision_effects WHERE ask=? AND state='pending'`, ask).Scan(&pending); err != nil || pending > 0 {
		return ms(s.now().Add(wait))
	}
	return nil
}

// scanAskDevice is scanAsk with the decision's device name after askCols.
func scanAskDevice(rows *sql.Rows) (orchapi.Ask, string, error) {
	var device string
	a, err := scanAsk(deviceScanner{rows, &device})
	return a, device, err
}

// deviceScanner adds one more destination to scanAsk's.
type deviceScanner struct {
	rows   *sql.Rows
	device *string
}

func (d deviceScanner) Scan(dest ...any) error { return d.rows.Scan(append(dest, d.device)...) }

// Start retries undelivered answers until shutdown.
func (s *Service) Start(ctx context.Context) error {
	if err := s.migrateLegacySlots(ctx); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(retryTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.retryDeliveries(ctx)
				s.applyDefaultsInBackground(ctx)
				s.watchActions(ctx)
				s.watchNotDone(ctx)
				s.rejudgeInBackground(ctx)
				s.sweepKeys(ctx)
			}
		}
	}()
	return nil
}

// retryDeliveries tries every answer whose next try is due.
func (s *Service) retryDeliveries(ctx context.Context) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+`, coalesce(d.device, '')`+askFrom+`
		WHERE d.recorded IS NULL AND d.next_try IS NOT NULL AND d.next_try <= ? ORDER BY d.next_try LIMIT 20`, ms(s.now()))
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("approvals: read undelivered answers", "err", err)
		}
		return
	}
	type due struct {
		a      orchapi.Ask
		device string
	}
	var list []due
	for rows.Next() {
		a, device, err := scanAskDevice(rows)
		if err != nil {
			s.d.Log.Warn("approvals: read an undelivered answer", "err", err)
			continue
		}
		list = append(list, due{a, device})
	}
	rows.Close()
	for _, x := range list {
		if x.a.Decision != nil {
			s.deliver(ctx, x.a, x.a.Decision, x.device)
		}
	}
}
