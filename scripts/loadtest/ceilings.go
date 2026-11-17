package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/scripts/ceilings"
	"github.com/sengirunners/potluck/kit/api"
)

// ceilingFix is what to do about an operation over its ceiling.
const ceilingFix = "Investigate the slow path by comparing this table with other Sengi runs; a shared VM can stall. " +
	"Ceilings in " + ceilings.Path + " only tighten."

// checkCeilings judges steady state only: during a disruption replicas queue
// and retry by design. Missing samples always fail; disabling budgets makes
// latency advisory, without hiding a load that no longer reaches an operation.
// ds is sorted.
func checkCeilings(ds map[string][]time.Duration, rows map[string]ceilings.Row, budgets bool) (fails, warnings []string) {
	for op, c := range rows {
		samples := ds[op]
		if len(samples) == 0 {
			fails = append(fails, fmt.Sprintf("%s ran no samples: the load no longer reaches it (fix the load test)", op))
			continue
		}
		if p := pct(samples, .95); float64(p.Microseconds())/1000 > c.P95 {
			msg := fmt.Sprintf("%s p95 %s ms over its ceiling %g ms", op, ms(p), c.P95)
			if budgets {
				fails = append(fails, msg)
			} else {
				warnings = append(warnings, msg)
			}
		}
	}
	sort.Strings(fails)
	sort.Strings(warnings)
	return fails, warnings
}

// writeResult reports timing warnings even when a correctness check failed.
// Its return value determines the process exit status.
func writeResult(b *strings.Builder, fails, warnings []string) error {
	if len(warnings) > 0 {
		fmt.Fprintf(b, "\nWARNING (latency only): %s\n\n%s\n", strings.Join(warnings, "; "), ceilingFix)
	}
	if len(fails) > 0 {
		fmt.Fprintf(b, "\nFAILED: %s\n", strings.Join(fails, "; "))
		return errors.New("checks failed")
	}
	b.WriteString("\nPASS\n")
	return nil
}

// replicaState is a stuck replica's status, for the failure line.
func replicaState(rs []api.Remote) string {
	if len(rs) != 1 || rs[0].Replica == nil {
		return fmt.Sprintf("%d remotes", len(rs))
	}
	return fmt.Sprintf("%+v", *rs[0].Replica)
}

// dumpGoroutines keeps every daemon's goroutines next to the run's other
// files (all daemons are in this process): a stuck replica says where.
func dumpGoroutines() {
	f, err := os.OpenFile("loadtest-goroutines.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = pprof.Lookup("goroutine").WriteTo(f, 2)
}
