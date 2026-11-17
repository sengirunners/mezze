package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunUsage(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("no adapter: %d", code)
	}
	if code := run([]string{"--read-only", "--write", "acme"}); code != 2 {
		t.Fatalf("--read-only with --write: %d", code)
	}
	if code := run([]string{"--config", "novalue", "acme"}); code != 2 {
		t.Fatalf("bad --config: %d", code)
	}
	p := filepath.Join(t.TempDir(), "zm-work-crash")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{p, "--read-only", "--config", "dir=/x"}); code != 1 {
		t.Fatalf("failing adapter: %d", code)
	}
}
