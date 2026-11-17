package approvals

import "github.com/sengirunners/mezze/internal/orchapi"

// Outcome bits (SPEC 16 "Whose turn"): what a client shows of an ask, read
// from the records the ask already carries, never from its text. One state
// at a time: Next names exactly one party, and RanUnchecked and YourStep are
// never both set.

// outcomeBits fills a's RanUnchecked, YourStep and Next.
func outcomeBits(a *orchapi.Ask) {
	a.RanUnchecked, a.YourStep = false, false
	a.Next = whoNext(a)
}

// whoNext is whose turn a is: the person it is for, an agent (its asker, a
// fresh lane or zm running its action), or nobody.
func whoNext(a *orchapi.Ask) string {
	switch a.Status {
	case orchapi.AskWithdrawn:
		return orchapi.NextNobody
	case orchapi.AskOpen:
		if a.For != nil {
			return orchapi.NextNobody // it waits on the outside party, not a person here
		}
		return orchapi.NextYou // to answer, or (held, not checked) an owner releases it
	}
	x := a.ActionState
	if x == nil {
		return orchapi.NextNobody // the answer is the end of what zm tracks
	}
	if x.ConfirmedAt != nil && !notDoneState(x.State) {
		return orchapi.NextNobody // an owner saw how it went
	}
	switch x.State {
	case orchapi.ActionQueued, orchapi.ActionRunning:
		return orchapi.NextAgent
	case orchapi.ActionApproved:
		if yourStep(a) {
			a.YourStep = true
			return orchapi.NextYou
		}
		if x.EscalatedAt != nil {
			return orchapi.NextYou // it came back to the approver (not_done.go)
		}
		return orchapi.NextAgent
	case orchapi.ActionFailed:
		return orchapi.NextYou // to try again, tell the agent, or confirm it
	case orchapi.ActionDone:
		if ranUnchecked(a) {
			a.RanUnchecked = true
			return orchapi.NextYou
		}
	}
	return orchapi.NextNobody
}

// ranUnchecked: a's action ran to done, no check proved it (the card has
// none and nobody recorded evidence), and no owner has looked yet
// (action/confirm, or an objection).
func ranUnchecked(a *orchapi.Ask) bool {
	x := a.ActionState
	return x != nil && x.State == orchapi.ActionDone && x.VerifiedAt == nil && x.DoneEvidence == "" &&
		x.ConfirmedAt == nil && a.Objection == nil && actionOf(*a) != nil && verifyOf(*a) == nil
}
