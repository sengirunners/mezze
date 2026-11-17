package main

// zm ci pr is Potluck's own command (package cli), run in this process
// against this machine's zm daemon: zm never calls cli.UseHome, so
// Potluck's commands read zm's home (ZM_HOME, zm.sock). The workflow edit
// lives in Potluck only, so a fix lands once.

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/spf13/cobra"
)

// potluckCICmd mounts Potluck's `ci <verb>`: its flags, help, refusals,
// output and exit codes are Potluck's, with Potluck's names said as zm's.
func potluckCICmd(use, short string) *cobra.Command {
	return potluckCmd([]string{"ci"}, use, short)
}

// potluckIsolationCmd is `zm isolation [test <size>]`: Potluck's isolation
// command (potluck cli/isolation.go, docs/isolation.md) on zm's home.
func potluckIsolationCmd() *cobra.Command {
	return potluckCmd(nil, "isolation [test <size>]", "Which sizes and targets this machine lends may run anyone's code, and each size's latest block test")
}

// potluckForgetCmd is `zm forget`: Potluck's forget command (potluck
// cli/forget.go), so the join refusal's "zm forget <project>" hint names a
// command zm has. It shows the bound key, asks, and passes its fingerprint.
func potluckForgetCmd() *cobra.Command {
	c := potluckCmd(nil, "forget <project id> (--keep-host-state | --drop-host-state) [--yes]", "Forget the authority key a project id is tied to here, so its machine can be joined at a new key")
	c.Annotations = map[string]string{poolWordsKey: "project"}
	return c
}

// poolWordsKey marks a mounted command whose Potluck words say "pool" for
// what zm calls a project (forget): those phrases are said as zm's too.
const poolWordsKey = "zm-pool-words"

// poolToProject is forget's "pool" phrases (potluck cli/forget.go), never a
// bare "pool", so a project named "carpool" is printed as it is.
var poolToProject = strings.NewReplacer("<pool id>", "<project id>", "a pool's ", "a project's ", "a pool ", "a project ",
	"the pool's ", "the project's ", "the pool ", "the project ", "the pool\n", "the project\n", "for pool ", "for project ")

// potluckCmd mounts Potluck's command parent+verb (verb: use's first word).
func potluckCmd(parent []string, use, short string) *cobra.Command {
	verb, _, _ := strings.Cut(use, " ")
	name := strings.Join(append(append([]string(nil), parent...), verb), " ")
	c := &cobra.Command{
		Use: use, Short: short,
		DisableFlagParsing: true, // Potluck parses its own flags (and --help)
		RunE: func(cmd *cobra.Command, args []string) error {
			args, jsonOut, help := potluckArgs(args)
			flagJSON = flagJSON || jsonOut // zm's own refusals below, as zm prints them
			if !help {
				if err := zmDaemonFor(cmd, name); err != nil {
					return err
				}
			}
			// Potluck's autostart would run zm daemon itself, past zm's
			// autostart lock and its no-autostart list.
			if err := os.Setenv(cli.EnvNoAutostart, "1"); err != nil {
				return err
			}
			args = append(strings.Fields(name), args...)
			pool := cmd.Annotations[poolWordsKey] != "" && !flagJSON // JSON is data: printed as it is
			if code := potluckRun(cmd.Context(), args, os.Stdout, os.Stderr, pool); code != 0 {
				return exitSilently(code) // Potluck said why
			}
			return nil
		},
	}
	// zm help <name> is Potluck's own help (its flags), as <name> --help is.
	c.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		_ = potluckRun(ctx, append(strings.Fields(name), "--help"), cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.Annotations[poolWordsKey] != "")
	})
	return c
}

// potluckRun runs Potluck's CLI with its words said as zm's.
func potluckRun(ctx context.Context, args []string, stdout, stderr io.Writer, pool bool) int {
	return cli.Run(ctx, args, os.Stdin, zmWords{stdout, pool}, zmWords{stderr, pool})
}

// potluckArgs drops zm's --as (the broker's routes are the socket owner's,
// never a session's), says zm's --project as Potluck's --pool (a pool is a
// zm project), and says whether --json or help was asked for.
func potluckArgs(in []string) (args []string, jsonOut, help bool) {
	for i := 0; i < len(in); i++ {
		a := in[i]
		switch {
		case a == "--":
			return append(args, in[i:]...), jsonOut, help
		case a == "--as":
			i++
			continue
		case strings.HasPrefix(a, "--as="):
			continue
		case a == "--project":
			a = "--pool"
		case strings.HasPrefix(a, "--project="):
			a = "--pool=" + strings.TrimPrefix(a, "--project=")
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "--json="):
			v := strings.ToLower(strings.TrimPrefix(a, "--json="))
			jsonOut = v != "false" && v != "0" && v != "f"
		case a == "-h" || a == "--help":
			help = true
		}
		args = append(args, a)
	}
	return args, jsonOut, help
}

// zmDaemonFor starts this machine's zm daemon as any zm command does, before
// Potluck reads its arguments (a failed start is zm's error; a daemon that
// is down with autostart off is Potluck's to report, exit 3). The broker's routes are local (the socket's owner), so a remote
// client ($ZM_HOST) is refused, not answered by an empty daemon beside it.
func zmDaemonFor(cmd *cobra.Command, name string) error {
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	if client.New(cfg).Socket == "" {
		return api.Errorf(api.CodeBadRequest, "zm %s reads this machine's own daemon, not a remote daemon", name).
			WithHint("run it on the machine that lends, without ZM_HOST")
	}
	if autostartAllowed(cmd, os.Getenv) {
		return autostart(cmd.Context(), cfg)
	}
	return nil
}

// zmWords says Potluck's command names as zm's in what Potluck writes (ci
// pr prints no JSON but its errors under --json). Potluck prints a diff of the repository's workflow, and the
// pull-request script, each in one write: those are shown as they are.
type zmWords struct {
	w    io.Writer
	pool bool // say forget's pool phrases as project ones (poolToProject)
}

// Refusals the daemon sends zm commands are said as zm's by mcp.ZmCommands.
var potluckToZm = strings.NewReplacer("potluck ci ", "zm ci ", "potluck runners ", "zm runners ",
	"potluck isolation", "zm isolation", "potluck forget", "zm forget", "potluck host share", "zm host share", "$POTLUCK_HOME", "$ZM_HOME",
	"potluck: ", "zm: ", "the potluck daemon", "the zm daemon", "potluck daemon", "zm daemon")

func (z zmWords) Write(p []byte) (int, error) {
	s := string(p)
	if !strings.HasPrefix(s, "--- a/") && !strings.HasPrefix(s, "#!") {
		s = potluckToZm.Replace(s)
		if z.pool {
			s = poolToProject.Replace(s)
			if rest, ok := strings.CutPrefix(s, "pool "); ok { // forget's question: "pool <id> is tied to..."
				s = "project " + rest
			}
		}
	}
	if _, err := io.WriteString(z.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

func init() { rootCmd.AddCommand(potluckIsolationCmd(), potluckForgetCmd()) }
