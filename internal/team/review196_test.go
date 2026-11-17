package team_test

import (
	"context"
	"fmt"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"strings"
	"testing"
	"time"
)

func TestA196GuestReenters(t *testing.T) {
	ctx := context.Background()
	f := newStaleFix(t)
	mustf(f.a.owner.AddArea(ctx, "harbor", api.Area{Name: "lbl", Labels: []string{"lbl"}}))(t)
	it := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "labelled", Labels: []string{"lbl"}}))(t)
	old := "bd:" + it.ID
	g, _ := replica(t, f.tn, f.a, "guest-mac", "nGUEST", "harbor/lbl", "gus", "guest")
	gc := at(g.owner, "harbor", "")
	eventually(t, 15*time.Second, func() error {
		if !teamService(t, g).CopyHolds(ctx, old) {
			return fmt.Errorf("guest copy has not got it")
		}
		return nil
	})
	res := mustf(orchclient.MoveIssues(ctx, f.ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	nid := res.Items[0].NewID
	eventually(t, 15*time.Second, func() error {
		if teamService(t, g).CopyHolds(ctx, old) {
			return fmt.Errorf("guest still holds")
		}
		return nil
	})
	// Unlabel in zm, then move back: outside the guest's area, so no event reaches it.
	if _, err := orchclient.RemoveIssueLabel(ctx, f.az, nid, "lbl"); err != nil {
		t.Fatal(err)
	}
	back := mustf(orchclient.MoveIssues(ctx, f.az, orchapi.IssueMoveRequest{To: "harbor", IDs: []string{nid}}))(t)
	if back.Moved != 1 {
		t.Fatalf("back %+v", back)
	}
	if teamService(t, g).CopyHolds(ctx, old) {
		t.Fatal("guest saw unlabelled item")
	}
	// Labelled in again: the guest may read it now.
	if _, err := orchclient.AddIssueLabel(ctx, f.ao, it.ID, "lbl"); err != nil {
		t.Fatal(err)
	}
	var w orchapi.WorkItem
	var err error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if teamService(t, g).CopyHolds(ctx, old) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	w, err = orchclient.GetWork(ctx, gc, old)
	holds := teamService(t, g).CopyHolds(ctx, old)
	inList := false
	for _, x := range mustf(orchclient.Work(ctx, gc, orchapi.WorkQuery{Project: "harbor"}))(t) {
		inList = inList || x.ID == old
	}
	t.Logf("labelled in: guest copy holds=%v, list=%v, get=%q err=%v", holds, inList, w.Title, err)
	if !holds || !inList {
		t.Errorf("guest can read it at the authority but its copy never shows it (holds=%v list=%v get err=%v)", holds, inList, err)
	}
}

func TestA196Edges(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	mustf(f.a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "third"}))(t)
	mustf(f.a.owner.ProjectTeam(ctx, "third"))(t)
	a3 := at(f.a.owner, "third", "")
	mustf(orchclient.SetupIssues(ctx, a3, "third"))(t)
	mv := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "moved"}))(t)
	blk := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "blocked", Deps: []string{mv.ID}}))(t)
	first := mustf(orchclient.MoveIssues(ctx, f.ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{mv.ID}}))(t)
	if first.Moved != 1 {
		t.Fatalf("first move: %+v", first)
	}
	nid := first.Items[0].NewID
	second := mustf(orchclient.MoveIssues(ctx, f.az, orchapi.IssueMoveRequest{To: "third", IDs: []string{nid}}))(t)
	if second.Moved != 1 {
		t.Fatalf("second move: %+v", second)
	}
	detail := mustf(orchclient.Issue(ctx, f.ao, blk.ID))(t)
	if len(detail.DependsOn) != 1 || detail.DependsOn[0].ID != second.Items[0].NewID {
		t.Fatalf("detail: %+v", detail.DependsOn)
	}
	requests := []struct{ method, path, body string }{
		{"GET", "/v0/issues?id=" + blk.ID + "&deps=1&status=all", ""},
		{"POST", "/v0/issues/sql", fmt.Sprintf(`{"query":"SELECT depends_on_external FROM dependencies WHERE issue_id='%s'"}`, blk.ID)},
	}
	for _, q := range requests {
		owner := raw(f.a, "harbor", q.method, q.path, q.body)
		hidden := raw(f.only, "harbor", q.method, q.path, q.body)
		if owner.code != 200 || !strings.Contains(owner.body, nid) {
			t.Errorf("visible edge %s: %d %s", q.path, owner.code, owner.body)
		}
		if hidden.code != 200 || strings.Contains(hidden.body, nid) || strings.Contains(hidden.body, second.Items[0].NewID) {
			t.Errorf("hidden edge %s: %d %s", q.path, hidden.code, hidden.body)
		}
	}
}

func TestA196Restore(t *testing.T) {
	ctx := context.Background()
	f := newStaleFix(t)
	it := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "restored"}))(t)
	old := "bd:" + it.ID
	for i := 0; !teamService(t, f.only).CopyHolds(ctx, old); i++ {
		if i > 100 {
			t.Fatal("never copied")
		}
		ti := "restored"
		if i%2 == 1 {
			ti = fmt.Sprintf("restored %d", i)
		}
		mustf(orchclient.UpdateIssue(ctx, f.ao, it.ID, orchapi.UpdateIssueRequest{Title: &ti}))(t)
		time.Sleep(300 * time.Millisecond)
	}
	dir := t.TempDir()
	bk, err := orchclient.AddTrackerBackup(ctx, f.ao, "bd", orchapi.AddBackupRequest{Target: "file://" + dir, Kind: orchapi.BackupSqlite, RunNow: true})
	if err != nil || !bk.Ran {
		t.Fatalf("backup %+v %v", bk, err)
	}
	mustf(orchclient.MoveIssues(ctx, f.ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	eventually(t, 15*time.Second, func() error {
		if teamService(t, f.only).CopyHolds(ctx, old) {
			return fmt.Errorf("still holds")
		}
		return nil
	})
	rr, err := orchclient.RestoreIssues(ctx, f.ao, orchapi.IssueRestoreRequest{Prefix: "bd"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Logf("restore: %+v", rr)
	iss, ierr := orchclient.Issue(ctx, f.ao, it.ID)
	t.Logf("issue after restore: %q %v", iss.Title, ierr)
	ti := "edited after restore"
	mustf(orchclient.UpdateIssue(ctx, f.ao, it.ID, orchapi.UpdateIssueRequest{Title: &ti}))(t)
	eventually(t, 20*time.Second, func() error {
		if !teamService(t, f.only).CopyHolds(ctx, old) {
			return fmt.Errorf("restored work not yet in replica")
		}
		return nil
	})
	w, werr := orchclient.GetWork(ctx, f.ao, old)
	listed := false
	for _, x := range mustf(orchclient.Work(ctx, f.ao, orchapi.WorkQuery{Project: "harbor"}))(t) {
		listed = listed || x.ID == old
	}
	_, cerr := orchclient.Claim(ctx, f.ao, old, orchapi.ClaimRequest{})
	t.Logf("after restore: issue ok; work get %q err=%v; authority lists=%v; only copy holds=%v; claim err=%v", w.Title, werr, listed, teamService(t, f.only).CopyHolds(ctx, old), cerr)
	if ierr != nil || werr != nil || cerr != nil || !listed || !teamService(t, f.only).CopyHolds(ctx, old) {
		t.Errorf("restored issue is not back as a work item")
	}
}
