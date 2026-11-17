package approvals_test

// Review of PR #316 (routing): who may join, what a join or a
// search shows, and what the owner's cascade decides.

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A guest of harbor/solver names an owner. Whether the name is a person
// anywhere at the authority must not show: GET /v0/identities is local only,
// and GET /v0/people hides whole-project members from an area guest.
func TestReview316OwnerProbeLeaksIdentities(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// secretbea holds a grant only in another project.
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "secretbea", Project: "zz-side", Role: "member"}))
	guest := h.guest()

	c1 := colorCard()
	_, errReal := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: c1, Topic: "probe-real", Owner: "secretbea"})
	_, errGhost := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: colorCard(), Topic: "probe-ghost", Owner: "nosuchperson"})
	if code(errReal) != code(errGhost) {
		t.Errorf("owner= is an oracle for identity names: real %q, ghost %q", code(errReal), code(errGhost))
	}

	// The default from a label the guest reads says the same.
	real := h.workItem("a", "owner:secretbea")
	ghost := h.workItem("b", "owner:nosuchperson")
	ar, err1 := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: real.ID}, Card: card()})
	ag, err2 := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: ghost.ID}, Card: card()})
	if err1 == nil && err2 == nil && ar.Owner != ag.Owner {
		t.Errorf("the label default is an oracle: owner:secretbea -> %q, owner:nosuchperson -> %q", ar.Owner, ag.Owner)
	}
}

// SPEC: the owner "must be a person's name or alias". An agent identity is
// not a person.
func TestReview316OwnerIsAnAgentIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "ci-bot", Kind: api.IdentityAgent}))
	agent, _ := h.session("91919191")
	a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "agent-owner", Owner: "ci-bot"})
	if err == nil {
		t.Errorf("an agent identity owns an ask: %+v", a.Owner)
	}
}

// A guest joining another's ask, or a joined ask whose first it cannot read,
// gets exactly the 404 of a missing ask; ?q= never lists it.
func TestReview316GuestJoinHiddenLikeMissing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("92929292")
	v := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	guest := h.guest()
	const missing = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	_, eHidden := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{SameAs: v.ID})
	_, eMissing := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{SameAs: missing})
	if code(eHidden) != api.CodeNotFound || strings.ReplaceAll(eHidden.Error(), v.ID, "X") != strings.ReplaceAll(eMissing.Error(), missing, "X") {
		t.Fatalf("hidden %v, missing %v", eHidden, eMissing)
	}
	// Answered: the 409 and its data must not reach the guest.
	must(orchclient.Choose(ctx, h.owner, "", v.ID, orchapi.DecideRequest{Decision: "b"}))
	_, eHidden = orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{SameAs: v.ID})
	if code(eHidden) != api.CodeNotFound || strings.Contains(eHidden.Error(), "Drop") {
		t.Fatalf("answered, hidden: %v", eHidden)
	}
	if got := must(orchclient.SimilarAsks(ctx, guest, "", "keep swaps holidays")); len(got) != 0 {
		t.Fatalf("q lists a hidden ask: %+v", got)
	}

	// The guest's own ask joined to an ask it may read (its own) is fine; a
	// guest cannot reach another's ask through its own joined one.
	own := must(orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: colorCard(), Topic: "button-color"}))
	j := must(orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{SameAs: own.ID}))
	if j.SameAs != own.ID {
		t.Fatalf("own join: %+v", j)
	}
}

// Cross-project: an ask readable in one project cannot be joined from
// another, and the owner's answer never decides an ask of another project.
func TestReview316CrossProjectJoinRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := must(orchclient.NewAsk(ctx, h.ownerIn("harbor", ""), orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	if _, err := orchclient.NewAsk(ctx, h.ownerIn("zz-side", ""), orchapi.NewAskRequest{SameAs: v.ID}); code(err) != api.CodeBadRequest {
		t.Fatalf("cross-project join: %v", err)
	}
	for _, a := range must(orchclient.Asks(ctx, h.owner, "zz-side", false)) {
		t.Fatalf("a row was left in zz-side: %+v", a)
	}
}

// An outside party's ask (for) and a withdrawn first are never joined; a join
// cannot raise its priority or move its owner.
func TestReview316JoinCannotOverride(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member"}))
	agent, _ := h.session("93939393")
	v := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps", Priority: new(3)}))
	other, _ := h.session("94949494")
	if _, err := orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{SameAs: v.ID, Owner: "bea"}); code(err) != api.CodeBadRequest {
		t.Errorf("join moves the owner: %v", err)
	}
	j := must(orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{SameAs: v.ID, Priority: new(0)}))
	if j.Priority != 3 {
		t.Errorf("join raised priority: %d", j.Priority)
	}
	must(orchclient.WithdrawAsk(ctx, agent, v.ID))
	if _, err := orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{SameAs: j.ID}); code(err) != api.CodeConflict {
		t.Errorf("join through a withdrawn first: %v", err)
	}
}

// SPEC 16 Routing: the inbox lists a joined ask with same_as, so a client
// shows the group as one card.
func TestReview316InboxCarriesTheGroup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a1, _ := h.session("95959595")
	a2, _ := h.session("96969696")
	v := must(orchclient.NewAsk(ctx, a1, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	j := must(orchclient.NewAsk(ctx, a2, orchapi.NewAskRequest{SameAs: v.ID}))
	seen := map[string]string{}
	for _, x := range must(orchclient.Decisions(ctx, h.owner, "", "open")).Decisions {
		if x.Ask != nil {
			seen[x.Ask.ID] = x.Ask.SameAs
		}
	}
	if len(seen) != 2 || seen[v.ID] != "" || seen[j.ID] != v.ID {
		t.Errorf("inbox rows: %v", seen)
	}
}
