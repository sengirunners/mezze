package approvals

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// the defaulted machine runs the action as cleanRunning checks
// it: a script needs a zm that runs scripts; a command, one
// that runs scripts and commands; an agent task, neither.
func TestTargetLacks(t *testing.T) {
	agent := orchapi.AskAction{Kind: orchapi.ActionRun, Agent: "claude"}
	script := orchapi.AskAction{Kind: orchapi.ActionRun, Script: "deploy.sh"}
	command := orchapi.AskAction{Kind: orchapi.ActionCommand, Command: "make deploy"}
	all := orchapi.RunTarget{Ready: true, ScriptRuns: true, CommandRuns: true}
	scripts := orchapi.RunTarget{Ready: true, ScriptRuns: true}
	commandsOnly := orchapi.RunTarget{Ready: true, CommandRuns: true}
	old := orchapi.RunTarget{Ready: true}
	for _, c := range []struct {
		name string
		t    orchapi.RunTarget
		act  orchapi.AskAction
		want string
	}{
		{"agent, old zm", old, agent, ""},
		{"script, all", all, script, ""},
		{"script, old zm", old, script, "scripts"},
		{"command, all", all, command, ""},
		{"command, scripts only", scripts, command, "commands"},
		{"command, commands but no scripts", commandsOnly, command, "scripts"},
		{"command, old zm", old, command, "scripts or commands"},
	} {
		if got := targetLacks(c.t, c.act); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if h := runForms(all); !strings.Contains(h, "--command") || !strings.Contains(h, "--script") {
		t.Errorf("all: %q", h)
	}
	if h := runForms(scripts); strings.Contains(h, "--command") || !strings.Contains(h, "--script") {
		t.Errorf("scripts: %q", h)
	}
	if h := runForms(old); strings.Contains(h, "--command") || strings.Contains(h, "--script") {
		t.Errorf("old: %q", h)
	}
}
