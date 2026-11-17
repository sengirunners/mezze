package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/potluck/cli"
)

// TestComputeSetupIsCIAddJSON: MCP compute_setup answers exactly what
// zm ci add --json prints, from the same broker state, on the tool's own
// client (which never starts a daemon).
func TestComputeSetupIsCIAddJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeBroker)
		req   mcp.SetupRequest
		args  []string
	}{
		{"app to make", func(b *fakeBroker) {}, mcp.SetupRequest{Repos: []string{"judy/app"}}, []string{"judy/app"}},
		{"public yes asked", func(b *fakeBroker) { b.app, b.linked["judy"], b.public["judy/app"] = true, true, true },
			mcp.SetupRequest{Repos: []string{"judy/app"}}, []string{"judy/app"}},
		{"connected", func(b *fakeBroker) { b.app, b.linked["judy"] = true, true }, mcp.SetupRequest{Repos: []string{"judy/app"}}, []string{"judy/app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cliOut, mcpOut string
			t.Run("cli", func(t *testing.T) {
				b := newFakeBroker(t)
				tc.setup(b)
				cliOut, _, _ = ciAddRun(t, true, tc.args...)
			})
			t.Run("mcp", func(t *testing.T) {
				b := newFakeBroker(t)
				tc.setup(b)
				t.Setenv(cli.EnvNoAutostart, "")
				c, _, err := newClientContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				c.Autostart = nil // as mcp.NewServer hands its tools
				v, err := computeSetup(context.Background(), c, tc.req)
				if err != nil {
					t.Fatal(err)
				}
				b2, _ := json.MarshalIndent(v, "", "  ")
				mcpOut = string(b2)
			})
			var a, m any
			if json.Unmarshal([]byte(cliOut), &a) != nil || json.Unmarshal([]byte(mcpOut), &m) != nil {
				t.Fatalf("not JSON:\ncli %s\nmcp %s", cliOut, mcpOut)
			}
			ab, _ := json.Marshal(a)
			mb, _ := json.Marshal(m)
			if string(ab) != string(mb) {
				t.Fatalf("compute_setup differs from zm ci add --json:\ncli %s\nmcp %s", ab, mb)
			}
			if strings.Contains(tc.name, "public") && !strings.Contains(string(mb), `"kind":"ask"`) {
				t.Fatalf("a public repository's yes must come back as an ask: %s", mb)
			}
		})
	}
}

// TestComputeSetupNeverSaysPublicYes: a public repository connected through
// compute_setup is never sent with public: true; the owner's yes comes back
// as an ask, however often the agent calls.
func TestComputeSetupNeverSaysPublicYes(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["judy"], b.public["judy/app"] = true, true, true
	t.Setenv(cli.EnvNoAutostart, "")
	c, _, err := newClientContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Autostart = nil
	var raw mcp.SetupRequest
	if err := json.Unmarshal([]byte(`{"repos":["judy/app"],"public":true}`), &raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		v, err := computeSetup(context.Background(), c, raw)
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := json.Marshal(v); !strings.Contains(string(out), `"kind":"ask"`) || !strings.Contains(string(out), "--public") {
			t.Fatalf("the public yes must come back as an ask: %s", out)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.connects {
		if req.Public {
			t.Fatalf("compute_setup said the owner's public yes: %+v", b.connects)
		}
	}
	if len(b.repos) != 0 {
		t.Fatalf("a public repository was connected without its owner: %+v", b.repos)
	}
}
