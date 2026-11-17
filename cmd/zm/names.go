package main

// Owned by the owners brick: zm names (SPEC 22, @owner/project).

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
)

func init() { rootCmd.AddCommand(namesCmd()) }

// splitOwnerName parses @owner/name (the "@" optional).
func splitOwnerName(s string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(s), "@"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("want @owner/name, got %q", s)
	}
	return owner, name, nil
}

func namesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "names", Short: "@owner/project names: an owner is a GitHub user or org, bound by its numeric id"}
	bind := &cobra.Command{
		Use:   "bind <project> @owner/name",
		Short: "Name a project @owner/name: approve in the browser as the GitHub owner, or as an active member of the owner org (a name is never moved)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[1])
			if err != nil {
				return err
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			out, err := githubLogin(cmd.Context(), c, api.GitHubLoginRequest{Purpose: "bind", Project: args[0], Owner: owner, Name: name}, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if b := out.Bind; b != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%s now names %s (%s)\n", b.Path, args[0], b.Account)
			}
			return nil
		},
	}
	ls := &cobra.Command{
		Use: "ls <project>", Short: "A project's @owner/name bindings, current first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			l, err := c.ProjectNames(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(l)
			}
			for _, b := range l.Names {
				mark := " "
				if b.Current {
					mark = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mark, b.Path)
			}
			return nil
		},
	}
	resolve := &cobra.Command{
		Use: "resolve @owner/name", Short: "The project @owner/name reaches on this authority", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[0])
			if err != nil {
				return err
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			r, err := c.ResolveName(cmd.Context(), owner, name)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			frozen := ""
			if r.Frozen {
				frozen = " [frozen: the owner's GitHub account is suspended or deleted]"
			}
			switch {
			case r.Moved:
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s (moved: %s)%s\n", r.Slug, r.Project, r.Path, frozen)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s%s\n", r.Slug, r.Project, frozen)
			}
			return nil
		},
	}
	cmd.AddCommand(bind, ls, resolve)
	return cmd
}
