package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// The pairing URL printed before the tailnet is up names the node zm
// tailnet up will start: a new home's is potluck-<machine>, a home with an
// older node's state keeps zm-<machine>, a recorded or running name wins.
func TestTailnetNodeName(t *testing.T) {
	cfg := &config.Config{Home: t.TempDir(), Machine: "mini"}
	if got := tailnetNodeName(cfg, api.TailnetStatus{}); got != "potluck-mini" {
		t.Errorf("fresh home: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Home, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := tailnetNodeName(cfg, api.TailnetStatus{}); got != "potluck-mini" {
		t.Errorf("empty state dir: %q", got)
	}
	if err := os.WriteFile(filepath.Join(cfg.Home, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailnetNodeName(cfg, api.TailnetStatus{}); got != "zm-mini" {
		t.Errorf("home with an older node: %q", got)
	}
	cfg.File.Tailnet.Hostname = "Potluck-Studio"
	if got := tailnetNodeName(cfg, api.TailnetStatus{}); got != "potluck-studio" {
		t.Errorf("recorded name: %q", got)
	}
	if got := tailnetNodeName(cfg, api.TailnetStatus{Hostname: "zm-other"}); got != "zm-other" {
		t.Errorf("running node: %q", got)
	}
}
