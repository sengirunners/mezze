package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func runtimeClientEnv(t *testing.T, handler http.Handler) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	for _, key := range []string{"ZM_HOST", "ZM_AS", "ZM_SESSION", "ZM_TOKEN", "ZM_PROJECT", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		t.Setenv(key, "")
	}
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_MACHINE", "mini")
	t.Setenv("ZM_NO_AUTOSTART", "1")
	oldAs, oldProject, oldRunning := flagAs, flagProject, running
	flagAs, flagProject, running = "", "", nil
	t.Cleanup(func() { flagAs, flagProject, running = oldAs, oldProject, oldRunning })
	ln, err := net.Listen("unix", filepath.Join(home, "zm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func TestRuntimeClientCanonicalCollision(t *testing.T) {
	canonical := map[string]string{"aabbccdd-first": "mini/aabbccdd", "aabbccdd-second": "mini/12345678"}
	var calls atomic.Int32
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.URL.Path == "/v0/sessions/register" {
			calls.Add(1)
			var req orchapi.RegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if api.GetHeader(r.Header, api.HeaderAs) != "" || req.Agent != "codex" || req.Cwd == "" || canonical[req.RuntimeID] == "" {
				t.Errorf("registration guessed sender or lost runtime: headers=%v req=%+v", r.Header, req)
			}
			api.WriteJSON(w, 200, orchapi.Session{Address: canonical[req.RuntimeID]})
			return
		}
		api.WriteJSON(w, 200, map[string]string{"as": api.GetHeader(r.Header, api.HeaderAs)})
	}))
	for _, rid := range []string{"aabbccdd-first", "aabbccdd-second", "aabbccdd-first", "aabbccdd-second"} {
		t.Setenv("CODEX_THREAD_ID", rid)
		c, _, err := newClient()
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := c.Do(context.Background(), "GET", "/probe", nil, &got); err != nil {
			t.Fatal(err)
		}
		if got["as"] != canonical[rid] {
			t.Fatalf("%s addressed as %q, want %q", rid, got["as"], canonical[rid])
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("registrations = %d", calls.Load())
	}
}

func TestRuntimeClientRegistrationFailureRefusesClient(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "missing address"}[malformed], func(t *testing.T) {
			runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if malformed {
					api.WriteJSON(w, 200, orchapi.Session{})
				} else {
					api.WriteError(w, api.Errorf(api.CodeUnavailable, "registration refused"))
				}
			}))
			t.Setenv("CODEX_THREAD_ID", "aabbccdd-second")
			if c, _, err := newClient(); err == nil || c != nil {
				t.Fatalf("failed registration returned usable client: %v, %v", c, err)
			}
		})
	}
}

func TestRuntimeClientExplicitIdentityPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, flag, key, value, want string
	}{
		{"flag", "human:ana", "", "", "human:ana"},
		{"environment", "", "ZM_AS", "mini/11111111", "mini/11111111"},
		{"managed", "", "ZM_SESSION", "mini/22222222", "mini/22222222"},
		{"token", "", "ZM_TOKEN", "test-token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
			t.Setenv("CODEX_THREAD_ID", "aabbccdd-second")
			flagAs = tc.flag
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}
			c, _, err := newClient()
			if err != nil || c.As != tc.want || requests.Load() != 0 {
				t.Fatalf("client=%+v err=%v requests=%d", c, err, requests.Load())
			}
		})
	}
}

func TestRuntimeIdentityCommandSelection(t *testing.T) {
	for _, tc := range []struct {
		path []string
		want bool
	}{
		{[]string{"send"}, true}, {[]string{"notice"}, true},
		{[]string{"hook", "connect"}, false}, {[]string{"session", "register"}, false},
		{[]string{"session", "hook"}, false}, {[]string{"session", "state"}, false},
		{[]string{"ci", "status"}, false}, {[]string{"mcp"}, false}, {[]string{"daemon", "restart"}, false}, {[]string{"status"}, false},
	} {
		r := &cobra.Command{Use: "zm"}
		cmd := r
		for _, part := range tc.path {
			next := &cobra.Command{Use: part}
			cmd.AddCommand(next)
			cmd = next
		}
		if got := runtimeIdentityAllowed(cmd); got != tc.want {
			t.Errorf("%s: got %v, want %v", cmd.CommandPath(), got, tc.want)
		}
	}
}

func TestRuntimeClientRemoteDoesNotRegister(t *testing.T) {
	var requests atomic.Int32
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	t.Setenv("CODEX_THREAD_ID", "aabbccdd-second")
	t.Setenv("ZM_HOST", "http://127.0.0.1:1")
	c, _, err := newClient()
	if err != nil || c.Socket != "" || requests.Load() != 0 {
		t.Fatalf("remote client=%+v err=%v requests=%d", c, err, requests.Load())
	}
}
