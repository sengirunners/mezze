package work

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mirrorTimingSamples(d time.Duration) []time.Duration {
	ds := make([]time.Duration, 100)
	for i := range ds {
		ds[i] = d
	}
	return ds
}

// Building the canonical case descriptors needs no daemon; their closures
// are never invoked by these reporter tests.
func mirrorTimingFixture() ([]mirrorTimingRow, []string) {
	var rows []mirrorTimingRow
	var want []string
	for _, c := range bigCases(&harness{}, nil, nil) {
		name := "mirror/" + c.name
		row, _ := summarizeMirrorTiming(name, mirrorTimingSamples(time.Second))
		rows, want = append(rows, row), append(want, name)
	}
	return rows, want
}

func TestMirrorTimingSummary(t *testing.T) {
	for _, d := range []time.Duration{mirrorTimingBudget, mirrorTimingBudget + time.Nanosecond} {
		row, err := summarizeMirrorTiming("mirror/boundary", mirrorTimingSamples(d))
		if err != nil || row.OK != (d == mirrorTimingBudget) || row.N != 100 || row.Tries != 1 || row.Budget != 50 {
			t.Fatalf("duration %v: row %+v, error %v", d, row, err)
		}
	}
	if _, err := summarizeMirrorTiming("mirror/missing", mirrorTimingSamples(time.Second)[:99]); err == nil {
		t.Fatal("incomplete samples accepted")
	}
	ds := mirrorTimingSamples(time.Millisecond)
	ds[0], ds[1] = 2*time.Millisecond, time.Second
	before := slices.Clone(ds)
	row, err := summarizeMirrorTiming("mirror/unsorted", ds)
	if err != nil || row.P50 != 1 || row.P95 != 1 || row.Max != 1000 || row.First != row.P95 || !slices.Equal(before, ds) {
		t.Fatalf("summary %+v, error %v, samples changed %v", row, err, !slices.Equal(before, ds))
	}
}

func TestMirrorTimingCoverage(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "unknown", "samples", "budget", "verdict", "nonfinite"} {
		t.Run(kind, func(t *testing.T) {
			rows, want := mirrorTimingFixture()
			switch kind {
			case "missing":
				rows = rows[:len(rows)-1]
			case "duplicate":
				rows = append(rows, rows[0])
			case "unknown":
				rows[0].Name = "unknown"
			case "samples":
				rows[0].N--
			case "budget":
				rows[0].Budget++
			case "verdict":
				rows[0].OK = true
			case "nonfinite":
				rows[0].P50, rows[0].P95, rows[0].Max, rows[0].First = math.Inf(1), math.Inf(1), math.Inf(1), math.Inf(1)
			}
			if err := writeMirrorTiming(filepath.Join(t.TempDir(), "timing.json"), rows, want); err == nil {
				t.Fatal("invalid timing report accepted")
			}
		})
	}
}

func TestMirrorTimingReporting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timing.json")
	cases := bigCases(&harness{}, nil, nil)
	t.Run("ordinary functional mode", func(t *testing.T) {
		t.Setenv("ZM_MIRROR_SPEED", "")
		t.Setenv("ZM_MIRROR_JSON", path)
		r := newMirrorTiming(t, cases)
		r.record(cases[0].name, mirrorTimingSamples(time.Hour))
		if len(r.rows) != 0 {
			t.Fatal("ordinary mode recorded timing")
		}
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ordinary mode wrote a report: %v", err)
	}
	t.Run("advisory preserves all cases and overruns", func(t *testing.T) {
		t.Setenv("ZM_MIRROR_SPEED", "1")
		t.Setenv("ZM_MIRROR_JSON", path)
		r := newMirrorTiming(t, cases)
		for _, c := range cases {
			r.record(c.name, mirrorTimingSamples(time.Second))
		}
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []mirrorTimingRow
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) != len(cases) || rows[0].OK || rows[0].P95 != 1000 || rows[0].N != 100 || rows[0].Budget != 50 {
		t.Fatalf("report %s: %v", b, err)
	}
	_, want := mirrorTimingFixture()
	if err := writeMirrorTiming(filepath.Join(path, "impossible.json"), rows, want); err == nil {
		t.Fatal("writer failure was hidden")
	}
}

func TestMirrorTimingStrict(t *testing.T) {
	if os.Getenv("ZM_MIRROR_STRICT_PROBE") == "1" {
		newMirrorTiming(t, []benchCase{{name: "strict-probe"}}).record("strict-probe", mirrorTimingSamples(time.Second))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMirrorTimingStrict$")
	cmd.Env = append(os.Environ(), "ZM_MIRROR_STRICT_PROBE=1", "ZM_MIRROR_SPEED=1", "ZM_MIRROR_JSON=")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "strict-probe: p95 1000 ms over the 50 ms budget") {
		t.Fatalf("strict test error %v, context %v, output %s", err, ctx.Err(), out)
	}
}
