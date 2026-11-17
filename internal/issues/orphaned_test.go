package issues_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// A follow-up a session filed and left with nobody on it is marked
// orphaned when the session ends, so a supervisor's `bd ready --label
// orphaned` picks it up; one somebody holds, or another filer's, is not; and
// the mark goes once the issue gets a holder.
func TestEndedSessionOrphansItsUnheldFollowUps(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	const rid = "abcdef01-orphan-follow-ups"
	session, err := orchclient.Register(ctx, f.owner, orchapi.RegisterRequest{Agent: "codex", RuntimeID: rid, State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	c := *f.owner
	c.As = session.Address
	filed := func(title string) string {
		it, err := orchclient.CreateIssue(ctx, &c, orchapi.NewIssueRequest{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if it.CreatedByAddress != session.Address {
			t.Fatalf("filed by %q, want the session %q", it.CreatedByAddress, session.Address)
		}
		return it.ID
	}
	left := filed("follow-up nobody took")
	taken := filed("follow-up someone took")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, taken); err != nil {
		t.Fatal(err)
	}
	closed := filed("follow-up already done")
	if _, err := orchclient.CloseIssue(ctx, &c, closed, orchapi.CloseIssueRequest{Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	other := f.create("someone else's backlog")

	if _, err := orchclient.Register(ctx, f.owner, orchapi.RegisterRequest{Agent: "codex", RuntimeID: rid, End: true}); err != nil {
		t.Fatal(err)
	}
	orphaned := func(id string) bool { return slices.Contains(f.issue(id).Labels, issues.OrphanedLabel) }
	for deadline := time.Now().Add(10 * time.Second); !orphaned(left); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the unheld follow-up was not marked: %+v", f.issue(left))
		}
	}
	for _, id := range []string{taken, closed, other} {
		if orphaned(id) {
			t.Fatalf("%s marked orphaned: %+v", id, f.issue(id))
		}
	}
	queue, err := orchclient.Issues(ctx, f.owner, orchapi.IssueQuery{Ready: true, Labels: []string{issues.OrphanedLabel}})
	if err != nil || len(queue.Issues) != 1 || queue.Issues[0].ID != left {
		t.Fatalf("the supervisor's queue: %+v %v", queue.Issues, err)
	}

	// Picked up: the mark goes with the claim.
	if _, err := orchclient.ClaimIssue(ctx, f.owner, left); err != nil {
		t.Fatal(err)
	}
	if orphaned(left) {
		t.Fatalf("a claimed issue is still marked orphaned: %+v", f.issue(left))
	}

	// The race: a claim lands between the end's list and its label add. The
	// add re-checks the holder, so a held issue never keeps the mark.
	if _, err := orchclient.AddIssueLabel(ctx, f.owner, taken, issues.OrphanedLabel); err != nil {
		t.Fatal(err)
	}
	if orphaned(taken) {
		t.Fatalf("a label add after a claim left the mark: %+v", f.issue(taken))
	}
}
