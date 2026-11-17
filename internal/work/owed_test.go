package work

import (
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
)

func owedKinds(p orchcore.OwedPart) map[string]int {
	out := map[string]int{}
	for _, it := range p.Items {
		out[it.Kind]++
	}
	return out
}

// What a session still owes (SPEC 8), the work part: the item a lease holds
// and its open PR; a last-held item only while no one else holds it now; an
// item that cannot be read is a gap, never a quiet drop.
func TestOwedWorkRows(t *testing.T) {
	h := newHarness(t)
	a := h.session("mini/aaaa1111", h.harbor.ID, "solver", true, orchapi.StateRunning)
	b := h.session("mini/bbbb2222", h.harbor.ID, "solver", true, orchapi.StateRunning)
	ca := h.as(a, "harbor/solver")
	it, err := orchclient.CreateWork(h.ctx, ca, orchapi.CreateWorkRequest{Title: "tidy the queue", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, ca, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Link(h.ctx, ca, it.ID, orchapi.Link{Rel: "pr", Target: "https://example.test/o/r/pull/5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetStatus(h.ctx, ca, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); err != nil {
		t.Fatal(err)
	}
	sa := orchapi.Session{Address: "mini/aaaa1111", Project: h.harbor.ID, Area: "solver"}
	sb := orchapi.Session{Address: "mini/bbbb2222", Project: h.harbor.ID, Area: "solver", Work: it.ID} // b only last held it

	part, err := h.s.owed(h.ctx, a, a.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{sa}})
	if err != nil {
		t.Fatal(err)
	}
	if k := owedKinds(part); k[orchapi.OwedWork] != 1 || k[orchapi.OwedPR] != 1 || len(part.Gaps) != 0 {
		t.Fatalf("holder's rows: %+v", part)
	}
	// b holds nothing: a's item is not b's, though b's record names it.
	if part, err = h.s.owed(h.ctx, b, b.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{sb}}); err != nil || len(part.Items) != 0 || len(part.Gaps) != 0 {
		t.Fatalf("last-held item another session holds now: %+v %v", part, err)
	}
	// An item that cannot be read is said, and the rest of the list stands.
	if _, err := h.d.Store.Writer(h.harbor.ID).Exec(`DROP TABLE work_items`); err != nil {
		t.Fatal(err)
	}
	part, err = h.s.owed(h.ctx, a, a.Scope, orchcore.OwedWho{Sessions: []orchapi.Session{sa}})
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Items) != 0 || len(part.Gaps) != 1 || part.Gaps[0].Kind != orchapi.OwedGapPartial || part.Gaps[0].Name != "work" {
		t.Fatalf("an unreadable item must be a gap: %+v", part)
	}
}

// A claim log that has been pruned past the window's start is said: an older
// claim of the session may be gone. A log that still reaches back is not.
func TestOwedPrunedClaimHistoryIsUnknown(t *testing.T) {
	h := newHarness(t)
	a := h.session("mini/aaaa1111", h.harbor.ID, "solver", true, orchapi.StateRunning)
	who := orchcore.OwedWho{Sessions: []orchapi.Session{{Address: "mini/aaaa1111", Project: h.harbor.ID}}}
	part, err := h.s.owed(h.ctx, a, a.Scope, who)
	if err != nil || len(part.Gaps) != 0 {
		t.Fatalf("a log that reaches back: %+v %v", part, err)
	}
	// A full log whose oldest event is newer than the window: older ones were pruned.
	if _, err := h.d.Store.Writer(h.harbor.ID).Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 20000)
		INSERT INTO events (id, type, subject, area, time, mentions, json) SELECT 'z' || i, 'dev.zm.test', '', '', ?, '[]', '{}' FROM n`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if part, err = h.s.owed(h.ctx, a, a.Scope, who); err != nil || len(part.Gaps) != 1 || part.Gaps[0].Name != "work" {
		t.Fatalf("a pruned log must be a gap: %+v %v", part, err)
	}
	// A session that started after the oldest event left could not have claimed
	// before it: nothing it did is lost to pruning.
	started := orchcore.OwedWho{Sessions: []orchapi.Session{{Address: "mini/aaaa1111", Project: h.harbor.ID, Started: time.Now().Add(time.Minute)}}}
	if part, err = h.s.owed(h.ctx, a, a.Scope, started); err != nil || len(part.Gaps) != 0 {
		t.Fatalf("a session younger than the log's start: %+v %v", part, err)
	}
}

// A pull request whose item no lease holds now is the session's that last
// claimed it, whether or not any replica's presence recorded the item: the
// claim history says so. Another session's later claim takes it over.
func TestOwedPRFromClaimHistory(t *testing.T) {
	h := newHarness(t)
	a := h.session("mini/aaaa1111", h.harbor.ID, "solver", true, orchapi.StateRunning)
	b := h.session("mini/bbbb2222", h.harbor.ID, "solver", true, orchapi.StateRunning)
	ca, cb := h.as(a, "harbor/solver"), h.as(b, "harbor/solver")
	it, err := orchclient.CreateWork(h.ctx, ca, orchapi.CreateWorkRequest{Title: "tidy the queue", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, ca, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Link(h.ctx, ca, it.ID, orchapi.Link{Rel: "pr", Target: "https://example.test/o/r/pull/7"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetStatus(h.ctx, ca, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Release(h.ctx, ca, it.ID, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	who := func(addr string) orchcore.OwedWho { // the session record names no item
		return orchcore.OwedWho{Sessions: []orchapi.Session{{Address: addr, Project: h.harbor.ID, Area: "solver"}}}
	}
	part, err := h.s.owed(h.ctx, a, a.Scope, who("mini/aaaa1111"))
	if err != nil {
		t.Fatal(err)
	}
	if k := owedKinds(part); k[orchapi.OwedPR] != 1 || k[orchapi.OwedWork] != 0 || len(part.Gaps) != 0 {
		t.Fatalf("the last claimer's PR: %+v", part)
	}
	if part, err = h.s.owed(h.ctx, b, b.Scope, who("mini/bbbb2222")); err != nil || len(part.Items) != 0 {
		t.Fatalf("a session that never claimed it: %+v %v", part, err)
	}
	// b claims it: the PR is b's now, and a no longer owes it.
	if _, err := orchclient.Claim(h.ctx, cb, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if part, err = h.s.owed(h.ctx, a, a.Scope, who("mini/aaaa1111")); err != nil || len(part.Items) != 0 {
		t.Fatalf("an item another session holds: %+v %v", part, err)
	}
	// A claim history that cannot be read is said, never a quiet gap.
	if _, err := h.d.Store.Writer(h.harbor.ID).Exec(`DROP TABLE events`); err != nil {
		t.Fatal(err)
	}
	if part, err = h.s.owed(h.ctx, a, a.Scope, who("mini/aaaa1111")); err != nil || len(part.Gaps) != 1 || part.Gaps[0].Name != "work" {
		t.Fatalf("an unreadable claim history must be a gap: %+v %v", part, err)
	}
}
