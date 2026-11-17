package main

// `zm owed` (SPEC 8 "What a session still owes"): what a session, or all of a
// person's sessions, still owe, read from zm's records only. It lists and
// settles nothing.

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

func init() { rootCmd.AddCommand(owedCmd()) }

func owedCmd() *cobra.Command {
	var q orchclient.OwedQuery
	cmd := &cobra.Command{
		Use: "owed", Short: "What a session (or all of a person's sessions) still owes, from zm's records", Args: cobra.NoArgs,
		Long: `List what is still owed: open pull requests, work items held, cards filed that
wait for an answer or are approved but not done, and sessions started that
still run. Inside a session it lists that session's; otherwise your own
sessions'. What zm keeps no record of is listed as unknown, never left out.
It settles nothing.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OwedList, error) {
				return orchclient.Owed(ctx, c, q)
			}, owedText)
		},
	}
	f := cmd.Flags()
	f.StringVar(&q.Session, "session", "", "a session's address or name (default: this session)")
	f.StringVar(&q.Person, "person", "", `"me" or your own name: all your sessions' (default outside a session)`)
	f.StringVar(&q.Since, "since", "", "how far back a person's ended sessions count, like 168h (default 336h)")
	return cmd
}

// owedText is the plain list: one line per owed item, then what is unknown.
func owedText(l orchapi.OwedList) string {
	var b strings.Builder
	fmt.Fprintf(&b, "owed by %s %s (%d sessions)\n", l.For, l.Subject, len(l.Sessions))
	if len(l.Items) == 0 {
		b.WriteString("nothing owed that zm can see\n")
	}
	for _, it := range l.Items {
		fmt.Fprintf(&b, "%-13s %s  %s [%s]", it.Kind, it.Ref, it.Title, it.State)
		if it.Since != nil {
			fmt.Fprintf(&b, " since %s", it.Since.Local().Format(time.DateTime))
		}
		b.WriteString("\n")
	}
	for _, g := range l.Unknown {
		fmt.Fprintf(&b, "unknown       %s: %s\n", g.Name, g.Why)
	}
	return b.String()
}
