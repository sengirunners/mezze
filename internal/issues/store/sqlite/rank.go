package sqlite

import (
	"context"
	"sort"
	"strings"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// labelHas is List's label filter (one ? arg); labelIn is the same set
// driven from the label index, for a filter run once as a rowid set.
const (
	labelHas = "EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)"
	labelIn  = "i.id IN (SELECT l.issue_id FROM labels l WHERE l.label = ?)"
)

// rankWeights are bm25's column weights (title, description, notes): a
// word in the title says far more about what an issue is.
const rankWeights = "10.0, 1.0, 0.5"

// Ranking cost model (µs per row, from a generated 11.7k store): scanning
// one FTS match, finding and filtering its issue row by rowid, putting one
// candidate into a rowid set, and bm25 on one row. It only picks the plan
// and skips hopeless rankings (an estimate over rankBudget); bm25's real
// cost grows with how often the words occur in long bodies, which no count
// shows, so the ranked pass stops outright rankDeadline after the search
// began (the counts included), leaving the unranked page and enrichment the
// rest: the 16 ms bar holds by construction. Variables for tests.
var (
	costScan, costJoin, costSet, costRank = 0.1, 1.1, 0.45, 2.6
	rankBudget                            = 8000.0
	rankDeadline                          = 11 * time.Millisecond
)

// SetRankDeadlineForTest sets the ranked pass's deadline and returns a func
// restoring it. For tests in other packages that drive a daemon (the sqlite
// package's own tests set rankDeadline directly): call it before the daemon
// starts and restore after it closes, so no search reads it mid-change. A
// loaded runner can overrun the 11 ms production deadline, and the search then
// falls back to unranked. Production never calls it.
func SetRankDeadlineForTest(d time.Duration) (restore func()) {
	old := rankDeadline
	rankDeadline = d
	return func() { rankDeadline = old }
}

// ranked is sort=rank: bm25 over the FTS matches that pass the filters, best
// first, every scored match read in one pass (the total, and the page from
// the top). The plan follows the counts:
//   - few FTS matches: the FTS scan drives, each match's row found by rowid
//     and filtered;
//   - many matches but few issues pass the filters (counted only then): the
//     filters run once as a rowid set the FTS scan tests (the unary + keeps
//     FTS5 from a lookup per rowid; labels from their index), and the wide
//     rows are read only for the page;
//   - neither fits rankBudget, or the ranked pass runs past rankDeadline: too
//     common to rank in budget. The newest matches that pass come back
//     unranked (no Score; IssueList.Unranked); Total is the issues passing
//     the filters when counted (an upper bound), else 0 (unknown).
func (p *project) ranked(ctx context.Context, q orchapi.IssueQuery, where []string, args []any, limit int, vis istore.Areas) (orchapi.IssueList, error) {
	start := time.Now()
	if strings.TrimSpace(q.Q) == "" {
		return orchapi.IssueList{}, api.Errorf(api.CodeBadRequest, "sort=rank ranks a search: pass q (the words to search for)")
	}
	fts := ftsQuery(q.Q)
	if w := strings.Fields(fts); len(w) > 0 && len([]rune(strings.Trim(w[len(w)-1], `"*`))) < 2 {
		fts = strings.TrimSuffix(fts, "*") // a one-letter prefix expands to most of the vocabulary
	}
	if fts == "" {
		return orchapi.IssueList{Issues: []orchapi.Issue{}}, nil
	}
	filt := strings.Join(where, " AND ")
	var matches int
	if err := p.r.QueryRowContext(ctx, `SELECT count(*) FROM issues_fts WHERE issues_fts MATCH ?`, fts).Scan(&matches); err != nil || matches == 0 {
		return orchapi.IssueList{Issues: []orchapi.Issue{}}, err
	}
	m := float64(matches)
	joined := "issues_fts f JOIN issues i ON i.rowid = f.rowid"
	from, cond, unranked, bound := joined, filt, false, 0
	if m*(costScan+costJoin+costRank) > rankBudget {
		// Many matches: count the issues passing the filters, then take the
		// cheaper plan (bm25 runs on at most the smaller count), if any fits.
		setFilt := strings.ReplaceAll(filt, labelHas, labelIn)
		if err := p.r.QueryRowContext(ctx, `SELECT count(*) FROM issues i WHERE `+setFilt, args...).Scan(&bound); err != nil {
			return orchapi.IssueList{}, err
		}
		c := float64(bound)
		scored := min(m, c) * costRank
		join, set := m*(costScan+costJoin)+scored, c*costSet+m*costScan+scored
		switch {
		case min(join, set) > rankBudget:
			unranked = true
		case set < join:
			from, cond = "issues_fts f", "+f.rowid IN (SELECT i.rowid FROM issues i WHERE "+setFilt+")"
		}
	}
	fargs := append([]any{fts}, args...)
	var ids []any
	score := map[int64]float64{}
	total := 0
	if !unranked {
		// Every scored match in one pass (no second scan to count them): the
		// total, and the page from the top. Past the deadline: unranked.
		rctx, cancel := context.WithDeadline(ctx, start.Add(rankDeadline))
		bm := "bm25(issues_fts, " + rankWeights + ")"
		err := func() error {
			rows, err := p.r.QueryContext(rctx, `SELECT f.rowid, -`+bm+` FROM `+from+` WHERE issues_fts MATCH ? AND `+cond+` ORDER BY `+bm+`, f.rowid`, fargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var rid int64
				var sc float64
				if err := rows.Scan(&rid, &sc); err != nil {
					return err
				}
				total++
				if limit == 0 || len(ids) < limit+1 {
					ids = append(ids, rid)
					score[rid] = sc
				}
			}
			return rows.Err()
		}()
		cancel()
		if err != nil && ctx.Err() == nil && rctx.Err() != nil {
			unranked, ids, total, score = true, nil, 0, map[int64]float64{}
		} else if err != nil {
			return orchapi.IssueList{}, err
		}
	}
	if unranked {
		// The newest matches that pass the filters.
		total = bound
		sqlText := `SELECT f.rowid FROM ` + joined + ` WHERE issues_fts MATCH ? AND ` + filt + ` ORDER BY f.rowid DESC`
		if limit > 0 {
			sqlText += ` LIMIT ?`
			fargs = append(fargs, limit+1)
		}
		rows, err := p.r.QueryContext(ctx, sqlText, fargs...)
		if err != nil {
			return orchapi.IssueList{}, err
		}
		for rows.Next() {
			var rid int64
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return orchapi.IssueList{}, err
			}
			ids = append(ids, rid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return orchapi.IssueList{}, err
		}
	}
	more := false
	if limit > 0 && len(ids) > limit {
		ids, more = ids[:limit], true
	}
	its := make([]orchapi.Issue, 0, len(ids))
	order := make(map[int64]int, len(ids))
	for i, id := range ids {
		order[id.(int64)] = i
	}
	pos := make(map[string]int, len(ids))
	if len(ids) > 0 {
		r2, err := p.r.QueryContext(ctx, `SELECT i.rowid, `+issueCols+` FROM issues i WHERE i.rowid IN (`+placeholders(len(ids))+`)`, ids...)
		if err != nil {
			return orchapi.IssueList{}, err
		}
		for r2.Next() {
			var rid int64
			it, err := scanIssue(scanFunc(func(dst ...any) error { return r2.Scan(append([]any{&rid}, dst...)...) }), p.id)
			if err != nil {
				r2.Close()
				return orchapi.IssueList{}, err
			}
			it.Score = score[rid]
			pos[it.ID] = order[rid]
			its = append(its, it)
		}
		r2.Close()
		if err := r2.Err(); err != nil {
			return orchapi.IssueList{}, err
		}
	}
	// bm25's order (ties by rowid: creation order), as the scan returned it.
	sort.Slice(its, func(i, j int) bool { return pos[its[i].ID] < pos[its[j].ID] })
	if err := enrich(ctx, p.r, p.id, its, vis, q.Deps); err != nil {
		return orchapi.IssueList{}, err
	}
	if err := enrichMore(ctx, p.r, its, vis, q.Incoming, q.Comments); err != nil {
		return orchapi.IssueList{}, err
	}
	return orchapi.IssueList{Issues: its, More: more, Total: total, Unranked: unranked}, nil
}
