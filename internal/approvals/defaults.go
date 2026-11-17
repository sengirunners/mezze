package approvals

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Defaults with deadlines (SPEC 16 "Defaults"). An ask made
// with apply_default carries a recommendation and a deadline (by). If nobody
// has answered when by passes, the retry ticker applies the recommendation:
// DEFAULT APPLIED on the work item (never an OWNER DECISION, and no receipt:
// nobody decided), and a reply to the ask's mail that wakes the asker. The
// ask stays open, so the card stays in Decisions: the person may Undo (POST
// /v0/asks/{id}/undo, which tells the asker to put it back) or answer, and
// answering is what ends the card. Only reversible asks qualify: the flag is
// refused when the card or its work item names an irreversible step (the
// irreversible judge).

var defaultsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0007_defaults", Script: `
ALTER TABLE asks ADD COLUMN apply_default INTEGER NOT NULL DEFAULT 0;
ALTER TABLE asks ADD COLUMN default_applied INTEGER;
ALTER TABLE asks ADD COLUMN default_undone INTEGER;
ALTER TABLE asks ADD COLUMN default_undone_by TEXT NOT NULL DEFAULT '';
CREATE INDEX asks_default_due ON asks (by_ms) WHERE apply_default = 1 AND default_applied IS NULL AND withdrawn IS NULL;
CREATE TABLE default_effects (
 ask TEXT NOT NULL REFERENCES asks(id), kind TEXT NOT NULL, id TEXT NOT NULL UNIQUE,
 target TEXT NOT NULL DEFAULT '', sender TEXT NOT NULL DEFAULT '', body TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0, next_try INTEGER,
 PRIMARY KEY(ask,kind)
);
CREATE INDEX default_effects_due ON default_effects (next_try) WHERE state = 'pending';
`},
}

// checkDefault decides whether a may carry apply_default (fillCard, with the
// card cleaned and by set).
func (s *Service) checkDefault(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, req orchapi.NewAskRequest) error {
	refuse := func(why string) error {
		return api.Errorf(api.CodeBadRequest, "apply_default: %s", why).
			WithHint("ask without it: the person answers when they can")
	}
	c := a.Card
	switch {
	case c == nil || c.Recommend == "":
		return refuse("a default needs a card with a recommendation (--recommend)")
	case a.By == nil:
		return refuse("a default needs a deadline (--by)")
	case req.For != nil:
		return refuse("an outside party's ask is answered by its contact, never by a default")
	case c.Action != nil:
		return refuse("an ask that carries an action never runs it by default")
	case c.Verify != nil:
		return refuse("an ask with a verify check is approved by a person: a default would leave its work unchecked")
	}
	if why, err := s.irreversible(ctx, sub, *a); err != nil || why != "" {
		if err != nil {
			return err
		}
		return refuse(why)
	}
	return nil
}

// irreversible says why a's card or work item, as sub reads it now, names an
// irreversible step ("" when neither does). It runs when the ask is made and
// again at its deadline, since the item may have changed in between. A
// label is read as it is; what the text means is the irreversible judge's
// question (irreversible.md: never a word list). A judge that
// cannot answer is an unavailable error: the flag is refused at the ask,
// and at the deadline the default waits for the next tick, never applied
// unchecked.
func (s *Service) irreversible(ctx context.Context, sub *authz.Subject, a orchapi.Ask) (string, error) {
	in := irreversibleInput{Card: a.Card}
	if a.Subject.Work != "" {
		wd, ok := orchcore.Work(s.d).(orchcore.WorkDescriber)
		if !ok {
			return "", api.Errorf(api.CodeUnavailable, "the work module is not running")
		}
		title, body, labels, err := wd.ItemText(ctx, sub, a.Project, a.Subject.Work)
		if err != nil {
			return "", err
		}
		for _, l := range labels {
			if strings.EqualFold(l, "irreversible") {
				return "the work item is labelled irreversible", nil
			}
		}
		in.Work = &irreversibleWork{Title: title, Body: body}
	}
	v := irreversibleVerdict(ctx, s.caller(), in, IrreversibleWait)
	switch v.Status {
	case judge.Pass:
		return "", nil
	case judge.Fail:
		why := strings.TrimSuffix(v.Reason, ".")
		if why == "" {
			why = strings.TrimSuffix(strings.Join(v.Fixes, "; "), ".")
		}
		return "it names a step that cannot be undone (" + why + "): a production write, a publish, an outbound message, money, access or a secret waits for a person", nil
	default:
		return "", api.Errorf(api.CodeUnavailable, "apply_default: the judge that reads an ask for a step that cannot be undone could not answer (%s); a default is never set unchecked", v.Reason).
			WithHint("send it again in a minute, or ask without it: the person answers when they can")
	}
}

//go:embed irreversible.md
var irreversiblePrompt string

const irreversibleSchema = `{"type":"object","properties":{
"pass":{"type":"boolean"},"fixes":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},
"required":["pass","fixes","reason"],"additionalProperties":false}`

// IrreversibleWait bounds one irreversible judging (tests change it).
var IrreversibleWait = 40 * time.Second

type irreversibleInput struct {
	Card *orchapi.AskCard  `json:"card"`
	Work *irreversibleWork `json:"work,omitempty"`
}

type irreversibleWork struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// irreversibleVerdict is the irreversible judge's verdict on in.
func irreversibleVerdict(ctx context.Context, c judge.Caller, in irreversibleInput, wait time.Duration) judge.Verdict {
	j := judge.Judge{Name: "irreversible judge", Prompt: irreversiblePrompt, Schema: json.RawMessage(irreversibleSchema), Timeout: wait}
	return j.Run(ctx, c, in)
}

// defaultEffect is one delivery of a default's record, retried from its row.
type defaultEffect struct{ Kind, ID, Target, Sender, Body string }

func insertDefaultEffects(ctx context.Context, tx *sql.Tx, ask string, list []defaultEffect) error {
	for _, e := range list {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO default_effects (ask,kind,id,target,sender,body) VALUES (?,?,?,?,?,?)`,
			ask, e.Kind, ids.New(), e.Target, e.Sender, e.Body); err != nil {
			return err
		}
	}
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func questionLine(a orchapi.Ask) string {
	if a.Card == nil {
		return ""
	}
	return "\nQuestion (asker supplied): " + quoted(a.Card.Question)
}

// appliedText is DEFAULT APPLIED as the work item and the asker read it.
func appliedText(a orchapi.Ask) string {
	return fmt.Sprintf("DEFAULT APPLIED (no answer by %s) on %s: %s. Not an owner decision: nobody answered in time, so zm applied the asker's recommendation. Proceed with it; the owner may still undo it or answer (zm ask show %s).%s",
		stamp(*a.By), a.ID, quoted(choiceAnswer(a.Card, a.Card.Recommend, "")), a.ID, questionLine(a))
}

// undoneText is DEFAULT UNDONE: the asker puts back what it did.
func undoneText(a orchapi.Ask, by string, at time.Time) string {
	return fmt.Sprintf("DEFAULT UNDONE (%s, %s) on %s: stop %s and put back what you changed for it; wait for the owner's answer (zm ask show %s).%s",
		by, stamp(at), a.ID, quoted(choiceAnswer(a.Card, a.Card.Recommend, "")), a.ID, questionLine(a))
}

// defaultNote is what a decision on an ask whose default applied adds, so
// the asker knows whether to keep or reverse it.
func defaultNote(a orchapi.Ask, decision string) string {
	if a.DefaultApplied == nil || a.DefaultUndone != nil || a.Card == nil {
		return ""
	}
	if decision == a.Card.Recommend {
		return "\nThis keeps the default applied at " + stamp(*a.DefaultApplied) + "."
	}
	return "\nThis replaces the default applied at " + stamp(*a.DefaultApplied) + ": put back what you changed for it."
}

// withDefaultNote adds defaultNote to the messages a decision sends (record,
// inside its transaction, so a default applied a moment before is named).
func withDefaultNote(list []effect, a orchapi.Ask, decision string) []effect {
	note := defaultNote(a, decision)
	if note == "" {
		return list
	}
	out := make([]effect, len(list))
	for i, e := range list {
		if e.Kind == "reply" || e.Kind == "comment" || e.Kind == "holder" {
			e.Body = strings.Replace(e.Body, "\nReceipt: zm ask receipt ", note+"\nReceipt: zm ask receipt ", 1)
			e.SHA = textSHA(e.Kind + "\n" + e.Target + "\n" + e.Body)
		}
		out[i] = e
	}
	return out
}

// applyDefaultsInBackground starts one sweep of due defaults unless one is
// running: each asks the irreversible judge (up to IrreversibleWait), and a
// hung provider CLI must not stall the retry ticker's answer deliveries,
// actions or held cards behind it. A default still waits
// for a pass; it only waits off the ticker.
func (s *Service) applyDefaultsInBackground(ctx context.Context) {
	if !s.defaulting.CompareAndSwap(false, true) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.defaulting.Store(false)
		s.applyDefaults(ctx)
	}()
}

// applyDefaults applies every due default (one background sweep), then retries
// any default record still to deliver.
func (s *Service) applyDefaults(ctx context.Context) {
	now := ms(s.now())
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+`
		WHERE a.apply_default = 1 AND a.default_applied IS NULL AND a.withdrawn IS NULL AND d.ask IS NULL AND a.by_ms <= ?`+notHeld+notReleased+`
		ORDER BY a.by_ms LIMIT 20`, now)
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("approvals: read due defaults", "err", err)
		}
		return
	}
	var due []orchapi.Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			s.d.Log.Warn("approvals: read a due default", "err", err)
			continue
		}
		due = append(due, a)
	}
	rows.Close()
	for _, a := range due {
		if err := s.applyDefault(ctx, a, now); err != nil {
			s.d.Log.Warn("approvals: apply a default", "ask", a.ID, "err", err)
		}
	}
	s.retryDefaultEffects(ctx)
}

// applyDefault records a's default unless someone answered, withdrew or
// applied it first: the conditional update and the decision's insert are
// both writes on the registry, so exactly one wins.
func (s *Service) applyDefault(ctx context.Context, a orchapi.Ask, now int64) error {
	if a.Card == nil || a.By == nil {
		return nil
	}
	why, err := s.irreversible(ctx, s.d.Projects.Local(""), a)
	if err != nil && !api.IsCode(err, api.CodeNotFound) {
		return err // try again next tick
	}
	if err != nil || why != "" {
		if err != nil {
			why = "its work item is gone"
		}
		return s.dropDefault(ctx, a, why)
	}
	at := store.FromMillis(now)
	a.DefaultApplied = &at
	body := appliedText(a)
	list := []defaultEffect{{Kind: "applied-reply", Target: a.ID, Body: body}, {Kind: "applied-event", Target: a.ID}}
	if a.Subject.Work != "" {
		list = append(list, defaultEffect{Kind: "applied-comment", Target: a.Subject.Work, Body: body})
	}
	won := false
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE asks SET default_applied = ? WHERE id = ? AND apply_default = 1 AND default_applied IS NULL
			AND withdrawn IS NULL AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.ask = asks.id)`, now, a.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		won = true
		return insertDefaultEffects(ctx, tx, a.ID, list)
	})
	if err != nil || !won {
		return err
	}
	s.deliverDefault(ctx, a.ID)
	return nil
}

// dropDefault turns a's default off at its deadline (it now names an
// irreversible step) and tells the asker it waits for the owner.
func (s *Service) dropDefault(ctx context.Context, a orchapi.Ask, why string) error {
	body := fmt.Sprintf("no default on %s: %s, so zm did not apply it. It waits for the owner's answer (zm ask show %s).%s", a.ID, why, a.ID, questionLine(a))
	won := false
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE asks SET apply_default = 0 WHERE id = ? AND default_applied IS NULL`, a.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		won = true
		return insertDefaultEffects(ctx, tx, a.ID, []defaultEffect{{Kind: "dropped-mail", Target: a.AskedBy, Body: body}})
	})
	if err != nil || !won {
		return err
	}
	s.deliverDefault(ctx, a.ID)
	return nil
}

// handleUndo is POST /v0/asks/{id}/undo: the project's owner takes back a
// default zm applied. The asker is told to put it back; the ask stays open
// for the owner's own answer. Undoing again is the same answer.
func (s *Service) handleUndo(w http.ResponseWriter, r *http.Request) {
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
	me, err := s.ownerOf(ctx, sub, a.Project)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	a, err = s.undo(ctx, a, me.Name)
	respond(w, s.view(ctx, sub, a), err)
}

// handleDeviceUndo is POST /v0/asks/{id}/device-undo: an active approver's
// device undoes a default, as decide answers. The approver check comes
// first, so another device learns nothing about which asks exist.
func (s *Service) handleDeviceUndo(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	me, err := s.activeApprover(r.Context(), rm)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.ask(r.Context(), r.PathValue("id"))
	if err == nil {
		a, err = s.undo(r.Context(), a, me.Name)
	}
	respond(w, a, err)
}

// undo takes back a's applied default for the person named by.
func (s *Service) undo(ctx context.Context, a orchapi.Ask, by string) (orchapi.Ask, error) {
	switch {
	case a.Decision != nil:
		return a, api.Errorf(api.CodeConflict, "ask %s was already answered: %s", a.ID, first(a.Decision.Answer, a.Decision.Decision)).
			WithHint("the answer stands; an answer other than the default already told the asker to put it back")
	case a.DefaultUndone != nil:
		return a, nil
	case a.Withdrawn != nil:
		return a, api.Errorf(api.CodeConflict, "ask %s was withdrawn", a.ID)
	case a.DefaultApplied == nil:
		return a, api.Errorf(api.CodeConflict, "ask %s has no applied default to undo", a.ID).
			WithHint("answer it instead: zm ask answer " + a.ID + " <choice>")
	}
	at := s.now().Truncate(time.Millisecond)
	body := undoneText(a, by, at)
	sender := ""
	if from, err := api.ParseAddress("human:" + by); err == nil && from.Kind == api.KindHuman {
		sender = from.String()
	}
	list := []defaultEffect{{Kind: "undone-mail", Target: a.AskedBy, Sender: sender, Body: body}, {Kind: "undone-event", Target: a.ID}}
	if a.Subject.Work != "" {
		list = append(list, defaultEffect{Kind: "undone-comment", Target: a.Subject.Work, Body: body})
	}
	won := false
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE asks SET default_undone = ?, default_undone_by = ? WHERE id = ? AND default_applied IS NOT NULL
			AND default_undone IS NULL AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.ask = asks.id)`, ms(at), by, a.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		won = true
		return insertDefaultEffects(ctx, tx, a.ID, list)
	})
	if err != nil {
		return a, err
	}
	if won {
		s.deliverDefault(ctx, a.ID)
	}
	a, err = s.ask(ctx, a.ID)
	if err == nil && a.DefaultUndone == nil {
		err = api.Errorf(api.CodeConflict, "ask %s was answered first", a.ID)
	}
	return a, err
}

// retryDefaultEffects retries default records whose next try is due.
func (s *Service) retryDefaultEffects(ctx context.Context) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT DISTINCT ask FROM default_effects WHERE state = 'pending' AND (next_try IS NULL OR next_try <= ?) LIMIT 20`, ms(s.now()))
	if err != nil {
		return
	}
	var asks []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			asks = append(asks, id)
		}
	}
	rows.Close()
	for _, id := range asks {
		s.deliverDefault(ctx, id)
	}
}

// deliverDefault runs ask's pending default records. Each sink takes the
// row's id as its write id, so a retry after a crash adds nothing twice.
func (s *Service) deliverDefault(ctx context.Context, id string) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	defer cancel()
	a, err := s.ask(ctx, id)
	if err != nil {
		return
	}
	reg := s.d.Store.Registry()
	rows, err := reg.QueryContext(ctx, `SELECT kind,id,target,sender,body FROM default_effects WHERE ask=? AND state='pending' ORDER BY kind`, id)
	if err != nil {
		return
	}
	var list []defaultEffect
	for rows.Next() {
		var e defaultEffect
		if rows.Scan(&e.Kind, &e.ID, &e.Target, &e.Sender, &e.Body) == nil {
			list = append(list, e)
		}
	}
	rows.Close()
	ref := api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}
	audience := &events.Audience{Need: authz.Operate, Parties: []string{a.AskedBy, a.To}}
	for _, e := range list {
		if (e.Kind == "applied-reply" || e.Kind == "applied-comment") && (a.Decision != nil || a.DefaultUndone != nil) {
			// An answer or an undo came first: telling the asker to proceed now
			// would be the wrong last word.
			_, _ = reg.ExecContext(ctx, `UPDATE default_effects SET state='superseded',next_try=NULL WHERE ask=? AND kind=?`, id, e.Kind)
			continue
		}
		if (e.Kind == "undone-mail" || e.Kind == "dropped-mail") && !orchapi.AgentSender(a.AskedBy) {
			// A person gets no notice from zm (SPEC 9 "No notice about what you
			// just did"): they see the ask in Decisions.
			_, _ = reg.ExecContext(ctx, `UPDATE default_effects SET state='superseded',next_try=NULL WHERE ask=? AND kind=?`, id, e.Kind)
			continue
		}
		var failure error
		switch e.Kind {
		case "applied-reply":
			// Answering the ask's mail wakes the asker and ends its needs_human;
			// a person who asked is told nothing, the mail is only taken.
			_, failure = orchcore.Mail(s.d).Reply(orchcore.AsAskNotice(ctx), s.d.Projects.Local(""), a.Project, e.Target,
				orchapi.ReplyRequest{ID: e.ID, Body: e.Body, Silent: !orchapi.AgentSender(a.AskedBy)})
		case "undone-mail", "dropped-mail":
			_, failure = orchcore.Mail(s.d).Send(orchcore.AsAskNotice(ctx), s.d.Projects.Local(e.Sender), ref, a.Area, orchapi.SendMailRequest{ID: e.ID, To: e.Target, Body: e.Body})
		case "applied-comment", "undone-comment":
			if rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder); ok {
				failure = rec.RecordComment(ctx, s.d.Projects.Local(""), a.Project, e.Target, e.ID, e.Body)
			} else {
				failure = api.Errorf(api.CodeUnavailable, "work module unavailable")
			}
		case "applied-event", "undone-event":
			typ := orchapi.EventDefaultApplied
			if e.Kind == "undone-event" {
				typ = orchapi.EventDefaultUndone
			}
			_, failure = s.d.Events.Publish(ctx, events.Input{DeliveryID: e.ID, Type: typ, Subject: a.ID, Project: a.Project, Area: a.Area,
				Data: a, Mentions: []string{a.AskedBy}, Audience: audience})
		default:
			failure = api.Errorf(api.CodeInternal, "unknown default record")
		}
		if failure == nil {
			_, _ = reg.ExecContext(ctx, `UPDATE default_effects SET state='succeeded',error='',next_try=NULL WHERE ask=? AND kind=?`, id, e.Kind)
			continue
		}
		state := "pending"
		if api.IsCode(failure, api.CodeNotFound) || api.IsCode(failure, api.CodeForbidden) || api.IsCode(failure, api.CodeConflict) || api.IsCode(failure, api.CodeBadRequest) {
			state = "failed"
		}
		var attempts int
		_ = reg.QueryRowContext(ctx, `UPDATE default_effects SET attempts=attempts+1 WHERE ask=? AND kind=? RETURNING attempts`, id, e.Kind).Scan(&attempts)
		wait := min(retryTick<<min(attempts, 10), maxRetryWait)
		_, _ = reg.ExecContext(ctx, `UPDATE default_effects SET state=?,error=?,next_try=? WHERE ask=? AND kind=?`, state, failure.Error(), ms(s.now().Add(wait)), id, e.Kind)
		s.d.Log.Warn("approvals: deliver a default record", "ask", id, "kind", e.Kind, "err", failure)
	}
}
