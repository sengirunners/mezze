package team_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A member's session reaches its authority with who it works for and its job
// kind, and both machines keep the item it claimed through the replica after
// it releases it (SPEC 8 "Usage" and "Work").
func TestReplicaSessionUsageAndLastWork(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{Agent: "codex", RuntimeID: "bea-usage",
		For: "bea-mac", JobKind: orchapi.JobKindFarm}))(t)
	heartbeat(t, r)
	rs := at(r.owner, "", cs.Address)
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "the lane's bead", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}))(t)
	heartbeat(t, r)
	mustf(orchclient.Release(ctx, rs, it.ID, orchapi.ReleaseRequest{}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.Work != it.ID || got.For != "bea-mac" {
		t.Fatalf("on the member: %+v", got)
	}
	got := mustf(orchclient.Session(ctx, a.owner, cs.Address))(t)
	if got.Work != it.ID || got.For != "bea-mac" || got.JobKind != orchapi.JobKindFarm {
		t.Fatalf("at the authority: %+v", got)
	}
}
