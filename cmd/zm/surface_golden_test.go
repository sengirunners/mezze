package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The command surface: every command path of zm with its aliases, whether it
// is hidden or deprecated, whether it parses no flags (a stub that hands its
// arguments to another program), and each flag it defines (name, shorthand,
// type, default, value when given bare, required, hidden). No help text: it
// churns. The golden file guards the plan that replaces zm's copies of the
// compute commands with Potluck's: a PR that changes the surface lists each
// change.
//
// Refresh after an intended change:
//
//	go test -p 2 ./cmd/zm -run TestCommandSurface -update-surface
var updateSurface = flag.Bool("update-surface", false, "rewrite cmd/zm/testdata/surface.golden from this run")

const surfaceGolden = "testdata/surface.golden"

// surfaceLines renders the tree under root as sorted lines, one per item:
//
//	cmd <path> [hidden] [noflagparse] [deprecated]
//	alias <path> <alias>
//	flag <path> --name [-s] type=<type> default=<default> [noopt=<value>] [required] [hidden]
//
// Flags are the ones a command defines itself (local and its own persistent
// ones), so an inherited flag appears once, at the command that declares it.
func surfaceLines(root *cobra.Command) []string {
	// Cobra adds help, completion and the -h/--version flags on first use (an
	// earlier test's Execute, or the user's run); add them now so the result
	// does not depend on which test ran first.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	var lines []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.InitDefaultHelpFlag()
		path := c.CommandPath()
		line := "cmd " + path
		if c.Hidden {
			line += " hidden"
		}
		if c.DisableFlagParsing {
			line += " noflagparse"
		}
		if c.Deprecated != "" {
			line += " deprecated"
		}
		lines = append(lines, line)
		for _, a := range c.Aliases {
			lines = append(lines, fmt.Sprintf("alias %s %s", path, a))
		}
		c.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
			l := fmt.Sprintf("flag %s --%s", path, f.Name)
			if f.Shorthand != "" {
				l += " -" + f.Shorthand
			}
			l += fmt.Sprintf(" type=%s default=%q", f.Value.Type(), f.DefValue)
			if f.NoOptDefVal != "" && f.Value.Type() != "bool" { // a bool is always bare-able
				l += fmt.Sprintf(" noopt=%q", f.NoOptDefVal)
			}
			if _, req := f.Annotations[cobra.BashCompOneRequiredFlag]; req {
				l += " required"
			}
			if f.Hidden {
				l += " hidden"
			}
			lines = append(lines, l)
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	sort.Strings(lines)
	return lines
}

func TestCommandSurface(t *testing.T) {
	groupCommands(rootCmd) // as main does
	got := strings.Join(surfaceLines(rootCmd), "\n") + "\n"
	if *updateSurface {
		if err := os.MkdirAll(filepath.Dir(surfaceGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(surfaceGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(surfaceGolden)
	if err != nil {
		t.Fatalf("%v\nwrite it with: go test -p 2 ./cmd/zm -run TestCommandSurface -update-surface", err)
	}
	want := string(raw)
	if got == want {
		return
	}
	added, removed := surfaceDiff(strings.Split(want, "\n"), strings.Split(got, "\n"))
	var b strings.Builder
	b.WriteString("zm's command surface changed (paths, aliases, flags).\n")
	for _, l := range removed {
		b.WriteString("  - " + l + "\n")
	}
	for _, l := range added {
		b.WriteString("  + " + l + "\n")
	}
	b.WriteString("If intended, list each change in the PR and refresh with:\n  go test -p 2 ./cmd/zm -run TestCommandSurface -update-surface\n")
	t.Error(b.String())
}

// surfaceDiff: the lines only in got (added) and only in want (removed).
func surfaceDiff(want, got []string) (added, removed []string) {
	in := func(set []string) map[string]bool {
		m := make(map[string]bool, len(set))
		for _, l := range set {
			m[l] = true
		}
		return m
	}
	w, g := in(want), in(got)
	for _, l := range got {
		if l != "" && !w[l] {
			added = append(added, l)
		}
	}
	for _, l := range want {
		if l != "" && !g[l] {
			removed = append(removed, l)
		}
	}
	return added, removed
}
