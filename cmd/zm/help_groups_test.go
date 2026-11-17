package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// TestHelpGroups: zm --help lists its verbs in sections, every verb a group
// names exists, and the top-level help carries no internal references.
func TestHelpGroups(t *testing.T) {
	groupCommands(rootCmd)
	names := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		names[c.Name()] = true
	}
	for _, g := range helpGroups {
		for _, v := range g.verbs {
			if !names[v] && v != "doctor" { // doctor lands in its own change
				t.Errorf("group %s names %q, which is not a zm verb", g.id, v)
			}
		}
	}
	usage := rootCmd.UsageString()
	last := -1
	for _, g := range helpGroups {
		i := strings.Index(usage, "\n"+g.title+"\n")
		if i < 0 {
			t.Fatalf("zm --help has no %q section:\n%s", g.title, usage)
		}
		if i < last {
			t.Errorf("section %q is out of order", g.title)
		}
		last = i
	}
	internal := regexp.MustCompile(`SPEC [0-9]|\bZ3\b|\bbeads\b|Potluck's|Pacific|40 of these`)
	for _, c := range rootCmd.Commands() {
		if c.Hidden {
			continue
		}
		if m := internal.FindString(c.Short); m != "" {
			t.Errorf("zm %s --help short line names %q: %s", c.Name(), m, c.Short)
		}
	}
	if m := internal.FindString(usage); m != "" {
		t.Errorf("zm --help names %q", m)
	}
}

// TestAskNewNamesMissingFlags: a bare zm ask new 'Merge it?' names every flag
// a card needs and both forms of the verb, as a usage error (exit 2).
func TestAskNewNamesMissingFlags(t *testing.T) {
	err := cardFlagsMissing(orchapi.AskCard{Question: "Merge it?"})
	var ew exitWith
	if !errors.As(err, &ew) || ew.code != 2 {
		t.Fatalf("got %v, want a usage error", err)
	}
	msg := ew.err.Error()
	for _, want := range []string{"--context", "--choice (2 to 4, got 0)", "--recommend", "--why", "--searched", "--repo", "--pr", "--head", "--work"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	// Through the verb itself, with no daemon: refused before any call.
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_NO_AUTOSTART", "1")
	cmd := askCmd()
	cmd.SetArgs([]string{"new", "Merge it?"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err = cmd.Execute()
	if !errors.As(err, &ew) || ew.code != 2 || !strings.Contains(ew.err.Error(), "--searched") {
		t.Fatalf("zm ask new 'Merge it?': %v, want the usage error", err)
	}
	// A pull request's ask with a stray card flag is the daemon's to refuse
	// ("takes no card"), not a list of card flags to add.
	cmd = askCmd()
	cmd.SetArgs([]string{"new", "--repo", "o/r", "--pr", "1", "--head", strings.Repeat("a", 40), "--context", "x", "Merge it?"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); errors.As(err, &ew) && ew.code == 2 {
		t.Fatalf("a pull request ask got the card usage error: %v", ew.err)
	}
	full := orchapi.AskCard{Question: "q", Context: "c", Recommend: "a", Why: "w", Searched: "s",
		Choices: []orchapi.AskChoice{{Label: "x"}, {Label: "y"}}}
	if err := cardFlagsMissing(full); err != nil {
		t.Fatalf("a full card: %v", err)
	}
}
