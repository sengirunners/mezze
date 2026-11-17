package approvals_test

// What zm measures reads that no other record holds: a
// judge's labels on a card, an owner's objection after the answer, and paging
// past the 200-card list.

import (
	"context"
	"fmt"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestLabelsAreRecordedAndShown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("a thing to decide")
	agent, _ := h.session("aaaaaaaa")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "topic-one", Card: card()}))
	if a.Labels != nil {
		t.Fatalf("a new card has labels %v", a.Labels)
	}
	if err := h.svc.SetLabels(ctx, a.ID, map[string]string{orchapi.LabelKind: "demo-kind", "other": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SetLabels(ctx, a.ID, map[string]string{"other": ""}); err != nil { // an empty value removes the key
		t.Fatal(err)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if len(got.Labels) != 1 || got.Labels[orchapi.LabelKind] != "demo-kind" {
		t.Fatalf("labels %v", got.Labels)
	}
	for _, bad := range []map[string]string{{"Bad Key": "x"}, {"kind": fmt.Sprintf("%0100d", 1)}} {
		if err := h.svc.SetLabels(ctx, a.ID, bad); code(err) != api.CodeBadRequest {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if err := h.svc.SetLabels(ctx, "01NOSUCHASK00000000000000", map[string]string{"kind": "x"}); code(err) != api.CodeNotFound {
		t.Errorf("missing ask: %v", err)
	}
}

func TestObjectionRecordsAnOwnersLaterPress(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("a thing to decide")
	agent, _ := h.session("aaaaaaaa")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "topic-one", Card: card()}))

	// Nothing to object to before the answer.
	if _, err := orchclient.ObjectAsk(ctx, h.owner, a.ID, "too soon"); code(err) != api.CodeConflict {
		t.Fatalf("object before the answer: %v", err)
	}
	if a.Next != orchapi.NextYou {
		t.Fatalf("an open card is the person's turn, got next %q", a.Next)
	}
	must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"}))
	if chosen := must(orchclient.GetAsk(ctx, h.owner, a.ID)); chosen.Next != orchapi.NextNobody {
		t.Fatalf("an answer with nothing tracked is nobody's turn, got next %q", chosen.Next)
	}
	// An agent session on the owner's machine is not an owner, and neither is a member who may
	// operate but does not own the project.
	if _, err := orchclient.ObjectAsk(ctx, agent, a.ID, "no"); code(err) != api.CodeForbidden {
		t.Fatalf("a session recorded an objection: %v", err)
	}
	m := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "wren", Project: "harbor", Area: "solver", Role: "member"}))
	member := client.WithHandler(h.dm.Handler)
	member.Token, member.Project, member.ProjectFrom = m.Secret, "harbor", "env"
	if _, err := orchclient.ObjectAsk(ctx, member, a.ID, "no"); err == nil {
		t.Fatal("a member recorded an owner's objection")
	}
	// A guest is not an owner.
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token, guest.Project, guest.ProjectFrom = g.Secret, "harbor", "env"
	if _, err := orchclient.ObjectAsk(ctx, guest, a.ID, "no"); err == nil {
		t.Fatal("a guest recorded an owner's objection")
	}
	got := must(orchclient.ObjectAsk(ctx, h.owner, a.ID, "the lane should not have started"))
	if got.Objection == nil || got.Objection.Note != "the lane should not have started" || got.Objection.By == "" || got.Objection.At.IsZero() {
		t.Fatalf("objection %+v", got.Objection)
	}
	// The first press stands.
	again := must(orchclient.ObjectAsk(ctx, h.owner, a.ID, "a different note"))
	if again.Objection.Note != got.Objection.Note {
		t.Fatalf("second press replaced the first: %+v", again.Objection)
	}
	// It shows wherever the card does.
	if shown := must(orchclient.GetAsk(ctx, h.owner, a.ID)); shown.Objection == nil {
		t.Fatal("GetAsk lost the objection")
	}
	long := make([]byte, orchapi.MaxObjectionNote+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := orchclient.ObjectAsk(ctx, h.owner, a.ID, string(long)); code(err) != api.CodeBadRequest {
		t.Fatalf("a note past the limit: %v", err)
	}
}

func TestAskListPagesPastTheDefaultCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("a thing to decide")
	agent, _ := h.session("aaaaaaaa")
	const n = 7
	for i := 0; i < n; i++ {
		must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: fmt.Sprintf("topic-%03d", i), Card: card()}))
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, next, err := orchclient.AskPage(ctx, h.owner, "", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, a := range page {
			if seen[a.ID] {
				t.Fatalf("%s listed twice", a.ID)
			}
			seen[a.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("paging does not end")
		}
	}
	if len(seen) != n || pages < 3 {
		t.Fatalf("%d asks over %d pages, want %d over at least 3", len(seen), pages, n)
	}
	// The unpaged call is what it was: everything up to the cap, no cursor.
	all, next, err := orchclient.AskPage(ctx, h.owner, "", "", 0)
	if err != nil || len(all) != n || next != "" {
		t.Fatalf("unpaged: %d asks, next %q, %v", len(all), next, err)
	}
}
