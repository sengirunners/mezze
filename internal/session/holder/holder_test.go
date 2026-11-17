package holder

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func tempHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// start runs a holder for script under sh; cleanup signals only the same
// recorded child, holder and group-leader identities.
func start(t *testing.T, home, session, script string, tied bool) *Started {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Start(exe, Spec{Session: session, Home: home, Path: "/bin/sh", Args: []string{"sh", "-c", script},
		Dir: home, Env: []string{"PATH=/usr/bin:/bin"}, Rows: 24, Cols: 80, Tied: tied})
	if err != nil {
		t.Fatal(err)
	}
	owned := []cleanupPID{recordCleanupPID(t, st.ChildPID), recordCleanupPID(t, st.Cmd.Process.Pid)}
	if st.GroupPID > 1 && st.GroupPID != st.ChildPID && st.GroupPID != st.Cmd.Process.Pid {
		owned = append(owned, recordCleanupPID(t, st.GroupPID))
	}
	done := make(chan struct{})
	go func() { _ = st.Cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		for _, p := range owned {
			signalHolderCleanupPID(p, cleanupProcessInfo, syscall.Kill)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("holder cleanup could not safely stop and reap its process")
		}
	})
	return st
}

// until reads frames until an O frame's bytes contain want or an X frame.
func until(t *testing.T, c *Conn, want string) (string, *Exit) {
	t.Helper()
	var out bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	_ = c.c.SetReadDeadline(deadline)
	for {
		typ, b, err := c.Next()
		if err != nil {
			t.Fatalf("reading for %q: %v (have %q)", want, err, out.String())
		}
		switch typ {
		case FrameOutput:
			_, data, err := ParseOutput(b)
			if err != nil {
				t.Fatal(err)
			}
			out.Write(data)
			if want != "" && strings.Contains(out.String(), want) {
				return out.String(), nil
			}
		case FrameExit:
			var x Exit
			if err := json.Unmarshal(b, &x); err != nil {
				t.Fatal(err)
			}
			return out.String(), &x
		}
	}
}

func TestHolderProtocol(t *testing.T) {
	home := tempHome(t)
	st := start(t, home, "abcd1234", `echo hello-holder; exec cat`, false)
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	major, _ := Proto()
	if h := c.Hello; h.Proto != major || h.Session != "abcd1234" || h.Home != home || h.ChildPID != st.ChildPID ||
		h.HolderPID != st.Cmd.Process.Pid || h.Exited {
		t.Fatalf("hello: %+v", h)
	}
	// A frame from a newer daemon is ignored.
	if err := c.send('Z', map[string]string{"new": "thing"}); err != nil {
		t.Fatal(err)
	}
	f, err := c.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	until(t, c, "hello-holder")
	if _, err := f.Write([]byte("typed-through-fd\n")); err != nil {
		t.Fatal(err)
	}
	until(t, c, "typed-through-fd")
	if err := c.Signal("TERM", "human:me"); err != nil {
		t.Fatal(err)
	}
	_, x := until(t, c, "")
	if x == nil || x.Signal != "TERM" || x.StopSig != "TERM" || x.StopBy != "human:me" || x.OutBytes == 0 {
		t.Fatalf("exit: %+v", x)
	}
	if fx, ok := ReadExit(home, "abcd1234"); !ok || fx.Signal != "TERM" {
		t.Fatalf("exit file: %+v %v", fx, ok)
	}
	if b, err := os.ReadFile(ScrollbackPath(home, "abcd1234")); err != nil || !bytes.Contains(b, []byte("hello-holder")) {
		t.Fatalf("scrollback file: %q %v", b, err)
	}
	if err := c.Ack(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, st)
	if _, err := os.Stat(SocketPath(home, "abcd1234")); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func waitExit(t *testing.T, st *Started) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for st.Cmd.ProcessState == nil && syscall.Kill(st.Cmd.Process.Pid, 0) == nil {
		var ws syscall.WaitStatus
		if pid, _ := syscall.Wait4(st.Cmd.Process.Pid, &ws, syscall.WNOHANG, nil); pid > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("holder still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDialChecksOwnership: a socket or directory other users could open is
// never trusted.
func TestDialChecksOwnership(t *testing.T) {
	home := tempHome(t)
	start(t, home, "abcd1234", `exec sleep 30`, false)
	sock := SocketPath(home, "abcd1234")
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(home, "abcd1234"); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Fatalf("open socket: %v", err)
	}
	_ = os.Chmod(sock, 0o600)
	if err := os.Chmod(Dir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(home, "abcd1234"); err == nil {
		t.Fatal("an open holders directory was trusted")
	}
	_ = os.Chmod(Dir(home), 0o700)
	if _, err := Dial(home, "ffff0000"); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("no holder: %v", err)
	}
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

// TestTiedHolderEndsWithItsDaemon: a tied holder ends its child when its
// stdin closes (the daemon that started it is gone).
func TestTiedHolderEndsWithItsDaemon(t *testing.T) {
	home := tempHome(t)
	st := start(t, home, "abcd1234", `exec sleep 30`, true)
	if st.Tie == nil {
		t.Fatal("a tied holder has no tie")
	}
	_ = st.Tie.Close()
	waitExit(t, st)
	x, ok := ReadExit(home, "abcd1234")
	if !ok || x.Signal != "TERM" || x.StopSig != "TERM" || x.StopBy != "" {
		t.Fatalf("tied exit: %+v %v", x, ok)
	}
}

// TestChildSeesHupAndInt: the holder catches HUP and INT; it must not pass
// them on ignored (an ignored signal stays ignored across exec).
func TestChildSeesHupAndInt(t *testing.T) {
	home := tempHome(t)
	st := start(t, home, "abcd1234", `exec sleep 30`, false)
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := c.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = syscall.Kill(st.ChildPID, syscall.SIGINT)
	_, x := until(t, c, "")
	if x == nil || x.Signal != "INT" {
		t.Fatalf("exit after INT: %+v", x)
	}
}

// TestAdoptAfterTheRingMoved: more than the whole ring is printed between
// the hello and the adoption; the stream starts where the ring now starts,
// and a mirror built from it holds no byte it never got.
func TestAdoptAfterTheRingMoved(t *testing.T) {
	home := tempHome(t)
	start(t, home, "abcd1234", `read go; head -c 3000000 /dev/zero | tr '\0' a; echo; echo END-OF-FLOOD; exec cat`, false)
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello := c.Hello
	// The flood must happen after this hello and before this connection
	// adopts: a second connection starts it and hangs up.
	c2, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := c2.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	until(t, c2, "END-OF-FLOOD")
	_ = f2.Close()
	_ = c2.Close()

	f, err := c.Adopt(hello.Start) // stale: the ring has moved on by 2 MB
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mirror := NewRing(1 << 20)
	mirror.WriteAt(hello.Start, nil)
	first := int64(-1)
	_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, b, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if typ != FrameOutput {
			continue
		}
		off, data, _ := ParseOutput(b)
		mirror.WriteAt(off, data)
		if first < 0 { // what an attach right after the adoption would replay
			first = off
			if held, from, _, _, _ := mirror.Next(0, 2<<20); from != off || bytes.IndexByte(held, 0) >= 0 {
				t.Fatalf("mirror after the first frame: from %d (frame at %d), NUL at %d", from, off, bytes.IndexByte(held, 0))
			}
		}
		if bytes.Contains(data, []byte("END-OF-FLOOD")) {
			break
		}
	}
	if first < 1<<20 {
		t.Fatalf("the stream started at %d: the ring did not move past the hello", first)
	}
	held, from, _, _, _ := mirror.Next(0, 2<<20)
	if from != first || bytes.IndexByte(held, 0) >= 0 || !bytes.HasSuffix(bytes.TrimRight(held, "\r\n"), []byte("END-OF-FLOOD")) {
		t.Fatalf("mirror from %d (stream from %d), %d bytes, NUL at %d", from, first, len(held), bytes.IndexByte(held, 0))
	}
}

// TestChildGetsOnlyTheSpecEnv: the agent's environment is the Spec's alone
// (the session module builds it through envMap, which strips agent markers);
// nothing of the starting process's environment passes through the holder.
func TestChildGetsOnlyTheSpecEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("HOLDER_LEAK", "leaked")
	home := tempHome(t)
	start(t, home, "abcd1234", `echo "env=[$CLAUDECODE$HOLDER_LEAK]"; exec cat`, false)
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := c.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	until(t, c, "env=[]")
}
