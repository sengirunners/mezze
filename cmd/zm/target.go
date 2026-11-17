package main

// `zm target` (SPEC 20): bind run targets and use them. Owned by run targets.

import (
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/computeclient"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

func init() {
	tg := &cobra.Command{Use: "target", Short: "Run targets: lend a throwaway database or a run of a script elsewhere",
		Long: `A run target lends a throwaway database or a run of a named script on other
compute (a VM on a Mac, a farm box, a cloud sandbox) through a
potluck-driver-<driver> (or zm-run-<driver>) process. The machine's owner
binds one to the project (project.toml [[run_targets]]); members with
operate acquire it. zm resolves --ref on the binding's remote and sends the
commit itself: a commit that is not the head of the binding's branch runs
only on an ephemeral, credential-free target.`}
	tg.AddCommand(targetLsCmd(), targetBindCmd(), targetUnbindCmd(), targetAcquireCmd(), targetStatusCmd(),
		targetReleaseCmd(), targetRenewCmd(), targetExecCmd())
	rootCmd.AddCommand(tg)
}

func targetLsCmd() *cobra.Command {
	var describe, host bool
	cmd := &cobra.Command{Use: "ls", Short: "List the project's run targets (boxes other machines lend show as <binding>@<machine>)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			var l computeapi.RunTargetList
			if host {
				l, err = computeclient.HostRunTargets(cmd.Context(), c)
			} else {
				l, err = computeclient.RunTargets(cmd.Context(), c, describe)
			}
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(l)
			}
			for _, t := range l.Targets {
				avail := ""
				switch {
				case t.Error != "":
					avail = "  error: " + t.Error
				case t.Describe != nil && !t.Describe.Available:
					avail = "  unavailable: " + t.Describe.Why
				}
				fmt.Printf("%-16s %-4s driver %-10s %s credentials=%t%s\n", t.Name, t.Kind, t.Driver, // the daemon picks the program (potluck-driver-<driver>, else zm-run-<driver>)
					t.Effective.Isolation, t.Effective.Credentials, avail)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&describe, "describe", false, "ask each driver (availability, effective class)")
	cmd.Flags().BoolVar(&host, "host", false, "this machine's machine-level bindings and who they are shared with (the machine's owner)")
	return cmd
}

func targetBindCmd() *cobra.Command {
	var (
		t       computeapi.RunTarget
		budget  string
		configs []string
		replace bool
		setup   bool
		host    bool
		share   []string
	)
	cmd := &cobra.Command{
		Use:   "bind <name> --driver <driver> --repo <url> [flags]",
		Short: "Bind a run target to the project (the machine's owner only)",
		Example: `  zm target bind mac-vm --driver tart --repo https://github.com/acme/app --config image=macos-xcode
  zm target bind farm --driver farm --isolation none --credentials --script test:e2e --budget cpu_s:36000:day`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t.Name = args[0]
			for _, kv := range configs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--config %q: want key=value", kv)
				}
				if t.Config == nil {
					t.Config = map[string]string{}
				}
				t.Config[k] = v
			}
			if budget != "" {
				p := strings.Split(budget, ":")
				if len(p) != 3 {
					return errors.New("--budget: want unit:limit:period, e.g. cpu_s:36000:day")
				}
				n, err := strconv.ParseInt(p[1], 10, 64)
				if err != nil {
					return fmt.Errorf("--budget limit: %v", err)
				}
				t.Budget = &computeapi.RunTargetBudget{Unit: p[0], Limit: n, Period: p[2]}
			}
			v, err := computeclient.BindRunTarget(cmd.Context(), c, computeapi.BindRunTargetRequest{Target: t, Replace: replace, Setup: setup, Host: host, Share: share})
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(v)
			}
			fmt.Printf("bound %s (driver %s; effective %s, credentials=%t)\n", v.Name, v.Driver,
				v.Effective.Isolation, v.Effective.Credentials)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&t.Driver, "driver", "", "potluck-driver-<driver> (or zm-run-<driver>) in the daemon program's directory or $ZM_HOME/drivers, never PATH; pinned by sha256 on first use (zm driver pin)")
	f.StringVar(&t.Kind, "kind", "run", "run or db")
	f.StringVar(&t.Isolation, "isolation", "ephemeral", "ephemeral or none")
	f.BoolVar(&t.Credentials, "credentials", false, "the target holds credentials (acquire is then local-only)")
	f.StringVar(&t.Repo, "repo", "", "the git remote zm resolves refs on (one of the project's remotes)")
	f.StringVar(&t.Branch, "branch", "main", "the protected default branch: its head is trusted code")
	f.BoolVar(&t.AllowAnyRef, "allow-any-ref", false, "let any pushed commit run here (the only override)")
	f.StringSliceVar(&t.Scripts, "script", nil, "an allowed script (repeat); required when credentialed or unisolated")
	f.IntVar(&t.TTLMaxS, "ttl-max", 0, "lifetime cap in seconds, renewals included")
	f.IntVar(&t.HandlesMax, "handles-max", 0, "live handles per actor")
	f.StringSliceVar(&t.CreateIn, "create-in", nil, "db: a host:port/maintenance-db the target may create in (repeat)")
	f.StringSliceVar(&t.Images, "image", nil, "box: an image pinned by digest, name@sha256:<hex> (repeat; the first is the default)")
	f.StringArrayVar(&configs, "config", nil, "driver config key=value (repeat; a credential only as a *_file key)")
	f.StringVar(&budget, "budget", "", "unit:limit:period (day, week or month; UTC)")
	f.BoolVar(&replace, "replace", false, "rebind an existing name")
	f.BoolVar(&host, "host", false, "bind machine-level (run_targets.toml), a box shared with --share projects")
	f.StringSliceVar(&share, "share", nil, "with --host: a team project slug that may use it (repeat)")
	f.BoolVar(&setup, "setup", false, "run the driver's one-time setup first (a dry run, then the real one)")
	return cmd
}

func targetUnbindCmd() *cobra.Command {
	var host bool
	cmd := &cobra.Command{Use: "unbind <name>", Short: "Remove a run target (refused while it has live handles)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if host {
				return computeclient.UnbindHostRunTarget(cmd.Context(), c, args[0])
			}
			return unbindHere(cmd.Context(), c, args[0], computeclient.UnbindRunTarget(cmd.Context(), c, args[0]))
		}}
	cmd.Flags().BoolVar(&host, "host", false, "remove a machine-level binding")
	return cmd
}

// unbindHere is err, unless a joined project's authority refused the unbind
// (a bind and an unbind stay where the box is served) and name is one of this
// machine's own machine-level bindings, as the pool lists it (<binding>@<machine>)
// or bare: then it names the command that works.
func unbindHere(ctx context.Context, c *client.Client, name string, err error) error {
	if !api.IsCode(err, api.CodeBadRequest) {
		return err
	}
	l, lerr := computeclient.HostRunTargets(ctx, c)
	if lerr != nil {
		return err
	}
	bare, machine, _ := strings.Cut(name, "@")
	if machine != "" { // the pool's name for it: <machine> must be this one, as its authorities know it
		rems, rerr := c.Remotes(ctx)
		if rerr != nil || !slices.ContainsFunc(rems, func(r api.Remote) bool { return r.Machine == machine }) {
			return err
		}
	}
	for _, v := range l.Targets {
		if v.RunTarget.Host == "" && v.Name == bare {
			return &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("%s is this machine's own binding: a joined project's authority does not unbind it", name),
				Hint: "zm target unbind --host " + bare}
		}
	}
	return err
}

// splitHandle takes run:<target>/<id> (or <target> <id>).
func splitHandle(args []string) (string, string, error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	h := args[0]
	if _, rest, ok := strings.Cut(h, ":"); ok {
		h = rest
	}
	t, id, ok := strings.Cut(h, "/")
	if !ok {
		return "", "", fmt.Errorf("want a handle (run:<target>/<id>) or <target> <id>")
	}
	return t, id, nil
}

func printHandle(h computeapi.RunTargetHandle) error {
	if flagJSON {
		return printJSON(h)
	}
	exit := ""
	if h.Exit != nil {
		exit = fmt.Sprintf(" exit=%d", *h.Exit)
	}
	commit := h.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	net := ""
	if h.Net != "" {
		net = " net=" + h.Net // a box: whether it has a network
	}
	switch {
	case h.State == computeapi.StateStopped:
		net += " persist (disk kept until expires; zm sandbox start)" // a stopped box's expires_at is its disk's
	case h.Persist && h.Ended == "":
		net += " persist"
	}
	fmt.Printf("%s %s%s commit=%s expires=%s%s\n", h.Handle, h.State, exit, commit, h.ExpiresAt, net)
	if h.URL != "" {
		fmt.Println(h.URL)
	}
	return nil
}

func targetAcquireCmd() *cobra.Command {
	var req computeapi.AcquireRunTargetRequest
	var ttl time.Duration
	var wait bool
	cmd := &cobra.Command{
		Use:   "acquire <target> [--ref main] [--script <name> [-- args...]] [--name <db>]",
		Short: "Acquire a run target: start a run of a pushed script, or make a throwaway database",
		Example: `  zm target acquire mac-vm --script test:e2e --ttl 30m --wait
  zm target acquire scratch --name pr-412`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			req.Args = args[1:]
			req.TTLS = int(ttl / time.Second)
			if req.RequestID == "" {
				req.RequestID = "cli-" + strings.ToLower(ids.New())
			}
			h, err := computeclient.AcquireRunTarget(cmd.Context(), c, args[0], req)
			if err != nil {
				return retryWithKey(err, req.RequestID)
			}
			for wait && (h.State == "running" || h.State == "acquiring") {
				time.Sleep(2 * time.Second)
				if h, err = computeclient.RunTargetStatus(cmd.Context(), c, args[0], h.Handle[strings.LastIndex(h.Handle, "/")+1:]); err != nil {
					return err
				}
			}
			if err := printHandle(h); err != nil {
				return err
			}
			if wait && h.State != "ended" {
				return exitWith{code: 1, err: fmt.Errorf("%s %s", h.Handle, h.State)}
			}
			if wait && h.Exit != nil && *h.Exit != 0 {
				return exitWith{code: *h.Exit, err: fmt.Errorf("%s exited %d", h.Handle, *h.Exit)}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Ref, "ref", "main", "the pushed branch or tag")
	f.StringVar(&req.Commit, "commit", "", "refuse unless --ref resolves to this commit")
	f.StringVar(&req.Script, "script", "", "the script to run (a name the target resolves)")
	f.StringVar(&req.Name, "name", "", "db: the database's name")
	f.StringVar(&req.Image, "image", "", "box: one of the binding's images, by name (default the first)")
	f.StringVar(&req.RequestID, "request-id", "", "idempotency key: the same key answers the same handle")
	f.DurationVar(&ttl, "ttl", 0, "lease length (default: the driver's)")
	f.BoolVar(&wait, "wait", false, "run: wait for the end and exit with the script's code")
	return cmd
}

func targetStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status <handle>|<target> [<id>]", Short: "A handle's state (asks the driver)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if len(args) == 1 && !strings.Contains(args[0], "/") {
				hs, err := computeclient.RunTargetHandles(cmd.Context(), c, args[0], false)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(hs)
				}
				for _, h := range hs {
					_ = printHandle(h)
				}
				return nil
			}
			t, id, err := splitHandle(args)
			if err != nil {
				return err
			}
			h, err := computeclient.RunTargetStatus(cmd.Context(), c, t, id)
			if err != nil {
				return err
			}
			return printHandle(h)
		}}
}

func targetReleaseCmd() *cobra.Command {
	return &cobra.Command{Use: "release <handle>|<target> <id>", Short: "End a handle now", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := splitHandle(args)
			if err != nil {
				return err
			}
			r, err := computeclient.ReleaseRunTarget(cmd.Context(), c, t, id)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Printf("%s released=%t\n", r.Handle, r.Released)
			return nil
		}}
}

func targetExecCmd() *cobra.Command {
	var req computeapi.ExecRunTargetRequest
	var stdin bool
	var secrets []string
	cmd := &cobra.Command{Use: "exec <box handle> -- <argv...>", Short: "Run a command in a box you hold (literal argv; its output is data)",
		Long: `Run a command in a box you hold (literal argv; its output is data).

A call is sealed between machines only when it carries --secret values;
without them argv, --stdin and --cwd travel readable, and the output
always does: the pool's machine reads them and, over a tunnel, so does
the tunnel's provider (Cloudflare). A command that prints a secret sends
it back readable.`,
		Example: `  zm target exec box:sandbox/b3 -- sh -c 'uname -a'`, Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := splitHandle(args[:1])
			if err != nil {
				return err
			}
			req.Argv = args[1:]
			if req.Secrets, err = readSecrets(secrets, os.Getenv); err != nil {
				return err
			}
			if stdin {
				b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
				if err != nil {
					return err
				}
				req.Stdin = string(b)
			}
			r, err := computeclient.ExecRunTarget(cmd.Context(), c, t, id, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Print(r.Stdout)
			fmt.Fprint(os.Stderr, r.Stderr)
			if r.Truncated {
				fmt.Fprintln(os.Stderr, "zm: output truncated at 1 MiB")
			}
			if r.Exit != 0 {
				return exitWith{code: r.Exit, err: fmt.Errorf("exit %d", r.Exit)}
			}
			return nil
		}}
	f := cmd.Flags()
	f.IntVar(&req.TimeoutS, "timeout", 0, "seconds (default 30, at most 600; cut to the lease)")
	f.StringVar(&req.Cwd, "cwd", "", "a guest path (default: the checkout, else HOME)")
	f.BoolVar(&stdin, "stdin", false, "send this command's stdin (at most 1 MiB)")
	f.StringArrayVar(&secrets, "secret", nil, "NAME=@file (its bytes exactly, a trailing newline included) or NAME (from $NAME): read by the command at $ZM_SECRETS/NAME; trusted hosts only, never in argv or env (repeatable)")
	return cmd
}

func targetRenewCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{Use: "renew <handle>|<target> <id> --ttl 10m", Short: "Extend a handle's lease (within the lifetime cap)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := splitHandle(args)
			if err != nil {
				return err
			}
			h, err := computeclient.RenewRunTarget(cmd.Context(), c, t, id, int(ttl/time.Second))
			if err != nil {
				return err
			}
			return printHandle(h)
		}}
	cmd.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "the new lease length from now")
	return cmd
}
