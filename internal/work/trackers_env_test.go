package work

import (
	"os"
	"path/filepath"
	"testing"
)

// A ~ in config is the daemon user's home, not the adapter's tracker HOME.
func TestExpandConfigUsesDaemonHome(t *testing.T) {
	h, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	in := map[string]string{"bin": "~/.zm-copy/bin/bd-copy", "prefix": "bd", "dir": "/abs/.beads"}
	got := expandConfig(in)
	if want := filepath.Join(h, ".zm-copy/bin/bd-copy"); got["bin"] != want {
		t.Fatalf("bin = %q, want %q", got["bin"], want)
	}
	if got["prefix"] != "bd" || got["dir"] != "/abs/.beads" {
		t.Fatalf("non-~ values changed: %v", got)
	}
	if in["bin"] != "~/.zm-copy/bin/bd-copy" {
		t.Fatal("input map was modified")
	}
}
