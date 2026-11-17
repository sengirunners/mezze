// Package storetest is the native issue store's driver conformance suite:
// every driver of istore.Store runs Run from its own test, before it lands.
// It speaks only the interface (store.go's contract and SPEC "Native
// issues"), never a driver's tables; driver-specific behaviour (files, WAL,
// speed, migrations) stays in the driver's own tests.
package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/ids"
)

// NewStore returns an empty Store in a temp dir (or database) of t's; Run
// closes it.
type NewStore func(t *testing.T) istore.Store

// Run runs the suite against stores from newStore.
func Run(t *testing.T, newStore NewStore) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, NewStore)
	}{
		{"Projects", testProjects},
		{"ApplyEveryOp", testApplyEveryOp},
		{"Authorize", testAuthorize},
		{"ListScopeAndPaging", testList},
		{"UnicodeSubstring", testUnicodeSubstring},
		{"ReadyParentScope", testReadyParent},
		{"ConcurrentWriters", testConcurrent},
		{"CreateListSearchClaimClose", testCreateListSearchClaimClose},
		{"UpdateBaseConflictAndAreas", testUpdateBaseConflictAndAreas},
		{"DepCycle", testDepCycle},
		{"LegacyLabelRemoval", testLegacyLabels},
		{"CommentReplayBinding", testCommentReplayBinding},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

var (
	ana = istore.Actor{Name: "Ana", Address: "human:ana"}
	bob = istore.Actor{Name: "Bob", Address: "mini/0000bbbb"}
	all = istore.Areas{All: true}
	bg  = context.Background()
)

// open is a fresh store, closed when t ends.
func open(t *testing.T, newStore NewStore) istore.Store {
	t.Helper()
	s := newStore(t)
	t.Cleanup(func() { s.Close() })
	return s
}

// projectIn is project id of s, made with prefix.
func projectIn(t *testing.T, s istore.Store, id, prefix string) istore.ProjectStore {
	t.Helper()
	p, err := s.Project(bg, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPrefix(bg, prefix); err != nil {
		t.Fatal(err)
	}
	return p
}

// project is one project (prefix "oc") of a fresh store.
func project(t *testing.T, newStore NewStore) istore.ProjectStore {
	t.Helper()
	return projectIn(t, open(t, newStore), "01PROJ", "oc")
}

func apply(t *testing.T, p istore.ProjectStore, m istore.Mutation) istore.Result {
	t.Helper()
	if m.Actor.Name == "" {
		m.Actor = ana
	}
	r, err := p.Apply(bg, m)
	if err != nil {
		t.Fatalf("%s %s: %v", m.Op, m.ID, err)
	}
	return r
}

func create(t *testing.T, p istore.ProjectStore, req orchapi.NewIssueRequest) orchapi.Issue {
	t.Helper()
	return apply(t, p, istore.Mutation{Op: istore.OpCreate, Create: &req}).Issue
}

func idList(its []orchapi.Issue) string {
	var out []string
	for _, it := range its {
		out = append(out, it.ID)
	}
	return strings.Join(out, " ")
}

func changeIDs(chs []orchapi.IssueChange) []string {
	var out []string
	for _, c := range chs {
		out = append(out, c.ID)
	}
	return out
}

func last(t *testing.T, p istore.ProjectStore) string {
	t.Helper()
	l, err := p.LastChange(bg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// labelArea is area rules: a label "area:<x>" puts an issue in x.
func labelArea(_ string, labels []string) string {
	for _, l := range labels {
		if a, ok := strings.CutPrefix(l, "area:"); ok {
			return a
		}
	}
	return ""
}

// ordered fails unless chs are ULIDs, strictly increasing, after since.
func ordered(t *testing.T, what, since string, chs []orchapi.IssueChange) {
	t.Helper()
	prev := since
	for _, c := range chs {
		if !ids.Valid(c.ID) {
			t.Fatalf("%s: history id %q is not a ULID", what, c.ID)
		}
		if c.ID <= prev {
			t.Fatalf("%s: history id %s not after %s", what, c.ID, prev)
		}
		prev = c.ID
	}
}

func testProjects(t *testing.T, newStore NewStore) {
	s := open(t, newStore)
	if ex, err := s.Existing(bg); err != nil || len(ex) != 0 {
		t.Fatalf("existing on an empty store: %v %v", ex, err)
	}
	if _, err := s.Project(bg, "01A", false); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("open without create: %v", err)
	}
	a := projectIn(t, s, "01A", "aa")
	again, err := s.Project(bg, "01A", false)
	if err != nil {
		t.Fatal(err)
	}
	if pre, err := again.Prefix(bg); err != nil || pre != "aa" {
		t.Fatalf("prefix %q %v", pre, err)
	}
	it := create(t, a, orchapi.NewIssueRequest{Title: "only in A"})
	if !strings.HasPrefix(it.ID, "aa-") {
		t.Fatalf("id %q does not use the prefix", it.ID)
	}
	b := projectIn(t, s, "01B", "bb")
	if l, err := b.List(bg, orchapi.IssueQuery{Status: "all"}, all, time.Now()); err != nil || len(l.Issues) != 0 {
		t.Fatalf("project B lists %v %v", idList(l.Issues), err)
	}
	if _, err := b.Get(bg, it.ID, all); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("A's issue from B: %v", err)
	}
	ex, err := s.Existing(bg)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(ex, func(x, y istore.ProjectMeta) int { return strings.Compare(x.Project, y.Project) })
	if want := []istore.ProjectMeta{{Project: "01A", Prefix: "aa"}, {Project: "01B", Prefix: "bb"}}; !slices.Equal(ex, want) {
		t.Fatalf("existing %v, want %v", ex, want)
	}

	// Meta and KV.
	if v, err := a.Meta(bg, "import"); err != nil || v != "" {
		t.Fatalf("missing meta %q %v", v, err)
	}
	if err := a.SetMeta(bg, "import", "done"); err != nil {
		t.Fatal(err)
	}
	if v, err := a.Meta(bg, "import"); err != nil || v != "done" {
		t.Fatalf("meta %q %v", v, err)
	}
	for i, want := range []string{"", "1"} {
		if old, err := a.SetKV(bg, "kv.memory.x", []string{"1", "2"}[i]); err != nil || old != want {
			t.Fatalf("set kv: old %q %v", old, err)
		}
	}
	if _, err := a.SetKV(bg, "other", "o"); err != nil {
		t.Fatal(err)
	}
	if kv, err := a.KV(bg, "kv.memory."); err != nil || len(kv) != 1 || kv["kv.memory.x"] != "2" {
		t.Fatalf("kv %v %v", kv, err)
	}
	if old, err := a.DeleteKV(bg, "kv.memory.x"); err != nil || old != "2" {
		t.Fatalf("delete kv: old %q %v", old, err)
	}
	if old, err := a.DeleteKV(bg, "kv.memory.x"); err != nil || old != "" {
		t.Fatalf("delete missing kv: old %q %v", old, err)
	}

	// Reset empties the project but keeps its prefix.
	if err := a.Reset(bg); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Counts(bg); err != nil || n["issues"] != 0 || n["history"] != 0 || n["kv"] != 0 {
		t.Fatalf("counts after reset %v %v", n, err)
	}
	if l := last(t, a); l != "" {
		t.Fatalf("last change after reset %q", l)
	}
	if pre, _ := a.Prefix(bg); pre != "aa" {
		t.Fatalf("prefix after reset %q", pre)
	}
}

// testApplyEveryOp runs each op through Apply: its history rows come back
// in the Result, are ULIDs in commit order, and are exactly what History,
// Changes and LastChange then show.
func testApplyEveryOp(t *testing.T, newStore NewStore) {
	s := open(t, newStore)
	p := projectIn(t, s, "01PROJ", "oc")
	var got []orchapi.IssueChange
	step := func(kind string, m istore.Mutation) istore.Result {
		t.Helper()
		before := last(t, p)
		m.AreaOf = labelArea
		r := apply(t, p, m)
		if r.Noop || len(r.Changes) == 0 {
			t.Fatalf("%s: no history (noop %v)", m.Op, r.Noop)
		}
		ordered(t, string(m.Op), before, r.Changes)
		if r.Changes[len(r.Changes)-1].Kind != kind {
			t.Fatalf("%s: last change kind %q, want %q", m.Op, r.Changes[len(r.Changes)-1].Kind, kind)
		}
		chs, err := p.Changes(bg, before, 1000)
		if err != nil || !slices.Equal(changeIDs(chs), changeIDs(r.Changes)) {
			t.Fatalf("%s: changes since %s are %v, result has %v (%v)", m.Op, before, changeIDs(chs), changeIDs(r.Changes), err)
		}
		if l := last(t, p); l != r.Changes[len(r.Changes)-1].ID {
			t.Fatalf("%s: last change %s", m.Op, l)
		}
		got = append(got, r.Changes...)
		return r
	}
	a := step("created", istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "a"}}).Issue
	b := step("created", istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "b"}}).Issue
	title := "a2"
	step("updated", istore.Mutation{Op: istore.OpUpdate, ID: a.ID, Update: &orchapi.UpdateIssueRequest{Title: &title}})
	step("claimed", istore.Mutation{Op: istore.OpClaim, ID: a.ID})
	c := step("commented", istore.Mutation{Op: istore.OpComment, ID: a.ID, Comment: &orchapi.IssueCommentRequest{Text: "first"}}).Comment
	if c == nil || c.Text != "first" {
		t.Fatalf("comment %+v", c)
	}
	step("comment_edited", istore.Mutation{Op: istore.OpCommentEdit, ID: a.ID, CommentEdit: &istore.CommentEdit{ID: c.ID, Text: "edited"}})
	step("comment_deleted", istore.Mutation{Op: istore.OpCommentDelete, ID: a.ID, CommentID: c.ID})
	c2 := step("commented", istore.Mutation{Op: istore.OpComment, ID: a.ID, Comment: &orchapi.IssueCommentRequest{Text: "secret text"}}).Comment
	redactAt := time.Now().Truncate(time.Millisecond)
	redact := istore.Mutation{Op: istore.OpCommentRedact, ID: a.ID, CommentID: c2.ID, At: redactAt, Moderate: func(string) bool { return true }}
	r := step("comment_redacted", redact)
	if !slices.Contains(r.Redacted, "secret text") {
		t.Fatalf("redacted %q", r.Redacted)
	}
	// The row says when the text left the store (the service's redaction
	// window ends there); a repeat with nothing left to overwrite adds none.
	if ch := r.Changes[len(r.Changes)-1]; ch.Comment != c2.ID || !ch.At.Equal(redactAt) {
		t.Fatalf("redaction row names comment %q at %v, want %q at %v", ch.Comment, ch.At, c2.ID, redactAt)
	}
	redact.At = redactAt.Add(time.Second)
	if again := apply(t, p, redact); len(again.Changes) != 0 || len(again.Redacted) != 0 {
		t.Fatalf("a repeat redaction wrote %+v, redacted %q", again.Changes, again.Redacted)
	}
	step("dep_added", istore.Mutation{Op: istore.OpDepAdd, ID: a.ID, Dep: &orchapi.IssueDepRequest{Target: b.ID}})
	step("dep_removed", istore.Mutation{Op: istore.OpDepRemove, ID: a.ID, Dep: &orchapi.IssueDepRequest{Target: b.ID}})
	r = step("label_added", istore.Mutation{Op: istore.OpLabelAdd, ID: a.ID, Label: "area:x"})
	if ch := r.Changes[0]; ch.AreaBefore != "" || ch.AreaAfter != "x" || r.Issue.Area != "x" {
		t.Fatalf("label move: change %s -> %s, issue in %q", ch.AreaBefore, ch.AreaAfter, r.Issue.Area)
	}
	step("label_removed", istore.Mutation{Op: istore.OpLabelRemove, ID: a.ID, Label: "area:x"})
	step("closed", istore.Mutation{Op: istore.OpClose, ID: a.ID, Close: &orchapi.CloseIssueRequest{Reason: "done"}})
	step("reopened", istore.Mutation{Op: istore.OpReopen, ID: a.ID, Reopen: &orchapi.ReopenIssueRequest{}})

	h, err := p.History(bg, a.ID, all)
	if err != nil {
		t.Fatal(err)
	}
	var wantA []string
	for _, c := range got {
		if c.IssueID == a.ID {
			wantA = append(wantA, c.ID)
		}
	}
	if !slices.Equal(changeIDs(h), wantA) {
		t.Fatalf("history of %s %v, want %v", a.ID, changeIDs(h), wantA)
	}
	whole, err := p.Changes(bg, "", 1000)
	if err != nil || !slices.Equal(changeIDs(whole), changeIDs(got)) {
		t.Fatalf("changelog %v, want every result's rows %v (%v)", changeIDs(whole), changeIDs(got), err)
	}
	// Paging the feed gives the same rows.
	var paged []orchapi.IssueChange
	for since := ""; ; {
		page, err := p.Changes(bg, since, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 3 {
			t.Fatalf("page of %d over limit 3", len(page))
		}
		paged = append(paged, page...)
		since = page[len(page)-1].ID
	}
	if !slices.Equal(changeIDs(paged), changeIDs(got)) {
		t.Fatalf("paged changelog %v", changeIDs(paged))
	}
	snapLast, err := p.Snapshot(bg, func(orchapi.Issue) error { return nil })
	if err != nil || snapLast != got[len(got)-1].ID {
		t.Fatalf("snapshot last %q %v", snapLast, err)
	}

	// Moves, when the driver has the doors.
	if _, ok := p.(istore.Mover); !ok {
		return
	}
	dst := projectIn(t, s, "01DST", "sg")
	mv, err := p.(istore.Mover).Export(bg, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	newID := "sg-" + strings.TrimPrefix(b.ID, "oc-")
	before := last(t, dst)
	r = apply(t, dst, istore.Mutation{Op: istore.OpMoveIn, ID: b.ID, Move: &istore.Move{Project: "01PROJ", NewID: newID, From: &mv, HomeArea: mv.HomeArea}})
	ordered(t, "move in", before, r.Changes)
	if len(r.Changes) == 0 || r.Changes[len(r.Changes)-1].Kind != "moved" || last(t, dst) != r.Changes[len(r.Changes)-1].ID {
		t.Fatalf("move in changes %+v", r.Changes)
	}
	before = last(t, p)
	r = apply(t, p, istore.Mutation{Op: istore.OpMoveOut, ID: b.ID, Move: &istore.Move{Project: "01DST", NewID: newID}})
	ordered(t, "move out", before, r.Changes)
	if len(r.Changes) != 1 || r.Changes[0].Kind != "moved" || last(t, p) != r.Changes[0].ID {
		t.Fatalf("move out changes %+v", r.Changes)
	}
	if _, err := p.Get(bg, b.ID, all); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("moved-out issue: %v", err)
	}
	before = last(t, dst)
	apply(t, dst, istore.Mutation{Op: istore.OpMoveDone, ID: b.ID, Move: &istore.Move{Project: "01PROJ", NewID: newID}})
	if l := last(t, dst); l != before {
		t.Fatalf("move done wrote history: %s", l)
	}
	if moved, err := dst.Get(bg, newID, all); err != nil || moved.Title != "b" {
		t.Fatalf("moved issue %+v %v", moved, err)
	}
	// A move keeps the redaction row, its comment and its time.
	mv, err = p.(istore.Mover).Export(bg, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	newA := "sg-" + strings.TrimPrefix(a.ID, "oc-")
	apply(t, dst, istore.Mutation{Op: istore.OpMoveIn, ID: a.ID, Move: &istore.Move{Project: "01PROJ", NewID: newA, From: &mv, HomeArea: mv.HomeArea}})
	h, err = dst.History(bg, newA, all)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(h, func(ch orchapi.IssueChange) bool {
		return ch.Kind == "comment_redacted" && ch.Comment == c2.ID && ch.At.Equal(redactAt)
	}) {
		t.Fatalf("moved history lost the redaction row: %+v", h)
	}
}
