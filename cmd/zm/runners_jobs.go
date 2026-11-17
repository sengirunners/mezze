package main

// zm runners jobs (SPEC 20 "GitHub runner broker", Jobs): the broker's
// recent jobs, where each ran and why one waits, failed or was ignored.
// Owned by the runner broker; cmd/zm/runners.go adds it.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/spf13/cobra"
)

func runnersJobsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Recent GitHub Actions jobs the broker saw: where each ran, and why one waits, failed or was ignored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Jobs []orchapi.CIJob `json:"jobs"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v0/runners/jobs?limit="+strconv.Itoa(limit), nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if len(out.Jobs) == 0 {
				fmt.Println("no jobs yet: a workflow asks for the pool with runs-on: <pool name> (or <pool name>:<size>)")
				return nil
			}
			return printCIJobs(os.Stdout, out.Jobs)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "how many jobs, newest first (at most 500)")
	return cmd
}

// printCIJobs is shared by ci status and runners jobs so every reason and
// GitHub-only job's name/link survives either text door.
func printCIJobs(w io.Writer, jobs []orchapi.CIJob) error {
	if len(jobs) == 0 {
		return nil
	}
	plain := func(s string) string { return dash(strings.Join(strings.Fields(s), " ")) }
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tREPO\tNAME\tSTATE\tWHERE\tUPDATED\tLIMIT\tSOURCE\tREASON\tURL")
	for _, j := range jobs {
		limit := "-"
		if j.LimitS > 0 {
			limit = strconv.Itoa(j.LimitS) + " s"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, plain(j.Repo), plain(j.Name), plain(j.State), plain(j.Target), plain(j.Updated), limit, plain(j.LimitSource), plain(j.Why), plain(j.URL))
	}
	return tw.Flush()
}
