package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// `zm sandbox shell` with piped input: the commands run
// on the box's terminal, the output comes back, and zm exits with the
// shell's code.
func TestSandboxShellPiped(t *testing.T) {
	c, home, _, _ := jobBox(t)
	h, err := computeclient.AcquireRunTarget(context.Background(), c, "box", computeapi.AcquireRunTargetRequest{RequestID: "sh1", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	cmd := zmCmd(home, t.TempDir(), "sandbox", "shell", "-P", "acme", h.Handle, "--", "sh")
	cmd.Stdin = strings.NewReader("echo hi-$((1+2)); test -t 0 && echo on-a-tty\nexit 3\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("exit: %v, want 3; stdout %q stderr %q", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "hi-3") || !strings.Contains(out.String(), "on-a-tty") {
		t.Errorf("stdout %q, want hi-3 and on-a-tty", out.String())
	}
}

// `zm sandbox shell` on a terminal: raw mode (keys go through one by one
// and the box echoes them), the window's size, a resize, a Ctrl-C to the
// box's foreground command, and the shell's exit code. It logs the
// keystroke echo latency it measured.
func TestSandboxShellTerminal(t *testing.T) {
	c, home, _, _ := jobBox(t)
	h, err := computeclient.AcquireRunTarget(context.Background(), c, "box", computeapi.AcquireRunTargetRequest{RequestID: "sh2", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	cmd := zmCmd(home, t.TempDir(), "sandbox", "shell", "-P", "acme", h.Handle, "--", "sh")
	cmd.Env = append(cmd.Env, "TERM=vt100")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		tty.Close()
	})
	var mu sync.Mutex
	var seen bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := tty.Read(b)
			mu.Lock()
			seen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	from := 0
	until := func(want string, within time.Duration) {
		t.Helper()
		for deadline := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			s := seen.String()
			mu.Unlock()
			if i := strings.Index(s[from:], want); i >= 0 {
				from += i + len(want)
				return
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("no %q in %q", want, s[from:])
			}
		}
	}
	write := func(s string) {
		t.Helper()
		if _, err := tty.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	write("echo \"t=$TERM\"; stty size\r")
	until("t=vt100", 20*time.Second)
	until("24 80", 10*time.Second)

	// Keystroke echo: each key is sent alone and the box's terminal echoes it.
	var lat []time.Duration
	for _, k := range "abcdefghijklmnopqrst" {
		start := time.Now()
		write(string(k))
		until(string(k), 10*time.Second)
		lat = append(lat, time.Since(start))
	}
	write("\x15") // Ctrl-U: the line typed so far is dropped
	slices.Sort(lat)
	t.Logf("keystroke echo over the local daemon: p50 %s, p90 %s, max %s", lat[len(lat)/2], lat[len(lat)*9/10], lat[len(lat)-1])

	if err := pty.Setsize(tty, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the SIGWINCH's resize goes ahead of what follows
	write("stty size\r")
	until("30 100", 10*time.Second)
	write("sleep 30 && echo slept\r")
	time.Sleep(500 * time.Millisecond)
	write("\x03")
	write("echo after-$((6*7))\r")
	until("after-42", 10*time.Second)
	if strings.Contains(seen.String(), "\nslept\r\n") { // the line typed echoes "slept\r\n" itself
		t.Errorf("Ctrl-C did not interrupt sleep: %q", seen.String())
	}
	write("exit 4\r")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 4 {
			t.Fatalf("exit: %v, want 4; terminal %q", err, seen.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("zm did not exit after the shell did: %q", seen.String())
	}
}

// The escape: Enter ~ . ends the shell from this side; ~~ sends one ~.
func TestShellEscape(t *testing.T) {
	for _, c := range []struct {
		in, out string
		quit    bool
	}{
		{"ab~.", "ab~.", false},
		{"~.", "", true},
		{"ls\r~.", "ls\r", true},
		{"\r~~x", "\r~x", false},
		{"\r~x", "\r~x", false},
	} {
		e := &shellEscape{start: true}
		var got []byte
		quit := false
		for i := range len(c.in) {
			b, q := e.feed([]byte{c.in[i]})
			got = append(got, b...)
			quit = quit || q
		}
		if string(got) != c.out || quit != c.quit {
			t.Errorf("%q: sent %q quit %v, want %q %v", c.in, got, quit, c.out, c.quit)
		}
	}
}

// shellHost stands in for the daemon behind runShell: input takes at most
// take bytes a call (0: all) or answers refuse; output answers the exit
// once every byte sent arrived.
type shellHost struct {
	mu       sync.Mutex
	got      []byte
	calls    int
	maxCall  int
	take     int
	refuse   error
	want     int
	killed   bool
	badCalls []string
}

func (s *shellHost) handler() http.Handler {
	mux := http.NewServeMux()
	base := "/v0/run_targets/box/handles/h1/"
	mux.HandleFunc("POST "+base+"input", func(w http.ResponseWriter, r *http.Request) {
		var req computeapi.InputRunTargetRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.refuse != nil {
			api.WriteError(w, s.refuse)
			return
		}
		b, err := base64.StdEncoding.DecodeString(req.Data)
		if req.Encoding != computeapi.EncodingBase64 || err != nil || len(b) > 64<<10 {
			s.badCalls = append(s.badCalls, fmt.Sprintf("encoding %q, %d bytes, %v", req.Encoding, len(b), err))
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "data is over 64 KiB"))
			return
		}
		n := len(b)
		if s.take > 0 {
			n = min(n, s.take)
		}
		s.got = append(s.got, b[:n]...)
		s.calls++
		s.maxCall = max(s.maxCall, len(b))
		api.WriteJSON(w, http.StatusOK, computeapi.RunTargetInput{Written: n, Running: true})
	})
	mux.HandleFunc("POST "+base+"output", func(w http.ResponseWriter, r *http.Request) {
		for range 100 {
			s.mu.Lock()
			done := len(s.got) >= s.want
			s.mu.Unlock()
			if done {
				code := 0
				api.WriteJSON(w, http.StatusOK, computeapi.RunTargetOutput{Exit: &code})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		api.WriteJSON(w, http.StatusOK, computeapi.RunTargetOutput{Running: true})
	})
	mux.HandleFunc("POST "+base+"signal", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.killed = true
		s.mu.Unlock()
		api.WriteJSON(w, http.StatusOK, computeapi.RunTargetSignal{Sent: true})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol) // as the daemon answers
		mux.ServeHTTP(w, r)
	})
}

// runShell's input: every byte arrives exactly (base64, so any byte), each
// call under 64 KiB, a partial write's rest sent again, and a refusal that
// would recur on every key ends the session instead of a dead keyboard.
func TestRunShellInput(t *testing.T) {
	in := make([]byte, 300<<10)
	for i := range in {
		in[i] = byte(i*7 + i/1000) // 0xff and other invalid UTF-8 among them
	}
	for _, c := range []struct {
		name string
		take int
	}{{"whole", 0}, {"partial writes", 1000}} {
		t.Run(c.name, func(t *testing.T) {
			h := &shellHost{take: c.take, want: len(in)}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			code, err := runShell(ctx, client.WithHandler(h.handler()), "box", "h1", "p1", shellIO{in: bytes.NewReader(in), out: io.Discard})
			if err != nil || code != 0 {
				t.Fatalf("runShell: %d %v (bad calls %v, calls %d, got %d)", code, err, h.badCalls, h.calls, len(h.got))
			}
			if !bytes.Equal(h.got, in) || h.maxCall > 64<<10 || len(h.badCalls) > 0 {
				t.Fatalf("got %d bytes (equal %v), largest call %d, bad calls %v", len(h.got), bytes.Equal(h.got, in), h.maxCall, h.badCalls)
			}
		})
	}
	for _, code := range []string{api.CodeNotImplemented, api.CodeBadRequest} {
		t.Run(code, func(t *testing.T) {
			h := &shellHost{refuse: api.Errorf(code, "zm-run-remote input: refused"), want: 1 << 30}
			done := make(chan error, 1)
			go func() {
				_, err := runShell(context.Background(), client.WithHandler(h.handler()), "box", "h1", "p1", shellIO{in: strings.NewReader("ls\r"), out: io.Discard})
				done <- err
			}()
			select {
			case err := <-done:
				h.mu.Lock()
				killed := h.killed
				h.mu.Unlock()
				if err == nil || !strings.Contains(err.Error(), "refused") || !killed {
					t.Fatalf("runShell: %v (killed %v), want the refusal and the shell killed", err, killed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a refused input left the session running")
			}
		})
	}
}
