package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues/store/sqlite"
)

// bd search ranks by relevance (bm25: title weighted most, then the body),
// best first with a score, and filters by label, status, parent and type, so
// an agent can check for an existing issue before filing one.
func TestShimSearchRanked(t *testing.T) {
	// The ranked pass has an 11 ms deadline in production, past which a search
	// falls back to unranked; a loaded runner overran it. Widen it here (before
	// the daemon starts, restored after it closes: cleanups run last first).
	t.Cleanup(sqlite.SetRankDeadlineForTest(time.Minute))
	s := newShim(t)
	id := func(args ...string) string {
		return strings.TrimSpace(s.ok(append([]string{"create", "--silent"}, args...)...))
	}
	for i := 0; i < 12; i++ { // a corpus where "weekend" is rare enough to weigh
		id("unrelated "+strings.Repeat("x", i+1), "-d", "nothing")
	}
	body := id("Fix the grid", "-d", strings.Repeat("month view columns ", 60)+"weekend")
	title := id("Weekend call target is wrong", "-d", "weekend target")
	epic := id("Epic", "-t", "epic")
	child := id("Weekends on the board", "--parent", epic, "-t", "bug", "-l", "perf")
	closed := id("Weekend fairness")
	s.ok("close", closed)

	out := s.ok("search", "weekend")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "Found 3 issues matching 'weekend', best first (score)") || len(lines) != 4 {
		t.Fatalf("ranked text:\n%s", out)
	}
	if !strings.Contains(lines[3], body) || strings.Contains(out, closed) || !strings.Contains(lines[1]+lines[2], title) {
		t.Fatalf("order: title matches first, the body-only match last, closed hidden:\n%s", out)
	}
	if f := strings.Fields(lines[1]); len(f) == 0 || !strings.Contains(f[0], ".") {
		t.Fatalf("no score on %q", lines[1])
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(s.ok("search", "weekend", "--json")), &got); err != nil || len(got) != 3 {
		t.Fatalf("json: %v %v", err, got)
	}
	for i, it := range got {
		sc, _ := it["score"].(float64)
		if sc <= 0 || i > 0 && sc > got[i-1]["score"].(float64) {
			t.Fatalf("json scores not best first: %v", got)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--label", "perf"}, child},
		{[]string{"--parent", epic}, child},
		{[]string{"--type", "bug"}, child},
		{[]string{"--status", "closed"}, closed},
	} {
		out := s.ok(append([]string{"search", "weekend", "--json"}, c.args...)...)
		var l []map[string]any
		_ = json.Unmarshal([]byte(out), &l)
		if len(l) != 1 || l[0]["id"] != c.want {
			t.Fatalf("search weekend %v: %s", c.args, out)
		}
	}
	if out := s.ok("search", "weekend", "--limit", "1"); !strings.Contains(out, "Found 3 issues") || !strings.Contains(out, "top 1 shown") {
		t.Fatalf("limit: %s", out)
	}
	// An issue id searches bd's way (substring of the id); so does --sort.
	if out := s.ok("search", child, "--json"); !strings.Contains(out, `"id": "`+child+`"`) || strings.Contains(out, `"score"`) {
		t.Fatalf("id search: %s", out)
	}
	if out := s.ok("search", strings.TrimPrefix(title, "oc-"), "--json"); !strings.Contains(out, `"id": "`+title+`"`) {
		t.Fatalf("bare hash search: %s", out)
	}
	if out := s.ok("search", "weekend", "--sort", "id"); strings.Contains(out, "best first") || !strings.Contains(out, title) {
		t.Fatalf("--sort: %s", out)
	}
}
