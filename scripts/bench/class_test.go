package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

const zmBench = "zm-bench workshop-bench"

// withClass makes loadClass see this file for class zm-bench (nil: none, calibrating).
func withClass(t *testing.T, rows map[string]float64) {
	t.Helper()
	was := loadClass
	t.Cleanup(func() { loadClass = was })
	loadClass = func(class string) (ceilings.File, bool, error) {
		if class != ceilings.Dedicated {
			return was(class)
		}
		if rows == nil {
			return ceilings.File{}, false, nil
		}
		f := ceilings.File{Bench: map[string]ceilings.Row{}}
		for k, v := range rows {
			f.Bench[k] = ceilings.Row{P95: v}
		}
		return f, true, nil
	}
}

// writeRows writes rows as a bench.json.
func writeRows(t *testing.T, rs []result) string {
	t.Helper()
	b, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// No ceilings file for zm-bench yet: its runs are calibration, never a verdict,
// even with the same rows over Sengi's ceilings twice; the notice counts the runs.
func TestRepeatZmBenchCalibrates(t *testing.T) {
	withClass(t, nil)
	code, s := runRepeat(t, "-runner", zmBench, "-class-runs", "3", "-prev", noisy, noisy)
	if code != 0 || !strings.Contains(s, "No verdict: calibrating zm-bench: 4 runs so far") ||
		!strings.Contains(s, "scripts/ceilings/ceilings.zm-bench.json") || strings.Contains(s, "**hook-stop**") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	if _, s := runRepeat(t, "-runner", zmBench, noisy); !strings.Contains(s, "calibrating zm-bench: run count unknown") {
		t.Fatal(s)
	}
	// A zm pool box is its size's class, calibrating the same way.
	if code, s := runRepeat(t, "-runner", "sengi:c2m8 zm-123", "-class-runs", "1", "-prev", noisy, noisy); code != 0 ||
		!strings.Contains(s, "No verdict: calibrating box-c2m8: 2 runs so far") || !strings.Contains(s, "ceilings.box-c2m8.json") {
		t.Fatalf("box: exit %d:\n%s", code, s)
	}
}

// Nothing ships a zm-bench file yet: the real loader says calibrating.
func TestZmBenchShipsUncalibrated(t *testing.T) {
	if _, ok, err := ceilings.LoadClass(ceilings.Dedicated); ok || err != nil {
		t.Fatalf("ok %v, err %v: a zm-bench file ships; delete this test with the PR that commits it", ok, err)
	}
}

// Calibrated: rows and the previous run are judged on zm-bench's own ceilings.
// hook-stop at 23.7 ms twice is over Sengi's 15 but under zm-bench's 30: green.
// version at 8.8 ms twice is under Sengi's 13 but over zm-bench's 5: red. A Sengi
// row with no zm-bench line is not judged.
func TestRepeatZmBenchJudgedOnItsOwnCeilings(t *testing.T) {
	withClass(t, map[string]float64{"hook-stop": 30})
	code, s := runRepeat(t, "-runner", zmBench, "-prev", withOver(t, noisy, "hook-stop"), withOver(t, noisy, "hook-stop"))
	if code != 0 || !strings.Contains(s, "No row over its ceiling.") || !strings.Contains(s, "not judged: version, hook-prompt") {
		t.Fatalf("hook-stop under zm-bench's ceiling: exit %d:\n%s", code, s)
	}
	withClass(t, map[string]float64{"version": 5})
	code, s = runRepeat(t, "-runner", zmBench, "-prev", clean, clean)
	if code != 1 || !strings.Contains(s, "**version**: p95 8.8 ms, ceiling 5 ms") || strings.Contains(s, "**hook-stop**") {
		t.Fatalf("version over zm-bench's ceiling: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-warn", "-runner", zmBench, "-prev", clean, clean); code != 0 {
		t.Fatalf("-warn: exit %d:\n%s", code, s)
	}
}

// Sengi stays judged by ceilings.json whatever zm-bench's file says.
func TestRepeatSengiIgnoresClassFile(t *testing.T) {
	withClass(t, map[string]float64{"hook-stop": 99, "version": 99})
	if code, s := runRepeat(t, "-runner", "sengi-standard-4-ubuntu-2404 sengi-4-07", "-prev", noisy, noisy); code != 1 {
		t.Fatalf("exit %d:\n%s", code, s)
	}
}

// make bench retries against a calibrated class's ceilings, Sengi's elsewhere.
func TestBenchCeilingsByLabel(t *testing.T) {
	withClass(t, nil)
	c, err := benchCeilings(zmBench)
	if err != nil || c["version"].P95 != 13 {
		t.Fatalf("calibrating: version %g, err %v", c["version"].P95, err)
	}
	withClass(t, map[string]float64{"version": 40})
	for label, want := range map[string]float64{zmBench: 40, "sengi-standard-4-ubuntu-2404": 13, "": 13, "acme-runner-4": 13} {
		c, err := benchCeilings(label)
		if err != nil || c["version"].P95 != want || c["hook-stop"].P95 != 15 {
			t.Errorf("%q: version %g hook-stop %g, err %v", label, c["version"].P95, c["hook-stop"].P95, err)
		}
	}
	if s, _ := ceilings.Load(); s.Bench["version"].P95 != 13 {
		t.Fatal("the overlay changed Sengi's ceilings")
	}
}

// calibrate: the highest p95 of the runs x 1.6 + 1, rounded up; bar rows capped
// at 16 ms and test-budget rows at their budget; the probe left out; under -min
// runs refused; the proposal parses as a ceilings file.
func TestCalibrate(t *testing.T) {
	base, err := readResults(clean)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := range 10 {
		rs := []result{}
		for _, r := range base {
			if r.Name == "version" {
				r.P95 = 10 + float64(i) // highest 19: 19 x 1.6 + 1 = 31.4 -> 32
			}
			if r.Name == "issue-show" {
				r.P95 = 12 // 12 x 1.6 + 1 = 20.2: over the bar
			}
			rs = append(rs, r)
		}
		rs = append(rs, result{Name: probeRow, P95: 3, Budget: 1},
			result{Name: "mirror/work show", P95: 40, Budget: 50, OK: true}, // 65 -> its 50 ms budget
			result{Name: "rank/common", P95: 5, Budget: 16, OK: true})       // 9
		paths = append(paths, writeRows(t, rs))
	}
	var out, errw bytes.Buffer
	if err := calibrateCmd(append([]string{"-class", "zm-bench"}, paths...), &out, &errw); err != nil {
		t.Fatal(err)
	}
	f, err := ceilings.Parse(out.Bytes())
	if err != nil {
		t.Fatalf("%v:\n%s", err, out.String())
	}
	for name, want := range map[string]float64{"version": 32, "issue-show": 16, "mirror/work show": 50, "rank/common": 9,
		"hook-stop": 20} { // 11.763 x 1.6 + 1 = 19.8 -> 20
		if got := f.Bench[name].P95; got != want {
			t.Errorf("%s: %g, want %g", name, got, want)
		}
	}
	if _, ok := f.Bench[probeRow]; ok {
		t.Error("the probe got a ceiling")
	}
	if !strings.Contains(errw.String(), "issue-show: 21 ms from p95 12.0, capped at 16 ms (the 16 ms bar)") ||
		!strings.Contains(errw.String(), "mirror/work show: 65 ms from p95 40.0, capped at 50 ms (its own test budget)") {
		t.Errorf("caps not said:\n%s", errw.String())
	}
	if !strings.Contains(f.Doc, "from 10 of its runs") {
		t.Error(f.Doc)
	}
	if err := calibrateCmd(append([]string{"-class", "zm-bench"}, paths[:9]...), &out, &errw); err == nil ||
		!strings.Contains(err.Error(), "9 runs, need 10: keep calibrating") {
		t.Fatalf("9 runs: %v", err)
	}
	if err := calibrateCmd(append([]string{"-class", "box-c2m8"}, paths...), &out, &errw); err != nil {
		t.Fatalf("a pool box's class: %v", err)
	}
	if err := calibrateCmd(append([]string{"-class", "sengi"}, paths...), &out, &errw); err == nil {
		t.Fatal("calibrated Sengi: ceilings.json is Sengi's and only tightens")
	}
}

// A committed class file only tightens, and deleting it gives every row away.
func TestTightenClass(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(base, []byte(`{"bench": {"version": {"p95_ms": 20}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rows map[string]float64
		code int
		want string
	}{
		{map[string]float64{"version": 18}, 0, "nothing loosened"},
		{map[string]float64{"version": 25}, 1, "bench/version went from 20 to 25 ms with no new reason"},
		{nil, 1, "bench/version is gone"},
	} {
		withClass(t, c.rows)
		var b bytes.Buffer
		code, err := tightenCmd([]string{"-class", "zm-bench", base}, &b)
		if err != nil || code != c.code || !strings.Contains(b.String(), c.want) || (code == 1 && !strings.Contains(b.String(), "ceilings.zm-bench.json")) {
			t.Errorf("%v: exit %d, err %v:\n%s", c.rows, code, err, b.String())
		}
	}
	// Without -class it is still ceilings.json against main's.
	var b bytes.Buffer
	sengi := filepath.Join(t.TempDir(), "sengi.json")
	if err := os.WriteFile(sengi, []byte(`{"bench": {"version": {"p95_ms": 13}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := tightenCmd([]string{sengi}, &b); err != nil || code != 0 {
		t.Fatal(fmt.Sprint(code, err, b.String()))
	}
}

// zm-bench's base is a zm-bench run: its "slower" rows are not "runner against Sengi".
func TestCompareZmBenchAgainstItsClassIsNotLabelled(t *testing.T) {
	base := []result{{Name: "a", First: 4, P95: 4, Budget: 16, OK: true}}
	now := []result{{Name: "a", First: 12, P95: 12, Budget: 16, OK: true}}
	var b bytes.Buffer
	compare(&b, base, now, "", zmBench)
	if strings.Contains(b.String(), "another runner") || !strings.Contains(b.String(), "Slower than main's last run (a report") {
		t.Fatal(b.String())
	}
}
