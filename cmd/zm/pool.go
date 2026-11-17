package main

// `zm pool` (SPEC 20 "Pools"): the co-op's front door. A pool
// is a team project whose members lend boxes and use them. Each command only
// strings together steps that exist (zm project new and team, zm tailnet up,
// zm invite, zm join, zm host doctor and share --pool, zm target ls); it keeps
// no state of its own. Owned by run targets.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

func init() {
	p := &cobra.Command{Use: "pool", Short: "Share compute with friends: one command to start a pool, one to lend a machine to it, one to join it",
		Long: `A pool is a team project whose members lend machines as boxes and run
sandboxes on each other's. Its owner runs zm pool create and sends each
friend an invite code; a friend runs zm pool join <code> to use the pool, or
zm pool host <code> to lend this machine to it too (and use it).`}
	p.AddCommand(poolCreateCmd(), poolHostCmd(), poolJoinCmd())
	rootCmd.AddCommand(p)
}

// tailnetWait bounds a first tailnet login.
var tailnetWait = 10 * time.Minute

// advertWait is how long zm pool host waits for the pool to list its boxes:
// a host's heartbeat carries them every 15 s.
var advertWait = 45 * time.Second

// poolInvites are zm pool create's invites (SPEC, Pools): renters are members of the
// team's area; hosts are guests of
// the hosts area, never project members (a project relation covers every
// area), with the operator class the owner chose on the invite: the machine
// that redeems it gets that class at its authority.
type poolInvites struct {
	Team                        string // the team area renters join
	Renters, Hosts, PublicHosts []string
	Via                         string
	Images                      []string
}

// HostsArea is the pool's area its lenders join as guests.
const HostsArea = "hosts"

// classMeans is what an operator class means for a host, in plain words.
var classMeans = map[string]string{
	api.MachineTrusted: "trusted (a friend): renters' sandboxes are placed on its boxes unasked, and they may run GitHub Actions jobs and get renters' secrets",
	api.MachinePublic:  "public (a stranger): its boxes run only sandboxes a renter names, never GitHub Actions jobs, never any secret",
}

func poolCreateCmd() *cobra.Command {
	var in poolInvites
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "create <pool> [--renter NAME]... [--host NAME]... [--public-host NAME]... [--image name@sha256:<digest>]",
		Short: "Start a pool on this machine (a team project it is the authority of, with a hosts area and a team area) and print an invite code for each friend",
		Long: `Create makes the pool (areas "hosts" and the team's), brings this machine's
tailnet node up and prints one invite code per name: a renter joins the team
area as a member (runs sandboxes); a host joins the hosts area as a guest
(lends a machine, sees nothing else). A host invite carries the machine's
class, so create returns at once: trusted for --host (a friend: its boxes
are placed and may run GitHub Actions jobs and get renters' secrets), public
for --public-host (a stranger: secret-free boxes, placed only when named,
never GitHub Actions jobs). Each friend's machine must reach this one: on
your tailnet, or with this machine shared into theirs (create prints where);
with --via tunnel no tailnet is needed. Run create again for a friend who
comes later: it adds their invite to the pool.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			return poolCreate(cmd.Context(), c, args[0], in, os.Stdout)
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Team, "team", "team", "the team area renters join")
	f.StringArrayVar(&in.Renters, "renter", nil, "a friend who runs sandboxes: a member of the team area (repeat)")
	f.StringArrayVar(&in.Hosts, "host", nil, "a friend who lends a machine: a guest of the hosts area, its machine trusted (repeat)")
	f.StringArrayVar(&in.PublicHosts, "public-host", nil, "a stranger who lends a machine: a guest of the hosts area, its machine public (repeat)")
	f.StringVar(&in.Via, "via", "", "tailnet or tunnel (an https code for a home behind cloudflared; default: the tailnet, started here if it is not)")
	f.StringArrayVar(&in.Images, "image", nil, "the pinned image hosts lend boxes of, written into the host line printed with each host invite (repeat; default: none, so each host lends zm's default)")
	f.DurationVar(&wait, "wait", 0, "no longer used: a host's class travels with its invite")
	_ = f.MarkDeprecated("wait", "a host's class travels with its invite now, so create no longer waits")
	return cmd
}

func poolCreate(ctx context.Context, c *client.Client, slug string, in poolInvites, w io.Writer) error {
	if !api.ValidSlug(slug) {
		return fmt.Errorf("pool name %q: use lowercase letters, digits and dashes", slug)
	}
	if in.Team == "" {
		in.Team = "team"
	}
	if !api.ValidSlug(in.Team) || in.Team == HostsArea {
		return fmt.Errorf("team area %q: a lowercase name other than %s", in.Team, HostsArea)
	}
	p, err := c.GetProject(ctx, slug)
	missing := api.IsCode(err, api.CodeNotFound)
	if err != nil && !missing {
		return err
	}
	// The tailnet login before any write: a pool cancelled while it waits
	// leaves no half-made project behind.
	if in.Via != api.ViaTunnel {
		if err := ensureTailnet(ctx, c, w); err != nil {
			return err
		}
	}
	if missing {
		if p, err = c.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
			return err
		}
	}
	for _, a := range []string{HostsArea, in.Team} {
		if !slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == a }) {
			if p, err = c.AddArea(ctx, slug, api.AddAreaRequest{Name: a}); err != nil {
				return fmt.Errorf("area %s: %w", a, err)
			}
		}
	}
	if _, err := c.ProjectTeam(ctx, slug); err != nil {
		return err
	}
	fmt.Fprintf(w, "pool %s is ready (areas %s and %s); this machine is its authority\n", slug, HostsArea, in.Team)
	images := "" // without --image, each host lends zm's default image
	if len(in.Images) > 0 {
		images = " --image " + strings.Join(in.Images, " --image ")
	}
	invite := func(name, area, role, class string) (api.InviteResponse, error) {
		r, err := c.Invite(ctx, api.InviteRequest{Project: slug, Area: area, As: name, Role: role, Via: in.Via, Class: class})
		if err != nil {
			return r, fmt.Errorf("invite %s: %w", name, err)
		}
		return r, nil
	}
	for _, name := range in.Renters {
		r, err := invite(name, in.Team, "member", "")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\nrenter %s (one use, until %s), on their machine:\n  zm pool join '%s'\n%s", name, r.Invite.Expires.Local().Format("Jan 2 15:04"), r.Code, tailnetStep(name, r.Code))
	}
	hosts := 0
	for _, h := range []struct {
		names []string
		class string
	}{{in.Hosts, api.MachineTrusted}, {in.PublicHosts, api.MachinePublic}} {
		for _, name := range h.names {
			r, err := invite(name, HostsArea, "guest", h.class)
			if err != nil {
				return err
			}
			hosts++
			fmt.Fprintf(w, "\nhost %s (one use, until %s), on the machine they lend:\n  zm pool host '%s'%s\n  their machine will be %s\n%s",
				name, r.Invite.Expires.Local().Format("Jan 2 15:04"), r.Code, images, classMeans[h.class], tailnetStep(name, r.Code))
		}
	}
	if len(in.Renters)+hosts == 0 {
		fmt.Fprintf(w, "next: zm pool create %s --renter <friend> or --host <friend> prints a code for each (run it again any time)\n", slug)
	}
	fmt.Fprintf(w, "lend this machine too: zm pool host %s%s\n", slug, images)
	return nil
}

// tailnetStep is what the owner does so a friend's machine reaches this one
// with a tailnet code (SPEC 17.4: the friend on the owner's tailnet, or this
// machine's node shared into theirs), in plain words with where to do it; ""
// for an https (tunnel) code, which needs no tailnet.
func tailnetStep(name, code string) string {
	if strings.HasPrefix(code, "https://") || strings.HasPrefix(code, "http://") {
		return ""
	}
	node := "this machine's node" // its name in the admin console: potluck-<machine>, or zm-<machine> for an older node
	if jc, err := project.ParseJoinCode(code); err == nil {
		node = jc.NodePrefix + jc.Machine // the code's own node
	}
	return fmt.Sprintf("  before %s runs it, their machine must reach this one over Tailscale: if they are not on your tailnet,\n"+
		"  share this machine with them at https://login.tailscale.com/admin/machines (%s, then the ... menu, Share)\n"+
		"  and send them the link, or invite them to your tailnet at https://login.tailscale.com/admin/users\n", name, node)
}

// ensureTailnet brings this machine's tailnet node up, waiting out the first
// login (its URL printed once). When it gives up before the login (ctrl-C,
// the timeout, a node error) it stops a node this run turned on, unless the
// node got logged in meanwhile, so the daemon does not keep asking for a
// login; and it says nothing was created.
func ensureTailnet(ctx context.Context, c *client.Client, w io.Writer) error {
	st, err := c.Tailnet(ctx)
	if err == nil && st.Running {
		return nil
	}
	startedHere := err == nil && !st.Enabled // the node was off: this run turns it on
	giveUp := func(why string) error {
		note := "the tailnet node was already on and is left as it was (zm tailnet status; zm tailnet down stops it)"
		if startedHere {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if now, err := c.Tailnet(stop); err == nil && now.Running {
				note = "the tailnet node is logged in now; run this again"
			} else if _, err := c.TailnetDown(stop); err != nil {
				note = "stopping the tailnet node it started failed (" + err.Error() + "): zm tailnet down"
			} else {
				note = "the tailnet node it started is stopped"
			}
		}
		return fmt.Errorf("%s: nothing was created; %s", why, note)
	}
	const cancelled = "cancelled while waiting for the Tailscale login"
	if st, err = c.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: os.Getenv("TS_AUTHKEY")}); err != nil {
		if ctx.Err() != nil {
			return giveUp(cancelled)
		}
		return giveUp("tailnet: " + err.Error())
	}
	shown, deadline := "", time.Now().Add(tailnetWait)
	for !st.Running {
		switch {
		case st.Error != "":
			return giveUp("tailnet: " + st.Error)
		case time.Now().After(deadline):
			return giveUp(fmt.Sprintf("no Tailscale login within %s; log in, then run this again (or set TS_AUTHKEY)", tailnetWait))
		case st.AuthURL != "" && st.AuthURL != shown:
			if shown == "" {
				fmt.Fprintf(w, "this pool needs this machine on a tailnet: a Tailscale account (free) is needed.\n"+
					"waiting for you to log in to Tailscale; nothing is created until then (ctrl-C cancels):\n")
			}
			fmt.Fprintf(w, "  %s\n", st.AuthURL)
			shown = st.AuthURL
		}
		select {
		case <-ctx.Done():
			return giveUp(cancelled)
		case <-time.After(3 * time.Second):
		}
		if st, err = c.Tailnet(ctx); err != nil {
			if ctx.Err() != nil {
				return giveUp(cancelled)
			}
			return giveUp("tailnet: " + err.Error())
		}
	}
	return nil
}

func poolJoinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join '<invite code>'",
		Short: "Join a pool with the invite code its owner sent, and list the boxes you can run sandboxes on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			rem, err := poolJoin(cmd.Context(), c, args[0], os.Stdout)
			if err != nil {
				return err
			}
			poolBoxes(cmd.Context(), c, rem.Project.Slug, os.Stdout)
			fmt.Printf("next: zm sandbox create -P %s, then zm sandbox exec <handle> -- uname -a\n", rem.Project.Slug)
			return nil
		},
	}
}

// poolJoin redeems an invite code as zm join does.
func poolJoin(ctx context.Context, c *client.Client, code string, w io.Writer) (api.Remote, error) {
	if !project.LooksLikeJoinCode(code) {
		return api.Remote{}, fmt.Errorf("%q is not an invite code: the pool's owner prints one with zm pool create --renter <you>", code)
	}
	req := api.JoinRequest{Code: strings.TrimSpace(code)}
	req.Cwd, _ = os.Getwd()
	r, err := joinUntilDone(ctx, c, req)
	if err != nil {
		return api.Remote{}, err
	}
	if r.Remote == nil {
		return api.Remote{}, errors.New("nothing joined")
	}
	x := r.Remote
	fmt.Fprintf(w, "joined pool %s on %s as %s (%s), machine %s\n", x.Project.Slug, x.Authority, x.Identity, x.Cap.Role, x.Machine)
	return *x, nil
}

// inPool is c with the pool as its project: the team proxy sends its run
// target calls to the pool's authority.
func inPool(c *client.Client, pool string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom = pool, "flag"
	return &x
}

// poolBoxViews are the pool's box targets as its authority lists them.
func poolBoxViews(ctx context.Context, c *client.Client, pool string) ([]computeapi.RunTargetView, error) {
	l, err := computeclient.RunTargets(ctx, inPool(c, pool), false)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(l.Targets, func(t computeapi.RunTargetView) bool { return t.Kind != "box" }), nil
}

// poolBoxes prints the pool's box targets as its authority lists them.
func poolBoxes(ctx context.Context, c *client.Client, pool string, w io.Writer) []string {
	views, err := poolBoxViews(ctx, c, pool)
	if err != nil {
		fmt.Fprintf(w, "boxes: not listed (%v)\n", err)
		return nil
	}
	var names []string
	for _, t := range views {
		names = append(names, t.Name)
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "no boxes lent to the pool yet: a member lends a machine with zm pool host")
	} else {
		fmt.Fprintf(w, "boxes in the pool: %s\n", strings.Join(names, ", "))
	}
	return names
}

func poolHostCmd() *cobra.Command {
	var s shareFlags
	cmd := &cobra.Command{
		Use:   "host '<invite code>'|<pool> [--image name@sha256:<digest>] [share flags]",
		Short: "Lend this machine to a pool: check it, join with the invite code (unless joined), share its boxes with the pool and wait until the pool lists them",
		Long: `Host runs what zm host doctor, zm join and zm host share --pool do, in one go:
it checks this machine (a missing root step it runs with sudo at a
terminal, which asks for your password; else, or with --no-sudo, it prints
the one sudo line),
pulls the images (without --image, zm's default for the layer) and runs
share's quick conformance check, and only when a size passed joins the pool with the invite code (or takes a pool this machine
already joined, or runs) and binds each size that passed, shared with the
pool; then it waits for the pool's authority to list the boxes. A machine
that fails never joins, and its invite stays unspent. On a Mac a box ends when you touch the
keyboard or pointer (--reclaim-idle-s). A box serves GitHub Actions jobs
when its image has GitHub's runner (zm's do; --runner-dir for another) and,
on Linux, Potluck's filtered network exists (doctor's root line makes it); else it
is lent for jobs only, and host says why. Once lent, it installs zm's daily
update: the newest signed release, once it is 6h old (--no-auto-update skips
it; zm daemon update disable removes it). Every share flag applies.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := s.options(cmd)
			if err != nil {
				return err
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			err = poolHost(cmd.Context(), c, shareRunner, args[0], o, os.Stdout, s.root(cmd.Context(), cmd.ErrOrStderr()), s.afterLend())
			if sc := (*spentCodeError)(nil); errors.As(err, &sc) {
				sc.Flags = shareFlagArgs(cmd)
			}
			return err
		},
	}
	s.add(cmd, false)
	return cmd
}

// poolHost lends this machine to the pool. root, when set, runs the root
// step once the pool is known and share's own checks pass; after, when set,
// runs once the machine is lent (zm's daily update).
func poolHost(ctx context.Context, c *client.Client, run hostprobe.Runner, arg string, o hostprobe.ShareOptions, w io.Writer, root func(*hostprobe.ShareOptions) error, after func(io.Writer)) error {
	_, err := poolHostResult(ctx, c, run, arg, o, w, root, after)
	return err
}

// poolHostResult is poolHost, with what share did (zm lend says it in its own words).
func poolHostResult(ctx context.Context, c *client.Client, run hostprobe.Runner, arg string, o hostprobe.ShareOptions, w io.Writer, root func(*hostprobe.ShareOptions) error, after func(io.Writer)) (res hostprobe.ShareResult, err error) {
	if root == nil {
		root = func(*hostprobe.ShareOptions) error { return nil }
	}
	if o.DryRun {
		if err := root(&o); err != nil {
			return res, err
		}
		res, err = runShare(ctx, run, nil, o, w)
		if res.RunnerDir != "" {
			pool := "<pool>"
			if !project.LooksLikeJoinCode(arg) {
				pool = arg
			}
			fmt.Fprintf(w, "note: runners only on a trusted host: a host the pool's invite made public serves no GitHub Actions jobs; its owner runs zm machine-class <this machine> trusted -P %s\n", pool)
		}
		return res, err
	}
	byCode := project.LooksLikeJoinCode(arg)
	pool, machine := arg, ""
	if !byCode { // a pool named: known before anything runs
		rems, err := c.Remotes(ctx)
		if err != nil {
			return res, err
		}
		for _, r := range rems {
			if r.Project.Slug == pool {
				machine = r.Machine
			}
		}
		if machine == "" {
			if _, err := c.GetProject(ctx, pool); err != nil {
				return res, fmt.Errorf("%s is no pool this machine joined or runs: pass the invite code, zm pool host '<code>'", pool)
			}
		}
	}
	if c != nil { // the bindings this run replaces, in its scope (nil: tests that stop before the join)
		var l computeapi.RunTargetList
		var err error
		if byCode || machine != "" {
			l, err = computeclient.HostRunTargets(ctx, c)
		} else {
			l, err = computeclient.RunTargets(ctx, inPool(c, pool), false)
		}
		if err != nil {
			o.EarlierUnread = err.Error() // fail closed in Check
		} else {
			o.Earlier = targetsOf(l)
		}
	}
	if err := root(&o); err != nil {
		return res, err
	}
	// Doctor, pulls and conformance before the join: a machine that cannot
	// lend, or fails the security checks, never spends the invite.
	res, err = hostprobe.Check(ctx, run, o)
	if err != nil {
		return res, err
	}
	if !slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Passed }) {
		var nb hostprobe.Binder // binds nothing: says each size is not bound
		no := o
		if !byCode && c != nil { // where it would bind, so a keyless earlier binding is unbound
			nb = poolBinder(c, &no, pool, machine)
		}
		res = hostprobe.Bind(ctx, nb, no, res)
		_ = reportShare(res, w)
		return res, errors.New("no size passed conformance: this machine did not join the pool")
	}
	if byCode {
		rem, err := poolJoin(ctx, c, arg, w)
		if err != nil {
			if ours := joinedAtAuthority(ctx, c, arg); len(ours) > 0 && api.IsCode(err, api.CodeForbidden) {
				return res, &spentCodeError{err: err, Pools: ours}
			}
			return res, err
		}
		pool, machine = rem.Project.Slug, rem.Machine
	}
	res = hostprobe.Bind(ctx, poolBinder(c, &o, pool, machine), o, res)
	if err := reportShare(res, w); err != nil {
		if after != nil && lent(res) { // some sizes serve: the machine is lent
			after(w)
		}
		return res, err
	}
	var want []string
	for _, s := range res.Sizes {
		if s.Bound {
			if machine != "" {
				want = append(want, s.Binding+"@"+machine)
			} else {
				want = append(want, s.Binding)
			}
		}
	}
	if err := awaitListed(ctx, c, pool, want, res.RunnerDir != "", w); err != nil {
		return res, err
	}
	if after != nil {
		after(w)
	}
	return res, nil
}

// spentCodeError is a join code that failed (invalid, used or expired) on a
// machine that already has healthy remotes at the code's authority. A code
// names no pool, so zm never guesses: it names the pools and the command that
// shares with one of them, and the run fails.
type spentCodeError struct {
	err   error
	Pools []string
	Flags string // the share flags the user passed, as they would retype them
}

func (e *spentCodeError) Unwrap() error { return e.err }

func (e *spentCodeError) Error() string {
	var b strings.Builder
	b.WriteString(e.err.Error())
	fmt.Fprintf(&b, "\nthis machine already joined %s at that authority; to lend it to one, run:", strings.Join(e.Pools, ", "))
	for _, p := range e.Pools {
		fmt.Fprintf(&b, "\n  zm host share --pool %s%s", p, e.Flags)
	}
	return b.String()
}

// joinedAtAuthority is the pools this machine holds a healthy remote for at
// the authority a join code names (same fingerprint, and for a tailnet code
// the same machine). A code carries no project, so this only informs a hint.
func joinedAtAuthority(ctx context.Context, c *client.Client, code string) []string {
	jc, err := project.ParseJoinCode(code)
	if err != nil || c == nil {
		return nil
	}
	rems, err := c.Remotes(ctx)
	if err != nil {
		return nil
	}
	var pools []string
	for _, r := range rems {
		if r.State == api.RemoteOK && strings.EqualFold(r.Fingerprint, jc.Fingerprint) && (jc.Machine == "" || r.Authority == jc.Machine) {
			pools = append(pools, r.Project.Slug)
		}
	}
	return pools
}

// shareFlagArgs renders the flags the user set as arguments to retype.
func shareFlagArgs(cmd *cobra.Command) string {
	var b strings.Builder
	cmd.Flags().Visit(func(f *pflag.Flag) {
		vals := []string{f.Value.String()}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			vals = sv.GetSlice()
		}
		for _, v := range vals {
			if f.Value.Type() == "bool" {
				fmt.Fprintf(&b, " --%s=%s", f.Name, v)
			} else {
				fmt.Fprintf(&b, " --%s %s", f.Name, shellQuote(v))
			}
		}
	})
	return b.String()
}

func shellQuote(v string) string {
	if v != "" && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// poolBinder is where pool host binds: machine-level, shared with the
// pool's authority (o.Pool), for a pool this machine joined; else, on the
// pool's own machine, a binding in it.
func poolBinder(c *client.Client, o *hostprobe.ShareOptions, pool, machine string) hostprobe.Binder {
	if machine != "" {
		o.Pool = pool
		return computeclient.Owner{C: c}
	}
	return computeclient.Owner{C: inPool(c, pool)}
}

// lent reports whether any size of res is bound.
func lent(res hostprobe.ShareResult) bool {
	return slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Bound })
}

// awaitListed waits up to advertWait for the pool's authority to list want.
// Boxes that would serve runners (runners) on a host the authority holds
// public serve none: the broker gives a runner only to a trusted host, so it
// says so with the owner's command.
func awaitListed(ctx context.Context, c *client.Client, pool string, want []string, runners bool, w io.Writer) error {
	deadline := time.Now().Add(advertWait)
	for {
		views, _ := poolBoxViews(ctx, c, pool)
		var names []string
		public := ""
		for _, v := range views {
			names = append(names, v.Name)
			if slices.Contains(want, v.Name) && v.HostClass != api.MachineTrusted {
				public = v.Host
			}
		}
		missing := slices.DeleteFunc(slices.Clone(want), func(n string) bool { return slices.Contains(names, n) })
		if len(missing) == 0 {
			fmt.Fprintf(w, "lent to pool %s: %s\nrenters: zm sandbox create -P %s\n", pool, strings.Join(want, ", "), pool)
			if runners && public != "" {
				fmt.Fprintf(w, "note: pool %s holds %s public, so its boxes serve no GitHub Actions jobs (a runner goes only to a trusted host)\n"+
					"  if the pool's owner trusts this machine, they run: zm machine-class %s trusted -P %s\n", pool, public, public, pool)
			}
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "bound, but the pool does not list %s yet: it does once this machine's heartbeat reaches its authority (zm target ls -P %s)\n", strings.Join(missing, ", "), pool)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
