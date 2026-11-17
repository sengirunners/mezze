package approvals_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// judgeRepeats is a pass that names open card id as the one it repeats.
func judgeRepeats(id string) string {
	return strings.Replace(judgePass, `"repeat_of":""`, `"repeat_of":"`+id+`"`, 1)
}

// otherWords is card()'s question in other words.
func otherWords() *orchapi.AskCard {
	c := card()
	c.Question = "Do last year's shift swaps stay in this year's schedule?"
	return c
}

// SPEC 16 "Card judge": a card the judge finds asks the same
// as an open one joins it, not a second card; the judge is shown the open
// cards; the owner's one answer reaches both.
func TestRepeatJoinsOpenCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{answer: judgePass}
	h.svc.SetJudge(j)
	agentA, _ := h.session("a1a1a1a1")
	agentB, _ := h.session("b1b1b1b1")
	first := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("swaps one").ID}, Card: card()}))
	j.set(judgeRepeats(first.ID))
	second := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("swaps two").ID}, Card: otherWords()}))
	if second.SameAs != first.ID || !strings.Contains(second.Note, "joined open ask "+first.ID) || second.Card.Question != first.Card.Question {
		t.Fatalf("a repeat was not joined: %+v", second)
	}
	if seen := j.seen[len(j.seen)-1]; !strings.Contains(seen, `"open_cards"`) || !strings.Contains(seen, first.ID) {
		t.Fatalf("the judge was not shown the open card: %q", seen)
	}
	must(orchclient.Choose(ctx, h.owner, "", first.ID, orchapi.DecideRequest{Decision: "b"}))
	if got := must(orchclient.GetAsk(ctx, agentB, second.ID)); got.Decision == nil || got.Decision.Decision != "b" {
		t.Fatalf("the joined ask did not get the answer: %+v", got.Decision)
	}
}

// A card that carries an action is joined only by a card with no action or
// the same action on the same target; another action is linked, not
// joined. A joined ask carries no action: the first ask is answered (on a
// signed device, as its action needs), its action runs once, and the
// answer reaches the joined asks; a joined ask cannot be answered itself.
// `zm ask join` on an action card works too.
func TestRepeatOfActionCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	j := &switchJudge{answer: judgePass}
	h.svc.SetJudge(j)
	agentA, _ := h.session("a2a2a2a2")
	agentB, _ := h.session("b2b2b2b2")
	brief := "1. Turn on remote tasks for this machine.\n2. Switch the shelf on.\n3. Run the check."
	hand := func(c *orchapi.AskCard, brief string) *orchapi.AskCard {
		c.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief}
		c.Verify = &orchapi.AskVerify{Command: "test -f /tmp/done"}
		return c
	}
	work := func(title string) orchapi.AskSubject { return orchapi.AskSubject{Work: h.workItem(title).ID} }
	first := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: work("act one"), Card: hand(card(), brief)}))
	j.set(judgeRepeats(first.ID))

	plain := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("act two"), Card: otherWords()}))
	if plain.SameAs != first.ID || plain.Card.Action != nil || plain.Card.Verify != nil {
		t.Fatalf("a card with no action did not join, or joined with the action: %+v", plain)
	}
	same := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("act three"), Card: hand(otherWords(), brief+"\n")}))
	if same.SameAs != first.ID || same.Card.Action != nil {
		t.Fatalf("the same action did not join: %+v", same)
	}
	other := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("act four"), Card: hand(otherWords(), "1. Turn the shelf off.\n2. Run the check.")}))
	if other.SameAs != "" || !strings.Contains(other.Note, "linked to open ask "+first.ID) {
		t.Fatalf("another action was joined: %+v", other)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, other.ID)); got.Linked != first.ID {
		t.Fatalf("the owner does not see the link: %+v", got)
	}
	explicit, err := orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{SameAs: first.ID})
	if err != nil || explicit.SameAs != first.ID || explicit.Card.Action != nil {
		t.Fatalf("zm ask join on an action card: %+v %v", explicit, err)
	}

	if _, err := orchclient.Choose(ctx, h.owner, "", plain.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "answer "+first.ID) {
		t.Fatalf("answering a joined ask of an action card from the socket: %v", err)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), plain.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("answering a joined ask of an action card on a device: %v", err)
	}
	if _, err := orchclient.Choose(ctx, h.owner, "", first.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the action's choice from the socket: %v", err)
	}
	must(orchclient.DecideWith(ctx, h.device(ipPhone), first.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, agentA, first.ID)); got.ActionState == nil || got.ActionState.State != orchapi.ActionApproved {
		t.Fatalf("the first ask's action: %+v", got.ActionState)
	}
	for _, id := range []string{plain.ID, same.ID} {
		got := must(orchclient.GetAsk(ctx, agentB, id))
		if got.Decision == nil || got.Decision.Decision != "a" || got.ActionState != nil {
			t.Fatalf("joined ask %s after the signed answer: %+v %+v", id, got.Decision, got.ActionState)
		}
	}
	if got := must(orchclient.GetAsk(ctx, agentB, other.ID)); got.Decision != nil {
		t.Fatalf("a linked ask was answered with the first: %+v", got.Decision)
	}
}

// A card held because the judge did not answer, then found to repeat an
// open card, joins it when it is judged again and is sent as a joined ask.
func TestRepeatHeldJoins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{answer: judgePass}
	h.svc.SetJudge(j)
	agentA, _ := h.session("a3a3a3a3")
	agentB, _ := h.session("b3b3b3b3")
	first := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("held one").ID}, Card: card()}))
	j.set("")
	held := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("held two").ID}, Card: otherWords()}))
	if !strings.Contains(held.Note, "held") {
		t.Fatalf("not held: %+v", held)
	}
	j.set(judgeRepeats(first.ID))
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	got := must(orchclient.GetAsk(ctx, agentB, held.ID))
	if got.SameAs != first.ID || strings.Contains(got.Note, "held") || got.Card.Question != first.Card.Question || !h.inDecisions(held.ID) {
		t.Fatalf("a held repeat was not joined and sent: %+v", got)
	}
	must(orchclient.Choose(ctx, h.owner, "", first.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, agentB, held.ID)); got.Decision == nil || got.Decision.Decision != "a" {
		t.Fatalf("the joined ask did not get the answer: %+v", got.Decision)
	}
}

// The judge is shown only open cards the asker may read: a guest sees its
// own, never another's, and a judge naming a card it was not shown holds
// the ask (it is never joined to it, live or when judged again).
func TestRepeatGuestSeesOnlyItsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{answer: judgePass}
	h.svc.SetJudge(j)
	agent, _ := h.session("a4a4a4a4")
	hidden := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "hidden-swaps"}))
	guest := h.guest()
	own := must(orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: colorCard(), Topic: "guest-one"}))
	j.set(judgeRepeats(hidden.ID))
	g := must(orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: otherWords(), Topic: "guest-two"}))
	seen := j.seen[len(j.seen)-1]
	if strings.Contains(seen, hidden.ID) || !strings.Contains(seen, own.ID) {
		t.Fatalf("the guest's judge was shown: %q", seen)
	}
	if g.SameAs != "" || !strings.Contains(g.Note, "held") {
		t.Fatalf("a repeat of a card the guest may not read: %+v", g)
	}
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, guest, g.ID)); got.SameAs != "" || got.Card.Question == hidden.Card.Question {
		t.Fatalf("judged again, it took the hidden card: %+v", got)
	}
}

// A repeat is joined only for the same person, and never past a status
// report or a ruling; once an action card's first ask is withdrawn, its
// joined asks are answered on their own.
func TestRepeatLimits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	j := &switchJudge{answer: judgePass}
	h.svc.SetJudge(j)
	agentA, _ := h.session("a5a5a5a5")
	agentB, _ := h.session("b5b5b5b5")
	work := func(title string) orchapi.AskSubject { return orchapi.AskSubject{Work: h.workItem(title).ID} }
	first := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: work("limits one"), Card: card()}))
	j.set(judgeRepeats(first.ID))
	named := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("limits two"), Card: otherWords(), Owner: "ivo"}))
	if named.SameAs != "" || named.Owner != "ivo" || !strings.Contains(named.Note, "linked") {
		t.Fatalf("a card for one person joined a card for the owners: %+v", named)
	}
	checked := otherWords()
	checked.Verify = &orchapi.AskVerify{Command: "test -f /tmp/swaps-kept"}
	if v := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("limits verify"), Card: checked})); v.SameAs != "" || v.Card.Verify == nil {
		t.Fatalf("a card with its own verify check joined and lost it: %+v", v)
	}
	j.set(strings.Replace(judgeRepeats(first.ID), `"asks_decision":true`, `"asks_decision":false`, 1))
	if _, err := orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("limits three"), Card: otherWords()}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "status") {
		t.Fatalf("a status report that repeats a card: %v", err)
	}

	j.set(judgePass)
	act := card()
	act.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: "1. Switch the shelf on.\n2. Run the check."}
	act.Verify = &orchapi.AskVerify{Command: "test -f /tmp/done"}
	root := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: work("limits four"), Card: act}))
	j.set(judgeRepeats(root.ID))
	joined := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("limits five"), Card: otherWords()}))
	if joined.SameAs != root.ID || !strings.Contains(joined.Note, "carries an action") {
		t.Fatalf("joined an action card: %+v", joined)
	}
	must(orchclient.WithdrawAsk(ctx, agentA, root.ID))
	if d, err := orchclient.Choose(ctx, h.owner, "", joined.ID, orchapi.DecideRequest{Decision: "b"}); err != nil || d.Decision != "b" {
		t.Fatalf("a joined ask of a withdrawn action card: %+v %v", d, err)
	}
}
