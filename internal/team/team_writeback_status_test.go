package team_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A native close releases its lease immediately. Force the release write-back
// to update the authority's mirror before incremental sync can notify replicas.
// The replica must learn DONE from write-back, not from a later sync or snapshot.
func TestNativeCloseWriteBackStatusReachesReplica(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "native close status", Area: "solver"}))(t)
	ss := register(t, a, "harbor/solver", "close-writer")
	writer := at(ao, "harbor", ss.Address)
	mustf(orchclient.ClaimIssue(ctx, writer, is.ID))(t)
	key := "issue:" + is.ID
	check := func(c *client.Client, want orchapi.WorkStatus) error {
		it, err := orchclient.GetWork(ctx, at(c, "harbor", ""), key)
		if err != nil {
			return err
		}
		if it.Status != want {
			return fmt.Errorf("work status %s, want %s", it.Status, want)
		}
		return nil
	}
	eventually(t, 10*time.Second, func() error { return check(a.owner, orchapi.WorkClaimed) })
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	eventually(t, 10*time.Second, func() error { return check(r.owner, orchapi.WorkClaimed) })
	allowSync, written := make(chan struct{}), make(chan struct{})
	var release, signal sync.Once
	unblock := func() { release.Do(func() { close(allowSync) }) }
	defer unblock() // release blocked sync work before daemon cleanup on any failure
	reset := mirrorstate.SetHook(func(project string, items []orchapi.WorkItem, notify bool) func() {
		for _, it := range items {
			if project != is.Project || it.ID != key || it.Status != orchapi.WorkDone {
				continue
			}
			if notify {
				<-allowSync
				return nil
			}
			return func() { signal.Do(func() { close(written) }) }
		}
		return nil
	})
	defer reset()
	mustf(orchclient.CloseIssue(ctx, writer, is.ID, orchapi.CloseIssueRequest{}))(t)
	select {
	case <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("release write-back did not reach its mirror update")
	}
	// Incremental notification remains blocked throughout these assertions.
	eventually(t, 10*time.Second, func() error { return check(a.owner, orchapi.WorkDone) })
	eventually(t, 10*time.Second, func() error { return check(r.owner, orchapi.WorkDone) })
	for _, c := range []*client.Client{a.owner, r.owner} {
		for _, lease := range mustf(orchclient.Leases(ctx, at(c, "harbor", ""), orchapi.LeaseQuery{}))(t) {
			if lease.Key == key {
				t.Fatalf("closed item still leased on %s", c.Project)
			}
		}
	}
}
