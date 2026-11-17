package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/scripts/ceilings"
)

func samples(d time.Duration, n int) []time.Duration {
	ds := make([]time.Duration, n)
	for i := range ds {
		ds[i] = d
	}
	return ds
}

func TestCheckCeilings(t *testing.T) {
	rows := map[string]ceilings.Row{"claim": {P95: 100}, "work ls": {P95: 50}, "notice": {P95: 15}}
	ds := map[string][]time.Duration{
		"claim":   samples(150*time.Millisecond, 100), // over
		"work ls": samples(50*time.Millisecond, 100),  // at the ceiling: fine
		// notice ran nothing: the load stopped reaching it.
		"during disruption: claim": samples(time.Hour, 100), // never judged
		"ack":                      samples(time.Hour, 100), // no ceiling
	}
	for _, budgets := range []bool{true, false} {
		t.Run(fmt.Sprintf("budgets=%t", budgets), func(t *testing.T) {
			fails, warnings := checkCeilings(ds, rows, budgets)
			if budgets {
				if len(fails) != 2 || !strings.HasPrefix(fails[0], "claim p95 150.00 ms over its ceiling 100 ms") || !strings.HasPrefix(fails[1], "notice ran no samples") || len(warnings) != 0 {
					t.Fatalf("failures %q, warnings %q", fails, warnings)
				}
			} else if len(fails) != 1 || !strings.HasPrefix(fails[0], "notice ran no samples") || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "claim p95 150.00 ms over its ceiling 100 ms") {
				t.Fatalf("failures %q, warnings %q", fails, warnings)
			}
		})
	}
}

func TestLoadResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budgets bool
		missing bool
		lost    bool
		wantErr bool
	}{
		{name: "advisory timing only"},
		{name: "strict timing", budgets: true, wantErr: true},
		{name: "advisory missing samples", missing: true, wantErr: true},
		{name: "advisory lost writes", lost: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := map[string][]time.Duration{"claim": samples(150*time.Millisecond, 100)}
			rows := map[string]ceilings.Row{"claim": {P95: 100}}
			if tc.missing {
				rows["notice"] = ceilings.Row{P95: 15}
			}
			fails, warnings := checkCeilings(ds, rows, tc.budgets)
			if tc.lost {
				fails = append(fails, "1 writes lost")
			}
			var b strings.Builder
			err := writeResult(&b, fails, warnings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v, report %s", err, b.String())
			}
			report := b.String()
			if strings.Contains(report, "WARNING (latency only): claim p95") == tc.budgets || strings.Contains(report, "PASS") == tc.wantErr {
				t.Fatalf("report %s", report)
			}
			if tc.missing && !strings.Contains(report, "notice ran no samples") {
				t.Fatalf("missing coverage not reported: %s", report)
			}
			if tc.lost && !strings.Contains(report, "1 writes lost") {
				t.Fatalf("lost writes not reported: %s", report)
			}
		})
	}
}

// Every gated operation's name must be one the load test records: a typo
// would fail every run with "ran no samples", and this says why at once.
func TestCeilingsNameRealOperations(t *testing.T) {
	f, err := ceilings.Load()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{"claim": true, "comment": true, "status": true, "release": true, "send": true, "inbox": true, "ack": true,
		"work ls": true, "work show": true, "notice": true, "presence": true, "propagation (replica to replica)": true}
	for op := range f.Loadtest {
		if !known[op] {
			t.Errorf("%s in %s is not an operation of the load test", op, ceilings.Path)
		}
	}
}
