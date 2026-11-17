//go:build unix

package adapter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// gone waits up to 3 s for pid to disappear (a killed orphan is reaped by init).
func gone(pid int) bool {
	for range 60 {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func readPid(t *testing.T, f string) int {
	t.Helper()
	for range 40 {
		if b, err := os.ReadFile(f); err == nil && len(bytes.TrimSpace(b)) > 0 {
			pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", f)
	return 0
}

// A tracker CLI the adapter runs (here sleep) dies with the adapter when the
// call times out, and a child left holding the adapter's stdout after it
// answered is killed too: Exec never leaves a process behind.
func TestExecKillsProcessGroup(t *testing.T) {
	ctx := context.Background()
	old := Timeout
	Timeout = 2 * time.Second // room for the script to start sleep under load
	defer func() { Timeout = old }()
	dir := t.TempDir()

	hung := filepath.Join(dir, "hung.pid")
	p := script(t, "zm-work-hung", `cat >/dev/null; sleep 60 >/dev/null 2>&1 & echo $! >`+hung+`; wait`)
	if err := Call(ctx, p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	pid := readPid(t, hung)
	if !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("tracker child %d outlived a timed-out adapter", pid)
	}

	left := filepath.Join(dir, "left.pid")
	p = script(t, "zm-work-left", `cat >/dev/null; sleep 60 & echo $! >`+left+`; echo '{"ok":true,"result":{}}'`)
	Timeout = 10 * time.Second
	start := time.Now()
	_ = Call(ctx, p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Exec hung on a child holding stdout (%s)", time.Since(start))
	}
	pid = readPid(t, left)
	if !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child %d left holding the adapter's stdout survived", pid)
	}
}
