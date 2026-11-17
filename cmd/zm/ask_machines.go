package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

// askMachinesCmd is zm ask machines: the machines of a team
// project an approved action may run on (--run-on), and whose they are, so
// an agent attaches the step to its ask instead of mailing it.
func askMachinesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "machines [human:<name>]",
		Short: "The machines an ask's action may run on (--run-on), and whose they are; a person's only, when named",
		Long: `A choice that needs the approver to act (run, deploy, log in) carries an action that runs on a
machine of theirs once they approve it: zm ask new --to human:<name> --run-on <machine> --agent
claude --brief <file>, or --command or --script. This lists the project's machines, each with the people whose
it is (who joined it), and whether an action may run there: ready means trusted and taking
remote tasks. Name a person to see only theirs. With exactly one ready machine of the approver,
zm ask new may leave --run-on out and zm picks it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			person := ""
			if len(args) == 1 {
				person = args[0]
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RunTargetList, error) {
				return orchclient.RunTargets(ctx, c, person)
			}, runTargetsText)
		},
	}
}

// runTargetsText is zm ask machines' human output.
func runTargetsText(l orchapi.RunTargetList) string {
	var b strings.Builder
	if len(l.Targets) == 0 {
		who := "the project has no machines"
		if l.Person != "" {
			who = l.Person + " has no machine in " + l.Project
		}
		fmt.Fprintf(&b, "%s\n", who)
	}
	for _, t := range l.Targets {
		people := strings.Join(t.People, ",")
		if people == "" {
			people = "(nobody's)"
		}
		state := "ready"
		if !t.Ready {
			state = "not ready: " + t.Why
		} else if !t.ScriptRuns {
			state = "ready (agent tasks only: its zm runs no scripts or commands)"
		} else if !t.CommandRuns {
			state = "ready (agent tasks and scripts: its zm runs no commands)"
		}
		seen := ""
		if t.Seen != nil {
			seen = "  seen " + time.Since(*t.Seen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(&b, "%-20s %-16s %s%s\n", t.Machine, people, state, seen)
	}
	if l.Hint != "" {
		fmt.Fprintf(&b, "hint: %s\n", l.Hint)
	}
	return b.String()
}
