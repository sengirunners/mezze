package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const mirrorTimingBudget = 50 * time.Millisecond

// scripts/bench's result schema. Keep the original 100 samples and 50 ms
// threshold; bench repeat decides whether repeated overruns fail in CI.
type mirrorTimingRow struct {
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

type mirrorTiming struct {
	t       *testing.T
	enabled bool
	path    string
	want    []string
	rows    []mirrorTimingRow
}

// Ordinary runs keep every functional check. Explicit timing opt-in is strict
// unless the benchmark requests JSON; output/coverage failures remain fatal.
func newMirrorTiming(t *testing.T, cases []benchCase) *mirrorTiming {
	t.Helper()
	r := &mirrorTiming{t: t, enabled: os.Getenv("ZM_MIRROR_SPEED") != "", path: os.Getenv("ZM_MIRROR_JSON")}
	for _, c := range cases {
		r.want = append(r.want, "mirror/"+c.name)
	}
	if r.enabled && r.path != "" {
		t.Cleanup(func() {
			if err := writeMirrorTiming(r.path, r.rows, r.want); err != nil {
				t.Fatal(err)
			}
		})
	}
	return r
}

func (r *mirrorTiming) record(name string, samples []time.Duration) {
	r.t.Helper()
	if !r.enabled {
		return
	}
	row, err := summarizeMirrorTiming("mirror/"+name, samples)
	if err != nil {
		r.t.Fatal(err)
	}
	r.rows = append(r.rows, row)
	if !row.OK {
		report := r.t.Errorf
		if r.path != "" {
			report = r.t.Logf
		}
		report("%s: p95 %g ms over the %g ms budget", name, row.P95, row.Budget)
	}
}

func summarizeMirrorTiming(name string, samples []time.Duration) (mirrorTimingRow, error) {
	if len(samples) != 100 {
		return mirrorTimingRow{}, fmt.Errorf("%s: need 100 timing samples, got %d", name, len(samples))
	}
	ds := slices.Clone(samples)
	slices.Sort(ds)
	p95 := ds[95]
	return mirrorTimingRow{Name: name, N: len(ds), P50: ms(ds[50]), P95: ms(p95), Max: ms(ds[99]),
		Budget: float64(mirrorTimingBudget / time.Millisecond), OK: p95 <= mirrorTimingBudget, Tries: 1, First: ms(p95)}, nil
}

// Expected names come from the exact cases exercised, not a second case list.
func writeMirrorTiming(path string, rows []mirrorTimingRow, expected []string) error {
	want := make(map[string]bool, len(expected))
	for _, name := range expected {
		if want[name] {
			return fmt.Errorf("duplicate expected mirror operation %q", name)
		}
		want[name] = true
	}
	if len(want) == 0 {
		return fmt.Errorf("mirror timing needs expected operations")
	}
	for _, row := range rows {
		if !want[row.Name] || row.N != 100 || row.Tries != 1 || row.Budget != 50 || row.P50 < 0 ||
			row.P50 > row.P95 || row.P95 > row.Max || row.First != row.P95 || row.OK != (row.P95 <= row.Budget) {
			return fmt.Errorf("invalid mirror timing row %q", row.Name)
		}
		delete(want, row.Name)
	}
	if len(want) != 0 {
		return fmt.Errorf("mirror timing missing %d operations", len(want))
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
