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

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

const askCols = `a.id, a.project, a.area, a.repo, a.pr, a.head_sha, a.options, a.body, a.asked_by, a.to_addr, a.created,
	coalesce(d.decision, ''), coalesce(d.approver, ''), coalesce(d.time, 0), coalesce(d.receipt, ''),
	a.work, a.question, a.card, a.card_sha, a.priority, a.topic, a.for_group, a.by_ms, a.withdrawn,
	coalesce(d.text, ''), coalesce(d.answered_as, ''), d.recorded, coalesce(d.write_error, ''), coalesce(d.mail_error, ''), coalesce(d.method, ''), coalesce(d.device, ''), (SELECT coalesce(json_group_array(json_object('kind',e.kind,'state',e.state)), '[]') FROM decision_effects e WHERE e.ask=a.id),
	a.owner, a.same_as, a.apply_default, a.default_applied, a.default_undone, a.default_undone_by,
	coalesce(x.state, ''), coalesce(x.reason, ''), coalesce(x.task, ''), coalesce(x.confirmed_by, ''), x.started, x.ended, x.confirmed,
	x.checked, x.check_exit, coalesce(x.check_output, ''), x.verified, x.reminded, x.escalated, coalesce(x.checked_by, ''), x.relaned, coalesce(x.relane_task, ''), coalesce(x.relane_machine, ''), coalesce(x.relane_reason, ''), coalesce(x.done_evidence, ''), coalesce(x.retries, 0), coalesce(x.retryable, 0), x.step_done, coalesce(x.step_done_by, ''),
	a.attachments, a.owner_person, a.asker_human, a.work_title, a.thread_title, a.pr_url,
	coalesce((SELECT l.other FROM ask_links l WHERE l.ask = a.id), ''), ` + answeredFromCols

const askFrom = ` FROM asks a LEFT JOIN decisions d ON d.ask = a.id LEFT JOIN ask_actions x ON x.ask = a.id`

func scanAsk(sc interface{ Scan(...any) error }) (orchapi.Ask, error) {
	var a orchapi.Ask
	var opts, dec, approver, receipt, card, forGroup, text, as, writeErr string
	var mailErr, deliveries, method, device string
	var created, at int64
	var by, withdrawn, recorded, applied, undone sql.NullInt64
	var xState, xReason, xTask, xConfirmedBy string
	var xStarted, xEnded, xConfirmed sql.NullInt64
	var attached, from string
	var xc actionCheck
	if err := sc.Scan(&a.ID, &a.Project, &a.Area, &a.Subject.Repo, &a.Subject.PR, &a.Subject.HeadSHA, &opts, &a.Body,
		&a.AskedBy, &a.To, &created, &dec, &approver, &at, &receipt,
		&a.Subject.Work, &a.Subject.Question, &card, &a.CardSHA, &a.Priority, &a.Topic, &forGroup, &by, &withdrawn,
		&text, &as, &recorded, &writeErr, &mailErr, &method, &device, &deliveries, &a.Owner, &a.SameAs,
		&a.ApplyDefault, &applied, &undone, &a.DefaultUndoneBy,
		&xState, &xReason, &xTask, &xConfirmedBy, &xStarted, &xEnded, &xConfirmed,
		&xc.checked, &xc.exit, &xc.output, &xc.verified, &xc.reminded, &xc.escalated, &xc.by, &xc.relaned, &xc.relaneTask, &xc.relaneMachine, &xc.relaneReason, &xc.evidence, &xc.retries, &xc.retryable, &xc.stepDone, &xc.stepDoneBy, &attached, &a.OwnerPerson, &a.AskerHuman, &a.WorkTitle, &a.ThreadTitle, &a.PRURL, &a.Linked, &from); err != nil {
		return a, err
	}
	scanActionState(&a, xState, xReason, xTask, xConfirmedBy, xStarted, xEnded, xConfirmed, xc)
	a.Created = store.FromMillis(created)
	a.By, a.Withdrawn = store.NullMillis(by), store.NullMillis(withdrawn)
	a.DefaultApplied, a.DefaultUndone = store.NullMillis(applied), store.NullMillis(undone)
	_ = json.Unmarshal([]byte(opts), &a.Options)
	if attached != "" {
		_ = json.Unmarshal([]byte(attached), &a.Attachments)
	}
	if card != "" {
		a.Card = &orchapi.AskCard{}
		_ = json.Unmarshal([]byte(card), a.Card)
	}
	if forGroup != "" {
		a.For = &orchapi.AskFor{}
		_ = json.Unmarshal([]byte(forGroup), a.For)
	}
	a.Status = orchapi.AskOpen
	if dec != "" {
		d := &orchapi.Decision{Ask: a.ID, Decision: dec, Approver: approver, Time: store.FromMillis(at), Text: text, AnsweredAs: as,
			RecordedAt: store.NullMillis(recorded), WriteError: writeErr, MailError: mailErr, Method: method}
		answeredWhere(d, device, from)
		_ = json.Unmarshal([]byte(receipt), &d.Receipt)
		_ = json.Unmarshal([]byte(deliveries), &d.Deliveries)
		if a.Card != nil {
			d.Answer = choiceAnswer(a.Card, dec, text)
		}
		a.Decision = d
		retryState(&a, xc)
		a.Status = orchapi.AskAnswered
		if d.RecordedAt != nil {
			a.Status = orchapi.AskRecorded
		}
	} else if a.Withdrawn != nil {
		a.Status = orchapi.AskWithdrawn
	}
	return a, nil
}

// mayRead is the restricted-mail rule (SPEC section 9) for asks: readers of
// the area who may operate there, or the ask's parties (who asked, and the
// human it went to). Anyone else gets exactly not_found.
func mayRead(sc authz.Scope, me string, a orchapi.Ask) bool {
	o := authz.Object{Project: a.Project, Area: a.Area}
	return sc.Can(authz.Read, o) && (sc.Can(authz.Operate, o) || (me != "" && (a.AskedBy == me || a.To == me)))
}

// view redacts an ask for sub (the asker may be a session it cannot see).
func (s *Service) view(ctx context.Context, sub *authz.Subject, a orchapi.Ask) orchapi.Ask {
	if pub, err := s.PublicKey(); err == nil {
		a.ReceiptKey = KeyID(pub)
	}
	if sub == nil || !sub.Scope.WholeProject(a.Project) && a.AskedBy != sub.Address {
		a.Decision = hideAskerHuman(a, a.Decision)
		a.OwnerPerson, a.AskerHuman = "", "" // as askOwner: an area member learns no one's account
	} else if a.Decision != nil {
		d := *a.Decision
		d.ApproverPerson = s.approverPerson(ctx, a)
		a.Decision = &d
	}
	if sub == nil || !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
		a.Linked = "" // a party who does not operate here may not read the other ask
	}
	a.AskedBy = s.d.Redact(ctx, sub, a.AskedBy)
	s.markChecked(ctx, &a)
	a.Attachments = s.liveAttachments(ctx, a)
	a.Repeat = s.repeatOf(ctx, a.ID)
	if a.Card != nil && (a.Card.Action != nil || a.Card.Verify != nil) {
		a.ActionText = actionText(a.Card.Action)
		if v := verifyText(a); v != "" {
			a.ActionText = strings.TrimSpace(a.ActionText + " " + v)
		}
		if st := stepOf(a); st != nil {
			a.ActionText = strings.TrimSpace(a.ActionText + " " + stepText(st, a.Card.Verify))
		}
		a.ActionStateText = actionStateText(a)
	}
	a.Objection = s.objectionOf(ctx, a) // every read: it settles a ran-unchecked action
	outcomeBits(&a)
	return a
}

// ---- asks ----

func normOptions(opts []string) ([]string, error) {
	if len(opts) == 0 {
		return []string{orchapi.DecisionApprove, orchapi.DecisionDecline}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range opts {
		if !api.ValidName(o) || len(o) > 32 {
			return nil, api.Errorf(api.CodeBadRequest, "option %q must be a short lowercase word", o)
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) < 2 || len(out) > 4 {
		return nil, api.Errorf(api.CodeBadRequest, "an ask offers 2 to 4 options")
	}
	return out, nil
}

func (s *Service) handleAsk(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	var req orchapi.NewAskRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	req.Body = textclean.Clean(req.Body)
	s.serveAsk(w, ctx, req, "")
}

// serveAsk makes the ask req. joinNote, when set, is why zm made it a join
// (the card judge found the card repeats an open one): the reply's note.
func (s *Service) serveAsk(w http.ResponseWriter, ctx context.Context, req orchapi.NewAskRequest, joinNote string) {
	orig := req // as asked, for a join the card judge turns it into
	if len(req.Attachments) > 0 && (req.Subject.Work == "" || req.Card == nil) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "attachments go on a work item's card: add work (zm ask new --work <id> --attach <file>)"))
		return
	}
	var joined *orchapi.Ask
	if req.SameAs != "" {
		p, err := s.joinTarget(ctx, core.SubjectOf(ctx), &req)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		joined = &p
	}
	if req.ApplyDefault && req.Card == nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "apply_default: a default needs a card with a recommendation and a deadline"))
		return
	}
	if req.Owner != "" && req.Card == nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "owner is for a note card's ask"))
		return
	}
	if (req.ThreadTitle != "" || req.PRURL != "") && req.Card == nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "thread_title and pr_url are for a note card's ask"))
		return
	}
	if (req.RepeatKey != "" || req.Latest != "") && (req.Card == nil || req.RepeatKey == "" || joined != nil) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "repeat_key (and latest with it) is for a note card's own ask, not a join"))
		return
	}
	threadTitle, prURL, err := cleanOrigin(req.ThreadTitle, req.PRURL)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if req.Subject.Question != "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "subject.question is the daemon's: send the card and no subject"))
		return
	}
	isPR := req.Subject.Repo != "" || req.Subject.PR != 0 || req.Subject.HeadSHA != ""
	if req.Card == nil && !isPR && req.Subject.Work == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "repo \"\" is not owner/name or host/owner/name").
			WithHint("an ask is about a pull request (repo, pr, head_sha), a work item or a plain question; the last two need a card"))
		return
	}
	if isPR && req.Subject.Work != "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "an ask is about a pull request or a work item, not both"))
		return
	}
	if isPR && req.Card != nil {
		// A merge gate trusts this receipt: the device must show the pull
		// request and the word approve, not plain words that hide them.
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a pull request's ask takes no card: its subject and approve or decline are what a person decides"))
		return
	}
	if req.Subject.Work != "" && req.Card == nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "an ask about a work item needs a note card"))
		return
	}
	subj := orchapi.AskSubject{Work: strings.TrimSpace(req.Subject.Work)}
	var opts []string
	if isPR {
		if subj, err = CanonSubject(req.Subject); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	if req.Card == nil {
		if opts, err = normOptions(req.Options); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	to := strings.TrimSpace(req.To)
	me := false
	switch low := strings.ToLower(to); {
	case low == "":
		to = "owners"
	case low == "me":
		// The asking session's person (meOwner), resolved once the project
		// is known; with no card it is refused below like any person.
		if req.Card != nil {
			if req.Owner != "" {
				api.WriteError(w, api.Errorf(api.CodeBadRequest, "to me and owner %q: name the person once", req.Owner))
				return
			}
			me, to = true, "owners"
		}
	case !strings.ContainsAny(low, ":/"):
		if low != "owners" && low != "all" {
			to = "human:" + strings.TrimPrefix(low, "@") // a bare name, alias or GitHub login: resolvePerson reads it
		}
	}
	// The decision answers the ask's mail as the human it was put to, so an
	// ask goes only where that answer reaches the asker: owners, or the
	// human of the asker's machine (from a member machine, its own human,
	// never the authority's OS user). `to` routes the mail only: every
	// active approver may decide any ask.
	// A card names its person, including the human of the asker's own
	// machine, and its mail goes to that person alone (an ask directed at one person must not reach everyone);
	// the answer still reaches the asker, sent as that person
	// (decisionSender). `to` and owner that name two people are refused.
	human, machine := s.askerHuman(core.SubjectOf(ctx))
	ad, err := api.ParseAddress(to)
	ownHuman, alsoOwner := "", ""
	switch {
	case err == nil && ad.Kind == api.KindHuman && req.Card != nil:
		if ad.String() == human {
			ownHuman = ad.Human
		}
		if req.Owner != "" && !strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(req.Owner), "human:"), ad.Human) {
			alsoOwner = req.Owner // two spellings: one person's two names pass (checked once the project is known)
		}
		to, req.Owner = "owners", ad.String()
	case err == nil && (ad.Kind == api.KindOwners || ad.String() == human):
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "an ask goes to owners, %s (the human of %s, where you asked) or, with a note card, me or any person who owns or administers the project by a name, alias or GitHub login (zm people lists them), not %q",
			human, machine, req.To))
		return
	}
	t, err := s.d.WriteTarget(ctx, req.Area, authz.Write)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if alsoOwner != "" && !s.samePerson(ctx, t.Project.ID, req.Owner, alsoOwner) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "to %s and owner %q name different people", req.Owner, alsoOwner))
		return
	}
	sub := core.SubjectOf(ctx)
	meVia := ""
	if me {
		req.Owner, meVia = s.meOwner(ctx, sub, t.Project.ID)
	}
	// The ask row goes in before its mail exists, under the mail's id: the
	// mail module refuses ack and reply on an undecided ask's mail
	// (OpenAsk), and sending first left a window where a human caller could
	// take the mail with nothing decided. A failed insert sends nothing; a
	// failed send deletes the row.
	a := orchapi.Ask{ID: ids.New(), Project: t.Project.ID, Area: t.Area, Subject: subj, Options: opts, Body: req.Body,
		AskedBy: sub.Address, AskerHuman: askerName(sub, human), ThreadTitle: threadTitle, PRURL: prURL, To: to, Created: s.now().Truncate(time.Millisecond), Priority: 2, Status: orchapi.AskOpen}
	var card, forJSON, dedupe string
	if req.Card != nil {
		if card, forJSON, dedupe, err = s.fillCard(ctx, sub, &a, req, t.Project.Slug, req.Area != ""); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	if a.Attachments, err = s.askAttachments(ctx, a.Project, a.Subject.Work, req.Attachments); err != nil {
		api.WriteError(w, err)
		return
	}
	var attached string
	if len(a.Attachments) > 0 {
		attached = marshal(a.Attachments)
	}
	if joined != nil {
		if joined.Project != a.Project {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "same_as names an ask of another project"))
			return
		}
		a.SameAs, a.Owner, a.OwnerPerson = joined.ID, joined.Owner, joined.OwnerPerson
		if joined.To == human { // the first went by mail to this machine's own human: so does this one
			to, a.To = human, human
		}
	} else if ownHuman != "" && !s.answers(ctx, a.Project, ownHuman) {
		// The human of a member machine who neither owns nor administers
		// the project still takes its own machine's asks: the mail
		// goes to them, as before, and the card names them.
		to = "human:" + ownHuman
		a.To, a.Owner = to, strings.ToLower(ownHuman)
		a.OwnerPerson = s.personOf(ctx, a.Project, ownHuman)
	} else if req.Card != nil {
		if me && a.Project != t.Project.ID { // the work item's own project: me answers there
			req.Owner, meVia = s.meOwner(ctx, sub, a.Project)
		}
		if a.Owner, a.OwnerPerson, err = s.askOwner(ctx, sub, a.Project, a.Area, a.Subject.Work, req.Owner); err != nil && ownHuman == "" {
			api.WriteError(w, err)
			return
		} else if err != nil {
			// A caller who may not name a person (an area guest) still asks
			// its own machine's human, by the route it had before.
			to = "human:" + ownHuman
			a.To, a.Owner, a.OwnerPerson = to, "", ""
		} else if a.Owner != "" {
			// The person the card names (or its work item's owner:<name>)
			// gets the mail, not every owner of the project.
			to = "human:" + a.Owner
			a.To = to
		}
	}
	if err := s.addressToMachinePerson(ctx, &a, &to); err != nil {
		api.WriteError(w, err)
		return
	}
	if a.Card != nil && a.Card.OwnerStep != nil && a.Owner == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a card with a person's step is for one person: name them (--to human:<name>; zm people lists them)"))
		return
	}
	if joined != nil {
		meVia = "" // a join takes the first ask's owner
	} else if me {
		switch {
		case meVia != "":
		case a.Owner != "":
			meVia = "me: no session that started yours was launched by a person who answers this project; the work item's owner: label names " + a.Owner
		default:
			meVia = "me: no session that started yours was launched by a person who answers this project, and no owner: label names one; the project's owners see it (to: <name> names one; zm people lists them)"
		}
	}
	var repeatKey, latest, repeatScoped, routedNote string
	if req.RepeatKey != "" {
		if repeatKey, latest, err = cleanRepeat(req.RepeatKey, req.Latest, []string{t.Project.Slug, s.d.Projects.Slug(a.Project)}); err != nil {
			api.WriteError(w, err)
			return
		}
		repeatScoped = repeatScope(sub, a.Project, a.Area, repeatKey)
		defer s.lockRepeat(a.Project, repeatScoped)()
		open, counted, err := s.countRepeat(ctx, sub.Address, a.Project, repeatScoped, latest)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		if counted { // the one open card for this key: nothing new is filed
			v := s.view(ctx, sub, open)
			v.Updated = true
			v.Note = fmt.Sprintf("not filed: open ask %s is about the same thing (repeat key %s); it counted this repeat", open.ID, repeatKey)
			api.WriteJSON(w, http.StatusOK, v)
			return
		}
	}
	if dedupe != "" {
		if err := s.refuseRepeat(ctx, sub, a.Project, dedupe); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	// The card judge reads every new card (a join shares one it passed):
	// a repeat of an open card joins it, or is linked to it (repeats.go);
	// a fail is refused with its fixes; no answer stores the ask held.
	held, linked := "", ""
	operator := sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area})
	if req.Card != nil && joined == nil {
		judged := time.Now()
		v := s.judgeCard(ctx, a, CardJudgeWait, operator)
		card = s.raisedCard(&a, v.Risk, card) // before a join compares actions: a raise is never lost
		if v.RepeatOf != "" {
			root, err := s.ask(ctx, v.RepeatOf)
			if err == nil && joinsRepeat(a, root, v) && !s.isHeld(ctx, root.ID) {
				// Asked again as a join of root, for the same person
				// (joinTarget copies root's owner, which is a's).
				j := orig
				j.SameAs, j.Card, j.Owner, j.To, j.Priority, j.By, j.Topic, j.ApplyDefault, j.Options = root.ID, nil, "", "", nil, nil, "", false, nil
				j.RepeatKey, j.Latest = "", "" // this request holds that key's lock (lockRepeat)
				s.serveAsk(w, ctx, j, joinedNote(root.ID))
				return
			}
			if err == nil {
				linked = root.ID
			}
		}
		switch v.Status {
		case judge.Pass:
			// The card router reads a passed card in what is left of the
			// wait (cardroute.go); no time left, it stays with the owners.
			if route := s.routeOutside(ctx, sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}), a, CardJudgeWait-time.Since(judged)); route != nil {
				a.For, forJSON = route, marshal(*route)
				a.Body = renderCard(*a.Card, a.For)
				routedNote = heldForNote(*route)
			}
		case judge.Fail:
			api.WriteError(w, s.cardRefused(ctx, sub, &a, req.Owner, v.Verdict))
			return
		default:
			held = v.Reason
			if err := s.holdAsk(ctx, a.ID, held, operator); err != nil {
				api.WriteError(w, err)
				return
			}
		}
	}
	if joined != nil {
		if testHookBeforeJoin != nil {
			testHookBeforeJoin(joined.ID)
		}
		s.groupMu.Lock()
		cur, err := s.ask(ctx, joined.ID)
		if err == nil && cur.Decision != nil && cur.Decision.Decision == orchapi.DecisionNeedsContext {
			err = api.Errorf(api.CodeConflict, "ask %s was sent back for more context: %s", cur.ID, cur.Decision.Text).WithHint("ask again with your own card, with that context in it")
		} else if err == nil && cur.Decision != nil {
			err = api.Errorf(api.CodeConflict, "ask %s was already answered: %s", cur.ID, first(cur.Decision.Answer, cur.Decision.Decision)).
				WithHint("its answer is yours: record it on your item as an AGENT DECISION naming the ask").WithData(s.view(ctx, sub, cur))
		} else if err == nil && cur.Withdrawn != nil {
			err = api.Errorf(api.CodeConflict, "ask %s was withdrawn", cur.ID).WithHint("ask again with your own card")
		}
		if err != nil {
			s.groupMu.Unlock()
			api.WriteError(w, err)
			return
		}
	}
	ob, _ := json.Marshal(a.Options)
	reg := s.d.Store.Registry()
	var by any
	if a.By != nil {
		by = ms(*a.By)
	}
	if _, err := reg.ExecContext(ctx, `INSERT INTO asks (id, project, area, repo, pr, head_sha, options, body, asked_by, to_addr, created,
		work, question, card, card_sha, priority, topic, for_group, by_ms, dedupe, owner, same_as, apply_default, attachments, owner_person, asker_human, work_title, thread_title, pr_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, a.Project, a.Area, subj.Repo, subj.PR, subj.HeadSHA, string(ob), a.Body,
		a.AskedBy, a.To, ms(a.Created), a.Subject.Work, a.Subject.Question, card, a.CardSHA, a.Priority, a.Topic, forJSON, by, dedupe, a.Owner, a.SameAs, a.ApplyDefault, attached, a.OwnerPerson, a.AskerHuman, a.WorkTitle, a.ThreadTitle, a.PRURL); err != nil {
		if joined != nil {
			s.groupMu.Unlock()
		}
		if held != "" {
			_ = s.unhold(context.WithoutCancel(ctx), a.ID)
		}
		if dedupe != "" && strings.Contains(err.Error(), "UNIQUE") {
			if rerr := s.refuseRepeat(ctx, sub, a.Project, dedupe); rerr != nil { // lost a race to the same ask
				api.WriteError(w, rerr)
				return
			}
		}
		s.d.Log.Error("approvals: record ask", "ask", a.ID, "err", err)
		api.WriteError(w, err)
		return
	}
	if joined != nil {
		s.groupMu.Unlock()
	}
	if repeatScoped != "" {
		s.recordRepeat(ctx, a, repeatScoped, repeatKey, latest)
	}
	if linked != "" {
		s.link(context.WithoutCancel(ctx), a.ID, linked)
	}
	if held != "" { // no mail, no event: nobody is asked until the judge passes it (rejudgeHeld)
		s.permitDryRun(ctx, sub, a, card) // records only, never skips a card (permits.go)
		v := s.view(ctx, sub, a)
		v.Note, v.OwnerVia = heldNote(held), meVia
		api.WriteJSON(w, http.StatusOK, v)
		return
	}
	resolved := api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}
	m, err := orchcore.Mail(s.d).Send(ctx, sub, resolved, a.Area, orchapi.SendMailRequest{To: to, Body: a.Body, Needs: orchapi.NeedsHuman, ID: a.ID})
	if err != nil {
		if _, derr := reg.ExecContext(context.WithoutCancel(ctx), `DELETE FROM asks WHERE id = ?`, a.ID); derr != nil {
			s.d.Log.Error("approvals: drop an unsent ask", "ask", a.ID, "err", derr)
		}
		if repeatScoped != "" {
			s.dropRepeat(context.WithoutCancel(ctx), a.ID)
		}
		if linked != "" {
			_, _ = reg.ExecContext(context.WithoutCancel(ctx), `DELETE FROM ask_links WHERE ask = ?`, a.ID)
		}
		api.WriteError(w, err)
		return
	}
	if testHookAskMailSent != nil {
		testHookAskMailSent(a.ID)
	}
	s.permitDryRun(ctx, sub, a, card) // records only, never skips a card (permits.go)
	if m.Area != a.Area || m.To != a.To {
		// The row follows its mail's routing (which canonicalises).
		a.Area, a.To = m.Area, m.To
		if _, err := reg.ExecContext(context.WithoutCancel(ctx), `UPDATE asks SET area = ?, to_addr = ? WHERE id = ?`,
			a.Area, a.To, a.ID); err != nil {
			s.d.Log.Warn("approvals: align ask with its mail", "ask", a.ID, "err", err)
		}
	}
	// The ask's own event, after its mail's mail.sent: the restricted-mail
	// audience (operate on the area, or a party), like decision.recorded.
	if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: orchapi.EventAskCreated, Subject: a.ID,
		Project: a.Project, Area: a.Area, Data: a, Mentions: []string{a.AskedBy},
		Audience: &events.Audience{Need: authz.Operate, Parties: []string{a.AskedBy, a.To}}}); err != nil {
		s.d.Log.Warn("approvals: publish ask", "ask", a.ID, "err", err)
	}
	if req.Card != nil {
		a.Note = s.budgetNote(ctx, sub, a)
	}
	a.Linked = linked // after its event: view shows it only to who operates here
	a.Note = repeatNotes(joinNote, joined, linked, operator, a.Note)
	v := s.view(ctx, sub, a)
	v.OwnerVia = meVia
	if routedNote != "" {
		v.Note = strings.TrimSpace(routedNote + "\n" + v.Note)
	}
	api.WriteJSON(w, http.StatusOK, v)
}

// askerHuman is the human address of the machine sub asks from, and that
// machine's name: a member machine's node-bound token (SPEC 17.4) is its
// identity, the human of the machine it was redeemed for; anyone else asks
// from this machine.
func (s *Service) askerHuman(sub *authz.Subject) (human, machine string) {
	if sub != nil && sub.Via == authz.ViaToken && sub.Node != "" && sub.Machine != "" && sub.Machine != s.d.Config.Machine && sub.Identity.Name != "" {
		return "human:" + sub.Identity.Name, sub.Machine
	}
	return s.d.Config.HumanAddress(), s.d.Config.Machine
}

// fillCard fills a from a request with a card: its subject, area, options,
// body and the rest. It returns what the row stores for the card and for, and
// the ask's dedupe key.
func (s *Service) fillCard(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, req orchapi.NewAskRequest, slug string, areaGiven bool) (card, forJSON, dedupe string, err error) {
	prefixes := []string{slug}
	if work := a.Subject.Work; work != "" {
		rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder)
		if !ok {
			return "", "", "", api.Errorf(api.CodeUnavailable, "the work module is not running")
		}
		canon, area, resolvedProject, err := rec.ResolveItem(ctx, sub, a.Project, work)
		if err != nil {
			return "", "", "", err
		}
		if err := sub.Check(authz.Write, authz.Object{Project: resolvedProject, Area: area}, "work item", canon); err != nil {
			return "", "", "", err
		}
		a.Project = resolvedProject
		a.Subject.Work = canon
		if !areaGiven {
			a.Area = area
		}
		if a.Area == area { // readers of the ask's area may not read the item's: its title stays its own
			a.WorkTitle = s.workTitle(ctx, sub, resolvedProject, canon)
		}
		if err := sub.Check(authz.Write, authz.Object{Project: a.Project, Area: a.Area}, "work item", canon); err != nil {
			return "", "", "", err
		}
		src, ref, _ := strings.Cut(canon, ":")
		prefixes = append(prefixes, src)
		if head, _, ok := strings.Cut(ref, "-"); ok {
			prefixes = append(prefixes, head)
		}
	}
	c, keys, err := cleanCard(*req.Card, prefixes)
	if err != nil {
		return "", "", "", err
	}
	c.Action, c.Verify, c.OwnerStep = nil, nil, nil
	if req.Card.Action != nil {
		act := *req.Card.Action
		if (act.Kind == orchapi.ActionRun || act.Kind == orchapi.ActionCommand) && strings.TrimSpace(act.Machine) == "" { // the approver's only ready machine (run_targets.go)
			if act.Machine, err = s.defaultRunOn(ctx, sub, a, req.Owner, act); err != nil {
				return "", "", "", err
			}
		}
		if c.Action, err = s.cleanAction(ctx, sub, a.Project, a.Area, act, keys); err != nil {
			return "", "", "", err
		}
	}
	if req.Card.Verify != nil {
		if c.Verify, err = cleanVerify(*req.Card.Verify, c.Action, keys); err != nil {
			return "", "", "", err
		}
	}
	if c.Action != nil && c.Action.Kind == orchapi.ActionHand && c.Verify == nil {
		return "", "", "", api.Errorf(api.CodeBadRequest, "a hand brief needs --verify: the check that proves its work is done")
	}
	if req.Card.OwnerStep != nil {
		if c.OwnerStep, err = cleanStep(*req.Card.OwnerStep, c.Action, c.Verify, prefixes); err != nil {
			return "", "", "", err
		}
	}
	a.Card, a.Options = &c, keys
	a.Topic = strings.TrimSpace(req.Topic)
	if a.Topic != "" && !topicRe.MatchString(a.Topic) {
		return "", "", "", api.Errorf(api.CodeBadRequest, "topic %q: 3 to 80 lower-case letters, digits and dashes", a.Topic)
	}
	if req.By != nil {
		by := req.By.UTC().Truncate(time.Millisecond)
		if !by.After(s.now()) {
			return "", "", "", api.Errorf(api.CodeBadRequest, "by is already past")
		}
		if by.After(s.now().Add(30 * 24 * time.Hour)) {
			return "", "", "", api.Errorf(api.CodeBadRequest, "by is more than 30 days out; ask closer to when it matters")
		}
		a.By = &by
	}
	if req.For != nil {
		f, err := cleanFor(*req.For, a.Topic, req.By != nil)
		if err != nil {
			return "", "", "", err
		}
		a.For = &f
		forJSON = marshal(f)
		dedupe = "for:" + first(f.Ref, f.Group) + "|" + a.Topic
	} else if a.Subject.Work != "" {
		dedupe = "work:" + a.Subject.Work + "|" + a.Topic
	} else if a.Topic != "" {
		dedupe = "topic:" + a.Topic
	}
	if req.Priority != nil {
		if *req.Priority < 0 || *req.Priority > 4 {
			return "", "", "", api.Errorf(api.CodeBadRequest, "priority is 0 (first) to 4")
		}
		a.Priority = *req.Priority
	}
	if req.ApplyDefault {
		if err := s.checkDefault(ctx, sub, a, req); err != nil {
			return "", "", "", err
		}
		a.ApplyDefault = true
	}
	card = marshal(c)
	a.CardSHA = cardSHA(card)
	if a.Subject.Work == "" {
		a.Subject.Question = textSHA("zm-question/1\n" + a.ID + "\n" + a.CardSHA)
	}
	a.Body = renderCard(c, a.For)
	if dedupe != "" {
		authority := "operators"
		if !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
			authority = "party:" + sub.Address
		}
		dedupe = a.Area + "\x1f" + authority + "\x1f" + dedupe
	}
	return card, forJSON, dedupe, nil
}

// refuseRepeat is a conflict carrying the earlier ask when dedupe is taken.
// The bounded range also covers legacy duplicate reservations: canonical key,
// then a unit separator and server-owned suffix. It excludes longer topics.
func (s *Service) refuseRepeat(ctx context.Context, sub *authz.Subject, project, dedupe string) error {
	a, err := scanAsk(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+askCols+askFrom+` WHERE a.project = ? AND (a.dedupe = ? OR (a.dedupe >= ? AND a.dedupe < ?)) ORDER BY a.created,a.id LIMIT 1`, project, dedupe, dedupe+"\x1f", dedupe+"\x20"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	e := api.Errorf(api.CodeConflict, "this was already asked").
		WithHint("read the answer, or withdraw the open ask and ask again with another topic")
	if mayRead(sub.Scope, sub.Address, a) {
		e = e.WithData(s.view(ctx, sub, a))
	}
	return e
}

// budgetNote tells an asker who may see the counts that today is over
// target. It never stops the ask. A held card reached nobody, so it does
// not count until it is sent. (The project's owner can always answer, so
// having no paired approver is not worth a note.)
func (s *Service) budgetNote(ctx context.Context, sub *authz.Subject, a orchapi.Ask) string {
	if !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
		return ""
	}
	var n int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM asks a WHERE a.project = ? AND a.created > ? AND a.card != ''`+notHeld,
		a.Project, ms(s.now().Add(-24*time.Hour))).Scan(&n); err != nil || n <= DailyTarget {
		return ""
	}
	return fmt.Sprintf("%d asks in 24 hours, target %d: go with your gut unless it could truly go either way, and record it as an AGENT DECISION comment on the item", n, DailyTarget)
}

// testHookBeforeJoin runs after a join read its first ask open, before the
// insert (tests only).
var testHookBeforeJoin func(first string)

// testHookAskMailSent runs right after an ask's mail is sent (tests only).
var testHookAskMailSent func(id string)

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
