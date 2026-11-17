package team_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// The same owner right sets an inbox alias from a member Mac: an identity
// that owns every project on the authority names itself, though the Mac's
// link is capped at member; owning only some is not enough, and a member
// never claims a name for itself or reaches another's.
func TestOwnerSetsOwnAliasFromAMemberMac(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	for _, slug := range []string{"zm", "sengi"} {
		mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: slug}))(t)
		mustf(a.owner.ProjectTeam(ctx, slug))(t)
	}
	mine, _ := replica(t, tn, a, "ivo-mac", "nIVO", "harbor", "ivogh", "member")
	theirs, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(mine.owner, "harbor", "")
	add := func() error {
		_, err := orchclient.AddAlias(ctx, rc, orchapi.AliasRequest{Alias: "ivo", Identity: "ivogh"})
		return err
	}

	for _, p := range []string{"harbor", "zm"} {
		mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: p, Role: "owner"}))(t)
	}
	if err := add(); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an owner of two of the three projects named itself: %v", err)
	}
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "sengi", Role: "owner"}))(t)
	if err := add(); err != nil {
		t.Fatalf("the owner of every project, from its member Mac: %v", err)
	}
	if al := mustf(orchclient.Aliases(ctx, a.owner, "ivogh"))(t); len(al.Aliases) != 1 || al.Aliases[0] != "ivo" {
		t.Fatalf("at the authority: %+v", al)
	}

	bea := at(theirs.owner, "harbor", "")
	if _, err := orchclient.AddAlias(ctx, bea, orchapi.AliasRequest{Alias: "cc"}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("a member claimed a name for itself: %v", err)
	}
	if _, err := orchclient.AddAlias(ctx, bea, orchapi.AliasRequest{Alias: "jj", Identity: "ivogh"}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a member reached another identity's aliases: %v", err)
	}
	if _, err := orchclient.RemoveAlias(ctx, bea, "ivo"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a member removed the owner's alias: %v", err)
	}
}

// The project's owner answers an ask from their own member Mac: its socket
// forwards the call to the authority with the Mac's node-bound token, capped
// at member, and the authority judges the identity's own owner grant. A
// member who is not an owner is refused, through its own Mac too.
func TestOwnerAnswersFromAMemberMac(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mine, _ := replica(t, tn, a, "ivo-mac", "nIVO", "harbor", "ivogh", "member")
	theirs, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	// Owners are the authority's to grant; the Mac's link stays capped at member.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)

	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	card := &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	// An agent on the owner's Mac asks: its receipt is mail (a person's own ask
	// gets none, SPEC 9 "No notice about what you just did").
	asker := register(t, mine, "harbor", "ivo-agent")
	heartbeat(t, mine)
	ask := mustf(orchclient.NewAsk(ctx, at(mine.owner, "harbor", asker.Address), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card}))(t)

	// Each Mac asks the authority whether it decides there (GET /v0/owner).
	if st := mustf(mine.owner.Owner(ctx, "harbor"))(t); !st.Owner || st.Identity != "ivogh" || st.GitHub != "" {
		t.Fatalf("the owner's Mac: %+v", st)
	}
	if st := mustf(theirs.owner.Owner(ctx, "harbor"))(t); st.Owner || st.Identity != "bea" {
		t.Fatalf("a member's Mac: %+v", st)
	}
	if _, err := mine.owner.Owner(ctx, "nowhere"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a project nobody has: %v", err)
	}

	// A member reads the ask but does not own the project.
	for _, c := range []struct {
		name    string
		project string
	}{{"named", "harbor"}, {"by id", ""}} {
		if _, err := orchclient.Choose(ctx, at(theirs.owner, c.project, ""), "", ask.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeForbidden) {
			t.Fatalf("a member's Mac (%s): %v", c.name, err)
		}
	}
	if got := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t); got.Decision != nil {
		t.Fatalf("a member decided: %+v", got.Decision)
	}

	// The owner, from their own Mac, by the ask's id alone.
	d, err := orchclient.Choose(ctx, mine.owner, "", ask.ID, orchapi.DecideRequest{Decision: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != orchapi.MethodOwner || d.Machine != "ivo-mac" || d.Approver != "ivogh" ||
		d.Receipt.Node != "owner:ivo-mac" || d.RecordedAt == nil {
		t.Fatalf("decision: %+v", d)
	}
	got := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t)
	if got.Decision == nil || got.Decision.Method != orchapi.MethodOwner || got.Decision.Machine != "ivo-mac" {
		t.Fatalf("at the authority: %+v", got.Decision)
	}
	cs := mustf(orchclient.Comments(ctx, a.owner, it.ID))(t)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "OWNER DECISION (ivogh via \"ivo-mac\", ") {
		t.Fatalf("comments at the authority: %+v", cs)
	}
	// The asker's answer mail is from the owner who answered, not the
	// authority machine's own user.
	var reply *orchapi.Mail
	for _, m := range mustf(orchclient.Thread(ctx, a.owner, ask.ID))(t) {
		if m.ID != ask.ID && strings.HasPrefix(m.Body, "OWNER DECISION") {
			reply = &m
		}
	}
	if reply == nil || reply.From != "human:ivogh" {
		t.Fatalf("the asker's decision mail: %+v", reply)
	}
	// Sent once: the same answer again is the same receipt, and nothing more lands.
	again, err := orchclient.Choose(ctx, at(mine.owner, "harbor", ""), "", ask.ID, orchapi.DecideRequest{Decision: "b"})
	if err != nil || again.Receipt.Sig != d.Receipt.Sig {
		t.Fatalf("again: %+v %v", again, err)
	}
	if n := len(mustf(orchclient.Comments(ctx, a.owner, it.ID))(t)); n != 1 {
		t.Fatalf("comments after a repeat: %d", n)
	}
}
