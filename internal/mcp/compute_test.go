package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// TestComputeToolsOptIn: the compute tools are listed only with
// Options.Compute (also beside SandboxOnly), answer the CLI's JSON types
// from the daemon's routes (orchclient's, as the CLI), and only read.
func TestComputeToolsOptIn(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/runners/doctor":
			_, _ = w.Write([]byte(`{"reports":[{"repo":"acme/worm","checked_at":"2026-11-12T00:00:00Z","checks":[{"number":9,"status":"fail","message":"Job native asks for macos-15","fix":"zm ci pr"}]}]}`))
		case "/v0/runners/setup":
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	list := func(o Options) map[string]bool {
		res, err := connect(t, client.WithHandler(h), o).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tl := range res.Tools {
			names[tl.Name] = true
		}
		return names
	}
	off, on, only := list(Options{}), list(Options{Compute: true}), list(Options{Compute: true, SandboxOnly: true})
	for _, tl := range ComputeTools() {
		if off[tl.Name] || !on[tl.Name] || !only[tl.Name] {
			t.Fatalf("%s: default=%v compute=%v sandbox-only=%v", tl.Name, off[tl.Name], on[tl.Name], only[tl.Name])
		}
	}

	cs := connect(t, client.WithHandler(h), Options{Compute: true})
	r, text := callTool(t, cs, "compute_doctor", map[string]any{"repo": "acme/worm"})
	if r.IsError || !strings.HasPrefix(text, UntrustedNote) {
		t.Fatalf("compute_doctor: %v %s", r.IsError, text)
	}
	var doc runners.DoctorResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(text, UntrustedNote))), &doc); err != nil {
		t.Fatalf("not the CLI's JSON: %v\n%s", err, text)
	}
	if len(doc.Reports) != 1 || !doc.Reports[0].Failed() || doc.Reports[0].Checks[0].Fix != "zm ci pr" {
		t.Fatalf("doctor result: %+v", doc)
	}
	callTool(t, cs, "compute_doctor", map[string]any{"repo": "a/b&x=1"}) // the daemon checks the repo, as for zm ci doctor
	if r, _ := callTool(t, cs, "compute_setup", map[string]any{"repos": []string{"acme/worm"}}); !r.IsError {
		t.Fatal("compute_setup without Options.Setup answered")
	}
	var got SetupRequest
	sc := connect(t, client.WithHandler(h), Options{Compute: true, Setup: func(_ context.Context, c *client.Client, in SetupRequest) (any, error) {
		if c.Autostart != nil {
			t.Error("compute_setup's client could start a daemon")
		}
		got = in
		return map[string]string{"state": "needs_human"}, nil
	}})
	if r, text := callTool(t, sc, "compute_setup", map[string]any{"repos": []string{"acme/worm"}, "job": "native", "own_account": true}); r.IsError || !strings.HasPrefix(text, UntrustedNote) || !strings.Contains(text, "needs_human") {
		t.Fatalf("compute_setup: %v %s", r.IsError, text)
	}
	if strings.Join(got.Repos, ",") != "acme/worm" || got.Job != "native" || !got.Own {
		t.Fatalf("compute_setup request: %+v", got)
	}
	if r, _ := callTool(t, cs, "compute_pause", nil); !r.IsError {
		t.Fatal("compute_pause without Options.Pause answered")
	}
	paused := 0
	pc := connect(t, client.WithHandler(h), Options{Compute: true, Pause: func(_ context.Context, c *client.Client) (any, error) {
		if c.Autostart != nil {
			t.Error("compute_pause's client could start a daemon")
		}
		paused++
		return map[string]any{"state": "refused", "refused": map[string]string{"reason": "job_running", "size": "c2m4"}}, nil
	}})
	if r, text := callTool(t, pc, "compute_pause", nil); r.IsError || paused != 1 || !strings.Contains(text, `"job_running"`) || !strings.Contains(text, `"state": "refused"`) {
		t.Fatalf("compute_pause: %v %d %s", r.IsError, paused, text)
	}
	// A public repository's yes is the owner's: the tool has no field for it,
	// and an agent that sends one is refused (or it is dropped), never applied.
	res, err := sc.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "compute_setup" {
			b, _ := json.Marshal(tl.InputSchema)
			if strings.Contains(string(b), `"public"`) {
				t.Fatalf("compute_setup takes public: %s", b)
			}
		}
		if tl.Name == "compute_doctor" {
			b, _ := json.Marshal(tl.InputSchema)
			if strings.Contains(string(b), `"public"`) || !strings.Contains(string(b), `"probe"`) {
				t.Fatalf("compute_doctor's probe input: %s", b)
			}
			// Its probe writes a temporary script folder: never said read-only.
			if a := tl.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
				t.Fatalf("compute_doctor's annotations: %+v", a)
			}
		}
	}
	// compute_doctor's probe is Options.Probe, with one repo; its flags alone are refused.
	if r, _ := callTool(t, cs, "compute_doctor", map[string]any{"repo": "acme/worm", "probe": true}); !r.IsError {
		t.Fatal("probe without Options.Probe answered")
	}
	var probed []string
	prc := connect(t, client.WithHandler(h), Options{Compute: true, Probe: func(_ context.Context, _ *client.Client, repo string, in ProbeRequest) (any, error) {
		probed = append(probed, repo+" "+in.Size)
		return map[string]any{"ask": map[string]string{"kind": "ask"}}, nil
	}})
	for _, bad := range []map[string]any{{"probe": true}, {"repo": "acme/worm", "size": "c4"}} {
		if r, _ := callTool(t, prc, "compute_doctor", bad); !r.IsError {
			t.Fatalf("%v answered", bad)
		}
	}
	if r, text := callTool(t, prc, "compute_doctor", map[string]any{"repo": "acme/worm", "probe": true, "size": "c4"}); r.IsError || !strings.Contains(text, `"ask"`) || strings.Join(probed, ",") != "acme/worm c4" {
		t.Fatalf("probe: %v %s %v", r.IsError, text, probed)
	}
	r, text = callTool(t, cs, "compute_status", nil)
	var st orchapi.CIStatus
	if r.IsError || json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(text, UntrustedNote))), &st) != nil || st.State == "" {
		t.Fatalf("compute_status: %v %s", r.IsError, text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "GET /v0/runners/doctor?repo=acme%2Fworm" {
		t.Fatalf("asked %v", asked)
	}
	if !slices.Contains(asked, "GET /v0/runners/doctor?repo=a%2Fb%26x%3D1") {
		t.Fatalf("repo not passed as one escaped value: %v", asked)
	}
	for _, a := range asked {
		if !strings.HasPrefix(a, "GET ") {
			t.Fatalf("compute tools must only read: %v", asked)
		}
	}
}
