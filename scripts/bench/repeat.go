package main

// `bench repeat`: whether a run's timing counts. Shared runners stall, so a row
// over its ceiling in one run only warns; the same row over in two finished main
// runs in a row is a real slowdown and reds bench.yml. Timing never touches
// ci.yml. rank/* rows (TestRankedSearchSpeed, ZM_RANK_JSON) follow the same rule.
//
// Only a Sengi runner gives a verdict: the ceilings came from Sengi.
// On 43 main runs on the Factory pool with no change to the CLI paths, `zm
// --version` p95 went from 7 to 51 ms and the rows over went with it, so two
// slow VMs in a row made a red. A run elsewhere lists its rows over for the
// record, gives no verdict, and is never the first of two (bench.yml passes only
// a Sengi run as -prev).
//
// "In a row" is bounded by age: the previous Sengi run must have
// started within -pair-max (48 h) of now, or it is not a pair. Pushes route to
// Factory, so the Sengi run before this one can be the nightly of days ago,
// with many changes between them; two slow days far apart prove nothing.
// bench-baseline.sh passes its created_at as -prev-at.
//
// A contended Sengi VM is no evidence either: runner/probe, an empty
// Go binary's start that no zm change can slow, gives the VM away. It is timed
// before the rows and again after the last one (runner/probe-late), and the
// worse reading counts. When this run's or the previous run's is above
// -contended, a row over twice gives no verdict: the rows over are listed and
// the job stays green.
// bench-quiet.sh cannot see this: it waits only for this repo's runs, not the
// VM's other tenants (it reported quiet after 2 s on the 2026-11-15 red).
//
// zm's own bench machine (a pool box or zm-bench) is another verdict
// class (class.go): judged against its own ceilings.<class>.json, rows and the
// previous run alike, and the previous run is of the same class. While that
// file does not exist it is calibrating: no verdict, a count of its runs.

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

// pairMax bounds "twice in a row": an older previous run is not a pair.
const pairMax = 48 * time.Hour

// clock is time.Now, swapped in tests.
var clock = time.Now

// sengiLabel prefixes the runs-on label of a Sengi runner (runner-route.yml).
const sengiLabel = "sengi-"

// probeRow times the start of an empty Go binary (main.go -probe), before the
// rows; lateProbeRow times it again after every timed row (bench probe, run by
// bench.yml after its last timing step). Never a row over themselves; on Sengi
// the worse of the two marks a contended run (contendedP95).
const (
	probeRow     = "runner/probe"
	lateProbeRow = "runner/probe-late"
)

// isProbe says whether a row is a runner probe, not a zm row.
func isProbe(name string) bool { return name == probeRow || name == lateProbeRow }

// contendedP95 is runner/probe's p95 (ms) above which a Sengi run is contended.
// Sengi runs 2026-11-13..15: the 8 with 0-1 rows over measured
// 0.91-1.23; 8 of the 10 with 7-26 rows over measured 1.31-5.91, and every
// noise red had one of those in its pair. The one red on two quiet VMs
// (zero-start 193 and 195 ms over 155) was a real slowdown, and still reds.
// Few samples and a thin margin: recalibrate from more Sengi runs. Two noisy
// runs had quiet early probes (0.99 and 1.05, 12 and 18 rows over, from
// hook-stop on): the VM slowed after it, hence the late probe, judged
// by the same limit until Sengi runs measure it. scripts/ci/bench-baseline.sh
// keeps a copy (CONTENDED_P95; a test holds them equal).
const contendedP95 = 1.3

// worstProbe is a run's worse probe reading: the row and its p95, or "" and 0
// when it has none (artifacts before runner/probe).
func worstProbe(rs []result) (name string, p95 float64) {
	for _, r := range rs {
		if isProbe(r.Name) && (name == "" || r.P95 > p95) {
			name, p95 = r.Name, r.P95
		}
	}
	return name, p95
}

// rankPrefix marks a ranked-search row: its ceiling is the 16 ms bar in
// TestRankedSearchSpeed, not a line of ceilings.json.
const rankPrefix = "rank/"

// repeated splits now's rows that are over their ceilings: over in prev too
// (twice), or only now (once).
func repeated(prev, now []result) (once, twice []result) {
	wasOver := map[string]bool{}
	for _, r := range prev {
		wasOver[r.Name] = !r.OK
	}
	for _, r := range now {
		switch {
		case r.OK || isProbe(r.Name):
		case wasOver[r.Name]:
			twice = append(twice, r)
		default:
			once = append(once, r)
		}
	}
	return once, twice
}

// repeatCmd writes the verdict for the job summary. Exit 1: main's job goes red.
// A previous run that is missing or unreadable has nothing to repeat: the rule
// needs two runs, and a lost artifact is no evidence of a slowdown.
func repeatCmd(args []string, w io.Writer) (int, error) {
	fs := flag.NewFlagSet("repeat", flag.ContinueOnError)
	prevPath := fs.String("prev", "", "the bench.json of the last finished main run (missing: nothing can repeat)")
	warn := fs.Bool("warn", false, "never exit 1: timing only warns (PRs, dispatches)")
	runner := fs.String("runner", "", "this run's runs-on label and runner name; not "+sengiLabel+"* or zm-bench: no verdict (empty: not checked)")
	classRuns := fs.String("class-runs", "", "earlier main runs of this runner class (bench-baseline.sh), counted while the class calibrates")
	prevAt := fs.String("prev-at", "", "when the -prev run started (RFC 3339); older than -pair-max: not a pair (empty: not checked)")
	maxGap := fs.Duration("pair-max", pairMax, "the oldest -prev run that still makes a pair")
	busy := fs.Float64("contended", contendedP95, "the worse runner/probe p95 (ms) above which a Sengi run, this one or -prev, is contended: no verdict")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 1 {
		return 0, fmt.Errorf("usage: bench repeat [-warn] [-runner label] [-prev main.json] bench.json")
	}
	now, err := readResults(fs.Arg(0))
	if err != nil {
		return 0, err
	}
	if len(now) == 0 {
		return 0, fmt.Errorf("%s has no rows", fs.Arg(0))
	}
	var prev []result
	noPrev := ""
	if *prevPath == "" {
		noPrev = "no path given"
	} else if prev, err = readResults(*prevPath); err != nil {
		prev, noPrev = nil, err.Error()
	} else if *prevAt != "" {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*prevAt))
		if err != nil {
			return 0, fmt.Errorf("-prev-at: %w", err)
		}
		if age := clock().Sub(at); age > *maxGap {
			prev, noPrev = nil, fmt.Sprintf("it started %s ago, over the %s bound, so it is not a pair", age.Round(time.Hour), *maxGap)
		}
	}
	fmt.Fprintln(w, "## Timing: is a row over twice in a row?")
	fmt.Fprintln(w)
	class := ceilings.Sengi // no -runner: not checked
	if *runner != "" {
		class = ceilings.ClassOf(*runner)
	}
	if class == "" {
		once, twice := repeated(prev, now)
		fmt.Fprintf(w, "No verdict: not a Sengi runner (%s), nor the dedicated zm-bench VM. The ceilings come from Sengi, so this run's timing says nothing about zm and is never the first of two. This job stays green.\n\n", *runner)
		if len(once)+len(twice) > 0 {
			fmt.Fprintln(w, "Over on this runner (for the record):")
			writeOver(w, append(once, twice...))
		}
		return 0, nil
	}
	if class != ceilings.Sengi {
		cf, ok, err := loadClass(class)
		if err != nil {
			return 0, err
		}
		if !ok {
			fmt.Fprintf(w, "%s\n", calibratingNote(class, *classRuns))
			return 0, nil
		}
		sengi, err := ceilings.Load()
		if err != nil {
			return 0, err
		}
		var unjudged []string
		now, unjudged = judge(now, cf, sengi)
		prev, _ = judge(prev, cf, sengi)
		fmt.Fprintf(w, "Judged against %s's own ceilings (%s), not Sengi's.\n\n", class, ceilings.ClassPath(class))
		if len(unjudged) > 0 {
			fmt.Fprintf(w, "No %s ceiling, so not judged: %s (propose them with bench calibrate).\n\n", class, strings.Join(unjudged, ", "))
		}
	}
	once, twice := repeated(prev, now)
	if len(twice) > 0 && class == ceilings.Sengi {
		if note := contendedNote(now, prev, *busy); note != "" {
			fmt.Fprintf(w, "No verdict: %s A contended VM times its neighbours, not zm, so this pair proves nothing. This job stays green.\n\n", note)
			fmt.Fprintln(w, "Over on this run (for the record):")
			writeOver(w, append(once, twice...))
			return 0, nil
		}
	}
	if noPrev != "" {
		fmt.Fprintf(w, "No bench from the last finished main run (%s), so nothing can repeat.\n\n", noPrev)
	}
	if len(once)+len(twice) == 0 {
		fmt.Fprintln(w, "No row over its ceiling.")
		return 0, nil
	}
	if len(once) > 0 {
		fmt.Fprintln(w, "Over in this run only, so this is a warning (one noisy run proves nothing):")
		writeOver(w, once)
	}
	if len(twice) > 0 {
		fmt.Fprintln(w, "Over in this run and in the last finished main run:")
		writeOver(w, twice)
		fmt.Fprintf(w, "%s\n", overFix)
	}
	if *warn || len(twice) == 0 {
		fmt.Fprintln(w, "This job stays green: timing only reds main, on the same row twice in a row.")
		return 0, nil
	}
	return 1, nil
}

// contendedNote says which run of the pair was contended, or "" when neither was.
func contendedNote(now, prev []result, limit float64) string {
	var hot []string
	if row, p := worstProbe(now); p > limit {
		hot = append(hot, fmt.Sprintf("this run's %s p95 is %.2f ms", row, p))
	}
	if row, p := worstProbe(prev); p > limit {
		hot = append(hot, fmt.Sprintf("the last finished main run's %s p95 is %.2f ms", row, p))
	}
	if len(hot) == 0 {
		return ""
	}
	return fmt.Sprintf("contended runner: %s, over %g ms (a quiet Sengi VM measures about 1).", strings.Join(hot, " and "), limit)
}

func writeOver(w io.Writer, rows []result) {
	for _, r := range rows {
		fmt.Fprintln(w, overLine(r))
	}
	fmt.Fprintln(w)
}

func overLine(r result) string {
	return fmt.Sprintf("- **%s**: p95 %.1f ms, ceiling %g ms", r.Name, r.P95, r.Budget)
}
