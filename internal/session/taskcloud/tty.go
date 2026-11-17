package taskcloud

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

// tailCapped keeps the last outputCap bytes: on a terminal the session's
// link comes after any redraw noise.
type tailCapped struct{ buf []byte }

func (t *tailCapped) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*outputCap {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-outputCap:]...)
	}
	return len(p), nil
}

func (t *tailCapped) String() string {
	b := t.buf
	if len(b) > outputCap {
		b = b[len(b)-outputCap:]
	}
	return string(b)
}

// runCLITerminal is runCLI with a terminal (wide, so a link never wraps) for
// the CLI's stdin and output, which come back as one stream with the
// terminal's CRs. Nothing is typed into it: a CLI that stops to ask waits
// until the timeout kills it.
func runCLITerminal(ctx context.Context, timeout time.Duration, dir string, argv ...string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 1000})
	if err != nil {
		return -1, err.Error()
	}
	var b tailCapped
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			_, _ = b.Write(buf[:n])
			if err != nil {
				return // EIO once every holder of the terminal is gone
			}
		}
	}()
	werr := cmd.Wait()
	select { // a grandchild holding the terminal does not outlast the CLI
	case <-done:
	case <-time.After(2 * time.Second):
	}
	_ = f.Close()
	<-done
	out := b.String()
	if werr == nil {
		return 0, out
	}
	if ee, ok := werr.(*exec.ExitError); ok && ctx.Err() == nil {
		return ee.ExitCode(), out
	}
	return -1, strings.TrimSpace(out + "\n" + werr.Error())
}
