// Package tasklaunch gates a Claude capability probe behind durable task launch.
// It runs inside the holder's managed group and never consumes the task prompt.
package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const Arg = "__zm-task-claude"
const ProbeTimeout = 3 * time.Second
const CaptureLimit = 4096

// Main runs only in the private managed child. fd3 is a one-shot release pipe
// from the daemon; EOF or an invalid grant cannot execute any agent code.
func Main(args []string) int {
	gate := os.NewFile(3, "task-launch-grant")
	if gate == nil {
		fmt.Fprintln(os.Stderr, "Claude task launch: missing recorded-launch grant")
		return 1
	}
	defer gate.Close()
	syscall.CloseOnExec(3)
	if err := run(gate, args); err != nil {
		// Plain stderr, never a forged provider protocol/terminal event.
		fmt.Fprintln(os.Stderr, "Claude task launch:", err)
		return 1
	}
	return 0
}

func run(gate io.Reader, argv []string) error {
	var grant [1]byte
	if _, err := io.ReadFull(gate, grant[:]); err != nil || grant[0] != 1 {
		return errors.New("recorded launch was not authorized")
	}
	if closer, ok := gate.(io.Closer); ok {
		_ = closer.Close()
	}
	if len(argv) < 4 {
		return errors.New("invalid guarded command")
	}
	n, err := strconv.Atoi(argv[0])
	if err != nil || n < 1 || n > 1024 || len(argv) <= n+1 {
		return errors.New("invalid guarded probe command")
	}
	if err := probe(argv[1:n+1], os.Environ(), ProbeTimeout); err != nil {
		return err
	}
	argv = argv[n+1:]
	return syscall.Exec(argv[0], argv, os.Environ())
}

// Arguments is the private launcher argv after argv[0]. Both vectors are
// already resolved by spawn; sandbox tasks use their existing wrapper/policy.
func Arguments(probe, command []string) []string {
	args := []string{Arg, strconv.Itoa(len(probe))}
	args = append(args, probe...)
	return append(args, command...)
}

type capture struct {
	data     []byte
	overflow bool
}

func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := CaptureLimit - len(c.data)
	if len(p) > remaining {
		c.overflow = true
		p = p[:remaining]
	}
	c.data = append(c.data, p...)
	return n, nil
}

func probe(argv []string, env []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	// nil stdin means /dev/null; do not read or advance the original prompt fd.
	// No SysProcAttr: the probe inherits the recorded task process group.
	var out capture
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("Claude version probe timed out")
		}
		return errors.New("Claude version probe failed")
	}
	if out.overflow || !SupportedVersion(string(out.data)) {
		return errors.New("task skills require Claude Code 2.1.318 or newer with a verifiable version")
	}
	return nil
}

func SupportedVersion(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	pieces := strings.Split(fields[0], ".")
	if len(pieces) != 3 {
		return false
	}
	var version [3]int
	for i, p := range pieces {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		version[i] = v
	}
	minimum := [3]int{2, 1, 318}
	for i, v := range version {
		if v != minimum[i] {
			return v > minimum[i]
		}
	}
	return true
}
