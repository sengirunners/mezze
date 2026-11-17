package team_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// On a replica, inflight is read from the copy, so an item in
// review with a pr link and no lease (its agent let the lease lapse while CI
// runs) shows there as unheld, as at the authority; an item without a PR
// does not, and a held one shows once, as its lease.
func TestReplicaInflightListsUnheldPRItems(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	waiting := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "waiting on CI", Area: "solver"}))(t)
	plain := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "review, no PR", Area: "solver"}))(t)
	mustf(orchclient.Link(ctx, ao, waiting.ID, orchapi.Link{Rel: "pr", Target: "https://github.com/o/r/pull/7"}))(t)
	for _, id := range []string{waiting.ID, plain.ID} {
		mustf(orchclient.SetStatus(ctx, ao, id, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	}
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	held := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "held with a PR", Area: "solver"}))(t)
	mustf(orchclient.Link(ctx, ao, held.ID, orchapi.Link{Rel: "pr", Target: "https://github.com/o/r/pull/8"}))(t)
	mustf(orchclient.Claim(ctx, at(r.owner, "", cs.Address), held.ID, orchapi.ClaimRequest{}))(t)
	eventually(t, 5*time.Second, func() error {
		rows := mustf(orchclient.Inflight(ctx, at(r.owner, "harbor", ""), ""))(t)
		unheld, leased := map[string]bool{}, map[string]int{}
		for _, row := range rows {
			if row.Unheld {
				if row.Lease.Holder != "" || row.Item == nil || row.Item.ID != row.Lease.Key {
					return fmt.Errorf("unheld row shape %+v", row)
				}
				unheld[row.Lease.Key] = true
			} else {
				leased[row.Lease.Key]++
			}
		}
		if !unheld[waiting.ID] || unheld[plain.ID] || unheld[held.ID] || leased[held.ID] != 1 {
			return fmt.Errorf("replica rows: unheld %v leased %v (%+v)", unheld, leased, rows)
		}
		return nil
	})
}

// Queued status writes count, as in listItems: with the authority cut off,
// an unheld item closed through the copy drops out at once, and a PR-linked
// open item put in review there shows.
func TestReplicaInflightUnheldSeesQueuedStatus(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	closing := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "merged while offline", Area: "solver"}))(t)
	opening := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "review sent offline", Area: "solver"}))(t)
	for i, id := range []string{closing.ID, opening.ID} {
		mustf(orchclient.Link(ctx, ao, id, orchapi.Link{Rel: "pr", Target: fmt.Sprintf("https://github.com/o/r/pull/%d", 20+i)}))(t)
	}
	mustf(orchclient.SetStatus(ctx, ao, closing.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	unheld := func() map[string]bool {
		out := map[string]bool{}
		for _, row := range mustf(orchclient.Inflight(ctx, at(r.owner, "harbor", ""), ""))(t) {
			if row.Unheld {
				out[row.Lease.Key] = true
			}
		}
		return out
	}
	eventually(t, 5*time.Second, func() error {
		if u := unheld(); !u[closing.ID] || u[opening.ID] {
			return fmt.Errorf("before: unheld %v", u)
		}
		return nil
	})
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	mustf(orchclient.SetStatus(ctx, rs, closing.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	mustf(orchclient.SetStatus(ctx, rs, opening.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	if u := unheld(); u[closing.ID] || !u[opening.ID] {
		t.Fatalf("with queued writes: unheld %v, want only %s", u, opening.ID)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)
}
