package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func init() { rootCmd.AddCommand(peopleCmd()) }

// peopleCmd is zm people: who an ask can be for.
func peopleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "people",
		Short: "The people who answer asks: their names, aliases and GitHub login",
		Long: `Lists the people who own or administer the project (-P, else every project you read), the
people an ask can be for. Name any one of a person's names, aliases or GitHub login in
zm ask new --to (or --owner): they all reach the same person; --to me is whoever started your
session. Machines, runners and bot accounts,
which answer nothing, are not listed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.PeopleList, error) {
				return orchclient.People(ctx, c, c.Project)
			}, peopleText)
		},
	}
}

// peopleText is zm people's human output: one line per person.
func peopleText(l api.PeopleList) string {
	var b strings.Builder
	if len(l.People) == 0 {
		b.WriteString("nobody here answers asks you can see\n")
	}
	for _, p := range l.People {
		line := strings.Join(append([]string{p.Name}, slices.DeleteFunc(slices.Clone(p.Names), func(n string) bool { return n == p.Name })...), ", ")
		if p.GitHub != "" {
			line += "  @" + p.GitHub
		}
		if len(p.Aliases) > 0 {
			line += "  aka " + strings.Join(p.Aliases, ", ")
		}
		var projects []string
		for _, pp := range p.Projects {
			if pp.Slug != "" && !slices.Contains(projects, pp.Slug) {
				projects = append(projects, pp.Slug)
			}
		}
		if len(projects) > 0 {
			line += "  answers " + strings.Join(projects, ", ")
		}
		if p.You {
			line += "  (you)"
		}
		fmt.Fprintln(&b, line)
	}
	for _, s := range l.Skipped {
		fmt.Fprintf(&b, "not listed: project %s did not answer (%s)\n", s.Project, s.Error)
	}
	return b.String()
}
