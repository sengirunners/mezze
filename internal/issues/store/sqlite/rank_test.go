package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// sort=rank: bm25 over title (weighted most), description and notes, best
// first with a score; the list's filters narrow the candidates first.
func TestRankedSearch(t *testing.T) {
	// Ranking here is about order, not speed: a slow runner (or -race) must
	// not push the pass past the 11 ms deadline (the deadline case is below).
	defer func(d time.Duration) { rankDeadline = d }(rankDeadline)
	rankDeadline = time.Second
	ctx := context.Background()
	p := testProject(t)
	all := istore.Areas{All: true}
	now := time.Now()
	long := strings.Repeat("grid month view columns rows ", 40)
	a := create(t, p, orchapi.NewIssueRequest{Title: "Weekend call target is wrong", Description: "the weekend target"})
	b := create(t, p, orchapi.NewIssueRequest{Title: "Fix the grid", Description: long + " weekend " + long})
	c := create(t, p, orchapi.NewIssueRequest{Title: "Weekend fairness", Description: "weekends"})
	apply(t, p, istore.Mutation{Op: istore.OpClose, ID: c.ID, Close: &orchapi.CloseIssueRequest{}})
	epic := create(t, p, orchapi.NewIssueRequest{Title: "Epic", IssueType: "epic"})
	d := create(t, p, orchapi.NewIssueRequest{Title: "Weekends on the board", Parent: epic.ID, IssueType: "bug", Labels: []string{"perf"}})
	for i := 0; i < 12; i++ { // a corpus where "weekend" is rare enough to weigh (bm25's idf)
		create(t, p, orchapi.NewIssueRequest{Title: "Unrelated " + strings.Repeat("x", i+1), Description: "nothing here"})
	}

	list := func(q orchapi.IssueQuery) orchapi.IssueList {
		t.Helper()
		q.Sort = "rank"
		l, err := p.List(ctx, q, all, now)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return l
	}
	l := list(orchapi.IssueQuery{Q: "weekend"})
	if got := idList(l.Issues); l.Total != 3 || l.Unranked || !strings.HasSuffix(got, b.ID) || strings.Contains(got, c.ID) {
		t.Fatalf("rank weekend: total %d unranked %v order %s (body-only %s last, closed %s hidden)", l.Total, l.Unranked, got, b.ID, c.ID)
	}
	for i, it := range l.Issues {
		if it.Score <= 0 || i > 0 && it.Score > l.Issues[i-1].Score {
			t.Fatalf("scores not best first: %v", scores(l.Issues))
		}
	}
	if l.Issues[0].ID != a.ID && l.Issues[0].ID != d.ID {
		t.Fatalf("a title match first: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Status: "all"}); l.Total != 4 {
		t.Fatalf("status all: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Labels: []string{"perf"}}); idList(l.Issues) != d.ID {
		t.Fatalf("label: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Parent: epic.ID}); idList(l.Issues) != d.ID {
		t.Fatalf("parent: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Type: "bug"}); idList(l.Issues) != d.ID {
		t.Fatalf("type: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Status: "closed"}); idList(l.Issues) != c.ID {
		t.Fatalf("status closed: %s", idList(l.Issues))
	}
	if l := list(orchapi.IssueQuery{Q: "weekend", Limit: 1}); len(l.Issues) != 1 || !l.More || l.Total != 3 {
		t.Fatalf("limit 1: %d more %v total %d", len(l.Issues), l.More, l.Total)
	}
	if l := list(orchapi.IssueQuery{Q: "zzzq"}); len(l.Issues) != 0 || l.Total != 0 {
		t.Fatalf("no match: %+v", l)
	}
	if _, err := p.List(ctx, orchapi.IssueQuery{Sort: "rank"}, all, now); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "pass q") {
		t.Fatalf("rank without q: %v", err)
	}
	// With a budget the four matches overrun: few issues pass the label
	// filter (one), so the filters run first as a rowid set, still ranked;
	// without a filter it is too common to rank: the newest matches, no
	// score, a total that is an upper bound.
	defer func(b float64) { rankBudget = b }(rankBudget)
	rankBudget = 5
	if l := list(orchapi.IssueQuery{Q: "weekend", Labels: []string{"perf"}}); idList(l.Issues) != d.ID || l.Issues[0].Score <= 0 || l.Unranked || l.Total != 1 {
		t.Fatalf("few candidates: %s %v unranked %v total %d", idList(l.Issues), scores(l.Issues), l.Unranked, l.Total)
	}
	l = list(orchapi.IssueQuery{Q: "weekend"})
	if !l.Unranked || l.Total < 3 || idList(l.Issues) != d.ID+" "+b.ID+" "+a.ID || l.Issues[0].Score != 0 {
		t.Fatalf("over the budget: unranked %v total %d %s %v", l.Unranked, l.Total, idList(l.Issues), scores(l.Issues))
	}
	// A ranked pass past its deadline stops: unranked, the total unknown (0).
	rankBudget = 8000
	rankDeadline = time.Nanosecond
	l = list(orchapi.IssueQuery{Q: "weekend"})
	if !l.Unranked || l.Total != 0 || idList(l.Issues) != d.ID+" "+b.ID+" "+a.ID {
		t.Fatalf("past the deadline: unranked %v total %d %s", l.Unranked, l.Total, idList(l.Issues))
	}
	// A one-letter last word is a word, not a prefix of most of the vocabulary.
	rankDeadline = time.Second
	create(t, p, orchapi.NewIssueRequest{Title: "Plan B for the board"})
	if l := list(orchapi.IssueQuery{Q: "b"}); l.Total != 1 {
		t.Fatalf("one-letter word: %d matches %s", l.Total, idList(l.Issues))
	}
}

func scores(its []orchapi.Issue) []float64 {
	out := make([]float64, len(its))
	for i, it := range its {
		out[i] = it.Score
	}
	return out
}

// The 16 ms bar for a ranked search on a store shaped like harbor's
// (~11.7k issues, ~80% closed, bodies of hundreds of words, common words
// in most issues). Timing noise only slows a run, so the best of three
// runs' p95 is held to the bar. A timing test: bench.yml runs it
// (ZM_RANK_SPEED=1), never make check. With ZM_RANK_JSON=<path> it writes
// its cases as rows of bench.json (scripts/bench's result, rank/<case>) and an
// over-the-bar case stops failing: bench repeat judges the rows. Run by hand
// without it, it fails as before.
func TestRankedSearchSpeed(t *testing.T) {
	if os.Getenv("ZM_RANK_SPEED") == "" {
		t.Skip("ZM_RANK_SPEED not set (bench.yml sets it): generates 11.7k issues and times them")
	}
	rowsPath := os.Getenv("ZM_RANK_JSON")
	overBar := t.Errorf
	if rowsPath != "" {
		overBar = t.Logf
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	type benchRow struct {
		Name   string  `json:"name"`
		N      int     `json:"n"`
		P50    float64 `json:"p50_ms"`
		P95    float64 `json:"p95_ms"`
		Max    float64 `json:"max_ms"`
		Budget float64 `json:"budget_ms"`
		OK     bool    `json:"ok"`
		Tries  int     `json:"tries"`
		First  float64 `json:"first_p95_ms"`
	}
	var benchRows []benchRow
	p, done := genStore(t, 11700)
	defer done()
	ctx := context.Background()
	all := istore.Areas{All: true}
	now := time.Now()
	open := "open,in_progress,in_review,blocked,deferred,pinned,hooked"
	cases := []struct {
		name string
		q    orchapi.IssueQuery
	}{
		{"common word", orchapi.IssueQuery{Q: "call", Status: open}},
		{"common word, all statuses", orchapi.IssueQuery{Q: "call", Status: "all"}},
		{"two words", orchapi.IssueQuery{Q: "solver timeout", Status: open}},
		{"two words, all statuses", orchapi.IssueQuery{Q: "weekend target", Status: "all"}},
		{"rare word", orchapi.IssueQuery{Q: genVocab[3000], Status: "all"}},
		{"label filter", orchapi.IssueQuery{Q: "weekend", Status: "all", Labels: []string{"perf"}}},
		{"type filter", orchapi.IssueQuery{Q: "board", Status: open, Type: "bug"}},
		{"everything", orchapi.IssueQuery{Q: "the", Status: "all"}},
	}
	rows := []string{"| ranked search (11.7k issues) | total | unranked | p95 |", "| --- | --- | --- | --- |"}
	defer func() {
		// On CI, the numbers in the job summary.
		if f := os.Getenv("GITHUB_STEP_SUMMARY"); f != "" {
			if w, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
				fmt.Fprintln(w, strings.Join(rows, "\n")+"\n")
				w.Close()
			}
		}
	}()
	for _, c := range cases {
		c.q.Sort, c.q.Limit, c.q.Count = "rank", 10, true
		best := time.Hour
		row := benchRow{Name: "rank/" + c.name, N: 100, Budget: 16}
		var l orchapi.IssueList
		for try := 0; try < 3 && best >= 16*time.Millisecond; try++ {
			ds := make([]time.Duration, 0, 100)
			for i := 0; i < 100; i++ {
				t0 := time.Now()
				var err error
				if l, err = p.List(ctx, c.q, all, now); err != nil {
					t.Fatal(err)
				}
				ds = append(ds, time.Since(t0))
			}
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			if try == 0 {
				row.First = ms(ds[95])
			}
			row.Tries = try + 1
			if ds[95] < best {
				best = ds[95]
				row.P50, row.P95, row.Max = ms(ds[50]), ms(ds[95]), ms(ds[99])
			}
		}
		row.OK = best < 16*time.Millisecond
		benchRows = append(benchRows, row)
		t.Logf("%-28s matches %5d unranked %-5v p95 %v", c.name, l.Total, l.Unranked, best)
		rows = append(rows, fmt.Sprintf("| %s | %d | %v | %.2f ms |", c.name, l.Total, l.Unranked, float64(best.Microseconds())/1000))
		if !row.OK {
			overBar("%s: ranked search p95 %v, over the 16 ms bar on 11.7k issues (fix: rankDeadline in rank.go bounds the ranked pass; look at what runs outside it: the counts, the unranked page, enrichment)", c.name, best)
		}
	}
	if rowsPath != "" {
		b, err := json.MarshalIndent(benchRows, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rowsPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
