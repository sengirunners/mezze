package approvals_test

// Your step (SPEC 16): a card may carry one step only the person it is for
// can do; after the yes it is their turn until zm's check passes.

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func stepCard() *orchapi.AskCard {
	c := verifyCard("curl -fsS https://example.com/accepted")
	c.OwnerStep = &orchapi.AskStep{Label: "Accept the new agreement", Steps: []string{"Sign in as yourself.", "Press Agree."},
		Link: "https://www.Example.com/account/agreements", Minutes: 2}
	return c
}

func TestYourStepWaitsOnThePersonAndOnlyTheCheckCloses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("ship the update")
	agent, agentAddr := h.session("aaaaaaaa")

	// Refused at creation.
	for name, mut := range map[string]func(c *orchapi.AskCard){
		"http link":    func(c *orchapi.AskCard) { c.OwnerStep.Link = "http://example.com/x" },
		"user in link": func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://bea:pw@example.com/x" },
		"no steps":     func(c *orchapi.AskCard) { c.OwnerStep.Steps = nil },
		"no label":     func(c *orchapi.AskCard) { c.OwnerStep.Label = " " },
		"path in step": func(c *orchapi.AskCard) { c.OwnerStep.Steps = []string{"open ~/work/notes/today.md"} },
		"no verify":    func(c *orchapi.AskCard) { c.Verify = nil },
		"with action": func(c *orchapi.AskCard) {
			c.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: "1. do it"}
		},
		"too many mins": func(c *orchapi.AskCard) { c.OwnerStep.Minutes = 61 },
		"ip address":    func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://93.184.216.34/x" },
		"odd port":      func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://example.com:8443/x" },
		"look-alike":    func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://\u0430pple.com/account" },
		"fragment":      func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://example.com/x#y" },
		"one label":     func(c *orchapi.AskCard) { c.OwnerStep.Link = "https://intranet/x" },
	} {
		c := stepCard()
		mut(c)
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "refused-" + strings.ReplaceAll(name, " ", "-"), Card: c, Owner: "ivo"}); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A step is one person's.
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "nobody-named", Card: stepCard()}); code(err) != api.CodeBadRequest {
		t.Errorf("a step for the owners at large: %v", err)
	}

	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "agreement", Card: stepCard(), Owner: "ivo"}))
	if st := a.Card.OwnerStep; st == nil || st.Domain != "example.com" || len(st.Steps) != 2 || !strings.Contains(a.ActionText, "your step, about 2 minutes: Accept the new agreement on example.com") {
		t.Fatalf("frozen step %+v, action text %q", a.Card.OwnerStep, a.ActionText)
	}
	if a.Next != orchapi.NextYou || a.YourStep {
		t.Fatalf("open: next %q your_step %v", a.Next, a.YourStep)
	}
	// Nothing to say done before the yes.
	if _, err := orchclient.StepDone(ctx, h.owner, "", a.ID, a.CardSHA); code(err) != api.CodeConflict {
		t.Fatalf("step before the yes: %v", err)
	}
	must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}))
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if !got.YourStep || got.Next != orchapi.NextYou || !strings.Contains(got.ActionStateText, "waiting on ivo's own step") {
		t.Fatalf("after the yes: your_step %v next %q text %q", got.YourStep, got.Next, got.ActionStateText)
	}
	// The asker's evidence never closes it.
	if _, err := orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Evidence: "I saw it accepted"}); code(err) != api.CodeConflict {
		t.Fatalf("evidence on a person's step: %v", err)
	}
	// An agent session may not say the person did it; a changed card is refused.
	if _, err := orchclient.StepDone(ctx, agent, "", a.ID, a.CardSHA); code(err) != api.CodeForbidden {
		t.Fatalf("an agent said the step is done: %v", err)
	}
	if _, err := orchclient.StepDone(ctx, h.owner, "", a.ID, strings.Repeat("0", 64)); code(err) != api.CodeConflict {
		t.Fatalf("a changed card: %v", err)
	}
	// Nobody says it for the person: another member of the project is refused.
	other := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	if _, err := orchclient.StepDone(ctx, other, "", a.ID, a.CardSHA); code(err) != api.CodeForbidden {
		t.Fatalf("another person said ivo's step is done: %v", err)
	}
	done := must(orchclient.StepDone(ctx, h.owner, "", a.ID, a.CardSHA))
	if done.YourStep || done.Next != orchapi.NextAgent || done.ActionState.StepDoneAt == nil || done.ActionState.State != orchapi.ActionApproved {
		t.Fatalf("after I did it: your_step %v next %q state %+v", done.YourStep, done.Next, done.ActionState)
	}
	if len(h.mailTo(agentAddr, "STEP DONE")) != 1 {
		t.Fatal("the asker was not asked to run the check")
	}
	// Pressing again at once mails nobody again.
	must(orchclient.StepDone(ctx, h.owner, "", a.ID, a.CardSHA))
	if n := len(h.mailTo(agentAddr, "STEP DONE")); n != 1 {
		t.Fatalf("a second press within a minute mailed the asker again (%d mails)", n)
	}
	// A failing check after it: the person's turn again.
	failed := must(orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(1), Output: "not accepted"}))
	if !failed.YourStep || failed.Next != orchapi.NextYou {
		t.Fatalf("after a failing check: your_step %v next %q", failed.YourStep, failed.Next)
	}
	// Only the passing check closes it.
	passed := must(orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(0)}))
	if passed.ActionState.State != orchapi.ActionDone || passed.ActionState.VerifiedAt == nil || passed.YourStep || passed.Next != orchapi.NextNobody {
		t.Fatalf("after the check: %+v next %q", passed.ActionState, passed.Next)
	}
}
