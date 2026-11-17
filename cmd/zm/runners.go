package main

// zm runners setup|set|link|unlink|allow|disallow|repos|ls|jobs (SPEC 20 "GitHub runner broker"): a pool's
// owner turns GitHub Actions on for the pool with zm commands and browser
// steps only. The daemon writes config.json's runners block and opens its
// listener; this file adds the tunnel's ingress for it and prints the one URL
// to open. Owned by the runner broker.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tunnel/cfsetup"
	"github.com/sengirunners/potluck/runners"
)

func init() { rootCmd.AddCommand(runnersCmd()) }

func runnersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runners",
		Short: "Run a GitHub org's Actions jobs in this pool's boxes: setup, set, link, allow, repos, ls, jobs",
	}
	cmd.AddCommand(runnersSetupCmd(), runnersSetCmd(), runnersLinkCmd(), runnersUnlinkCmd(),
		runnersAllowCmd(), runnersDisallowCmd(), runnersReposCmd(), runnersLsCmd(), runnersJobsCmd())
	return cmd
}

func runnersSetupCmd() *cobra.Command {
	var req runners.SetupRequest
	c := &cobra.Command{
		Use:   "setup <pool>",
		Short: "Turn the runner broker on for a pool: config, listener, tunnel ingress, then the one URL to open",
		Long: `Turn GitHub Actions on for a pool project on this machine (its owner, on its
socket). zm writes config.json's runners block, opens the broker's listener on
a free loopback port (live: no restart), and, when zm tunnel setup made this
home's tunnel, routes the broker's public paths on its hostname to it. GitHub
reaches the broker at --url, by default the tunnel's https://<hostname>.

Then it prints one URL. Open it to create the GitHub App; GitHub then offers
to install it, and installing brings you back to sign in and say yes to the
link. The App is private: it installs only on the account that owns it, so
pass --org for an org's jobs. With the App made, the URL installs (or
re-links) it. Re-running it is safe.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cfg, err := newClient()
			if err != nil {
				return err
			}
			if cl.Socket == "" {
				return errors.New("zm runners setup runs on the pool's own machine, against its socket (unset ZM_HOST)")
			}
			req.Pool = args[0]
			var st runners.Setup
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/setup", req, &st); err != nil {
				return err
			}
			ingress, ierr := runnersIngress(cmd, cfg.Home, st)
			if flagJSON {
				if ierr != nil {
					return ierr
				}
				return printJSON(map[string]any{"setup": st, "tunnel_ingress": ingress})
			}
			fmt.Printf("zm: runner broker on for pool %s: GitHub reaches it at %s (listener %s, live)\n", st.Pool, st.PublicURL, st.Listening)
			switch {
			case ierr != nil:
				return fmt.Errorf("the tunnel's ingress for the broker: %w", ierr)
			case ingress != "":
				fmt.Println("zm: " + ingress)
			}
			switch st.Next {
			case "create_app":
				if req.Org == "" {
					fmt.Println("zm: no --org: the App goes on your own GitHub account and installs only there (an org's jobs need --org <org>)")
				}
				fmt.Printf("Open this URL to create the GitHub App (until %s); GitHub then offers the install, which brings you back to link it:\n    %s\n", st.ExpiresAt, st.URL)
			case "install":
				fmt.Printf("The App %s exists. Open this URL to install it and link it (until %s):\n    %s\n", st.App.Slug, st.ExpiresAt, st.URL)
				fmt.Printf("(If GitHub does not bring you back after installing, set the App's Setup URL on GitHub to %s, or run zm runners link <installation-id>.)\n", st.SetupURL)
			}
			return nil
		},
	}
	c.Flags().StringVar(&req.PublicURL, "url", "", "the https URL GitHub calls (default: runners.public_url, else the tunnel's https://<hostname>)")
	c.Flags().StringVar(&req.Org, "org", "", "create the App under this GitHub org (default: your user account)")
	c.Flags().StringVar(&req.Name, "name", "", "the App's name on GitHub (default: the runner broker's own)")
	c.Flags().StringVar(&req.Area, "area", "", "the pool area the first linked org's jobs run in (default: the whole pool)")
	c.Flags().StringSliceVar(&req.Repos, "repo", nil, "an owner/repo to pre-select on GitHub's install page (repeat for more; all of one account)")
	return c
}

// runnersIngress routes the broker's public paths through this home's
// tunnel when public_url is on its hostname; it says what it did ("" when
// there was nothing to do).
func runnersIngress(cmd *cobra.Command, home string, st runners.Setup) (string, error) {
	ts, err := cfsetup.LoadState(home)
	if err != nil || ts.Hostname == "" {
		return "", err
	}
	if u, err := url.Parse(st.PublicURL); err != nil || !strings.EqualFold(u.Hostname(), ts.Hostname) {
		return "", nil // GitHub comes in through another proxy: its owner routes it
	}
	if ts.Quick {
		return "", fmt.Errorf("a quick tunnel cannot route paths; run zm tunnel setup <hostname> for a named one")
	}
	s, err := tunnelSetup(cmd, "", "", false)
	if err != nil {
		return "", err
	}
	// The broker's route at its listener now; every other route (the Mezze app
	// door's) as tunnel setup builds it.
	routes := runnersRoutes(st.Listening)
	for _, r := range s.Routes {
		if r.Path != runnersPath {
			routes = append(routes, r)
		}
	}
	s.Routes = routes
	changed, err := s.Ingress(cmd.Context())
	if err != nil || !changed {
		return "", err
	}
	return fmt.Sprintf("tunnel %s now sends the broker's paths to %s (cloudflared restarted)", ts.Hostname, st.Listening), nil
}

// runnersSetCmd is potluck runners set (potluck cli/runners.go) under zm's
// name: the daemon checks the setting and its value.
func runnersSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <setting> <value>",
		Short: "Change one broker setting, from the next job on: label, job_ttl_s",
		Long: `Changes one setting of the runner broker on this machine (the pool's owner).
It is written to config.json and applies to the next job: no restart, no
hand edit. A job that runs already keeps the time limit it started with.
zm runners ls shows the values. Which repositories are served is
zm runners allow.

  label <name>|default        the label jobs ask for (runs-on: <name>);
                              default is the pool's own name. Jobs still
                              waiting under the old label are not served
  job_ttl_s <seconds>         fallback runtime limit, 900 at least, when a
                              workflow timeout cannot be resolved; supported
                              literal timeout-minutes takes precedence
                              (5400 is 90 minutes)`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			var st runners.Setup
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/settings", runners.SetRequest{Key: args[0], Value: args[1]}, &st); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"setup": st})
			}
			fmt.Printf("zm: %s set, from the next job on\n", args[0])
			printRunnersSettings(&st)
			return nil
		},
	}
}

// printRunnersSettings prints what zm runners set changes, as it is now.
func printRunnersSettings(st *runners.Setup) {
	fmt.Printf("fallback    %d s (job_ttl_s; supported workflow timeout-minutes takes precedence; lending machine limits still apply)\n", st.JobTTLS)
}

func runnersLinkCmd() *cobra.Command {
	var area string
	c := &cobra.Command{
		Use:   "link [<installation-id>]",
		Short: "Link an org's install of the App to a pool area: prints the URL to open",
		Long: `Without an installation id, prints the App's install URL: install it on an org
(or account), and GitHub brings you back to sign in as its admin and say yes.
With one (an installation that exists already), prints GitHub's sign-in URL
for the same proof. Either way the link is made only for an admin of the
installation's account, on their yes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			body := map[string]any{"area": area}
			if len(args) == 1 {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id <= 0 {
					return fmt.Errorf("%q is not an installation id", args[0])
				}
				body["installation_id"] = id
			}
			var out struct {
				Install   string `json:"install_url"`
				Authorize string `json:"authorize_url"`
				Expires   string `json:"expires_at"`
			}
			if err := cl.Do(cmd.Context(), "POST", "/v0/runners/links", body, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if out.Install != "" {
				fmt.Printf("Open this URL to install the App and link it (until %s):\n    %s\n", out.Expires, out.Install)
				fmt.Println("(If GitHub does not bring you back after installing, the App lacks its Setup URL: zm runners link <installation-id>.)")
			} else {
				fmt.Printf("Open this URL as an admin of the installation's account to link it (until %s):\n    %s\n", out.Expires, out.Authorize)
			}
			return nil
		},
	}
	c.Flags().StringVar(&area, "area", "", "the pool area its jobs run in (default: the whole pool)")
	return c
}

func runnersUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <installation-id>",
		Short: "Stop serving an installation's jobs (revokes its token, disconnects its repositories)",
		Long: `Stops serving an installation's jobs and revokes its token. Every repository
connected through that installation is disconnected too (zm runners repos):
linking it again does not bring them back, each needs zm runners allow.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
				return fmt.Errorf("%q is not an installation id", args[0])
			}
			if err := cl.Do(cmd.Context(), "DELETE", "/v0/runners/links/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Printf("zm: installation %s unlinked\n", args[0])
			return nil
		},
	}
}

func runnersLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "The runner broker: its App, listener, pool, settings, sizes and linked installations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := newClient()
			if err != nil {
				return err
			}
			st, links, err := runnersList(cmd.Context(), cl)
			if err != nil {
				return err
			}
			// The repositories are one read among several: a daemon that has
			// not restarted into this zm's version answers 404 for it, and the
			// rest of ls is still true.
			repos, reposErr := []runners.Repo{}, error(nil)
			if st == nil || st.Pool != "" {
				repos, reposErr = runnersRepos(cmd.Context(), cl)
			}
			if flagJSON {
				out := map[string]any{"setup": st, "links": links, "repos": repos}
				if reposErr != nil {
					out["repos_error"] = reposErr.Error()
				}
				return printJSON(out)
			}
			line := ""
			if st != nil && st.Pool != "" {
				line, err = orchclient.CIWorkflowLine(cmd.Context(), cl, *st)
				if err != nil {
					line = "Could not read serving machines: " + err.Error()
				}
				if line == "" {
					line = "No serving machine was found; run zm pool host " + st.Pool
				}
			}
			printRunners(st, links, line)
			switch {
			case reposErr != nil:
				fmt.Printf("repos       could not read: %s\n", reposErr)
			case st == nil || st.Pool != "":
				printRunnersRepos(os.Stdout, repos)
			}
			return nil
		},
	}
}

// runnersList is the setup (on the socket only) and the links the caller
// may read.
func runnersList(ctx context.Context, cl *client.Client) (*runners.Setup, []runners.Link, error) {
	var st *runners.Setup
	if cl.Socket != "" {
		st = &runners.Setup{}
		if err := cl.Do(ctx, "GET", "/v0/runners/setup", nil, st); err != nil {
			return nil, nil, err
		}
		if st.Pool == "" {
			return st, nil, nil
		}
	}
	var out struct {
		Links []runners.Link `json:"links"`
	}
	if err := cl.Do(ctx, "GET", "/v0/runners/links", nil, &out); err != nil {
		return st, nil, err
	}
	return st, out.Links, nil
}

func printRunners(st *runners.Setup, links []runners.Link, line string) {
	w := os.Stdout
	if st != nil {
		if st.Pool == "" {
			fmt.Fprintln(w, "the runner broker is off on this machine (zm runners setup <pool>)")
			return
		}
		fmt.Fprintf(w, "pool        %s\n%s\n", st.Pool, line)
		fmt.Fprintf(w, "public url  %s\n", st.PublicURL)
		switch {
		case st.Listening == "":
			fmt.Fprintf(w, "listener    not serving (zm runners setup %s)\n", st.Pool)
		default:
			fmt.Fprintf(w, "listener    %s\n", st.Listening)
		}
		if st.App == nil {
			fmt.Fprintf(w, "app         none yet (zm runners setup %s prints the URL to create it)\n", st.Pool)
		} else {
			fmt.Fprintf(w, "app         %s (id %d)\n", st.App.Slug, st.App.ID)
		}
		printRunnersSettings(st)
		printRunnersCheck(w, st.Check)
		sizes := make([]string, 0, len(st.Sizes))
		for k := range st.Sizes {
			sizes = append(sizes, k)
		}
		sort.Strings(sizes)
		for i, k := range sizes {
			head := "sizes      "
			if i > 0 {
				head = "           "
			}
			def := ""
			if k == st.DefaultSize {
				def = " (default)"
			}
			fmt.Fprintf(w, "%s %s -> box %s%s\n", head, k, st.Sizes[k], def)
		}
		if len(sizes) == 0 {
			fmt.Fprintln(w, "sizes       none")
		}
	}
	if len(links) == 0 {
		fmt.Fprintln(w, "links       none (zm runners link)")
		return
	}
	fmt.Fprintln(w, "links")
	for _, l := range links {
		area := l.Area
		if area == "" {
			area = "(whole pool)"
		}
		fmt.Fprintf(w, "  %-10d %s (%s)  area %s  by %s  %s\n", l.Installation, l.Account, strings.ToLower(l.AccountType), area, l.LinkedBy, l.Created)
	}
}
