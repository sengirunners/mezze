package team_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func writerReplyBarrier(t *testing.T, svc *team.Service, kind, item string) (<-chan string, func(), <-chan bool) {
	t.Helper()
	reached, applied := make(chan string, 1), make(chan bool, 1)
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	var armed atomic.Bool
	armed.Store(true)
	undo := svc.HoldWriterReply("harbor", func(gotKind, id string) func() {
		if gotKind != kind || item != "" && id != item || !armed.CompareAndSwap(true, false) {
			return nil
		}
		reached <- id
		<-release
		return func() { applied <- svc.CopyHolds(context.Background(), id) }
	})
	t.Cleanup(func() { resume(); undo() })
	return reached, resume, applied
}

func awaitWriterReply(t *testing.T, reached <-chan string) string {
	t.Helper()
	select {
	case id := <-reached:
		return id
	case <-time.After(15 * time.Second):
		t.Fatal("authority reply never reached the writer barrier")
		return ""
	}
}

func awaitWriterApplied(t *testing.T, applied <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-applied:
		if got != want {
			t.Fatalf("copy immediately after local application: holds=%v, want%v", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("writer did not finish local application")
	}
}

func narrowWriterReplica(t *testing.T, a *daemon) {
	t.Helper()
	ctx := context.Background()
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
}

func waitWriterSnapshot(t *testing.T, svc *team.Service, item string, before uint64, holds bool) {
	t.Helper()
	eventually(t, 15*time.Second, func() error {
		if svc.WriterCopyEpoch("harbor") <= before || svc.CopyHolds(context.Background(), item) != holds {
			return fmt.Errorf("snapshot not yet applied: epoch%d holds%v", svc.WriterCopyEpoch("harbor"), svc.CopyHolds(context.Background(), item))
		}
		return nil
	})
}

func writerHiddenIsMissing(t *testing.T, r *daemon, id string) {
	t.Helper()
	const missing = "local:zzzzzz"
	for _, suffix := range []string{"", "/comments", "/holder"} {
		hidden := raw(r, "harbor", "GET", "/v0/work/"+id+suffix, "")
		gone := raw(r, "harbor", "GET", "/v0/work/"+missing+suffix, "")
		if hidden.code != 404 || hidden.code != gone.code ||
			strings.ReplaceAll(hidden.body, id, "X") != strings.ReplaceAll(gone.body, missing, "X") {
			t.Fatalf("current hidden and missing responses differ: %d %s / %d %s", hidden.code, hidden.body, gone.code, gone.body)
		}
	}
	for _, path := range []string{"/v0/work?limit=5000", "/v0/team/outbox?project=harbor&all=1"} {
		got := raw(r, "harbor", "GET", path, "")
		if got.code != 200 || strings.Contains(got.body, id) || strings.Contains(got.body, "old-scope secret") {
			t.Fatalf("current list disclosed old-scope copy/outcome metadata: %d %s", got.code, got.body)
		}
	}
}

func TestWriterFenceLate404AfterWideningSurvivesRestart(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "billing row", Area: "billing"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	svc := teamService(t, r)
	before := svc.WriterCopyEpoch("harbor")
	narrowWriterReplica(t, a)
	waitWriterSnapshot(t, svc, it.ID, before, false)
	reached, resume, applied := writerReplyBarrier(t, svc, "refresh404", it.ID)
	svc.WantRefresh("harbor", it.ID)
	awaitWriterReply(t, reached)
	before = svc.WriterCopyEpoch("harbor")
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))(t)
	waitWriterSnapshot(t, svc, it.ID, before, true)
	// The old404 is already received. Disable repair traffic before releasing
	// it so eventual convergence cannot hide a destructive transient write.
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	resume()
	awaitWriterApplied(t, applied, true)
	r.restart(tn, tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	eventually(t, 5*time.Second, func() error {
		remotes, err := r.owner.Remotes(ctx)
		if err != nil {
			return err
		}
		if len(remotes) != 1 || remotes[0].Replica == nil {
			return fmt.Errorf("replica copy has not reopened after restart")
		}
		if !teamService(t, r).CopyHolds(ctx, it.ID) {
			return fmt.Errorf("freshly visible row did not survive replica restart")
		}
		return nil
	})
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
}

func TestWriterFenceLateCreateAcrossNarrowing(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	svc := teamService(t, r)
	reached, resume, applied := writerReplyBarrier(t, svc, "create", "")
	type answer struct {
		item orchapi.WorkItem
		err  error
	}
	result := make(chan answer, 1)
	go func() {
		it, err := orchclient.CreateWork(ctx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "old-scope secret", Area: "billing"})
		result <- answer{it, err}
	}()
	id := awaitWriterReply(t, reached)
	before := svc.WriterCopyEpoch("harbor")
	narrowWriterReplica(t, a)
	waitWriterSnapshot(t, svc, id, before, false)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	resume()
	awaitWriterApplied(t, applied, false)
	select {
	case got := <-result:
		if got.err != nil || got.item.ID != id {
			t.Fatalf("cache fence changed the real authority outcome: %+v %v", got.item, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("creator did not receive its accepted result")
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	writerHiddenIsMissing(t, r, id)
	// A new request in the current scope continues to work.
	fresh := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor/solver", ""), orchapi.CreateWorkRequest{Title: "fresh", Area: "solver"}))(t)
	if got := mustf(orchclient.GetWork(ctx, at(r.owner, "harbor", ""), fresh.ID))(t); got.Title != "fresh" {
		t.Fatal("fresh create result is not readable")
	}
}

func TestWriterFenceLateSettlementAcrossNarrowing(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "old-scope secret", Area: "billing"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	svc := teamService(t, r)
	reached, resume, applied := writerReplyBarrier(t, svc, "settle", it.ID)
	mustf(orchclient.SetStatus(ctx, at(r.owner, "harbor", ""), it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	awaitWriterReply(t, reached)
	before := svc.WriterCopyEpoch("harbor")
	narrowWriterReplica(t, a)
	waitWriterSnapshot(t, svc, it.ID, before, false)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	resume()
	awaitWriterApplied(t, applied, false)
	eventually(t, 5*time.Second, func() error {
		if st := remoteState(t, r); st.Pending != 0 {
			return fmt.Errorf("accepted result left pending: %+v", st)
		}
		return nil
	})
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	writerHiddenIsMissing(t, r, it.ID)
}
