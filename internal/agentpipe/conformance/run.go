package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

// SubstEnv: see agentpipe.FakeSubstEnv.
const SubstEnv = agentpipe.FakeSubstEnv

// Proc is a running adapter process.
type Proc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	mu     sync.Mutex
	events []agentpipe.Event
	bad    []string
	cond   *sync.Cond
	done   bool
	stderr bytes.Buffer
}

// StartProc starts adapter (argv) with extra environment.
func StartProc(argv []string, env []string) (*Proc, error) {
	if len(argv) == 0 {
		return nil, errors.New("no adapter command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &Proc{cmd: cmd}
	p.cond = sync.NewCond(&p.mu)
	cmd.Stderr = &lockedBuf{p: p}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.in = in
	go func() {
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 64<<10), agentpipe.MaxLine)
		for s.Scan() {
			var ev agentpipe.Event
			p.mu.Lock()
			if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
				p.bad = append(p.bad, fmt.Sprintf("stdout line %d is not an event: %.200s", len(p.events)+1, s.Text()))
			} else {
				p.events = append(p.events, ev)
			}
			p.cond.Broadcast()
			p.mu.Unlock()
		}
		_ = cmd.Wait()
		p.mu.Lock()
		p.done = true
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	return p, nil
}

type lockedBuf struct{ p *Proc }

func (l *lockedBuf) Write(b []byte) (int, error) {
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	if l.p.stderr.Len() < 1<<20 {
		l.p.stderr.Write(b)
	}
	return len(b), nil
}

// Send writes one op.
func (p *Proc) Send(op agentpipe.Op) error {
	b, _ := json.Marshal(op)
	return p.SendRaw(b)
}

// SendRaw writes one op line as given.
func (p *Proc) SendRaw(b []byte) error {
	_, err := p.in.Write(append(append([]byte(nil), b...), '\n'))
	return err
}

// Wait waits until pred holds over the events so far, the process ends or
// the deadline passes; it returns whether pred held.
func (p *Proc) Wait(pred func([]agentpipe.Event) bool, d time.Duration) bool {
	timer := time.AfterFunc(d, func() { p.mu.Lock(); p.cond.Broadcast(); p.mu.Unlock() })
	defer timer.Stop()
	deadline := time.Now().Add(d)
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if pred(p.events) {
			return true
		}
		if p.done || time.Now().After(deadline) {
			return false
		}
		p.cond.Wait()
	}
}

// Events is a copy of the events so far.
func (p *Proc) Events() []agentpipe.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agentpipe.Event(nil), p.events...)
}

// Close ends stdin and waits for the process (killing its group after d).
func (p *Proc) Close(d time.Duration) (stderr string, bad []string) {
	_ = p.in.Close()
	if !p.Wait(func([]agentpipe.Event) bool { return false }, d) {
		p.mu.Lock()
		done := p.done
		p.mu.Unlock()
		if !done {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			p.Wait(func([]agentpipe.Event) bool { return false }, 5*time.Second)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String(), append([]string(nil), p.bad...)
}

// Run replays recording rec against an adapter process: adapter is its
// argv up to and excluding "--"; fake is the fake runtime's argv (the
// recording path is appended). Ops go in when the adapter has emitted as
// many events as when they were recorded.
func Run(adapter, fake []string, rec string, timeout time.Duration) ([]agentpipe.Event, []string, error) {
	rec, err := filepath.Abs(rec)
	if err != nil {
		return nil, nil, err
	}
	_, entries, err := Load(rec)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "zm-agent")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	recorded := ""
	for _, e := range entries {
		if e.T == agentpipe.RecOp {
			var op agentpipe.Op
			if json.Unmarshal(e.Op, &op) == nil && op.Op == agentpipe.OpStart && op.Start != nil {
				recorded = op.Start.Cwd
				break
			}
		}
	}
	argv := append(append(append([]string(nil), adapter...), "--"), append(append([]string(nil), fake...), rec)...)
	p, err := StartProc(argv, []string{SubstEnv + "=" + dir + "=" + recorded})
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(timeout)
	var problems []string
	for _, e := range entries {
		if e.T != agentpipe.RecOp {
			continue
		}
		after := int(e.After)
		if !p.Wait(func(evs []agentpipe.Event) bool { return len(evs) >= after }, time.Until(deadline)) {
			problems = append(problems, fmt.Sprintf("the adapter stopped at %d events, before the op recorded after %d", len(p.Events()), after))
			break
		}
		b := e.Op
		var op agentpipe.Op
		if json.Unmarshal(b, &op) == nil && op.Op == agentpipe.OpStart && op.Start != nil {
			op.Start.Cwd = dir
			b, _ = json.Marshal(op)
		}
		if err := p.SendRaw(b); err != nil {
			problems = append(problems, "write op: "+err.Error())
			break
		}
	}
	// The adapter ends when its runtime does (the fake exits as recorded).
	p.Wait(func(evs []agentpipe.Event) bool {
		n := len(evs)
		return n > 0 && evs[n-1].Type == agentpipe.EvSession && (evs[n-1].Status == agentpipe.StateStopped || evs[n-1].Status == agentpipe.StateError)
	}, time.Until(deadline))
	stderr, bad := p.Close(10 * time.Second)
	problems = append(problems, bad...)
	if strings.Contains(stderr, "fake runtime:") {
		problems = append(problems, "the fake runtime refused a line: "+stderr)
	} else if len(problems) > 0 && stderr != "" {
		problems = append(problems, "adapter stderr: "+agentpipe.Clip(stderr, 4000))
	}
	events := p.Events()
	for i := range events {
		if events[i].Cwd == dir {
			events[i].Cwd = recorded
		}
	}
	return events, problems, nil
}
