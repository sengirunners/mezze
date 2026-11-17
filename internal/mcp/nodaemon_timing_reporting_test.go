package mcp

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completeNoDaemonTiming() []mcpTimingRow {
	rows := []mcpTimingRow{mcpNoDaemonRow("mcp/no-daemon/initialize-list", time.Second)}
	for _, tool := range tools {
		rows = append(rows, mcpNoDaemonRow("mcp/no-daemon/tool/"+tool.Name, time.Millisecond))
	}
	return rows
}

func TestMCPNoDaemonTimingCoverage(t *testing.T) {
	if row := mcpNoDaemonRow("boundary", noDaemonBudget); !row.OK {
		t.Fatal("the exact existing budget must pass")
	}
	if row := mcpNoDaemonRow("over", noDaemonBudget+time.Nanosecond); row.OK {
		t.Fatal("one nanosecond over the existing budget must fail")
	}
	for _, kind := range []string{"missing", "duplicate", "unknown", "budget", "verdict", "nonfinite"} {
		t.Run(kind, func(t *testing.T) {
			rows := completeNoDaemonTiming()
			switch kind {
			case "missing":
				rows = rows[:len(rows)-1]
			case "duplicate":
				rows = append(rows, rows[0])
			case "unknown":
				rows[0].Name = "unknown"
			case "budget":
				rows[0].Budget++
			case "verdict":
				rows[0].OK = true
			case "nonfinite":
				rows[0].P50, rows[0].P95, rows[0].Max, rows[0].First = math.Inf(1), math.Inf(1), math.Inf(1), math.Inf(1)
			}
			path := filepath.Join(t.TempDir(), "timing.json")
			if err := writeMCPNoDaemonTiming(path, rows); err == nil {
				t.Fatal("invalid report accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid report left output: %v", err)
			}
		})
	}
}

func TestMCPNoDaemonTimingReporting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timing.json")
	t.Run("ordinary functional mode", func(t *testing.T) {
		t.Setenv("ZM_MCP_SPEED", "")
		t.Setenv("ZM_MCP_JSON", path)
		r := newMCPNoDaemonTiming(t)
		r.record("mcp/no-daemon/initialize-list", time.Hour)
		if len(r.rows) != 0 {
			t.Fatal("ordinary mode recorded timing")
		}
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ordinary mode wrote a report: %v", err)
	}
	t.Run("advisory preserves every operation", func(t *testing.T) {
		t.Setenv("ZM_MCP_SPEED", "1")
		t.Setenv("ZM_MCP_JSON", path)
		r := newMCPNoDaemonTiming(t)
		r.record("mcp/no-daemon/initialize-list", time.Second)
		for _, tool := range tools {
			r.record("mcp/no-daemon/tool/"+tool.Name, time.Millisecond)
		}
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []mcpTimingRow
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) != len(tools)+1 || rows[0].OK || rows[0].P95 != 1000 || rows[0].Budget != 200 {
		t.Fatalf("report %s: %v", b, err)
	}
	if err := writeMCPNoDaemonTiming(filepath.Join(path, "impossible.json"), rows); err == nil {
		t.Fatal("writer error was hidden")
	}
}

// Exercise strict mode's actual test failure in the current test binary;
// the child records an injected duration and never starts a daemon.
func TestMCPNoDaemonTimingStrict(t *testing.T) {
	if os.Getenv("ZM_MCP_STRICT_PROBE") == "1" {
		newMCPNoDaemonTiming(t).record("mcp/no-daemon/initialize-list", time.Second)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMCPNoDaemonTimingStrict$")
	cmd.Env = append(os.Environ(), "ZM_MCP_STRICT_PROBE=1", "ZM_MCP_SPEED=1", "ZM_MCP_JSON=")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "mcp/no-daemon/initialize-list took 1000 ms (budget 200 ms)") {
		t.Fatalf("strict test error %v, context %v, output %s", err, ctx.Err(), out)
	}
}
