package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

// Every row has a ceiling, and every ceiling a row: a row added without one
// would run unguarded, a stale one would guard nothing.
func TestEveryRowHasACeiling(t *testing.T) {
	f, err := ceilings.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range order {
		if _, ok := f.Bench[name]; !ok {
			t.Errorf("%s has no ceiling in %s", name, ceilings.Path)
		}
	}
	for name := range f.Bench {
		if !slices.Contains(order, name) {
			t.Errorf("%s in %s is not a row of make bench", name, ceilings.Path)
		}
	}
}

func TestSpeedBarRowsExist(t *testing.T) {
	for _, want := range []string{"issue-list", "issue-show", "issue-search", "issue-create", "issue-update", "replica-ls", "replica-show", "replica-comment"} {
		if !slices.Contains(order, want) || !ceilings.OnBar(want) {
			t.Errorf("%s: the 16 ms bar needs this row", want)
		}
	}
}

// A slow spell passes on a later try; a real regression fails after all of
// them, and -budgets=false never retries.
func TestMeasureRetriesOverCeiling(t *testing.T) {
	calls := 0
	row := func(slowFor int) func() (time.Duration, error) {
		calls = 0
		return func() (time.Duration, error) {
			calls++
			if calls <= slowFor {
				return 30 * time.Millisecond, nil
			}
			return 5 * time.Millisecond, nil
		}
	}
	const runs, perTry = 10, 12 // sample adds two warm-up calls
	r, err := measure("spell", runs, 16, true, row(perTry))
	if err != nil || !r.OK || r.Tries != 2 || r.P95 != 5 || r.First != 30 {
		t.Fatalf("a slow spell: %+v %v", r, err)
	}
	r, err = measure("regression", runs, 16, true, row(1000))
	if err != nil || r.OK || r.Tries != 1+retries || calls != perTry*(1+retries) {
		t.Fatalf("a regression: %+v %v after %d calls", r, err, calls)
	}
	r, err = measure("off", runs, 16, false, row(1000))
	if err != nil || r.OK || r.Tries != 1 || calls != perTry {
		t.Fatalf("no ceilings: %+v %v after %d calls", r, err, calls)
	}
}

func TestCompareCallsOutWhatMoved(t *testing.T) {
	base := []result{{Name: "a", First: 4, Budget: 16, OK: true}, {Name: "b", First: 0.2, Budget: 16, OK: true}, {Name: "c", First: 8, Budget: 16, OK: true}}
	now := []result{
		{Name: "a", First: 7, P95: 7, Budget: 16, OK: true},     // 1.75x and 3 ms: called out
		{Name: "b", First: 0.5, P95: 0.5, Budget: 16, OK: true}, // 2.5x but 0.3 ms: noise, and room to tighten
		{Name: "c", First: 20, P95: 20, Budget: 16, OK: false},  // over: said once, as over
		{Name: "d", First: 2, P95: 2, Budget: 3, OK: true},      // new: nothing to compare
	}
	var b bytes.Buffer
	compare(&b, base, now, "", "")
	s := b.String()
	for _, want := range []string{"- a: p95 7.0 ms, main 4.0 ms (1.8x)", "**c**: p95 20.0 ms, ceiling 16 ms", "b (0.5 of 16)", "| d | 0 | 0.0 | 2.0 | - | 3 |"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "- b:") || strings.Contains(s, "- c:") {
		t.Errorf("noise called out:\n%s", s)
	}
}

func TestCompareWithoutBase(t *testing.T) {
	var b bytes.Buffer
	compare(&b, nil, []result{{Name: "a", First: 7, P95: 7, Budget: 16, OK: true}}, "", "")
	if !strings.Contains(b.String(), "No bench from main") {
		t.Fatal(b.String())
	}
}
