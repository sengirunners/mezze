package agentpipe

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Arg is argv[1] of a built-in adapter process: `zm __zm-agent <driver>
// [--record <file>] -- <cli argv>`, or `zm __zm-agent <driver> describe`.
const Arg = orchapi.AgentArg

// MaxLine bounds one line in either direction (a runtime's tool result can
// be large; a longer line is dropped with a notice).
const MaxLine = 64 << 20

// stopWait is how long a stopping runtime gets after its stdin closes
// before TERM, and again before KILL.
const stopWait = 5 * time.Second

// Main runs an adapter process for a registered driver and returns its exit
// code. Stdin carries operations, stdout events, stderr the adapter's log.
func Main(args []string) int {
	log := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "zm-agent: "+format+"\n", a...) }
	if len(args) < 1 {
		log("usage: zm %s <driver> [--record <file>] -- <cli argv> | <driver> describe", Arg)
		return 2
	}
	d, ok := New(args[0])
	if !ok {
		log("no driver %q (have %v)", args[0], Names())
		return 2
	}
	rest := args[1:]
	if len(rest) == 1 && rest[0] == "describe" {
		_ = json.NewEncoder(os.Stdout).Encode(d.Describe())
		return 0
	}
	record := ""
	if len(rest) >= 2 && rest[0] == "--record" {
		record, rest = rest[1], rest[2:]
	}
	if len(rest) < 2 || rest[0] != "--" {
		log("usage: zm %s <driver> [--record <file>] -- <cli argv>", Arg)
		return 2
	}
	r := &Runner{Driver: d, CLI: rest[1:], In: os.Stdin, Out: os.Stdout, Log: log}
	if record != "" {
		rec, err := CreateRecording(record, args[0])
		if err != nil {
			log("record: %v", err)
			return 1
		}
		defer rec.Close()
		r.Rec = rec
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	r.Signals = sig
	return r.Run()
}

// Runner runs one runtime under one driver.
type Runner struct {
	Driver  Driver
	CLI     []string
	In      io.Reader
	Out     io.Writer
	Log     func(string, ...any)
	Rec     *Recording
	Signals <-chan os.Signal
	// Exec makes the runtime's command; tests and the conformance replay
	// swap it.
	Exec func(argv []string, dir string) (*exec.Cmd, error)
}

func lines(rd io.Reader, out chan<- []byte, log func(string, ...any)) {
	defer close(out)
	br := bufio.NewReaderSize(rd, 64<<10)
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A long line: gather it, bounded.
			buf := append([]byte(nil), b...)
			for errors.Is(err, bufio.ErrBufferFull) {
				b, err = br.ReadSlice('\n')
				if len(buf) <= MaxLine {
					buf = append(buf, b...)
				}
			}
			if len(buf) > MaxLine {
				log("dropped a line of %d+ bytes", MaxLine)
				buf = nil
			}
			b = buf
		} else {
			b = append([]byte(nil), b...)
		}
		if t := trimLine(b); len(t) > 0 {
			out <- t
		}
		if err != nil {
			return
		}
	}
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// Run waits for the start op, starts the runtime and translates until the
// runtime exits. It returns the adapter's exit code.
func (r *Runner) Run() int {
	if r.Exec == nil {
		r.Exec = execRuntime
	}
	enc := json.NewEncoder(r.Out)
	enc.SetEscapeHTML(false)
	em := &Emitter{Now: time.Now, Out: func(ev Event) {
		if err := enc.Encode(ev); err != nil {
			r.Log("write event: %v", err)
		}
	}}
	var stdin io.WriteCloser
	sendFailed := false
	var stopTimer, killTimer <-chan time.Time
	s := &Session{D: r.Driver, Em: em, Rec: r.Rec, Log: r.Log,
		Send: func(b []byte) {
			if stdin == nil || sendFailed {
				return
			}
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				sendFailed = true // the runtime is gone; its exit reports it
				r.Log("write to runtime: %v", err)
			}
		},
		Stopping: func() {
			if stdin != nil {
				_ = stdin.Close()
			}
			stopTimer = time.After(stopWait)
		}}
	ops := make(chan []byte, 16)
	go lines(r.In, ops, r.Log)

	var st *Start
	for st == nil {
		b, ok := <-ops
		if !ok {
			return 0 // zm went away before a start: nothing ran
		}
		st = s.Op(b)
	}
	argv, err := r.Driver.Argv(r.CLI, *st)
	var cmd *exec.Cmd
	var stdout io.ReadCloser
	if err == nil {
		cmd, err = r.Exec(argv, st.Cwd)
	}
	if err == nil {
		stdin, err = cmd.StdinPipe()
	}
	if err == nil {
		stdout, err = cmd.StdoutPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		s.Started(err)
		return 1
	}
	out := make(chan []byte, 64)
	go lines(stdout, out, r.Log)
	s.Started(nil)
	for out != nil {
		select {
		case b, ok := <-out:
			if !ok {
				out = nil
				break
			}
			s.Line(b)
		case b, ok := <-ops:
			if !ok {
				ops = nil // zm closed our stdin: the holder is going; end the runtime
				s.Stop()
				continue
			}
			s.Op(b)
		case sig := <-r.Signals:
			r.Log("signal %v: stopping", sig)
			s.Stop()
		case <-stopTimer:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			stopTimer, killTimer = nil, time.After(stopWait)
		case <-killTimer:
			_ = cmd.Process.Kill()
			killTimer = nil
		}
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(stopWait):
		_ = cmd.Process.Kill()
		<-exited
	}
	code, signaled := 0, false
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code, signaled = -1, true
		} else {
			code = ps.ExitCode()
		}
	}
	s.Exit(code, signaled)
	return 0
}

// execRuntime makes argv's command in dir with stderr passed through (it
// lands in the session's pipe-err file). The runtime stays in the adapter's
// process group, so a stop reaches both.
func execRuntime(argv []string, dir string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty runtime command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	if path, err = filepath.Abs(path); err != nil { // cmd.Dir must not change what runs
		return nil, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args = argv
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	return cmd, nil
}
