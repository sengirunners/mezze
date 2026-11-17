package main

// Owned by the owners brick: zm login github (SPEC 22).

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func init() { rootCmd.AddCommand(loginCmd()) }

func loginCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "login", Short: "Link an outside account to your zm identity on this authority"}
	gh := &cobra.Command{
		Use:   "github",
		Short: "Link your GitHub account: approve in the browser; the authority talks to GitHub and keeps no token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			out, err := githubLogin(cmd.Context(), c, api.GitHubLoginRequest{Purpose: "link"}, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if l := out.Link; l != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "identity %s is linked to GitHub @%s (%s)\n", l.Identity, l.Login, l.Account)
			}
			return nil
		},
	}
	cmd.AddCommand(gh)
	return cmd
}

// githubLogin starts a login at the authority, shows the person where to
// approve it, and waits for its outcome. No GitHub token passes through
// the CLI: the authority receives it from GitHub and drops it.
func githubLogin(ctx context.Context, c *client.Client, req api.GitHubLoginRequest, msg io.Writer) (api.GitHubLoginOutcome, error) {
	st, err := c.StartGitHubLogin(ctx, req)
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	fmt.Fprintf(msg, "zm: authority %s asks GitHub to sign in %s.\nzm: open %s and enter %s (waiting, until %s).\nzm: enter a code only if you started this yourself.\n",
		st.Authority, st.For, st.VerificationURI, st.UserCode, st.Expires.Local().Format("15:04"))
	for {
		out, err := c.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 25000})
		if err != nil || !out.Pending {
			return out, err
		}
		if time.Now().After(st.Expires.Add(time.Minute)) {
			return out, fmt.Errorf("the GitHub login expired")
		}
	}
}
