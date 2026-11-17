package holder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/sengirunners/mezze/internal/session/taskcloud"
	"github.com/sengirunners/mezze/internal/session/tasklaunch"
	"github.com/sengirunners/potluck/kit/api"
)

// The holder runs before anything else when this binary was started as one.
func init() {
	if len(os.Args) > 1 && os.Args[1] == PriorityArg {
		os.Exit(priorityMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == tasklaunch.Arg {
		os.Exit(tasklaunch.Main(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == taskcloud.Arg {
		os.Exit(taskcloud.Main(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "__zm-task-anchor" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		// This process created and leads the task group. On holder death,
		// stop that group before its identity anchor disappears.
		if pid := os.Getpid(); pid > 1 && syscall.Getpgrp() == pid {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == Arg {
		os.Exit(Main(os.Args[2:]))
	}
}

const (
	scrollbackSize = 1 << 20
	drainWait      = 500 * time.Millisecond // after the child exits, for its last output
	ackWait        = 10 * time.Second       // for the daemon to record the exit
	tiedTermWait   = 2 * time.Second        // a tied holder whose daemon went: TERM, then KILL
)

// cursorQuery is a terminal's cursor position request; cursorReply the
// answer given while no daemon is connected (the daemon answers otherwise).
var cursorQuery, cursorReply = []byte("\x1b[6n"), []byte("\x1b[1;1R")

var signals = map[string]syscall.Signal{"TERM": syscall.SIGTERM, "INT": syscall.SIGINT,
	"KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP}

type holder struct {
	spec       Spec
	cmd        *exec.Cmd
	anchor     *exec.Cmd
	anchorTie  *os.File
	pty        *os.File
	stream     *os.File
	spool      *os.File
	scrollback *os.File
	spooled    int64
	overflow   bool
	out        *Ring

	mu      sync.Mutex
	peer    *peer // the adopted daemon connection, if any
	exit    *Exit // set once the child is gone
	stopSig string
	stopBy  string

	exited chan struct{} // the exit is recorded in the exit file
	acked  chan struct{}
	ack    sync.Once
}

type peer struct {
	c    *net.UnixConn
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
}

func (p *peer) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.c.Close()
	})
}

func (p *peer) write(typ byte, payload []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return WriteFrame(p.c, typ, payload)
}

// Main is a holder process: args are <home> <session8>, the Spec arrives on
// stdin. It returns the process's exit code.
func Main(args []string) int {
	// Caught, not ignored: an ignored signal stays ignored in the child
	// after exec, and the agent must see HUP and INT.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP, syscall.SIGINT)
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("zm-holder: ")
	ready := json.NewEncoder(os.Stdout)
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		_ = ready.Encode(Ready{Error: msg})
		log.Print(msg)
		return 1
	}
	if len(args) != 2 {
		return fail("usage: zm %s <home> <session8>", Arg)
	}
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return fail("read spec: %v", err)
	}
	var spec Spec
	if err := json.Unmarshal(line, &spec); err != nil {
		return fail("spec: %v", err)
	}
	if spec.Home != args[0] || spec.Session != args[1] || len(spec.Args) == 0 {
		return fail("spec does not match argv")
	}
	if err := preflightTaskEnv(spec); err != nil {
		return fail("%v", err)
	}
	if err := os.MkdirAll(Dir(spec.Home), 0o700); err != nil {
		return fail("%v", err)
	}
	sock := SocketPath(spec.Home, spec.Session)
	_ = os.Remove(sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return fail("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false) // removed by hand, once the exit is handed over
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sock)
		return fail("%v", err)
	}
	cmd := exec.Command(spec.Path, spec.Args[1:]...)
	cmd.Args = spec.Args
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	var launchGate *os.File
	if spec.RecordedLaunch {
		if !spec.Task {
			return fail("recorded launch requires a task")
		}
		launchGate = os.NewFile(3, "recorded-task-launch")
		syscall.CloseOnExec(3)
		defer launchGate.Close()
		cmd.ExtraFiles = []*os.File{launchGate}
	}
	var f *os.File
	var stream, stderr, spool, stderrSpool, scrollback, anchorTie *os.File
	var anchor *exec.Cmd
	if spec.Task {
		var stdoutWrite, stderrWrite, prompt *os.File
		exe, e := os.Executable()
		if e != nil {
			err = e
		} else {
			var anchorRead *os.File
			anchorRead, anchorTie, err = os.Pipe()
			if err == nil {
				anchor = exec.Command(exe, "__zm-task-anchor")
				anchor.Stdin = anchorRead
				anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				err = anchor.Start()
				_ = anchorRead.Close()
			}
		}
		if err == nil {
			err = os.MkdirAll(filepath.Dir(TaskOutputPath(spec.Home, spec.Session)), 0700)
		}
		if err == nil {
			spool, err = os.OpenFile(TaskOutputPath(spec.Home, spec.Session), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		}
		if err == nil {
			stderrSpool, err = os.OpenFile(TaskErrorPath(spec.Home, spec.Session), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		}
		if err == nil {
			scrollback, err = os.OpenFile(ScrollbackPath(spec.Home, spec.Session), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		}
		if err == nil {
			stream, stdoutWrite, err = os.Pipe()
		}
		if err == nil {
			stderr, stderrWrite, err = os.Pipe()
		}
		if err == nil {
			f, err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		}
		if err == nil {
			prompt, err = os.CreateTemp(filepath.Dir(TaskOutputPath(spec.Home, spec.Session)), ".task-input-*")
			if err == nil {
				err = os.Remove(prompt.Name())
			}
			if err == nil {
				_, err = prompt.WriteString(spec.TaskPrompt)
			}
			if err == nil {
				_, err = prompt.Seek(0, io.SeekStart)
			}
		}
		if err == nil {
			cmd.Stdin, cmd.Stdout, cmd.Stderr = prompt, stdoutWrite, stderrWrite
			cmd.SysProcAttr = taskProcessAttr(anchor.Process.Pid)
			cmd.WaitDelay = time.Second
			err = startPriority(cmd, spec.Nice, cmd.Start)
			if launchGate != nil {
				_ = launchGate.Close()
			}
		}
		if stdoutWrite != nil {
			_ = stdoutWrite.Close()
		}
		if stderrWrite != nil {
			_ = stderrWrite.Close()
		}
		if prompt != nil {
			_ = prompt.Close()
		}
		if err != nil {
			if anchor != nil && anchor.Process != nil {
				_ = anchor.Process.Kill()
				_ = anchor.Wait()
			}
			for _, x := range []*os.File{f, stream, stderr, spool, stderrSpool, scrollback, anchorTie} {
				if x != nil {
					_ = x.Close()
				}
			}
		}
	} else if spec.Pipe {
		f, stream, stderr, spool, stderrSpool, err = startPipe(cmd, spec)
	} else {
		err = startPriority(cmd, spec.Nice, func() error {
			var e error
			f, e = pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(spec.Rows), Cols: uint16(spec.Cols)})
			return e
		})
		if err != nil && f != nil {
			_ = f.Close()
		}
		stream = f
	}
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(sock)
		return fail("start %s: %v", spec.Args[0], err)
	}
	var out *Ring
	out = NewRing(scrollbackSize)
	spec.TaskPrompt = ""
	h := &holder{spec: spec, cmd: cmd, anchor: anchor, anchorTie: anchorTie, pty: f, stream: stream, spool: spool, scrollback: scrollback, out: out,
		exited: make(chan struct{}), acked: make(chan struct{})}
	group := 0
	if anchor != nil {
		group = anchor.Process.Pid
	}
	_ = ready.Encode(Ready{ChildPID: cmd.Process.Pid, GroupPID: group})
	_ = os.Stdout.Close()
	if spec.Tied {
		go h.tied(in)
	}
	readDone := make(chan struct{})
	var stderrDone chan struct{}
	if stderr != nil {
		stderrDone = make(chan struct{})
		go func() {
			defer close(stderrDone)
			defer stderr.Close()
			defer stderrSpool.Close()
			buf := make([]byte, 32<<10)
			var size int64
			overflow := false
			limit := int64(32 << 20)
			if spec.Pipe {
				limit = PipeErrLimit
			}
			for {
				n, e := stderr.Read(buf)
				if n > 0 {
					if !spec.Pipe { // a pipe session's stdout is its event stream: stderr stays out of it
						_, _ = h.out.Write(buf[:n])
					}
					left := limit - size
					if int64(n) > left {
						overflow = true
					}
					if left > 0 {
						written, werr := stderrSpool.Write(buf[:min(int64(n), left)])
						size += int64(written)
						if werr != nil || int64(written) != min(int64(n), left) {
							overflow = true
						}
					}
				}
				if e != nil {
					break
				}
			}
			if overflow && !spec.Pipe {
				_ = writeFile(TaskOverflowPath(spec.Home, spec.Session), []byte("stderr output exceeded 32 MiB"))
			}
		}()
	}
	go h.readLoop(readDone)
	go h.accept(ln)
	h.waitLoop(readDone, stderr, stderrDone)

	// Hand the exit over: an adopted daemon gets it from the stream and acks.
	h.mu.Lock()
	adopted := h.peer != nil
	h.mu.Unlock()
	if adopted {
		select {
		case <-h.acked:
		case <-time.After(ackWait):
		}
	}
	_ = ln.Close()
	_ = os.Remove(sock)
	h.mu.Lock()
	if h.peer != nil {
		h.peer.close()
	}
	h.mu.Unlock()
	return 0
}

// tied ends the child once the daemon that started this holder is gone.
func (h *holder) tied(in io.Reader) {
	_, _ = io.Copy(io.Discard, in)
	h.signal(syscall.SIGTERM, "TERM", "")
	select {
	case <-h.exited:
	case <-time.After(tiedTermWait):
		h.signal(syscall.SIGKILL, "KILL", "")
	}
}

func (h *holder) signal(sig syscall.Signal, name, by string) {
	h.mu.Lock()
	if h.exit != nil {
		h.mu.Unlock()
		return
	}
	h.stopSig, h.stopBy = name, by
	h.mu.Unlock()
	pid := h.cmd.Process.Pid
	if h.anchor != nil {
		pid = h.anchor.Process.Pid
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = h.cmd.Process.Signal(sig)
	}
}

// readLoop copies PTY output into the ring until the PTY closes.
func (h *holder) readLoop(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := h.stream.Read(buf)
		if n > 0 {
			if h.spool != nil {
				remaining := h.spoolLimit() - h.spooled
				if remaining < int64(n) {
					h.overflow = true
				}
				if remaining > 0 {
					count, e := h.spool.Write(buf[:min(int64(n), remaining)])
					h.spooled += int64(count)
					if e != nil || int64(count) != min(int64(n), remaining) {
						h.overflow = true
					}
				}
			}
			_, _ = h.out.Write(buf[:n])
			h.mu.Lock()
			alone := h.peer == nil
			h.mu.Unlock()
			if !h.spec.Task && !h.spec.Pipe && alone && bytes.Contains(buf[:n], cursorQuery) {
				_, _ = h.pty.Write(cursorReply)
			}
		}
		if err != nil {
			return
		}
	}
}

// waitLoop waits for the child, then writes the exit file and the
// scrollback, and closes the ring (adopted daemons then get the exit).
func (h *holder) waitLoop(readDone chan struct{}, stderr *os.File, stderrDone chan struct{}) {
	_ = h.cmd.Wait()
	wait := drainWait
	if h.spec.Task || h.spec.Pipe {
		wait = 5 * time.Second
	}
	drainDeadline := time.Now().Add(wait)
	select {
	case <-readDone:
	case <-time.After(wait):
		if h.spec.Task || h.spec.Pipe {
			_ = h.stream.Close()
			<-readDone
		}
	}
	if h.stream != h.pty {
		_ = h.stream.Close()
	}
	if stderrDone != nil {
		select {
		case <-stderrDone:
		case <-time.After(max(0, time.Until(drainDeadline))):
			_ = stderr.Close()
			<-stderrDone
		}
	}
	if h.spool != nil {
		_ = h.spool.Close()
		if h.overflow && h.spec.Task {
			_ = writeFile(TaskOverflowPath(h.spec.Home, h.spec.Session), []byte("output exceeded limit or could not be spooled"))
		}
	}
	_ = h.pty.Close()
	var groupStopped *bool
	if h.anchor != nil {
		h.mu.Lock()
		stopSig := h.stopSig
		h.mu.Unlock()
		if stopSig == "" || stopSig == "KILL" {
			err := syscall.Kill(-h.anchor.Process.Pid, syscall.SIGKILL)
			stopped := err == nil || errors.Is(err, syscall.ESRCH)
			groupStopped = &stopped
		} else {
			stopped := false
			groupStopped = &stopped
			// A soft stop did not authorize KILL. The anchor is an internal
			// process; ask it to exit using that same soft signal.
			if sig, ok := signals[stopSig]; ok {
				_ = h.anchor.Process.Signal(sig)
			}
		}
		_ = h.anchor.Wait()
		_ = h.anchorTie.Close()
	} else {
		_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGHUP)
	}
	x := Exit{Code: -1, At: time.Now().UTC(), GroupStopped: groupStopped}
	if ps := h.cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			x.Signal = sigName(ws.Signal())
		} else {
			x.Code = ps.ExitCode()
		}
	}
	_, end := h.out.Bounds()
	x.OutBytes = end
	h.mu.Lock()
	x.StopSig, x.StopBy = h.stopSig, h.stopBy
	h.mu.Unlock()
	// The files first: a daemon that finds this holder gone, or refused,
	// reads the exit from them.
	data, _, _, _, _ := h.out.Next(0, scrollbackSize)
	if h.scrollback != nil {
		if _, err := h.scrollback.Write(data); err != nil {
			log.Printf("scrollback: %v", err)
		}
		_ = h.scrollback.Close()
	} else if err := writeFile(ScrollbackPath(h.spec.Home, h.spec.Session), data); err != nil {
		log.Printf("scrollback: %v", err)
	}
	b, _ := json.Marshal(x)
	if err := writeFile(ExitPath(h.spec.Home, h.spec.Session), b); err != nil {
		log.Printf("exit file: %v", err)
	}
	h.mu.Lock()
	h.exit = &x
	h.mu.Unlock()
	close(h.exited)
	h.out.Close()
}

// writeFile writes atomically (temp file, rename), 0600.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sigName(sig syscall.Signal) string {
	for k, v := range signals {
		if v == sig {
			return k
		}
	}
	return unix.SignalName(sig)
}

func (h *holder) accept(ln *net.UnixListener) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		go h.serve(c)
	}
}

// serve is one daemon connection: hello, then frames until it hangs up.
func (h *holder) serve(c *net.UnixConn) {
	p := &peer{c: c, done: make(chan struct{})}
	defer func() {
		p.close()
		h.mu.Lock()
		if h.peer == p {
			h.peer = nil
		}
		h.mu.Unlock()
	}()
	start, end := h.out.Bounds()
	major, minor := Proto()
	h.mu.Lock()
	exited := h.exit != nil
	h.mu.Unlock()
	group := 0
	if h.anchor != nil {
		group = h.anchor.Process.Pid
	}
	hello := Hello{Proto: major, Minor: minor, ReplayModes: 1, Version: api.Version, Commit: api.Commit, Session: h.spec.Session, GroupPID: group,
		Home: h.spec.Home, HolderPID: os.Getpid(), ChildPID: h.cmd.Process.Pid, Start: start, End: end, Exited: exited, Pipe: h.spec.Pipe}
	b, _ := json.Marshal(hello)
	if p.write(FrameHello, b) != nil {
		return
	}
	for {
		typ, payload, err := ReadFrame(c)
		if err != nil {
			return
		}
		switch typ {
		case FrameAdopt:
			var a Adopt
			if json.Unmarshal(payload, &a) != nil || !h.adopt(p, a) {
				return
			}
		case FrameSignal:
			var s Signal
			if json.Unmarshal(payload, &s) == nil {
				if sig, ok := signals[s.Sig]; ok {
					h.signal(sig, s.Sig, s.By)
				}
			}
		case FrameAck:
			h.ack.Do(func() { close(h.acked) })
		default: // a newer daemon's frame: ignored
		}
	}
}

// adopt makes p the daemon connection: the PTY master, then output from
// a.Since. A previous connection is dropped.
func (h *holder) adopt(p *peer, a Adopt) bool {
	h.mu.Lock()
	if h.exit != nil {
		h.mu.Unlock()
		return false // the daemon reads the exit file
	}
	old := h.peer
	h.peer = p
	h.mu.Unlock()
	if old != nil && old != p {
		old.close()
	}
	rc, err := h.pty.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	p.wmu.Lock()
	cerr := rc.Control(func(fd uintptr) {
		hdr := make([]byte, headerLen)
		hdr[0] = FrameFD
		_, _, serr = p.c.WriteMsgUnix(hdr, unix.UnixRights(int(fd)), nil)
	})
	p.wmu.Unlock()
	if cerr != nil || serr != nil {
		// The PTY closed under us: the child is exiting. Hang up only once
		// the exit file is written, so the daemon finds it.
		select {
		case <-h.exited:
		case <-time.After(ackWait):
		}
		return false
	}
	go h.pump(p, a.Since, a.ReplayModes == 1)
	return true
}

// pump streams the ring to p from off, then the exit.
func (h *holder) pump(p *peer, off int64, replay bool) {
	first := true
	for {
		var data []byte
		var from, next int64
		var wait <-chan struct{}
		var closed bool
		var checkpoint *ReplayCheckpoint
		if replay {
			chunk := h.out.nextReplay(off, 32<<10, first)
			data, from, next, wait, closed = chunk.Data, chunk.From, chunk.To, chunk.Wait, chunk.Closed
			if chunk.HasCheckpoint {
				checkpoint = &ReplayCheckpoint{From: from, State: chunk.State}
			}
		} else {
			data, from, next, wait, closed = h.out.Next(off, 32<<10)
		}
		if len(data) > 0 {
			if p.writeOutput(from, data, checkpoint) != nil {
				p.close()
				return
			}
			off = next
			first = false
			continue
		}
		if closed {
			h.mu.Lock()
			x := h.exit
			h.mu.Unlock()
			if x != nil {
				b, _ := json.Marshal(x)
				_ = p.write(FrameExit, b)
			}
			return
		}
		select {
		case <-wait:
		case <-p.done:
			return
		}
	}
}

// writeOutput serializes the metadata/output pair under one writer lock. FD
// transfer has already completed; no metadata precedes the adoption FD.
func (p *peer) writeOutput(from int64, data []byte, checkpoint *ReplayCheckpoint) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if checkpoint != nil {
		b, err := EncodeReplay(*checkpoint)
		if err != nil {
			return err
		}
		if err = WriteFrame(p.c, FrameReplay, b); err != nil {
			return err
		}
	}
	return WriteFrame(p.c, FrameOutput, outputFrame(from, data))
}

func (h *holder) spoolLimit() int64 {
	if h.spec.Pipe {
		return PipeSpoolLimit
	}
	return 32 << 20
}

// startPipe starts a pipe-mode child: its own session and process group,
// stdin a pipe whose write end the holder keeps (and hands to the daemon in
// place of a PTY master, so a daemon restart never closes it), stdout the
// event stream, stderr a capped file. The returned in is the stdin write
// end, out the stdout read end.
func startPipe(cmd *exec.Cmd, spec Spec) (in, out, errOut, spool, errSpool *os.File, err error) {
	if spec.Task {
		return nil, nil, nil, nil, nil, errors.New("pipe mode is not for tasks")
	}
	var inR, outW, errW *os.File
	closeAll := func() {
		for _, x := range []*os.File{inR, in, out, outW, errOut, errW, spool, errSpool} {
			if x != nil {
				_ = x.Close()
			}
		}
	}
	if err = os.MkdirAll(filepath.Dir(PipeEventsPath(spec.Home, spec.Session)), 0o700); err == nil {
		spool, err = os.OpenFile(PipeEventsPath(spec.Home, spec.Session), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	}
	if err == nil {
		errSpool, err = os.OpenFile(PipeErrorPath(spec.Home, spec.Session), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	}
	if err == nil {
		inR, in, err = os.Pipe()
	}
	if err == nil {
		out, outW, err = os.Pipe()
	}
	if err == nil {
		errOut, errW, err = os.Pipe()
	}
	if err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = startPriority(cmd, spec.Nice, cmd.Start)
	}
	if err != nil {
		closeAll()
		return nil, nil, nil, nil, nil, err
	}
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	return in, out, errOut, spool, errSpool, nil
}
