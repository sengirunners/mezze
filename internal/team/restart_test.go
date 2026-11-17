package team_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// waitCopyOK waits for the copy to be ok. Right after a restart the socket
// answers before the team module has opened the copy (Serve starts the
// modules after listening), so a remote listed without one is waited out,
// not a failure (main red on b39a13f).
func waitCopyOK(t *testing.T, r *daemon) {
	t.Helper()
	eventually(t, 5*time.Second, func() error {
		rs := mustf(r.owner.Remotes(context.Background()))(t)
		if len(rs) != 1 || rs[0].Replica == nil || rs[0].Replica.State != "ok" {
			return fmt.Errorf("copy not ok: %+v", rs)
		}
		return nil
	})
}

// After a restart a replica showed "copy stale, authority
// unreachable" for about 30 s. Its first sync was a long poll, and on a quiet
// project the authority answers one (the first answer that marks it
// reachable) only when the wait runs out. With the waits an hour long, the
// copy must still be ok right after the restart.
func TestRestartedReplicaCopyOKAtOnce(t *testing.T) {
	quietPresence(t)
	oldWait, oldMax := team.SyncWait, team.SyncWaitMax
	team.SyncWait, team.SyncWaitMax = time.Hour, time.Hour
	t.Cleanup(func() { team.SyncWait, team.SyncWaitMax = oldWait, oldMax })
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitCopyOK(t, r)
	r.restart(tn, tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	waitCopyOK(t, r)

	// Once reachable it long-polls again: a sync without the wait answers
	// (and moves Synced) at once, over and over, on a quiet project; a long
	// poll answers only when a change comes.
	synced := remoteState(t, r).Synced
	time.Sleep(300 * time.Millisecond)
	if st := remoteState(t, r); st.Synced == nil || !st.Synced.Equal(*synced) {
		t.Fatalf("answers on a quiet project (no long poll): synced %v, then %v", synced, st.Synced)
	}
	it := mustf(orchclient.CreateWork(context.Background(), at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "ends the poll", Area: "solver"}))(t)
	eventually(t, 5*time.Second, func() error {
		if _, err := orchclient.GetWork(context.Background(), r.owner, it.ID); err != nil {
			return fmt.Errorf("the change never reached the copy: %v", err)
		}
		return nil
	})
}

// A call on the socket in a restarted daemon's first moments waits for the
// modules to start, so it never finds a joined project without its copy
// (CI: GET /v0/team/remotes listed one with no replica status while the
// copy was still opening, which failed TestRestartedReplicaCopyOKAtOnce on
// a slow runner). The copy takes 300 ms to open here.
func TestRestartedReplicaNeverShowsNoCopy(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	waitCopyOK(t, r)
	t.Cleanup(team.SlowCopyOpen(300 * time.Millisecond))
	r.restart(tn, tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	for _, c := range []*client.Client{r.owner, r.sock} { // in process, and over the real socket
		rs := mustf(c.Remotes(context.Background()))(t)
		if len(rs) != 1 || rs[0].Replica == nil {
			t.Fatalf("right after the restart: %+v", rs)
		}
	}
	waitCopyOK(t, r)
}
