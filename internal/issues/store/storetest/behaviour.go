package storetest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func testCreateListSearchClaimClose(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	a := create(t, p, orchapi.NewIssueRequest{Title: "Fix the solver timeout", Description: "The **solver** hangs", Labels: []string{"solver", "solver", " v0 "}})
	if !strings.HasPrefix(a.ID, "oc-") || a.Status != "open" || a.Priority != 2 || a.IssueType != "task" || a.CreatedBy != "Ana" {
		t.Fatalf("created %+v", a)
	}
	if strings.Join(a.Labels, ",") != "solver,v0" {
		t.Fatalf("labels %v", a.Labels)
	}
	prio := 1
	b := create(t, p, orchapi.NewIssueRequest{Title: "Epic", IssueType: "epic", Priority: &prio})
	c := create(t, p, orchapi.NewIssueRequest{Title: "Child", Parent: b.ID})
	if c.ID != b.ID+".1" || c.Parent != b.ID {
		t.Fatalf("child %+v", c)
	}
	c2 := create(t, p, orchapi.NewIssueRequest{Title: "Child two", Parent: b.ID, Deps: []string{a.ID}})
	if c2.ID != b.ID+".2" || !c2.Blocked {
		t.Fatalf("child2 %+v", c2)
	}
	// Ready: a, b, c (c2 is blocked by a).
	l, err := p.List(bg, orchapi.IssueQuery{Ready: true}, all, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := idList(l.Issues); got != b.ID+" "+a.ID+" "+c.ID && got != b.ID+" "+c.ID+" "+a.ID {
		t.Fatalf("ready %s", got)
	}
	// Full text and substring search.
	l, err = p.List(bg, orchapi.IssueQuery{Q: "solv"}, all, time.Now())
	if err != nil || idList(l.Issues) != a.ID {
		t.Fatalf("fts %v %v", idList(l.Issues), err)
	}
	l, err = p.List(bg, orchapi.IssueQuery{Substr: "olver time"}, all, time.Now())
	if err != nil || idList(l.Issues) != a.ID {
		t.Fatalf("substr %v %v", idList(l.Issues), err)
	}
	// Claims are atomic and exclusive.
	r := apply(t, p, istore.Mutation{Op: istore.OpClaim, ID: a.ID})
	if r.Issue.Status != "in_progress" || r.Issue.Assignee != "Ana" || r.Issue.ClaimedBy != "human:ana" || r.Issue.StartedAt == nil {
		t.Fatalf("claim %+v", r.Issue)
	}
	if r := apply(t, p, istore.Mutation{Op: istore.OpClaim, ID: a.ID}); !r.Noop || len(r.Changes) != 0 {
		t.Fatal("reclaim should be a no-op")
	}
	// The same address under another name (a team write-back run as the
	// system) is the same claim: a no-op, the assignee kept.
	if r := apply(t, p, istore.Mutation{Op: istore.OpClaim, ID: a.ID, Actor: istore.Actor{Name: "mini", Address: "human:ana"}}); !r.Noop || r.Issue.Assignee != "Ana" {
		t.Fatalf("reclaim by the same address as another name: noop=%v assignee %q", r.Noop, r.Issue.Assignee)
	}
	if _, err := p.Apply(bg, istore.Mutation{Op: istore.OpClaim, ID: a.ID, Actor: bob}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "already claimed by Ana") {
		t.Fatalf("bob claim: %v", err)
	}
	// Closing a unblocks c2.
	r = apply(t, p, istore.Mutation{Op: istore.OpClose, ID: a.ID, Close: &orchapi.CloseIssueRequest{Reason: "done"}})
	if r.Issue.Status != "closed" || r.Issue.ClosedAt == nil || r.Issue.CloseReason != "done" || len(r.Touched) != 1 || r.Touched[0].ID != c2.ID || r.Touched[0].Blocked {
		t.Fatalf("close %+v touched %+v", r.Issue, r.Touched)
	}
	// Epic with open children refuses to close.
	if _, err := p.Apply(bg, istore.Mutation{Op: istore.OpClose, ID: b.ID, Actor: ana}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("epic close: %v", err)
	}
	// Reopen with a reason adds a comment.
	r = apply(t, p, istore.Mutation{Op: istore.OpReopen, ID: a.ID, Reopen: &orchapi.ReopenIssueRequest{Reason: "regressed"}})
	if r.Issue.Status != "open" || r.Issue.ClosedAt != nil || r.Issue.CloseReason != "" || r.Comment == nil || r.Comment.Text != "regressed" {
		t.Fatalf("reopen %+v", r.Issue)
	}
	d, err := p.Detail(bg, c2.ID, all)
	if err != nil || len(d.DependsOn) != 2 || !d.Blocked {
		t.Fatalf("detail %+v %v", d, err)
	}
	h, err := p.History(bg, a.ID, all)
	if err != nil || len(h) != 5 {
		t.Fatalf("history %d %v", len(h), err)
	}
	kinds := []string{}
	for _, c := range h {
		kinds = append(kinds, c.Kind)
	}
	if strings.Join(kinds, ",") != "created,claimed,closed,reopened,commented" {
		t.Fatalf("kinds %v", kinds)
	}
	st, err := p.Stats(bg, all, time.Now())
	if err != nil || st.TotalIssues != 4 || st.OpenIssues != 4 || st.BlockedIssues != 1 {
		t.Fatalf("stats %+v %v", st, err)
	}
}

func testUpdateBaseConflictAndAreas(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	areaOf := func(id string, labels []string) string {
		if slices.Contains(labels, "solver") {
			return "solver"
		}
		return ""
	}
	a := apply(t, p, istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "x"}, AreaOf: areaOf}).Issue
	base := a.Version
	title := "y"
	r := apply(t, p, istore.Mutation{Op: istore.OpUpdate, ID: a.ID, Update: &orchapi.UpdateIssueRequest{Title: &title, Base: base}, AreaOf: areaOf})
	if r.Issue.Title != "y" || r.Issue.Version == base {
		t.Fatalf("update %+v", r.Issue)
	}
	// A stale base on the same field is refused; on another field it is not.
	t2 := "z"
	if _, err := p.Apply(bg, istore.Mutation{Op: istore.OpUpdate, ID: a.ID, Actor: ana, Update: &orchapi.UpdateIssueRequest{Title: &t2, Base: base}}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("stale base: %v", err)
	}
	notes := "n"
	apply(t, p, istore.Mutation{Op: istore.OpUpdate, ID: a.ID, Update: &orchapi.UpdateIssueRequest{Notes: &notes, Base: base}, AreaOf: areaOf})
	// A label moves it into an area; Authorize sees before and after.
	var saw [2]string
	r = apply(t, p, istore.Mutation{Op: istore.OpLabelAdd, ID: a.ID, Label: "solver", AreaOf: areaOf,
		Authorize: func(b, af string) error { saw = [2]string{b, af}; return nil }})
	if r.Issue.Area != "solver" || saw != [2]string{"", "solver"} || r.Changes[0].AreaBefore != "" || r.Changes[0].AreaAfter != "solver" {
		t.Fatalf("label move %+v %v", r.Issue, saw)
	}
	// Hidden from a reader of another area, like a missing id.
	if _, err := p.Get(bg, a.ID, istore.Areas{Names: []string{"other"}}); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("hidden get: %v", err)
	}
	// Comments with a client id are idempotent.
	cid := "01JA0M2C9Q3T4V5W6X7Y8Z9A0B"
	r1 := apply(t, p, istore.Mutation{Op: istore.OpComment, ID: a.ID, Comment: &orchapi.IssueCommentRequest{ID: cid, Text: "hi"}})
	r2 := apply(t, p, istore.Mutation{Op: istore.OpComment, ID: a.ID, Comment: &orchapi.IssueCommentRequest{ID: cid, Text: "hi"}})
	if r1.Noop || !r2.Noop || r2.Comment.ID != cid || len(r2.Changes) != 0 {
		t.Fatalf("idempotent comment %v %v", r1.Noop, r2.Noop)
	}
	if cs, _ := p.Comments(bg, a.ID, all); len(cs) != 1 {
		t.Fatalf("comments %d", len(cs))
	}
}

func testDepCycle(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	a := create(t, p, orchapi.NewIssueRequest{Title: "a"})
	b := create(t, p, orchapi.NewIssueRequest{Title: "b"})
	apply(t, p, istore.Mutation{Op: istore.OpDepAdd, ID: a.ID, Dep: &orchapi.IssueDepRequest{Target: b.ID}})
	if _, err := p.Apply(bg, istore.Mutation{Op: istore.OpDepAdd, ID: b.ID, Actor: ana, Dep: &orchapi.IssueDepRequest{Target: a.ID}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := p.Apply(bg, istore.Mutation{Op: istore.OpDepAdd, ID: a.ID, Actor: ana, Dep: &orchapi.IssueDepRequest{Target: "oc-nope"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing target: %v", err)
	}
}

// testLegacyLabels: labels stored before normalization (hidden characters)
// can still be removed, by their raw or their normalized form.
func testLegacyLabels(t *testing.T, newStore NewStore) {
	const target, other = "\u200b", "\u2066"
	for _, tc := range []struct {
		name string
		m    istore.Mutation
		want []string
		bad  bool
	}{
		{"label door", istore.Mutation{Op: istore.OpLabelRemove, Label: target}, []string{other, "keep"}, false},
		{"update door", istore.Mutation{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{RemoveLabels: []string{target}}}, []string{other, "keep"}, false},
		{"trimmed selector", istore.Mutation{Op: istore.OpLabelRemove, Label: " " + target + " "}, []string{other, "keep"}, false},
		{"empty update selector", istore.Mutation{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{RemoveLabels: []string{""}}}, []string{target, other, "keep"}, false},
		{"whitespace update selector", istore.Mutation{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{RemoveLabels: []string{" "}}}, []string{target, other, "keep"}, false},
		{"empty label door", istore.Mutation{Op: istore.OpLabelRemove}, []string{target, other, "keep"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := project(t, newStore)
			is := create(t, p, orchapi.NewIssueRequest{Title: "legacy labels", Labels: []string{target, other, "keep"}})
			tc.m.ID, tc.m.Actor = is.ID, ana
			res, err := p.Apply(bg, tc.m)
			if tc.bad {
				if !api.IsCode(err, api.CodeBadRequest) {
					t.Fatalf("empty removal: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if tc.name == "empty update selector" && !res.Noop {
				t.Fatal("empty update selector was not a no-op")
			}
			got, err := p.Get(bg, is.ID, all)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(tc.want)
			if !slices.Equal(got.Labels, tc.want) {
				t.Fatalf("labels %q, want %q", got.Labels, tc.want)
			}
		})
	}
	p := project(t, newStore)
	for _, sel := range []string{"ab", "a\u200bb"} {
		it := create(t, p, orchapi.NewIssueRequest{Title: "x", Labels: []string{"a\u200bb"}})
		if got := apply(t, p, istore.Mutation{Op: istore.OpLabelRemove, ID: it.ID, Label: sel}); len(got.Issue.Labels) != 0 {
			t.Fatalf("remove %q: labels %+v", sel, got.Issue.Labels)
		}
	}
	is := create(t, p, orchapi.NewIssueRequest{Title: "normalized aliases", Labels: []string{"tag", "ta\u200bg", "keep"}})
	if r := apply(t, p, istore.Mutation{Op: istore.OpLabelRemove, ID: is.ID, Label: "tag"}); !slices.Equal(r.Issue.Labels, []string{"keep"}) {
		t.Fatalf("normal removal stopped matching normalized aliases: %q", r.Issue.Labels)
	}
}

// testCommentReplayBinding: a replayed client comment id must match the
// stored body and author, else it conflicts.
func testCommentReplayBinding(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	issue := create(t, p, orchapi.NewIssueRequest{Title: "delivery identity"})
	id := "01JA0M2C9Q3T4V5W6X7Y8Z9A0B"
	apply(t, p, istore.Mutation{Op: istore.OpComment, ID: issue.ID, Actor: ana, Comment: &orchapi.IssueCommentRequest{ID: id, Text: "approved answer"}})
	for name, m := range map[string]istore.Mutation{
		"body":  {Op: istore.OpComment, ID: issue.ID, Actor: ana, Comment: &orchapi.IssueCommentRequest{ID: id, Text: "fabricated answer"}},
		"actor": {Op: istore.OpComment, ID: issue.ID, Actor: bob, Comment: &orchapi.IssueCommentRequest{ID: id, Text: "approved answer"}},
	} {
		if _, err := p.Apply(bg, m); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	comments, err := p.Comments(bg, issue.ID, all)
	if err != nil || len(comments) != 1 || comments[0].Text != "approved answer" {
		t.Fatalf("comments: %+v %v", comments, err)
	}
}
