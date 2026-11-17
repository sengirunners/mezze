package team_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An old successful refresh must not restore a row removed by a narrowing
// snapshot, even when that snapshot's visible event cursor goes backwards.
func TestNarrowingSnapshotRejectsLateRefresh(t *testing.T) {
	ctx := context.Background()
	f := newStaleFix(t)
	svc := teamService(t, f.only)
	mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "solver", Area: "solver"}))(t)
	it := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "billing secret", Area: "billing"}))(t)
	id := "bd:" + it.ID
	for i := 0; !svc.CopyHolds(ctx, id); i++ {
		if i > 100 {
			t.Fatal("copy never received billing")
		}
		title := fmt.Sprintf("billing secret %d", i)
		mustf(orchclient.UpdateIssue(ctx, f.ao, it.ID, orchapi.UpdateIssueRequest{Title: &title}))(t)
		time.Sleep(100 * time.Millisecond)
	}
	reached, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	undo := svc.HoldRefresh("harbor", func(got string) func() {
		if got != id || !armed.CompareAndSwap(true, false) {
			return nil
		}
		close(reached)
		<-release
		return func() { close(done) }
	})
	t.Cleanup(undo)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	svc.WantRefresh("harbor", id)
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("refresh did not reach authority")
	}
	before := svc.CopyCursor("harbor")
	mustf(f.a.owner.Grant(ctx, api.GrantRequest{Identity: "cyd", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	mustf(f.a.owner.Revoke(ctx, api.GrantRequest{Identity: "cyd", Project: "harbor"}))(t)
	eventually(t, 20*time.Second, func() error {
		if svc.CopyHolds(ctx, id) {
			return fmt.Errorf("billing still in copy")
		}
		after := svc.CopyCursor("harbor")
		if after == "" || after >= before {
			return fmt.Errorf("snapshot cursor %q did not narrow from %q", after, before)
		}
		return nil
	})
	close(release)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("held refresh did not finish")
	}
	check := func() {
		t.Helper()
		h := raw(f.only, "harbor", "GET", "/v0/work/"+id, "")
		m := raw(f.only, "harbor", "GET", "/v0/work/bd:harbor-zzzzz", "")
		if h.code != 404 || h.code != m.code || strings.ReplaceAll(h.body, it.ID, "X") != strings.ReplaceAll(m.body, "harbor-zzzzz", "X") {
			t.Errorf("hidden and missing differ: %d %s / %d %s", h.code, h.body, m.code, m.body)
		}
		if teamService(t, f.only).CopyHolds(ctx, id) {
			t.Error("narrowed copy restored the billing row")
		}
	}
	check()
	f.only.restart(f.tn, tailnettest.Identity{NodeID: "nCYD", NodeKey: "nodekey:cyd-mac", Login: "cyd@example.com", Host: "cyd-mac"})
	check()
}
