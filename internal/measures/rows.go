package measures

import (
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Labels shown where the records say nothing.
const (
	Unlabelled  = "unlabelled"
	OwnersLabel = "(owners)"  // a card addressed to the project's owners, no one by name
	ProjectMail = "(project)" // mail has no owner of its own
	Unknown     = "(unknown)" // an answer with no recorded answerer
)

// Rows is every measure, in the order the report shows them. Adding a row is
// appending one Row here (see the permit dry-run, approved-not-done and
// content-card rows other lanes add): write a Scan that emits Obs, pick an
// Agg, add a case to measures_test.go. Nothing else changes.
var Rows = []Row{
	{ID: "filed", Title: "cards filed", Agg: Count, Scan: scanFiled},
	{ID: "answered", Title: "cards answered", Agg: Count, Scan: scanAnswered},
	{ID: "recommended", Title: "answered with the recommendation", Agg: Rate, Scan: scanRecommended},
	{ID: "withdrawn_fast", Title: "withdrawn within the first minutes", Agg: Count, Scan: scanWithdrawnFast},
	{ID: "repeats", Title: "repeats of an open card", Agg: Count, Scan: scanRepeats},
	{ID: "median_answer", Title: "median time to answer", Agg: Median, Scan: scanMedianAnswer},
	{ID: "yes_not_done", Title: "yes not done after the wait", Agg: Count, Scan: scanNotDone},
	{ID: "not_done_steps", Title: "approved work zm brought back", Agg: Count, Scan: scanNotDoneSteps},
	{ID: "mail_ended", Title: "mail re-routed off ended sessions", Agg: Count, Scan: scanMailEnded},
	{ID: "group_untaken", Title: "group mail untaken after the wait", Agg: Count, Scan: scanGroupUntaken},
	{ID: "harm", Title: "harm: owner reversed or objected", Agg: Count, Scan: scanHarm},
}

func first(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// addressee is the owner a card is for; answerer is who answered it.
func addressee(a orchapi.Ask) string { return first(a.Owner, OwnersLabel) }

// counted reports whether a card's decision is one owner press that answered
// it: a card joined to an open one (same_as) is decided by its first's press
// and is not a second one, and a send-back (needs_context) answers nothing.
func counted(a orchapi.Ask) bool {
	return a.Decision != nil && a.SameAs == "" && a.Decision.Decision != orchapi.DecisionNeedsContext
}
func answerer(a orchapi.Ask) string {
	if a.Decision == nil {
		return ""
	}
	return first(a.Decision.Approver, Unknown)
}

// kindOf is the kind a judge labelled the card with, else "unlabelled".
func kindOf(a orchapi.Ask) string { return first(a.Labels[orchapi.LabelKind], Unlabelled) }

// filed: by the day the card was filed, for whom it was filed.
func scanFiled(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		emit(Obs{At: a.Created, Owner: addressee(a)})
	}
}

// answered: by the day it was answered, by who answered, one per press (see
// counted). Kept apart from filed: a card filed on one day is answered on
// another, often by another owner.
func scanAnswered(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if counted(a) {
			emit(Obs{At: a.Decision.Time, Owner: answerer(a)})
		}
	}
}

// recommended: of the answered cards that carried a recommendation, how many
// were answered with it, per kind.
func scanRecommended(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if !counted(a) || a.Card == nil || a.Card.Recommend == "" {
			continue
		}
		emit(Obs{At: a.Decision.Time, Owner: answerer(a), Group: "kind=" + kindOf(a), Hit: a.Decision.Decision == a.Card.Recommend})
	}
}

// withdrawn_fast: filed that day, taken back within WithdrawnWithin.
func scanWithdrawnFast(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if a.Withdrawn != nil && a.Withdrawn.Sub(a.Created) <= in.Cfg.WithdrawnWithin {
			emit(Obs{At: a.Created, Owner: addressee(a)})
		}
	}
}

// repeats: a card that joined an open one (zm's own join record, same_as) or
// that a judge labelled a repeat. Nothing else is guessed.
func scanRepeats(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if a.SameAs != "" || a.Labels[orchapi.LabelRepeatOf] != "" {
			emit(Obs{At: a.Created, Owner: addressee(a)})
		}
	}
}

// median_answer: the wait from filing to answer, by the day it was answered.
func scanMedianAnswer(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if counted(a) {
			emit(Obs{At: a.Decision.Time, Owner: answerer(a), Value: a.Decision.Time.Sub(a.Created).Seconds()})
		}
	}
}

// yes_not_done: answered, its work still not done (the card's own NotDone
// flag) and the answer older than NotDoneAfter when the records were read.
func scanNotDone(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if counted(a) && a.NotDone && in.Now.Sub(a.Decision.Time) >= in.Cfg.NotDoneAfter {
			emit(Obs{At: a.Decision.Time, Owner: answerer(a)})
		}
	}
}

// not_done_steps: what zm did with approved work still not done,
// one line per step, dated when zm took it, for the
// approver: back to the asker, handed to a fresh lane, no lane could take
// it, back to the approver; and work recorded done with evidence instead of
// its check.
func scanNotDoneSteps(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		x := a.ActionState
		if x == nil || !counted(a) {
			continue
		}
		step := func(at *time.Time, group string) {
			if at != nil {
				emit(Obs{At: *at, Owner: answerer(a), Group: "step=" + group})
			}
		}
		step(x.RemindedAt, "asker")
		if x.RelaneTask != "" {
			step(x.RelanedAt, "lane")
		} else if x.RelaneReason != "" {
			step(x.RelanedAt, "no_lane")
		}
		step(x.EscalatedAt, "approver")
		if x.DoneEvidence != "" && x.VerifiedAt == nil {
			step(x.CheckedAt, "evidence")
		}
	}
}

// mail_ended: the copies zm re-routed because the recipient session had ended
// ("orphan of <address>", dated when it re-routed them). Only what the orphan
// sweep copies: an agent's notes left unacked past its wait; reports, mail
// for a human and mail acked quickly are not.
func scanMailEnded(in *Input, emit func(Obs)) {
	for _, m := range in.Mail {
		if strings.HasPrefix(m.Via, "orphan of ") {
			emit(Obs{At: m.Created, Owner: ProjectMail})
		}
	}
}

// group_untaken: an agent's note in a machine's group mailbox that nobody
// took within GroupUntakenAfter: still open past it, or taken only after it
// (zm's own expiry sweep takes what is left). The same mail the expiry
// sweep picks: a plain note, not for a human, to everyone there.
func scanGroupUntaken(in *Input, emit func(Obs)) {
	for _, m := range in.Mail {
		if !strings.HasPrefix(m.Recipient, "machine:") || m.Kind != orchapi.MailKindNote || m.Needs == orchapi.NeedsHuman || m.Audience != "" {
			continue
		}
		waited := in.Now.Sub(m.Created)
		if m.AckedAt != nil {
			waited = m.AckedAt.Sub(m.Created)
		}
		if waited >= in.Cfg.GroupUntakenAfter {
			emit(Obs{At: m.Created, Owner: ProjectMail})
		}
	}
}

// harm: an owner's press after the fact: an objection to an answered card
// (POST /v0/asks/{id}/object) or an undo of an applied default. By the day
// of the press, for the owner who pressed.
func scanHarm(in *Input, emit func(Obs)) {
	for _, a := range in.Asks {
		if o := a.Objection; o != nil {
			emit(Obs{At: o.At, Owner: first(o.By, Unknown)})
		}
		if a.DefaultUndone != nil {
			emit(Obs{At: *a.DefaultUndone, Owner: first(a.DefaultUndoneBy, Unknown)})
		}
	}
}
