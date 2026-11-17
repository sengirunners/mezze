package team_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A replica's status answer names its outbox write, and
// SetStatusSettled (zm work status, MCP work_set_status) reports what the
// authority did with it: applied, refused (an error with the reason), or
// still queued while the authority is away. It never claims a refused write.
func TestReplicaStatusWriteReportsTheOutcome(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	applied := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "applied", Area: "solver"}))(t)
	queued := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "queued", Area: "solver"}))(t)
	refused := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "refused", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 5*time.Second, func() error {
		if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); len(wl.Items) != 3 {
			return fmt.Errorf("copy has %d items", len(wl.Items))
		}
		return nil
	})
	waitReachable(t, r, true)

	// Applied: the answer carries the write, and the wait ends with it acked.
	it, q, err := orchclient.SetStatusSettled(ctx, rs, applied.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}, 10*time.Second)
	if err != nil || q || it.Write == "" || it.Status != orchapi.WorkDone {
		t.Fatalf("applied: %+v queued=%v err=%v", it, q, err)
	}
	if st := mustf(orchclient.GetWork(ctx, ao, applied.ID))(t); st.Status != orchapi.WorkDone {
		t.Fatalf("authority status %s", st.Status)
	}

	// Queued: the authority is away; the bounded wait ends with it pending.
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	start := time.Now()
	if it, q, err := orchclient.SetStatusSettled(ctx, rs, queued.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}, 500*time.Millisecond); err != nil || !q || it.Write == "" {
		t.Fatalf("queued: %+v queued=%v err=%v", it, q, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("queued wait took %s", took)
	}

	// Refused: the status changes at the authority after the replica's base;
	// when the authority is back it refuses the queued write, and the writer
	// gets the reason, not "done".
	mustf(orchclient.SetStatus(ctx, ao, refused.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		tn.Cut("potluck-bea-mac", "potluck-mini", false)
	}()
	_, q, err = orchclient.SetStatusSettled(ctx, rs, refused.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}, 15*time.Second)
	if !api.IsCode(err, api.CodeConflict) || q || !strings.Contains(err.Error(), "zm outbox") || !strings.Contains(err.Error(), "status changed") {
		t.Fatalf("refused: queued=%v err=%v", q, err)
	}
	if st := mustf(orchclient.GetWork(ctx, ao, refused.ID))(t); st.Status != orchapi.WorkInReview {
		t.Fatalf("authority status %s, want in_review", st.Status)
	}
	waitDrained(t, r)
	if st := mustf(orchclient.GetWork(ctx, ao, queued.ID))(t); st.Status != orchapi.WorkDone {
		t.Fatalf("queued write never applied: %s", st.Status)
	}
}
