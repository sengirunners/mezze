package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// zm t3 enable / disable: this machine's Mezze app switch (SPEC 8 "Mezze app on a
// machine"). Only the machine's owner, in a terminal of their own: never
// from a zm session or a remote client. It writes the "t3" key of
// config.json, then restarts a running daemon through the safe swap (zm
// daemon restart: its preflight, refused while managed sessions would end).

func t3SwitchCmd(on bool) *cobra.Command {
	use, short := "enable", "Let a team project's owner start the companion cockpit app on this machine (zm t3 <machine>): writes \"t3\" to zm's config.json (its checkout defaults to <zm home>/t3code, cloned on first use) and restarts the daemon"
	if !on {
		use, short = "disable", "Stop accepting cockpit app starts here: removes \"t3\" from zm's config.json and restarts the daemon (a running server keeps running)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t3SwitchOwner(os.Getenv); err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			changed, err := orchcore.SetT3(cfg.Home, on)
			if err != nil {
				return err
			}
			state := map[bool]string{true: "enabled", false: "disabled"}[on]
			if !changed {
				fmt.Printf("the Mezze app is already %s on %s\n", state, cfg.Machine)
				return nil
			}
			fmt.Printf("the Mezze app %s on %s (%s)\n", state, cfg.Machine, cfg.Path(config.FileConfig))
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
			_, herr := client.NewUnix(cfg.SocketPath()).Health(ctx)
			cancel()
			if herr != nil {
				fmt.Println("the daemon is not running: it reads the change when it starts")
				return nil
			}
			if err := runSwap(cmd.Context(), swapOpts{what: "restart"}); err != nil {
				return fmt.Errorf("config.json is written, but the daemon did not restart: %w (zm daemon restart when it can)", err)
			}
			return nil
		},
	}
}

// t3SwitchOwner refuses a zm session (an agent) and a remote client: the
// switch is the machine owner's consent.
func t3SwitchOwner(getenv func(string) string) error {
	if getenv("ZM_SESSION") != "" || getenv(config.EnvAs) != "" {
		return api.Errorf(api.CodeForbidden, "only this machine's owner turns the Mezze app on or off here, not a zm session").
			WithHint("run it in a terminal of your own on this machine")
	}
	if config.HostEnv != "" && getenv(config.HostEnv) != "" {
		return api.Errorf(api.CodeForbidden, "zm t3 enable and disable change this machine's own config: unset %s", config.HostEnv)
	}
	return nil
}
