package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recorded runs: main at 0e51d6e, run 41641907512. The code under these rows did
// not change between the attempts, which is what the rule is for.
const (
	noisy  = "testdata/run41641907512-attempt1.json"        // 15 rows over
	clean  = "testdata/run41641907512-attempt2.json"        // no row over
	ranked = "testdata/run41641907512-attempt2-ranked.json" // 3 ranked rows over
)

// withOver writes the recorded file back with exactly these rows over.
func withOver(t *testing.T, file string, rows ...string) string {
	t.Helper()
	rs, err := readResults(file)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for i := range rs {
		rs[i].OK = true
		for _, name := range rows {
			if rs[i].Name == name {
				rs[i].OK = false
				found++
			}
		}
	}
	if found != len(rows) {
		t.Fatalf("%s: found %d of %v", file, found, rows)
	}
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

func runRepeat(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	code, err := repeatCmd(args, &b)
	if err != nil {
		t.Fatal(err)
	}
	return code, b.String()
}

func TestRepeatManyOverThenCleanStaysQuiet(t *testing.T) {
	code, s := runRepeat(t, "-prev", noisy, clean)
	if code != 0 || !strings.Contains(s, "No row over its ceiling.") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
}

// The real attempt 1 as the first of two runs: all 15 rows warn, none reds.
func TestRepeatManyOverOnceOnlyWarns(t *testing.T) {
	code, s := runRepeat(t, "-prev", clean, noisy)
	if code != 0 || !strings.Contains(s, "**hook-stop**: p95 23.7 ms, ceiling 15 ms") || !strings.Contains(s, "This job stays green") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	if strings.Contains(s, "in the last finished main run:") {
		t.Fatalf("called a repeat:\n%s", s)
	}
}

func TestRepeatSameRowTwiceGoesRed(t *testing.T) {
	code, s := runRepeat(t, "-prev", withOver(t, clean, "hook-stop", "send"), withOver(t, noisy, "hook-stop", "version"))
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	repeat := s[strings.Index(s, "in the last finished main run:"):]
	if !strings.Contains(repeat, "**hook-stop**") || strings.Contains(repeat, "**version**") || strings.Contains(repeat, "**send**") {
		t.Fatalf("named the wrong rows:\n%s", s)
	}
	if !strings.Contains(s, "**version**") || strings.Contains(s, "This job stays green") {
		t.Fatalf("%s", s)
	}
}

// Stays red for as long as the row stays over.
func TestRepeatThirdRunStaysRed(t *testing.T) {
	if code, s := runRepeat(t, "-prev", noisy, noisy); code != 1 {
		t.Fatalf("exit %d:\n%s", code, s)
	}
}

func TestRepeatDifferentRowsStayQuiet(t *testing.T) {
	code, s := runRepeat(t, "-prev", withOver(t, clean, "hook-stop"), withOver(t, clean, "send"))
	if code != 0 || !strings.Contains(s, "**send**") || strings.Contains(s, "**hook-stop**") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
}

func TestRepeatMissingPreviousStaysQuiet(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(bad, []byte(`[{"name":`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-prev", filepath.Join(t.TempDir(), "main-bench", "bench.json"), noisy}, // no artifact to download
		{"-prev", bad, noisy}, // a cut-off artifact
		{noisy},
	} {
		code, s := runRepeat(t, args...)
		if code != 0 || !strings.Contains(s, "nothing can repeat") || !strings.Contains(s, "**version**") {
			t.Errorf("%v: exit %d:\n%s", args, code, s)
		}
	}
}

// A PR is compared with main, and only warns, however bad it looks.
func TestRepeatWarnNeverReds(t *testing.T) {
	code, s := runRepeat(t, "-warn", "-prev", noisy, noisy)
	if code != 0 || !strings.Contains(s, "in the last finished main run:") || !strings.Contains(s, "This job stays green") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
}

// Ranked search's rows are rows: one bad run warns, the same row twice reds.
func TestRepeatRankedRows(t *testing.T) {
	if code, s := runRepeat(t, "-prev", withOver(t, ranked), ranked); code != 0 || !strings.Contains(s, "**rank/label filter**: p95 16.1 ms, ceiling 16 ms") {
		t.Fatalf("one run: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-prev", withOver(t, ranked, "rank/type filter"), ranked); code != 1 || !strings.Contains(s[strings.Index(s, "in the last finished main run:"):], "**rank/type filter**") {
		t.Fatalf("twice: exit %d:\n%s", code, s)
	}
}

// A run with nothing in it is a broken harness, not a clean bill.
func TestRepeatRefusesAnEmptyRun(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `[{"name":`} {
		p := filepath.Join(t.TempDir(), "bench.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := repeatCmd([]string{p}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
	if _, err := repeatCmd([]string{filepath.Join(t.TempDir(), "absent.json")}, &bytes.Buffer{}); err == nil {
		t.Error("absent file: no error")
	}
}

// Ranked rows have no line in ceilings.json to tighten.
func TestCompareOffersNoRoomOnRankedRows(t *testing.T) {
	rs, err := readResults(ranked)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	compare(&b, nil, rs, "", "")
	s := b.String()
	if strings.Contains(s, "Room to tighten") || !strings.Contains(s, "| rank/rare word |") {
		t.Fatalf("%s", s)
	}
}

// The ceilings came from Sengi: the same rows over twice reds there, and is no
// verdict on any other runner (a Factory VM had 15 to 40 rows over at once).
func TestRepeatVerdictOnlyOnSengi(t *testing.T) {
	if code, s := runRepeat(t, "-runner", "sengi-standard-4-ubuntu-2404 sengi-4-07", "-prev", noisy, noisy); code != 1 {
		t.Fatalf("Sengi: exit %d:\n%s", code, s)
	}
	code, s := runRepeat(t, "-runner", "acme-runner-4 tfp-dovym6twkor5", "-prev", noisy, noisy)
	if code != 0 || !strings.Contains(s, "No verdict: not a Sengi runner (acme-runner-4 tfp-dovym6twkor5)") ||
		!strings.Contains(s, "**hook-stop**") || strings.Contains(s, "in the last finished main run:") {
		t.Fatalf("Factory: exit %d:\n%s", code, s)
	}
}

// runner/probe is a report: never a row over, even from an artifact of PR #479's
// draft that marked it over.
func TestRepeatProbeIsNotARow(t *testing.T) {
	rs, err := readResults(clean)
	if err != nil {
		t.Fatal(err)
	}
	rs = append(rs, result{Name: probeRow, N: 120, P50: 2, P95: 6, Budget: 1, OK: false, Tries: 1, First: 6})
	b, _ := json.Marshal(rs)
	p := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, s := runRepeat(t, "-runner", "sengi-standard-4-ubuntu-2404", "-prev", p, p); code != 0 || !strings.Contains(s, "No row over its ceiling.") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	var c bytes.Buffer
	compare(&c, rs, rs, "", "")
	if s := c.String(); strings.Contains(s, "Over a ceiling") || !strings.Contains(s, "| runner/probe | 120 | 2.0 | 6.0 | 6.0 | report only |") ||
		!strings.Contains(s, "marks the run contended") {
		t.Fatalf("compare:\n%s", s)
	}
}

// No Sengi run to compare with: main's last finished run stands in, labelled.
func TestCompareOtherRunnerBaseIsLabelled(t *testing.T) {
	var b bytes.Buffer
	base := []result{{Name: "a", First: 4, P95: 4, Budget: 16, OK: true}}
	compare(&b, base, base, "run 42338331083 on acme-runner-4", "")
	if !strings.Contains(b.String(), "other-runner data (run 42338331083 on acme-runner-4)") {
		t.Fatal(b.String())
	}
}

// "Twice in a row" is bounded by age: a Sengi run over two days ago
// is not the first of two, even with the same rows over.
func TestRepeatPairBoundedByAge(t *testing.T) {
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })
	sengi := "sengi-standard-4-ubuntu-2404 sengi-4-07"
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	if code, s := runRepeat(t, "-runner", sengi, "-prev-at", at(47*time.Hour), "-prev", noisy, noisy); code != 1 {
		t.Fatalf("47 h: exit %d:\n%s", code, s)
	}
	code, s := runRepeat(t, "-runner", sengi, "-prev-at", at(49*time.Hour), "-prev", noisy, noisy)
	if code != 0 || !strings.Contains(s, "it started 49h0m0s ago, over the 48h0m0s bound, so it is not a pair") ||
		!strings.Contains(s, "Over in this run only") || strings.Contains(s, "in the last finished main run:") {
		t.Fatalf("49 h: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-runner", sengi, "-pair-max", "72h", "-prev-at", at(49*time.Hour), "-prev", noisy, noisy); code != 1 {
		t.Fatalf("-pair-max 72h: exit %d:\n%s", code, s)
	}
	var b bytes.Buffer
	if _, err := repeatCmd([]string{"-prev-at", "yesterday", "-prev", noisy, noisy}, &b); err == nil {
		t.Fatal("a bad -prev-at was accepted")
	}
}

// A Factory run against a Sengi base labels "slower" as runner against Sengi.
func TestCompareFactoryAgainstSengiIsLabelled(t *testing.T) {
	base := []result{{Name: "a", First: 4, P95: 4, Budget: 16, OK: true}}
	now := []result{{Name: "a", First: 12, P95: 12, Budget: 16, OK: true}}
	var f, s bytes.Buffer
	compare(&f, base, now, "", "acme-runner-4 tfp-abc")
	if !strings.Contains(f.String(), "this run timed on another runner (acme-runner-4 tfp-abc): runner against Sengi") ||
		!strings.Contains(f.String(), "- a: p95 12.0 ms, main 4.0 ms") {
		t.Fatal(f.String())
	}
	compare(&s, base, now, "", "sengi-standard-4-ubuntu-2404 sengi-4-07")
	if strings.Contains(s.String(), "another runner") || !strings.Contains(s.String(), "Slower than main's last run (a report") {
		t.Fatal(s.String())
	}
}

// Recorded Sengi pairs. Contended: main's red at b39b841a (run 43677887923 attempt 2,
// probe p95 1.805 ms, 12 rows over) against the run before it (43666015368, probe
// 2.185, 24 over). Quiet: zero-start over in 43531023112 and 43536681544 (probe 1.03 and 1.13),
// a real slowdown.
const (
	contendedPrev = "testdata/run43666015368.json"
	contendedNow  = "testdata/run43677887923.json"
	quietPrev     = "testdata/run43531023112.json"
	quietNow      = "testdata/run43536681544.json"
)

func TestRepeatContendedPairGivesNoVerdict(t *testing.T) {
	sengi := "sengi-standard-4-ubuntu-2404 gr8zzrptcrby5n98t5hrcqa2ep"
	code, s := runRepeat(t, "-runner", sengi, "-prev", contendedPrev, contendedNow)
	if code != 0 || !strings.Contains(s, "No verdict: contended runner: this run's runner/probe p95 is 1.80 ms and the last finished main run's runner/probe p95 is 2.19 ms") ||
		!strings.Contains(s, "**hook-prompt**: p95 23.5 ms, ceiling 15 ms") || strings.Contains(s, "in the last finished main run:") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	// Either run contended is enough: the quiet run after a contended one, and the other way round.
	if code, s := runRepeat(t, "-runner", sengi, "-prev", contendedPrev, withProbe(t, contendedNow, 0.9)); code != 0 ||
		!strings.Contains(s, "No verdict: contended runner: the last finished main run's") {
		t.Fatalf("prev contended: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-runner", sengi, "-prev", withProbe(t, contendedPrev, 0.9), contendedNow); code != 0 ||
		!strings.Contains(s, "No verdict: contended runner: this run's") {
		t.Fatalf("now contended: exit %d:\n%s", code, s)
	}
	// Both quiet: the same rows twice are a verdict again.
	if code, s := runRepeat(t, "-runner", sengi, "-prev", withProbe(t, contendedPrev, 0.9), withProbe(t, contendedNow, 0.9)); code != 1 {
		t.Fatalf("both quiet: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-runner", sengi, "-contended", "2.5", "-prev", contendedPrev, contendedNow); code != 1 {
		t.Fatalf("-contended 2.5: exit %d:\n%s", code, s)
	}
}

func TestRepeatQuietPairBothOverStaysRed(t *testing.T) {
	sengi := "sengi-standard-4-ubuntu-2404 grgw8ma78evp1hreh18bagwg9s"
	code, s := runRepeat(t, "-runner", sengi, "-prev", quietPrev, quietNow)
	if code != 1 || strings.Contains(s, "No verdict") || !strings.Contains(s, "**zero-start**: p95 194.6 ms, ceiling 155 ms") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	// Quiet late probes too: still red.
	if code, s := runRepeat(t, "-runner", sengi, "-prev", withLateProbe(t, quietPrev, 1.0), withLateProbe(t, quietNow, 1.2)); code != 1 ||
		strings.Contains(s, "No verdict") {
		t.Fatalf("quiet late probes: exit %d:\n%s", code, s)
	}
}

// Recorded Sengi runs whose VM slowed after the first probe: quiet runner/probe (1.054 and 0.992 ms),
// then 18 and 12 rows over from hook-stop on. Run 43649180029 is main's red at e33aa767; 43680130314 is a
// dispatch of f1fb1938. Five rows are over in both, so with the first probe alone the pair reds.
const (
	lateSlowPrev = "testdata/run43649180029.json"
	lateSlowNow  = "testdata/run43680130314.json"
)

func TestRepeatLateProbeContendedGivesNoVerdict(t *testing.T) {
	sengi := "sengi-standard-4-ubuntu-2404 grf5emgzrdce1jczxbkfpzrfgh"
	if code, s := runRepeat(t, "-runner", sengi, "-prev", lateSlowPrev, lateSlowNow); code != 1 || strings.Contains(s, "No verdict") ||
		!strings.Contains(s, "**hook-stop**") {
		t.Fatalf("first probe only: exit %d:\n%s", code, s)
	}
	// No recorded run has a late probe yet: 1.805 is the contended first probe measured at b39b841a.
	code, s := runRepeat(t, "-runner", sengi, "-prev", lateSlowPrev, withLateProbe(t, lateSlowNow, 1.805))
	if code != 0 || !strings.Contains(s, "No verdict: contended runner: this run's runner/probe-late p95 is 1.80 ms, over 1.3 ms") ||
		!strings.Contains(s, "**hook-stop**") {
		t.Fatalf("late probe slow now: exit %d:\n%s", code, s)
	}
	if code, s := runRepeat(t, "-runner", sengi, "-prev", withLateProbe(t, lateSlowPrev, 1.805), lateSlowNow); code != 0 ||
		!strings.Contains(s, "No verdict: contended runner: the last finished main run's runner/probe-late p95 is 1.80 ms") {
		t.Fatalf("late probe slow prev: exit %d:\n%s", code, s)
	}
	// The worse reading counts: a slow first probe with a quiet late one is contended too.
	if code, s := runRepeat(t, "-runner", sengi, "-prev", contendedPrev, withLateProbe(t, contendedNow, 0.9)); code != 0 ||
		!strings.Contains(s, "this run's runner/probe p95 is 1.80 ms") {
		t.Fatalf("early slow, late quiet: exit %d:\n%s", code, s)
	}
	// Never a row over itself, nor slower in compare.
	var c bytes.Buffer
	compare(&c, readFile(t, lateSlowNow), readFile(t, withLateProbe(t, lateSlowNow, 9)), "", "")
	if s := c.String(); strings.Contains(s, "runner/probe-late**") || !strings.Contains(s, "| runner/probe-late | 120 |") ||
		!strings.Contains(s, "runner/probe-late (after the last) p50 0.9 ms, p95 9.0 ms") {
		t.Fatalf("compare:\n%s", s)
	}
}

// bench-baseline.sh judges blindness with its own copy of contendedP95.
func TestBaselineContendedLimitMatches(t *testing.T) {
	b, err := os.ReadFile("../ci/bench-baseline.sh")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`CONTENDED_P95="${CONTENDED_P95:-%g}"`, contendedP95); !strings.Contains(string(b), want) {
		t.Fatalf("bench-baseline.sh lacks %s", want)
	}
}

func TestProbeCmdIsALateProbeRow(t *testing.T) {
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true binary")
	}
	var b bytes.Buffer
	if err := probeCmd([]string{"-n", "5", bin}, &b); err != nil {
		t.Fatal(err)
	}
	var rs []result
	if err := json.Unmarshal(b.Bytes(), &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Name != lateProbeRow || rs[0].N != 5 || !rs[0].OK || rs[0].P95 <= 0 {
		t.Fatalf("%+v", rs)
	}
	if err := probeCmd([]string{filepath.Join(t.TempDir(), "missing")}, &b); err == nil {
		t.Fatal("a missing probe binary is no error")
	}
}

func readFile(t *testing.T, file string) []result {
	t.Helper()
	rs, err := readResults(file)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// withProbe writes the recorded file back with runner/probe's p95 set to p95.
func withProbe(t *testing.T, file string, p95 float64) string {
	t.Helper()
	return withProbeRow(t, file, probeRow, p95)
}

// withLateProbe writes the recorded file back with a runner/probe-late row of p95 (p50 0.9).
func withLateProbe(t *testing.T, file string, p95 float64) string {
	t.Helper()
	return withProbeRow(t, file, lateProbeRow, p95)
}

func withProbeRow(t *testing.T, file, row string, p95 float64) string {
	t.Helper()
	rs := readFile(t, file)
	found := false
	for i := range rs {
		if rs[i].Name == row {
			rs[i].P95, rs[i].First, found = p95, p95, true
		}
	}
	if !found && row == probeRow {
		t.Fatalf("%s has no %s row", file, probeRow)
	}
	if !found {
		rs = append(rs, result{Name: row, N: 120, P50: 0.9, P95: p95, Max: p95, OK: true, Tries: 1, First: p95})
	}
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
