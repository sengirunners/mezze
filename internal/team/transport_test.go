package team_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// altTransport is a third transport for the test: the tailnet's driver
// under another name, whose codes are "alt+<tailnet code>". The team
// module must take it as it takes the built-in ones.
type altTransport struct{ core.TeamTransport }

func (a altTransport) Via() string { return "alt" }

func (a altTransport) ParseCode(code string) (core.JoinTarget, bool, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(code), "alt+")
	if !ok {
		return core.JoinTarget{}, false, nil
	}
	t, _, err := a.TeamTransport.ParseCode(rest)
	t.Via = "alt"
	return t, true, err
}

// The team module names no transport: a driver registered on the member's
// daemon joins, syncs and claims through it, and the remote records it.
func TestTeamTakesAnyRegisteredTransport(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "through alt", Area: "solver"}))(t)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	r.dm.Deps.AddTransport(altTransport{r.dm.Deps.Transport(core.ViaTailnet)})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: "alt+" + inv.Code}))(t)
	if j.Remote == nil || j.Remote.Via != "alt" {
		t.Fatalf("remote %+v", j.Remote)
	}
	if rs := mustf(r.owner.Remotes(ctx))(t); len(rs) != 1 || rs[0].Via != "alt" {
		t.Fatalf("remotes %+v", rs)
	}
	sess := register(t, r, "harbor/solver", "alt-sess-1")
	if cl := mustf(orchclient.Claim(ctx, at(r.owner, "", sess.Address), it.ID, orchapi.ClaimRequest{}))(t); cl.Lease.Holder != sess.Address {
		t.Fatalf("claim %+v", cl)
	}
	if ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t); len(ls) != 1 || ls[0].Holder != sess.Address {
		t.Fatalf("authority leases %+v", ls)
	}
	// A code no registered transport reads is refused with the formats.
	if _, err := r.owner.Join(ctx, api.JoinRequest{Code: "nope:x:y"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("unknown code: %v", err)
	}
}
