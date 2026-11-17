// Package rolluptest is the conformance suite every rollup.Table driver passes
// (docs/ARCHITECTURE.md rule 1): each rule of the rollup, the reads' query plans
// and the read's speed on 12k items, on the table its owner describes.
package rolluptest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/rollup"
)

var now = time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)

// Row is one work item as the suite writes it.
type Row struct {
	Ref, Parent, Type, Status, Tracker, Area, Due, Deferred string
	Priority                                                int // -1: none
	Labels                                                  []string
	At                                                      time.Time
	Body                                                    string
}

// Driver is a table under test. Create makes the table as its owner's
// migrations leave it, before Table.IndexSQL; Insert writes one row of it from
// an item as it arrives (source bd, id bd:<ref>).
type Driver struct {
	Table  rollup.Table
	Create string
	Insert func(tx *sql.Tx, id, ref, area, status string, updated int64, title string, labels []string, item map[string]any) error
	// Children: the owner also runs Table.ChildIndexSQL (it serves
	// SQL.Owned), so Owned's read must come from that index.
	Children bool
}

// Run runs the suite.
func Run(t *testing.T, d Driver) {
	for name, fn := range map[string]func(*testing.T, Driver){
		"counts":      countsRollUp,
		"closed":      closedEpics,
		"loops":       loopsAndFlat,
		"hidden":      hiddenArea,
		"deadlines":   deadlines,
		"claims":      claims,
		"claimcounts": claimCounts,
		"owned":       owned,
		"plans":       plans,
		"speed":       speed12k,
	} {
		t.Run(name, func(t *testing.T) { fn(t, d) })
	}
}

func (d Driver) open(t testing.TB) *sql.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = os.RemoveAll(dir) })
	schema := d.Create + ";" + d.Table.IndexSQL()
	if d.Children {
		schema += d.Table.ChildIndexSQL()
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func (d Driver) put(t testing.TB, db *sql.DB, rows ...Row) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		extra := map[string]any{}
		for k, v := range map[string]string{"parent": r.Parent, "issue_type": r.Type, "status": r.Tracker, "due_at": r.Due, "defer_until": r.Deferred} {
			if v != "" {
				extra[k] = v
			}
		}
		if r.Priority >= 0 {
			extra["priority"] = r.Priority
		}
		at := r.At
		if at.IsZero() {
			at = now.Add(-time.Hour)
		}
		labels := r.Labels
		if labels == nil {
			labels = []string{}
		}
		item := map[string]any{"extra": extra, "updated": at.Format(time.RFC3339Nano), "labels": labels, "title": "T " + r.Ref, "body": r.Body}
		if err := d.Insert(tx, "bd:"+r.Ref, r.Ref, r.Area, r.Status, at.UnixMilli(), "T "+r.Ref, labels, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, s rollup.Source) orchapi.WorkRollup {
	t.Helper()
	r, err := rollup.Build(context.Background(), s, "P", "p", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func nodeOf(t *testing.T, r orchapi.WorkRollup, id string) orchapi.RollupNode {
	t.Helper()
	for _, n := range r.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node %s in %+v", id, r.Nodes)
	return orchapi.RollupNode{}
}

func hasNode(r orchapi.WorkRollup, id string) bool {
	for _, n := range r.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func countsRollUp(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	late := now.Add(-time.Minute)
	put(
		Row{Ref: "th", Type: "epic", Status: "open", Tracker: "open", Priority: 2, Labels: []string{"theme"}},
		Row{Ref: "ep", Type: "epic", Status: "open", Tracker: "open", Parent: "th", Priority: 2},
		Row{Ref: "ep2", Type: "epic", Status: "open", Tracker: "open", Parent: "th", Priority: 2},
		// Under ep: every bucket.
		Row{Ref: "a", Type: "task", Status: "open", Tracker: "open", Parent: "ep", Priority: 0},
		Row{Ref: "b", Type: "task", Status: "open", Tracker: "open", Parent: "ep", Priority: 3},
		Row{Ref: "c", Type: "bug", Status: "claimed", Tracker: "in_progress", Parent: "ep", Priority: 1},
		Row{Ref: "d", Type: "bug", Status: "in_review", Tracker: "in_review", Parent: "ep", Priority: 1},
		Row{Ref: "e", Type: "bug", Status: "open", Tracker: "blocked", Parent: "ep", Priority: 1, At: late},
		Row{Ref: "f", Type: "bug", Status: "open", Tracker: "deferred", Parent: "ep", Priority: 0},
		Row{Ref: "g", Type: "bug", Status: "done", Tracker: "closed", Parent: "ep", Priority: 0},
		// A task's own sub-task counts for the epic above it.
		Row{Ref: "c.1", Type: "task", Status: "open", Tracker: "open", Parent: "c", Priority: 2},
		// Under ep2 and directly under the theme.
		Row{Ref: "h", Type: "task", Status: "open", Tracker: "open", Parent: "ep2", Priority: 1},
		Row{Ref: "i", Type: "task", Status: "done", Tracker: "closed", Parent: "th", Priority: 2},
		// No epic: none above, and a parent that does not exist.
		Row{Ref: "j", Type: "task", Status: "open", Tracker: "open", Priority: 1},
		Row{Ref: "k", Type: "task", Status: "open", Tracker: "open", Parent: "gone", Priority: 3},
	)
	r := build(t, rollup.SQL{DB: db, Table: tb})
	ep := nodeOf(t, r, "bd:ep")
	if ep.Kind != orchapi.RollupEpic || ep.Parent != "bd:th" || ep.Title != "T ep" {
		t.Fatalf("epic %+v", ep)
	}
	want := orchapi.RollupCounts{Total: 8, Open: 3, Urgent: 2, Working: 2, Blocked: 1, Deferred: 1, Done: 1, Changed: late}
	if got := ep.RollupCounts; !got.Changed.Equal(late) || strip(got) != strip(want) {
		t.Fatalf("epic counts %+v, want %+v", got, want)
	}
	th := nodeOf(t, r, "bd:th")
	want = orchapi.RollupCounts{Total: 10, Open: 4, Urgent: 3, Working: 2, Blocked: 1, Deferred: 1, Done: 2}
	if th.Kind != orchapi.RollupTheme || th.Parent != "" || strip(th.RollupCounts) != strip(want) {
		t.Fatalf("theme %+v", th)
	}
	if strings.Join(th.Children, ",") != "bd:ep,bd:ep2" {
		t.Fatalf("children in list order (urgent first): %v", th.Children)
	}
	if r.NoEpic.Total != 2 || r.NoEpic.Open != 2 || r.NoEpic.Urgent != 1 {
		t.Fatalf("no epic %+v", r.NoEpic)
	}
	if r.Totals.Total != 12 {
		t.Fatalf("totals count no epic rows: %+v", r.Totals)
	}
	if got := r.Totals; got.Total != got.Open+got.Working+got.Blocked+got.Deferred+got.Done {
		t.Fatalf("buckets do not add up: %+v", got)
	}
	if r.Nodes[0].ID != "bd:th" {
		t.Fatalf("most urgent first: %+v", r.Nodes)
	}
}

func strip(c orchapi.RollupCounts) orchapi.RollupCounts { c.Changed = time.Time{}; return c }

func closedEpics(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	put(
		Row{Ref: "th", Type: "epic", Status: "open", Labels: []string{"theme"}, Priority: -1},
		Row{Ref: "shut", Type: "epic", Status: "done", Parent: "th", Priority: -1},
		Row{Ref: "x", Type: "task", Status: "done", Parent: "shut", Priority: -1},
		Row{Ref: "orphan", Type: "epic", Status: "done", Priority: -1},
		Row{Ref: "y", Type: "task", Status: "open", Parent: "orphan", Priority: -1},
		Row{Ref: "empty", Type: "epic", Status: "open", Priority: -1},
		Row{Ref: "empty-theme", Type: "epic", Status: "open", Labels: []string{"theme"}, Priority: -1},
		// Labelled theme below an open epic: an epic there.
		Row{Ref: "inner", Type: "epic", Status: "open", Parent: "th", Labels: []string{"theme"}, Priority: -1},
		Row{Ref: "z", Type: "task", Status: "open", Parent: "inner", Priority: -1},
	)
	r := build(t, rollup.SQL{DB: db, Table: tb})
	if n := nodeOf(t, r, "bd:th"); n.Done != 1 || n.Open != 1 || n.Total != 2 {
		t.Fatalf("a closed epic's items count for the open one above: %+v", n)
	}
	if hasNode(r, "bd:shut") || hasNode(r, "bd:orphan") || hasNode(r, "bd:empty") {
		t.Fatalf("closed or empty epics listed: %+v", r.Nodes)
	}
	if r.NoEpic.Open != 1 {
		t.Fatalf("an open item under a closed epic with nothing above is no_epic: %+v", r.NoEpic)
	}
	if n := nodeOf(t, r, "bd:empty-theme"); n.Kind != orchapi.RollupTheme {
		t.Fatalf("an empty theme stays: %+v", n)
	}
	if n := nodeOf(t, r, "bd:inner"); n.Kind != orchapi.RollupEpic || n.Parent != "bd:th" {
		t.Fatalf("a theme label below an epic: %+v", n)
	}
}

// A tracker's parents may loop, or it may report no hierarchy at all.
func loopsAndFlat(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	put(
		Row{Ref: "a", Type: "epic", Status: "open", Parent: "b", Priority: -1},
		Row{Ref: "b", Type: "epic", Status: "open", Parent: "a", Priority: -1},
		Row{Ref: "t1", Type: "task", Status: "open", Parent: "t2", Priority: -1},
		Row{Ref: "t2", Type: "task", Status: "open", Parent: "t1", Priority: -1},
		Row{Ref: "l", Type: "task", Status: "open", Parent: "a", Priority: -1},
		Row{Ref: "flat", Status: "open", Priority: -1},
	)
	r := build(t, rollup.SQL{DB: db, Table: tb})
	if r.Totals.Total != 4 || r.NoEpic.Total != 3 {
		t.Fatalf("totals %+v no epic %+v", r.Totals, r.NoEpic)
	}
	if n := nodeOf(t, r, "bd:a"); n.Total < 1 {
		t.Fatalf("looped epic %+v", n)
	}
}

// Out of sight means gone: no id, no count, no shape.
func hiddenArea(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	put(
		Row{Ref: "secret-theme", Type: "epic", Status: "open", Area: "billing", Labels: []string{"theme"}, Priority: -1},
		Row{Ref: "secret-task", Type: "task", Status: "open", Area: "billing", Parent: "secret-theme", Priority: 0, Due: now.Format(time.RFC3339)},
		Row{Ref: "ep", Type: "epic", Status: "open", Area: "solver", Parent: "secret-theme", Priority: -1},
		Row{Ref: "a", Type: "task", Status: "open", Area: "solver", Parent: "ep", Priority: 1},
		Row{Ref: "b", Type: "task", Status: "open", Area: "solver", Parent: "secret-task", Priority: 1},
	)
	r := build(t, rollup.SQL{DB: db, Table: tb, Where: "area = ?", Args: []any{"solver"}})
	js, _ := json.Marshal(r)
	if strings.Contains(string(js), "secret") {
		t.Fatalf("hidden item named: %s", js)
	}
	if n := nodeOf(t, r, "bd:ep"); n.Parent != "" || n.Total != 1 || n.Kind != orchapi.RollupEpic {
		t.Fatalf("visible epic under a hidden theme: %+v", n)
	}
	if len(r.Nodes) != 1 || r.NoEpic.Total != 1 || r.Totals.Total != 2 || r.Totals.Urgent != 2 || len(r.Deadlines) != 0 {
		t.Fatalf("hidden items counted: %s", js)
	}
}

func deadlines(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	put(
		// Typed in Pacific: midnight there is 07:00Z. Due "today" is not overdue.
		Row{Ref: "today", Status: "open", Priority: 2, Due: "2026-11-03T08:00:00Z"},
		Row{Ref: "late", Status: "claimed", Priority: 2, Due: "2026-11-01T07:00:00Z"},
		Row{Ref: "soon-low", Status: "open", Priority: 3, Due: "2026-11-09T00:00:00Z"},
		Row{Ref: "soon-high", Status: "open", Priority: 0, Due: "2026-11-09T00:00:00Z"},
		Row{Ref: "done", Status: "done", Priority: 0, Due: "2026-11-01T00:00:00Z"},
		Row{Ref: "back", Status: "open", Priority: 2, Tracker: "deferred", Deferred: "2026-11-06T00:00:00Z"},
		Row{Ref: "far", Status: "open", Priority: 2, Tracker: "deferred", Deferred: "2026-12-25T00:00:00Z"},
		Row{Ref: "past", Status: "open", Priority: 2, Tracker: "deferred", Deferred: "2026-10-01T00:00:00Z"},
		Row{Ref: "both", Status: "open", Priority: 2, Due: "2026-11-20T00:00:00Z", Deferred: "2026-11-06T00:00:00Z"},
	)
	r := build(t, rollup.SQL{DB: db, Table: tb})
	var ids []string
	for _, x := range r.Deadlines {
		ids = append(ids, strings.TrimPrefix(x.ID, "bd:"))
	}
	if got := strings.Join(ids, ","); got != "late,today,soon-high,soon-low,both,back" {
		t.Fatalf("order %s", got)
	}
	dl := r.Deadlines
	if !dl[0].Overdue || dl[0].Day != "2026-11-01" || dl[1].Overdue || dl[1].Day != "2026-11-03" || dl[1].DueAt == nil {
		t.Fatalf("overdue: %+v %+v", dl[0], dl[1])
	}
	if dl[5].ReturnsAt == nil || dl[5].DueAt != nil || dl[5].Overdue || dl[5].Day != "2026-11-06" {
		t.Fatalf("returns: %+v", dl[5])
	}
}

func claims(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	put := func(rows ...Row) { d.put(t, db, rows...) }
	put(Row{Ref: "a", Type: "task", Status: "claimed", Priority: 1}, Row{Ref: "b", Type: "task", Status: "claimed", Priority: -1})
	at := now.Add(-time.Hour)
	leases := []rollup.Claim{
		{Lease: orchapi.Lease{Key: "bd:a", Holder: "mini/aaaaaaaa", Bound: true, Claimed: at}, Mine: true},
		{Lease: orchapi.Lease{Key: "bd:b", Holder: "someone", Claimed: at}},
		{Lease: orchapi.Lease{Key: "lock:migrations", Holder: "mini/aaaaaaaa", Claimed: at}}, // no item
		{Lease: orchapi.Lease{Key: "bd:hidden", Holder: "someone", Claimed: at}},             // out of sight
	}
	r, err := rollup.Build(context.Background(), rollup.SQL{DB: db, Table: tb}, "P", "p", leases, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Claimed) != 2 {
		t.Fatalf("claimed %+v", r.Claimed)
	}
	a, b := r.Claimed[0], r.Claimed[1]
	if a.ID != "bd:a" || !a.Mine || !a.Bound || a.Title != "T a" || a.Priority == nil || *a.Priority != 1 || a.Holder != "mini/aaaaaaaa" {
		t.Fatalf("a %+v", a)
	}
	if b.Mine || b.Holder != "someone" || b.Priority != nil {
		t.Fatalf("b %+v", b)
	}
}

// A node counts the live and blocked claims on the items beneath it, as its
// item counts roll up; each claim names the node that counts its item.
func claimCounts(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	d.put(t, db,
		Row{Ref: "th", Type: "epic", Status: "open", Labels: []string{"theme"}, Priority: -1},
		Row{Ref: "ep", Type: "epic", Status: "open", Parent: "th", Priority: -1},
		Row{Ref: "ep2", Type: "epic", Status: "open", Parent: "th", Priority: -1},
		Row{Ref: "stuck", Type: "task", Status: "claimed", Tracker: "blocked", Parent: "ep", Priority: 2},
		Row{Ref: "going", Type: "task", Status: "claimed", Parent: "ep", Priority: 2},
		Row{Ref: "sub", Type: "task", Status: "claimed", Parent: "going", Priority: 2}, // under a task: ep's still
		Row{Ref: "asks", Type: "task", Status: "claimed", Parent: "ep2", Priority: 2},
		Row{Ref: "gone", Type: "task", Status: "claimed", Parent: "ep2", Priority: 2},
		Row{Ref: "mine", Type: "task", Status: "claimed", Parent: "ep2", Priority: 2},
		Row{Ref: "loose", Type: "task", Status: "claimed", Priority: 2},
	)
	at := now.Add(-time.Hour)
	claim := func(ref string, holder orchapi.SessionState) rollup.Claim {
		return rollup.Claim{Lease: orchapi.Lease{Key: "bd:" + ref, Holder: "mini/aaaaaaaa", Claimed: at}, Holder: holder}
	}
	leases := []rollup.Claim{
		claim("stuck", orchapi.StateRunning), // a blocked item: blocked whoever holds it
		claim("going", orchapi.StateRunning),
		claim("sub", orchapi.StateIdle),
		claim("asks", orchapi.StateNeedsHuman),
		claim("gone", orchapi.StateEnded),
		claim("mine", ""), // a person, or a holder not known here: neither
		claim("loose", orchapi.StateRunning),
	}
	r, err := rollup.Build(context.Background(), rollup.SQL{DB: db, Table: tb}, "P", "p", leases, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]orchapi.RollupClaimCounts{"bd:ep": {Live: 2, Blocked: 1}, "bd:ep2": {Blocked: 2}, "bd:th": {Live: 2, Blocked: 3}}
	for id, c := range want {
		if got := nodeOf(t, r, id).Claims; got != c {
			t.Errorf("%s claims %+v, want %+v", id, got, c)
		}
	}
	epics := map[string]string{}
	states := map[string]string{}
	for _, c := range r.Claimed {
		epics[c.ID], states[c.ID] = c.Epic, c.State
	}
	wantEpic := map[string]string{"bd:stuck": "bd:ep", "bd:going": "bd:ep", "bd:sub": "bd:ep", "bd:asks": "bd:ep2", "bd:gone": "bd:ep2", "bd:mine": "bd:ep2", "bd:loose": ""}
	wantState := map[string]string{"bd:stuck": orchapi.ClaimBlocked, "bd:going": orchapi.ClaimLive, "bd:sub": orchapi.ClaimLive, "bd:asks": orchapi.ClaimBlocked,
		"bd:gone": orchapi.ClaimBlocked, "bd:mine": "", "bd:loose": orchapi.ClaimLive}
	for id := range wantEpic {
		if epics[id] != wantEpic[id] || states[id] != wantState[id] {
			t.Errorf("%s: epic %q state %q, want %q %q", id, epics[id], states[id], wantEpic[id], wantState[id])
		}
	}
}

// Owned is what an open epic owns by Build's rule, within the reader's sight.
func owned(t *testing.T, d Driver) {
	tb, db := d.Table, d.open(t)
	d.put(t, db,
		Row{Ref: "th", Type: "epic", Status: "open", Labels: []string{"theme"}, Priority: -1},
		Row{Ref: "ep", Type: "epic", Status: "open", Parent: "th", Priority: -1},
		Row{Ref: "t1", Type: "task", Status: "open", Parent: "ep", Priority: -1},
		Row{Ref: "s1", Type: "task", Status: "done", Parent: "t1", Priority: -1},
		Row{Ref: "inner", Type: "epic", Status: "open", Parent: "ep", Priority: -1},
		Row{Ref: "u", Type: "task", Status: "open", Parent: "inner", Priority: -1},
		Row{Ref: "shut", Type: "epic", Status: "done", Parent: "ep", Priority: -1},
		Row{Ref: "v", Type: "task", Status: "open", Parent: "shut", Priority: -1},
		Row{Ref: "hid", Type: "task", Status: "open", Parent: "ep", Area: "billing", Priority: -1},
		Row{Ref: "hc", Type: "task", Status: "open", Parent: "hid", Priority: -1},
		Row{Ref: "other", Type: "task", Status: "open", Priority: -1},
	)
	src := rollup.SQL{DB: db, Table: tb, Where: "area != ?", Args: []any{"billing"}}
	get := func(s rollup.SQL, ref string) string {
		t.Helper()
		refs, err := s.Owned(context.Background(), "bd", ref)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(refs)
		return strings.Join(refs, ",")
	}
	if got := get(src, "ep"); got != "inner,s1,shut,t1,v" {
		t.Fatalf("ep owns %q", got)
	}
	if got := get(src, "th"); got != "ep" {
		t.Fatalf("th owns %q", got)
	}
	for _, ref := range []string{"t1", "shut", "hid", "missing"} {
		if got := get(src, ref); got != "" {
			t.Fatalf("%s is no open epic in sight but owns %q", ref, got)
		}
	}
	if got := get(rollup.SQL{DB: db, Table: tb, Where: "area = ?", Args: []any{"billing"}}, "ep"); got != "" {
		t.Fatalf("an epic out of sight owns %q", got)
	}
	if got := get(rollup.SQL{DB: db, Table: tb, Sources: []string{"gh"}}, "ep"); got != "" {
		t.Fatalf("a tracker out of sight owns %q", got)
	}
	if got := get(rollup.SQL{DB: db, Table: tb}, "ep"); got != "hc,hid,inner,s1,shut,t1,v" {
		t.Fatalf("ep owns %q for a whole-project reader", got)
	}
	if !d.Children {
		return
	}
	for _, s := range []rollup.SQL{src, {DB: db, Table: tb, Sources: []string{"bd"}}, {DB: db, Table: tb}} {
		got, err := s.Plans(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got["children"], tb.Name+"_rollup_children") {
			t.Errorf("Owned's read (where %q) is off its index: %s", s.Where, got["children"])
		}
	}
}

// The reads must come from the indexes: a drifted expression would still
// answer right, by reading every item's JSON.
func plans(t *testing.T, d Driver) {
	db := d.open(t)
	d.put(t, db, Row{Ref: "a", Type: "task", Status: "open", Priority: 1})
	idx := d.Table.Name + "_rollup"
	for _, src := range []rollup.SQL{
		{DB: db, Table: d.Table},
		{DB: db, Table: d.Table, Sources: []string{"bd"}},
		{DB: db, Table: d.Table, Sources: []string{"bd", "gh"}},
		{DB: db, Table: d.Table, Where: "area = ?", Args: []any{"x"}},
		{DB: db, Table: d.Table, Where: "area IN (?,?)", Args: []any{"x", "y"}, Sources: []string{"bd"}},
	} {
		got, err := src.Plans(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"live": idx + " ", "done": idx + "_done", "marked": idx + "_marked", "parents": idx + "_parent"}
		for read, name := range want {
			plan := got[read] + " "
			if !strings.Contains(plan, name) || strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("%s read (where %q, sources %v) is off its index %s: %s", read, src.Where, src.Sources, name, got[read])
			}
		}
	}
}

// seed fills the table with n items shaped like a large tracker store: mostly done,
// 280 epics in two levels, about 600 parents, each item with a real body.
func (d Driver) seed(t testing.TB, db *sql.DB, n int) {
	t.Helper()
	body := strings.Repeat("Some description text that a real item carries. ", 40)
	rows := make([]Row, 0, n)
	for i := range n {
		r := Row{Ref: fmt.Sprintf("x-%d", i), Type: "task", Status: "done", Tracker: "closed", Priority: i % 5, Body: body,
			At: now.Add(-time.Duration(i) * time.Minute)}
		switch {
		case i < 30:
			r.Type, r.Status, r.Tracker, r.Labels = "epic", "open", "open", []string{"theme"}
		case i < 280:
			r.Type, r.Status, r.Tracker, r.Parent = "epic", "open", "open", fmt.Sprintf("x-%d", i%30)
		case i < 600: // tasks that are parents of tasks
			r.Parent = fmt.Sprintf("x-%d", 30+i%250)
		default:
			r.Parent = fmt.Sprintf("x-%d", 280+i%320)
		}
		if r.Type == "task" && i%5 == 0 {
			r.Status, r.Tracker = "open", "open"
		}
		if i%1000 == 7 {
			r.Due = now.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		}
		rows = append(rows, r)
	}
	d.put(t, db, rows...)
}

// speed12k is the read on 12k items. The bar itself is measured in CI
// (scripts/bench: work-rollup, replica-rollup); this holds a loose ceiling,
// against a read that stopped using its indexes.
func speed12k(t *testing.T, d Driver) {
	if testing.Short() {
		t.Skip("short")
	}
	db := d.open(t)
	d.seed(t, db, 12000)
	src := rollup.SQL{DB: db, Table: d.Table, Sources: []string{"bd"}}
	var times []time.Duration
	var r orchapi.WorkRollup
	for range 15 {
		t0 := time.Now()
		var err error
		if r, err = rollup.Build(context.Background(), src, "P", "p", nil, now); err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(t0))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("%s: %d nodes, %d items: p50 %v, max %v", d.Table.Name, len(r.Nodes), r.Totals.Total, times[7], times[14])
	if r.Totals.Total != 12000-280 || len(r.Nodes) != 280 {
		t.Fatalf("totals %+v nodes %d", r.Totals, len(r.Nodes))
	}
	if times[7] > 100*time.Millisecond {
		t.Fatalf("p50 %v: is a read off its index?", times[7])
	}
}
