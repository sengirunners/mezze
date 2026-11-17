package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchclient"
)

// askRetryCmd is zm ask retry: start an approved action that failed before
// it started again, identical, without a new card (SPEC 16 "Retry").
func askRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <id>",
		Short: "Start an approved run or command that failed before it started again, unchanged",
		Long: `Starts the approved run or command action of the ask again, exactly as it was approved: the same
command or script, ref, commit and machine, as the same approver. Nothing about the action can change;
for anything else, ask again. Only its asker or a person who owns or administers the project, only
when it failed before it started (its machine never took it, or zm could not hand it over: the ask
says it never reached the machine, or that zm has no record it did; a start that did reach it is
followed, never made twice), never once anything may have run, at most 3 times, and only within an
hour of the yes, unless an owner objected. zm checks the yes again as when it was given: the card unchanged, the approver still an
owner and their approving device still paired. An owner who confirmed the failure ended it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			a, err := orchclient.GetAsk(ctx, c, args[0])
			if err != nil {
				return err
			}
			out, err := orchclient.RetryAction(ctx, c, "", a.ID, a.CardSHA)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ask %s: started again. %s\n", out.ID, out.ActionStateText)
			return nil
		},
	}
}
