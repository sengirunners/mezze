package main

// Owned by the integrations builder: `zm install`, `zm uninstall`, `zm hook`.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func init() {
	rootCmd.AddCommand(installCmd(false), installCmd(true), hookCmd())
}

func installCmd(un bool) *cobra.Command {
	var o integrations.Options
	var yes, dryRun bool
	agents := strings.Join(integrations.Agents(), "|")
	use, short := "install <"+agents+">", "Show (or with --yes, make) the config changes that connect an agent to zm"
	if un {
		use, short = "uninstall <"+agents+">", "Show (or with --yes, make) the changes that remove what zm install added"
	}
	cmd := &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		Long: short + `.

claude: hooks in settings.json (SessionStart, UserPromptSubmit, Notification, Stop,
        SessionEnd -> zm hook claude <event>) and the zm MCP server (user scope) in
        ~/.claude.json. Honours $CLAUDE_CONFIG_DIR.
codex:  notify and [mcp_servers.zm] in config.toml, SessionStart,
        UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd and SubagentStop
        hooks in hooks.json. Honours $CODEX_HOME. Codex runs
        hooks.json hooks only after you trust them in Codex (/hooks); zm never
        trusts them for you, and this command reports whether you have.

The default is a dry run that prints the exact diff. Nothing is written without
--yes. Every edited file is backed up next to itself (<file>.zm-backup-<time>).
Running it again changes nothing. --home DIR installs under DIR instead of your
home and ignores $CLAUDE_CONFIG_DIR / $CODEX_HOME.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Agent = args[0]
			if o.Home == "" {
				if o.ConfigDir == "" {
					o.ConfigDir = os.Getenv(integrations.ConfigDirEnv[o.Agent])
				}
				h, err := os.UserHomeDir()
				if err != nil && o.ConfigDir == "" {
					return fmt.Errorf("no home directory: pass --home DIR")
				}
				o.Home = h
			}
			if o.PeoplesCommune == "" {
				o.PeoplesCommune = zmCommand()
			}
			o.Apply = yes && !(cmd.Flags().Changed("dry-run") && dryRun)
			run := integrations.Install
			if un {
				run = integrations.Uninstall
			}
			r, err := run(o)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Print(formatInstall(cmd.Name(), r))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "write the changes (default: dry run)")
	cmd.Flags().BoolVar(&yes, "apply", false, "same as --yes")
	_ = cmd.Flags().MarkHidden("apply")
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "only show what would change (the default; --dry-run beats --yes)")
	cmd.Flags().StringVar(&o.Home, "home", "", "home directory to install under (default: yours)")
	cmd.Flags().StringVar(&o.ConfigDir, "config-dir", "", "the agent's config dir (default $CLAUDE_CONFIG_DIR / $CODEX_HOME, else under --home)")
	cmd.Flags().StringVar(&o.PeoplesCommune, "zm", "", "command the hooks and MCP entry run (default: this zm)")
	return cmd
}

// zmCommand is the path hooks should run: the zm on PATH when it is this
// binary (a stable symlink survives upgrades), else this binary's path.
func zmCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "zm"
	}
	if p, err := exec.LookPath("zm"); err == nil {
		a, errA := os.Stat(p)
		b, errB := os.Stat(self)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return p
		}
	}
	return self
}

func formatInstall(verb string, r integrations.Result) string {
	var b strings.Builder
	if r.DryRun {
		fmt.Fprintf(&b, "zm %s %s: dry run, nothing written\n", verb, r.Agent)
	} else {
		fmt.Fprintf(&b, "zm %s %s\n", verb, r.Agent)
	}
	for _, f := range r.Files {
		switch {
		case f.Action == integrations.ActionUnchanged:
			fmt.Fprintf(&b, "\n%s: unchanged\n", f.Path)
			continue
		case r.DryRun:
			fmt.Fprintf(&b, "\n%s: would %s\n", f.Path, f.Action)
		default:
			fmt.Fprintf(&b, "\n%s: %sd\n", f.Path, f.Action)
		}
		for _, c := range f.Changes {
			fmt.Fprintf(&b, "  %s\n", c)
		}
		if f.Backup != "" {
			if r.DryRun {
				fmt.Fprintf(&b, "  backup -> %s\n", f.Backup)
			} else {
				fmt.Fprintf(&b, "  backed up to %s\n", f.Backup)
			}
		}
		if f.Diff != "" {
			b.WriteString(f.Diff)
		}
	}
	b.WriteString("\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	if r.Changed() {
		for _, n := range r.Next {
			fmt.Fprintf(&b, "then: %s\n", n)
		}
	}
	b.WriteString(trustLines(r))
	switch {
	case r.DryRun && r.Changed():
		b.WriteString("Run again with --yes to apply.\n")
	case !r.Changed() && len(r.Untrusted()) > 0:
		b.WriteString("No config changes needed; trust the hooks in Codex as above.\n")
	case !r.Changed():
		b.WriteString("Nothing to do.\n")
	}
	return b.String()
}

// hookCmd is what installed hooks run. It never fails the agent: errors go to
// stderr and it exits 0; a stopped daemon is silent.
func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <agent> <" + strings.Join(integrations.HookEvents(), "|") + "> [payload-json]",
		Short:  "Run by agent hooks: register the session, report its state, print the mail notice",
		Args:   cobra.RangeArgs(2, 3),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			in := integrations.HookInput{Agent: args[0], Event: args[1]}
			// One deadline for the whole hook, stdin included.
			ctx, cancel := context.WithTimeout(cmd.Context(), hookBound(in.Event))
			defer cancel()
			if len(args) == 3 {
				in.Payload = []byte(args[2]) // Codex notify passes its JSON as an argument
			} else {
				in.Payload = readHookStdin(ctx)
			}
			c, cfg, err := newClient()
			if err != nil {
				fmt.Fprintln(os.Stderr, "zm hook:", err)
				return nil
			}
			if c.Socket == "" {
				return nil // remote ($ZM_HOST): sessions register on their own machine only
			}
			in.Machine = cfg.Machine
			out, err := integrations.RunHook(ctx, func(as, cwd string) integrations.Daemon {
				cc := *c
				cc.As = as
				if cwd != "" {
					cc.Cwd = cwd
				}
				return orchclient.Hooks(&cc)
			}, in)
			if err != nil && !api.IsCode(err, api.CodeUnavailable) {
				fmt.Fprintln(os.Stderr, "zm hook:", err)
			}
			if out != "" {
				fmt.Println(out)
			}
			return nil
		},
	}
}

// Bounds on a hook end to end (stdin, connect, request): a wedged daemon
// (socket accepts, never answers) must not hold the agent; a stopped one
// fails at connect, at once. The prompt path (prompt, notification, stop,
// notify, and `zm notice`) only reports state and reads the notice, both
// best effort that the next hook repeats, so it gives up after promptBound.
// session-start and session-end register and end a session once, so they get
// the longer bound.
const (
	promptBound    = 300 * time.Millisecond
	lifecycleBound = 2 * time.Second
)

// hookBound is the end-to-end bound for one hook event.
func hookBound(event string) time.Duration {
	switch event {
	case integrations.HookSessionStart, integrations.HookSessionEnd:
		return lifecycleBound
	}
	return promptBound
}

// readHookStdin reads the hook's JSON from stdin, giving up when ctx ends (an
// open, silent stdin must never hang the agent) and on a terminal.
func readHookStdin(ctx context.Context) []byte {
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	ch := make(chan []byte, 1)
	go func() { ch <- readCapped(os.Stdin) }()
	select {
	case b := <-ch:
		return b
	case <-ctx.Done():
		return nil
	}
}

// trustLines says, for codex, whether Codex will run zm's hooks: it skips
// any hook the user has not trusted with /hooks, and zm never trusts them.
func trustLines(r integrations.Result) string {
	if len(r.Trust) == 0 {
		return ""
	}
	var states []string
	why := map[string]bool{}
	unsure := false
	for _, t := range r.Trust {
		states = append(states, t.Event+" "+t.State)
		why[t.State] = true
		unsure = unsure || t.Unverified
	}
	var b strings.Builder
	fmt.Fprintf(&b, "codex hooks: %s\n", strings.Join(states, ", "))
	switch {
	case why[integrations.TrustChanged]:
		// the "then:" line already says to trust them once written
	case why[integrations.TrustMissing]:
		b.WriteString("  zm's hooks are not in hooks.json: run zm install codex --yes\n")
	case unsure && !why[integrations.TrustDisabled]:
		// Codex records hook trust, just not under the keys zm reads.
		b.WriteString("  Codex has not recorded trust for these hooks under the keys zm reads (checked against Codex 0.166).\n" +
			"  If you already trusted them with /hooks, this zm cannot read that Codex version's state; otherwise open\n" +
			"  Codex, run /hooks, review zm's hooks and trust them. zm never trusts hooks for you.\n")
	case why[integrations.TrustUntrusted] || why[integrations.TrustDisabled]:
		b.WriteString("  Codex will not run them yet, so its sessions get no zm address and no mail notice.\n" +
			"  Open Codex, run /hooks, review zm's hooks and trust (or enable) them. zm never trusts hooks for you.\n")
	}
	return b.String()
}

// readCapped reads r to EOF, keeping the first api.MaxBody bytes and
// discarding the rest: an agent writing a large hook payload (a Write of a
// big file) must never get EPIPE because the hook stopped reading.
func readCapped(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, api.MaxBody))
	if len(b) == api.MaxBody {
		_, _ = io.Copy(io.Discard, r)
	}
	return b
}
