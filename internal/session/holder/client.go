package holder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Conn is the daemon's end of a holder's socket.
type Conn struct {
	Hello Hello

	c   *net.UnixConn
	r   *bufio.Reader
	wmu sync.Mutex
}

// IncompatibleError is a holder that speaks another major version: it keeps
// running, but this daemon cannot adopt it.
type IncompatibleError struct{ Hello Hello }

func (e *IncompatibleError) Error() string {
	major, _ := Proto()
	return fmt.Sprintf("its holder speaks %s (zm %s); this daemon speaks %s", ProtoName(e.Hello.Proto),
		e.Hello.Version, ProtoName(major))
}

// ErrNoHolder: nothing listens on the session's socket.
var ErrNoHolder = errors.New("no holder")

// Dial connects to a session's holder and checks who answers: the socket
// and its directory belong to this user with no group or other access, the
// peer runs as this user, and its hello names this home, this session and
// its own pid. An *IncompatibleError comes with the open connection's hello
// (the connection is closed).
func Dial(home, session string) (*Conn, error) {
	if err := private(Dir(home), true); err != nil {
		return nil, err
	}
	path := SocketPath(home, session)
	if err := private(path, false); err != nil {
		return nil, err
	}
	nc, err := (&net.Dialer{Timeout: 2 * time.Second}).Dial("unix", path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %v", ErrNoHolder, err)
		}
		return nil, err
	}
	c := nc.(*net.UnixConn)
	uid, pid, err := peerCred(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if uid != os.Getuid() {
		_ = c.Close()
		return nil, fmt.Errorf("holder socket %s answered as uid %d", path, uid)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, b, err := ReadFrame(c)
	if err == nil && typ != FrameHello {
		err = ErrFrame
	}
	var h Hello
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("holder hello: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if major, _ := Proto(); h.Proto != major {
		_ = c.Close()
		return nil, &IncompatibleError{Hello: h}
	}
	if h.Session != session || filepath.Clean(h.Home) != filepath.Clean(home) || h.HolderPID != pid {
		_ = c.Close()
		return nil, fmt.Errorf("holder socket %s answered for session %s in %s, pid %d (peer %d)", path, h.Session, h.Home, h.HolderPID, pid)
	}
	return &Conn{Hello: h, c: c}, nil
}

// private checks path is a directory (dir) or a socket owned by this user
// with no group or other permission bits, and not a symlink.
func private(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s is missing", ErrNoHolder, path)
		}
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case dir && !fi.IsDir(), !dir && fi.Mode().Type() != os.ModeSocket:
		return fmt.Errorf("%s is not a holder %s", path, map[bool]string{true: "directory", false: "socket"}[dir])
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s is not owned by this user", path)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is open to other users (mode %o)", path, fi.Mode().Perm())
	}
	return nil
}

// Adopt asks for the PTY master and the output from since. The returned
// file is the daemon's own descriptor for it (input, resize, tcgetpgrp).
func (c *Conn) Adopt(since int64) (*os.File, error) {
	a := Adopt{Since: since}
	if c.Hello.ReplayModes == 1 {
		a.ReplayModes = 1
	}
	if err := c.send(FrameAdopt, a); err != nil {
		return nil, err
	}
	_ = c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.c.SetReadDeadline(time.Time{}) }()
	hdr := make([]byte, headerLen)
	oob := make([]byte, unix.CmsgSpace(4))
	// Hold off forks until the received descriptor is close-on-exec, so no
	// child this daemon starts meanwhile inherits the PTY master.
	syscall.ForkLock.RLock()
	n, oobn, _, _, err := c.c.ReadMsgUnix(hdr, oob)
	fds := receivedFDs(oob[:oobn])
	for _, f := range fds {
		unix.CloseOnExec(f)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		for _, f := range fds {
			_ = unix.Close(f)
		}
		return nil, err
	}
	fd := -1
	for _, f := range fds {
		if fd < 0 {
			fd = f
		} else {
			_ = unix.Close(f)
		}
	}
	if fd >= 0 && n < headerLen {
		_, err = io.ReadFull(c.c, hdr[n:])
	}
	if err != nil || hdr[0] != FrameFD || fd < 0 {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, fmt.Errorf("holder: no terminal in its answer to adopt")
	}
	c.r = bufio.NewReaderSize(c.c, 64<<10)
	return os.NewFile(uintptr(fd), "pty"), nil
}

// receivedFDs are the descriptors in a control message (SCM_RIGHTS).
func receivedFDs(oob []byte) []int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var out []int
	for i := range msgs {
		if fds, err := unix.ParseUnixRights(&msgs[i]); err == nil {
			out = append(out, fds...)
		}
	}
	return out
}

// Next reads the next frame after Adopt.
func (c *Conn) Next() (byte, []byte, error) { return ReadFrame(c.r) }

// Signal asks the holder to signal the child's process group.
func (c *Conn) Signal(sig, by string) error { return c.send(FrameSignal, Signal{Sig: sig, By: by}) }

// Ack tells the holder its exit is recorded.
func (c *Conn) Ack() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.c, FrameAck, nil)
}

// Close hangs up; the holder keeps running.
func (c *Conn) Close() error { return c.c.Close() }

func (c *Conn) send(typ byte, v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return WriteJSON(c.c, typ, v)
}

// StartRefused means the holder confirmed that the child did not start.
type StartRefused struct{ Message string }

func (e *StartRefused) Error() string { return e.Message }

// Started is a holder this daemon started.
type Started struct {
	Cmd      *exec.Cmd // reap it with Wait
	ChildPID int
	GroupPID int

	LaunchGrant io.WriteCloser // one-shot release after durable group recording; nil for ordinary launches
	// Tie is the holder's stdin while tied (nil otherwise): closing it ends
	// the session.
	Tie io.WriteCloser
}

// NeverStartedError means the holder confirmed it did not start its child.
// Transport failures and timeouts deliberately do not carry this guarantee.
type NeverStartedError struct{ Err error }

func (e *NeverStartedError) Error() string { return e.Err.Error() }
func (e *NeverStartedError) Unwrap() error { return e.Err }

// Start runs exe as a new holder for spec in its own session and waits
// until its child runs (or it says why not).
func Start(exe string, spec Spec) (*Started, error) {
	if err := os.MkdirAll(Dir(spec.Home), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(Dir(spec.Home), 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(LogPath(spec.Home, spec.Session), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, Arg, spec.Home, spec.Session)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var grantRead, grantWrite *os.File
	handedOff := false
	if spec.RecordedLaunch {
		if !spec.Task {
			return nil, errors.New("recorded launch requires a task")
		}
		grantRead, grantWrite, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.ExtraFiles = []*os.File{grantRead}
		defer func() {
			_ = grantRead.Close()
			if !handedOff {
				_ = grantWrite.Close()
			}
		}()
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &NeverStartedError{Err: err}
	}
	fail := func(err error) (*Started, error) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	b, _ := json.Marshal(spec)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return fail(fmt.Errorf("holder spec: %w", err))
	}
	type answer struct {
		r   Ready
		err error
	}
	got := make(chan answer, 1)
	go func() {
		var r Ready
		err := json.NewDecoder(stdout).Decode(&r)
		got <- answer{r, err}
	}()
	var a answer
	select {
	case a = <-got:
	case <-time.After(10 * time.Second):
		return fail(fmt.Errorf("holder did not start within 10s"))
	}
	if a.err != nil {
		return fail(fmt.Errorf("holder: %w", a.err))
	}
	if a.r.Error != "" {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, &NeverStartedError{Err: &StartRefused{Message: a.r.Error}}
	}
	st := &Started{Cmd: cmd, ChildPID: a.r.ChildPID, GroupPID: a.r.GroupPID}
	if grantWrite != nil {
		st.LaunchGrant = grantWrite
		handedOff = true
	}
	if spec.Tied {
		st.Tie = stdin
	} else {
		_ = stdin.Close()
	}
	return st, nil
}

// ReadExit reads a session's exit file.
func ReadExit(home, session string) (Exit, bool) {
	b, err := os.ReadFile(ExitPath(home, session))
	if err != nil {
		return Exit{}, false
	}
	var x Exit
	if json.Unmarshal(b, &x) != nil {
		return Exit{}, false
	}
	return x, true
}

// Forget removes a session's holder files (socket, exit file, log) once its
// end is recorded.
func Forget(home, session string) {
	for _, p := range []string{SocketPath(home, session), ExitPath(home, session), LogPath(home, session), DraftPath(home, session)} {
		_ = os.Remove(p)
	}
}
