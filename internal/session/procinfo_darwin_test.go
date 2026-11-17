package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An lsof whose child keeps the output pipe open must not hold the
// sampler: listenPorts gives up within its timeout plus WaitDelay.
func TestListenPortsLsofStall(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "lsof")
	// The child outlives the kill and holds stdout; it exits on its own.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n(sleep 2) &\nsleep 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldP, oldT, oldW := lsofPath, lsofTimeout, lsofWaitDelay
	lsofPath, lsofTimeout, lsofWaitDelay = stub, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { lsofPath, lsofTimeout, lsofWaitDelay = oldP, oldT, oldW })
	start := time.Now()
	_, err := listenPorts([]int{os.Getpid()})
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("listenPorts held for %s", took)
	}
	if err == nil {
		t.Fatal("a timed-out lsof reported no error")
	}
	time.Sleep(2 * time.Second) // let the stub's child exit
}
