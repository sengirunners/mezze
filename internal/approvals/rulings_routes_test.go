package approvals_test

// zm project ruling add|rm|ls: an owner changes the
// project's rulings at its authority, on the strength of an approved card,
// and the card judge reads the change at once.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestRulingAddNeedsAnOwnersSignedYesOnTheExactText(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	agent, _ := h.session("61616161")
	person := h.ownerIn("harbor", "")
	const text = "A reviewer agent's clean pass is enough to land a change; nobody asks the owners to review it."
	proposal := func(topic string) orchapi.Ask {
		c := card()
		c.Question = "Add this standing ruling?"
		c.Choices = []orchapi.AskChoice{{Label: "Yes, add it"}, {Label: "No"}}
		c.Context = "Agents keep asking you to review changes. The ruling, word for word: name \"review-by-agent\", text \"" + text + "\""
		return must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c, Topic: topic}))
	}
	add := func(name, text, ask string) (orchapi.RulingList, error) {
		return orchclient.AddRuling(ctx, person, orchapi.RulingAddRequest{Name: name, Text: text, Ask: ask})
	}

	yes := proposal("ruling-yes")
	if _, err := add("review-by-agent", text, yes.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("unanswered: %v", err)
	}
	must(orchclient.Decide(ctx, h.device(ipPhone), yes.ID, "a"))

	// Refused: no ask, other words, a missing ask, a guest.
	if _, err := add("review-by-agent", text, ""); code(err) != api.CodeBadRequest {
		t.Fatalf("no ask: %v", err)
	}
	for name, r := range map[string][2]string{
		"more words":       {"review-by-agent", text + " Always."},
		"part of the text": {"review-by-agent", "nobody asks the owners to review it."},
		"another name":     {"owners-review", text},
	} {
		if _, err := add(r[0], r[1], yes.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "does not propose") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := add("review-by-agent", text, "01ZZZZZZZZZZZZZZZZZZZZZZZZ"); code(err) != api.CodeNotFound {
		t.Fatalf("missing ask: %v", err)
	}
	if _, err := orchclient.AddRuling(ctx, h.guest(), orchapi.RulingAddRequest{Name: "review-by-agent", Text: text, Ask: yes.ID}); code(err) != api.CodeForbidden {
		t.Fatalf("a guest added a ruling: %v", err)
	}
	if _, err := orchclient.Rulings(ctx, h.guest()); code(err) != api.CodeForbidden {
		t.Fatalf("a guest listed rulings: %v", err)
	}

	// The other choice, and an unsigned answer from the socket, add nothing.
	no := proposal("ruling-no")
	must(orchclient.Decide(ctx, h.device(ipPhone), no.ID, "b"))
	if _, err := add("review-by-agent", text, no.ID); code(err) != api.CodeConflict {
		t.Fatalf("answered no: %v", err)
	}
	sock := proposal("ruling-socket")
	must(orchclient.Choose(ctx, h.ownerIn("harbor", ""), "harbor", sock.ID, orchapi.DecideRequest{Decision: "a"}))
	if _, err := add("review-by-agent", text, sock.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("socket answer: %v", err)
	}

	// The owner adds it; it is recorded with its ask, in the file too.
	l := must(add("review-by-agent", text, strings.ToLower(yes.ID)))
	if l.Project != "harbor" || len(l.Rulings) != 1 || l.Rulings[0] != (orchapi.Ruling{Name: "review-by-agent", Text: text, Ask: yes.ID}) {
		t.Fatalf("after add: %+v", l)
	}
	if got := must(orchclient.Rulings(ctx, agent)); len(got.Rulings) != 1 || got.Rulings[0].Ask != yes.ID {
		t.Fatalf("an agent reads: %+v", got)
	}
	toml := must(os.ReadFile(filepath.Join(h.dm.Deps.Config.Home, "projects", h.project.ID, "project.toml")))
	if !strings.Contains(string(toml), "[[rulings]]") || !strings.Contains(string(toml), `ask = "`+yes.ID+`"`) {
		t.Fatalf("project.toml:\n%s", toml)
	}
	if _, err := add("review-by-agent", text, yes.ID); code(err) != api.CodeConflict {
		t.Fatalf("the same name twice: %v", err)
	}

	// The judge reads it on the next card, with no restart, and is never
	// shown where it came from.
	var mu sync.Mutex
	var seen string
	h.svc.SetJudge(judge.Fake{Fn: func(prompt string) (json.RawMessage, error) {
		mu.Lock()
		seen = prompt
		mu.Unlock()
		return h.judge.answer(prompt)
	}})
	next := card()
	next.Question = "Should we drop last year's swaps?"
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: next, Topic: "after-ruling"}))
	mu.Lock()
	if !strings.Contains(seen, "review-by-agent") || !strings.Contains(seen, "nobody asks the owners") || strings.Contains(seen, yes.ID) {
		t.Errorf("the judge's input after the add: %s", seen)
	}
	mu.Unlock()

	// Removing: an agent may not, the owner may; a missing name is not_found.
	if _, err := orchclient.RemoveRuling(ctx, agent, "review-by-agent"); code(err) != api.CodeForbidden {
		t.Fatalf("an agent removed a ruling: %v", err)
	}
	if _, err := orchclient.RemoveRuling(ctx, person, "no-such-ruling"); code(err) != api.CodeNotFound {
		t.Fatalf("missing ruling: %v", err)
	}
	if l := must(orchclient.RemoveRuling(ctx, person, "review-by-agent")); len(l.Rulings) != 0 {
		t.Fatalf("after rm: %+v", l)
	}
	toml = must(os.ReadFile(filepath.Join(h.dm.Deps.Config.Home, "projects", h.project.ID, "project.toml")))
	if strings.Contains(string(toml), "[[rulings]]") {
		t.Fatalf("project.toml after rm:\n%s", toml)
	}
}

func TestRulingAddNeedsAnOwnerOrAdmin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "pat"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "pat", Role: "member", Project: "harbor"}))
	add := must(orchclient.AddApproverFor(ctx, h.owner, "pat-phone", "pat"))
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	agent, _ := h.session("71717171")
	c := card()
	const text = "Keep last year's swaps."
	c.Context += ` The ruling: "keep-swaps", "` + text + `"`
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c, Topic: "member-yes"}))
	if _, err := orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"); err != nil {
		t.Fatalf("a member's device decides here: %v", err)
	}
	if _, err := orchclient.AddRuling(ctx, h.ownerIn("harbor", ""), orchapi.RulingAddRequest{Name: "keep-swaps", Text: text, Ask: a.ID}); code(err) != api.CodeConflict ||
		!strings.Contains(err.Error(), "own or administer") {
		t.Fatalf("a member's yes: %v", err)
	}
}

// The owner's signed ask is the authority: an agent session of the whole
// project carries out a ruling written into the approved choice's own action
// (owners never type commands). A ruling elsewhere on the card, any other
// ask, or the same yes a second time adds nothing, and an agent never
// removes one.
func TestRulingAddByAnAgentCarryingASignedAsk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	s := must(orchclient.Register(ctx, h.ownerIn("harbor", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "81818181-uuid"}))
	agent := h.ownerIn("harbor", s.Address)
	person := h.ownerIn("harbor", "")
	it := h.workItem("standing rulings")
	const text = "Either owner's answer covers both owners."
	table := `[[rulings]]` + "\nname = \"either-answers\"\ntext = \"" + text + "\"\n"
	proposal := func(topic string, inBrief bool) orchapi.Ask {
		c := card()
		c.Question = "Add this standing ruling?"
		c.Choices = []orchapi.AskChoice{{Label: "Yes, add it"}, {Label: "No"}}
		brief := "1. Add this ruling to the project's settings.\n2. Report what changed."
		if inBrief {
			brief = "1. Add this ruling to the project's settings:\n" + table + "2. Report what changed."
		} else {
			c.Background = `The ruling: name "either-answers", text "` + text + `"`
		}
		c.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief}
		c.Verify = &orchapi.AskVerify{Command: "zm project ruling ls"}
		return must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: c, Topic: topic}))
	}
	add := func(c *client.Client, name, ask string) (orchapi.RulingList, error) {
		return orchclient.AddRuling(ctx, c, orchapi.RulingAddRequest{Name: name, Text: text, Ask: ask})
	}

	// No ask, an unanswered one, a no: refused.
	if _, err := add(agent, "either-answers", ""); code(err) != api.CodeBadRequest {
		t.Fatalf("no ask: %v", err)
	}
	open := proposal("agent-open", true)
	if _, err := add(agent, "either-answers", open.ID); code(err) != api.CodeConflict {
		t.Fatalf("unanswered: %v", err)
	}
	no := proposal("agent-no", true)
	must(orchclient.Decide(ctx, h.device(ipPhone), no.ID, "b"))
	if _, err := add(agent, "either-answers", no.ID); code(err) != api.CodeConflict {
		t.Fatalf("answered no: %v", err)
	}
	// A yes whose ruling sits elsewhere on the card is not the agent's to
	// carry out (the owner may still add it).
	beside := proposal("agent-beside", false)
	must(orchclient.Decide(ctx, h.device(ipPhone), beside.ID, "a"))
	if _, err := add(agent, "either-answers", beside.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "approved action") {
		t.Fatalf("a ruling beside the action: %v", err)
	}
	yes := proposal("agent-yes", true)
	must(orchclient.Decide(ctx, h.device(ipPhone), yes.ID, "a"))
	if _, err := add(agent, "other-name", yes.ID); code(err) != api.CodeConflict {
		t.Fatalf("a name the card does not propose: %v", err)
	}
	// A token of a whole-project guest operates on nothing: refused.
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Role: "guest"}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token, guest.Project, guest.ProjectFrom = g.Secret, "harbor", "env"
	if _, err := add(guest, "either-answers", yes.ID); code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "member of all of") {
		t.Fatalf("a whole-project guest: %v", err)
	}

	// With the signed yes on its action, the agent adds it, recorded with its ask.
	l := must(add(agent, "either-answers", yes.ID))
	if len(l.Rulings) != 1 || l.Rulings[0] != (orchapi.Ruling{Name: "either-answers", Text: text, Ask: yes.ID}) {
		t.Fatalf("after the agent's add: %+v", l)
	}
	// A removal stays a person's; once removed, the old yes never brings it back.
	if _, err := orchclient.RemoveRuling(ctx, agent, "either-answers"); code(err) != api.CodeForbidden {
		t.Fatalf("an agent removed a ruling: %v", err)
	}
	must(orchclient.RemoveRuling(ctx, person, "either-answers"))
	if _, err := add(agent, "either-answers", yes.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "once") {
		t.Fatalf("the same yes again: %v", err)
	}
	// A refused add does not use the yes up: the beside card stays the owner's to add.
	if l := must(add(person, "either-answers", beside.ID)); len(l.Rulings) != 1 {
		t.Fatalf("the owner's add: %+v", l)
	}
	// A member of the whole project carries out a new yes too.
	must(orchclient.RemoveRuling(ctx, person, "either-answers"))
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "pat"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "pat", Role: "member", Project: "harbor"}))
	mt := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "pat", Project: "harbor", Role: "member"}))
	member := client.WithHandler(h.dm.Handler)
	member.Token, member.Project, member.ProjectFrom = mt.Secret, "harbor", "env"
	again := proposal("agent-again", true)
	must(orchclient.Decide(ctx, h.device(ipPhone), again.ID, "a"))
	if l := must(add(member, "either-answers", again.ID)); len(l.Rulings) != 1 || l.Rulings[0].Ask != again.ID {
		t.Fatalf("a member's add: %+v", l)
	}
	if _, err := orchclient.RemoveRuling(ctx, member, "either-answers"); code(err) != api.CodeForbidden {
		t.Fatalf("a member removed a ruling: %v", err)
	}
}
