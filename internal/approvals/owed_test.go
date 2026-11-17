package approvals_test

// What a session still owes (SPEC 8), the cards' part: a card it filed that
// no one has answered, and one answered yes whose work is not done. A done,
// withdrawn or another session's card is not owed.

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

// Held cards are counted only for a reader who may read them: a guest sees
// the session's cards in scope but not a held card, so it is told nothing of
// it (no gap), while a project operator is told some are held.
func TestOwedHeldCardNotSaidToReaderWhoMayNotRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.SetJudge(&switchJudge{}) // the provider is down: every card is held
	it := h.workItem("held card")
	agent, addr := h.session("cccccccc")
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "held-two"}))
	who := orchcore.OwedWho{Sessions: []orchapi.Session{{Address: addr}}}
	heldGap := func(sub *authz.Subject) bool {
		part, err := h.svc.Owed(ctx, sub, who)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range part.Gaps {
			if g.Name == "asks" {
				return true
			}
		}
		return false
	}
	scope := authz.FromTuples([]authz.Tuple{{Subject: "viewer", Relation: authz.Guest, Object: authz.ProjectObject(h.project.ID)}}, nil)
	obj := authz.Object{Project: h.project.ID}
	if !scope.Can(authz.Read, authz.Object{Project: h.project.ID, Area: "solver"}) || !scope.Can(authz.Read, obj) || scope.Can(authz.Operate, obj) {
		t.Fatal("the guest scope must read but not operate")
	}
	if heldGap(&authz.Subject{Address: "human:vida", Scope: scope}) {
		t.Fatal("a reader who may not read the held card was told some are held")
	}
	if !heldGap(h.dm.Deps.Projects.Local("")) {
		t.Fatal("an operator is not told a card is held")
	}
}

func TestOwedCardsUnansweredAndNotDone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("make the app side live")
	agent, addr := h.session("aaaaaaaa")
	other, _ := h.session("bbbbbbbb")

	unanswered := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "first", Card: card()}))
	approved := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "go-live", Card: verifyCard("true")}))
	withdrawn := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "never-mind", Card: card()}))
	theirs := must(orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "theirs", Card: card()}))
	must(orchclient.WithdrawAsk(ctx, agent, withdrawn.ID))
	must(orchclient.DecideWith(ctx, h.device(ipPhone), approved.ID, orchapi.DecideRequest{Decision: "a"}))

	kinds := func(c orchclient.OwedQuery) map[string]string {
		t.Helper()
		l := must(orchclient.Owed(ctx, agent, c))
		out := map[string]string{}
		for _, x := range l.Items {
			if x.Kind == orchapi.OwedAskOpen || x.Kind == orchapi.OwedAskNotDone {
				out[x.Ref] = x.Kind
			}
		}
		return out
	}
	got := kinds(orchclient.OwedQuery{})
	if len(got) != 2 || got[unanswered.ID] != orchapi.OwedAskOpen || got[approved.ID] != orchapi.OwedAskNotDone {
		t.Fatalf("owed cards %v (unanswered %s, approved %s)", got, unanswered.ID, approved.ID)
	}
	if _, ok := got[theirs.ID]; ok {
		t.Fatalf("another session's card is owed: %v", got)
	}
	for _, x := range must(orchclient.Owed(ctx, agent, orchclient.OwedQuery{})).Items {
		if x.Ref == approved.ID && (x.Holder != addr || x.State == "") {
			t.Fatalf("not-done row: %+v", x)
		}
	}
	// All of the owner's sessions: both sessions' open cards.
	all := must(orchclient.Owed(ctx, h.owner, orchclient.OwedQuery{Person: "me"}))
	seen := map[string]bool{}
	for _, x := range all.Items {
		seen[x.Ref] = true
	}
	if !seen[unanswered.ID] || !seen[theirs.ID] || seen[withdrawn.ID] {
		t.Fatalf("owner's list: %+v", all.Items)
	}
	// Once the asker's check passes the card is done and no longer owed.
	must(orchclient.CheckAsk(ctx, agent, approved.ID, orchapi.CheckRequest{CardSHA: approved.CardSHA, Exit: exit(0)}))
	if got = kinds(orchclient.OwedQuery{}); len(got) != 1 || got[unanswered.ID] != orchapi.OwedAskOpen {
		t.Fatalf("after the check passed: %v", got)
	}
}

// A read capped short of the cards says so.
func TestOwedCardsCapIsUnknown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("make the app side live")
	agent, _ := h.session("aaaaaaaa")
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "one", Card: card()}))
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "two", Card: card()}))
	partial := func() bool {
		for _, g := range must(orchclient.Owed(ctx, agent, orchclient.OwedQuery{})).Unknown {
			if g.Kind == orchapi.OwedGapPartial && g.Name == "asks" {
				return true
			}
		}
		return false
	}
	if partial() {
		t.Fatal("an uncapped read is partial")
	}
	defer approvals.SetOwedAskCap(1)()
	if !partial() {
		t.Fatal("a capped read must be listed as unknown")
	}
}

// A card the judge holds is its asker's alone: the asker's list has it; the
// person's own list says some are held, and never what.
func TestOwedHeldCardIsSaidNotListed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.SetJudge(&switchJudge{}) // the provider is down: every card is held
	it := h.workItem("held card")
	agent, _ := h.session("bbbbbbbb")
	held := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "held-one"}))

	own := must(orchclient.Owed(ctx, agent, orchclient.OwedQuery{}))
	listed := false
	for _, x := range own.Items {
		listed = listed || x.Ref == held.ID
	}
	if !listed {
		t.Fatalf("the asker's own list lacks its held card: %+v", own.Items)
	}
	person := must(orchclient.Owed(ctx, h.owner, orchclient.OwedQuery{Person: "me"}))
	for _, x := range person.Items {
		if x.Ref == held.ID {
			t.Fatalf("a held card is listed to someone else: %+v", x)
		}
	}
	said := false
	for _, g := range person.Unknown {
		said = said || g.Kind == orchapi.OwedGapPartial && g.Name == "asks"
	}
	if !said {
		t.Fatalf("held cards are not said: %+v", person.Unknown)
	}
}
