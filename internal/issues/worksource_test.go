package issues_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// Native issues as a zm work source: a [[trackers]] binding with adapter
// "native" serves them through zm work (list, show, claim, comment, status),
// with the native rules.
func TestNativeWorkSource(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	solver := f.ownerAt("harbor/solver")
	a, err := orchclient.CreateIssue(ctx, solver, orchapi.NewIssueRequest{Title: "native via work", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	oc := f.ownerAt("harbor")
	res, err := orchclient.AddTracker(ctx, oc, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10})
	if err != nil {
		t.Fatalf("add native tracker: %v", err)
	}
	if !res.OK {
		t.Fatalf("add: %+v", res)
	}
	wid := "issue:" + a.ID
	var items []orchapi.WorkItem
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if items, err = orchclient.Work(ctx, oc, orchapi.WorkQuery{Source: "issue"}); err == nil && len(items) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(items) != 1 || items[0].ID != wid || items[0].Area != "solver" || items[0].Status != orchapi.WorkOpen || items[0].Extra["number"] != 1.0 {
		t.Fatalf("work ls: %+v %v", items, err)
	}
	// A claim through zm work is a native claim (and a lease).
	if _, err := orchclient.Claim(ctx, oc, wid, orchapi.ClaimRequest{}); err != nil {
		t.Fatalf("work claim: %v", err)
	}
	got, err := orchclient.Issue(ctx, solver, a.ID)
	if err != nil || got.Status != orchapi.IssueInProgress || got.ClaimedBy != "human:ivo" {
		t.Fatalf("native after work claim: %+v %v", got.Issue, err)
	}
	if _, err := orchclient.Comment(ctx, oc, wid, orchapi.CommentRequest{Body: "from zm work"}); err != nil {
		t.Fatalf("work comment: %v", err)
	}
	if got, _ := orchclient.Issue(ctx, solver, a.ID); len(got.Comments) != 1 || got.Comments[0].AuthorAddress != "human:ivo" {
		t.Fatalf("native comments: %+v", got.Comments)
	}
	if _, err := orchclient.SetStatus(ctx, oc, wid, orchapi.StatusRequest{Status: orchapi.WorkDone}); err != nil {
		t.Fatalf("work status done: %v", err)
	}
	if got, _ := orchclient.Issue(ctx, solver, a.ID); got.Status != orchapi.IssueClosed {
		t.Fatalf("native status %s", got.Status)
	}
	// A guest reads it through zm work but cannot claim it (native: guests are read-only).
	b, err := orchclient.CreateIssue(ctx, solver, orchapi.NewIssueRequest{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	bid := "issue:" + b.ID
	deadline = time.Now().Add(10 * time.Second)
	code, body := 0, ""
	for time.Now().Before(deadline) {
		if code, body = f.as("guest", "GET", "/v0/work/"+bid, ""); code == 200 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != 200 || !strings.Contains(body, "second") {
		t.Fatalf("guest work show: %d %s", code, body)
	}
	if code, body := f.as("guest", "POST", "/v0/work/"+bid+"/claim", "{}"); code == 200 {
		t.Fatalf("guest claimed a native issue through zm work: %s", body)
	}
}

// A move holds native writes: every write door answers conflict until it releases.
func TestHoldWrites(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	solver := f.ownerAt("harbor/solver")
	a, err := orchclient.CreateIssue(ctx, solver, orchapi.NewIssueRequest{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := orchclient.IssueProjects(ctx, solver)
	release, err := orchcore.Issues(f.dm.Deps).HoldWrites(ps.Projects[0].Project, "a test move")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CreateIssue(ctx, solver, orchapi.NewIssueRequest{Title: "y"}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "a test move") {
		t.Fatalf("create while held: %v", err)
	}
	if _, err := orchclient.CommentIssue(ctx, solver, a.ID, orchapi.IssueCommentRequest{Text: "z"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("comment while held: %v", err)
	}
	if _, err := orchcore.Issues(f.dm.Deps).HoldWrites(ps.Projects[0].Project, "another"); err == nil {
		t.Fatal("a second hold was granted")
	}
	release()
	if _, err := orchclient.CommentIssue(ctx, solver, a.ID, orchapi.IssueCommentRequest{Text: "z"}); err != nil {
		t.Fatalf("comment after release: %v", err)
	}
}
