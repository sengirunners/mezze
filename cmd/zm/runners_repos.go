package main

// zm runners allow|disallow|repos: which repositories the broker serves.
// These are potluck runners allow|disallow|repos (potluck cli/runners.go)
// under zm's name. The daemon (potluck runners/repos.go) holds the rule: a
// repository is served only once its pool's owner connects it, and a public
// one only with --public, the owner's yes for its own branches.

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

func runnersAllowCmd() *cobra.Command {
	var req runners.AllowRequest
	c := &cobra.Command{
		Use:   "allow <owner/repo> [--public]",
		Short: "Serve a repository's jobs on this pool (a public one needs --public)",
		Long: `Connects a repository to the pool (its owner, on this machine): its jobs that
ask for the pool are served from the next one on. The App must be installed
on the repository's account, given the repository, and linked (zm runners
link). A repository not connected is never served.

A public repository needs --public, your yes for it, given while it is
public (it is refused for a private one). It serves the repository's own
branches: pushes, manual runs, schedules, and pull requests from its own
branches, that is code from people who can push to it. A pull request from
a fork is never served for a public repository, and neither is a
pull_request_target, workflow_run or issue_comment run. The broker then
reads the repository's rule for outside contributors' runs, now and every
hour, and warns if it lets any start without your approval.

Running it again changes the yes; zm runners disallow undoes it.

After an upgrade from a zm that served every repository of a linked
installation: only repositories with a job the broker took (not one it
ignored) in the last 30 days, through an installation still linked, were
connected for you (by "upgrade"), and a public one is not served until
you give --public. Run zm runners repos, then zm runners allow each
repository missing from it (--public for a public one).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			req.Repo = args[0]
			var rp runners.Repo
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/repos", req, &rp); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"repo": rp})
			}
			how := "served"
			if rp.Public {
				how = "served while public: its own branches only, never a fork's pull request"
			}
			fmt.Printf("zm: %s connected (%s), from its next job on\n", rp.Repo, how)
			if rp.Warning != "" {
				fmt.Printf("zm: warning: %s\n", rp.Warning)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&req.Public, "public", false, "yes: serve this public repository's own branches (never a fork's pull request)")
	return c
}

func runnersDisallowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disallow <owner/repo>",
		Short: "Stop serving a repository's jobs (a job that runs already finishes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			o, n, ok := strings.Cut(args[0], "/")
			if !ok || o == "" || n == "" || strings.Contains(n, "/") {
				return fmt.Errorf("%q is not owner/repo", args[0])
			}
			if err := cl.Do(cmd.Context(), "DELETE", "/v0/runners/repos/"+url.PathEscape(o)+"/"+url.PathEscape(n), nil, nil); err != nil {
				return err
			}
			fmt.Printf("zm: %s disconnected: its next jobs are not served\n", args[0])
			return nil
		},
	}
}

func runnersReposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "The repositories whose jobs are served, and any warning about their settings",
		Long: `Lists the connected repositories. After an upgrade, a repository is missing from
the list (and its jobs are not served) unless the broker took a job of it
(not one it ignored) in the last 30 days through an installation still
linked. A public one connected by "upgrade" is listed but not served until
zm runners allow <owner/repo> --public; until the broker has read its
visibility (minutes after the daemon starts) its row says it may be public. Connect each such repository
with zm runners allow <owner/repo>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			repos, err := runnersRepos(cmd.Context(), cl)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"repos": repos})
			}
			printRunnersRepos(os.Stdout, repos)
			return nil
		},
	}
}

// runnersRepos is GET /v0/runners/repos: the connected repositories the
// caller may read.
func runnersRepos(ctx context.Context, cl *client.Client) ([]runners.Repo, error) {
	var out struct {
		Repos []runners.Repo `json:"repos"`
	}
	if err := cl.Do(ctx, "GET", "/v0/runners/repos", nil, &out); err != nil {
		return nil, err
	}
	if out.Repos == nil {
		out.Repos = []runners.Repo{}
	}
	return out.Repos, nil
}

func printRunnersRepos(w io.Writer, repos []runners.Repo) {
	if len(repos) == 0 {
		fmt.Fprintln(w, "repos       none connected: no job is served (zm runners allow <owner/repo>)")
		return
	}
	fmt.Fprintln(w, "repos")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  REPO\tVISIBILITY\tPUBLIC YES\tBY\tCHECKED\tWARNING")
	for _, r := range repos {
		yes := "no"
		if r.Public {
			yes = "yes"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.Repo, dash(r.Visibility), yes, r.By, dash(r.Checked), dash(orchclient.RepoWarning(r)))
	}
	_ = tw.Flush()
	for _, r := range repos {
		if r.By == "upgrade" {
			fmt.Fprintln(w, "  \"upgrade\" connected only repositories with a job taken (not ignored) in the last 30 days through a still-linked installation; any other repository you serve: zm runners allow <owner/repo> (--public for a public one)")
			break
		}
	}
}
