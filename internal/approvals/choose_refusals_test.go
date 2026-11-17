package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// POST /v0/asks/{id}/choose takes the card text the person read: a
// card_sha256 that is not the ask's is a 409 (reason card-changed) and records
// nothing; the ask's own, or none, records.
func TestChooseCardSHA(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("c1c1c1c1")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("which colour").ID}, Card: card()}))
	if a.CardSHA == "" {
		t.Fatal("ask has no card_sha256")
	}
	_, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a", CardSHA: "0000"})
	if !api.IsCode(err, api.CodeConflict) || refusalReason(err) != orchapi.ReasonCardChanged {
		t.Fatalf("another card: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
		t.Fatalf("recorded on a card the person did not read: %+v", got.Decision)
	}
	// A choice the card no longer has, from a stale reader, is still "the card changed".
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "gone", CardSHA: "0000"}); refusalReason(err) != orchapi.ReasonCardChanged {
		t.Fatalf("stale reader, choice gone: %v", err)
	}
	// Uppercase or padded hex of the same card is the same card.
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "gone", CardSHA: " " + strings.ToUpper(a.CardSHA) + " "}); api.IsCode(err, api.CodeConflict) {
		t.Fatalf("same card, other spelling: %v", err)
	}
	if d := must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a", CardSHA: a.CardSHA})); d.Decision != "a" {
		t.Fatalf("its own card: %+v", d)
	}
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("which size").ID}, Card: card()}))
	if d := must(orchclient.Choose(ctx, h.owner, "", b.ID, orchapi.DecideRequest{Decision: "b"})); d.Decision != "b" {
		t.Fatalf("no card_sha256: %+v", d)
	}
}

// Every other 409 of choose says its reason, except the one whose data is
// the answer that stands.
func TestChooseRefusalsSayWhy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("c2c2c2c2")

	w := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("to withdraw").ID}, Card: card()}))
	must(orchclient.WithdrawAsk(ctx, agent, w.ID))
	if _, err := orchclient.Choose(ctx, h.owner, "", w.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeConflict) || refusalReason(err) != orchapi.ReasonWithdrawn {
		t.Fatalf("withdrawn: %v", err)
	}

	d := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("answered").ID}, Card: card()}))
	must(orchclient.Choose(ctx, h.owner, "", d.ID, orchapi.DecideRequest{Decision: "a"}))
	_, err := orchclient.Choose(ctx, h.owner, "", d.ID, orchapi.DecideRequest{Decision: "b"})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict {
		t.Fatalf("already answered: %v", err)
	}
	var stands orchapi.Decision
	if json.Unmarshal(e.Data, &stands) != nil || stands.Ask != d.ID || stands.Decision != "a" {
		t.Fatalf("already answered: data is the standing decision, got %s", e.Data)
	}

	a, _, _ := h.defaultAsk("pick a runner", ciCard(), time.Minute)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext, Text: "which one?"}); !api.IsCode(err, api.CodeConflict) || refusalReason(err) != orchapi.ReasonDefaultStands {
		t.Fatalf("default stands: %v", err)
	}
}

// card_sha256 names the card on the ask answered: a joined ask's card hashes
// differently (it carries no verify check), and its answer still lands.
func TestChooseCardSHAReachesJoinedAsks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a1c, _ := h.session("c3c3c3c3")
	a2c, _ := h.session("c4c4c4c4")
	c := card()
	c.Verify = &orchapi.AskVerify{Command: "test -f /tmp/done"}
	first := must(orchclient.NewAsk(ctx, a1c, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("first", "owner:ivo").ID}, Card: c, Topic: "pick"}))
	joined := must(orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID, Subject: orchapi.AskSubject{Work: h.workItem("second").ID}}))
	if joined.CardSHA == first.CardSHA {
		t.Fatal("test needs a joined card that hashes differently")
	}
	must(orchclient.Choose(ctx, h.owner, "", first.ID, orchapi.DecideRequest{Decision: "a", CardSHA: first.CardSHA}))
	if got := must(orchclient.GetAsk(ctx, h.owner, joined.ID)); got.Decision == nil || got.Decision.Decision != "a" {
		t.Fatalf("the joined ask was not answered: %+v", got.Decision)
	}
}

// A joined ask whose first ask carries an open action is answered on the
// first: 409 joined-action-ask.
func TestChooseJoinedActionAskSaysWhy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a1c, _ := h.session("c5c5c5c5")
	a2c, _ := h.session("c6c6c6c6")
	first := must(orchclient.NewAsk(ctx, a1c, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("act", "owner:ivo").ID}, Card: handCard(""), Topic: "act"}))
	joined := must(orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID, Subject: orchapi.AskSubject{Work: h.workItem("joined").ID}}))
	if _, err := orchclient.Choose(ctx, h.owner, "", joined.ID, orchapi.DecideRequest{Decision: "b"}); !api.IsCode(err, api.CodeConflict) || refusalReason(err) != orchapi.ReasonJoinedAction {
		t.Fatalf("joined to an action ask: %v", err)
	}
}
