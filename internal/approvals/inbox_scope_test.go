package approvals_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// tokenClient is a client holding a token minted on the socket.
func (h *harness) tokenClient(req api.MintTokenRequest) *client.Client {
	h.t.Helper()
	tok := must(h.owner.MintToken(context.Background(), req))
	c := client.WithHandler(h.dm.Handler)
	c.Token, c.Project, c.ProjectFrom = tok.Secret, "harbor", "env"
	return c
}

// waiting is the work item IDs in c's open inbox.
func waiting(t *testing.T, c *client.Client) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, x := range must(orchclient.Decisions(context.Background(), c, "", "open")).Decisions {
		if x.Work != nil {
			out[x.Work.ID] = true
		}
	}
	return out
}

// The inbox counts the waits addressed to its reader: its names and
// aliases, ask:owner where it owns the project, and generic labels on items
// that name no one. Someone else's owner: or ask: does not count.
func TestInboxScopesToReader(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mine := h.workItem("mine", "owner:ivo")
	aliased := h.workItem("short name", "owner:jj")
	beas := h.workItem("bea's", "owner:bea")
	chiefs := h.workItem("the chiefs'", "ask:chiefs", "human")
	owners := h.workItem("an owner's", "ask:owner", "owner-run")
	generic := h.workItem("anyone's", "owner-run")
	beaRun := h.workItem("bea runs it", "owner:bea", "owner-run")
	needs := h.workItem("needs me", "needs-ivo")

	// Bea owns harbor by his own grant, but reads through a member token.
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "owner", Project: "harbor"}))
	bea := h.tokenClient(api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member", NoGrant: true})
	vida := h.tokenClient(api.MintTokenRequest{As: "vida", Project: "harbor", Role: "member"})

	check := func(who string, got map[string]bool, in, out []orchapi.WorkItem) {
		t.Helper()
		for _, it := range in {
			if !got[it.ID] {
				t.Errorf("%s: %q missing", who, it.Title)
			}
		}
		for _, it := range out {
			if got[it.ID] {
				t.Errorf("%s: %q listed", who, it.Title)
			}
		}
	}
	check("ivo", waiting(t, h.owner), []orchapi.WorkItem{mine, owners, generic, needs}, []orchapi.WorkItem{aliased, beas, chiefs, beaRun})
	check("bea", waiting(t, bea), []orchapi.WorkItem{beas, beaRun, owners, generic}, []orchapi.WorkItem{mine, aliased, chiefs, needs})
	check("vida", waiting(t, vida), []orchapi.WorkItem{generic}, []orchapi.WorkItem{mine, beas, owners, chiefs, beaRun})

	// An alias makes owner:jj Ivo's, exactly.
	if a := must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "jj"})); a.Identity != "ivo" || len(a.Aliases) != 1 || a.Aliases[0] != "jj" {
		t.Fatalf("alias: %+v", a)
	}
	check("ivo+jj", waiting(t, h.owner), []orchapi.WorkItem{mine, aliased}, []orchapi.WorkItem{beas})
	check("bea after jj", waiting(t, bea), nil, []orchapi.WorkItem{aliased})

	// Everyone's waits stay reachable.
	all := map[string]bool{}
	for _, x := range must(orchclient.DecisionsFor(ctx, h.owner, "", "open", "everyone")).Decisions {
		if x.Work != nil {
			all[x.Work.ID] = true
		}
	}
	check("everyone", all, []orchapi.WorkItem{mine, beas, chiefs, beaRun}, nil)
	if _, err := orchclient.DecisionsFor(ctx, h.owner, "", "open", "bea"); code(err) != api.CodeBadRequest {
		t.Errorf("for=bea: %v", err)
	}
}

// Aliases are exact, canonical, never a role word or someone else's name,
// and only the identity, the machine owner or a project admin manages them.
func TestInboxAliases(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	vida := h.tokenClient(api.MintTokenRequest{As: "vida", Project: "harbor", Role: "member"})
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))

	for raw, want := range map[string]string{
		"owner":  api.CodeBadRequest, // a role word
		"chiefs": api.CodeBadRequest,
		"Ivo":    api.CodeBadRequest, // not canonical
		"ivo":    api.CodeBadRequest, // already the name
		"bea":    api.CodeConflict,   // another identity
		"be.a":   api.CodeConflict,   // its lookalike
	} {
		if _, err := orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: raw}); code(err) != want {
			t.Errorf("alias %q: %v, want %s", raw, err, want)
		}
	}
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "jj"}))
	if _, err := orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "jj", Identity: "bea"}); code(err) != api.CodeConflict {
		t.Errorf("someone else's alias: %v", err)
	}
	// The machine owner sets another identity's alias.
	if a := must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "cc", Identity: "bea"})); a.Identity != "bea" {
		t.Errorf("for bea: %+v", a)
	}
	// A member manages no one's aliases, and no identity sets its own.
	if _, err := orchclient.AddAlias(ctx, vida, orchapi.AliasRequest{Alias: "vv"}); code(err) != api.CodeForbidden {
		t.Errorf("vida adds its own: %v", err)
	}
	for _, who := range []string{"ivo", "bea"} {
		if _, err := orchclient.AddAlias(ctx, vida, orchapi.AliasRequest{Alias: "vv", Identity: who}); code(err) != api.CodeNotFound {
			t.Errorf("vida adds for %q: %v", who, err)
		}
	}
	if a := must(orchclient.Aliases(ctx, vida, "")); a.Identity != "vida" || len(a.Aliases) != 0 {
		t.Errorf("vida reads its own: %+v", a)
	}
	if _, err := orchclient.RemoveAlias(ctx, vida, "jj"); code(err) != api.CodeNotFound {
		t.Errorf("vida removes jj: %v", err)
	}
	if _, err := orchclient.Aliases(ctx, vida, "ivo"); code(err) != api.CodeNotFound {
		t.Errorf("vida reads ivo's: %v", err)
	}
	// An admin of every project the identity is in (bea is only in harbor)
	// manages its aliases, and is named as the setter.
	admin := h.tokenClient(api.MintTokenRequest{As: "rae", Project: "harbor", Role: "owner"})
	if a := must(orchclient.AddAlias(ctx, admin, orchapi.AliasRequest{Alias: "cy", Identity: "bea"})); a.AddedBy["cy"] == "" || a.AddedBy["cy"] == a.AddedBy["cc"] {
		t.Errorf("admin adds cy, named: %+v", a)
	}
	if a := must(orchclient.RemoveAlias(ctx, admin, "cc")); a.Identity != "bea" || len(a.Aliases) != 1 {
		t.Errorf("admin removes cc: %+v", a)
	}
	if a := must(orchclient.Aliases(ctx, h.owner, "")); len(a.Aliases) != 1 || a.Aliases[0] != "jj" {
		t.Errorf("own aliases: %+v", a)
	}
}

// waitingFor is the work item IDs in c's open inbox for forWho; the agents'
// list holds no asks.
func waitingFor(t *testing.T, c *client.Client, forWho string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, x := range must(orchclient.DecisionsFor(context.Background(), c, "", "open", forWho)).Decisions {
		if x.Ask != nil && forWho == "agents" {
			t.Errorf("for=agents lists an ask: %s", x.Ask.ID)
		}
		if x.Work != nil {
			out[x.Work.ID] = true
		}
	}
	return out
}

// A question with no ask card never reaches a
// person's Waiting on you, however it names them; it waits on an agent, who
// writes the card or decides. Steps still reach the person, and once the
// card exists the ask is what shows.
func TestUnwrittenQuestionsWaitOnAgents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "owner", Project: "harbor"}))
	bea := h.tokenClient(api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member", NoGrant: true})

	question := h.workItem("QUESTION: block the change? (default: block)", "ask:owner", "questions")
	forMe := h.workItem("which lane", "ask:ivo")
	beas := h.workItem("bea decides", "owner:bea", "owner-decision")
	night := h.workItem("for the night", "needs-night-ruling")
	step := h.workItem("rotate the key", "ask:owner", "owner-run")
	mine := h.workItem("mine to finish", "owner:ivo")
	asked := h.workItem("written up", "ask:owner", "questions")
	ruled := h.workItem("already ruled", "ask:owner", "questions", "ruled")
	ask := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: asked.ID}, Card: card()}))

	for who, c := range map[string]*client.Client{"ivo": h.owner, "bea": bea} {
		for _, view := range []string{"me", "everyone"} {
			got := waitingFor(t, c, view)
			for _, it := range []orchapi.WorkItem{question, forMe, beas, night, asked} {
				if got[it.ID] {
					t.Errorf("%s for=%s: %q (no card) is listed", who, view, it.Title)
				}
			}
			if !got[step.ID] {
				t.Errorf("%s for=%s: the step %q is missing", who, view, step.Title)
			}
		}
	}
	if !waiting(t, h.owner)[mine.ID] {
		t.Errorf("ivo's own step is missing")
	}
	listed := false
	for _, x := range must(orchclient.Decisions(ctx, h.owner, "", "open")).Decisions {
		listed = listed || (x.Ask != nil && x.Ask.ID == ask.ID)
	}
	if !listed {
		t.Errorf("the written ask is not in ivo's inbox")
	}

	agents := waitingFor(t, h.owner, "agents")
	for _, it := range []orchapi.WorkItem{question, forMe, beas, night} {
		if !agents[it.ID] {
			t.Errorf("agents: %q missing", it.Title)
		}
	}
	for _, it := range []orchapi.WorkItem{step, mine, asked, ruled} {
		if agents[it.ID] {
			t.Errorf("agents: %q listed", it.Title)
		}
	}
	if _, err := orchclient.DecisionsFor(ctx, h.owner, "", "open", "agent"); code(err) != api.CodeBadRequest {
		t.Errorf("for=agent: %v", err)
	}
}
