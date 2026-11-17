package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Process is the executable driver: one JSON request on stdin, one JSON
// response on stdout, exit 0 (SPEC 18.3).
type Process struct {
	Path string
	// Env is the whole environment the gate runs with (an allowlist the
	// caller builds; never the daemon's).
	Env []string

	mu   sync.Mutex
	desc *Description
	stat fileStamp // the file the cached describe came from
}

type fileStamp struct {
	size  int64
	mtime time.Time
}

type request struct {
	Zm   string `json:"zm"`
	Verb string `json:"verb"`
	Args any    `json:"args,omitempty"`
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// describeTimeout bounds describe itself (before its own timeout is known).
const describeTimeout = 30 * time.Second

// SetEnv replaces the environment the next calls run with.
func (p *Process) SetEnv(env []string) {
	p.mu.Lock()
	p.Env = env
	p.mu.Unlock()
}

func (p *Process) env() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Env
}

// Describe runs describe, cached until the file changes.
func (p *Process) Describe(ctx context.Context) (Description, error) {
	fi, err := os.Stat(p.Path)
	if err != nil {
		return Description{}, fmt.Errorf("gate %s: %w", p.Path, err)
	}
	st := fileStamp{fi.Size(), fi.ModTime()}
	p.mu.Lock()
	if p.desc != nil && p.stat == st {
		d := *p.desc
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()
	var d Description
	if err := p.call(ctx, describeTimeout, VerbDescribe, nil, &d); err != nil {
		return Description{}, err
	}
	if err := CheckDescription(d); err != nil {
		return Description{}, err
	}
	p.mu.Lock()
	p.desc, p.stat = &d, st
	p.mu.Unlock()
	return d, nil
}

// Match runs match when the gate offers it.
func (p *Process) Match(ctx context.Context, pr PR) ([]string, error) {
	d, err := p.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if !d.Has(VerbMatch) {
		return nil, ErrNoMatch
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := p.call(ctx, d.Timeout(), VerbMatch, map[string]any{"pr": pr}, &out); err != nil {
		return nil, err
	}
	return CheckIDs(out.IDs)
}

// Decide runs decide.
func (p *Process) Decide(ctx context.Context, req Request) (Decision, error) {
	d, err := p.Describe(ctx)
	if err != nil {
		return Decision{}, err
	}
	var out Decision
	if err := p.call(ctx, d.Timeout(), VerbDecide, req, &out); err != nil {
		return Decision{}, err
	}
	return CheckDecision(out)
}

func (p *Process) call(ctx context.Context, timeout time.Duration, verb string, args, out any) error {
	in, err := json.Marshal(request{Zm: "zm/v0", Verb: verb, Args: args})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Path)
	cmd.Env = p.env()
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr capped
	stdout.max, stderr.max = MaxOutput, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	ownGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("gate %s: no answer in %s", verb, timeout)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("gate %s: exit %d: %s", verb, ee.ExitCode(), firstLine(stderr.String()))
		}
		return fmt.Errorf("gate %s: %v", verb, err)
	case stdout.over:
		return fmt.Errorf("gate %s: answer over %d bytes", verb, MaxOutput)
	}
	var r response
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return fmt.Errorf("gate %s: answer is not JSON: %s", verb, firstLine(stdout.String()))
	}
	if !r.OK {
		msg := "refused"
		if r.Error != nil {
			msg = r.Error.Code + ": " + r.Error.Message
		}
		return fmt.Errorf("gate %s: %s", verb, CleanNote(firstLine(msg)))
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("gate %s: bad result: %v", verb, err)
	}
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(l) > 200 {
		l = l[:200] + "..."
	}
	return CleanNote(l)
}

// capped is a buffer that keeps at most max bytes and notes an overflow.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.Len(); len(b) > room {
		c.over = true
		if room > 0 {
			c.Buffer.Write(b[:room])
		}
		return len(b), nil
	}
	return c.Buffer.Write(b)
}
