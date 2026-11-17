package finished

import (
	"testing"
	"time"
)

func TestLeases(t *testing.T) {
	var f Leases
	now := time.Now()
	if got := f.Of("p", "h", []string{"k"}, now); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	f.Add("p", "h", "k", now)
	if got := f.Of("p", "h", []string{"k", "other"}, now); len(got) != 1 || got[0] != "k" {
		t.Fatalf("recorded: %v", got)
	}
	for _, q := range [][2]string{{"p2", "h"}, {"p", "h2"}} {
		if got := f.Of(q[0], q[1], []string{"k"}, now); len(got) != 0 {
			t.Fatalf("%v sees another's record: %v", q, got)
		}
	}
	if got := f.Of("p", "h", []string{"k"}, now.Add(Keep+time.Second)); len(got) != 0 {
		t.Fatalf("past Keep: %v", got)
	}
	f.Add("p", "h2", "k2", now.Add(Keep+time.Second)) // prunes the old one
	if len(f.at) != 1 {
		t.Fatalf("not pruned: %v", f.at)
	}
	f.Forget("p", "h2", "k2")
	if got := f.Of("p", "h2", []string{"k2"}, now.Add(Keep+time.Second)); len(got) != 0 {
		t.Fatalf("forgotten: %v", got)
	}
}
