package issues_test

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// The native door (bd's update --claim, close, comments add) carries lease
// fences: a lease-backed claim answers with its fence, and a write naming a
// fence whose lease lapsed or was taken again is refused (stale-lease),
// writing nothing. Without the binding there are no leases, so any named
// fence is stale.
func TestNativeFence(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("fenced native issue")
	w, err := orchclient.ClaimIssueWith(ctx, f.owner, id, orchapi.ClaimIssueRequest{TTLSeconds: 1})
	if err != nil || w.LeaseFence <= 0 {
		t.Fatalf("claim %+v %v", w, err)
	}
	f1 := w.LeaseFence
	if _, err := orchclient.CommentIssue(ctx, f.owner, id, orchapi.IssueCommentRequest{Text: "under the lease", Fence: f1}); err != nil {
		t.Fatalf("fenced comment while live: %v", err)
	}
	// The ticker expires the lease and releases the issue.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && f.lease(id) != nil {
		time.Sleep(100 * time.Millisecond)
	}
	if f.lease(id) != nil {
		t.Fatal("the lease did not expire")
	}
	if _, err := orchclient.CloseIssue(ctx, f.owner, id, orchapi.CloseIssueRequest{Reason: "late", Fence: f1}); !orchapi.IsStaleLease(err) {
		t.Fatalf("close after expiry: %v", err)
	}
	if _, err := orchclient.CommentIssue(ctx, f.owner, id, orchapi.IssueCommentRequest{Text: "late", Fence: f1}); !orchapi.IsStaleLease(err) {
		t.Fatalf("comment after expiry: %v", err)
	}
	if it := f.issue(id); it.Status == orchapi.IssueClosed {
		t.Fatal("a stale close landed")
	}
	// Claimed again: a new fence; the old one is still refused, the new one works.
	w2, err := orchclient.ClaimIssueWith(ctx, f.owner, id, orchapi.ClaimIssueRequest{TTLSeconds: 600})
	if err != nil || w2.LeaseFence <= f1 {
		t.Fatalf("claim again %+v %v (was %d)", w2, err, f1)
	}
	if _, err := orchclient.ClaimIssueWith(ctx, f.owner, id, orchapi.ClaimIssueRequest{Fence: f1}); !orchapi.IsStaleLease(err) {
		t.Fatalf("renew with the old fence: %v", err)
	}
	title := "renamed late"
	if _, err := orchclient.UpdateIssue(ctx, f.owner, id, orchapi.UpdateIssueRequest{Title: &title, Fence: f1}); !orchapi.IsStaleLease(err) {
		t.Fatalf("update with the old fence: %v", err)
	}
	if _, err := orchclient.CloseIssue(ctx, f.owner, id, orchapi.CloseIssueRequest{Reason: "done", Fence: w2.LeaseFence}); err != nil {
		t.Fatalf("close with the current fence: %v", err)
	}
	if it := f.issue(id); it.Status != orchapi.IssueClosed || f.lease(id) != nil {
		t.Fatalf("after the fenced close: %s lease %+v", it.Status, f.lease(id))
	}

	plain := newLeaseFixture(t, false)
	pid := plain.create("no binding, no leases")
	if _, err := orchclient.ClaimIssueWith(ctx, plain.owner, pid, orchapi.ClaimIssueRequest{Fence: 7}); !orchapi.IsStaleLease(err) {
		t.Fatalf("fenced claim without a binding: %v", err)
	}
	if _, err := orchclient.CloseIssue(ctx, plain.owner, pid, orchapi.CloseIssueRequest{Fence: 7}); !orchapi.IsStaleLease(err) {
		t.Fatalf("fenced close without a binding: %v", err)
	}
}
