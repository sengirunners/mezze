//go:build darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// TestInPlaceOverwriteKeepsSessions: the installed zm overwritten
// in place under a running daemon (cp over ~/go/bin/zm). macOS kills every
// new exec of that file (code signing), yet the running session lives, a new
// one still starts (its holder runs the daemon's private copy), and after the
// daemon is SIGKILLed and a proper install puts a good binary back, the next
// daemon adopts both.
func TestInPlaceOverwriteKeepsSessions(t *testing.T) {
	z := newZeroEnv(t)
	z.env = append(z.env, "ZM_NO_AUTOSTART=1")
	if err := os.MkdirAll(z.zmHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1}}`
	if err := os.WriteFile(filepath.Join(z.zmHome(), "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(z.home, "inst", "zm") // the daemon's installed binary
	cli := filepath.Join(z.home, "cli", "zm")   // the CLI the test drives it with
	copyExe(t, os.Args[0], inst)
	copyExe(t, os.Args[0], cli)
	if _, e, code := z.runBin(inst, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	pid0 := z.daemonPID()
	run := func(word string) (string, int) {
		t.Helper()
		out, e, code := z.runBin(cli, "run", "-d", "--agent", "shell", "/bin/sh", "-c", "echo "+word+"; exec cat")
		if code != 0 {
			t.Fatalf("run %s: %d %s %s", word, code, out, e)
		}
		id := strings.TrimSpace(out)
		ss := z.showSession(cli, id)
		t.Cleanup(func() { _ = syscall.Kill(ss.PID, syscall.SIGKILL) })
		z.waitOutput(cli, id, word)
		return id, ss.PID
	}
	idA, pidA := run("a-ready")

	// Overwrite the installed file in place: same inode, one byte changed.
	b, err := os.ReadFile(inst)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(inst, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{b[len(b)/2] ^ 0xff}, int64(len(b)/2)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, code := z.runBin(inst, "--version"); code == 0 {
		t.Fatal("a binary overwritten in place still execs: the hazard this test guards is gone")
	}

	idB, pidB := run("b-ready") // its holder must not exec the overwritten file
	if syscall.Kill(pidA, 0) != nil {
		t.Fatal("the running session died when its binary was overwritten")
	}

	if err := syscall.Kill(pid0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && syscall.Kill(pid0, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	// A proper install (new file, renamed over) and the next daemon.
	copyExe(t, os.Args[0], inst+".new")
	if err := os.Rename(inst+".new", inst); err != nil {
		t.Fatal(err)
	}
	if _, e, code := z.runBin(inst, "daemon", "--detach"); code != 0 {
		t.Fatalf("restart: %s", e)
	}
	for _, c := range []struct {
		id  string
		pid int
	}{{idA, pidA}, {idB, pidB}} {
		got := z.showSession(cli, c.id)
		if got.State == orchapi.StateEnded || got.PID != c.pid || got.Unreachable != "" {
			t.Fatalf("session %s after the kill and reinstall: %+v", c.id, got)
		}
		if _, e, code := z.runBin(cli, "session", "input", c.id, "typed-"+c.id); code != 0 {
			t.Fatalf("input: %s", e)
		}
		z.waitOutput(cli, c.id, "typed-"+c.id)
	}
	log, _ := os.ReadFile(filepath.Join(z.zmHome(), "daemon.log"))
	if !strings.Contains(string(log), "previous daemon ended without a clean exit") {
		t.Fatalf("the next daemon did not say how the last one ended:\n%s", log)
	}
}

func (z *zeroEnv) showSession(bin, id string) orchapi.Session {
	z.t.Helper()
	o, e, code := z.runBin(bin, "session", "show", id, "--json")
	var ss orchapi.Session
	if code != 0 || json.Unmarshal([]byte(o), &ss) != nil {
		z.t.Fatalf("session show: %d %s %s", code, o, e)
	}
	return ss
}

func (z *zeroEnv) waitOutput(bin, id, want string) {
	z.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		o, _, _ := z.runBin(bin, "session", "output", id)
		if strings.Contains(o, want) {
			return
		}
		if time.Now().After(deadline) {
			z.t.Fatalf("output of %s never had %q: %q", id, want, o)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
