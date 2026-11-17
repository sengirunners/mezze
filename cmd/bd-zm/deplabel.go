package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// ---- dep, label ----

func depCmd() *cobra.Command {
	var blocks string
	cmd := &cobra.Command{
		Use: "dep <id> --blocks <other>", Args: cobra.ArbitraryArgs,
		Short: "Add, remove and list dependencies",
		Long: "bd-zm has: dep add, dep remove, dep list <id> [--direction down|up|both], dep <id> --blocks <other>.\n" +
			"The full read, dependents included: bd show <id> --include-dependents --json.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if blocks == "" || len(args) != 1 {
				return errors.New(depUnsupported)
			}
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			br, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			dr, err := route(ctx, c, blocks)
			if err != nil {
				return err
			}
			if err := crossProject(dr, br, false); err != nil {
				return err
			}
			blocker, blocked := br.id, dr.id
			w, err := orchclient.AddIssueDep(ctx, dr.c, blocked, orchapi.IssueDepRequest{Target: blocker, Type: orchapi.DepBlocks})
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"blocked_id": blocked, "blocker_id": blocker, "status": "added", "type": orchapi.DepBlocks}, true)
			}
			fmt.Fprintf(stdout, "✓ Added dependency: %s blocks %s (%s)\n", blocker, blocked, w.Title)
			return nil
		},
	}
	cmd.Flags().StringVarP(&blocks, "blocks", "b", "", "the issue this one blocks")
	var typ, blockedBy, dependsOn string
	add := &cobra.Command{
		Use: "add <id> <depends-on>", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target := blockedBy
			if target == "" {
				target = dependsOn
			}
			if len(args) == 2 {
				if target != "" {
					return errors.New("give the dependency as an argument or a flag, not both")
				}
				target = args[1]
			}
			if target == "" {
				return errors.New("dep add needs the issue it depends on")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			fr, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			tr, err := route(ctx, c, target)
			if err != nil {
				return err
			}
			if err := crossProject(fr, tr, false); err != nil {
				return err
			}
			from, to, c := fr.id, tr.id, fr.c
			w, err := orchclient.AddIssueDep(ctx, c, from, orchapi.IssueDepRequest{Target: to, Type: typ})
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"depends_on_id": to, "issue_id": from, "status": "added", "type": typ}, true)
			}
			toTitle := ""
			if d, err := orchclient.Issues(ctx, c, orchapi.IssueQuery{IDs: []string{to}, All: true, Limit: 1}); err == nil && len(d.Issues) == 1 {
				toTitle = d.Issues[0].Title
			}
			fmt.Fprintf(stdout, "✓ Added dependency: %s (%s) depends on %s (%s) (%s)\n", from, w.Title, to, toTitle, typ)
			return nil
		},
	}
	add.Flags().StringVarP(&typ, "type", "t", orchapi.DepBlocks, "dependency type")
	add.Flags().StringVar(&blockedBy, "blocked-by", "", "the issue that blocks this one")
	add.Flags().StringVar(&dependsOn, "depends-on", "", "the issue this one depends on")
	rm := &cobra.Command{
		Use: "remove <id> <depends-on>", Aliases: []string{"rm"}, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			fr, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			tr, err := route(ctx, c, args[1])
			if err != nil {
				return err
			}
			if err := crossProject(fr, tr, true); err != nil {
				return err
			}
			from, to := fr.id, tr.id
			w, err := orchclient.RemoveIssueDep(ctx, fr.c, from, to)
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"depends_on_id": to, "issue_id": from, "status": "removed"}, true)
			}
			fmt.Fprintf(stdout, "✓ Removed dependency: %s (%s) no longer depends on %s\n", from, w.Title, to)
			return nil
		},
	}
	cmd.AddCommand(add, rm, depListCmd())
	return cmd
}

const depUnsupported = "unknown or unsupported dep command (bd-zm has: dep add, dep remove, dep list <id> [--direction down|up|both], " +
	"dep <id> --blocks <other>; for the full read with dependents: bd show <id> --include-dependents --json)"

// depListCmd is bd's dep list: each issue's dependencies (down), dependents
// (up) or both, as IssueWithDependencyMetadata, in bd v1.0.5's per-ID output.
// It reads what bd show reads. With several ids, one that does not resolve is
// skipped with a warning (bd's batch mode); with one, it is an error.
func depListCmd() *cobra.Command {
	var direction, typ string
	cmd := &cobra.Command{
		Use: "list <id>...", Aliases: []string{"ls"}, Args: cobra.MinimumNArgs(1),
		Short: "List issues' dependencies or dependents (same read as bd show --include-dependents)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if direction != "down" && direction != "up" && direction != "both" {
				return fmt.Errorf("invalid --direction %q (want down, up or both)", direction)
			}
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			batch := len(args) > 1
			// External edges (depends_on_external) have no issue row; bd's
			// dep list joins on issues, so it never lists them.
			pick := func(ls []orchapi.IssueLink) []orchapi.IssueLink {
				var out []orchapi.IssueLink
				for _, l := range ls {
					if l.Status != "" && (typ == "" || l.DependencyType == typ) {
						out = append(out, l)
					}
				}
				return out
			}
			var all []orchapi.IssueLink
			var ids []string
			for _, arg := range args {
				r, err := route(ctx, c, arg)
				var d orchapi.IssueDetail
				if err == nil {
					d, err = orchclient.Issue(ctx, r.c, r.id)
				}
				if err != nil && batch {
					fmt.Fprintf(stderr, "warning: resolving %s: %v (skipped)\n", arg, err)
					continue
				}
				if notFound(err) {
					return errors.New(missing(arg, err))
				}
				if err != nil {
					return err
				}
				ids = append(ids, d.ID)
				if direction != "up" {
					all = append(all, pick(d.DependsOn)...)
				}
				if direction != "down" {
					all = append(all, pick(d.Dependents)...)
				}
			}
			if asJSON(cmd) {
				out := []*bdWithType{}
				for _, l := range all {
					out = append(out, &bdWithType{bdIssue: *toBD(l.Issue, false), DependencyType: l.DependencyType})
				}
				return writeJSON(stdout, out, false)
			}
			if batch && len(ids) == 0 {
				fmt.Fprintln(stderr, "no resolvable issues in batch")
				return nil
			}
			if len(all) == 0 {
				switch {
				case len(ids) > 1 || direction == "both":
					fmt.Fprintln(stdout, "\nNo dependencies found")
				case direction == "up":
					fmt.Fprintf(stdout, "\nNo issues depend on %s\n", ids[0])
				default:
					fmt.Fprintf(stdout, "\n%s has no dependencies\n", ids[0])
				}
				return nil
			}
			for _, l := range all {
				fmt.Fprintf(stdout, "  %s: %s [P%d] (%s) via %s\n", l.ID, l.Title, l.Priority, l.Status, l.DependencyType)
			}
			fmt.Fprintln(stdout)
			return nil
		},
	}
	cmd.Flags().StringVar(&direction, "direction", "down", "down (dependencies), up (dependents) or both")
	cmd.Flags().StringVarP(&typ, "type", "t", "", "only this dependency type")
	return cmd
}

func labelCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "label", Args: cobra.ArbitraryArgs, RunE: func(*cobra.Command, []string) error {
		return errors.New("unknown or unsupported label command (bd-zm has: label add, label remove)")
	}}
	mk := func(verb string) *cobra.Command {
		return &cobra.Command{
			Use: verb + " <id...> <label>", Args: cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				label := strings.TrimSpace(args[len(args)-1])
				if label == "" {
					return errors.New("label cannot be empty")
				}
				c, err := connect(ctx)
				if err != nil {
					return err
				}
				var out []map[string]any
				for _, raw := range args[:len(args)-1] {
					r, err := route(ctx, c, raw)
					id := r.id
					switch {
					case err != nil:
					case verb == "add":
						_, err = orchclient.AddIssueLabel(ctx, r.c, id, label)
					default:
						_, err = orchclient.RemoveIssueLabel(ctx, r.c, id, label)
					}
					if err != nil {
						return fmt.Errorf("%s: %s", raw, errMsg(err))
					}
					status := "added"
					if verb != "add" {
						status = "removed"
					}
					out = append(out, map[string]any{"issue_id": id, "label": label, "status": status})
					if !asJSON(cmd) {
						if verb == "add" {
							fmt.Fprintf(stdout, "✓ Added label '%s' to %s\n", label, id)
						} else {
							fmt.Fprintf(stdout, "✓ Removed label '%s' from %s\n", label, id)
						}
					}
				}
				if asJSON(cmd) {
					return writeJSON(stdout, out, false)
				}
				return nil
			},
		}
	}
	cmd.AddCommand(mk("add"), mk("remove"))
	return cmd
}
