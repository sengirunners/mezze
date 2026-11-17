package approvals_test

// Regressions for the independent review of PR 289.

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// Finding 1: an alias holds its name against identities made later, so one
// name never answers for two identities.
func TestReview289AliasThenIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "dana", Identity: "bea"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "dama", Identity: "bea"}))
	if _, err := h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "dana"}); code(err) != api.CodeConflict {
		t.Errorf("identity dana created although it is bea's alias: %v", err)
	}
	if _, err := h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "darna"}); code(err) != api.CodeConflict {
		t.Errorf("identity darna (looks like alias dama) created: %v", err)
	}
	if _, err := h.owner.Grant(ctx, api.GrantRequest{Identity: "dana", Role: "member", Project: "harbor"}); code(err) != api.CodeConflict {
		t.Errorf("a grant made identity dana: %v", err)
	}
	if _, err := h.owner.MintToken(ctx, api.MintTokenRequest{As: "dana", Project: "harbor", Role: "member"}); code(err) != api.CodeConflict {
		t.Errorf("a token made identity dana: %v", err)
	}
	// Once the alias is gone, the name is free.
	must(orchclient.RemoveAlias(ctx, h.owner, "dana"))
	if _, err := h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "dana"}); err != nil {
		t.Errorf("dana after the alias is removed: %v", err)
	}
}

// Findings 2 and 3: nobody claims an alias for itself, and an admin of only
// some of the target's projects cannot reach it (an alias applies at every
// project on the authority).
func TestReview289AdminReachesOtherProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "zz-side"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "cc", Identity: "bea"}))
	theirs := h.workItem("bea's, under cc", "owner:cc")

	rae := h.tokenClient(api.MintTokenRequest{As: "rae", Project: "zz-side", Role: "owner"})
	if _, err := orchclient.RemoveAlias(ctx, rae, "cc"); code(err) != api.CodeNotFound {
		t.Errorf("the admin of zz-side removed bea's alias: %v", err)
	}
	if _, err := orchclient.AddAlias(ctx, rae, orchapi.AliasRequest{Alias: "cz", Identity: "bea"}); code(err) != api.CodeNotFound {
		t.Errorf("the admin of zz-side added an alias for bea: %v", err)
	}
	if _, err := orchclient.AddAlias(ctx, rae, orchapi.AliasRequest{Alias: "jj"}); code(err) != api.CodeForbidden {
		t.Errorf("rae claimed a free short name for itself: %v", err)
	}
	if a := must(orchclient.Aliases(ctx, h.owner, "bea")); len(a.Aliases) != 1 || a.Aliases[0] != "cc" {
		t.Errorf("bea's aliases: %+v", a)
	}
	bea := h.tokenClient(api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member", NoGrant: true})
	if !waiting(t, bea)[theirs.ID] {
		t.Errorf("owner:cc left bea's inbox")
	}
	// The authority's owner may still take the free name for the person it means.
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "jj"}))
}

// Finding 4: needs-<name> names someone (other than a generic label such as
// needs-night-ruling), so a generic label beside it does not reach everyone.
// And a ruled item no longer waits on a generic label or its type.
func TestReview289NeedsNamesSomeone(t *testing.T) {
	h := newHarness(t)
	must(h.owner.Grant(context.Background(), api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	needsBea := h.workItem("for bea", "needs-bea", "human")
	night := h.workItem("for the night", "needs-night-ruling", "human")
	mine := h.workItem("for me", "needs-ivo", "human")
	ruled := h.workItem("already ruled", "human", "ruled")
	ruledMine := h.workItem("ruled but still mine", "owner:ivo", "ruled")
	got := waiting(t, h.owner)
	if got[needsBea.ID] {
		t.Errorf("ivo's inbox lists an item labelled needs-bea (+human)")
	}
	if !got[night.ID] || !got[mine.ID] || !got[ruledMine.ID] {
		t.Errorf("missing: night %v, needs-ivo %v, owner:ivo+ruled %v", got[night.ID], got[mine.ID], got[ruledMine.ID])
	}
	if got[ruled.ID] {
		t.Errorf("a ruled item still waits on its generic label")
	}
}

// Re-review note (b): needs-<x> names a person only when x is an identity's
// name or an alias at this authority. harbor's needs-design-pass, needs-spec,
// needs-device and needs-owner-approval are plain tags, so the generic label
// beside them still reaches the reader.
func TestNeedsTagsNameOnlyPeople(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "beagh", Role: "member", Project: "harbor"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "bea", Identity: "beagh"}))
	bea := h.tokenClient(api.MintTokenRequest{As: "beagh", Project: "harbor", Role: "member", NoGrant: true})

	var tags []orchapi.WorkItem
	for _, tag := range []string{"needs-design-pass", "needs-spec", "needs-device", "needs-owner-approval"} {
		tags = append(tags, h.workItem("tagged "+tag, tag, "human"), h.workItem("run "+tag, tag, "owner-run"))
	}
	forBea := h.workItem("for bea by alias", "needs-bea", "human")
	forBeaName := h.workItem("for bea by name", "needs-beagh", "human")

	owner, theirs := waiting(t, h.owner), waiting(t, bea)
	for _, it := range tags {
		if !owner[it.ID] || !theirs[it.ID] {
			t.Errorf("%q (a plain needs- tag): owner %v, bea %v; want both", it.Title, owner[it.ID], theirs[it.ID])
		}
	}
	for _, it := range []orchapi.WorkItem{forBea, forBeaName} {
		if owner[it.ID] || !theirs[it.ID] {
			t.Errorf("%q: owner %v, bea %v; want bea only", it.Title, owner[it.ID], theirs[it.ID])
		}
	}
}

// ?for=everyone lists needs-<x> for every identity name and alias at
// this authority, not one hard-coded person; a needs-<x> naming no one is a
// plain tag and stays out unless a generic label rides with it.
func TestEveryoneListsEveryPersonsNeeds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "g1", Role: "member", Project: "harbor"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Alias: "gee", Identity: "g1"}))

	byName := h.workItem("for g1", "needs-g1")
	byAlias := h.workItem("for g1 by alias", "needs-gee")
	nobody := h.workItem("for bea, not an identity here", "needs-bea")

	all := map[string]bool{}
	for _, x := range must(orchclient.DecisionsFor(ctx, h.owner, "", "open", "everyone")).Decisions {
		if x.Work != nil {
			all[x.Work.ID] = true
		}
	}
	for _, it := range []orchapi.WorkItem{byName, byAlias} {
		if !all[it.ID] {
			t.Errorf("everyone: %q missing", it.Title)
		}
	}
	if all[nobody.ID] {
		t.Errorf("everyone: needs-bea listed though bea is no identity here")
	}
}
