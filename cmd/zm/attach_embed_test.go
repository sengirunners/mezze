package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// fakeDaemon answers GET /v0/sessions/{id} and the attach upgrade on a Unix
// socket in a throwaway ZM_HOME, so `zm attach --embed` can be run whole.
type fakeAttachDaemon struct {
	mu       sync.Mutex
	state    orchapi.SessionState // what GET answers; "" = not found
	afterEnd orchapi.SessionState // what GET answers once the attach stream closed
	streamed bool
	ln       net.Listener
}

func startFakeAttachDaemon(t *testing.T) *fakeAttachDaemon {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_NO_AUTOSTART", "1")
	t.Setenv("ZM_HOST", "")
	ln, err := net.Listen("unix", filepath.Join(home, "zm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAttachDaemon{ln: ln}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeAttachDaemon) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	f.mu.Lock()
	st := f.state
	if f.streamed {
		st = f.afterEnd
	}
	f.mu.Unlock()
	if st == "" {
		api.WriteError(w, api.NotFound("session", "s"))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/attach") {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: zm-pty\r\n" +
			api.HeaderProtocol + ": " + api.Protocol + "\r\n\r\nreplayed output\r\n")
		_ = rw.Flush()
		time.Sleep(50 * time.Millisecond)
		f.mu.Lock()
		f.streamed = true
		f.mu.Unlock()
		conn.Close()
		return
	}
	ss := orchapi.Session{Address: "mini/abcd1234", ID: "abcd1234", Machine: "mini", Agent: "claude", State: st, Managed: true}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ss)
}

// runZm runs the CLI in-process and returns its exit code.
func runZm(args ...string) int {
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(context.Background())
	if err == nil {
		return 0
	}
	return report(err)
}

func TestAttachEmbedExitCodes(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	oldIn := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() { os.Stdin = oldIn })

	f := startFakeAttachDaemon(t)
	cases := []struct {
		name            string
		state, afterEnd orchapi.SessionState
		closeDaemon     bool
		args            []string
		want            int
	}{
		{"the session ends while attached", orchapi.StateRunning, orchapi.StateEnded, false, nil, 0},
		{"the stream drops, the session lives on", orchapi.StateRunning, orchapi.StateRunning, false, nil, 3},
		{"already ended", orchapi.StateEnded, orchapi.StateEnded, false, nil, 0},
		{"no such session", "", "", false, nil, 2},
		{"usage", orchapi.StateRunning, orchapi.StateRunning, false, []string{"attach", "--embed"}, 2},
		{"(last) the daemon goes away", orchapi.StateRunning, orchapi.StateRunning, true, nil, 3},
	}
	for _, c := range cases {
		f.mu.Lock()
		f.state, f.afterEnd, f.streamed = c.state, c.afterEnd, false
		f.mu.Unlock()
		if c.closeDaemon {
			go func() {
				for {
					f.mu.Lock()
					done := f.streamed
					f.mu.Unlock()
					if done {
						f.ln.Close()
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
		}
		args := c.args
		if args == nil {
			args = []string{"attach", "--embed", "abcd1234"}
		}
		if got := runZm(args...); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}
