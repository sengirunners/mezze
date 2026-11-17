// Command zm is the one binary: the daemon (zm daemon) and the CLI that
// humans and agents share. Owned by the integrator; each module registers its
// own subcommands from cmd/zm/<module>.go in init():
//
//	func init() { rootCmd.AddCommand(sendCmd()) }
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/initgc"
	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// Global flags.
var (
	flagJSON    bool   // --json: print raw JSON instead of human text
	flagAs      string // --as: reply address (default $ZM_AS)
	flagProject string // --project: project for writes / filter for lists (beats $ZM_PROJECT)
)

var rootCmd = &cobra.Command{
	Use:   "zm",
	Short: "Direct every agent, on every machine, from any app",
	Long: `Mezze is the bus for coding agents, and zm is its command: mail,
work items, leases and sessions, grouped by project, served by a local daemon
over a Unix socket. Humans and agents use the same CLI. Mail is data, never
authority.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       versionLine(),
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "print raw JSON")
	rootCmd.PersistentFlags().StringVar(&flagAs, "as", "", "reply address to act as (default $ZM_AS)")
	rootCmd.PersistentFlags().StringVarP(&flagProject, "project", "P", "", "project slug (or slug/area); default: $ZM_PROJECT, then the repo you are in")
	rootCmd.AddCommand(daemonCmd(), whoamiCmd(), statusCmd(), eventsCmd())
}

// caughtSignals end a command's context. SIGHUP too: a closed terminal ends
// a command as Ctrl-C does, so what it holds (zm job run's box) is
// released. Not when started with SIGHUP ignored (nohup): catching it would
// undo that.
func caughtSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

func main() {
	initgc.Restore() // the GC was off for package init; first thing, always (TestMain too)
	groupCommands(rootCmd)
	ctx, stop := signal.NotifyContext(context.Background(), caughtSignals()...)
	err := rootCmd.ExecuteContext(ctx)
	stop()
	if err != nil {
		os.Exit(report(err))
	}
}

// report prints err for humans (or as JSON with --json) and returns the exit code:
// 1 for refusals and errors, 2 for usage, 3 when the daemon is not running.
func report(err error) int {
	var quiet exitSilently
	if errors.As(err, &quiet) {
		return int(quiet)
	}
	var ew exitWith
	if errors.As(err, &ew) {
		if ew.err != nil {
			report(ew.err)
		}
		return ew.code
	}
	err = orchclient.BoxCallError(os.Getenv, err)
	var e *api.Error
	if errors.As(err, &e) {
		e = mcp.ZmCommands(e)
		if flagJSON {
			_ = json.NewEncoder(os.Stderr).Encode(json.RawMessage(textclean.JSON(mustErrorJSON(e))))
		} else {
			msg := "zm: " + e.Message
			if e.Hint != "" {
				msg += "\n  " + e.Hint
			}
			fmt.Fprintln(os.Stderr, textclean.Clean(msg))
		}
		switch e.Code {
		case api.CodeUnavailable:
			return 3
		case api.CodeQueued:
			return 4 // queued on a team replica: not done yet (zm outbox)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "zm:", textclean.Clean(err.Error()))
	if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag") ||
		strings.Contains(err.Error(), "arg(s)") {
		return 2
	}
	return 1
}

// exitSilently ends a command with this exit code and no message: its
// output already said why (zm project why with no project).
type exitSilently int

func (e exitSilently) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// exitWith ends a command with this exit code, printing err (when set) as
// report does (zm attach --embed's codes for its host).
type exitWith struct {
	code int
	err  error
}

func (e exitWith) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

// loadConfig resolves ZM_HOME and identity (the daemon's commands).
func loadConfig() (*config.Config, error) { return config.Load() }

// newClient returns a client for this machine's daemon (or $ZM_HOST with
// $ZM_TOKEN), with --as and --project applied. On the socket it starts the
// daemon when none answers, unless the running command is one that never does
// (autostart.go, SPEC 17.1).
func newClient() (*client.Client, *config.Config, error) {
	ctx := context.Background()
	if running != nil && running.Context() != nil {
		ctx = running.Context()
	}
	return newClientContext(ctx)
}

// newClientContext includes runtime registration in the caller's deadline.
func newClientContext(ctx context.Context) (*client.Client, *config.Config, error) {
	cfg, err := config.LoadClient() // no user-database lookup: the daemon knows its human
	if err != nil {
		return nil, nil, err
	}
	// In a pool box the credential file alone names the authority: no
	// socket, no autostart, no runtime registration, no fallback.
	if c, ok, err := orchclient.BoxClient(os.Getenv); ok {
		if err != nil {
			return nil, cfg, err
		}
		if flagAs != "" {
			c.As = flagAs
		}
		if flagProject != "" {
			c.Project, c.ProjectFrom = flagProject, "flag"
		}
		return c, cfg, nil
	}
	c := client.New(cfg)
	if c.Socket != "" && autostartAllowed(running, os.Getenv) {
		c.Autostart = func(ctx context.Context) error { return autostart(ctx, cfg) }
	}
	if flagAs != "" {
		c.As = flagAs
	}
	if flagProject != "" {
		c.Project, c.ProjectFrom = flagProject, "flag"
	}
	if flagAs == "" && os.Getenv(config.EnvAs) == "" && os.Getenv("ZM_SESSION") == "" &&
		c.Socket != "" && c.Token == "" && runtimeIdentityAllowed(running) {
		if agent, rid := integrations.EnvSession(func(key string) string { return strings.TrimSpace(os.Getenv(key)) }); strings.TrimSpace(rid) != "" {
			if err := orchclient.ResolveRuntimeSession(ctx, c, orchapi.RegisterRequest{Agent: agent, RuntimeID: rid, Cwd: c.Cwd, PID: integrations.AgentPID(agent)}); err != nil {
				return nil, cfg, err
			}
		}
	}
	return c, cfg, nil
}

// Hook and MCP entrypoints own their registration. Diagnostics must not
// mutate sessions; notice still resolves its caller without starting a daemon.
func runtimeIdentityAllowed(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.CommandPath() {
		case "zm help", "zm completion", "zm status", "zm ci status", "zm daemon", "zm mcp", "zm hook",
			"zm session hook", "zm session register", "zm session state":
			return false
		}
	}
	return true
}

func mustErrorJSON(e *api.Error) []byte { b, _ := json.Marshal(api.ErrorResponse{Error: e}); return b }

// printJSON writes v indented to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return enc.Encode(json.RawMessage(textclean.JSON(b)))
}

// call is the common RunE body: build a client, run fn, print its result.
// human formats the result for people; nil (or --json) prints JSON.
func call[T any](cmd *cobra.Command, fn func(context.Context, *client.Client) (T, error), human func(T) string) error {
	c, _, err := newClient()
	if err != nil {
		return err
	}
	v, err := fn(cmd.Context(), c)
	if err != nil {
		return err
	}
	if flagJSON || human == nil {
		return printJSON(v)
	}
	if s := human(v); s != "" {
		fmt.Println(strings.TrimRight(textclean.Clean(s), "\n"))
	}
	return nil
}

// joinArgs joins positional words into one body ("-" reads stdin).
func joinArgs(args []string) (string, error) {
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, api.MaxBody))
		return strings.TrimRight(string(b), "\n"), err
	}
	return strings.Join(args, " "), nil
}

func daemonCmd() *cobra.Command {
	var detach, verbose, tailnet bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the zm daemon for this machine (foreground; --detach to background it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if tailnet {
				cfg.File.Tailnet.Enabled = true // this run only; `zm tailnet up` saves it
			}
			if detach {
				return detachDaemon(cmd.Context(), cfg, tailnet)
			}
			lvl := slog.LevelInfo
			if verbose {
				lvl = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
			if err := cfg.EnsureHome(); err != nil {
				return err
			}
			release, err := server.LockHome(cfg.Home)
			if err != nil {
				return err
			}
			defer release()
			keepPATH(cfg.Home, log) // before anything looks up an agent
			noteLastExit(cfg.Home, log)
			done := markRunning(cfg.Home, log)
			ctx, signalled, stop := exitSignals(cmd.Context())
			defer stop()
			dm, err := server.New(ctx, cfg, log, wiring.Full)
			if err == nil {
				dm.KeepSessions() // sessions outlive this process (SPEC 8, "Daemon restarts")
				err = dm.Serve(ctx)
			}
			log.Warn("daemon exiting", "cause", exitCause(signalled(), err), "pid", os.Getpid())
			done()
			return err
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start in the background, logging to $ZM_HOME/daemon.log")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "log every request")
	cmd.Flags().BoolVar(&tailnet, "tailnet", false, "also join your tailnet as potluck-<machine>, or the node this home already has (see zm tailnet up)")
	cmd.AddCommand(daemonStopCmd(), daemonRestartCmd(), daemonUpgradeCmd(), daemonPreflightCmd(), daemonUpdateCmd())
	return cmd
}

// detachDaemon re-runs `zm daemon` in its own session and waits until it answers.
func detachDaemon(ctx context.Context, cfg *config.Config, tailnet bool) error {
	already, err := startDetached(ctx, cfg, tailnet, false)
	switch {
	case err != nil:
		return err
	case already:
		fmt.Println("zm daemon already running:", cfg.SocketPath())
	default:
		fmt.Println("zm daemon started:", cfg.SocketPath())
	}
	return nil
}

// lastLine is the last non-empty line of a file (the daemon's exit reason).
func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "see " + path
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimPrefix(lines[len(lines)-1], "zm: ")
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Who the daemon thinks you are (replies to you land here)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Whoami, error) { return c.Whoami(ctx) },
				func(w api.Whoami) string {
					return fmt.Sprintf("%s (identity %s, via %s, %s on %s)", w.As, w.Identity.Name, w.Via, w.Protocol, w.Machine)
				})
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is the daemon running, and can it serve everything? (exit 3 if not running, 1 on a self-check problem)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var problems bool
			var drivers *wiring.DriverNotice
			err := call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Health, error) {
				h, err := orchclient.Health(ctx, c)
				problems = h.SelfCheck != nil && !h.SelfCheck.OK
				if err == nil {
					drivers = healthDrivers(ctx, c)
				}
				return h, err
			}, func(h orchapi.Health) string { return statusLines(h) + driversText(drivers) })
			if err == nil && problems {
				return exitSilently(1) // the daemon runs but cannot serve something
			}
			return err
		},
	}
}

// healthDrivers is health's drivers section (wiring.DriverNotice): api.Health
// keeps a module's sections in Parts, which it does not read back.
func healthDrivers(ctx context.Context, c *client.Client) *wiring.DriverNotice {
	var raw struct {
		Drivers *wiring.DriverNotice `json:"drivers"`
	}
	if err := c.Do(ctx, http.MethodGet, "/v0/health", nil, &raw); err != nil {
		return nil
	}
	return raw.Drivers
}

// driversText is zm status's warning for run-target drivers the daemon
// cannot run: never a failure (the exit code stays), since a binding may
// stay broken until its owner fixes it.
func driversText(n *wiring.DriverNotice) string {
	if n == nil || n.Refused == 0 {
		return ""
	}
	s := fmt.Sprintf("\nwarning: %d run target(s) whose driver this daemon cannot run (checked %s):", n.Refused, n.Checked.Local().Format("15:04:05"))
	for _, p := range n.Problems {
		s += "\n  - " + p
	}
	return s
}

// holdersText is what zm status says about session holders: only what
// needs a person (sessions the daemon cannot reach, orphans).
func holdersText(r *orchapi.HolderReport) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, u := range r.Unreachable {
		fmt.Fprintf(&b, "\nsession %s is running but unreachable: %s", u.Session, u.Why)
	}
	for _, o := range r.Orphans {
		fmt.Fprintf(&b, "\norphaned session holder: pid %d (session %s; its home %s is gone). zm never stops it; to end it: kill %d",
			o.PID, o.Session, o.Home, o.PID)
	}
	return b.String()
}

// statusLines is zm status for people: the daemon, the self-check's
// problems (loud), the last swap and session holders that need a person.
func statusLines(h orchapi.Health) string {
	s := fmt.Sprintf("zm %s (%s, %s)", h.Version, h.Commit, h.Protocol)
	if h.Machine != "" {
		s += fmt.Sprintf(" on %s, pid %d, up %s", h.Machine, h.PID, time.Since(h.Started).Round(time.Second))
	}
	if h.Version != api.Version || h.Commit != api.Commit {
		s += fmt.Sprintf("\nnote: this CLI is %s (%s); restart the daemon to match (zm daemon upgrade, or make install)", api.Version, api.Commit)
	}
	if sc := h.SelfCheck; sc != nil && !sc.OK {
		s += fmt.Sprintf("\nPROBLEM: this daemon cannot serve %d thing(s) (self-check %s):", len(sc.Problems), sc.Checked.Local().Format("15:04:05"))
		for _, p := range sc.Problems {
			s += "\n  - " + p
		}
	}
	if w := h.LastSwap; w != nil {
		s += fmt.Sprintf("\nlast swap: %s by %s at %s, %s", w.What, w.By, w.At.Local().Format("2006-01-02 15:04:05"), w.Result)
		if w.From != "" || w.To != "" {
			s += fmt.Sprintf(" (%s -> %s)", firstNonEmpty(w.From, "?"), firstNonEmpty(w.To, "?"))
		}
		if w.Forced {
			s += ", FORCED past: " + strings.Join(w.Overridden, "; ")
		}
		if w.Reason != "" {
			s += "; " + w.Reason
		}
	}
	if j := h.CardJudge; j != nil {
		s += fmt.Sprintf("\nwarning: the card judge has not answered for a while: %d ask card(s) held, not sent, since %s (%s). "+
			"They are listed in Decisions as not checked, and an owner may release one; each is sent once the judge passes it; the judge is the claude CLI on this machine (ZM_JUDGE), installed and logged in",
			j.Held, j.Since.Local().Format("2006-01-02 15:04"), j.Reason)
	}
	return s + holdersText(h.Holders)
}

func eventsCmd() *cobra.Command {
	var q api.EventQuery
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream events you may see (CloudEvents JSON, one per line)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if q.Project == "" {
				q.Project = flagProject
			}
			enc := json.NewEncoder(os.Stdout)
			return c.Events(cmd.Context(), q, func(e api.Event) error {
				b, err := json.Marshal(e)
				if err != nil {
					return err
				}
				return enc.Encode(json.RawMessage(textclean.JSON(b)))
			})
		},
	}
	cmd.Flags().StringVar(&q.Types, "types", "", "comma list of type prefixes, e.g. dev.zm.mail.")
	cmd.Flags().StringVar(&q.Subject, "subject", "", "only this subject")
	cmd.Flags().StringVar(&q.Since, "since", "", "replay stored events after this event id first (0 = all)")
	cmd.Flags().BoolVar(&q.NoLive, "no-follow", false, "exit after the replay")
	return cmd
}
