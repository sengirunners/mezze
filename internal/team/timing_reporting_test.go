package team_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A child test process proves strict mode actually fails the test, rather
// than only checking the row's classification. It never starts a daemon.
func TestTeamTimingStrict(t *testing.T) {
	if os.Getenv("ZM_TEAM_STRICT_PROBE") == "1" {
		newTeamTiming(t, "strict").record("team/strict-probe", []time.Duration{time.Second}, 50*time.Millisecond)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestTeamTimingStrict$")
	cmd.Env = append(os.Environ(), "ZM_TEAM_STRICT_PROBE=1", "ZM_TEAM_SPEED=1", "ZM_TEAM_JSON_DIR=")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "team/strict-probe p95 1000 ms over 50 ms") {
		t.Fatalf("strict test error %v, context %v, output %s", err, ctx.Err(), out)
	}
}

func TestTeamTimingSummary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []time.Duration
		budget  time.Duration
		wantOK  bool
		wantErr bool
	}{
		{name: "boundary passes", samples: []time.Duration{50 * time.Millisecond}, budget: 50 * time.Millisecond, wantOK: true},
		{name: "one nanosecond over fails", samples: []time.Duration{50*time.Millisecond + time.Nanosecond}, budget: 50 * time.Millisecond},
		{name: "missing samples", budget: 50 * time.Millisecond, wantErr: true},
		{name: "missing budget", samples: []time.Duration{time.Millisecond}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := summarizeTeamTiming("team/test", tc.samples, tc.budget)
			if (err != nil) != tc.wantErr || err == nil && row.OK != tc.wantOK {
				t.Fatalf("row %+v, error %v", row, err)
			}
		})
	}
	ds := []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}
	before := append([]time.Duration(nil), ds...)
	row, err := summarizeTeamTiming("team/test", ds, 2*time.Millisecond)
	if err != nil || row.N != 3 || row.P50 != 2 || row.P95 != 3 || row.Max != 3 || row.First != 3 || row.Tries != 1 || !reflect.DeepEqual(ds, before) {
		t.Fatalf("summary %+v, error %v, samples %v", row, err, ds)
	}
}

func TestTeamTimingReporting(t *testing.T) {
	dir := t.TempDir()
	t.Run("ordinary functional mode ignores timing", func(t *testing.T) {
		t.Setenv("ZM_TEAM_SPEED", "")
		t.Setenv("ZM_TEAM_JSON_DIR", dir)
		r := newTeamTiming(t, "disabled")
		r.record("team/offline-list", []time.Duration{time.Hour}, 50*time.Millisecond)
		if len(r.rows) != 0 {
			t.Fatal("ordinary mode recorded timing")
		}
	})
	if _, err := os.Stat(filepath.Join(dir, "disabled.json")); !os.IsNotExist(err) {
		t.Fatalf("ordinary mode wrote output: %v", err)
	}
	t.Run("advisory preserves over-budget row", func(t *testing.T) {
		t.Setenv("ZM_TEAM_SPEED", "1")
		t.Setenv("ZM_TEAM_JSON_DIR", dir)
		newTeamTiming(t, "advisory").record("team/offline-list", []time.Duration{time.Second}, 50*time.Millisecond)
	})
	b, err := os.ReadFile(filepath.Join(dir, "advisory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []teamTimingRow
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) != 1 || rows[0].OK || rows[0].P95 != 1000 || rows[0].Budget != 50 {
		t.Fatalf("advisory rows %s: %v", b, err)
	}
	// An output failure is a harness error, never an advisory timing miss.
	if err := writeTeamTiming(filepath.Join(dir, "advisory.json", "impossible.json"), rows); err == nil {
		t.Fatal("writing through a regular file succeeded")
	}
}
