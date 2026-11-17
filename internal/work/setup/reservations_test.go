package setup

import (
	"testing"
	"time"
)

func TestMutationWaitsForScanButConflictsWithMutation(t *testing.T) {
	var prefixes Prefixes
	if !prefixes.TryScan("p", "bd") {
		t.Fatal("scan reservation")
	}
	// Release asynchronously: Reserve must wait, not report maintenance as
	// another user mutation or hold the map lock while waiting.
	go func() { time.Sleep(5 * time.Millisecond); prefixes.Release("p", "bd") }()
	result := make(chan bool, 1)
	go func() { result <- prefixes.Reserve("p", "bd") }()
	select {
	case reserved := <-result:
		if !reserved {
			t.Fatal("background scan caused a user-visible conflict")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mutation blocked scan release")
	}
	defer prefixes.Release("p", "bd")
	if prefixes.Reserve("p", "bd") || prefixes.TryScan("p", "bd") {
		t.Fatal("active mutation was not exclusive")
	}
}
