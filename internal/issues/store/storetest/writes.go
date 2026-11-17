package storetest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// testAuthorize: Authorize sees the areas before and after, runs before the
// write commits, and a refusal is returned as is and leaves nothing behind.
func testAuthorize(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	deny := errors.New("refused")
	refuse := func(string, string) error { return deny }

	// Create: before == after, the area rules' answer.
	var saw [][2]string
	x := apply(t, p, istore.Mutation{Op: istore.OpCreate, AreaOf: labelArea, Create: &orchapi.NewIssueRequest{Title: "x", Labels: []string{"area:a"}},
		Authorize: func(b, a string) error { saw = append(saw, [2]string{b, a}); return nil }}).Issue
	if !slices.Equal(saw, [][2]string{{"a", "a"}}) || x.Area != "a" {
		t.Fatalf("create authorized %v, area %q", saw, x.Area)
	}

	// Inside the write: what the callback sees is not committed yet. (The
	// contract: a read on the same store may run while a write is open.)
	w := create(t, p, orchapi.NewIssueRequest{Title: "w"})
	start := last(t, p)
	var inside string
	apply(t, p, istore.Mutation{Op: istore.OpLabelAdd, ID: w.ID, Label: "area:b", AreaOf: labelArea,
		Authorize: func(b, a string) error {
			saw = append(saw, [2]string{b, a})
			inside = last(t, p)
			return nil
		}})
	if inside != start {
		t.Fatalf("Authorize ran after the write committed (last change %s, was %s)", inside, start)
	}
	// A driver may check the current area first; the last call has both.
	if got := saw[len(saw)-1]; len(saw) < 2 || got != [2]string{"", "b"} {
		t.Fatalf("label move authorized %v, last want [ b]", saw[1:])
	}

	// Refusals roll back every op.
	countIssues := func() int {
		l, err := p.List(bg, orchapi.IssueQuery{Status: "all", Count: true}, all, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return l.Total
	}
	y := create(t, p, orchapi.NewIssueRequest{Title: "y"})
	before, err := p.Get(bg, x.ID, all)
	if err != nil {
		t.Fatal(err)
	}
	n0, l0 := countIssues(), last(t, p)
	title, prio := "changed", 0
	for _, m := range []istore.Mutation{
		{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "never"}},
		{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "never child", Parent: x.ID}},
		{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{Title: &title, Priority: &prio}},
		{Op: istore.OpClaim},
		{Op: istore.OpClose, Close: &orchapi.CloseIssueRequest{Reason: "no"}},
		{Op: istore.OpComment, Comment: &orchapi.IssueCommentRequest{Text: "never"}},
		{Op: istore.OpDepAdd, Dep: &orchapi.IssueDepRequest{Target: y.ID}},
		{Op: istore.OpLabelAdd, Label: "area:c"},
		{Op: istore.OpLabelRemove, Label: "area:a"},
	} {
		if m.Op != istore.OpCreate {
			m.ID = x.ID
		}
		m.Actor, m.AreaOf, m.Authorize = ana, labelArea, refuse
		if _, err := p.Apply(bg, m); !errors.Is(err, deny) {
			t.Fatalf("refused %s: %v", m.Op, err)
		}
		after, err := p.Get(bg, x.ID, all)
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != before.Version || after.Title != before.Title || after.Status != before.Status || after.Area != before.Area ||
			after.CommentCount != before.CommentCount || after.DependencyCount != before.DependencyCount || !slices.Equal(after.Labels, before.Labels) {
			t.Fatalf("refused %s changed the issue: %+v, was %+v", m.Op, after, before)
		}
		if n, l := countIssues(), last(t, p); n != n0 || l != l0 {
			t.Fatalf("refused %s left %d issues (was %d), last change %s (was %s)", m.Op, n, n0, l, l0)
		}
	}
	if cs, err := p.Comments(bg, x.ID, all); err != nil || len(cs) != 0 {
		t.Fatalf("comments after refusals %v %v", cs, err)
	}

	// The area Authorize sees is read inside the write: a second writer
	// waiting on the first sees the first's result as its before.
	z := create(t, p, orchapi.NewIssueRequest{Title: "z"})
	var once sync.Once
	var wg sync.WaitGroup
	var second [][2]string
	var secondErr error
	wg.Add(1)
	_, err = p.Apply(bg, istore.Mutation{Op: istore.OpLabelAdd, ID: z.ID, Actor: ana, Label: "area:q", AreaOf: labelArea,
		Authorize: func(string, string) error {
			once.Do(func() {
				go func() {
					defer wg.Done()
					_, secondErr = p.Apply(bg, istore.Mutation{Op: istore.OpLabelAdd, ID: z.ID, Actor: bob, Label: "area:r", AreaOf: labelArea,
						Authorize: func(b, a string) error { second = append(second, [2]string{b, a}); return nil }})
				}()
				time.Sleep(30 * time.Millisecond)
			})
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if secondErr != nil || len(second) == 0 || slices.ContainsFunc(second, func(c [2]string) bool { return c[0] != "q" }) {
		t.Fatalf("second writer authorized %v (%v), want before = q (the first writer's result)", second, secondErr)
	}
}

// testConcurrent: writers in parallel keep one history in commit order, and
// a claim race has exactly one winner.
func testConcurrent(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	const writers, writes = 8, 12
	var wg sync.WaitGroup
	got := make([][]orchapi.IssueChange, writers)
	errs := make([]error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actor := istore.Actor{Name: fmt.Sprintf("w%d", w), Address: fmt.Sprintf("mini/w%d", w)}
			r, err := p.Apply(bg, istore.Mutation{Op: istore.OpCreate, Actor: actor, Create: &orchapi.NewIssueRequest{Title: actor.Name}})
			if err != nil {
				errs[w] = err
				return
			}
			id := r.Issue.ID
			got[w] = append(got[w], r.Changes...)
			for i := range writes {
				m := istore.Mutation{Op: istore.OpComment, ID: id, Actor: actor, Comment: &orchapi.IssueCommentRequest{Text: fmt.Sprint(i)}}
				if i%2 == 1 {
					notes := fmt.Sprint("n", i)
					m = istore.Mutation{Op: istore.OpUpdate, ID: id, Actor: actor, Update: &orchapi.UpdateIssueRequest{Notes: &notes}}
				}
				if r, err = p.Apply(bg, m); err != nil {
					errs[w] = err
					return
				}
				got[w] = append(got[w], r.Changes...)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	whole, err := p.Changes(bg, "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, "changelog", "", whole)
	if want := writers * (writes + 1); len(whole) != want {
		t.Fatalf("%d history rows, want %d", len(whole), want)
	}
	pos := map[string]int{}
	for i, c := range whole {
		pos[c.ID] = i
	}
	for w, chs := range got {
		prev := -1
		for _, c := range chs {
			i, ok := pos[c.ID]
			if !ok || i <= prev {
				t.Fatalf("writer %d: change %s at %d (found %v) after %d", w, c.ID, i, ok, prev)
			}
			prev = i
		}
	}
	if l := last(t, p); l != whole[len(whole)-1].ID {
		t.Fatalf("last change %s, newest row %s", l, whole[len(whole)-1].ID)
	}

	// Claim race.
	it := create(t, p, orchapi.NewIssueRequest{Title: "contested"})
	wins := make([]bool, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actor := istore.Actor{Name: fmt.Sprintf("c%d", w), Address: fmt.Sprintf("mini/c%d", w)}
			r, err := p.Apply(bg, istore.Mutation{Op: istore.OpClaim, ID: it.ID, Actor: actor})
			switch {
			case err == nil:
				wins[w] = !r.Noop
			case !api.IsCode(err, api.CodeConflict):
				errs[w] = err
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d writers won the claim", n)
	}
}
