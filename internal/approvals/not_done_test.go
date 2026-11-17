package approvals_test

// Approved, not done (SPEC 16): a yes on a choice that needs work
// records APPROVED, NOT DONE on the item and stays in Decisions until the
// asker's check passes; it comes back to the asker, then the approver
// (relane_test.go: a fresh lane between them).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func verifyCard(cmd string) *orchapi.AskCard {
	c := card()
	c.Verify = &orchapi.AskVerify{Command: cmd}
	return c
}

func exit(n int) *int { return &n }

func listedAt(t *testing.T, in orchapi.Inbox, id string) int {
	t.Helper()
	for i, x := range in.Decisions {
		if x.Ask != nil && x.Ask.ID == id {
			return i
		}
	}
	return -1
}

func TestApprovedNotDoneUntilTheCheckPasses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("make the app side live")
	agent, agentAddr := h.session("aaaaaaaa")

	// Refused at creation: two lines, a choice the card does not have, empty.
	for name, v := range map[string]*orchapi.AskVerify{
		"two lines": {Command: "true\nrm -rf /"},
		"choice":    {Command: "true", On: "c"},
		"empty":     {Command: "  "},
	} {
		c := card()
		c.Verify = v
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "refused-" + strings.ReplaceAll(name, " ", "-"), Card: c}); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}

	check := "curl -fsS https://app.example.com/health && curl -fsS https://example.com/pricing"
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "go-live", Card: verifyCard(check)}))
	if a.Card.Verify == nil || a.Card.Verify.On != "a" || a.Card.Verify.Command != check || !strings.Contains(a.Body, "approved, not done until the asker's check passes") {
		t.Fatalf("frozen verify %+v, body %q", a.Card.Verify, a.Body)
	}
	if a.ActionState != nil || a.NotDone {
		t.Fatalf("open ask has a state: %+v", a.ActionState)
	}
	// Not approved yet: there is nothing to check.
	if _, err := orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(0)}); code(err) != api.CodeConflict {
		t.Fatalf("check before the yes: %v", err)
	}
	open := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "other", Card: card()}))

	must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}))
	cs := h.comments(it.ID)
	if len(cs) != 1 || !strings.Contains(cs[0], "OWNER DECISION") || !strings.Contains(cs[0], "APPROVED, NOT DONE: zm keeps ask "+a.ID) {
		t.Fatalf("decision comment: %q", cs)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.ActionState == nil || got.ActionState.State != orchapi.ActionApproved || !got.NotDone || !strings.HasPrefix(got.ActionStateText, "Approved, not done") {
		t.Fatalf("after the yes: %+v %q", got.ActionState, got.ActionStateText)
	}
	// Listed on its own: after the open ask.
	in := must(orchclient.Decisions(ctx, h.owner, "", ""))
	if i, j := listedAt(t, in, open.ID), listedAt(t, in, a.ID); i < 0 || j < 0 || j < i {
		t.Fatalf("placement: open %d, not done %d", i, j)
	}
	// Confirming is refused while it is not done.
	if _, err := orchclient.ConfirmAction(ctx, h.owner, "", a.ID); code(err) != api.CodeConflict {
		t.Fatalf("confirm while not done: %v", err)
	}

	// A check of another card is refused; a failing
	// one is recorded and leaves it not done.
	if _, err := orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: strings.Repeat("0", 64), Exit: exit(0)}); code(err) != api.CodeConflict {
		t.Fatalf("another card: %v", err)
	}
	got = must(orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(22), Output: "curl: (22) 404\n"}))
	if got.ActionState.State != orchapi.ActionApproved || got.ActionState.CheckExit == nil || *got.ActionState.CheckExit != 22 ||
		!strings.Contains(got.ActionStateText, "exited 22") || !strings.Contains(got.ActionStateText, "404") {
		t.Fatalf("failed check: %+v %q", got.ActionState, got.ActionStateText)
	}

	// A report with no exit, a blank give-up, or both: refused.
	for name, req := range map[string]orchapi.CheckRequest{
		"no exit":   {CardSHA: a.CardSHA},
		"blank":     {CardSHA: a.CardSHA, GiveUp: "  "},
		"exit, too": {CardSHA: a.CardSHA, GiveUp: "stuck", Exit: exit(0)},
		"two lines": {CardSHA: a.CardSHA, GiveUp: "stuck\nreally"},
	} {
		if _, err := orchclient.CheckAsk(ctx, agent, a.ID, req); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.ActionState.State != orchapi.ActionApproved {
		t.Fatalf("after refused reports: %+v", got.ActionState)
	}

	// After a day it comes back to the asker; with no fresh lane for the
	// project, a day later to the approver, and on top of Decisions.
	h.clock.add(25 * time.Hour)
	h.svc.WatchNotDone(ctx)
	ms := must(orchclient.Inbox(ctx, agent, orchapi.InboxQuery{}))
	reminded := 0
	for _, m := range ms {
		if strings.HasPrefix(m.Body, "APPROVED, NOT DONE: ") && strings.Contains(m.Body, a.ID) && strings.Contains(m.Body, "zm ask check "+a.ID) {
			reminded++
		}
	}
	if reminded != 1 {
		t.Fatalf("asker reminders: %d in %+v", reminded, ms)
	}
	h.svc.WatchNotDone(ctx) // once
	got = must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.ActionState.RemindedAt == nil || got.ActionState.EscalatedAt != nil {
		t.Fatalf("reminded: %+v", got.ActionState)
	}
	h.clock.add(24 * time.Hour)
	h.svc.WatchNotDone(ctx)
	got = must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.ActionState.EscalatedAt == nil {
		t.Fatalf("not escalated: %+v", got.ActionState)
	}
	in = must(orchclient.Decisions(ctx, h.owner, "", ""))
	if len(in.Decisions) == 0 || in.Decisions[0].Ask == nil || in.Decisions[0].Ask.ID != a.ID {
		t.Fatalf("escalated is not on top: %+v", in.Decisions)
	}

	// The check passes: DONE on the item, once.
	got = must(orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(0), Output: "ok"}))
	if got.ActionState.State != orchapi.ActionDone || got.NotDone || got.ActionState.VerifiedAt == nil {
		t.Fatalf("passed: %+v", got.ActionState)
	}
	h.svc.WatchNotDone(ctx)
	done := 0
	for _, c := range h.comments(it.ID) {
		if strings.HasPrefix(c, "DONE (verified ") && strings.Contains(c, a.ID) && strings.Contains(c, "as "+agentAddr+" reported") {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("DONE comments: %d in %q", done, h.comments(it.ID))
	}
	if _, err := orchclient.CheckAsk(ctx, agent, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Exit: exit(1)}); code(err) != api.CodeConflict {
		t.Fatalf("check after done: %v", err)
	}
	if c := must(orchclient.ConfirmAction(ctx, h.owner, "", a.ID)); c.ActionState.ConfirmedAt == nil {
		t.Fatalf("confirm: %+v", c.ActionState)
	}
}

// The other choice approves nothing to do: no NOT DONE, no state. Giving the
// work up fails it, on top until an owner confirms it.
func TestNotDoneOnlyOnTheCheckedChoiceAndGiveUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("deploy the app")
	agent, _ := h.session("aaaaaaaa")

	by := time.Now().Add(time.Hour)
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "by-default", Card: verifyCard("true"), By: &by, ApplyDefault: true}); code(err) != api.CodeBadRequest || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("apply_default with a verify check: %v", err)
	}

	no := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "say-no", Card: verifyCard("true")}))
	must(orchclient.DecideWith(ctx, h.device(ipPhone), no.ID, orchapi.DecideRequest{Decision: "b"}))
	got := must(orchclient.GetAsk(ctx, h.owner, no.ID))
	if got.ActionState != nil || got.NotDone {
		t.Fatalf("b: %+v", got.ActionState)
	}
	for _, c := range h.comments(it.ID) {
		if strings.Contains(c, "NOT DONE") {
			t.Fatalf("b recorded NOT DONE: %q", c)
		}
	}

	yes := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "say-yes", Card: verifyCard("true")}))
	must(orchclient.DecideWith(ctx, h.device(ipPhone), yes.ID, orchapi.DecideRequest{Decision: "a"}))
	got = must(orchclient.CheckAsk(ctx, agent, yes.ID, orchapi.CheckRequest{CardSHA: yes.CardSHA, GiveUp: "the vendor is down until Monday"}))
	if got.ActionState.State != orchapi.ActionFailed || !strings.Contains(got.ActionState.Reason, "vendor is down") || got.NotDone {
		t.Fatalf("give up: %+v", got.ActionState)
	}
	in := must(orchclient.Decisions(ctx, h.owner, "", ""))
	if len(in.Decisions) == 0 || in.Decisions[0].Ask == nil || in.Decisions[0].Ask.ID != yes.ID {
		t.Fatalf("given up is not on top: %+v", in.Decisions)
	}
}

// The approver reminder goes to the person the approving device was added
// for (zm approver add --for), by the identity's name, not its ID.
func TestApproverReminderGoesToTheDevicesPerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	add := must(orchclient.AddApproverFor(ctx, h.owner, "bea-phone", "bea"))
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	it := h.workItem("make the app side live")
	agent, _ := h.session("aaaaaaaa")

	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "go-live", Card: verifyCard("true")}))
	must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}))
	h.clock.add(49 * time.Hour)
	h.svc.WatchNotDone(ctx)
	if got := h.mailTo("human:bea", "you approved ask "+a.ID); len(got) != 1 {
		t.Fatalf("mails to human:bea: %d", len(got))
	}
}
