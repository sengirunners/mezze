package main

// Owned by the tailnet builder: `zm tailnet up|down|status`, `zm peers`.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/secrets"
)

func init() {
	tn := &cobra.Command{Use: "tailnet", Short: "Serve this daemon on your tailnet (the only network door; every caller needs a token or a mapped login)"}
	var up api.TailnetUpRequest
	upCmd := &cobra.Command{
		Use: "up", Short: "Join the tailnet (prints a login URL the first time)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if up.AuthKey == "" {
				if v, err := secrets.Get(cmd.Context(), secrets.Env("TS_AUTHKEY")); err == nil {
					up.AuthKey = v.Reveal()
				}
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.TailnetStatus, error) { return c.TailnetUp(ctx, up) }, tailnetLine)
		},
	}
	upCmd.Flags().StringVar(&up.Hostname, "hostname", "", "tailnet node name, potluck-<name> (or zm-<name>) so other daemons find it (default: config tailnet.hostname, else zm-<machine> for a node made before the Potluck rename, else potluck-<machine>)")
	upCmd.Flags().StringVar(&up.AuthKey, "auth-key", "", "Tailscale auth key, used once and never saved (default $TS_AUTHKEY, else interactive login); restarts a node still waiting for a login")
	upCmd.Flags().StringVar(&up.ControlURL, "control-url", "", "coordination server (e.g. Headscale)")
	down := &cobra.Command{
		Use: "down", Short: "Leave the tailnet", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.TailnetStatus, error) { return c.TailnetDown(ctx) }, tailnetLine)
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Tailnet node status", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.TailnetStatus, error) { return c.Tailnet(ctx) }, tailnetLine)
		},
	}
	tn.AddCommand(upCmd, down, status)
	peers := &cobra.Command{
		Use: "peers", Short: "Other zm nodes on the tailnet", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Peer, error) { return c.Peers(ctx) },
				func(ps []api.Peer) string {
					var b strings.Builder
					if len(ps) == 0 {
						return "no zm peers seen (is this daemon on the tailnet? zm tailnet status)"
					}
					for _, p := range ps {
						state := "online"
						if !p.Online {
							state = "offline"
							if !p.LastSeen.IsZero() {
								state += " since " + p.LastSeen.Local().Format(time.DateTime)
							}
						}
						fmt.Fprintf(&b, "%-20s %-8s %-36s %s %s\n", p.Machine, p.Version, p.URL, p.Owner, state)
					}
					return b.String()
				})
		},
	}
	rootCmd.AddCommand(tn, peers)
}

func tailnetLine(s api.TailnetStatus) string {
	switch {
	case s.AuthURL != "":
		return "log in: " + s.AuthURL
	case s.Running:
		return "on the tailnet as " + s.URL
	case s.Error != "":
		return "tailnet error: " + s.Error
	}
	return "not on a tailnet"
}
