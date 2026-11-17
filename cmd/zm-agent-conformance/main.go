// Command zm-agent-conformance checks a zm-agent/1 adapter (SPEC 8
// "Structured sessions") against recordings of real sessions:
//
//	zm-agent-conformance check [--json] <adapter argv...> -- <recording>...
//	zm-agent-conformance record --scenario <name> --out <file> [--cwd dir] [--model m] <adapter argv...> -- <runtime argv...>
//	zm-agent-conformance scrub <in> <out> [<from>=<to>...]
//	zm-agent-conformance golden <driver> <recording>...
//
// check replays each recording against the adapter process, with a fake
// runtime (this binary) playing the recorded runtime side and refusing any
// line the recording does not have; the events must keep the protocol's
// rules and match the recording's golden file (<name>.events.jsonl) item by
// item. A built-in adapter is `zm __zm-agent claude`; a third party's is its
// own `zm-agent-<name>`. Exit 0 = conformant, 1 = a check failed, 2 = usage.
//
// record runs a scenario against a live runtime (it spends tokens on the
// login it finds); scrub removes paths, accounts and machine setup before a
// recording is committed; golden writes a built-in driver's events for a
// recording (the in-process replay). Owned by the structured-sessions lane.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/builtin"
	"github.com/sengirunners/mezze/internal/agentpipe/conformance"
)

func main() { os.Exit(run(os.Args[1:])) }

func split(args []string) ([]string, []string, bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: zm-agent-conformance check|record|scrub|golden ... (see go doc)")
	return 2
}

func run(args []string) int {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "check":
		return check(args[1:])
	case "record":
		return record(args[1:])
	case "scrub":
		return scrub(args[1:])
	case "golden":
		return golden(args[1:])
	}
	return usage()
}

func goldenPath(rec string) string {
	return strings.TrimSuffix(rec, ".rec.jsonl") + ".events.jsonl"
}

func check(args []string) int {
	asJSON := len(args) > 0 && args[0] == "--json"
	if asJSON {
		args = args[1:]
	}
	adapter, recs, ok := split(args)
	if !ok || len(adapter) == 0 || len(recs) == 0 {
		return usage()
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	type result struct {
		Recording string   `json:"recording"`
		OK        bool     `json:"ok"`
		Problems  []string `json:"problems,omitempty"`
	}
	var results []result
	failed := false
	for _, rec := range recs {
		_, entries, err := conformance.Load(rec)
		r := result{Recording: rec}
		if err != nil {
			r.Problems = append(r.Problems, err.Error())
		} else {
			events, problems, err := conformance.Run(adapter, []string{self, agentpipe.FakeArg}, rec, 2*time.Minute)
			if err != nil {
				problems = append(problems, err.Error())
			}
			problems = append(problems, conformance.Check(events, conformance.Ops(entries))...)
			if b, err := os.ReadFile(goldenPath(rec)); err == nil {
				want, err := conformance.ReadEvents(b)
				if err != nil {
					problems = append(problems, err.Error())
				}
				problems = append(problems, conformance.Loose(want, events)...)
			}
			r.Problems = problems
		}
		r.OK = len(r.Problems) == 0
		failed = failed || !r.OK
		results = append(results, r)
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(results)
	} else {
		for _, r := range results {
			if r.OK {
				fmt.Printf("PASS %s\n", r.Recording)
				continue
			}
			fmt.Printf("FAIL %s\n", r.Recording)
			for _, p := range r.Problems {
				fmt.Printf("  %s\n", p)
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

func record(args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	name := fs.String("scenario", "", "scenario name")
	out := fs.String("out", "", "recording file")
	cwd := fs.String("cwd", "", "the session's directory (default: a new temp dir)")
	model := fs.String("model", "", "model")
	timeout := fs.Duration("timeout", 4*time.Minute, "per wait")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	adapter, cli, ok := split(fs.Args())
	sc, found := conformance.ScenarioByName(*name)
	if !ok || !found || *out == "" || len(adapter) == 0 {
		return usage()
	}
	if *cwd == "" {
		d, err := os.MkdirTemp("", "zm-agent-rec")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*cwd = d
	}
	events, stderr, err := conformance.Record(adapter, cli, sc, *cwd, *model, *out, nil, *timeout)
	_ = json.NewEncoder(os.Stdout).Encode(events)
	if stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "record:", err)
		return 1
	}
	if bad := conformance.Check(events, nil); len(bad) > 0 {
		fmt.Fprintln(os.Stderr, "the live run breaks the protocol:\n  "+strings.Join(bad, "\n  "))
		return 1
	}
	return 0
}

func scrub(args []string) int {
	if len(args) < 2 {
		return usage()
	}
	head, entries, err := conformance.Load(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var repl [][2]string
	for _, kv := range args[2:] {
		from, to, ok := strings.Cut(kv, "=")
		if !ok || from == "" {
			return usage()
		}
		repl = append(repl, [2]string{from, to})
	}
	if err := os.WriteFile(args[1], conformance.WriteRecording(head, conformance.Scrub(entries, repl)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func golden(args []string) int {
	if len(args) < 2 {
		return usage()
	}
	code := 0
	for _, rec := range args[1:] {
		d, ok := agentpipe.New(args[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "no driver %q\n", args[0])
			return 2
		}
		_, entries, err := conformance.Load(rec)
		if err == nil {
			var events []agentpipe.Event
			events, err = conformance.Replay(d, entries)
			if err == nil {
				if bad := conformance.Check(events, conformance.Ops(entries)); len(bad) > 0 {
					err = fmt.Errorf("breaks the protocol:\n  %s", strings.Join(bad, "\n  "))
				}
			}
			if err == nil {
				err = os.WriteFile(goldenPath(rec), conformance.WriteEvents(events), 0o644)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", rec, err)
			code = 1
		}
	}
	return code
}
