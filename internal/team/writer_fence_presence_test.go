package team_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A canonical snapshot can already contain a lease whose accepted answer
// is still held before settlement. Fencing that old answer must not make
// presence forget the current SQL lease when another project's key ends.
// This is intentionally red before the canonical presence-cache repair.
func TestWriterFenceSnapshotKeepsOtherProjectLeaseInPresence(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := shortLeaseAuthority(t, tn)
	harbor := mustf(a.owner.GetProject(ctx, "harbor"))(t)
	second := mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "second"}))(t)
	mustf(a.owner.AddArea(ctx, "second", api.Area{Name: "solver"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "second"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "second", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	ss := register(t, r, "side", "fenced-presence-1")
	svc := teamService(t, r)
	firstClient := at(r.owner, "harbor", ss.Address)
	mustf(orchclient.Lease(ctx, firstClient, orchapi.ClaimRequest{Key: "lock:harbor"}))(t)
	// Complete initial join/rescope work before arranging the tested race.
	// A fresh renewal and real heartbeat establish the known cached first
	// lease; no test-only cache injection participates in the reproduction.
	eventually(t, 10*time.Second, func() error {
		if _, err := orchclient.Lease(ctx, firstClient, orchapi.ClaimRequest{Key: "lock:harbor", Renew: true}); err != nil {
			return err
		}
		svc.BeatProject(ctx, "harbor")
		row, err := orchclient.Session(ctx, a.owner, ss.Address)
		if err != nil || row.Project != harbor.ID || row.State != orchapi.StateRunning {
			return fmt.Errorf("first lease did not establish harbor presence: %+v %v", row, err)
		}
		return nil
	})

	reached, settled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(release) }) }
	var armed atomic.Bool
	armed.Store(true)
	undo := svc.HoldWriterReply("second", func(kind, key string) func() {
		if kind != "settle" || key != "lock:second" || !armed.CompareAndSwap(true, false) {
			return nil
		}
		close(reached)
		<-release
		return func() { close(settled) }
	})
	t.Cleanup(func() { resume(); undo() })
	type claimAnswer struct {
		response orchapi.ClaimResponse
		err      error
	}
	answer := make(chan claimAnswer, 1)
	go func() {
		res, err := orchclient.Lease(ctx, at(r.owner, "second", ss.Address), orchapi.ClaimRequest{Key: "lock:second"})
		answer <- claimAnswer{res, err}
	}()
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("second lease answer did not reach the settlement barrier")
	}
	eventually(t, 10*time.Second, func() error {
		ls, err := orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{})
		if err != nil || len(ls) != 2 {
			return fmt.Errorf("both accepted leases are not present: %+v %v", ls, err)
		}
		for _, lease := range ls {
			if lease.Holder != ss.Address || !lease.Bound {
				return fmt.Errorf("session's lease is not yet bound: %+v", lease)
			}
		}
		return nil
	})
	beforeHarbor, beforeSecond := svc.WriterCopyEpoch("harbor"), svc.WriterCopyEpoch("second")
	// Adding a redundant guest tuple increments scope_version while the
	// member grant still permits exactly the same project data and leases.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "second", Area: "solver"}))(t)
	for _, slug := range []string{"harbor", "second"} {
		mustf(orchclient.Sessions(ctx, at(r.owner, slug, ""), orchapi.SessionQuery{}))(t) // forwarded: observes the new scope version
	}
	eventually(t, 10*time.Second, func() error {
		if svc.WriterCopyEpoch("harbor") <= beforeHarbor || svc.WriterCopyEpoch("second") <= beforeSecond {
			return fmt.Errorf("new canonical snapshots not yet committed")
		}
		ls, err := orchclient.Leases(ctx, r.owner, orchapi.LeaseQuery{Project: "second"})
		if err != nil || len(ls) != 1 || ls[0].Key != "lock:second" || ls[0].Holder != ss.Address || !ls[0].Bound {
			return fmt.Errorf("snapshot has not installed the held answer's SQL lease: %+v %v", ls, err)
		}
		return nil
	})
	t.Logf("second lease is in committed SQL snapshot before old settlement; epoch %d -> %d", beforeSecond, svc.WriterCopyEpoch("second"))
	resume()
	select {
	case <-settled:
	case <-time.After(15 * time.Second):
		t.Fatal("held second lease did not settle after the snapshot")
	}
	select {
	case got := <-answer:
		if got.err != nil && !api.IsCode(got.err, api.CodeQueued) || got.err == nil && (got.response.Lease.Key != "lock:second" || got.response.Lease.Holder != ss.Address) {
			t.Fatalf("held accepted claim lost its actual outcome: %+v %v", got.response, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim caller remained blocked after settlement")
	}
	rr := mustf(orchclient.ReleaseLease(ctx, firstClient, "lock:harbor", orchapi.ReleaseRequest{}))(t)
	if !rr.Released {
		t.Fatal("first lease was not released")
	}
	for i, slug := range []string{"harbor", "harbor", "second", "second"} {
		svc.BeatProject(ctx, slug)
		ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
		row := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t)
		t.Logf("after heartbeat %d to %s: leases=%+v authority session project=%s state=%s", i+1, slug, ls, row.Project, row.State)
		if len(ls) != 1 || ls[0].Project != second.ID || ls[0].Key != "lock:second" || ls[0].Holder != ss.Address || !ls[0].Bound || row.State == orchapi.StateEnded {
			t.Fatalf("snapshot's current lease was forgotten after heartbeat %d to %s: leases=%+v session=%+v", i+1, slug, ls, row)
		}
	}
}
