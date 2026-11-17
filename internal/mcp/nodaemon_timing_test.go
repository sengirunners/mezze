package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const noDaemonBudget = 200 * time.Millisecond

// The standard scripts/bench result schema; each no-daemon operation is
// sampled once, preserving TestNoDaemon's existing elapsed-time thresholds.
type mcpTimingRow struct {
	Name   string  `json:"name"`
	N      int     `json:"n"`
	P50    float64 `json:"p50_ms"`
	P95    float64 `json:"p95_ms"`
	Max    float64 `json:"max_ms"`
	Budget float64 `json:"budget_ms"`
	OK     bool    `json:"ok"`
	Tries  int     `json:"tries"`
	First  float64 `json:"first_p95_ms"`
}

type mcpNoDaemonTiming struct {
	t       *testing.T
	enabled bool
	path    string
	rows    []mcpTimingRow
}

// Ordinary tests retain all functional assertions without judging timing.
// Explicit timing opt-in is strict unless the benchmark requests JSON output.
func newMCPNoDaemonTiming(t *testing.T) *mcpNoDaemonTiming {
	t.Helper()
	r := &mcpNoDaemonTiming{t: t, enabled: os.Getenv("ZM_MCP_SPEED") != "", path: os.Getenv("ZM_MCP_JSON")}
	if r.enabled && r.path != "" {
		t.Cleanup(func() {
			if err := writeMCPNoDaemonTiming(r.path, r.rows); err != nil {
				t.Fatal(err)
			}
		})
	}
	return r
}

func (r *mcpNoDaemonTiming) record(name string, d time.Duration) {
	r.t.Helper()
	if !r.enabled {
		return
	}
	row := mcpNoDaemonRow(name, d)
	r.rows = append(r.rows, row)
	if !row.OK {
		report := r.t.Errorf
		if r.path != "" {
			report = r.t.Logf
		}
		report("%s took %g ms (budget %g ms)", name, row.P95, row.Budget)
	}
}

func mcpNoDaemonRow(name string, d time.Duration) mcpTimingRow {
	ms := float64(d) / float64(time.Millisecond)
	return mcpTimingRow{Name: name, N: 1, P50: ms, P95: ms, Max: ms,
		Budget: float64(noDaemonBudget / time.Millisecond), OK: d <= noDaemonBudget, Tries: 1, First: ms}
}

// Derive coverage from the registered tools, so adding a tool cannot silently
// drop its no-daemon benchmark. Reject missing, duplicate or malformed rows.
func writeMCPNoDaemonTiming(path string, rows []mcpTimingRow) error {
	want := map[string]bool{"mcp/no-daemon/initialize-list": true}
	for _, tool := range tools {
		want["mcp/no-daemon/tool/"+tool.Name] = true
	}
	for _, row := range rows {
		if !want[row.Name] || row.N != 1 || row.Tries != 1 || row.Budget != 200 || row.P95 < 0 ||
			row.P50 != row.P95 || row.Max != row.P95 || row.First != row.P95 || row.OK != (row.P95 <= row.Budget) {
			return fmt.Errorf("invalid no-daemon timing row %q", row.Name)
		}
		delete(want, row.Name)
	}
	if len(want) != 0 {
		return fmt.Errorf("no-daemon timing missing %d operations", len(want))
	}
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
