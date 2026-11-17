package approvals_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// a card is its person's to decide (the person it is addressed to,
// else the human whose session asked). Another owner's answer stands, as
// the owners' standing rule allows, but the delivered answer says so; a
// device paired for no one answers as no one where several people answer.
func TestAnswerFromSomeoneElseIsLabelled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	add := must(orchclient.AddApprover(ctx, h.owner, "spare-phone")) // paired for no one
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	agent, _ := h.session("7c7c7c7c")

	// answer files a card (owner "" = every owner) on a fresh work item,
	// has decide answer it, and returns the comment the answer left there,
	// which is also the asker's mail.
	asker := agent
	answer := func(topic, owner string, decide func(id string) error) (string, orchapi.Ask) {
		t.Helper()
		it := h.workItem(topic)
		a := must(orchclient.NewAsk(ctx, asker, orchapi.NewAskRequest{Card: colorCard(), Topic: topic, Owner: owner, Subject: orchapi.AskSubject{Work: it.ID}}))
		if err := decide(a.ID); err != nil {
			t.Fatalf("%s: %v", topic, err)
		}
		cs := h.comments(it.ID)
		if len(cs) != 1 {
			t.Fatalf("%s: comments %q", topic, cs)
		}
		got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
		// The decision names whose answer it was not exactly when the mail says so.
		want := ""
		if strings.Contains(cs[0], "BY ANOTHER OWNER") || strings.Contains(cs[0], "PAIRED FOR NO ONE") {
			want = "wren"
		}
		if got.Decision == nil || got.Decision.NotTheirs != want {
			t.Fatalf("%s: not_theirs %+v, want %q; body %s", topic, got.Decision, want, cs[0])
		}
		return cs[0], got
	}
	by := func(who string) func(string) error {
		c := map[string]func(string) error{
			"wren": func(id string) error {
				_, err := orchclient.Choose(ctx, wren, "", id, orchapi.DecideRequest{Decision: "a"})
				return err
			},
			"tobi": func(id string) error {
				_, err := orchclient.Choose(ctx, tobi, "", id, orchapi.DecideRequest{Decision: "a"})
				return err
			},
			"phone": func(id string) error { _, err := orchclient.Decide(ctx, h.device(ipPhone), id, "a"); return err },
		}
		return c[who]
	}

	// Their own answer carries no label.
	if body, _ := answer("own", "wren", by("wren")); strings.Contains(body, "ANSWERED") {
		t.Fatalf("wren answered wren's card, yet: %s", body)
	}
	// Another owner's answer stands, labelled with both names.
	body, got := answer("other", "wren", by("tobi"))
	if got.Decision == nil || got.Decision.Approver != "tobi" {
		t.Fatalf("another owner's answer must stand: %+v", got.Decision)
	}
	if !strings.Contains(body, "ANSWERED BY ANOTHER OWNER: this card was for wren; tobi answered it, not wren.") ||
		!strings.Contains(body, "until wren agrees") {
		t.Fatalf("another owner's answer is not labelled: %s", body)
	}
	// A device paired for no one, where several people answer, answers as
	// no one: labelled, and no person is claimed for it.
	body, got = answer("phone", "wren", by("phone"))
	if !strings.Contains(body, "ANSWERED ON A DEVICE PAIRED FOR NO ONE: this card was for wren;") || got.Decision.ApproverPerson != "" {
		t.Fatalf("an unbound phone's answer: person %q, body %s", got.Decision.ApproverPerson, body)
	}
	// A card for every owner from a session at this box, where several
	// people answer, is no one person's (anyone at the box asks as its
	// account): no label, and never "hold until <the box> agrees".
	if body, _ := answer("box-owners", "", by("tobi")); strings.Contains(body, "ANSWERED") {
		t.Fatalf("an owners card from the box's own account: %s", body)
	}
	// A card for every owner is the asker's human's when they answer the
	// project: another owner's answer is labelled, as is an unbound phone's.
	asker = wren
	if body, _ := answer("owners", "", by("tobi")); !strings.Contains(body, "this card went to every owner, and wren started your session; tobi answered it, not wren.") {
		t.Fatalf("an owners card answered by another owner: %s", body)
	}
	if body, _ := answer("owners-own", "", by("wren")); strings.Contains(body, "ANSWERED") {
		t.Fatalf("the asker's own human answered: %s", body)
	}
	// On a card for every owner the person is the asker's human, which an
	// area member is not shown.
	inArea := must(orchclient.NewAsk(ctx, wren, orchapi.NewAskRequest{Card: colorCard(), Topic: "in-area", Area: "solver"}))
	must(orchclient.Choose(ctx, tobi, "", inArea.ID, orchapi.DecideRequest{Decision: "a"}))
	ana := h.tokenClient(api.MintTokenRequest{As: "ana", Project: "harbor", Area: "solver", Role: "member"})
	if got := must(orchclient.GetAsk(ctx, ana, inArea.ID)); got.Decision == nil || got.Decision.NotTheirs != "" {
		t.Fatalf("an area member sees whose answer it was not: %+v", got.Decision)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, inArea.ID)); got.Decision.NotTheirs != "wren" {
		t.Fatalf("the owner reads not_theirs %q", got.Decision.NotTheirs)
	}
	body, got = answer("phone-owners", "", by("phone"))
	if !strings.Contains(body, "ANSWERED ON A DEVICE PAIRED FOR NO ONE: this card went to every owner, and wren") || got.Decision.ApproverPerson != "" {
		t.Fatalf("an unbound phone on wren's owners card: person %q, body %s", got.Decision.ApproverPerson, body)
	}
}

// Where the machine's owner is the one person who answers the project, a
// device paired for no one is theirs, as before: no label.
func TestSoleOwnersUnboundPhoneIsTheirs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("spare-phone", ipPhone)
	agent, _ := h.session("7d7d7d7d")
	it := h.workItem("solo")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "solo", Subject: orchapi.AskSubject{Work: it.ID}}))
	must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if cs := h.comments(it.ID); len(cs) != 1 || strings.Contains(cs[0], "ANSWERED") {
		t.Fatalf("the sole owner's phone: %q", cs)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision.ApproverPerson == "" {
		t.Fatal("the sole owner's phone claims no person")
	}
}
