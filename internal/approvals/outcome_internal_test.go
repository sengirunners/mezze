package approvals

import (
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Each ask is in exactly one state, and Next names exactly one party.
func TestOutcomeBits(t *testing.T) {
	now := time.Now()
	run := &orchapi.AskAction{Kind: orchapi.ActionRun, On: "a", Machine: "box"}
	check := &orchapi.AskVerify{Command: "true", On: "a"}
	card := func(act *orchapi.AskAction, v *orchapi.AskVerify) *orchapi.AskCard {
		return &orchapi.AskCard{Question: "q", Choices: []orchapi.AskChoice{{Key: "a"}, {Key: "b"}}, Action: act, Verify: v}
	}
	answered := func(c *orchapi.AskCard, x *orchapi.ActionState) orchapi.Ask {
		return orchapi.Ask{Status: orchapi.AskAnswered, Card: c, ActionState: x}
	}
	cases := []struct {
		name      string
		a         orchapi.Ask
		next      string
		unchecked bool
	}{
		{"open", orchapi.Ask{Status: orchapi.AskOpen, Card: card(nil, nil)}, orchapi.NextYou, false},
		{"held for an outside party", orchapi.Ask{Status: orchapi.AskOpen, Card: card(nil, nil), For: &orchapi.AskFor{Group: "Acme", Contact: "Bea"}}, orchapi.NextNobody, false},
		{"given up", answered(card(nil, check), &orchapi.ActionState{State: orchapi.ActionFailed, Reason: gaveUp + "no access"}), orchapi.NextYou, false},
		{"withdrawn", orchapi.Ask{Status: orchapi.AskWithdrawn}, orchapi.NextNobody, false},
		{"answered, nothing tracked", answered(card(nil, nil), nil), orchapi.NextNobody, false},
		{"queued", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionQueued}), orchapi.NextAgent, false},
		{"running", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionRunning}), orchapi.NextAgent, false},
		{"approved, check to pass", answered(card(nil, check), &orchapi.ActionState{State: orchapi.ActionApproved}), orchapi.NextAgent, false},
		{"approved, escalated", answered(card(nil, check), &orchapi.ActionState{State: orchapi.ActionApproved, EscalatedAt: &now}), orchapi.NextYou, false},
		{"failed", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionFailed}), orchapi.NextYou, false},
		{"failed, confirmed", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionFailed, ConfirmedAt: &now}), orchapi.NextNobody, false},
		{"ran, no check", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionDone}), orchapi.NextYou, true},
		{"ran, no check, confirmed", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionDone, ConfirmedAt: &now}), orchapi.NextNobody, false},
		{"ran, check passed", answered(card(run, check), &orchapi.ActionState{State: orchapi.ActionDone, VerifiedAt: &now}), orchapi.NextNobody, false},
		{"done with evidence", answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionDone, DoneEvidence: "seen live"}), orchapi.NextNobody, false},
	}
	for _, c := range cases {
		a := c.a
		outcomeBits(&a)
		if a.Next != c.next || a.RanUnchecked != c.unchecked || a.YourStep {
			t.Errorf("%s: next %q ran_unchecked %v your_step %v, want %q %v false", c.name, a.Next, a.RanUnchecked, a.YourStep, c.next, c.unchecked)
		}
	}
	// An objection settles a ran-unchecked action.
	a := answered(card(run, nil), &orchapi.ActionState{State: orchapi.ActionDone})
	a.Objection = &orchapi.AskObjection{At: now, By: "bea"}
	if outcomeBits(&a); a.RanUnchecked || a.Next != orchapi.NextNobody {
		t.Errorf("objected: next %q ran_unchecked %v", a.Next, a.RanUnchecked)
	}
}
