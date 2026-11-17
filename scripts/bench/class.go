package main

// Runner classes. A verdict class is a kind of runner whose timing
// can judge zm: Sengi (ceilings.json, where the numbers came from) and zm's
// own bench machine, bench.yml's ZM_BENCH_RUNNER (a zm pool box size, class
// box-<size>, or a dedicated runner, zm-bench; ceilings.ClassOf). Its speed is
// not Sengi's, so it is calibrated before it judges: while it has no
// ceilings.<class>.json, its runs give no verdict (bench repeat says
// "calibrating: N runs so far"). `bench calibrate` proposes that file from the
// class's last runs; once committed it only tightens (bench tighten -class).
// Sengi's ceilings are not touched by any of this.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

// loadClass is ceilings.LoadClass, swapped in tests.
var loadClass = ceilings.LoadClass

// Calibration's headroom: the same rule as ceilings.json's bench rows
// (DESIGN-NOTES, The speed gate): the highest p95 of the runs x 1.6 + 1 ms.
const (
	calibrateRatio = 1.6
	calibrateAddMS = 1.0
	calibrateMin   = 10
)

// benchCeilings is what make bench measures against on a runs-on label:
// Sengi's ceilings, with a calibrated class's rows over them, so a row over the
// class's ceiling is retried like any other. Not calibrated, or no class: Sengi's.
func benchCeilings(label string) (map[string]ceilings.Row, error) {
	sengi, err := ceilings.Load()
	if err != nil {
		return nil, err
	}
	class := ceilings.ClassOf(label)
	if class == "" || class == ceilings.Sengi {
		return sengi.Bench, nil
	}
	cf, ok, err := loadClass(class)
	if err != nil || !ok {
		return sengi.Bench, err
	}
	out := maps.Clone(sengi.Bench)
	maps.Copy(out, cf.Bench)
	return out, nil
}

// judge re-judges rows against a calibrated class's file. A row with a class
// ceiling is judged by it. A row whose only ceiling is Sengi's (ceilings.json)
// is not judged here: Sengi's number says nothing about this runner, so it is
// marked OK and named in unjudged. A row whose budget is its own test's (mcp,
// mirror, team, rank: SPEC budgets) keeps it. The probes stay a report.
func judge(rows []result, class, sengi ceilings.File) (out []result, unjudged []string) {
	for _, r := range rows {
		if c, ok := class.Bench[r.Name]; ok && !isProbe(r.Name) {
			r.Budget, r.OK = c.P95, r.P95 <= c.P95
		} else if _, ok := sengi.Bench[r.Name]; ok {
			r.OK = true
			unjudged = append(unjudged, r.Name)
		}
		out = append(out, r)
	}
	return out, unjudged
}

// calibratingNote is what a run of a class with no ceilings file says.
func calibratingNote(class, earlier string) string {
	n := "run count unknown"
	if k, err := strconv.Atoi(strings.TrimSpace(earlier)); err == nil && k >= 0 {
		n = fmt.Sprintf("%d runs so far", k+1)
	}
	return fmt.Sprintf("No verdict: calibrating %s: %s (this one included). %s has no ceilings file yet (%s), and Sengi's numbers say nothing about it. After about %d runs, propose one with scripts/ci/bench-calibrate.sh and commit it (runbook: docs/runbooks/zm-bench-vm.md in the private ops repo). This job stays green.",
		class, n, class, ceilings.ClassPath(class), calibrateMin)
}

// calibrateCmd proposes a class's ceilings file from its runs' bench.json:
// per row, the highest p95 x 1.6 + 1 ms, rounded up. issue-* and replica-*
// are capped at the 16 ms bar, and a row with its own test budget at that
// budget, so calibration never moves a SPEC number; a capped row is said on
// errw. Fewer than -min runs: refused, keep calibrating.
func calibrateCmd(args []string, w, errw io.Writer) error {
	fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	class := fs.String("class", ceilings.Dedicated, "the runner class (not sengi: ceilings.json stays Sengi's)")
	min := fs.Int("min", calibrateMin, "the fewest runs that make a proposal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *class == ceilings.Sengi || ceilings.ClassOf(*class) == "" {
		return fmt.Errorf("calibrate: -class %q is not a calibrated runner class (sengi's ceilings are ceilings.json)", *class)
	}
	if fs.NArg() < *min {
		return fmt.Errorf("calibrate: %d runs, need %d: keep calibrating", fs.NArg(), *min)
	}
	sengi, err := ceilings.Load()
	if err != nil {
		return err
	}
	worst := map[string]float64{}
	budget := map[string]float64{}
	seen := map[string]int{}
	for _, p := range fs.Args() {
		rows, err := readResults(p)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("calibrate: %s has no rows", p)
		}
		for _, r := range rows {
			if isProbe(r.Name) {
				continue
			}
			worst[r.Name] = math.Max(worst[r.Name], r.P95)
			budget[r.Name] = r.Budget
			seen[r.Name]++
		}
	}
	f := ceilings.File{Bench: map[string]ceilings.Row{}}
	for _, name := range slices.Sorted(maps.Keys(worst)) {
		if seen[name] < *min {
			fmt.Fprintf(errw, "calibrate: %s is in %d of %d runs, under %d: left out (not judged on %s)\n", name, seen[name], fs.NArg(), *min, *class)
			continue
		}
		c := math.Ceil(worst[name]*calibrateRatio + calibrateAddMS)
		limit, why := 0.0, ""
		if _, ok := sengi.Bench[name]; !ok {
			limit, why = budget[name], "its own test budget"
		} else if ceilings.OnBar(name) {
			limit, why = ceilings.BarMS, "the 16 ms bar"
		}
		if limit > 0 && c > limit {
			fmt.Fprintf(errw, "calibrate: %s: %g ms from p95 %.1f, capped at %g ms (%s); check it is not already over on %s\n", name, c, worst[name], limit, why, *class)
			c = limit
		}
		f.Bench[name] = ceilings.Row{P95: c}
	}
	f.Doc = fmt.Sprintf("%s's p95 ceilings in ms, proposed by `go run ./scripts/bench calibrate` from %d of its runs: the highest p95 x %g + %g ms, rounded up; issue-* and replica-* capped at the %d ms bar, rows with their own test budget capped at it. A bench row with no line here is not judged on %s. Only tightens from its first commit (bench tighten -class %s), like ceilings.json, which stays Sengi's (DESIGN-NOTES, The speed gate).",
		*class, fs.NArg(), calibrateRatio, calibrateAddMS, ceilings.BarMS, *class, *class)
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"_doc\": %s,\n  \"bench\": {\n", quote(f.Doc))
	names := slices.Sorted(maps.Keys(f.Bench))
	for i, name := range names {
		sep := ","
		if i == len(names)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    %s: {\"p95_ms\": %g}%s\n", quote(name), f.Bench[name].P95, sep)
	}
	b.WriteString("  }\n}\n")
	if _, err := ceilings.Parse([]byte(b.String())); err != nil {
		return fmt.Errorf("calibrate: the proposal does not parse: %w", err)
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// quote is a JSON string.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
