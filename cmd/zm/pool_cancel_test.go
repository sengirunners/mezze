package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// fakeTailnet answers the tailnet routes with a node that never finishes its
// login, says no pool exists yet, and records every other request (a project
// write would show here).
type fakeTailnet struct {
	mu      sync.Mutex
	enabled bool // the node is on
	authURL bool // it has a login URL yet
	running bool
	downs   int
	other   []string
}

func (f *fakeTailnet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set(api.HeaderProtocol, api.WireProtocol)
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /v0/tailnet":
	case "POST /v0/tailnet/up":
		f.enabled, f.authURL = true, true
	case "POST /v0/tailnet/down":
		f.enabled, f.authURL, f.running = false, false, false
		f.downs++
	case "GET /v0/projects/friends":
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: &api.Error{Code: api.CodeNotFound, Message: "no project friends"}})
		return
	default:
		f.other = append(f.other, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: &api.Error{Code: api.CodeInternal, Message: "unexpected"}})
		return
	}
	st := api.TailnetStatus{Enabled: f.enabled, Running: f.running}
	if f.enabled && f.authURL && !f.running {
		st.AuthURL = "https://login.example/a/1"
	}
	_ = json.NewEncoder(w).Encode(st)
}

// TestPoolCreateCancelledLogin: ctrl-C while zm pool create waits for the
// Tailscale login creates no project, stops the node only when this run
// turned it on, and says nothing was created. A node already on (waiting
// for its login, or still starting) is left as it was.
func TestPoolCreateCancelledLogin(t *testing.T) {
	for _, tc := range []struct {
		name             string
		enabled, authURL bool
		downs            int
	}{
		{"off", false, false, 1},
		{"waiting", true, true, 0},
		{"starting", true, false, 0},
	} {
		f := &fakeTailnet{enabled: tc.enabled, authURL: tc.authURL}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		var out bytes.Buffer
		err := poolCreate(ctx, client.WithHandler(f), "friends", poolInvites{Renters: []string{"ana"}}, &out)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "nothing was created") || strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("%s: err %v, want 'nothing was created'", tc.name, err)
		}
		if len(f.other) > 0 {
			t.Errorf("%s: requests past the login: %v", tc.name, f.other)
		}
		if f.downs != tc.downs {
			t.Errorf("%s: %d tailnet downs, want %d (err: %v)", tc.name, f.downs, tc.downs, err)
		}
		if s := out.String(); strings.Count(s, "https://login.example/a/1") != 1 || !strings.Contains(s, "Tailscale account") {
			t.Errorf("%s: output:\n%s", tc.name, s)
		}
	}
	// A node already logged in: no wait, no output, nothing stopped.
	f := &fakeTailnet{enabled: true, running: true}
	var out bytes.Buffer
	if err := ensureTailnet(context.Background(), client.WithHandler(f), &out); err != nil || out.Len() > 0 || f.downs > 0 {
		t.Errorf("running node: err %v, output %q, downs %d", err, out.String(), f.downs)
	}
}
