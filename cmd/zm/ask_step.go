package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/spf13/pflag"
)

// askStepFlags are zm ask new's flags for a person's own step (SPEC 16
// "Your step").
type askStepFlags struct {
	label, link string
	steps       []string
	minutes     int
}

func (s *askStepFlags) flags(f *pflag.FlagSet) {
	f.StringVar(&s.label, "step-label", "", "card: a step only the person it is for can do (a sign-in, a press in a service's console), in a few words; needs --to human:<name>, --step, --step-link and --verify")
	f.StringArrayVar(&s.steps, "step", nil, "card: one numbered line of the person's step, in plain words (repeat, at most 8)")
	f.StringVar(&s.link, "step-link", "", "card: the https page on the service's own site where the step starts")
	f.IntVar(&s.minutes, "step-minutes", 0, "card: about how many minutes the step takes")
}

// step is the card's owner step the flags name, or nil.
func (s *askStepFlags) step() (*orchapi.AskStep, error) {
	if s.label == "" && len(s.steps) == 0 && s.link == "" && s.minutes == 0 {
		return nil, nil
	}
	if s.label == "" || len(s.steps) == 0 || s.link == "" {
		return nil, fmt.Errorf("a person's step needs --step-label, --step (one or more) and --step-link")
	}
	return &orchapi.AskStep{Label: s.label, Steps: s.steps, Link: s.link, Minutes: s.minutes}, nil
}

// askStepDoneCmd is zm ask step-done: the person says they did their step.
func askStepDoneCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "step-done <id>",
		Short: "As the person a card is for, say you did its step; zm asks its asker to run the check",
		Long: `Says you did the card's own step (zm ask show <id> shows it). It closes nothing: zm
records when and asks the asker to run the card's check now; only a passing check
records it done. Only a person who owns or administers the project, never an agent.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				a, err := orchclient.GetAsk(ctx, c, args[0])
				if err != nil {
					return a, err
				}
				return orchclient.StepDone(ctx, c, project, a.ID, a.CardSHA)
			}, func(a orchapi.Ask) string {
				return "ask " + a.ID + ": step recorded; its asker runs the check"
			})
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "the project whose authority holds the ask (default: the current one)")
	return cmd
}
