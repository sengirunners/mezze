package gate_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink/gate"
	"github.com/sengirunners/mezze/internal/ghlink/gate/gatetest"
)

func TestMain(m *testing.M) {
	if code, ok := gatetest.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func fake(t *testing.T, cfg gatetest.Config) (*gate.Process, string) {
	t.Helper()
	dir := t.TempDir()
	p, err := gatetest.Install(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &gate.Process{Path: p, Env: []string{"PATH=" + os.Getenv("PATH")}}, dir
}

// The process driver passes the conformance checks, with and without match.
func TestProcessConforms(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []gatetest.Config{{}, {Match: []string{"conf-abc12"}}} {
		g, _ := fake(t, cfg)
		for _, c := range gate.Conform(ctx, g) {
			if !c.OK {
				t.Errorf("match=%v: %s: %s", cfg.Match != nil, c.Name, c.Detail)
			}
		}
	}
}

// Conformance catches a gate that breaks the protocol.
func TestConformCatchesBadGates(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]gatetest.Config{
		"describe fails":   {FailDescribe: true},
		"wrong protocol":   {Describe: map[string]any{"zm": "zm/v9", "name": "x", "verbs": []string{"describe", "decide"}}},
		"no decide verb":   {Describe: map[string]any{"zm": "zm/v0", "name": "x", "verbs": []string{"describe"}}},
		"unknown decision": {Decide: map[string]map[string]any{"merged": {"decision": "close-it"}}},
		"decide fails":     {FailDecide: true},
		"garbage":          {Garbage: true},
		"bad match ids":    {Match: []string{"harbor abc; rm -rf"}},
	} {
		g, _ := fake(t, cfg)
		failed := false
		for _, c := range gate.Conform(ctx, g) {
			failed = failed || !c.OK
		}
		if !failed {
			t.Errorf("%s: conformance passed", name)
		}
	}
}

func TestBoundsAndCleaning(t *testing.T) {
	d := gate.Description{TimeoutSeconds: 9999, RecheckSeconds: 1, RetentionSeconds: 400 * 86400}
	if d.Timeout() != gate.MaxTimeout || d.Recheck() != gate.MinRecheck || d.Retention() != gate.MaxRetention {
		t.Fatalf("bounds: %s %s %s", d.Timeout(), d.Recheck(), d.Retention())
	}
	if (gate.Description{}).Retention() != gate.DefaultRetention {
		t.Fatal("default retention")
	}
	n := gate.CleanNote("ok\x1b[31m\x00 " + strings.Repeat("é", 3000))
	if len(n) > gate.MaxNote || strings.ContainsAny(n, "\x1b\x00") || !strings.HasPrefix(n, "ok") {
		t.Fatalf("note not cleaned: %q", n[:20])
	}
	if _, err := gate.CheckDecision(gate.Decision{Decision: "close"}); err == nil {
		t.Fatal("unknown decision accepted")
	}
}

// A gate that does not answer in time is killed (with its children) and
// reported, never taken as an answer.
func TestTimeoutKillsGate(t *testing.T) {
	// describe answers at once and is cached; then decide is made slow.
	g2, dir := fake(t, gatetest.Config{Describe: map[string]any{"zm": "zm/v0", "name": "slow", "verbs": []string{"describe", "decide"}, "timeout_seconds": 1}})
	ctx := context.Background()
	if _, err := g2.Describe(ctx); err != nil {
		t.Fatal(err)
	}
	// Now make decide slow without touching the gate file (describe stays cached).
	if err := gatetest.Set(dir, gatetest.Config{SleepSeconds: 30, Describe: map[string]any{"zm": "zm/v0", "name": "slow", "verbs": []string{"describe", "decide"}, "timeout_seconds": 1}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := g2.Decide(ctx, gate.Request{PR: gate.PR{State: "merged"}})
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("slow decide: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}
