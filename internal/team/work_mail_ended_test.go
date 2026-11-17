package team_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// TestWorkMailSkipsEndedReplicaCreator: mail to a team item
// resolves at the authority. When the item's creator is a replica session
// that has ended, the mail goes to the replica's group mailbox instead of
// the dead inbox; while the creator is live it goes to the creator.
func TestWorkMailSkipsEndedReplicaCreator(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-made-1")
	heartbeat(t, r)
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "made on bea", Area: "solver"}))(t)
	ms := register(t, a, "harbor", "mini-asker-1")
	am := at(a.owner, "harbor", ms.Address)

	m1 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: it.ID, Body: "creator still here?"}))(t)
	if m1.Recipient != cs.Address || m1.Via != "creator of "+it.ID {
		t.Fatalf("live creator: %+v", m1)
	}

	mustf(orchclient.Register(ctx, r.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-made-1", End: true}))(t)
	heartbeat(t, r)
	m2 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: it.ID, Body: "creator gone"}))(t)
	if m2.Recipient != "machine:bea-mac" || m2.Via != "creator of "+it.ID+" (that session has ended)" {
		t.Fatalf("ended creator: %+v", m2)
	}
}
