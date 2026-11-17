package deps

// Potluck layers (DESIGN DECISIONS G1). KIT (the shared base)
// and COMPUTE (run targets and their drivers) live in the potluck module,
// github.com/sengirunners/potluck, whose own test holds them
// to their rule (KIT imports no COMPUTE). Everything in this repository is
// ORCH, zm's orchestration, which may import anything; a COMPUTE package
// kept here would be listed in computeHere (none is: cmd/zm-run-farm drives
// zm tasks through hub and is orchestration-side). A
// package under a directory that moved to potluck fails: it belongs there.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// movedToPotluck are the directories that moved to sengirunners/potluck:
// kit/<name>, or the top-level compute packages.
var movedToPotluck = []string{
	"internal/ids", "internal/config", "internal/secrets", "internal/store", "internal/events", "internal/authz",
	"internal/netguard", "internal/sqlbackup", "internal/release", "internal/swap", "internal/tailnet",
	"internal/tunnel", "internal/owners", "internal/project", "internal/core", "internal/api", "internal/client",
	"internal/server", "internal/teamlink", "internal/runtarget", "internal/runners", "internal/usage",
	"internal/hostprobe", "internal/wiring/base", "cmd/zm-e2b",
}

// computeHere are COMPUTE packages that stay in this repository: they may
// import potluck, never a package of this repository (all ORCH). Empty since
// cmd/zm-run-farm became orchestration-side: it starts zm tasks, whose types are orchestration's.
var computeHere []string

func isMoved(path string) bool {
	for _, d := range movedToPotluck {
		if under(path, d) {
			return true
		}
	}
	return false
}

func isComputeHere(path string) bool {
	for _, d := range computeHere {
		if under(path, d) {
			return true
		}
	}
	return false
}

// layerViolation is why from may not import to ("" = it may). Paths are
// repo-relative; potluck packages appear as "potluck/...".
func layerViolation(from, to string) string {
	if isComputeHere(from) && !strings.HasPrefix(to, "potluck/") {
		return fmt.Sprintf("%s is COMPUTE and imports %s (ORCH): it may import potluck packages only", from, to)
	}
	return ""
}

func TestLayers(t *testing.T) {
	var fails []string
	seen := map[string]bool{}
	for _, p := range repoPackages(t) {
		if isMoved(p.ImportPath) {
			fails = append(fails, fmt.Sprintf("%s is under a directory that moved to sengirunners/potluck: add it there", p.ImportPath))
		}
		for _, d := range computeHere {
			if under(p.ImportPath, d) {
				seen[d] = true
			}
		}
		for _, imp := range append(append([]string(nil), p.Imports...), p.TestImports...) {
			if why := layerViolation(p.ImportPath, imp); why != "" {
				fails = append(fails, why)
			}
		}
	}
	for _, d := range computeHere {
		if !seen[d] {
			fails = append(fails, fmt.Sprintf("computeHere lists %s, which is gone: delete the row", d))
		}
	}
	sort.Strings(fails)
	for _, f := range fails {
		t.Error(f)
	}
}

// The rule on synthetic edges, so a refactor cannot quietly allow everything.
func TestLayerRule(t *testing.T) {
	// computeHere is empty today; the rule is still checked on a stand-in.
	defer func(old []string) { computeHere = old }(computeHere)
	computeHere = []string{"cmd/example-compute"}
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"cmd/example-compute", "potluck/kit/client", true},
		{"cmd/example-compute", "internal/team", false},
		{"cmd/example-compute", "internal/wiring", false},
		{"cmd/zm-run-farm", "internal/orchapi", true},
		{"cmd/zm", "internal/mail", true},
		{"internal/team", "potluck/kit/client", true},
	} {
		if got := layerViolation(c.from, c.to) == ""; got != c.ok {
			t.Errorf("%s -> %s: allowed=%v, want %v", c.from, c.to, got, c.ok)
		}
	}
	for _, c := range []struct {
		path  string
		moved bool
	}{
		{"internal/server", true}, {"internal/client/boxcp", true}, {"internal/wiring/base", true},
		{"cmd/zm-e2b", true}, {"internal/wiring", false}, {"internal/wiring/fulltest", false},
		{"internal/team", false}, {"internal/serverless", false},
	} {
		if got := isMoved(c.path); got != c.moved {
			t.Errorf("isMoved(%s) = %v, want %v", c.path, got, c.moved)
		}
	}
}
