package approvals_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// SPEC 16 "Repeating cards": an automation that is still
// broken counts on its one open card (seen, last seen, latest) instead of
// filing another; once that card is withdrawn or answered, the next failure
// files a fresh one.
func TestRepeatKeyCountsOnTheOpenCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("Automation: nightly-export")
	agent, _ := h.session("abababab")
	ask := func(key, latest string) (orchapi.Ask, error) {
		return orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(),
			Topic: "automation-fix", RepeatKey: key, Latest: latest})
	}
	first := must(ask("automation:job-one", "the export could not sign in"))
	if first.Updated || first.Repeat == nil || first.Repeat.Seen != 1 || first.Repeat.Latest != "the export could not sign in" {
		t.Fatalf("first: %+v", first)
	}
	h.clock.add(3 * 60e9)
	second := must(ask("automation:job-one", "the export timed out"))
	if !second.Updated || second.ID != first.ID || second.Repeat.Seen != 2 || second.Repeat.Latest != "the export timed out" ||
		!second.Repeat.LastSeen.After(first.Repeat.LastSeen) {
		t.Fatalf("a repeat while open: %+v", second)
	}
	third := must(ask("automation:job-one", ""))
	if third.ID != first.ID || third.Repeat.Seen != 3 || third.Repeat.Latest != "the export timed out" {
		t.Fatalf("a repeat without latest keeps the last one: %+v", third.Repeat)
	}
	if n := len(h.mailTo("owners", "last year's swaps")) + len(h.mailTo("human:ivo", "last year's swaps")); n != 1 {
		t.Fatalf("one broken job sent %d cards", n)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, first.ID)); got.Repeat == nil || got.Repeat.Seen != 3 {
		t.Fatalf("the owner reads: %+v", got.Repeat)
	}

	// Withdrawn: the next failure is a new card, counted from one.
	must(orchclient.WithdrawAsk(ctx, agent, first.ID))
	fresh := must(ask("automation:job-one", "still failing"))
	if fresh.Updated || fresh.ID == first.ID || fresh.Repeat.Seen != 1 {
		t.Fatalf("after withdrawal: %+v", fresh)
	}
	// Answered: the card's usual slot holds (one ask per work item and
	// topic, open or answered), so the same item and topic is 409 carrying
	// the answer; another item or topic files a fresh card.
	must(orchclient.Decide(ctx, h.device(ipPhone), fresh.ID, "a"))
	if _, err := ask("automation:job-one", "broke again"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("after an answer, same item and topic: %v", err)
	}
	other := h.workItem("Automation: other-job")
	again := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: other.ID}, Card: card(),
		Topic: "automation-fix", RepeatKey: "automation:job-one"}))
	if again.Updated || again.ID == fresh.ID || again.Repeat.Seen != 1 {
		t.Fatalf("after an answer, another item: %+v", again)
	}
	// Another key is another card.
	plain := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "job-two", RepeatKey: "automation:job-two"}))
	if plain.Updated || plain.ID == again.ID {
		t.Fatalf("another key: %+v", plain)
	}
}

func TestRepeatKeyRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("acacacac")
	for name, req := range map[string]orchapi.NewAskRequest{
		"bad key":            {Card: card(), RepeatKey: "a b"},
		"latest alone":       {Card: card(), Latest: "it broke"},
		"latest with a path": {Card: card(), RepeatKey: "automation:x1", Latest: "failed in /srv/app/deploy.sh"},
		"latest two lines":   {Card: card(), RepeatKey: "automation:x1", Latest: "one\ntwo"},
		"no card":            {Subject: orchapi.AskSubject{Repo: "acme/widgets", PR: 1, HeadSHA: "0123456789abcdef0123456789abcdef01234567"}, RepeatKey: "automation:x1"},
	} {
		if _, err := orchclient.NewAsk(ctx, agent, req); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A guest's key is its own: an operator's open card with the same key, in
// the same area, is not counted on by a guest, nor the guest's by the
// operator.
func TestRepeatKeyNamespace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	op := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: card(), Topic: "ns-op", RepeatKey: "automation:shared"}))
	g := must(orchclient.NewAsk(ctx, h.guest(), orchapi.NewAskRequest{Card: card(), Topic: "ns-guest", RepeatKey: "automation:shared"}))
	if g.Updated || g.ID == op.ID {
		t.Fatalf("a guest counted on an operator's card: %+v", g)
	}
	op2 := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: card(), Topic: "ns-op2", RepeatKey: "automation:shared"}))
	if !op2.Updated || op2.ID != op.ID {
		t.Fatalf("the operator's repeat: %+v", op2)
	}
}

// A card the judge holds is its asker's alone: another agent's repeat is not
// counted on it (nor shown it), and a held card's own asker still counts.
func TestRepeatKeyHeldCardIsTheAskers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{} // no answer: held
	h.svc.SetJudge(j)
	a, _ := h.session("a1a1a1a1")
	b, _ := h.session("b1b1b1b1")
	held := must(orchclient.NewAsk(ctx, a, orchapi.NewAskRequest{Card: card(), Topic: "held-a", RepeatKey: "automation:held-job"}))
	if again := must(orchclient.NewAsk(ctx, a, orchapi.NewAskRequest{Card: card(), Topic: "held-a2", RepeatKey: "automation:held-job"})); !again.Updated || again.ID != held.ID {
		t.Fatalf("its own asker's repeat: %+v", again)
	}
	other := must(orchclient.NewAsk(ctx, b, orchapi.NewAskRequest{Card: card(), Topic: "held-b", RepeatKey: "automation:held-job"}))
	if other.Updated || other.ID == held.ID {
		t.Fatalf("another agent was counted on a held card: %+v", other)
	}
}

// Two first failures at once file one card: new asks with one key are taken
// one at a time.
func TestRepeatKeyConcurrentFirstAsks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("c1c1c1c1")
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Go(func() {
			a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: fmt.Sprintf("burst-%d", i), RepeatKey: "automation:burst"})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = a.ID
		})
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("a burst filed several cards: %q", ids)
		}
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, ids[0])); got.Repeat == nil || got.Repeat.Seen != len(ids) {
		t.Fatalf("seen: %+v", got.Repeat)
	}
}
