package team_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// zm approver pair-me on a member Mac reserves the person's phone at the
// project's authority, as that person; an agent session there cannot.
func TestPairMeFromAMemberReachesTheAuthority(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	agent := register(t, r, "harbor/solver", "fixture-agent")
	if _, err := orchclient.AddMyPhone(ctx, at(r.owner, "harbor", agent.Address), ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an agent session on the member reserved a phone: %v", err)
	}
	add := mustf(orchclient.AddMyPhone(ctx, at(r.owner, "harbor", ""), ""))(t)
	if add.Approver.Name != "bea-phone" || add.Approver.AddedBy != "self:human:bea" {
		t.Fatalf("reservation: %+v", add.Approver)
	}
	ls := mustf(orchclient.Approvers(ctx, at(a.owner, "", "")))(t)
	if len(ls) != 1 || ls[0].Name != "bea-phone" || ls[0].Identity != "bea" {
		t.Fatalf("the authority's approvers: %+v", ls)
	}
}

// zm approver ls -P on a member Mac: the authority, which decides
// the project's asks, answers with its name and only the person's own
// approvers there; an agent session cannot list them. Each daemon names
// itself on its own list.
func TestMyApproversAtTheAuthority(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	mustf(orchclient.AddMyPhone(ctx, at(r.owner, "harbor", ""), ""))(t)
	mustf(orchclient.AddApprover(ctx, at(a.owner, "", ""), "mini-ipad"))(t) // the authority owner's, for nobody

	mine := mustf(orchclient.MyApprovers(ctx, at(r.owner, "harbor", "")))(t)
	if mine.Daemon != "mini" || len(mine.Approvers) != 1 || mine.Approvers[0].Name != "bea-phone" {
		t.Fatalf("bea's approvers at the authority: %+v", mine)
	}
	agent := register(t, r, "harbor/solver", "fixture-agent")
	if _, err := orchclient.MyApprovers(ctx, at(r.owner, "harbor", agent.Address)); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an agent session listed the person's approvers: %v", err)
	}
	owner := mustf(orchclient.MyApprovers(ctx, at(a.owner, "", "")))(t)
	if owner.Daemon != "mini" || len(owner.Approvers) != 1 || owner.Approvers[0].Name != "mini-ipad" {
		t.Fatalf("the authority owner's own approvers: %+v", owner)
	}
	if all := mustf(orchclient.ApproversHere(ctx, at(a.owner, "", "")))(t); all.Daemon != "mini" || len(all.Approvers) != 2 {
		t.Fatalf("the authority's approvers: %+v", all)
	}
	if here := mustf(orchclient.ApproversHere(ctx, at(r.owner, "", "")))(t); here.Daemon != "bea-mac" || len(here.Approvers) != 0 {
		t.Fatalf("the member's own approvers: %+v", here)
	}
}
