package main

// `zm secret keys [publish|revoke]`, `zm secret trust` (SPEC 17.12): the
// key directory a sealed secret is sent through.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/secretkeys"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func init() {
	c := &cobra.Command{Use: "secret", Short: "Hand a secret to another machine, sealed to its key (the key directory today)",
		Long: `A secret goes to a machine sealed to that machine's own key before it
leaves the sender, so the project's authority holds and relays only
ciphertext (SPEC 17.12). Today: the key directory. Each machine publishes
its seal key (zm secret keys publish); senders list keys and pin the one
they trust for each machine on first use; a changed key is refused until a
person compares its fingerprint and runs zm secret trust.`}
	keys := secretKeysCmd()
	keys.AddCommand(secretPublishCmd(), secretRevokeCmd())
	c.AddCommand(keys, secretTrustCmd())
	rootCmd.AddCommand(c)
}

// pinnedKey is a listed key with what this machine's pins say of it.
type pinnedKey struct {
	orchapi.SecretKey
	Pin string `json:"pin"`
}

func secretKeysCmd() *cobra.Command {
	var to string
	var all bool
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "List the published seal keys (-P), and whether this machine trusts each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cfg, err := newClient()
			if err != nil {
				return err
			}
			l, err := orchclient.SecretKeys(cmd.Context(), c, to, all)
			if err != nil {
				return err
			}
			pins := secretkeys.OpenPins(cfg.Home)
			out := make([]pinnedKey, 0, len(l))
			for _, k := range l {
				v, _, err := pins.Check(k)
				if err != nil {
					return err
				}
				out = append(out, pinnedKey{k, v})
			}
			if flagJSON {
				return printJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("no seal keys published (on each machine: zm secret keys publish)")
				return nil
			}
			for _, k := range out {
				line := fmt.Sprintf("%-20s %s  %-8s %s  %s", k.Machine, k.Fingerprint, k.State, k.Pin, k.Owner)
				if k.Pin == secretkeys.PinChanged {
					line += "  (KEY CHANGED: compare the fingerprint with that machine's zm secret keys, then zm secret trust)"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "only machine:<name> or human:<name>")
	cmd.Flags().BoolVar(&all, "all", false, "include replaced and revoked keys")
	return cmd
}

func secretPublishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish",
		Short: "Publish this machine's seal key in the project's directory (-P)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.SecretKey, error) {
				return orchclient.PublishSecretKey(ctx, c)
			}, func(k orchapi.SecretKey) string {
				return fmt.Sprintf("published %s's seal key in %s: %s", k.Machine, k.Project, k.Fingerprint)
			})
		},
	}
}

func secretRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <fingerprint>",
		Short: "End a seal key (its own machine, or an admin of the project): senders refuse it from then on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.SecretKey, error) {
				return orchclient.RevokeSecretKey(ctx, c, strings.TrimSpace(args[0]))
			}, func(k orchapi.SecretKey) string {
				return fmt.Sprintf("%s's seal key %s is %s", k.Machine, k.Fingerprint, k.State)
			})
		},
	}
}

func secretTrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust <fingerprint>",
		Short: "Trust this active key for its machine from now on (after comparing the fingerprint)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cfg, err := newClient()
			if err != nil {
				return err
			}
			fp := strings.TrimSpace(args[0])
			l, err := orchclient.SecretKeys(cmd.Context(), c, "", false)
			if err != nil {
				return err
			}
			var hit *orchapi.SecretKey
			for i := range l {
				if l[i].Fingerprint == fp {
					if hit != nil {
						return api.Errorf(api.CodeBadRequest, "key %s is listed in more than one project", fp).WithHint("name the project with -P")
					}
					hit = &l[i]
				}
			}
			if hit == nil {
				return api.NotFound("active secret key", fp)
			}
			if err := secretkeys.OpenPins(cfg.Home).Trust(*hit, time.Now()); err != nil {
				return err
			}
			fmt.Printf("this machine now trusts %s for %s\n", hit.Fingerprint, hit.Machine)
			return nil
		},
	}
}
