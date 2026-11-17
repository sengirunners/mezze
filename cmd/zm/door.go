package main

// `zm door [on|off|tasks|add|rotate|rm|deliveries]` (SPEC 18.4).

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

func init() {
	d := &cobra.Command{Use: "door", Short: "A public URL per webhook task, forwarded to the machine that owns it",
		Long: `The door gives each of this machine's webhook tasks (run by the cockpit app) a
stable public URL at the team project's authority (its tunnel host). Deliveries are
queued there and forwarded to this machine's app in order, retried while it is
offline (72 h). The app still checks each delivery's signature; zm never holds the
secret. Name the team project with -P. The app calls the same routes over the socket.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.DoorStatus, error) {
				return orchclient.Door(ctx, c, p)
			}, doorStatusText)
		}}
	d.AddCommand(doorToggleCmd("on", true), doorToggleCmd("off", false), doorTasksCmd(), doorAddCmd(), doorRotateCmd(), doorRmCmd(), doorDeliveriesCmd())
	rootCmd.AddCommand(d)
}

func doorProject() (string, error) {
	if p := explicitProject(); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("name the team project with -P <slug>")
}

func doorStatusText(st orchapi.DoorStatus) string {
	state := "off"
	if st.On {
		state = "on"
	}
	s := fmt.Sprintf("door %s for %s (%d tasks)", state, st.Machine, st.Tasks)
	switch {
	case !st.Available:
		s += "\nthe authority serves no door yet: door.listen in its config, then zm tunnel setup there"
	case st.BaseURL == "":
		s += "\nthe authority has no public URL (tunnel.url or door.public_url)"
	default:
		s += "\nURLs start " + st.BaseURL + "/v0/door/hook/"
	}
	return s
}

func doorToggleCmd(use string, on bool) *cobra.Command {
	return &cobra.Command{Use: use, Short: "Turn the door " + use + " for this machine's tasks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.DoorStatus, error) {
				return orchclient.SetDoor(ctx, c, p, on)
			}, doorStatusText)
		}}
}

func doorTaskText(t orchapi.DoorTask) string {
	u := t.URL
	if u == "" {
		u = "(no URL: the door is off or the authority has no public URL)"
	}
	s := t.Task + "  " + u + "\n  -> " + t.Target
	if t.Queued > 0 {
		s += fmt.Sprintf("  (%d queued)", t.Queued)
	}
	return s
}

func doorTasksCmd() *cobra.Command {
	return &cobra.Command{Use: "tasks", Aliases: []string{"ls"}, Short: "List this machine's door tasks and their URLs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.DoorTask, error) {
				return orchclient.DoorTasks(ctx, c, p)
			},
				func(ts []orchapi.DoorTask) string {
					if len(ts) == 0 {
						return "no door tasks"
					}
					var b strings.Builder
					for _, t := range ts {
						b.WriteString(doorTaskText(t) + "\n")
					}
					return b.String()
				})
		}}
}

func doorAddCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{Use: "add <task> <target>", Short: "Give a webhook task a door URL (target: its hook URL on this machine)",
		Example: `  zm door add main-red http://127.0.0.1:3773/api/hooks/main-red/<token> -P zm`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.DoorTask, error) {
				return orchclient.PutDoorTask(ctx, c, p, args[0], orchapi.DoorTaskRequest{Target: args[1], Label: label})
			}, doorTaskText)
		}}
	cmd.Flags().StringVar(&label, "label", "", "a name to show")
	return cmd
}

func doorRotateCmd() *cobra.Command {
	return &cobra.Command{Use: "rotate <task>", Short: "Mint a new URL for a task (the old one stops at once)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.DoorTask, error) {
				return orchclient.RotateDoorTask(ctx, c, p, args[0])
			}, doorTaskText)
		}}
}

func doorRmCmd() *cobra.Command {
	return &cobra.Command{Use: "rm <task>", Short: "Remove a task's door URL and its queue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.DoorTask, error) {
				return orchclient.RemoveDoorTask(ctx, c, p, args[0])
			},
				func(t orchapi.DoorTask) string { return "removed " + t.Task })
		}}
}

func doorDeliveriesCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{Use: "deliveries <task>", Short: "A task's last deliveries, newest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := doorProject()
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.DoorDelivery, error) {
				return orchclient.DoorDeliveries(ctx, c, p, args[0], limit)
			}, func(ds []orchapi.DoorDelivery) string {
				if len(ds) == 0 {
					return "no deliveries"
				}
				var b strings.Builder
				for _, d := range ds {
					fmt.Fprintf(&b, "%s  %-9s %s", time.UnixMilli(d.Received).Format(time.DateTime), d.State, d.Event)
					if d.Status != 0 {
						fmt.Fprintf(&b, "  %d", d.Status)
					}
					if d.Error != "" {
						b.WriteString("  " + d.Error)
					}
					b.WriteString("\n")
				}
				return b.String()
			})
		}}
	cmd.Flags().IntVar(&limit, "limit", 0, "how many (1-100, default 20)")
	return cmd
}
