package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// routeJudge passes every card and answers the card router with the next of
// its votes ("" = no answer); it records what the router was shown.
type routeJudge struct {
	mu     sync.Mutex
	judge  string // the card judge's answer ("" = no answer: held)
	votes  []string
	i      int
	routed []string
}

func (j *routeJudge) Call(_ context.Context, prompt string, _ json.RawMessage) (json.RawMessage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !strings.Contains(prompt, "zm's card router") {
		if j.judge == "" {
			return nil, errors.New("the provider is down")
		}
		return json.RawMessage(j.judge), nil
	}
	j.routed = append(j.routed, prompt)
	v := ""
	if len(j.votes) > 0 {
		v = j.votes[j.i%len(j.votes)]
	}
	j.i++
	if v == "" {
		return nil, errors.New("the provider is down")
	}
	return json.RawMessage(`{"reasoning":"","party":` + v + `}`), nil
}

func (j *routeJudge) set(judge string, votes ...string) {
	j.mu.Lock()
	j.judge, j.votes, j.i, j.routed = judge, votes, 0, nil
	j.mu.Unlock()
}

// SPEC 16 "Card judge": a card whose question is for one of
// the project's known outside parties is stored held for that party, out of
// the owners' queue; anything short of a clear majority for one party (a
// split, no answer, a number off the list) leaves it with the owners.
func TestCardRouterHoldsForAKnownParty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &routeJudge{judge: judgePass}
	h.svc.SetJudge(j)
	agent, _ := h.session("aeaeaeae")

	// No party known yet: the router is not asked.
	plain := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "before-parties"}))
	if plain.For != nil || len(j.routed) != 0 {
		t.Fatalf("routed with no parties: %+v (%d router calls)", plain.For, len(j.routed))
	}
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "known-a",
		For: &orchapi.AskFor{Group: "Northwind Clinic", Contact: "Dr. Example", Ref: "nw-1"}}))
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "known-b",
		For: &orchapi.AskFor{Group: "Contoso Residency", Contact: "the chief"}}))

	// Two of three votes name party 2 (the older, Northwind), one has no answer.
	j.set(judgePass, "2", "2", "")
	asker, addr := h.session("aeaeaeab") // asks nothing else
	routed := must(orchclient.NewAsk(ctx, asker, orchapi.NewAskRequest{Card: card(), Topic: "routed"}))
	if routed.For == nil || routed.For.Group != "Northwind Clinic" || routed.For.Contact != "Dr. Example" || routed.For.Ref != "nw-1" ||
		!strings.Contains(routed.Note, "held for Northwind Clinic") {
		t.Fatalf("routed ask: for %+v note %q", routed.For, routed.Note)
	}
	if !strings.Contains(j.routed[0], "Should we keep last year's swaps?") || !strings.Contains(j.routed[0], "Contoso Residency") {
		t.Fatalf("the router saw: %q", j.routed[0])
	}
	if m := must(orchclient.GetMail(ctx, h.owner, routed.ID)); m.HeldFor != "Northwind Clinic" {
		t.Fatalf("routed ask's mail: held_for %q", m.HeldFor)
	}
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("a routed ask put its asker at needs_human")
	}

	for name, votes := range map[string][]string{
		"split":          {"1", "2", "2"},
		"no majority":    {"1", "", ""},
		"stays":          {"0", "0", "1"},
		"off the list":   {"7", "7", "7"},
		"one off":        {"7", "2", "2"},
		"router is down": {"", "", ""},
	} {
		j.set(judgePass, votes...)
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "stay-" + strings.ReplaceAll(name, " ", "-")}))
		if a.For != nil {
			t.Errorf("%s: routed to %+v", name, a.For)
		}
	}
	// A card with a deadline is never routed (a party's list has none).
	j.set(judgePass, "1", "1", "1")
	by := h.clock.now().Add(48 * time.Hour)
	if a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "with-deadline", By: &by})); a.For != nil {
		t.Fatalf("a card with a deadline was routed: %+v", a.For)
	}
	// The judge's fail wins: nothing is stored.
	j.set(judgeFail, "1", "1", "1")
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "judge-fails"}); err == nil {
		t.Fatal("a failed card was stored")
	}
}

// A card the judge held is routed when a later judging passes it, before it
// is sent.
func TestCardRouterOnRejudge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &routeJudge{judge: judgePass}
	h.svc.SetJudge(j)
	agent, _ := h.session("afafafaf")
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "known",
		For: &orchapi.AskFor{Group: "Northwind Clinic", Contact: "Dr. Example"}}))
	j.set("", "1", "1", "1")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "held-then-routed"}))
	if a.For != nil || !strings.Contains(a.Note, "held, not sent") {
		t.Fatalf("held: %+v", a)
	}
	j.set(judgePass, "1", "1", "1")
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.For == nil || got.For.Group != "Northwind Clinic" || !strings.Contains(got.Body, "Northwind Clinic") {
		t.Fatalf("after the pass: for %+v body %q", got.For, got.Body)
	}
	if m := must(orchclient.GetMail(ctx, h.owner, a.ID)); m.HeldFor != "Northwind Clinic" {
		t.Fatalf("its mail: held_for %q", m.HeldFor)
	}
}

// The router never moves a guest's card, a card put to a named person, or
// onto a party only a guest's asks name.
func TestCardRouterGates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &routeJudge{judge: judgePass}
	h.svc.SetJudge(j)
	solver := h.ownerIn("harbor/solver", "")
	// A party only a guest named is not a candidate.
	must(orchclient.NewAsk(ctx, h.guest(), orchapi.NewAskRequest{Card: card(), Topic: "guest-party",
		For: &orchapi.AskFor{Group: "Fabrikam Outpatient", Contact: "the manager"}}))
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card(), Topic: "op-card"})); a.For != nil || len(j.routed) != 0 {
		t.Fatalf("routed onto a guest's party: %+v (%d router calls)", a.For, len(j.routed))
	}
	// An operator's party in that area: a guest's card is still not routed.
	must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card(), Topic: "op-party",
		For: &orchapi.AskFor{Group: "Northwind Clinic", Contact: "Dr. Example"}}))
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, h.guest(), orchapi.NewAskRequest{Card: card(), Topic: "guest-card"})); a.For != nil {
		t.Fatalf("a guest's card was routed: %+v", a.For)
	}
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card(), Topic: "op-card-2"})); a.For == nil || a.For.Group != "Northwind Clinic" {
		t.Fatalf("an operator's card in the area: %+v", a.For)
	}
	// Another area's parties are not this area's candidates.
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, h.ownerIn("harbor", ""), orchapi.NewAskRequest{Card: card(), Topic: "other-area"})); a.For != nil || len(j.routed) != 0 {
		t.Fatalf("routed onto another area's party: %+v", a.For)
	}
	// One vote to stay keeps it with the owners.
	j.set(judgePass, "1", "1", "0")
	if a := must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card(), Topic: "one-stays"})); a.For != nil {
		t.Fatalf("routed past a vote to stay: %+v", a.For)
	}
	// A card with no topic is not routed (one ask per party and topic).
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card()})); a.For != nil {
		t.Fatalf("a card with no topic was routed: %+v", a.For)
	}
	// A card put to a named person stays theirs.
	j.set(judgePass, "1", "1", "1")
	if a := must(orchclient.NewAsk(ctx, solver, orchapi.NewAskRequest{Card: card(), Topic: "to-ivo", Owner: "ivo"})); a.For != nil {
		t.Fatalf("a card to a named person was routed: %+v", a.For)
	}
}
