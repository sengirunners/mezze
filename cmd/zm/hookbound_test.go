package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// TestPromptPathWedgedDaemon: a daemon whose socket accepts but never answers
// held every prompt hook and `zm notice` for the full 2 s. The prompt path
// must give up within its bound (plus process start), print nothing and exit 0,
// also when stdin stays open and silent.
func TestPromptPathWedgedDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	ln, err := net.Listen("unix", filepath.Join(home, "zm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() { // accept, read nothing, answer nothing
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	defer func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}()

	payload := `{"session_id":"wedged-test","cwd":"` + home + `"}`
	cases := []struct {
		name  string
		args  []string
		stdin bool // an open pipe nobody writes to
	}{
		{"notice", []string{"notice"}, false},
		{"hook prompt (arg)", []string{"hook", "claude", "prompt", payload}, false},
		{"hook stop (arg)", []string{"hook", "claude", "stop", payload}, false},
		{"hook prompt (silent stdin)", []string{"hook", "claude", "prompt"}, true},
	}
	const limit = time.Second // promptBound plus process start on a loaded Mac; was 2 s
	for _, tc := range cases {
		c := exec.Command(os.Args[0], tc.args...)
		c.Env = append(os.Environ(), "ZM_TEST_MAIN=1", "ZM_HOME="+home, "ZM_HOST=", "ZM_AS=", "ZM_SESSION=",
			"CLAUDE_CODE_SESSION_ID=wedged-test")
		var w *os.File
		if tc.stdin {
			r, wp, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			c.Stdin, w = r, wp
			defer r.Close()
		}
		start := time.Now()
		out, err := c.Output()
		d := time.Since(start)
		if w != nil {
			w.Close()
		}
		if err != nil {
			t.Errorf("%s: exit %v", tc.name, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: printed %q", tc.name, out)
		}
		if d > limit {
			t.Errorf("%s against a wedged daemon took %s (bound %s)", tc.name, d, limit)
		}
	}
}

// TestHookBounds: lifecycle hooks keep the longer bound (the register must
// land); everything on the prompt path is short.
func TestHookBounds(t *testing.T) {
	for _, ev := range integrations.HookEvents() {
		want := promptBound
		if ev == integrations.HookSessionStart || ev == integrations.HookSessionEnd {
			want = lifecycleBound
		}
		if got := hookBound(ev); got != want {
			t.Errorf("hookBound(%q) = %s, want %s", ev, got, want)
		}
	}
	if promptBound > 500*time.Millisecond {
		t.Errorf("promptBound %s: the prompt path must fail open within a few hundred ms", promptBound)
	}
}

// A hook payload past the cap is read to the end (the rest discarded), so
// the agent writing it never gets EPIPE.
func TestReadCappedDrains(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	big := make([]byte, 3<<20)
	werr := make(chan error, 1)
	go func() { _, err := w.Write(big); w.Close(); werr <- err }()
	got := readCapped(r)
	if len(got) != api.MaxBody {
		t.Fatalf("kept %d bytes, want %d", len(got), api.MaxBody)
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer blocked: the rest was not drained")
	}
}

// Runtime registration and notice share one prompt budget, including when
// registration succeeds just before the deadline and notice never answers.
func TestNoticeRegistrationSharesPromptDeadline(t *testing.T) {
	var noticeCalls atomic.Int32
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.URL.Path == "/v0/sessions/register" {
			select {
			case <-time.After(promptBound * 3 / 4):
				api.WriteJSON(w, 200, orchapi.Session{Address: "mini/12345678"})
			case <-r.Context().Done():
			}
			return
		}
		noticeCalls.Add(1)
		<-r.Context().Done()
	}))
	t.Setenv("CODEX_THREAD_ID", "aabbccdd-delayed")
	cmd := noticeCmd()
	root := &cobra.Command{Use: "zm"}
	root.AddCommand(cmd)
	cmd.SetContext(context.Background())
	running = cmd
	start := time.Now()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if noticeCalls.Load() != 1 {
		t.Fatalf("notice calls = %d; registration did not finish within its budget", noticeCalls.Load())
	}
	if elapsed > promptBound+150*time.Millisecond {
		t.Fatalf("registration and notice took %s; they must share the %s prompt budget", elapsed, promptBound)
	}
}
