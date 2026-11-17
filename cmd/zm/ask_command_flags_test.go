package main

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// --command makes a command action with --run-on, --ref and --commit, and
// goes with nothing a script run or agent task takes. With no --run-on the
// daemon picks the approver's only ready machine
// (TestAskActionFlagsRunWithoutMachine).
func TestAskActionFlagsCommand(t *testing.T) {
	commit := strings.Repeat("a", 40)
	a := askActionFlags{runOn: "bea-mac", command: "make deploy-dns", ref: "main", commit: commit}
	act, err := a.action("a")
	if err != nil {
		t.Fatal(err)
	}
	if act.Kind != orchapi.ActionCommand || act.Machine != "bea-mac" || act.Command != "make deploy-dns" || act.Ref != "main" || act.Commit != commit || act.On != "a" || act.Script != "" {
		t.Fatalf("action %+v", act)
	}
	for name, bad := range map[string]askActionFlags{
		"with script": {runOn: "m", command: "ls", script: "x.sh", ref: "main", commit: commit},
		"with arg":    {runOn: "m", command: "ls", args: []string{"x"}, ref: "main", commit: commit},
		"with agent":  {runOn: "m", command: "ls", agent: "claude"},
	} {
		if _, err := bad.action("a"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
