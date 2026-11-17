package team_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// 401s from the authority (its WhoIs not knowing the replica's
// connection yet, say) leave queued writes pending, never refused; they
// drain once the authority knows the replica again.
func TestQueuedWritesOutlast401s(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "commented through 401s", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitReachable(t, r, true)
	bea := tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"}
	other := bea
	other.NodeID = "nOTHER"
	tn.SetIdentity("nBEA", other) // the node-bound token is refused from here
	c := mustf(orchclient.Comment(ctx, at(r.owner, "harbor", ""), it.ID, orchapi.CommentRequest{Body: "kept through 401s"}))(t)
	eventually(t, 5*time.Second, func() error {
		if st := remoteState(t, r); st.Pending != 1 || !strings.Contains(st.LastError, "unauthenticated") {
			return fmt.Errorf("want the write pending behind a 401: %+v", st)
		}
		return nil
	})
	tn.SetIdentity("nOTHER", bea)
	waitDrained(t, r)
	if st := remoteState(t, r); st.Conflicts != 0 {
		t.Fatalf("a 401 settled a write: %+v", st)
	}
	got := mustf(orchclient.GetWork(ctx, ao, it.ID))(t)
	if len(got.Comments) != 1 || got.Comments[0].ID != c.ID {
		t.Fatalf("the authority's item after the 401s: %+v", got.Comments)
	}
}
