package main

// `zm connection add|ls|show|rm|check` (SPEC 24).

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/connections"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

func init() {
	c := &cobra.Command{Use: "connection", Aliases: []string{"connections"}, Short: "Show outside systems (CI, infra) from their own summaries",
		Long: `A connection is an outside system that answers GET with a summary in
zm.connection.show/1: headline rows (ok, warn, red), meters (used,
reserved, total) and running items, every one with an account, plus as_of.
zm fetches it when someone looks (at most every 15 seconds), checks it
against the contract, keeps the last good one, and says plainly when it is
stale or unusable. zm connection check validates a summary file offline.`}
	c.AddCommand(connectionAddCmd(), connectionLsCmd(), connectionShowCmd(), connectionRmCmd(), connectionCheckCmd())
	rootCmd.AddCommand(c)
}

func connectionAddCmd() *cobra.Command {
	var tokenFile, tokenRef, note string
	var allowPrivate bool
	cmd := &cobra.Command{
		Use:   "add <name> <endpoint>",
		Short: "Register a provider for a project (-P)",
		Example: `  zm connection add "Acme CI" https://ci.example.com/zm/show -P acme --token-file ~/.ci-token
  zm connection add "Build farm" unix:/run/farm/zm.sock -P acme --token-ref keychain:farm/zm`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := orchapi.NewConnectionRequest{Name: args[0], Endpoint: args[1], Project: explicitProject(), TokenRef: tokenRef,
				AllowPrivate: allowPrivate, Note: note}
			if req.Project == "" {
				return fmt.Errorf("name the project with -P <slug>[/<area>]")
			}
			if tokenFile != "" {
				v, err := readSecretFile(cmd.Context(), tokenFile)
				if err != nil {
					return err
				}
				req.Token = strings.TrimSpace(v.Reveal())
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Connection, error) {
				return orchclient.AddConnection(ctx, c, req)
			}, func(r orchapi.Connection) string { return "added connection " + r.ID + " (" + r.Name + ")" })
		},
	}
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the provider's bearer token from this file")
	cmd.Flags().StringVar(&tokenRef, "token-ref", "", "a secret reference read at each fetch (file:, keychain:, env:); machine owner only")
	cmd.Flags().BoolVar(&allowPrivate, "allow-private", false, "allow http and loopback, private or tailnet addresses (machine owner only)")
	cmd.Flags().StringVar(&note, "note", "", "a note shown in zm connection ls")
	return cmd
}

func connectionLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List the connections you may read", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ConnectionList, error) {
				l, err := orchclient.Connections(ctx, c, flagProject)
				return orchapi.ConnectionList{Connections: l}, err
			}, func(l orchapi.ConnectionList) string {
				if len(l.Connections) == 0 {
					return "no connections"
				}
				var b strings.Builder
				for _, x := range l.Connections {
					scope := x.Project
					if x.Area != "" {
						scope += "/" + x.Area
					}
					fmt.Fprintf(&b, "%s  %-16s %s", x.ID, scope, x.Name)
					if x.Endpoint != "" {
						b.WriteString("  " + x.Endpoint)
					}
					b.WriteString("\n")
				}
				return b.String()
			})
		},
	}
}

// connectionID resolves a name (in -P's project, else any you may read) to an id.
func connectionID(ctx context.Context, c *client.Client, ref string) (string, error) {
	if ids.Valid(strings.ToUpper(ref)) {
		return ref, nil
	}
	l, err := orchclient.Connections(ctx, c, flagProject)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, x := range l {
		if strings.EqualFold(x.Name, ref) {
			hits = append(hits, x.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", api.NotFound("connection", ref)
	}
	return "", api.Errorf(api.CodeBadRequest, "%d connections are named %q", len(hits), ref).WithHint("name the project with -P, or pass the id")
}

func connectionShowCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use: "show <id|name>", Short: "Show a connection's summary, and whether it is fresh", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ConnectionShow, error) {
				id, err := connectionID(ctx, c, args[0])
				if err != nil {
					return orchapi.ConnectionShow{}, err
				}
				return orchclient.ShowConnection(ctx, c, id, account)
			}, func(s orchapi.ConnectionShow) string { return connectionText(s, time.Now()) })
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "only this account's rows, meters and items")
	return cmd
}

func connectionText(s orchapi.ConnectionShow, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", s.Connection.Name, s.State)
	if s.Reason != "" {
		b.WriteString(" (" + s.Reason + ")")
	}
	b.WriteString("\n")
	if s.Summary == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "updated %s ago\n", connections.Ago(max(now.Sub(s.Summary.AsOf), 0)))
	for _, r := range s.Summary.Headline {
		fmt.Fprintf(&b, "  %-4s %s", r.State, r.Label)
		if r.Count != nil {
			fmt.Fprintf(&b, " %d", *r.Count)
		}
		if r.Since != nil {
			fmt.Fprintf(&b, " for %s", connections.Ago(max(now.Sub(*r.Since), 0)))
		}
		if r.Detail != "" {
			b.WriteString(": " + r.Detail)
		}
		b.WriteString("\n")
	}
	for _, m := range s.Summary.Meters {
		fmt.Fprintf(&b, "  %s: %g", m.Label, m.Used)
		if m.Reserved > 0 {
			fmt.Fprintf(&b, " (+%g reserved)", m.Reserved)
		}
		fmt.Fprintf(&b, " of %g %s\n", m.Total, m.Unit)
	}
	for _, r := range s.Summary.Running {
		fmt.Fprintf(&b, "  > %s", r.Title)
		if r.Owner != "" {
			b.WriteString(" (" + r.Owner + ")")
		}
		if r.Started != nil {
			fmt.Fprintf(&b, " %s", connections.Ago(max(now.Sub(*r.Started), 0)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func connectionRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <id|name>", Short: "Remove a connection", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Connection, error) {
				id, err := connectionID(ctx, c, args[0])
				if err != nil {
					return orchapi.Connection{}, err
				}
				return orchclient.RemoveConnection(ctx, c, id)
			}, func(r orchapi.Connection) string { return "removed connection " + r.ID + " (" + r.Name + ")" })
		},
	}
}

func connectionCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use: "check <file|->", Short: "Check a summary against zm.connection.show/1 (offline; for provider authors)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var r io.Reader = os.Stdin
			if args[0] != "-" {
				f, err := os.Open(expandTilde(args[0]))
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			b, err := io.ReadAll(io.LimitReader(r, connections.MaxBody+1))
			if err != nil {
				return err
			}
			now := time.Now()
			s, err := connections.Validate(b, now)
			if err != nil {
				return err
			}
			out := connections.Judge(b, &now, &now, "", now)
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("ok: %s, %d headline rows, %d meters, %d running; it reads as %s now\n",
				orchapi.ConnectionSchema, len(s.Headline), len(s.Meters), len(s.Running), out.State)
			return nil
		},
	}
}
