package main

// zm ask bundle: person steps (SPEC 16 "Person steps"). A manual step for one
// person waits at the project's authority until their daily steps card
// carries it; the person's agent reports each step, and its adder's check
// records it done.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func askBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Steps for one person, carried by their daily steps card (add, ls, drop, report, step-check, check)",
		Long: `A manual step for one person (install an update, accept an invite, turn on a setting), with no real
choice in it, goes in their daily steps card instead of a message or a card of its own: one press on
that card hands every step on it to the person's own agent. A step that leaves a real choice is a
decision: file it as its own ask (zm ask new).`,
	}
	cmd.AddCommand(askBundleAddCmd(), askBundleLsCmd(), askBundleDropCmd(), askBundleReportCmd(), askBundleStepCheckCmd(), askBundleCheckCmd())
	return cmd
}

// readBrief reads a --brief file ("-" is stdin).
func readBrief(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, api.MaxBody))
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("--brief: %w", err)
	}
	return string(b), nil
}

func askBundleAddCmd() *cobra.Command {
	var req orchapi.StepAddRequest
	var brief string
	cmd := &cobra.Command{
		Use:   "add --to human:<name> --work <id> --title '<plain line>' --brief <file> --check '<shell line>'",
		Short: "Add a step to a person's next daily steps card, after the step judge reads it",
		Long: `Adds one step for one person. --title is the line they read (at most 80 bytes); --brief is a file
with the numbered steps to done that their agent works from (at most 900 bytes; say where every link,
version or value comes from); --check is one shell line (at most 200 bytes) that you run later to see
it done (zm ask bundle step-check), so it reads the result from where you can see it.

A step judge reads it first: a step that leaves the person a real choice is refused (file its own
ask), and so is one their agent cannot finish, one you could do yourself, or a check that proves
nothing. No answer from the judge refuses it as unavailable: add it again later.

--draft <id> completes a draft step (one zm saved from a refused report), keeping its person and
work item. --from-ask <ask> moves an open, unanswered hand ask into a step: its brief and verify line
(title: --title, else its card's), then withdraws the ask (its asker or an operator).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := readBrief(brief)
			if err != nil {
				return err
			}
			req.Brief = b
			if req.FromAsk == "" && b == "" {
				return fmt.Errorf("--brief <file> is required: the numbered steps to done")
			}
			if req.FromAsk != "" && (b != "" || req.Check != "") {
				return fmt.Errorf("--from-ask takes the ask's own brief and verify line: leave out --brief and --check")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonStep, error) {
				return orchclient.AddStep(ctx, c, req)
			}, func(st orchapi.PersonStep) string {
				return fmt.Sprintf("step %s for %s: pending, on their next daily steps card (risk %s)", st.ID, st.Person, st.Risk)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.To, "to", "", "the person: human:<name> (zm people lists them)")
	f.StringVar(&req.Work, "work", "", "the work item the step serves")
	f.StringVar(&req.Title, "title", "", "one plain line the person reads (at most 80 bytes)")
	f.StringVar(&brief, "brief", "", "file with the numbered steps to done for their agent (at most 900 bytes; - is stdin)")
	f.StringVar(&req.Check, "check", "", "one shell line, at most 200 bytes, that exits 0 only once the step is done")
	f.StringVar(&req.Draft, "draft", "", "complete this draft step (its person and work item stay)")
	f.StringVar(&req.FromAsk, "from-ask", "", "move this open, unanswered hand ask into a step and withdraw it")
	return cmd
}

func askBundleLsCmd() *cobra.Command {
	var to, state string
	cmd := &cobra.Command{
		Use:   "ls [--to human:<name>] [--state all|draft|pending|carded|reported|done|failed|dropped]",
		Short: "List person steps, oldest first (default: not done or dropped)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.StepList, error) {
				return orchclient.Steps(ctx, c, flagProject, to, state)
			}, func(l orchapi.StepList) string {
				if len(l.Steps) == 0 {
					return "no steps"
				}
				var b strings.Builder
				for _, st := range l.Steps {
					fmt.Fprintf(&b, "%s  %-8s  %s  %s  (%s, added %s)", st.ID, st.State, st.Person, st.Title, st.Work, st.Created.Local().Format(time.DateTime))
					if st.Bundle != "" {
						fmt.Fprintf(&b, "  card %s #%d", first(st.Ask, st.Bundle), st.Number)
					}
					b.WriteString("\n")
				}
				return b.String()
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "only this person's steps (human:<name>)")
	cmd.Flags().StringVar(&state, "state", "", "all, or one state (default: not done or dropped)")
	return cmd
}

func askBundleDropCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drop <step>",
		Short: "Take a step back (its adder, or who may operate on its area)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonStep, error) {
				return orchclient.DropStep(ctx, c, args[0])
			}, func(st orchapi.PersonStep) string { return "step " + st.ID + " dropped" })
		},
	}
}

func askBundleReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report <card> <n> done|failed \"<note>\"",
		Short: "As the person's agent, report step n of a steps card done or failed",
		Long: `Reports step n of a steps card (<card>: its bundle id or its ask) done or failed, with one plain
line saying what happened. zm comments it on the step's work item and asks the step's adder to run
its check; only that check records it done.`,
		Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return fmt.Errorf("<n> is the step's number on the card (1, 2, ...), not %q", args[1])
			}
			result := strings.ToLower(args[2])
			if result != orchapi.StepDone && result != orchapi.StepFailed {
				return fmt.Errorf("say done or failed, not %q", args[2])
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonStep, error) {
				return orchclient.ReportStep(ctx, c, args[0], n, orchapi.StepReportRequest{Result: result, Note: args[3]})
			}, func(st orchapi.PersonStep) string {
				return fmt.Sprintf("step %d (%s): %s; its adder runs the check", n, st.ID, st.State)
			})
		},
	}
}

func askBundleStepCheckCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "step-check <step>",
		Short: "Run a step's check line and report it: only an exit of 0 records it done",
		Long: `Runs the step's check line with sh -c in this directory, as you, for at most 10 minutes (as zm ask
check does), and reports its exit and last lines. An exit of 0 records the step done; another exit
after its agent reported it done records it failed. It is its adder's command: --yes runs it as
someone else.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			st, err := orchclient.GetStep(ctx, c, args[0])
			if err != nil {
				return err
			}
			switch st.State {
			case orchapi.StepDraft, orchapi.StepDone, orchapi.StepDropped:
				return api.Errorf(api.CodeConflict, "step %s is %s: nothing to check", st.ID, st.State)
			}
			if w, err := c.Whoami(ctx); (err != nil || w.As == "" || w.As != st.AddedBy) && !yes {
				return fmt.Errorf("step %s's check is its adder's command: %q. Run it as its adder, or add --yes to run it as you", st.ID, st.Check)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "running: %s\n", st.Check)
			exit, output := runCheck(ctx, st.Check)
			out, err := orchclient.CheckStep(ctx, c, st.ID, orchapi.StepCheckRequest{Exit: &exit, Output: output})
			if err != nil {
				return err
			}
			if flagJSON {
				if err := printJSON(out); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "step %s: check exited %d; %s\n", out.ID, exit, out.State)
			}
			if out.State != orchapi.StepDone {
				return exitSilently(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "run the check although you are not the step's adder")
	return cmd
}

func askBundleCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <card>",
		Short: "Exit 0 when every step of a steps card is done or dropped",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			b, err := orchclient.GetBundle(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				if err := printJSON(b); err != nil {
					return err
				}
			} else {
				left := 0
				for _, st := range b.Steps {
					if st.State != orchapi.StepDone && st.State != orchapi.StepDropped {
						left++
						fmt.Fprintf(cmd.OutOrStdout(), "step %d %s: %s (%s)\n", st.Number, st.ID, st.State, st.Title)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "card %s: %d of %d steps not done\n", first(b.Ask, b.ID), left, len(b.Steps))
			}
			if !b.Done {
				return exitSilently(1)
			}
			return nil
		},
	}
}
