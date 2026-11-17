package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

// askObjectCmd is zm ask object: an owner records, after the
// answer, that what a card set off should not have happened. It undoes
// nothing; zm measures counts it as harm.
func askObjectCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "object <id>",
		Short: "As an owner, record that an answered card's outcome should not have happened",
		Long: `Records your objection to an answered card: the action it set off was wrong, or you
would have wanted to be asked differently. It undoes nothing by itself and needs no
signature; zm measures counts it as harm. Only a project owner or admin may, and only on
an answered card. Asking twice keeps the first record.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				return orchclient.ObjectAsk(ctx, c, args[0], note)
			}, func(a orchapi.Ask) string {
				who := ""
				if a.Objection != nil {
					who = " by " + a.Objection.By
				}
				return strings.TrimSpace("ask " + a.ID + " objected" + who)
			})
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why, in one line (optional)")
	return cmd
}
