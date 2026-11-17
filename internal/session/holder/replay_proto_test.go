package holder

import (
	"bytes"
	"encoding/json"
	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"testing"
	"time"
)

func replaySocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out [2]*net.UnixConn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "synthetic-replay-socket")
		c, e := net.FileConn(f)
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
		out[i] = c.(*net.UnixConn)
		t.Cleanup(func() { c.Close() })
		c.SetDeadline(time.Now().Add(3 * time.Second))
	}
	return out[0], out[1]
}
func TestReplayAdoptionFDCompatibilityAndHelloRace(t *testing.T) {
	for _, capability := range []int{0, 1} {
		// No PTY/provider process: /dev/null is an inert descriptor that only
		// exercises SCM_RIGHTS ordering. The stream is entirely invented.
		server, client := replaySocketPair(t)
		dummy, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer dummy.Close()
		r := NewRing(32)
		r.Write([]byte(modeSetup + "old"))
		helloStart, helloEnd := r.Bounds()
		r.Write(bytes.Repeat([]byte("synthetic-redraw "), 10))
		expected := r.NextReplay(helloStart, 32)
		if expected.From <= helloStart {
			t.Fatal("fixture did not advance after Hello")
		}
		h := &holder{pty: dummy, out: r}
		p := &peer{c: server, done: make(chan struct{})}
		defer p.close()
		done := make(chan bool, 1)
		go func() {
			typ, b, e := ReadFrame(server)
			var a Adopt
			if e != nil || typ != FrameAdopt || json.Unmarshal(b, &a) != nil {
				done <- false
				return
			}
			done <- h.adopt(p, a)
		}()
		c := &Conn{c: client, Hello: Hello{Start: helloStart, End: helloEnd, ReplayModes: capability}}
		fd, err := c.Adopt(helloStart)
		if err != nil {
			t.Fatalf("FD was not first, cap%d: %v", capability, err)
		}
		fd.Close()
		if !<-done {
			t.Fatal("adopt failed")
		}
		typ, b, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if capability == 1 {
			if typ != FrameReplay {
				t.Fatal("new/new missing checkpoint")
			}
			cp, e := ParseReplay(b)
			if e != nil || cp.From != expected.From || cp.State != expected.State {
				t.Fatal("metadata used stale Hello bounds")
			}
			typ, b, err = c.Next()
			if err != nil {
				t.Fatal(err)
			}
		}
		if typ != FrameOutput {
			t.Fatalf("legacy/new O ordering cap%d got%c", capability, typ)
		}
		off, data, err := ParseOutput(b)
		if err != nil || off != expected.From || !bytes.Equal(data, expected.Data) {
			t.Fatal("raw output changed")
		}
		h.mu.Lock()
		h.exit = &Exit{OutBytes: expected.To}
		h.mu.Unlock()
		r.Close()
		typ, _, err = c.Next()
		if err != nil || typ != FrameExit {
			t.Fatal("extra metadata/output or missing exit")
		}
	}
}
func TestReplayPumpMetadataOnlyFirstAndGap(t *testing.T) {
	server, client := replaySocketPair(t)
	r := NewRing(16)
	r.Write([]byte(modeSetup))
	h := &holder{out: r}
	p := &peer{c: server, done: make(chan struct{})}
	defer p.close()
	go h.pump(p, 0, true)
	readPair := func(wantMeta bool) {
		t.Helper()
		typ, b, err := ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		var cp ReplayCheckpoint
		if wantMeta {
			if typ != FrameReplay {
				t.Fatalf("expected metadata got%c", typ)
			}
			cp, err = ParseReplay(b)
			if err != nil {
				t.Fatal(err)
			}
			typ, b, err = ReadFrame(client)
			if err != nil {
				t.Fatal(err)
			}
		}
		if typ != FrameOutput {
			t.Fatalf("expected raw O got%c", typ)
		}
		from, data, err := ParseOutput(b)
		if err != nil {
			t.Fatal(err)
		}
		if wantMeta {
			want := r.NextReplay(from, len(data))
			if cp.From != from || cp.State != want.State {
				t.Fatal("M/O snapshot mismatch")
			}
		}
	}
	readPair(true)
	r.Write([]byte("x"))
	readPair(false) // contiguous despite a moving floor
	r.Write([]byte("\x1b[?47l" + string(bytes.Repeat([]byte("synthetic"), 10))))
	readPair(true)
	r.Close()
}
func TestReplayProtocolBoundedSchema(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(modeSetup + "\x1b[?47"))
	good, err := EncodeReplay(ReplayCheckpoint{From: 99, State: p})
	if err != nil || len(good) != ReplayFrameSize {
		t.Fatal("encoding")
	}
	cp, err := ParseReplay(good)
	if err != nil || cp.From != 99 || cp.State != p {
		t.Fatal("partial parser roundtrip")
	}
	for _, bad := range [][]byte{nil, good[:len(good)-1], append(append([]byte(nil), good...), 0), bytes.Repeat([]byte("x"), MaxFrame)} {
		if _, err := ParseReplay(bad); err == nil {
			t.Fatal("accepted malformed size")
		}
	}
	for _, at := range []int{0, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19} {
		bad := append([]byte(nil), good...)
		bad[at] = 255
		if _, err := ParseReplay(bad); err == nil {
			t.Fatalf("accepted invalid offset/schema byte%d", at)
		}
	}
	if _, err := EncodeReplay(ReplayCheckpoint{From: -1}); err == nil {
		t.Fatal("negative offset")
	}
	// Additional Hello/Adopt fields remain normal JSON for legacy decoders.
	b, _ := json.Marshal(Hello{Proto: 1, Minor: 1, ReplayModes: 1})
	var legacy struct {
		Proto int
		Minor int
	}
	if json.Unmarshal(b, &legacy) != nil || legacy.Proto != 1 {
		t.Fatal("legacy hello compatibility")
	}
}
