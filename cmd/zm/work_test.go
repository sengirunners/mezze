package main

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// A skipped tracker must reach the user as a warning naming project/source.
func TestWarnSkipped(t *testing.T) {
	var b strings.Builder
	warnSkipped(&b, map[string]string{"01P": "harbor"}, []api.WorkSkipped{
		{Project: "01P", Source: "bd", Error: "unavailable: the beads tracker is unavailable"},
		{Project: "01Q", Source: "gh", Error: "x"},
	})
	got := b.String()
	if !strings.Contains(got, "warning: harbor/bd unavailable") || !strings.Contains(got, "the beads tracker is unavailable") ||
		!strings.Contains(got, "warning: 01Q/gh unavailable") {
		t.Fatalf("warning %q", got)
	}
	b.Reset()
	warnSkipped(&b, nil, nil)
	if b.Len() != 0 {
		t.Fatalf("warned with nothing skipped: %q", b.String())
	}
}

func TestSplitQualified(t *testing.T) {
	for in, want := range map[string][2]string{
		"acme/local:k3x9qa": {"acme", "local:k3x9qa"},
		"local:k3x9qa":      {"", "local:k3x9qa"},
		"mini/4bb7545a":     {"", "mini/4bb7545a"},
		"machine:mini":      {"", "machine:mini"},
	} {
		p, id, _ := splitQualified(in)
		if p != want[0] || id != want[1] {
			t.Errorf("splitQualified(%q) = %q, %q", in, p, id)
		}
	}
}

// zm project why with nothing resolved exits 1 without a second message.
func TestExitSilently(t *testing.T) {
	if got := report(exitSilently(1)); got != 1 {
		t.Fatalf("exit %d", got)
	}
}

func TestClip(t *testing.T) {
	if clip("dev-01/task-10256-962b48d6", 20) != "dev-01/task-10256-9~" || clip("short", 20) != "short" {
		t.Fatal(clip("dev-01/task-10256-962b48d6", 20))
	}
}

func TestItemArgNativeIssueIDs(t *testing.T) {
	longID := "zm-" + strings.Repeat("a", 100)
	for _, tc := range []struct{ input, project, id string }{
		{"zm-abc1", "original", "zm-abc1"},
		{"zm/zm-abc1", "zm", "zm-abc1"},
		{"zm/" + longID, "zm", longID},
		{"zm/zm-Custom_ID", "zm", "zm-Custom_ID"},
		{"zm/bd:zm-abc1", "zm", "bd:zm-abc1"},
		{"gh:owner/repo#7", "original", "gh:owner/repo#7"},
		{"zm/gh:owner/repo#7", "zm", "gh:owner/repo#7"},
		{"zm/", "original", "zm/"},
		{"zm/zm-one/two", "original", "zm/zm-one/two"},
		{"zm/zm-two words", "original", "zm/zm-two words"},
	} {
		c := &client.Client{Project: "original"}
		if got := itemArg(c, tc.input); got != tc.id || c.Project != tc.project {
			t.Errorf("itemArg(%q) = %q in %q, want %q in %q", tc.input, got, c.Project, tc.id, tc.project)
		}
	}
}

// zm who on a key that is no work item names the plain lock's holder, and
// only when exactly one lease has that key.
func TestPlainLockWho(t *testing.T) {
	ls := []orchapi.Lease{{Key: "bd:zm-1", Holder: "m/aaaaaaaa"}, {Key: "land:migrations", Holder: "m/bbbbbbbb", Note: "n"}}
	l, ok := plainLock(ls, "land:migrations")
	if !ok || l.Holder != "m/bbbbbbbb" {
		t.Fatalf("plainLock = %+v %v", l, ok)
	}
	if _, ok := plainLock(ls, "land:other"); ok {
		t.Fatal("a key nobody leases was found")
	}
	if _, ok := plainLock(append(ls, orchapi.Lease{Key: "land:migrations", Holder: "x/cccccccc", Project: "01Q"}), "land:migrations"); ok {
		t.Fatal("a key leased in two projects picked one")
	}
	h := lockHolder(l)
	if h.ID != "land:migrations" || h.Lease == nil || h.MailTo != "m/bbbbbbbb" || h.Via != "holder of the lock land:migrations" {
		t.Fatalf("lockHolder = %+v", h)
	}
}
