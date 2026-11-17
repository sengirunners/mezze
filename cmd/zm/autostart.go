package main

// Owned by the zero-install stream (SPEC 17.1): starting the daemon when a
// command needs it and none answers.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/swap"
)

// EnvNoAutostart = 1 turns autostart off (scripts, CI, tests that expect "not running").
const EnvNoAutostart = "ZM_NO_AUTOSTART"

// noAutostart are the commands (and everything under them) that never start
// the daemon: asking whether it runs, running or stopping it, serving MCP (its
// tools answer "daemon not running" instead), and everything a hook runs (a
// hook with no daemon must answer within its 15 ms budget, silently).
var noAutostart = map[string]bool{
	"zm help":             true,
	"zm completion":       true,
	"zm status":           true,
	"zm doctor":           true, // says whether the daemon runs; never starts it
	"zm daemon":           true,
	"zm mcp":              true,
	"zm hook":             true,
	"zm notice":           true, // the prompt hook's line
	"zm session hook":     true,
	"zm session register": true, // documented for hooks
	"zm session state":    true, // documented for hooks
	"zm t3 enable":        true, // writes config.json; restarts only a running daemon
	"zm t3 disable":       true,
}

// running is the command being executed (set before its RunE), so
// newClient can tell whether it may start the daemon.
var running *cobra.Command

func init() {
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) { running = cmd }
}

// autostartAllowed reports whether cmd may start the daemon.
func autostartAllowed(cmd *cobra.Command, getenv func(string) string) bool {
	if cmd == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvNoAutostart))) {
	case "", "0", "false", "no":
	default:
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		if noAutostart[c.CommandPath()] {
			return false
		}
	}
	return true
}

// autostart starts the daemon for cfg, detached, unless another process
// already did, and says so in one line on stderr. A lock file serialises
// concurrent starters, so two commands run at once start one daemon.
func autostart(ctx context.Context, cfg *config.Config) error {
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	c := client.NewUnix(cfg.SocketPath())
	f, err := os.OpenFile(cfg.Path(fileAutostartLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(autostartSwapWait)
	for {
		// A swap stops one daemon and starts another (itself, or through
		// launchd or systemd): a daemon started in that gap takes the home
		// from the swap's (SPEC 2, "Swaps"). Wait for the swap's daemon or
		// for the swap to end; never start one while it runs.
		if h, held := swap.Holder(cfg.Home); held {
			if _, err := c.Health(ctx); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%w; not starting another daemon beside it", &swap.BusyError{Holder: h})
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		if _, err := c.Health(ctx); err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return nil // another command started it while we waited
		}
		if _, held := swap.Holder(cfg.Home); held { // a swap began while we waited
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			continue
		}
		break
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	already, err := startDetached(ctx, cfg, false, true)
	if err != nil || already {
		return err
	}
	fmt.Fprintf(os.Stderr, "zm: started the daemon (%s)\n", tildePath(cfg.Home))
	return nil
}

// autostartSwapWait bounds how long an autostart waits for a swap (its
// gap: the old daemon's exit, the new one's start, maybe a rollback).
var autostartSwapWait = 2 * time.Minute

// fileAutostartLock serialises autostarting commands ($ZM_HOME).
const fileAutostartLock = "autostart.lock"

// tildePath shows p under the user's home as ~/...
func tildePath(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if rel, err := filepath.Rel(h, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		if rel == "." {
			return "~"
		}
		return "~/" + rel
	}
	return p
}

// callerEnv are variables that describe the command that started the daemon,
// not the machine: a daemon started from inside an agent session must not
// carry that session's identity, project or token.
var callerEnv = []string{config.EnvAs, config.EnvProject, config.EnvToken, config.EnvHost, "ZM_SESSION", EnvNoAutostart}

// trackerEnvPrefixes are the tracker CLIs' own settings (bd's and Dolt's),
// which an autostarted daemon does not take from the shell that happened to
// run the first command: a BEADS_DOLT_SERVER_HOST there must not reach bd
// behind the user's back. A daemon started on purpose (zm daemon, or
// --detach) keeps them in its own environment, and they reach bd only through
// a tracker bound with env = "inherit"; every other binding gets SPEC 17.2's
// allowlist whichever way the daemon was started.
var trackerEnvPrefixes = []string{"BEADS_", "BD_", "DOLT_"}

// daemonEnv is the environment for a detached daemon on home, from environ
// (os.Environ()); auto: it is being autostarted.
func daemonEnv(environ []string, home string, auto bool) []string {
	drop := map[string]bool{orchapi.EnvHome: true}
	for _, k := range callerEnv {
		drop[k] = true
	}
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] || config.IsAgentEnv(k) || (auto && hasAnyPrefix(k, trackerEnvPrefixes)) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, orchapi.EnvHome+"="+home)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// startDetached runs `zm daemon` in its own session, logging to
// daemon.log, and waits until it answers. already: one was running. auto:
// autostarted (see daemonEnv).
func startDetached(ctx context.Context, cfg *config.Config, tailnet, auto bool) (already bool, err error) {
	if err := cfg.EnsureHome(); err != nil {
		return false, err
	}
	c := client.NewUnix(cfg.SocketPath())
	if _, err := c.Health(ctx); err == nil {
		return true, nil
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	argv := []string{self, "daemon"}
	if tailnet {
		argv = append(argv, "--tailnet")
	}
	p, err := swap.StartDetached(ctx, swap.Detach{Argv: argv, Env: daemonEnv(os.Environ(), cfg.Home, auto),
		Log: cfg.Path(config.FileLog), Home: cfg.Home})
	if err != nil {
		return false, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.Health(ctx); err == nil {
			return false, nil
		}
		select {
		case <-p.Exited: // say why (e.g. another daemon holds the lock), not "not running"
			return false, fmt.Errorf("daemon did not start: %s", lastLine(cfg.Path(config.FileLog)))
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("daemon did not answer within 10s; see %s", cfg.Path(config.FileLog))
		}
	}
}
