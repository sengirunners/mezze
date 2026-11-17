package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An update stages under home (on disk), never the system temp, and clears a
// killed earlier run's leftovers first.
func TestUpdateScratchStagesUnderHomeAndSweepsLeftovers(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, "update-tmp", "zm-release-1")
	if err := os.MkdirAll(filepath.Join(old, "stage"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := updateScratch(home, "zm-release-")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(home, "update-tmp")+string(os.PathSeparator)) {
		t.Fatalf("scratch %s is not under home", dir)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("leftover %s survived: %v", old, err)
	}
}
