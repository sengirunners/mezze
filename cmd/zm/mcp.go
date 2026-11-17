package main

// Owned by the integrations builder: `zm mcp`.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/mezze/internal/orchapi"
	potluckcli "github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

func init() {
	var sessions, compute bool
	var sandboxes string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve zm as an MCP server on stdio (for MCP-capable agents)",
		Long: `Serve zm's verbs (whoami, project_current, mail_*, work_*, session_list) as MCP
tools on stdin/stdout. Every tool calls the daemon as you: the same --as, --project,
$ZM_HOST and $ZM_TOKEN as the CLI. Started by an agent (CLAUDE_CODE_SESSION_ID or
CODEX_THREAD_ID set), tools act as that agent's session, registering it if needed.
--sandboxes=auto adds the sandbox tools only if the project has a box run target
when the server starts. --sandboxes=only serves just sandbox tools, so the
compute plugin can run alongside the default plugin without duplicate tools.
--compute adds compute_status, compute_doctor (with probe: --probe), compute_setup and
compute_pause: zm ci status, zm ci doctor, zm ci add and zm lend pause as
--json (the broker's routes answer only this machine's own socket).
compute_setup never waits or prompts: a person's step comes back as a step
of kind ask. compute_pause refuses (state refused) while a job runs or another
zm lend is going on, or when the lend asks another program (admission).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cfg, err := newClient()
			if err != nil {
				return err
			}
			o := mcpOptions(c, cfg.Machine, flagAs, os.Getenv)
			o.Sessions, o.Compute, o.Setup, o.Pause, o.Probe = sessions, compute, computeSetup, computePause, computeProbe
			if compute { // compute_setup runs Potluck's ci pr, whose autostart would bypass zm's (and its lock)
				if err := os.Setenv(potluckcli.EnvNoAutostart, "1"); err != nil {
					return err
				}
			}
			o.SandboxOnly = strings.EqualFold(sandboxes, "only")
			if o.Sandboxes, err = sandboxesOn(cmd.Context(), sandboxes, computeclient.Owner{C: c}.RunTargets); err != nil {
				return err
			}
			return mcp.Serve(cmd.Context(), c, o, os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&sessions, "sessions", false, "add the structured-session tools (session_events, session_send)")
	cmd.Flags().BoolVar(&compute, "compute", false, "add the compute tools (compute_status, compute_doctor, compute_setup, compute_pause: zm ci status, doctor, add and zm lend pause as JSON)")
	cmd.Flags().StringVar(&sandboxes, "sandboxes", "off", "add the sandbox tools (sandbox_create, _exec, _list, _renew, _stop): on, off, only (no default tools), or auto (only if the project has a box run target)")
	cmd.Flags().Lookup("sandboxes").NoOptDefVal = "on"
	rootCmd.AddCommand(cmd)
}

// sandboxesOn reads --sandboxes. auto asks the daemon once, with a short
// deadline, whether the project has a box binding; no answer means no tools
// (an MCP server must start even when the daemon is down).
func sandboxesOn(ctx context.Context, v string, targets func(context.Context, bool) (computeapi.RunTargetList, error)) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "t", "1", "only":
		return true, nil
	case "off", "false", "f", "0", "":
		return false, nil
	case "auto":
	default:
		return false, fmt.Errorf("--sandboxes: want on, off, only or auto, not %q", v)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	l, err := targets(ctx, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zm mcp: no sandbox tools: run targets: %v\n", err)
		return false, nil
	}
	for _, t := range l.Targets {
		if t.Kind == "box" {
			return true, nil
		}
	}
	return false, nil
}

func mcpOptions(c *client.Client, machine, as string, getenv func(string) string) mcp.Options {
	// A managed-session marker already names the caller, even if ZM_AS was
	// dropped. Do not replace it by registering an inherited runtime id.
	// An address only inherited through the environment (not --as) is
	// checked against the runtime before tools use it.
	var o mcp.Options
	if as != "" || c.Token != "" {
		return o
	}
	agent, rid := integrations.EnvSession(getenv)
	if rid == "" {
		return o
	}
	cwd, _ := os.Getwd()
	own := orchapi.RegisterRequest{Agent: agent, RuntimeID: rid, Cwd: cwd}
	inherited := getenv(config.EnvAs)
	if inherited == "" {
		inherited = getenv("ZM_SESSION")
	}
	switch {
	case inherited == "":
		o.Register = &own
	case machine != "" && strings.HasPrefix(inherited, machine+"/"):
		// A local session address: provable against this process tree.
		// Another machine's session (a team address) is used as before.
		o.Inherited = &mcp.Inherited{Address: inherited, Own: own, NearestAgent: integrations.NearestAgent}
	}
	return o
}

// computeSetup is MCP compute_setup: zm ci add --json on the tool's client,
// which never starts a daemon (zm mcp turns Potluck's autostart off too).
// public is never set: a public repository's yes is the owner's own run of
// zm ci add <repo> --public, which compute_setup answers as an ask.
func computeSetup(ctx context.Context, c *client.Client, in mcp.SetupRequest) (any, error) {
	if in.Org != "" && in.Own {
		return nil, api.Errorf(api.CodeBadRequest, "org or own_account, not both")
	}
	var said []string
	o := ciAddOpts{mine: in.Mine, job: in.Job, workflow: in.Workflow, org: in.Org, orgSet: in.Org != "" || in.Own, json: true,
		say: func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }}
	res, err := ciAddWith(ctx, c, in.Repos, o)
	res.Said = append([]string{}, said...)
	return res, err
}

// computeProbe is MCP compute_doctor with probe: zm ci doctor <repo>
// --probe --json, never with the owner's --public (ProbeRequest has none).
func computeProbe(ctx context.Context, c *client.Client, repo string, in mcp.ProbeRequest) (any, error) {
	if c.Socket == "" { // Potluck's probe reads this machine's own daemon
		return nil, api.Errorf(api.CodeBadRequest, "compute_doctor's probe reads this machine's own daemon, not a remote daemon")
	}
	return ciDoctorProbe(ctx, repo, ciProbeOpts{labels: in.Labels, size: in.Size, osName: in.OS})
}

// computePause is MCP compute_pause: zm lend pause --json on the tool's
// client, for this machine (its name) and $ZM_HOME.
func computePause(ctx context.Context, c *client.Client) (any, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	var f hostFlags
	return lendPauseResult(ctx, c, strings.ToLower(cfg.Machine), f.dir()) // raw error: its code reaches the agent
}
