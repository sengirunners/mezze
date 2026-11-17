package deps

// Machine-enforced brick boundaries, in the style of Tailscale's
// tstest/deptest: a plain Go test over `go list`, no linter. The rules
// (docs/ARCHITECTURE.md):
//
//   - a module (internal/<module>/...) imports only shared libraries and its
//     own packages, never another module: cross-module calls go through a
//     core interface or an event;
//   - shared libraries import only shared libraries;
//   - cmd/zm, internal/wiring and the listed scripts are the composition
//     roots and may link modules; every other cmd/* binary imports only
//     shared libraries plus what rootImports lists;
//   - the adapter protocol (work/adapter) links only shared libraries and
//     its own packages: adapters are processes speaking section 11;
//   - daemon-side modules do not import the client (potluck's kit/client,
//     clientPkg, and zm's calls over it, orchclientPkg): calling another
//     module's routes in process bypasses core.Deps (doors such as mcp are
//     clients and may).
//
// KIT and COMPUTE live in github.com/sengirunners/potluck:
// imports of potluck packages are not edges here (they are shared
// libraries to this module), except kit/client, kept as clientPkg for the
// client rule.
//
// Test files are not checked (a module's tests may start a daemon through
// potluck's kit/server). An exception goes in allowedEdges with an owner, a date
// and a one-line reason; any agent may add one in its own PR (no sign-off).
// The test fails once an entry is no longer needed.

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

const modPath = "github.com/sengirunners/mezze/"

// clientPkg is how potluck's kit/client appears among a package's imports.
const (
	potluckClient = "github.com/sengirunners/potluck/kit/client"
	clientPkg     = "potluck/kit/client"
	// orchclientPkg is zm's orchestration calls over the client (functions
	// that were kit/client methods): a client, under the same rule.
	orchclientPkg = "internal/orchclient"
)

// sharedLibs may be imported by any package. A new one is a maintainers' decision
// (docs/ARCHITECTURE.md, rule 2), not a way around a boundary.
var sharedLibs = map[string]bool{
	"internal/budget": true, "internal/deps": true, "internal/tools": true, "potluck/kit": true,
	// orchapi, orchcore and orchclient are the orchestration types, seams
	// and client calls leaving potluck's kit/api, kit/core and kit/client.
	// orchclient is also a client:
	// the client rule below applies to it.
	"internal/orchapi": true, "internal/orchcore": true, orchclientPkg: true,
	// judge is the one model-judge helper every module's meaning checks use
	// (owner direction): no state, no routes.
	"internal/judge": true,
}

// roots are the composition roots: they link modules together. Scripts are
// listed one by one, so a new script is checked like a module until added.
// internal/wiring holds the daemon's module lists.
var roots = []string{"cmd/zm", "internal/wiring", "scripts/bench", "scripts/loadtest", "scripts/loopnet", "scripts/release", "scripts/release-sign"}

// doors are client-side modules: they talk to the daemon through
// the client, which daemon-side modules may not import.
// internal/connect is zm-connect's bridge, a zm client run by the machine's
// owner.
var doors = map[string]bool{"internal/mcp": true, "internal/connect": true}

// adapterProtocol is the section 11 protocol package adapters build on.
const adapterProtocol = "internal/work/adapter"

// rootImports is what a non-root binary may import beyond shared libraries.
var rootImports = map[string][]string{
	"cmd/zm-work-conformance":  {"internal/work/adapter", "internal/work/conformance"},
	"cmd/zm-connect":           {"internal/connect/bridge", "internal/connect/dbrole"},
	"cmd/zm-agent-conformance": {"internal/agentpipe", "internal/agentpipe/builtin", "internal/agentpipe/conformance"},
}

// edge is an allowed exception: from imports to (package paths, repo-relative).
type edge struct {
	from, to   string
	owner, why string
	added      string // YYYY-MM-DD
}

// allowedEdges are known exceptions, each with an owner and the date it was
// allowed. Removing the import makes the entry fail until it is deleted too.
var allowedEdges = []edge{
	{from: "internal/work", to: "internal/rollup", owner: "work", added: "2026-11-03",
		why: "internal/rollup is a pure counting library with no state or routes, shared by the mirror's and the copy's rollup; a candidate for sharedLibs, the zm lead's call"},
	{from: "internal/work/rollupmirror", to: "internal/rollup", owner: "work", added: "2026-11-03",
		why: "the rollup driver over the mirror; the rules stay in one place for the mirror and the copy"},
	{from: "internal/team", to: "internal/rollup", owner: "team", added: "2026-11-03",
		why: "as internal/work: a replica answers GET /v0/work/rollup from its copy with the same rules, so it needs no tailnet call"},
	{from: "internal/ghlink", to: clientPkg, owner: "zm-hooks", added: "2026-11-01",
		why: "apply.go drives the work module's REST routes in process (client.WithHandler(d.Handler)); replace with a core.Deps method"},
	{from: "internal/ghlink", to: orchclientPkg, owner: "zm-hooks", added: "2026-11-10",
		why: "the same in-process calls as the kit/client edge, now functions in orchclient; goes with it"},
	{from: "internal/mcp", to: "internal/issues/imgsafe", owner: "integrations", added: "2026-11-12",
		why: "ask_new card.attach prepares a file as zm ask new --attach does (imgsafe.ForUpload, pure bytes in and out, no state); a candidate for sharedLibs"},
}

type goPkg struct {
	ImportPath                         string
	GoFiles                            []string // non-test files (none: a test-only package)
	Imports, TestImports, XTestImports []string
}

var (
	pkgsOnce sync.Once
	pkgs     []goPkg
	pkgsErr  error
)

// repoPackages is every package in the module with its direct imports,
// repo-relative ("internal/work"): non-test in Imports, its tests' (in and
// external package) in TestImports.
func repoPackages(t *testing.T) []goPkg {
	t.Helper()
	pkgsOnce.Do(func() {
		// Direct imports of every repo package are the edges; -deps would add
		// only the standard library and third-party closure.
		cmd := exec.Command("go", "list", "-json", "./...")
		cmd.Dir = "../.."
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				err = fmt.Errorf("%v: %s", err, ee.Stderr)
			}
			pkgsErr = err
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for {
			var p goPkg
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				pkgsErr = err
				return
			}
			if !strings.HasPrefix(p.ImportPath, modPath) {
				continue // the standard library and third-party packages
			}
			p.ImportPath = strings.TrimPrefix(p.ImportPath, modPath)
			own := func(imps ...[]string) []string {
				var out []string
				for _, l := range imps {
					for _, imp := range l {
						if strings.HasPrefix(imp, modPath) {
							out = append(out, strings.TrimPrefix(imp, modPath))
						} else if imp == potluckClient {
							out = append(out, clientPkg)
						}
					}
				}
				return out
			}
			p.Imports, p.TestImports, p.XTestImports = own(p.Imports), own(p.TestImports, p.XTestImports), nil
			pkgs = append(pkgs, p)
		}
	})
	if pkgsErr != nil {
		t.Fatalf("go list: %v", pkgsErr)
	}
	return pkgs
}

// brick is the top-level directory a package belongs to: internal/<x>,
// cmd/<x> or scripts/<x>.
func brick(path string) string {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		return path
	}
	return parts[0] + "/" + parts[1]
}

func under(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

func isRoot(path string) bool {
	for _, r := range roots {
		if under(path, r) {
			return true
		}
	}
	return false
}

func allowed(from, to string) *edge {
	for i := range allowedEdges {
		if allowedEdges[i].from == from && allowedEdges[i].to == to {
			return &allowedEdges[i]
		}
	}
	return nil
}

// violation is why from may not import to ("" = it may).
func violation(from, to string) string {
	fb, tb := brick(from), brick(to)
	switch {
	case isRoot(from):
		return ""
	case strings.HasPrefix(from, "cmd/"):
		if sharedLibs[tb] {
			return ""
		}
		for _, ok := range rootImports[fb] {
			if to == ok {
				return ""
			}
		}
		return fmt.Sprintf("%s is a binary, not a composition root: it imports shared libraries (and what rootImports lists) only; talk to the daemon through the client (potluck kit/client)", fb)
	case sharedLibs[fb]:
		if to == orchclientPkg && fb != orchclientPkg {
			return fmt.Sprintf("%s is a shared library and must not import the client calls %s", fb, to)
		}
		if sharedLibs[tb] {
			return ""
		}
		return fmt.Sprintf("%s is a shared library and must not depend on the module %s", fb, tb)
	}
	if under(from, adapterProtocol) {
		if under(to, adapterProtocol) || sharedLibs[tb] {
			return ""
		}
		return fmt.Sprintf("%s is the adapter protocol: it links only shared libraries and its own packages, never the daemon's work module (adapters are processes speaking SPEC section 11)", adapterProtocol)
	}
	if (to == clientPkg || to == orchclientPkg) && !doors[fb] {
		return fmt.Sprintf("%s runs in the daemon and must not import the client (potluck kit/client): calling routes in process bypasses core.Deps (doors such as internal/mcp are clients)", fb)
	}
	if sharedLibs[tb] || fb == tb {
		return ""
	}
	return fmt.Sprintf("%s is a module and must not import the module %s", fb, tb)
}

func TestImportBoundaries(t *testing.T) {
	owners := ownersByDir(t)
	used := map[*edge]bool{}
	var fails []string
	for _, p := range repoPackages(t) {
		for i, imp := range append(slices.Clip(p.Imports), p.TestImports...) {
			why := violation(p.ImportPath, imp)
			if why == "" || i >= len(p.Imports) { // tests are not checked
				continue
			}
			if e := allowed(p.ImportPath, imp); e != nil {
				used[e] = true
				continue
			}
			owner := func(b string) string {
				if o := owners[b]; o != "" {
					return o
				}
				return "unowned / zm lead"
			}
			fails = append(fails, fmt.Sprintf("%s imports %s: %s.\n    Fix: use a core interface (core.Deps) or an event instead; owners: %s (%s), %s (%s); or, if the edge is right, add it to allowedEdges in internal/deps/boundaries_test.go with a one-line reason; see docs/ARCHITECTURE.md.",
				p.ImportPath, imp, why, brick(p.ImportPath), owner(brick(p.ImportPath)), brick(imp), owner(brick(imp))))
		}
	}
	for i := range allowedEdges {
		if e := &allowedEdges[i]; !used[e] {
			fails = append(fails, fmt.Sprintf("allowedEdges lists %s -> %s (added %s, owner %s), but nothing needs it any more: delete the entry so the boundary holds.",
				e.from, e.to, e.added, e.owner))
		}
	}
	sort.Strings(fails)
	for _, f := range fails {
		t.Error(f)
	}
}

// The rule itself, on synthetic edges, so a refactor of violation() cannot
// quietly allow everything.
func TestBoundaryRule(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"internal/mail", "internal/tools", true},
		{"internal/mail", clientPkg, false},
		{"internal/mail", "internal/work", false},
		{"internal/issues/store/sqlite", "internal/issues/store", true},
		{"internal/issues", "internal/session", false},
		{"internal/tools", "internal/mail", false},
		{"internal/wiring", "internal/mail", true},
		{"cmd/zm", "internal/session", true},
		{"scripts/loadtest", "internal/team", true},
		{"scripts/newtool", "internal/team", false},
		{"internal/work/adapter", "internal/work", false},
		{"internal/work/adapter", "internal/tools", true},
		{"internal/issues", clientPkg, false},
		{"internal/mcp", clientPkg, true},
		{"internal/mail", orchclientPkg, false},
		{"internal/mcp", orchclientPkg, true},
		{"cmd/bd-zm", orchclientPkg, true},
		{"internal/wiring", orchclientPkg, true},
		{"internal/orchcore", orchclientPkg, false},
		{orchclientPkg, "internal/orchapi", true},
		{orchclientPkg, clientPkg, true},
		{orchclientPkg, "internal/mail", false},
		{"cmd/bd-zm", clientPkg, true},
		{"cmd/bd-zm", "internal/issues", false},
		{"cmd/zm-work-conformance", "internal/work/conformance", true},
		{"cmd/zm-work-conformance", "internal/work", false},
		{"cmd/zm-agent-conformance", "internal/agentpipe/conformance", true},
		{"cmd/zm-agent-conformance", "internal/work/adapter", false},
		{"internal/work", "internal/work/adapter", true},
		{"internal/work", "internal/work/home", true},
	} {
		if got := violation(c.from, c.to) == ""; got != c.ok {
			t.Errorf("%s -> %s: allowed=%v, want %v (%s)", c.from, c.to, got, c.ok, violation(c.from, c.to))
		}
	}
}
