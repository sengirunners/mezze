package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/spf13/pflag"
)

// askActionFlags are zm ask new's flags for an action that runs something
// as the approver (SPEC 16 "Asks that carry an action").
type askActionFlags struct {
	runOn, script, ref, commit, agent, brief string
	command                                  string
	args                                     []string
	browserOn, plan, signIn                  string
	origins                                  []string
	submit                                   bool
	keyFile, onMachine, user                 string
	hand                                     bool
	risk                                     string
}

func (a *askActionFlags) flags(f *pflag.FlagSet) {
	f.StringVar(&a.runOn, "run-on", "", "card: the machine a run action runs on (with --script, --ref, --commit, --arg; --command, --ref, --commit; or --agent and --brief; default the approver's only ready one: zm ask machines)")
	f.StringVar(&a.command, "command", "", "card: one shell command line --run-on runs exactly as the card shows it, in a clean checkout of --ref at --commit (no secrets in it)")
	f.StringVar(&a.script, "script", "", "card: --run-on's script, a path in the repository")
	f.StringVar(&a.ref, "ref", "", "card: --run-on's branch or tag name (main, v1.2; never refs/...; a tag for a card that may wait)")
	f.StringVar(&a.commit, "commit", "", "card: the full 40-character commit id --ref points at now (git rev-parse <ref>); the run uses exactly it")
	f.StringArrayVar(&a.args, "arg", nil, "card: one literal argument of --script (repeat)")
	f.StringVar(&a.agent, "agent", "", "card: --run-on's or --hand's agent, claude or codex (with --brief)")
	f.BoolVar(&a.hand, "hand", false, "card: for an approver with no machine that takes tasks, hand --agent and --brief (numbered steps to done, any one-time setup included) to their own agent, preapproved by --ack-on's choice; needs --verify")
	f.StringVar(&a.brief, "brief", "", "card: the file holding the agent task's brief (at most 8 KiB)")
	f.StringVar(&a.browserOn, "browser-on", "", "card: the machine a browser action runs on (with --plan and --origin; refused until a runtime enforces the scope)")
	f.StringVar(&a.plan, "plan", "", "card: the file holding the browser action's numbered steps (at most 4 KiB)")
	f.StringArrayVar(&a.origins, "origin", nil, "card: an https origin the browser may open (repeat)")
	f.StringVar(&a.signIn, "sign-in", "", "card: the saved account the browser signs in with (default none)")
	f.BoolVar(&a.submit, "submit", false, "card: the browser may press final buttons (send, pay, delete)")
	f.StringVar(&a.keyFile, "authorize-key", "", "card: an OpenSSH public key file zm adds to --user's authorized_keys on --on-machine")
	f.StringVar(&a.onMachine, "on-machine", "", "card: the machine --authorize-key acts on")
	f.StringVar(&a.user, "user", "", "card: the account --authorize-key adds the key to (the user zm runs as there)")
	f.StringVar(&a.risk, "risk", "", "card: the action cannot be undone (irreversible) or spends money (spending): the person types a word to pick it; the card judge may raise it, never lower it")
}

// action is the card action the flags name, or nil, with --risk.
func (a *askActionFlags) action(on string) (*orchapi.AskAction, error) {
	act, err := a.kindAction(on)
	if err != nil || a.risk == "" {
		return act, err
	}
	if act == nil {
		return nil, fmt.Errorf("--risk goes with an action (--run-on, --browser-on, --authorize-key or --hand)")
	}
	act.Risk = strings.ToLower(strings.TrimSpace(a.risk))
	return act, nil
}

func (a *askActionFlags) kindAction(on string) (*orchapi.AskAction, error) {
	// A run may leave --run-on out: the daemon picks the approver's only
	// ready machine, or refuses with their list.
	run := a.runOn != "" || a.script != "" || a.agent != "" || a.command != ""
	n := 0
	for _, set := range []bool{run, a.browserOn != "", a.keyFile != ""} {
		if set {
			n++
		}
	}
	read := func(flag, path string) (string, error) {
		if path == "" {
			return "", nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		return string(b), nil
	}
	if a.hand {
		if n > 1 || a.runOn != "" || a.script != "" || a.command != "" || a.ref != "" || a.commit != "" || len(a.args) > 0 {
			return nil, fmt.Errorf("--hand takes only --agent and --brief: it runs on the approver's own machine")
		}
		brief, err := read("--brief", a.brief)
		if err != nil {
			return nil, err
		}
		return &orchapi.AskAction{Kind: orchapi.ActionHand, On: on, Agent: a.agent, Brief: brief}, nil
	}
	switch {
	case n > 1:
		return nil, fmt.Errorf("one action per card: --run-on, --browser-on, --authorize-key or --hand")
	case run && a.command != "":
		if a.script != "" || len(a.args) > 0 || a.agent != "" || a.brief != "" {
			return nil, fmt.Errorf("--command goes with --run-on, --ref and --commit, not --script, --arg, --agent or --brief")
		}
		return &orchapi.AskAction{Kind: orchapi.ActionCommand, On: on, Machine: a.runOn, Command: a.command, Ref: a.ref, Commit: a.commit}, nil
	case run:
		brief, err := read("--brief", a.brief)
		if err != nil {
			return nil, err
		}
		return &orchapi.AskAction{Kind: orchapi.ActionRun, On: on, Machine: a.runOn, Script: a.script, Ref: a.ref, Commit: a.commit,
			Args: a.args, Agent: a.agent, Brief: brief}, nil
	case a.browserOn != "":
		plan, err := read("--plan", a.plan)
		if err != nil {
			return nil, err
		}
		return &orchapi.AskAction{Kind: orchapi.ActionBrowser, On: on, Machine: a.browserOn, Plan: plan,
			Scope: &orchapi.BrowserScope{Origins: a.origins, SignIn: a.signIn, Submit: a.submit}}, nil
	case a.keyFile != "":
		key, err := read("--authorize-key", a.keyFile)
		if err != nil {
			return nil, err
		}
		return &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, On: on, Machine: a.onMachine, User: a.user, Key: strings.TrimSpace(key)}, nil
	}
	if a.command != "" || a.script != "" || a.ref != "" || a.commit != "" || len(a.args) > 0 || a.agent != "" || a.brief != "" || a.plan != "" ||
		len(a.origins) > 0 || a.signIn != "" || a.submit || a.onMachine != "" || a.user != "" {
		return nil, fmt.Errorf("those flags go with --run-on, --browser-on or --authorize-key")
	}
	return nil, nil
}
