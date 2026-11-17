package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A run may leave --run-on out (the daemon picks the approver's
// only ready machine); the other run flags alone still need a run.
func TestAskActionFlagsRunWithoutMachine(t *testing.T) {
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Deploy the fix."), 0o600); err != nil {
		t.Fatal(err)
	}
	a := askActionFlags{agent: "claude", brief: brief}
	act, err := a.action("a")
	if err != nil || act == nil || act.Kind != orchapi.ActionRun || act.Machine != "" || act.Brief != "Deploy the fix." {
		t.Fatalf("agent task, no machine: %+v %v", act, err)
	}
	a = askActionFlags{script: "scripts/deploy.sh", ref: "main", commit: strings.Repeat("a", 40)}
	if act, err := a.action("a"); err != nil || act == nil || act.Script != "scripts/deploy.sh" || act.Machine != "" {
		t.Fatalf("script, no machine: %+v %v", act, err)
	}
	a = askActionFlags{command: "make deploy", ref: "main", commit: strings.Repeat("a", 40)}
	if act, err := a.action("a"); err != nil || act == nil || act.Kind != orchapi.ActionCommand || act.Command != "make deploy" || act.Machine != "" {
		t.Fatalf("command, no machine: %+v %v", act, err)
	}
	a = askActionFlags{command: "make deploy", agent: "claude", brief: brief}
	if _, err := a.action("a"); err == nil || !strings.Contains(err.Error(), "--command goes with") {
		t.Fatalf("command with an agent: %v", err)
	}
	a = askActionFlags{brief: brief}
	if _, err := a.action("a"); err == nil || !strings.Contains(err.Error(), "go with --run-on") {
		t.Fatalf("a brief alone: %v", err)
	}
	a = askActionFlags{agent: "claude", brief: brief, keyFile: brief}
	if _, err := a.action("a"); err == nil || !strings.Contains(err.Error(), "one action per card") {
		t.Fatalf("two actions: %v", err)
	}
}

func TestRunTargetsText(t *testing.T) {
	out := runTargetsText(orchapi.RunTargetList{Project: "harbor", Person: "bea", Targets: []orchapi.RunTarget{
		{Machine: "studio", People: []string{"bea"}, Ready: true, ScriptRuns: true},
		{Machine: "mac", People: []string{"bea"}, Why: "takes no tasks from other machines"},
	}})
	if !strings.Contains(out, "studio") || !strings.Contains(out, "ready") || !strings.Contains(out, "not ready: takes no tasks") {
		t.Fatalf("text:\n%s", out)
	}
	for _, c := range []struct {
		t    orchapi.RunTarget
		want string
	}{
		{orchapi.RunTarget{Machine: "a", Ready: true, ScriptRuns: true, CommandRuns: true}, "a                    (nobody's)       ready\n"},
		{orchapi.RunTarget{Machine: "b", Ready: true, ScriptRuns: true}, "its zm runs no commands"},
		{orchapi.RunTarget{Machine: "c", Ready: true, CommandRuns: true}, "agent tasks only"},
		{orchapi.RunTarget{Machine: "d", Ready: true}, "agent tasks only"},
	} {
		if out := runTargetsText(orchapi.RunTargetList{Targets: []orchapi.RunTarget{c.t}}); !strings.Contains(out, c.want) {
			t.Errorf("%s: %q, want %q", c.t.Machine, out, c.want)
		}
	}
	if out := runTargetsText(orchapi.RunTargetList{Project: "harbor", Person: "bea", Hint: "join one"}); !strings.Contains(out, "bea has no machine in harbor") || !strings.Contains(out, "hint: join one") {
		t.Fatalf("none:\n%s", out)
	}
}
