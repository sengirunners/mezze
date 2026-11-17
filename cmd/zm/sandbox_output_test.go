package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// `zm sandbox output` exits with the process's code, and a process that
// ended without one (its holder died) is an error, never a pass.
func TestSandboxOutputEnd(t *testing.T) {
	code := func(n int) *int { return &n }
	for _, c := range []struct {
		name     string
		o        computeapi.RunTargetOutput
		follow   bool
		done     bool
		exitCode int // -1: no error
	}{
		{"ended 0", computeapi.RunTargetOutput{Exit: code(0)}, true, true, -1},
		{"ended 7", computeapi.RunTargetOutput{Exit: code(7)}, true, true, 7},
		{"ended with no exit", computeapi.RunTargetOutput{}, true, true, 1},
		{"running, follow", computeapi.RunTargetOutput{Running: true}, true, false, -1},
		{"running, one page", computeapi.RunTargetOutput{Running: true}, false, true, -1},
	} {
		done, err := outputEnd(c.o, c.follow)
		var ew exitWith
		switch {
		case done != c.done:
			t.Errorf("%s: done %v", c.name, done)
		case c.exitCode < 0 && err != nil:
			t.Errorf("%s: %v, want no error", c.name, err)
		case c.exitCode >= 0 && (!errors.As(err, &ew) || ew.code != c.exitCode):
			t.Errorf("%s: %v, want exit %d", c.name, err, c.exitCode)
		}
	}
}

// `zm sandbox create --size` names zm's real sizes, not "small".
func TestSandboxCreateSizeHelp(t *testing.T) {
	cmd := sandboxCreateCmd()
	help := cmd.Use + " " + cmd.Flags().Lookup("size").Usage
	for _, s := range []string{"c1m2", "c2m4", "c2m8", "c4m16", "c8m32"} {
		if !strings.Contains(help, s) {
			t.Errorf("--size help lacks %s: %s", s, help)
		}
	}
	if strings.Contains(help, "small") {
		t.Errorf("--size help says small: %s", help)
	}
}

// `zm sandbox create --template agent` places on a box
// lending the agent image and asks acquire for the template; without it, the
// request names no template (the binding's first image).
func TestSandboxCreateTemplate(t *testing.T) {
	agent := "ghcr.io/sengirunners/potluck-box-agent@sha256:" + strings.Repeat("c", 64)
	base := "ghcr.io/sengirunners/potluck-box-linux@sha256:" + strings.Repeat("d", 64)
	var got []string
	hostClass := api.MachineTrusted
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch {
		case r.Method == "GET" && r.URL.Path == "/v0/run_targets":
			api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{
				{HostClass: hostClass, RunTarget: computeapi.RunTarget{Name: "plain", Kind: "box", Images: []string{base}}},
				{HostClass: hostClass, RunTarget: computeapi.RunTarget{Name: "agentbox", Kind: "box", Images: []string{base, agent}}}}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/acquire"):
			b, _ := io.ReadAll(r.Body)
			got = append(got, r.URL.Path+" "+string(b))
			api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:agentbox/b1", State: "ready"})
		default:
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		}
	})
	c := client.WithHandler(h)
	if _, _, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{Template: "agent"}, computeapi.AcquireRunTargetRequest{RequestID: "k1"}, pushFlags{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `/v0/run_targets/agentbox/acquire {"request_id":"k1","template":"agent"}` {
		t.Errorf("acquire: %q", got)
	}
	for _, class := range []string{api.MachinePublic, ""} {
		t.Run("refuse automatic placement class="+class, func(t *testing.T) {
			hostClass = class
			got = nil
			_, _, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{Template: "agent"}, computeapi.AcquireRunTargetRequest{RequestID: "refused"}, pushFlags{}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "only public hosts") {
				t.Fatalf("automatic placement: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("refused placement acquired a box: %q", got)
			}
		})
	}
	if f := sandboxCreateCmd().Flags().Lookup("template"); f == nil || !strings.Contains(f.Usage, "agent") {
		t.Errorf("--template flag: %+v", f)
	}
}
