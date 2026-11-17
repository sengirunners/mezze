package main

// `zm sandbox usage limit`: Potluck's `usage limit` (potluck cli/sandbox.go,
// PR 105) on zm's client, so the caller is zm's subject (--as, the session),
// never the socket's owner: the daemon's usage module (Potluck's, mounted in
// internal/wiring) checks admin on the whole project (API.md "Box usage").

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
)

func sandboxUsageLimitCmd() *cobra.Command {
	var per string
	cmd := &cobra.Command{Use: "limit [<area> <minutes>] [--per day|week|month]",
		Short: "Show or set each area's sandbox minutes per period; at the limit a new sandbox there is refused",
		Long: `With no arguments, lists the project's per-area limits on areas you read.
With an area and a number of minutes, sets that area's limit (an owner or admin
of the whole project; 0 removes it). Once an area's minutes for the period, its
running sandboxes counted so far, reach the limit, a new sandbox there is
refused until the period ends; a running one is not stopped. Periods are UTC
calendar days, ISO weeks or months. Without --per a limit keeps the period it
has, else it is per month.`,
		Example: "  zm sandbox usage limit\n  zm sandbox usage limit ci 600 --per week\n  zm sandbox usage limit ci 0",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return exitWith{2, fmt.Errorf("zm sandbox usage limit takes nothing, or an area and a number of minutes")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var set *computeapi.SetAreaMinuteLimitRequest
			area := ""
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return exitWith{2, fmt.Errorf("minutes: want a whole number, got %q", args[1])}
				}
				area, set = args[0], &computeapi.SetAreaMinuteLimitRequest{Minutes: n, Period: per}
			} else if per != "" {
				return exitWith{2, fmt.Errorf("--per goes with an area and a number of minutes")}
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			return sandboxUsageLimit(cmd.Context(), c, area, set, cmd.OutOrStdout())
		}}
	cmd.Flags().StringVar(&per, "per", "", "the period: day, week or month (UTC); default the area's current one, else month")
	return cmd
}

// sandboxUsageLimit lists the limits (set nil) or sets area's, then prints them.
func sandboxUsageLimit(ctx context.Context, c *client.Client, area string, set *computeapi.SetAreaMinuteLimitRequest, out io.Writer) error {
	var l computeapi.AreaMinuteLimits
	var err error
	if set == nil {
		l, err = computeclient.AreaMinuteLimits(ctx, c)
	} else {
		l, err = computeclient.SetAreaMinuteLimit(ctx, c, area, *set)
	}
	if err != nil {
		return err
	}
	if flagJSON {
		return printJSON(l)
	}
	if len(l.Limits) == 0 {
		fmt.Fprintf(out, "no area of %s has a sandbox minute limit\n", l.Project)
	}
	for _, x := range l.Limits {
		fmt.Fprintf(out, "%s %d minutes per %s\n", x.Area, x.Minutes, x.Period)
	}
	return nil
}
