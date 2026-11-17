package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDaemonExitCause: a clean exit leaves no run marker; a daemon that never
// reached one (SIGKILL) is reported by the next start, a crash pointing at
// its crash log; caught signals are named on the way out.
func TestDaemonExitCause(t *testing.T) {
	home := t.TempDir()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	done := markRunning(home, log)
	done()
	noteLastExit(home, log)
	if buf.Len() != 0 {
		t.Fatalf("a clean exit was reported: %s", buf.String())
	}

	_ = markRunning(home, log) // never done: like a SIGKILL
	noteLastExit(home, log)
	if s := buf.String(); !strings.Contains(s, "previous daemon ended without a clean exit") ||
		!strings.Contains(s, "SIGKILL") || !strings.Contains(s, "pid="+strconv.Itoa(os.Getpid())) {
		t.Fatalf("killed daemon: %s", s)
	}

	buf.Reset()
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(home, crashLogName), []byte("panic: boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noteLastExit(home, log)
	if !strings.Contains(buf.String(), "crashed: see") {
		t.Fatalf("crashed daemon: %s", buf.String())
	}

	ctx, which, stop := exitSignals(t.Context())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("HUP did not end the daemon's context")
	}
	if got := exitCause(which(), nil); got != "signal hangup" {
		t.Fatalf("cause: %q", got)
	}
	if got := exitCause("", errors.New("listen: busy")); got != "error: listen: busy" {
		t.Fatalf("cause: %q", got)
	}
}
