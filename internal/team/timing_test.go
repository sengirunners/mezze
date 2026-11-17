package team_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Match scripts/bench's result schema, as ranked search does. These test-owned
// budgets remain unchanged; bench repeat judges their rows in CI.
type teamTimingRow struct {
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

func teamTimingEnabled() bool { return os.Getenv("ZM_TEAM_SPEED") != "" }

type teamTiming struct {
	t       *testing.T
	enabled bool
	dir     string
	rows    []teamTimingRow
}

// Ordinary functional tests never judge latency. Opt in with ZM_TEAM_SPEED=1
// for strict manual checks; ZM_TEAM_JSON_DIR makes latency advisory and writes
// one file per test for bench.yml. Functional failures always remain fatal.
func newTeamTiming(t *testing.T, name string) *teamTiming {
	t.Helper()
	r := &teamTiming{t: t, enabled: teamTimingEnabled(), dir: os.Getenv("ZM_TEAM_JSON_DIR")}
	if r.enabled && r.dir != "" {
		t.Cleanup(func() {
			if len(r.rows) == 0 {
				t.Error("timing harness recorded no rows")
				return
			}
			if err := writeTeamTiming(filepath.Join(r.dir, name+".json"), r.rows); err != nil {
				t.Fatal(err)
			}
		})
	}
	return r
}

func (r *teamTiming) record(name string, samples []time.Duration, budget time.Duration) {
	r.t.Helper()
	if !r.enabled {
		return
	}
	row, err := summarizeTeamTiming(name, samples, budget)
	if err != nil {
		r.t.Fatal(err)
	}
	r.rows = append(r.rows, row)
	if !row.OK {
		report := r.t.Errorf
		if r.dir != "" {
			report = r.t.Logf
		}
		report("%s p95 %g ms over %g ms", name, row.P95, row.Budget)
	}
}

func summarizeTeamTiming(name string, samples []time.Duration, budget time.Duration) (teamTimingRow, error) {
	if len(samples) == 0 || budget <= 0 {
		return teamTimingRow{}, fmt.Errorf("%s: timing needs samples and a positive budget", name)
	}
	ds := slices.Clone(samples)
	slices.Sort(ds)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	p95 := ds[len(ds)*95/100]
	return teamTimingRow{Name: name, N: len(ds), P50: ms(ds[len(ds)/2]), P95: ms(p95),
		Max: ms(ds[len(ds)-1]), Budget: ms(budget), OK: p95 <= budget, Tries: 1, First: ms(p95)}, nil
}

func writeTeamTiming(path string, rows []teamTimingRow) error {
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
