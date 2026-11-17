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

// A replica's failed session reaches the authority with its outcome.
func TestReplicaPresenceFailedOutcome(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ss := mustf(orchclient.Spawn(ctx, at(r.owner, "harbor/solver", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 1; exit 3"}, Cwd: t.TempDir()}))(t)
	heartbeat(t, r)
	eventually(t, 5*time.Second, func() error {
		x, err := orchclient.Session(ctx, r.owner, ss.Address)
		if err != nil {
			return err
		}
		if x.State != orchapi.StateEnded {
			return fmt.Errorf("still %s", x.State)
		}
		return nil
	})
	heartbeat(t, r)
	x := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t)
	if x.Outcome == nil || x.Outcome.Kind != orchapi.OutcomeFailed {
		t.Fatalf("authority outcome: %+v", x.Outcome)
	}
}
