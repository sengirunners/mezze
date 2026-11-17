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
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

type staleFix struct {
	a     *daemon
	ao    *client.Client
	az    *client.Client
	only  *daemon
	onlyc *client.Client
	tn    *tailnettest.Net
}

func newStaleFix(t *testing.T) staleFix {
	t.Helper()
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	for slug, c := range map[string]*client.Client{"harbor": ao, "zm": az} {
		mustf(orchclient.SetupIssues(ctx, c, slug))(t)
		if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "bd", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("add native tracker: %+v %v", res, err)
		}
	}
	only, _ := replica(t, tn, a, "cyd-mac", "nCYD", "harbor", "cyd", "member")
	return staleFix{a: a, ao: ao, az: az, only: only, onlyc: at(only.owner, "harbor", ""), tn: tn}
}

// A tracker page read before a move lands in the mirror after the move finished.
// Unmodified code runs either side of the delay point.
func TestAttackStalePageAfterMove(t *testing.T) {
	ctx := context.Background()
	f := newStaleFix(t)
	it := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "secret-after-move"}))(t)
	old := "bd:" + it.ID
	for i := 0; !teamService(t, f.only).CopyHolds(ctx, old); i++ {
		if i > 100 {
			t.Fatal("the copy never got it")
		}
		ti := fmt.Sprintf("v%d", i)
		mustf(orchclient.UpdateIssue(ctx, f.ao, it.ID, orchapi.UpdateIssueRequest{Title: &ti}))(t)
		time.Sleep(300 * time.Millisecond)
	}
	// Hold the page that carries the old row.
	reached := make(chan bool, 1)
	release := make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	done := make(chan struct{})
	undo := mirrorstate.SetHook(func(project string, items []orchapi.WorkItem, notify bool) func() {
		if !armed.Load() {
			return nil
		}
		for _, w := range items {
			if strings.HasSuffix(w.ID, ":"+it.ID) && armed.CompareAndSwap(true, false) {
				reached <- notify
				<-release
				return func() { close(done) }
			}
		}
		return nil
	})
	t.Cleanup(undo)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	title := "edited before the move"
	mustf(orchclient.UpdateIssue(ctx, f.ao, it.ID, orchapi.UpdateIssueRequest{Title: &title}))(t)
	var notify bool
	select {
	case notify = <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("tracker page never reached the hook")
	}
	t.Logf("page held (notify=%v), moving now", notify)
	res := mustf(orchclient.MoveIssues(ctx, f.ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	if res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	var moveID string
	if err := f.ao.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0", Types: orchapi.EventWorkMoved}, func(e api.Event) error {
		moveID = e.ID
		return nil
	}); err != nil || moveID == "" {
		t.Fatalf("move event: %q %v", moveID, err)
	}
	// The PR's fixture bar: the copy passed the move, and does not hold the row.
	eventually(t, 10*time.Second, func() error {
		if teamService(t, f.only).CopyCursor("harbor") < moveID {
			return fmt.Errorf("copy not past move")
		}
		return nil
	})
	if teamService(t, f.only).CopyHolds(ctx, old) {
		t.Fatalf("copy still holds before the stale page")
	}
	if _, err := orchclient.Holder(ctx, f.onlyc, old); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("before the stale page: %v", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stale page did not finish")
	}
	if err := f.ao.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: moveID, Types: orchapi.EventWorkChanged}, func(e api.Event) error {
		if e.Subject == old {
			t.Errorf("stale page published work.changed after the move")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, hidden := orchclient.Holder(ctx, f.onlyc, old)
	_, missing := orchclient.Holder(ctx, f.onlyc, "bd:harbor-zzzzz")
	if !api.IsCode(hidden, api.CodeNotFound) || !api.IsCode(missing, api.CodeNotFound) {
		t.Fatalf("hidden %v, missing %v", hidden, missing)
	}
	if hm, mm := strings.ReplaceAll(hidden.Error(), it.ID, "X"), strings.ReplaceAll(missing.Error(), "harbor-zzzzz", "X"); hm != mm {
		t.Fatalf("hidden and missing differ: %q / %q", hm, mm)
	}
}
