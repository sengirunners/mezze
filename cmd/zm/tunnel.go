package main

// zm tunnel setup|status|remove (SPEC 17.11 "Your own domain"): the home at
// https://<hostname> on the team's own Cloudflare zone. The steps are
// potluck/kit/tunnel/cfsetup's; this file wires the pinned cloudflared
// (internal/tools), the machine's service manager (the one tracker homes use)
// and this daemon's socket.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tunnel/cfsetup"
)

func init() { rootCmd.AddCommand(tunnelCmd()) }

func tunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Put this home on your own domain through a Cloudflare Tunnel, so teammates join without Tailscale",
	}
	var listen, name, cfPath, sup string
	var asRoot, quick bool
	var verify time.Duration
	setup := &cobra.Command{
		Use:   "setup <hostname>",
		Short: "Make this home reachable at https://<hostname>: cloudflared, its login, a tunnel, DNS, a service, zm's listener, a check",
		Long: `Make this home reachable at https://<hostname> on a zone in your Cloudflare account.

It downloads the pinned cloudflared into $ZM_HOME/tools, runs its login (open
the printed URL on any device and pick the zone), creates or reuses the tunnel
zm-<hostname>, routes the hostname to it, writes cloudflared's config under
$ZM_HOME/tunnel/cloudflared, keeps cloudflared running as this user's
launchd or systemd --user service, starts zm's tunnel listener, and checks
https://<hostname>/v0/health from outside. Re-running it is safe.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfsetup.IsRoot() && !asRoot {
				return errors.New("zm tunnel setup is running as root: its service would run cloudflared as root too. Run it as the user who runs zm, or pass --as-root to do this on purpose")
			}
			s, err := tunnelSetup(cmd, cfPath, sup, true)
			if err != nil {
				return err
			}
			s.Listen, s.Name, s.VerifyFor = listen, name, verify
			if quick {
				return s.RunQuick(cmd.Context())
			}
			if len(args) != 1 {
				return errors.New("zm tunnel setup <hostname>")
			}
			s.Hostname = args[0]
			return s.Run(cmd.Context())
		},
	}
	setup.Flags().StringVar(&listen, "listen", cfsetup.DefaultListen, "zm's tunnel listener (loopback)")
	setup.Flags().StringVar(&name, "name", "", "the tunnel's name in Cloudflare (default zm-<hostname>)")
	setup.Flags().StringVar(&cfPath, "cloudflared", "", "use this cloudflared instead of the pinned download")
	setup.Flags().BoolVar(&asRoot, "as-root", false, "allow running as root (the service then runs as root)")
	setup.Flags().DurationVar(&verify, "wait", 2*time.Minute, "how long to wait for the hostname to answer")
	// Rehearsals (development): a quick tunnel on a temporary trycloudflare
	// name (no login, nothing in any account), and plain processes instead
	// of launchd or systemd.
	setup.Flags().BoolVar(&quick, "quick", false, "rehearse on a temporary trycloudflare name (no login, no account resources)")
	setup.Flags().StringVar(&sup, "supervisor", "", "process: run cloudflared as a plain process (development)")
	_ = setup.Flags().MarkHidden("quick")
	_ = setup.Flags().MarkHidden("supervisor")

	status := &cobra.Command{
		Use:   "status",
		Short: "What zm tunnel setup made, zm's listener, and one look at https://<hostname> from outside",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := tunnelSetup(cmd, cfPath, sup, false)
			if err != nil {
				return err
			}
			st, zs, a, err := s.Status(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"setup": st, "listener": zs, "outside": a})
			}
			if st.Hostname == "" {
				fmt.Printf("no tunnel set up on this home (zm tunnel setup <hostname>)\nlistener: running=%v listen=%s url=%s\n", zs.Running, zs.Listen, zs.URL)
				return nil
			}
			tun := st.Name + " (" + st.TunnelID + ")"
			if st.Quick {
				tun = "quick tunnel (a temporary name: it changes when cloudflared restarts)"
			}
			fmt.Printf("hostname   %s\ntunnel     %s\nlistener   running=%v on %s, invites name %s\n", st.Hostname, tun, zs.Running, zs.Listen, zs.URL)
			edge := "never seen"
			if zs.EdgeSeen != nil {
				edge = "last seen " + time.Since(*zs.EdgeSeen).Round(time.Second).String() + " ago"
			}
			fmt.Printf("edge       secret set=%v, %s\nservice    %s\n", zs.EdgeSecret, edge, st.Label)
			switch {
			case a == nil:
			case a.HomeKey != "" && a.Status == 401:
				fmt.Printf("outside    https://%s answers zm's signed 401: ready\n", st.Hostname)
			case a.Challenge:
				fmt.Printf("outside    Cloudflare challenges the request: turn Bot Fight Mode off for the zone (Security > Bots)\n")
			default:
				fmt.Printf("outside    not answering as zm yet (status %d %s)\n", a.Status, a.ServerSays)
			}
			return nil
		},
	}
	var keep bool
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Undo zm tunnel setup: zm's listener, the service, and the tunnel (the DNS record is a dashboard step it prints)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := tunnelSetup(cmd, cfPath, sup, false)
			if err != nil {
				return err
			}
			return s.Remove(cmd.Context(), keep)
		},
	}
	remove.Flags().BoolVar(&keep, "keep-tunnel", false, "keep the tunnel (and its DNS record) in Cloudflare")
	remove.Flags().StringVar(&sup, "supervisor", "", "")
	_ = remove.Flags().MarkHidden("supervisor")
	cmd.AddCommand(setup, status, remove, tunnelKeyCmd())
	return cmd
}

// tunnelSetup wires a cfsetup.Setup to this machine. Only the home's own
// owner on its socket can do it (the routes are local).
func tunnelSetup(cmd *cobra.Command, cfPath, sup string, download bool) (*cfsetup.Setup, error) {
	c, cfg, err := newClient()
	if err != nil {
		return nil, err
	}
	if c.Socket == "" {
		return nil, errors.New("zm tunnel runs on the home's own machine, against its socket (unset ZM_HOST)")
	}
	if cfPath == "" {
		p, err := tools.Verify(cfg.Home, tools.Cloudflared)
		switch {
		case err == nil:
		case !errors.Is(err, tools.ErrMissing):
			// A binary that no longer hashes to what was installed (or any
			// other doubt) is never run.
			return nil, fmt.Errorf("%w (remove %s and run zm tunnel setup again to fetch the pinned one)", err, tools.Path(cfg.Home, tools.Cloudflared))
		case download:
			res, err := tools.Ensure(cmd.Context(), cfg.Home, []string{tools.Cloudflared}, tools.Options{Download: true, Progress: os.Stderr})
			if err != nil {
				return nil, err
			}
			p = res[0].Path
		default:
			p = "" // status and remove: nothing to run; remove of a named tunnel then says so
		}
		cfPath = p
	}
	if cfPath != "" {
		if abs, err := filepath.Abs(cfPath); err == nil {
			cfPath = abs
		}
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var mgr home.Manager
	switch sup {
	case "":
		if mgr, err = home.DefaultManager(userHome); err != nil {
			return nil, err
		}
	case "process":
		mgr = home.Process{Dir: cfsetup.Dir(cfg.Home)}
	default:
		return nil, fmt.Errorf("--supervisor %q: only process", sup)
	}
	s := &cfsetup.Setup{ZmHome: cfg.Home, CF: cfsetup.ExecRunner{Path: cfPath}, CFPath: cfPath, Sup: cfSupervisor{mgr},
		Zm: tunnelDaemon{c}, Out: os.Stdout}
	s.Routes = runnersRoutes(cfg.File.Runners.Listen)
	if l := cfg.File.Door.Listen; l != "" {
		// The Mezze app door's hook route goes to its own listener, unsigned (SPEC
		// 18.4); the URL's token is its credential.
		s.Routes = append(s.Routes, cfsetup.Route{Path: `^/v0/door/hook/[A-Za-z0-9_-]+$`, Listen: l})
	}
	if runtime.GOOS == "linux" {
		s.Linger = linger
	}
	return s, nil
}

// runnersRoutes sends the runner broker's public routes to its own listener,
// unsigned (SPEC 20); the door keeps everything else.
func runnersRoutes(listen string) []cfsetup.Route {
	if listen == "" {
		return nil
	}
	return []cfsetup.Route{{Path: runnersPath, Listen: listen}}
}

const runnersPath = `^/v0/runners/(hook|oauth|app/new|app/created|app/installed)$`

// cfSupervisor is the tracker homes' service manager as cfsetup's.
type cfSupervisor struct{ m home.Manager }

func (s cfSupervisor) Name() string { return s.m.Name() }
func (s cfSupervisor) unit(u cfsetup.Unit) home.Unit {
	return home.Unit{Label: u.Label, Args: u.Args, Dir: u.Dir, Log: u.Log, Env: u.Env}
}
func (s cfSupervisor) Install(ctx context.Context, u cfsetup.Unit) error {
	return s.m.Install(ctx, s.unit(u))
}
func (s cfSupervisor) Restart(ctx context.Context, u cfsetup.Unit) error {
	return s.m.Restart(ctx, s.unit(u))
}
func (s cfSupervisor) Remove(ctx context.Context, u cfsetup.Unit) error {
	return s.m.Remove(ctx, s.unit(u))
}
func (s cfSupervisor) Path(u cfsetup.Unit) string { return s.m.Path(s.unit(u)) }

// tunnelDaemon is this daemon's tunnel routes.
type tunnelDaemon struct{ *client.Client }

// linger reports whether this user's systemd services outlive a logout.
func linger() (bool, error) {
	out, err := exec.Command("loginctl", "show-user", os.Getenv("USER"), "-p", "Linger", "--value").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}
