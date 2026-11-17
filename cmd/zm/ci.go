package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(ciCmd()) }

func ciCmd() *cobra.Command {
	c := &cobra.Command{Use: "ci", Short: "GitHub Actions on lent machines"}
	c.AddCommand(&cobra.Command{
		Use: "status", Short: "Each repo's workflow line, recent jobs and reasons for waiting or refused jobs",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClientContext(cmd.Context())
			if err != nil {
				return err
			}
			st, err := orchclient.CIStatus(cmd.Context(), c)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(st)
			}
			printCIStatus(st)
			return nil
		},
	})
	c.AddCommand(ciDoctorCmd())
	c.AddCommand(ciAddCmd())
	c.AddCommand(potluckCICmd("pr <owner/repo> [--job <id>]", "Move one job's runs-on to the lent Mac for own branches, as a pull request"))
	return c
}

func printCIStatus(st orchapi.CIStatus) {
	plain := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, issue := range st.Issues {
		fmt.Println(plain(issue))
	}
	for _, r := range st.Repos {
		fmt.Printf("\n%s: %s\n", plain(r.Repo), plain(r.Summary))
		if r.RunsOn != "" {
			fmt.Println(r.RunsOn)
		}
		fmt.Printf("Fallback job limit: %d s (supported workflow timeout-minutes takes precedence)\n", r.JobLimitS)
		for _, issue := range r.Issues {
			fmt.Println(plain(issue))
		}
		_ = printCIJobs(os.Stdout, r.Jobs)
	}
}
