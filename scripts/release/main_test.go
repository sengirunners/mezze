package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutKeepsOtherFiles: -out names a directory the release writes into,
// never one it deletes: `-out .` must not remove the repo. Only old
// tarballs, SHA256SUMS and .build are replaced.
func TestOutKeepsOtherFiles(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	out := t.TempDir()
	write := func(name string) {
		p := filepath.Join(out, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"go.mod", "src/main.go", "zm_v0_linux_amd64.tar.gz", "SHA256SUMS", ".build/junk"} {
		write(n)
	}
	// A bad target fails before anything is built, after the old output is cleared.
	if err := run("v0", "abc", "", out, []string{"nonsense"}); err == nil {
		t.Fatal("bad target accepted")
	}
	for _, keep := range []string{"go.mod", "src/main.go"} {
		if _, err := os.Stat(filepath.Join(out, keep)); err != nil {
			t.Errorf("release removed %s from -out: %v", keep, err)
		}
	}
	for _, gone := range []string{"zm_v0_linux_amd64.tar.gz", "SHA256SUMS", ".build"} {
		if _, err := os.Stat(filepath.Join(out, gone)); err == nil {
			t.Errorf("old %s left in -out", gone)
		}
	}
}

// TestCommandsPerTarget: every tarball can lend a machine (zm-run-farm runs
// anywhere), and each one zm host share can lend from carries the tart
// driver (potluck-driver-tart, and zm-run-tart, its old name), the driver it
// binds on every layer: Tart on Apple silicon, vm=oci on Linux.
// Intel Macs have neither layer.
func TestCommandsPerTarget(t *testing.T) {
	base := "zm bd-zm zm-work-conformance potluck-conformance zm-run-conformance zm-run-farm potluck-driver-local zm-run-local zm-connect"
	want := map[string]string{
		"darwin/arm64": base + " potluck-driver-tart zm-run-tart",
		"darwin/amd64": base,
		"linux/amd64":  base + " potluck-driver-tart zm-run-tart",
		"linux/arm64":  base + " potluck-driver-tart zm-run-tart",
	}
	for _, target := range Targets {
		goos, goarch, _ := strings.Cut(target, "/")
		if got := strings.Join(commandsFor(goos, goarch), " "); got != want[target] {
			t.Errorf("%s ships %q, want %q", target, got, want[target])
		}
		delete(want, target)
	}
	if len(want) != 0 {
		t.Errorf("targets not released: %v", want)
	}
	// Every listed command is a real main package: ./cmd here, or potluck's cmd
	// at the version go.mod requires.
	for _, c := range commandsFor("darwin", "arm64") {
		if PotluckCommands[c] {
			out, err := exec.Command("go", "list", "-f", "{{.Name}}", commandPkg(c)).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "main" {
				t.Errorf("%s: go list %s: %v %s", c, commandPkg(c), err, out)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", "cmd", c, "main.go")); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
}
