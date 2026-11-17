package main

// `bench compare` and `bench tighten`: what CI runs around the measurement.
// compare only reports. bench repeat (repeat.go) decides what a run over its
// ceilings means; tighten is the one check here that reads a file and times nothing.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

// A row is called out when it moved this much on both scales: a ratio alone
// flags a 0.2 ms row that went to 0.4.
const (
	slowerRatio = 1.5
	slowerMS    = 1.0
	// roomRatio is the headroom past which a row is offered a tighter ceiling.
	roomRatio = 4.0
)

func subcommand(args []string) int {
	var err error
	code := 0
	switch args[0] {
	case "compare":
		err = compareCmd(args[1:])
	case "tighten":
		code, err = tightenCmd(args[1:], os.Stdout)
	case "repeat":
		code, err = repeatCmd(args[1:], os.Stdout)
	case "calibrate":
		err = calibrateCmd(args[1:], os.Stdout, os.Stderr)
	case "diagnose-send":
		err = diagnoseSendCmd(args[1:])
	case "probe":
		err = probeCmd(args[1:], os.Stdout)
	default:
		err = fmt.Errorf("unknown subcommand %q (compare, tighten, repeat, calibrate, probe)", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	return code
}

// tightenCmd fails when the embedded ceilings give away anything the file
// named in args (main's) had, without a reason.
// With -class, the files are that runner class's (ceilings.<class>.json): a
// class file, once committed, only tightens too, and deleting it is a loosening.
func tightenCmd(args []string, w io.Writer) (int, error) {
	fs := flag.NewFlagSet("tighten", flag.ContinueOnError)
	class := fs.String("class", ceilings.Sengi, "the runner class whose file this is")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 1 {
		return 0, fmt.Errorf("usage: bench tighten [-class c] <main's ceilings file>")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return 0, err
	}
	base, err := ceilings.Parse(b)
	if err != nil {
		return 0, err
	}
	now, ok, err := loadClass(*class)
	if err != nil {
		return 0, err
	}
	if !ok { // gone: every row was given away
		now = ceilings.File{}
	}
	lines := now.LoosenedAt(base, ceilings.ClassPath(*class))
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	if len(lines) > 0 {
		return 1, nil
	}
	fmt.Fprintln(w, "ceilings: nothing loosened without a reason")
	return 0, nil
}

func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	basePath := fs.String("base", "", "main's bench.json (none: this run alone)")
	baseNote := fs.String("base-note", "", "said of the base when it is not a Sengi run, e.g. its runner (other-runner data)")
	runner := fs.String("runner", "", "this run's runs-on label and runner name; not "+sengiLabel+"* or zm-bench, against a Sengi base: labelled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: bench compare [-base main.json] bench.json")
	}
	now, err := readResults(fs.Arg(0))
	if err != nil {
		return err
	}
	var base []result
	if *basePath != "" {
		if base, err = readResults(*basePath); err != nil {
			return err
		}
	}
	compare(os.Stdout, base, now, *baseNote, *runner)
	return nil
}

func readResults(path string) ([]result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs []result
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rs, nil
}

// compare writes the job summary: what moved against main, what is over, and
// the whole table folded away. base may be empty; a baseNote marks it as timed
// on another runner than Sengi, so only a rough guide. A runner that is not
// Sengi against a Sengi base (no baseNote) labels "slower": that is mostly the
// runner.
func compare(w io.Writer, base, now []result, baseNote, runner string) {
	was := map[string]result{}
	for _, r := range base {
		was[r.Name] = r
	}
	var over, slower, room []string
	var probe []string
	var all strings.Builder
	all.WriteString("| row | n | p50 ms | p95 ms | main p95 ms | ceiling ms |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range now {
		b, ok := was[r.Name]
		mainP95 := "-"
		if ok {
			mainP95 = fmt.Sprintf("%.1f", b.First)
		}
		if isProbe(r.Name) { // a report, not a zm row: never over, slower or room
			fmt.Fprintf(&all, "| %s | %d | %.1f | %.1f | %s | report only |\n", r.Name, r.N, r.P50, r.P95, mainP95)
			when := "before the rows"
			if r.Name == lateProbeRow {
				when = "after the last"
			}
			probe = append(probe, fmt.Sprintf("%s (%s) p50 %.1f ms, p95 %.1f ms", r.Name, when, r.P50, r.P95))
			continue
		}
		fmt.Fprintf(&all, "| %s | %d | %.1f | %.1f | %s | %g |\n", r.Name, r.N, r.P50, r.P95, mainP95, r.Budget)
		switch {
		case !r.OK:
			over = append(over, overLine(r))
		case ok && r.First >= b.First*slowerRatio && r.First-b.First >= slowerMS:
			slower = append(slower, fmt.Sprintf("- %s: p95 %.1f ms, main %.1f ms (%.1fx)", r.Name, r.First, b.First, r.First/b.First))
		}
		if r.P95 > 0 && r.Budget/r.P95 >= roomRatio && !strings.HasPrefix(r.Name, rankPrefix) {
			room = append(room, fmt.Sprintf("%s (%.1f of %g)", r.Name, r.P95, r.Budget))
		}
	}
	fmt.Fprintln(w, "## Latency (make bench)")
	fmt.Fprintln(w)
	if len(base) > 0 && baseNote != "" {
		fmt.Fprintf(w, "No Sengi bench from main to compare with, so main's last finished run stands in: other-runner data (%s), a rough guide only.\n\n", baseNote)
	}
	switch {
	case len(base) == 0:
		fmt.Fprintln(w, "No bench from main to compare with yet.")
	case len(slower) == 0:
		fmt.Fprintln(w, "No row moved much against main's last run.")
	// A verdict class's base is its own class's run (bench-baseline.sh): zm-bench against zm-bench is not labelled.
	case runner != "" && ceilings.ClassOf(runner) == "" && baseNote == "":
		fmt.Fprintf(w, "Slower than main's last Sengi run, but this run timed on another runner (%s): runner against Sengi, mostly the runner, not zm (a report, not a failure):\n", runner)
		fmt.Fprintln(w, strings.Join(slower, "\n"))
	default:
		fmt.Fprintln(w, "Slower than main's last run (a report, not a failure):")
		fmt.Fprintln(w, strings.Join(slower, "\n"))
	}
	if len(probe) > 0 {
		fmt.Fprintf(w, "\nAn empty Go binary's start on this runner: %s. Never over itself; on Sengi, the worse p95 above %g ms marks the run contended, and bench repeat then gives no verdict.\n",
			strings.Join(probe, "; "), contendedP95)
	}
	if len(over) > 0 {
		fmt.Fprintf(w, "\nOver a ceiling:\n%s\n", strings.Join(over, "\n"))
	}
	if len(room) > 0 {
		fmt.Fprintf(w, "\nRoom to tighten in %s (seen over a few runs, lower p95_ms with no reason needed): %s.\n", ceilings.Path, strings.Join(room, ", "))
	}
	fmt.Fprintf(w, "\n<details><summary>All rows</summary>\n\n%s\n</details>\n", all.String())
}
