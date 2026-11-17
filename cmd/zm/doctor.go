package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// zm doctor: is this machine's setup OK? One line per check, and a plain
// fix line under each one that is not. It never starts the daemon, never
// changes anything, and exits 1 when a check fails (a warning does not).
//
// Extending it: append a doctorCheck to doctorChecks (in its own file's
// init, if it belongs to another module). A check gets the loaded config
// and, when the daemon answered, a client and its health; it returns one
// result. Keep each check read-only and quick (doctorTimeout covers them all).

// Doctor results, worst last.
const (
	doctorOK   = "ok"
	doctorSkip = "skip"
	doctorWarn = "warn"
	doctorFail = "FAIL"
)

type doctorResult struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// doctorEnv is what a check may read. Client and Health are nil when the
// daemon did not answer; a check that needs them returns doctorSkip.
type doctorEnv struct {
	Config  *config.Config
	ConfErr error
	Client  *client.Client
	Health  *orchapi.Health
	// DaemonErr is why the daemon did not answer (nil when it did).
	DaemonErr error
	// Remote: the client talks to a daemon elsewhere (ZM_HOST), not this
	// home's socket; the local socket and start advice do not apply.
	Remote   bool
	LookPath func(string) (string, error)
}

type doctorCheck struct {
	name string
	run  func(context.Context, *doctorEnv) doctorResult
}

// doctorTimeout bounds each check (and the first health call) on its own,
// so one slow answer fails only its own line.
const doctorTimeout = 5 * time.Second

var doctorChecks = []doctorCheck{
	{"home", doctorHome},
	{"daemon", doctorDaemon},
	{"version", doctorVersion},
	{"trackers", doctorTrackers},
	{"tailnet", doctorTailnet},
	{"agents", doctorAgents},
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Check this machine's setup: home, daemon, version, trackers, tailnet, agent CLIs (exit 1 on a failure)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rs := runDoctor(cmd.Context(), doctorSetup(cmd.Context()))
			if flagJSON {
				if err := printJSON(rs); err != nil {
					return err
				}
			} else {
				fmt.Print(doctorText(rs))
			}
			for _, r := range rs {
				if r.Status == doctorFail {
					return exitSilently(1)
				}
			}
			return nil
		},
	})
}

// doctorSetup loads the config and asks the daemon for its health, without
// starting it (zm doctor is in noAutostart).
func doctorSetup(ctx context.Context) *doctorEnv {
	env := &doctorEnv{LookPath: exec.LookPath}
	cfg, err := config.LoadClient()
	env.Config, env.ConfErr = cfg, err
	if err != nil {
		return env
	}
	c := client.New(cfg) // no Autostart: only newClient sets it
	if flagProject != "" {
		c.Project, c.ProjectFrom = flagProject, "flag"
	}
	env.Remote = c.Socket == ""
	hctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	h, err := orchclient.Health(hctx, c)
	if err != nil {
		env.DaemonErr = err
		return env
	}
	env.Client, env.Health = c, &h
	return env
}

func runDoctor(ctx context.Context, env *doctorEnv) []doctorResult {
	out := make([]doctorResult, 0, len(doctorChecks))
	for _, ch := range doctorChecks {
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		r := ch.run(cctx, env)
		cancel()
		r.Check = ch.name
		out = append(out, r)
	}
	return out
}

func doctorText(rs []doctorResult) string {
	var b strings.Builder
	fails, warns := 0, 0
	for _, r := range rs {
		fmt.Fprintf(&b, "%-4s  %-9s %s\n", r.Status, r.Check, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(&b, "      %-9s fix: %s\n", "", r.Fix)
		}
		switch r.Status {
		case doctorFail:
			fails++
		case doctorWarn:
			warns++
		}
	}
	switch {
	case fails > 0:
		fmt.Fprintf(&b, "%d failed, %d warning(s)\n", fails, warns)
	case warns > 0:
		fmt.Fprintf(&b, "all good, %d warning(s)\n", warns)
	default:
		b.WriteString("all good\n")
	}
	return b.String()
}

// maxSocketPath is the longest Unix socket path every platform zm runs on
// accepts (macOS: 104 bytes with the NUL).
const maxSocketPath = 103

func doctorHome(_ context.Context, env *doctorEnv) doctorResult {
	if env.ConfErr != nil {
		where := "config.json in your zm home (ZM_HOME, default ~/.zm)"
		if h, err := config.HomeDir(); err == nil {
			where = filepath.Join(h, "config.json")
		}
		return doctorResult{Status: doctorFail, Detail: "cannot read the config: " + env.ConfErr.Error(),
			Fix: "fix or move aside " + where + ", then run zm doctor again"}
	}
	home := env.Config.Home
	if env.Remote {
		return doctorResult{Status: doctorSkip, Detail: "using a remote daemon (ZM_HOST); this machine's home is not in use"}
	}
	st, err := os.Stat(home)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return doctorResult{Status: doctorWarn, Detail: home + " does not exist yet",
			Fix: "zm init (in a project directory) makes it and starts the daemon"}
	case err != nil:
		return doctorResult{Status: doctorFail, Detail: err.Error(), Fix: "check the permissions on " + home + " (or set ZM_HOME)"}
	case !st.IsDir():
		return doctorResult{Status: doctorFail, Detail: home + " is not a directory", Fix: "set ZM_HOME to a directory, or move that file aside"}
	}
	if err := syscall.Access(home, 0o2|0o1); err != nil { // W_OK|X_OK, without writing anything
		return doctorResult{Status: doctorFail, Detail: home + " is not writable: " + err.Error(), Fix: "chmod u+rwx " + home + " (or set ZM_HOME)"}
	}
	if sock := env.Config.SocketPath(); !env.Remote && len(sock) > maxSocketPath {
		return doctorResult{Status: doctorFail, Detail: fmt.Sprintf("socket path is %d bytes, over %d: %s", len(sock), maxSocketPath, sock),
			Fix: "set ZM_HOME to a shorter path, e.g. ~/.zm"}
	}
	if st.Mode().Perm()&0o077 != 0 {
		return doctorResult{Status: doctorWarn, Detail: fmt.Sprintf("%s is readable by others (%o)", home, st.Mode().Perm()), Fix: "chmod 700 " + home}
	}
	return doctorResult{Status: doctorOK, Detail: home}
}

func doctorDaemon(_ context.Context, env *doctorEnv) doctorResult {
	if env.ConfErr != nil {
		return doctorResult{Status: doctorSkip, Detail: "needs a readable config"}
	}
	if env.Health == nil {
		return doctorDaemonDown(env)
	}
	h := env.Health
	if sc := h.SelfCheck; sc != nil && !sc.OK {
		return doctorResult{Status: doctorFail, Detail: "running, but cannot serve: " + strings.Join(sc.Problems, "; "),
			Fix: "zm status shows each problem; zm daemon restart after fixing them"}
	}
	d := "running"
	if h.Machine != "" {
		d += fmt.Sprintf(" on %s, pid %d, up %s", h.Machine, h.PID, time.Since(h.Started).Round(time.Second))
	}
	return doctorResult{Status: doctorOK, Detail: d}
}

// doctorDaemonDown says why the daemon did not answer, with a fix that fits:
// never "start it" for a remote daemon, never "restart it" for a refusal.
func doctorDaemonDown(env *doctorEnv) doctorResult {
	err := env.DaemonErr
	var e *api.Error
	code := ""
	if errors.As(err, &e) {
		code = e.Code
	}
	why := "no answer"
	if err != nil {
		why = err.Error()
	}
	switch {
	case code == api.CodeUnauthenticated || code == api.CodeForbidden:
		return doctorResult{Status: doctorFail, Detail: "refused this caller: " + why,
			Fix: "check ZM_TOKEN (or zm join again for a team project); the daemon itself is up"}
	case env.Remote:
		return doctorResult{Status: doctorFail, Detail: "the remote daemon did not answer: " + why,
			Fix: "check ZM_HOST and that the network reaches it; unset ZM_HOST to use this machine's daemon"}
	case errors.Is(err, context.DeadlineExceeded):
		return doctorResult{Status: doctorFail, Detail: fmt.Sprintf("did not answer within %s", doctorTimeout),
			Fix: "zm status; if it hangs too, zm daemon restart and read " + filepath.Join(env.Config.Home, "daemon.log")}
	case err == nil || code == api.CodeUnavailable:
		return doctorResult{Status: doctorFail, Detail: "not running", Fix: "zm daemon --detach (any other zm command starts it too)"}
	}
	return doctorResult{Status: doctorFail, Detail: "did not answer: " + why,
		Fix: "zm daemon restart; if it still fails, read " + filepath.Join(env.Config.Home, "daemon.log")}
}

func doctorVersion(_ context.Context, env *doctorEnv) doctorResult {
	cli := fmt.Sprintf("%s (%.12s)", api.Version, api.Commit)
	if env.Health == nil {
		return doctorResult{Status: doctorOK, Detail: "zm " + cli}
	}
	if env.Health.Version != api.Version || env.Health.Commit != api.Commit {
		return doctorResult{Status: doctorWarn, Detail: fmt.Sprintf("this CLI is %s, the daemon is %s (%.12s)", cli, env.Health.Version, env.Health.Commit),
			Fix: "zm daemon restart (it restarts with the zm it runs from; after make install, the new one)"}
	}
	return doctorResult{Status: doctorOK, Detail: "zm " + cli + ", daemon matches"}
}

func doctorTrackers(ctx context.Context, env *doctorEnv) doctorResult {
	if env.Client == nil {
		return doctorResult{Status: doctorSkip, Detail: "needs the daemon"}
	}
	vs, err := orchclient.Trackers(ctx, env.Client, flagProject)
	if err != nil {
		return doctorResult{Status: doctorFail, Detail: "cannot list trackers: " + err.Error(), Fix: "zm tracker ls"}
	}
	if len(vs) == 0 {
		return doctorResult{Status: doctorOK, Detail: "none bound (local work items only; zm tracker add native for issues)"}
	}
	var bad []string
	var first string
	for _, v := range vs {
		if v.Sync != nil && v.Sync.Error != "" {
			bad = append(bad, fmt.Sprintf("%s %s: %s", v.Slug, v.Prefix, v.Sync.Error))
			if first == "" {
				first = v.Prefix
			}
		}
	}
	if len(bad) > 0 {
		return doctorResult{Status: doctorFail, Detail: fmt.Sprintf("%d of %d failing: %s", len(bad), len(vs), strings.Join(bad, "; ")),
			Fix: "zm tracker test " + first + " shows which check fails"}
	}
	return doctorResult{Status: doctorOK, Detail: fmt.Sprintf("%d bound, all syncing", len(vs))}
}

func doctorTailnet(ctx context.Context, env *doctorEnv) doctorResult {
	if env.Client == nil {
		return doctorResult{Status: doctorSkip, Detail: "needs the daemon"}
	}
	s, err := env.Client.Tailnet(ctx)
	switch {
	case err != nil:
		return doctorResult{Status: doctorWarn, Detail: "cannot read the tailnet node: " + err.Error(), Fix: "zm tailnet status"}
	case s.Running:
		return doctorResult{Status: doctorOK, Detail: "on the tailnet as " + s.URL}
	case s.AuthURL != "":
		return doctorResult{Status: doctorWarn, Detail: "waiting for a Tailscale login",
			Fix: "open " + s.AuthURL + " and log in (a Tailscale account is needed), or zm tailnet down"}
	case s.Error != "":
		return doctorResult{Status: doctorWarn, Detail: "tailnet error: " + s.Error, Fix: "zm tailnet up (TS_AUTHKEY, or a login link)"}
	}
	return doctorResult{Status: doctorSkip, Detail: "not on a tailnet; only pools and Tailscale team projects need it (zm tailnet up, with a Tailscale account)"}
}

func doctorAgents(_ context.Context, env *doctorEnv) doctorResult {
	var found []string
	for _, a := range []string{"claude", "codex"} {
		if _, err := env.LookPath(a); err == nil {
			found = append(found, a)
		}
	}
	if len(found) == 0 {
		return doctorResult{Status: doctorWarn, Detail: "no agent CLI (claude, codex) on PATH",
			Fix: "install one, then zm install claude (or codex) shows how it connects; --yes connects it"}
	}
	return doctorResult{Status: doctorOK, Detail: strings.Join(found, ", ") + " on PATH (zm install <agent> --yes connects one)"}
}
