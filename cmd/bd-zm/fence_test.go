package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// bd-zm's --fence reaches the native door: update --claim --json answers
// lease_fence, a write with it stands while the lease does, one with a stale
// fence is refused and writes nothing, and a fence with several ids is
// refused before anything is written.
func TestShimFence(t *testing.T) {
	s := newShim(t)
	ctx := context.Background()
	a := strings.TrimSpace(s.ok("create", "fenced a", "--silent"))
	b := strings.TrimSpace(s.ok("create", "fenced b", "--silent"))

	if _, errOut, code := s.bd("close", a, b, "--fence", "5"); code == 0 || !strings.Contains(errOut, "--fence names one lease") {
		t.Fatalf("close two ids with a fence: exit %d %s", code, errOut)
	}
	// No native binding: no lease, so any fence is stale and nothing is written.
	if _, errOut, code := s.bd("close", a, "--fence", "5"); code == 0 || !strings.Contains(errOut, "no longer live") {
		t.Fatalf("close with a fence and no lease: exit %d %s", code, errOut)
	}
	if _, _, code := s.bd("comments", "add", a, "late", "--fence", "5"); code == 0 {
		t.Fatal("a fenced comment with no lease was accepted")
	}
	if cs, err := orchclient.IssueComments(ctx, s.c, a); err != nil || len(cs) != 0 {
		t.Fatalf("comments after a refused fenced comment: %+v %v", cs, err)
	}
	if _, errOut, code := s.bd("update", a, "--title", "renamed late", "--fence", "5"); code == 0 || !strings.Contains(errOut, "no longer live") {
		t.Fatalf("update with a stale fence: exit %d %s", code, errOut)
	}
	if it, _ := orchclient.Issue(ctx, s.c, a); it.Issue.Title != "fenced a" {
		t.Fatalf("a refused fenced update wrote the title: %q", it.Issue.Title)
	}
	for _, id := range []string{a, b} {
		if it, err := orchclient.Issue(ctx, s.c, id); err != nil || it.Issue.Status != orchapi.IssueOpen {
			t.Fatalf("%s after refused writes: %+v %v", id, it.Issue.Status, err)
		}
	}

	if res, err := orchclient.AddTracker(ctx, s.c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	var claimed []struct {
		LeaseFence int64 `json:"lease_fence"`
	}
	if err := json.Unmarshal([]byte(s.ok("update", a, "--claim", "--json")), &claimed); err != nil || len(claimed) != 1 || claimed[0].LeaseFence <= 0 {
		t.Fatalf("update --claim --json: %+v %v", claimed, err)
	}
	f := strconv.FormatInt(claimed[0].LeaseFence, 10)
	s.ok("comments", "add", a, "under the lease", "--fence", f)
	if _, _, code := s.bd("close", a, "--fence", strconv.FormatInt(claimed[0].LeaseFence+1, 10)); code == 0 {
		t.Fatal("close with another fence")
	}
	s.ok("close", a, "--fence", f)
	if it, _ := orchclient.Issue(ctx, s.c, a); it.Issue.Status != orchapi.IssueClosed {
		t.Fatalf("after the fenced close: %s", it.Issue.Status)
	}
}
