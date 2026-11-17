package issues_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A claim's routing label (the bd door's actor, a hub address) becomes the
// assignee; claimed_by stays the caller's address, authz the caller's. Same
// with and without the native binding (a lease).
func TestClaimRoutingLabel(t *testing.T) {
	for _, bind := range []bool{false, true} {
		f := newLeaseFixture(t, bind)
		ctx := context.Background()
		o := f.owner
		id := f.create("labelled")
		w, err := orchclient.ClaimIssueAs(ctx, o, id, "ivo-mac/58565660")
		if err != nil {
			t.Fatalf("bind=%v claim: %v", bind, err)
		}
		it := f.issue(id)
		if w.Assignee != "ivo-mac/58565660" || it.Assignee != "ivo-mac/58565660" || it.Status != orchapi.IssueInProgress || it.ClaimedBy == "" {
			t.Fatalf("bind=%v claimed: %+v", bind, it)
		}
		if bind && f.lease(id) == nil {
			t.Fatalf("bind=%v: no lease", bind)
		}
		// Claiming again under the same label is a no-op; under another label
		// it is someone else's claim, even from the same address (scripts on
		// the socket share one), and the claim and its lease stay.
		if _, err := orchclient.ClaimIssueAs(ctx, o, id, "ivo-mac/58565660"); err != nil {
			t.Fatalf("bind=%v reclaim: %v", bind, err)
		}
		if _, err := orchclient.ClaimIssueAs(ctx, o, id, "ivo-mac/11112222"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "ivo-mac/58565660") {
			t.Fatalf("bind=%v other label, same address: %v", bind, err)
		}
		if it := f.issue(id); it.Assignee != "ivo-mac/58565660" || it.Status != orchapi.IssueInProgress {
			t.Fatalf("bind=%v after refused claim: %+v", bind, it)
		}
		if bind && f.lease(id) == nil {
			t.Fatalf("bind=%v: a refused claim dropped the holder's lease", bind)
		}
		// Comments keep the caller's zm identity as author (audit, not routing).
		c, err := orchclient.CommentIssue(ctx, o, id, orchapi.IssueCommentRequest{Text: "note"})
		if err != nil || c.Author == "ivo-mac/58565660" || c.Author == "" {
			t.Fatalf("bind=%v comment author %+v %v", bind, c, err)
		}

		// An imported claim (assignee set by hand, no claimed_by): another label
		// conflicts naming the holder; the same label adopts it.
		id2 := f.create("imported-style")
		st, who := orchapi.IssueInProgress, "dev-03/aaaabbbb"
		if _, err := orchclient.UpdateIssue(ctx, o, id2, orchapi.UpdateIssueRequest{Status: &st, Assignee: &who}); err != nil {
			t.Fatal(err)
		}
		if _, err := orchclient.ClaimIssueAs(ctx, o, id2, "ivo-mac/58565660"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "dev-03/aaaabbbb") {
			t.Fatalf("bind=%v: other label on a held issue: %v", bind, err)
		}
		if _, err := orchclient.ClaimIssueAs(ctx, o, id2, "dev-03/aaaabbbb"); err != nil {
			t.Fatalf("bind=%v adopt: %v", bind, err)
		}
		if it := f.issue(id2); it.Assignee != "dev-03/aaaabbbb" || it.ClaimedBy == "" {
			t.Fatalf("bind=%v adopted: %+v", bind, it)
		}
		// No label: the caller's name, as before; its own unlabelled claim
		// (zm work claim, MCP) then takes a label from bd.
		id3 := f.create("plain")
		if _, err := orchclient.ClaimIssue(ctx, o, id3); err != nil {
			t.Fatal(err)
		}
		if it := f.issue(id3); it.Assignee != "ivo" {
			t.Fatalf("bind=%v plain claim assignee %q", bind, it.Assignee)
		}
		if _, err := orchclient.ClaimIssueAs(ctx, o, id3, "ivo-mac/58565660"); err != nil {
			t.Fatalf("bind=%v label own claim: %v", bind, err)
		}
		if it := f.issue(id3); it.Assignee != "ivo-mac/58565660" || (bind && f.lease(id3) == nil) {
			t.Fatalf("bind=%v labelled own claim: %+v lease=%v", bind, it, f.lease(id3))
		}
		if _, err := orchclient.ClaimIssueAs(ctx, o, f.create("bad"), "a\nb"); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("bind=%v control char label: %v", bind, err)
		}
	}
}

// Guests stay read-only whatever label they send.
func TestClaimLabelKeepsAuthz(t *testing.T) {
	f := newAuthzFixture(t)
	o := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(context.Background(), o, orchapi.NewIssueRequest{Title: "solver work"})
	if err != nil {
		t.Fatal(err)
	}
	code, body := f.as("guest", "POST", "/v0/issues/"+it.ID+"/claim", `{"assignee":"ivo-mac/58565660"}`)
	if code < 400 || strings.Contains(body, `"in_progress"`) {
		t.Fatalf("guest claim with a label: %d %s", code, body)
	}
	if cur, _ := orchclient.Issue(context.Background(), o, it.ID); cur.Status != orchapi.IssueOpen || cur.Assignee != "" {
		t.Fatalf("guest changed it: %+v", cur.Issue)
	}
}

// A session's claim made under a person's name (bd-zm's git user.name
// fallback before labels named sessions) is taken over by that session's
// own label, or a lane's under it, when it claims again: claimed_by already
// says it is the session's. Another session, or a label naming another
// session, still conflicts.
func TestSessionLabelAdoptsOldStyleClaim(t *testing.T) {
	for _, bind := range []bool{false, true} {
		f := newLeaseFixture(t, bind)
		ctx := context.Background()
		sess := *f.owner
		sess.As = "mini/aaaa0001"
		other := *f.owner
		other.As = "mini/bbbb0002"
		for i, next := range []string{"mini/aaaa0001", "lane1@mini/aaaa0001"} {
			id := f.create("old style " + itoa(i))
			if _, err := orchclient.ClaimIssueAs(ctx, &sess, id, "Ivo Doe"); err != nil {
				t.Fatalf("bind=%v old-style claim: %v", bind, err)
			}
			if _, err := orchclient.ClaimIssueAs(ctx, &other, id, "mini/bbbb0002"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "Ivo Doe") {
				t.Fatalf("bind=%v another session took it: %v", bind, err)
			}
			if _, err := orchclient.ClaimIssueAs(ctx, &sess, id, "dev-03/aaaabbbb"); !api.IsCode(err, api.CodeConflict) {
				t.Fatalf("bind=%v a label naming another session took it: %v", bind, err)
			}
			if _, err := orchclient.ClaimIssueAs(ctx, &sess, id, next); err != nil {
				t.Fatalf("bind=%v session label %q re-claim: %v", bind, next, err)
			}
			it := f.issue(id)
			if it.Assignee != next || it.ClaimedBy != "mini/aaaa0001" || it.Status != orchapi.IssueInProgress {
				t.Fatalf("bind=%v adopted as %q: %+v", bind, next, it)
			}
			if bind && f.lease(id) == nil {
				t.Fatalf("bind=%v: adoption dropped the lease", bind)
			}
			// Now a lane's claim is the lane's: the session's bare label conflicts.
			if next != "mini/aaaa0001" {
				if _, err := orchclient.ClaimIssueAs(ctx, &sess, id, "mini/aaaa0001"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), next) {
					t.Fatalf("bind=%v session over its lane's claim: %v", bind, err)
				}
			}
		}
	}
}

func TestReview2ClaimAssigneeCleanedBeforeLease(t *testing.T) {
	for _, bind := range []bool{false, true} {
		f := newLeaseFixture(t, bind)
		id := f.create("claim")
		w, err := orchclient.ClaimIssueAs(context.Background(), f.owner, id, "a\u200bb")
		if err != nil || w.Assignee != "ab" {
			t.Fatalf("bind=%v claim %+v %v", bind, w, err)
		}
	}
}

// An issue set back to open by hand ends its claim (claimed_by goes, the
// assignee stays): another session of the same person may claim it (bd lets
// the assignee's name decide); a claimed_by left by older builds counts for
// nothing on an open issue either. Seen on a handover: the successor
// set the bead open and was still refused "already claimed by another session".
func TestOpenIssueStaleClaimedByDoesNotBlock(t *testing.T) {
	for _, bind := range []bool{false, true} {
		f := newLeaseFixture(t, bind)
		ctx := context.Background()
		old := *f.owner
		old.As = "mini/aaaa0001"
		next := *f.owner
		next.As = "mini/bbbb0002"
		id := f.create("handed over")
		if _, err := orchclient.ClaimIssue(ctx, &old, id); err != nil {
			t.Fatalf("bind=%v first claim: %v", bind, err)
		}
		if _, err := orchclient.ClaimIssue(ctx, &next, id); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("bind=%v a live claim was taken: %v", bind, err)
		}
		open := orchapi.IssueOpen
		if _, err := orchclient.UpdateIssue(ctx, f.owner, id, orchapi.UpdateIssueRequest{Status: &open}); err != nil {
			t.Fatal(err)
		}
		if it := f.issue(id); it.Status != orchapi.IssueOpen || it.ClaimedBy != "" || it.Assignee != "ivo" {
			t.Fatalf("bind=%v set open: %s claimed_by %q assignee %q (the claim ends, the assignee stays)", bind, it.Status, it.ClaimedBy, it.Assignee)
		}
		if _, err := orchclient.ClaimIssue(ctx, &next, id); err != nil {
			t.Fatalf("bind=%v successor refused on an open issue: %v", bind, err)
		}
		if it := f.issue(id); it.Status != orchapi.IssueInProgress || it.ClaimedBy != "mini/bbbb0002" {
			t.Fatalf("bind=%v after the successor's claim: %s %q", bind, it.Status, it.ClaimedBy)
		}
		// Filed assigned to a session's own address, it is that session's to claim.
		mineByAddr := f.create("assigned to the session")
		addr := "mini/bbbb0002"
		if _, err := orchclient.UpdateIssue(ctx, f.owner, mineByAddr, orchapi.UpdateIssueRequest{Assignee: &addr}); err != nil {
			t.Fatal(err)
		}
		if _, err := orchclient.ClaimIssue(ctx, &old, mineByAddr); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("bind=%v another session took an issue assigned to mini/bbbb0002: %v", bind, err)
		}
		if _, err := orchclient.ClaimIssue(ctx, &next, mineByAddr); err != nil {
			t.Fatalf("bind=%v session refused its own assigned issue: %v", bind, err)
		}
		// Someone else's name on an open issue still decides.
		other := f.create("assigned to bea")
		bea := "bea"
		if _, err := orchclient.UpdateIssue(ctx, f.owner, other, orchapi.UpdateIssueRequest{Assignee: &bea}); err != nil {
			t.Fatal(err)
		}
		if _, err := orchclient.ClaimIssue(ctx, &next, other); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("bind=%v claimed over bea's assignment: %v", bind, err)
		}
	}
}
