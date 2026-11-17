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

func init() { rootCmd.AddCommand(permitCmd()) }

// permitCmd is zm permit (SPEC 16 "Standing permissions").
func permitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "permit",
		Short: "Standing permissions: zm acts without a card while its own records show each condition holds",
		Long: `A standing permission lets zm do one kind of act it performs itself (start an agent on planned
work, place work on a box marked as holding no secrets) without a card each time. Kinds are a
closed list with frozen text; zm checks each condition from its own records, and anything it
cannot read means a card as now. Granting, renewing or widening takes a card answered yes on a
paired device or with a key; revoking takes one press by an owner or admin. While a kind is in its dry
run, zm skips no card and only records which cards it would have covered (zm permit dryrun).`,
	}
	cmd.AddCommand(permitListCmd(), permitAskCmd(), permitGrantCmd(), permitRevokeCmd(), permitUnmarkCmd(), permitDryRunCmd())
	return cmd
}

func permitListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "The kinds, the project's standing permissions and its box marks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, orchclient.Permits, permitListText)
		},
	}
}

func permitListText(l orchapi.PermitList) string {
	var b strings.Builder
	for _, k := range l.Kinds {
		dry := ""
		if k.DryRun {
			dry = " (dry run: no card is skipped)"
		}
		fmt.Fprintf(&b, "kind %s%s\n  %s\n", k.Name, dry, k.Text)
		for _, c := range k.Conditions {
			fmt.Fprintf(&b, "  - %s\n", c.Text)
		}
	}
	if len(l.Permits) == 0 {
		b.WriteString("no standing permissions\n")
	}
	for _, p := range l.Permits {
		fmt.Fprintf(&b, "%s  %-12s %-8s by %s, until %s\n", p.ID, p.Kind, p.State, p.Approver, p.Expires.Local().Format(time.DateTime))
	}
	for _, m := range l.Boxes {
		fmt.Fprintf(&b, "box %s: %s (by %s, %s)\n", m.Machine, m.Class, m.Approver, m.Set.Local().Format(time.DateOnly))
	}
	return b.String()
}

// permitTarget reads <kind> --days N, or --box <machine> --class <class>.
func permitTarget(cmd *cobra.Command, args []string) (orchapi.PermitCardRequest, error) {
	var req orchapi.PermitCardRequest
	req.Days, _ = cmd.Flags().GetInt("days")
	req.Machine, _ = cmd.Flags().GetString("box")
	req.Class, _ = cmd.Flags().GetString("class")
	if len(args) > 0 {
		req.Kind = args[0]
	}
	if req.Kind == "" && req.Machine == "" {
		return req, fmt.Errorf("name a kind (zm permit list) with --days, or --box <machine> --class no-secrets")
	}
	return req, nil
}

func targetFlags(cmd *cobra.Command) {
	cmd.Flags().Int("days", 0, "how long the permission lasts, in days")
	cmd.Flags().String("box", "", "a machine to mark, instead of a kind")
	cmd.Flags().String("class", "", "the box class (no-secrets)")
}

func permitAskCmd() *cobra.Command {
	var recommend, why, searched, to, work string
	cmd := &cobra.Command{
		Use:   "ask [<kind>] (--days N | --box <machine> --class <class>)",
		Short: "File the card that grants a kind or marks a box, exactly as zm wrote it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := permitTarget(cmd, args)
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				card, err := orchclient.PermitCard(ctx, c, req)
				if err != nil {
					return orchapi.Ask{}, err
				}
				card.Recommend, card.Why, card.Searched = recommend, why, searched
				return orchclient.NewAsk(ctx, c, orchapi.NewAskRequest{Card: &card, To: to, Subject: orchapi.AskSubject{Work: work}})
			}, func(a orchapi.Ask) string {
				return "asked " + a.ID + "; once an owner answers yes on a paired device or with a key: zm permit grant " + a.ID + " (same target flags)"
			})
		},
	}
	targetFlags(cmd)
	cmd.Flags().StringVar(&recommend, "recommend", "", "yes or no")
	cmd.Flags().StringVar(&why, "why", "", "what the recommendation does, good and bad")
	cmd.Flags().StringVar(&searched, "searched", "", "what you checked before asking")
	cmd.Flags().StringVar(&to, "to", "", "owners (default), or one owner by name")
	cmd.Flags().StringVar(&work, "work", "", "the work item the card belongs to")
	return cmd
}

func permitGrantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant <ask> [<kind>] (--days N | --box <machine> --class <class>)",
		Short: "Record the permission or box mark an owner's signed yes on that card granted",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := permitTarget(cmd, args[1:])
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PermitGrant, error) {
				return orchclient.GrantPermit(ctx, c, orchapi.PermitGrantRequest{PermitCardRequest: req, Ask: args[0]})
			}, func(g orchapi.PermitGrant) string {
				if g.Permit != nil {
					return fmt.Sprintf("permit %s: %s until %s", g.Permit.ID, g.Permit.Kind, g.Permit.Expires.Local().Format(time.DateTime))
				}
				return fmt.Sprintf("box %s marked %s", g.Box.Machine, g.Box.Class)
			})
		},
	}
	targetFlags(cmd)
	return cmd
}

func permitRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <permit>",
		Short: "End a standing permission now (any owner or admin; no signature needed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Permit, error) {
				return orchclient.RevokePermit(ctx, c, args[0])
			}, func(p orchapi.Permit) string { return "permit " + p.ID + " " + p.State })
		},
	}
}

func permitUnmarkCmd() *cobra.Command {
	var class string
	cmd := &cobra.Command{
		Use:   "unmark <machine>",
		Short: "Take a class off a box (any owner or admin; no signature needed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.BoxUnmarkRequest, error) {
				return orchclient.UnmarkBox(ctx, c, orchapi.BoxUnmarkRequest{Machine: args[0], Class: class})
			}, func(r orchapi.BoxUnmarkRequest) string { return "box " + r.Machine + ": " + r.Class + " taken off" })
		},
	}
	cmd.Flags().StringVar(&class, "class", "no-secrets", "the box class")
	return cmd
}

func permitDryRunCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "dryrun",
		Short: "Per kind: cards a standing permission would have covered, and how people answered them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var from time.Time
			if days > 0 {
				from = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PermitDryRunReport, error) {
				return orchclient.PermitDryRun(ctx, c, from, time.Time{})
			}, permitDryRunText)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "the window, in days back from now (default 7)")
	return cmd
}

func permitDryRunText(r orchapi.PermitDryRunReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s to %s\n", r.Project, r.From.Local().Format(time.DateOnly), r.To.Local().Format(time.DateOnly))
	fmt.Fprintf(&b, "%-14s %8s %12s %7s %16s %6s\n", "kind", "checked", "would cover", "saved", "wrongly covered", "open")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "%-14s %8d %12d %7d %16d %6d\n", row.Kind, row.Checked, row.WouldCover, row.Saved, row.WronglyCovered, row.Open)
	}
	return b.String()
}
