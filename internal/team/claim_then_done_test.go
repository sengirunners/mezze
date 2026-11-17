package team_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// On a replica, claim a tracker item, mark it done and release it,
// one call after another (the coordinator's close-out). The claim's tracker
// write-back moves the item to claimed at the authority after the claim has
// answered; that status change is the claimer's own, so the done that follows
// must not be refused as a conflict with it (it was, the release then left
// the item open, and the CLI had already said "done").
func TestReplicaClaimThenDoneOnTrackerItem(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "merged, then closed out", Area: "solver"}))(t)
	key := "issue:" + is.ID
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	eventually(t, 10*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, rc, key)
		return err
	})
	ss := register(t, r, "harbor/solver", "closer")
	w := at(r.owner, "harbor", ss.Address)
	mustf(orchclient.Claim(ctx, w, key, orchapi.ClaimRequest{}))(t)
	mustf(orchclient.Comment(ctx, w, key, orchapi.CommentRequest{Body: "merged; closing"}))(t)
	if it := mustf(orchclient.SetStatus(ctx, w, key, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t); it.Status != orchapi.WorkDone {
		t.Fatalf("status answer %s", it.Status)
	}
	mustf(orchclient.Release(ctx, w, key, orchapi.ReleaseRequest{}))(t)
	eventually(t, 15*time.Second, func() error {
		es := mustf(rc.Outbox(ctx, "harbor", false))(t)
		for _, e := range es {
			if e.Kind == "status" && e.State != api.OutboxPending && e.State != api.OutboxAcked {
				return fmt.Errorf("status write %s: %s", e.State, e.Error)
			}
		}
		for _, e := range es {
			if e.State == api.OutboxPending {
				return fmt.Errorf("outbox still pending: %s %s", e.Kind, e.State)
			}
		}
		if it := mustf(orchclient.GetWork(ctx, ao, key))(t); it.Status != orchapi.WorkDone {
			return fmt.Errorf("authority status %s, want done", it.Status)
		}
		return nil
	})
}
