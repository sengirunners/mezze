package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// handCard is a card whose choice A hands a brief to the approver's own
// agent (an action with no machine: its confirm word is the project's slug).
func handCard(risk string) *orchapi.AskCard {
	c := card()
	c.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: "1. Do the thing.\n2. Check it.", Risk: risk}
	c.Verify = &orchapi.AskVerify{Command: "test -f /tmp/done"}
	return c
}

func refusalReason(err error) string {
	var e *api.Error
	if !errors.As(err, &e) {
		return ""
	}
	var r orchapi.Refusal
	_ = json.Unmarshal(e.Data, &r)
	return r.Reason
}

// An action the asker marks irreversible takes its word typed in place:
// without it, or with another word, its choice is a 409 (reason
// confirm-word); with it the answer records. Other choices take no word.
func TestRiskyActionTakesTheWord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("eeeeeeee")
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("bad risk").ID}, Card: handCard("maybe")}); code(err) != api.CodeBadRequest {
		t.Fatalf("an unknown risk: %v", err)
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("risky").ID}, Card: handCard("irreversible")}))
	act := a.Card.Action
	if act.Risk != orchapi.RiskIrreversible || act.ConfirmWord != "harbor" || !strings.Contains(a.ActionText, "cannot be undone: to choose A, type harbor") {
		t.Fatalf("risky ask: %+v %q", act, a.ActionText)
	}
	phone := h.device(ipPhone)
	for _, word := range []string{"", "Harbor", "solver"} {
		_, err := orchclient.DecideWith(ctx, phone, a.ID, orchapi.DecideRequest{Decision: "a", Confirm: word})
		if code(err) != api.CodeConflict || refusalReason(err) != orchapi.ReasonConfirmWord {
			t.Fatalf("word %q: %v", word, err)
		}
	}
	if got := must(orchclient.GetAsk(ctx, agent, a.ID)); got.Decision != nil {
		t.Fatalf("decided without the word: %+v", got.Decision)
	}
	if d := must(orchclient.DecideWith(ctx, phone, a.ID, orchapi.DecideRequest{Decision: "a", Confirm: " harbor "})); d.Decision != "a" {
		t.Fatalf("with the word: %+v", d)
	}
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("risky b").ID}, Card: handCard("spending")}))
	if d := must(orchclient.DecideWith(ctx, phone, b.ID, orchapi.DecideRequest{Decision: "b"})); d.Decision != "b" {
		t.Fatalf("the other choice: %+v", d)
	}
	// A card with no risk is unchanged.
	c := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("plain").ID}, Card: handCard("")}))
	if c.Card.Action.Risk != "" || c.Card.Action.ConfirmWord != "" {
		t.Fatalf("plain ask: %+v", c.Card.Action)
	}
	must(orchclient.DecideWith(ctx, phone, c.ID, orchapi.DecideRequest{Decision: "a"}))
}

// The card judge may raise an action's risk, never lower it; a card
// released without its check is unchecked, and takes the word too.
func TestCardJudgeRaisesRisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("ffffffff")
	spending := strings.Replace(judgePass, `"risk":"none"`, `"risk":"spending"`, 1)
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(spending)})
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("raised").ID}, Card: handCard("")}))
	if a.Card.Action.Risk != orchapi.RiskSpending || a.Card.Action.ConfirmWord == "" {
		t.Fatalf("raised by the judge: %+v", a.Card.Action)
	}
	if got := must(orchclient.GetAsk(ctx, agent, a.ID)); got.CardSHA != a.CardSHA || got.Card.Action.Risk != orchapi.RiskSpending {
		t.Fatalf("stored: %+v (sha %s, made %s)", got.Card.Action, got.CardSHA, a.CardSHA)
	}
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(strings.Replace(judgePass, `"risk":"none"`, `"risk":"irreversible"`, 1))})
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("kept").ID}, Card: handCard("spending")}))
	if b.Card.Action.Risk != orchapi.RiskSpending {
		t.Fatalf("lowered by the judge: %+v", b.Card.Action)
	}
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(judgePass)})
	c := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("none").ID}, Card: handCard("irreversible")}))
	if c.Card.Action.Risk != orchapi.RiskIrreversible {
		t.Fatalf("a none from the judge lowered it: %+v", c.Card.Action)
	}
	// No answer from the judge: held; released unchecked, the risk is unknown.
	h.svc.SetJudge(judge.Fake{Err: errors.New("the provider is down")})
	held := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("held").ID}, Card: handCard("")}))
	phone := h.device(ipPhone)
	must(orchclient.ReleaseHeldOnDevice(ctx, phone, held.ID))
	got := must(orchclient.GetAsk(ctx, agent, held.ID))
	if got.Card.Action.Risk != orchapi.RiskUnchecked || got.Card.Action.ConfirmWord == "" || got.CardSHA == held.CardSHA {
		t.Fatalf("released unchecked: %+v", got.Card.Action)
	}
	if _, err := orchclient.DecideWith(ctx, phone, held.ID, orchapi.DecideRequest{Decision: "a"}); refusalReason(err) != orchapi.ReasonConfirmWord {
		t.Fatalf("an unchecked action without the word: %v", err)
	}
	must(orchclient.DecideWith(ctx, phone, held.ID, orchapi.DecideRequest{Decision: "a", Confirm: got.Card.Action.ConfirmWord}))
}

// A held card the judge passes later takes the risk it found before it is
// sent; one released unchecked while it was judged keeps unchecked.
func TestRejudgeRaisesRisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("abababab")
	h.svc.SetJudge(judge.Fake{Err: errors.New("the provider is down")})
	held := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("held then judged").ID}, Card: handCard("")}))
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(strings.Replace(judgePass, `"risk":"none"`, `"risk":"irreversible"`, 1))})
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	got := must(orchclient.GetAsk(ctx, agent, held.ID))
	if got.NotChecked || got.Card.Action.Risk != orchapi.RiskIrreversible || got.CardSHA == held.CardSHA {
		t.Fatalf("after the rejudge: not_checked %v %+v", got.NotChecked, got.Card.Action)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), held.ID, orchapi.DecideRequest{Decision: "a"}); refusalReason(err) != orchapi.ReasonConfirmWord {
		t.Fatalf("without the word: %v", err)
	}
}

// A repeat the judge flags never joins an open card with a lower risk: the
// raise counts before actions are compared, so it is linked, not lost.
func TestRaisedRepeatDoesNotJoinALowerRisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agentA, _ := h.session("c1c1c1c1")
	agentB, _ := h.session("d1d1d1d1")
	work := func(title string) orchapi.AskSubject { return orchapi.AskSubject{Work: h.workItem(title).ID} }
	root := must(orchclient.NewAsk(ctx, agentA, orchapi.NewAskRequest{Subject: work("first"), Card: handCard("")}))
	flagged := strings.Replace(judgeRepeats(root.ID), `"risk":"none"`, `"risk":"irreversible"`, 1)
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(flagged)})
	a := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("second"), Card: handCard("")}))
	if a.SameAs != "" || a.Card == nil || a.Card.Action.Risk != orchapi.RiskIrreversible {
		t.Fatalf("a flagged repeat: same_as %q card %+v", a.SameAs, a.Card)
	}
	// Control: the same repeat unflagged joins.
	h.svc.SetJudge(judge.Fake{Answer: json.RawMessage(judgeRepeats(root.ID))})
	b := must(orchclient.NewAsk(ctx, agentB, orchapi.NewAskRequest{Subject: work("third"), Card: handCard("")}))
	if b.SameAs != root.ID {
		t.Fatalf("an unflagged repeat did not join: %+v", b)
	}
}
