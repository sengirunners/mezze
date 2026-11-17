package main

// `zm host doctor` and `zm host share` (SPEC 20, "Host onboarding"): check what
// this machine can lend as run-target boxes, then pull digest-pinned images,
// run the conformance suite and bind only what passed. Owned by run targets;
// the logic is potluck/hostprobe.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
)

func init() {
	h := &cobra.Command{Use: "host", Short: "Lend this machine as run-target boxes: check it, then share it"}
	h.AddCommand(hostDoctorCmd(), hostShareCmd())
	rootCmd.AddCommand(h)
}

type hostFlags struct {
	share    float64
	maxCores int
	maxRAMMB int64
	stateDir string
	flags    *pflag.FlagSet
}

func (f *hostFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	f.flags = fl
	fl.Float64Var(&f.share, "share", hostprobe.DefaultShare, "the fraction of this machine's cores and RAM to lend (0 to 1]")
	fl.IntVar(&f.maxCores, "max-cores", 0, "cap the lent cores (0: no cap beyond --share)")
	fl.Int64Var(&f.maxRAMMB, "max-ram-mb", 0, "cap the lent RAM in MB (0: no cap beyond --share)")
	fl.StringVar(&f.stateDir, "state-dir", "", "where the driver keeps images and records (default: the zm home)")
}

func (f *hostFlags) limits() (hostprobe.Limits, error) {
	if f.share <= 0 || f.share > 1 {
		return hostprobe.Limits{}, errors.New("--share: want a fraction above 0 and at most 1")
	}
	lim := hostprobe.Limits{Share: f.share, MaxCores: f.maxCores, MaxRAMMB: f.maxRAMMB}
	if !f.flags.Changed("share") {
		lim.Share = 0 // zm's default, not the owner's: hostprobe.Lend may lend what fits once
	}
	return lim, nil
}

func (f *hostFlags) dir() string {
	if f.stateDir != "" {
		return f.stateDir
	}
	if cfg, err := config.LoadClient(); err == nil && cfg.Home != "" {
		return cfg.Home
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".zm")
}

type hostDoctorReport struct {
	hostprobe.Report
	Share float64          `json:"share"`
	Sizes []hostprobe.Size `json:"sizes"`
	// OneAtATime: the sizes fit once inside zm's default share, not twice,
	// and share binds them one box at a time.
	OneAtATime bool `json:"one_at_a_time,omitempty"`
	// Admission is each box binding that asks an admission controller first.
	Admission   []doctorAdmission `json:"admission,omitempty"`
	Best        string            `json:"best_layer,omitempty"`
	RootCommand string            `json:"root_command,omitempty"`
}

func hostDoctorCmd() *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{Use: "doctor", Short: "Report what this machine can lend and the one root command that would fix the rest", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			lim, err := f.limits()
			if err != nil {
				return err
			}
			r := detectHost(cmd.Context(), f.dir())
			out := hostDoctorReport{Report: r, Share: lim.Fraction()}
			var images []string // what share lends without --image: the best layer's default
			if b, ok := r.Best(); ok {
				out.Best = b.Name
				if p, err := hostprobe.DefaultImage(b.Name); err == nil {
					images = []string{p.Image}
					for _, tp := range hostprobe.LayerTemplates(b.Name) { // and its templates'
						if tp.OS == p.OS && tp.Image != "" {
							images = append(images, tp.Image)
						}
					}
				}
			}
			l := hostprobe.Lend(r, lim, images)
			out.Sizes, out.OneAtATime = l.Sizes, l.OneAtATime
			out.RootCommand = hostprobe.RootCommand(r.RootSteps(out.Best))
			out.Admission = bindingAdmission(cmd.Context())
			if flagJSON {
				return printJSON(out)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s/%s, %d cores, %d MB RAM, %.0f GB free in %s, kvm: %t\n", r.OS, r.Arch, r.Cores, r.RAMMB, r.FreeDiskGB, r.StateDir, r.KVM)
			for _, l := range r.Layers {
				state := "not ready"
				switch {
				case l.Available && !l.Supported:
					state = "ready, but zm cannot bind it yet"
				case l.Available:
					state = "ready"
				}
				detail := strings.TrimSpace(strings.Join([]string{l.Version, l.CLI, l.Runtime}, " "))
				fmt.Fprintf(w, "  %-6s %s %s\n", l.Name, state, detail)
				for _, p := range l.Problems {
					fmt.Fprintf(w, "         - %s\n", p)
				}
				if l.Network != "" {
					capped := ""
					if l.NetworkMbit > 0 {
						capped = fmt.Sprintf("; downloads capped at %d Mbit/s for all of them together", l.NetworkMbit)
					}
					fmt.Fprintf(w, "         GitHub runners and sandboxes: filtered network %s (internet, never this machine or its LAN%s)\n", l.Network, capped)
				} else if l.NetworkProblem != "" && l.Available {
					fmt.Fprintf(w, "         - %s\n", l.NetworkProblem)
				}
			}
			var n []string
			for _, s := range out.Sizes {
				n = append(n, s.Name)
			}
			switch {
			case len(n) == 0 && len(hostprobe.Lend(r, lim, nil).Sizes) > 0:
				how := "twice inside " + lim.Within()
				if hostprobe.Lend(r, lim, nil).OneAtATime {
					how = "inside " + lim.Within() + ", even one at a time"
				}
				fmt.Fprintf(w, "sizes: none big enough for zm's default image fit %s\n", how)
			case len(n) == 0 && lim.Share == 0:
				fmt.Fprintf(w, "sizes: none fit inside %s, even one at a time\n", lim.Within())
			case len(n) == 0:
				fmt.Fprintf(w, "sizes: none fit twice inside %s\n", lim.Within())
			case out.OneAtATime:
				fmt.Fprintf(w, "sizes (one at a time: none fits twice inside %.0f%%): %s\n", out.Share*100, strings.Join(n, ", "))
			default:
				fmt.Fprintf(w, "sizes (two fit inside %.0f%%): %s\n", out.Share*100, strings.Join(n, ", "))
			}
			printDoctorAdmission(w, out.Admission)
			if out.RootCommand != "" {
				fmt.Fprintf(w, "as root, once:\n  %s\n", out.RootCommand)
			}
			if out.Best != "" {
				fmt.Fprintf(w, "next: zm host share   (layer %s; zm's default image unless --image name@sha256:<digest>)\n", out.Best)
			}
			return nil
		}}
	f.add(cmd)
	return cmd
}

// shareRunner runs share's pulls and conformance (tests replace it).
var shareRunner hostprobe.Runner = hostprobe.ExecRunner{}

// detectHost reads this machine (tests replace it).
var detectHost = func(ctx context.Context, dir string) hostprobe.Report {
	return hostprobe.Detect(ctx, hostprobe.ExecRunner{}, hostprobe.SystemFacts{}, dir)
}

// rootStep is how zm host share and zm pool host run doctor's root step
// themselves: sudo asks the person at the terminal for the
// password; zm never reads, sees, stores or passes it. Tests replace it.
type rootStep struct {
	tty  func() bool                                    // stdin and stdout are a terminal
	sudo func() bool                                    // sudo is on PATH
	run  func(ctx context.Context, script string) error // sudo sh -c script, on the terminal
}

var hostRoot = rootStep{
	tty: onATerminal,
	sudo: func() bool {
		_, err := exec.LookPath("sudo")
		return err == nil
	},
	// Not CommandContext: Ctrl-C reaches sudo from the terminal, and sudo
	// then puts the terminal's echo back; a kill could leave it off, or cut
	// a package install short.
	run: func(_ context.Context, script string) error {
		c := exec.Command("sudo", "sh", "-c", script)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr // stdout stays share's (--json)
		return c.Run()
	},
}

// first runs the root step share needs (o.RootSteps: doctor's root line)
// with sudo when a person at a terminal can answer sudo, then reads the
// machine again, so share goes on as if the owner had run it. Else, or when
// sudo does not finish, it says why in plain words and share goes on as
// before: it prints the root line and lends jobs only, or refuses. Only a
// cancelled command (Ctrl-C, which sudo hears from the terminal) is an error.
func (rs rootStep) first(ctx context.Context, o *hostprobe.ShareOptions, noSudo bool, again func() hostprobe.Report, w io.Writer) error {
	steps := o.RootSteps()
	if len(steps) == 0 {
		return nil
	}
	need := "zm needs admin rights once to " + rootWhy(*o, steps)
	switch {
	case o.DryRun:
		fmt.Fprintf(w, "%s; a dry run does not ask: the root line below does it\n", need)
	case noSudo:
		fmt.Fprintf(w, "%s; --no-sudo: run the root line below yourself, then this again\n", need)
	case !rs.tty():
		fmt.Fprintf(w, "%s; not at a terminal, so zm did not ask: run the root line below yourself, then this again\n", need)
	case !rs.sudo():
		fmt.Fprintf(w, "%s; sudo is not installed: run the root line below as root, then this again\n", need)
	default:
		fmt.Fprintf(w, "%s; sudo may ask for your password (zm host doctor prints the exact command).\n", need)
		err := rs.run(ctx, hostprobe.RootScript(steps))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.Report = again() // what the step did, even part way
		switch {
		case err != nil:
			fmt.Fprintf(w, "the admin step did not finish (%v): zm goes on without it; the root line below does it\n", err)
		case len(o.RootSteps()) > 0:
			fmt.Fprintln(w, "the admin step ran, but this machine is not ready yet (if it added you to a group, such as kvm, that counts only from your next login): what is left is below")
		}
	}
	return nil
}

// capStepsOf says steps are what --net-mbit alone runs: the cap steps under
// the names of one of the layers' networks (an old-name machine keeps zm-box).
func capStepsOf(o hostprobe.ShareOptions, steps []string) bool {
	for _, l := range o.Report.Layers {
		if slices.Equal(steps, hostprobe.BoxNetCapStepsFor(l, o.NetMbit)) {
			return true
		}
	}
	return slices.Equal(steps, hostprobe.BoxNetCapSteps(o.NetMbit))
}

// rootWhy says in plain words what the root steps are for.
func rootWhy(o hostprobe.ShareOptions, steps []string) string {
	var network []string
	for _, l := range o.Report.Layers {
		network = append(network, l.NetworkSteps...)
	}
	// --net-mbit alone: its steps rewrite the network's unit with the cap.
	if o.NetMbitSet && capStepsOf(o, steps) {
		switch {
		case len(network) > 0 && o.NetMbit == 0:
			return "set up the firewall its VMs run behind"
		case len(network) > 0:
			return fmt.Sprintf("set up the firewall its VMs run behind, with downloads capped at %d Mbit/s", o.NetMbit)
		case o.NetMbit == 0:
			return "remove the download cap from its VMs' network"
		}
		return fmt.Sprintf("cap its VMs' downloads at %d Mbit/s", o.NetMbit)
	}
	net, other := false, false
	for _, s := range steps {
		if slices.Contains(network, s) {
			net = true
		} else {
			other = true
		}
	}
	switch {
	case net && other:
		return "set up what its VMs need and the firewall they run behind"
	case net:
		return "set up the firewall its VMs run behind"
	}
	return "set up what its VMs need on this machine"
}

// shareFlags are zm host share's flags, which zm pool host takes too.
type shareFlags struct {
	f      hostFlags
	o      hostprobe.ShareOptions
	sizes  []string
	dryRun bool
	noSudo bool
	// noAutoUpdate: lend without installing zm's daily update.
	noAutoUpdate bool
	// poolHost: zm pool host, which binds machine-level or in the pool.
	poolHost bool
	// shelfGB and volumeMaxGB are --shelf-gb and --volume-max-gb, sent only
	// when given (a re-run keeps an earlier share's otherwise).
	shelfGB, volumeMaxGB int
}

func (s *shareFlags) add(cmd *cobra.Command, pool bool) {
	s.f.add(cmd)
	s.poolHost = !pool
	fl, o := cmd.Flags(), &s.o
	fl.StringArrayVar(&o.Images, "image", nil, "an image pinned by digest, name@sha256:<64 hex> (repeat; default: zm's pinned image for the layer, macOS on tart)")
	fl.StringVar(&o.Layer, "layer", "", "oci or tart (default: the best available)")
	fl.StringSliceVar(&s.sizes, "sizes", nil, "sizes to share, e.g. c2m4,c4m16 (default: every size that fits)")
	if pool {
		fl.StringVar(&o.Pool, "pool", "", "lend the boxes to this team project's pool: bind them machine-level and share them with it")
	}
	fl.StringVar(&o.NamePrefix, "name-prefix", "", "bindings are <prefix>-<size> (default: this machine's zm name)")
	fl.StringVar(&o.OCIRuntime, "oci-runtime", "", "oci: the isolating runtime (Kata or runsc; default: detected)")
	fl.StringVar(&o.OCIHostProfile, "oci-host-profile", "", "oci: absolute owner resource-profile JSON; required for Kata's admitted, bounded host resources")
	fl.StringVar(&o.OCICLI, "oci-cli", "", "oci: verified local Docker (default: detected; Podman and nerdctl are unavailable)")
	fl.StringVar(&o.OCINetwork, "oci-network", "", "oci: a filtered network you made for zm (default: zm's own for boxes that serve GitHub runners when doctor finds it, else none; a named one needs --net-mail)")
	fl.BoolVar(&o.Docker, "docker", false, "oci: lend sandboxes with root (sudo) and rootless Docker inside; on gVisor this registers runsc-root (runsc with --allow-suid) with docker once, beside runsc (default: what an earlier share bound; --docker=false turns it off)")
	fl.BoolVar(&o.Root, "root", false, "tart: sandboxes keep the image's passwordless sudo, root inside each throwaway VM as on GitHub's macOS runners; conformance checks root is still fenced (default: what an earlier share bound; --root=false turns it off)")
	fl.StringVar(&o.RunnerDir, "runner-dir", "", "where the image keeps GitHub's runner, so each box serves Actions jobs (default: from zm's pin for its own images; none: no runner)")
	fl.StringArrayVar(&o.NetAllow, "net-allow", nil, "host:port each box must reach (the internet), passed to conformance (repeat)")
	fl.StringArrayVar(&o.NetForbid, "net-forbid", nil, "host:port no box may reach (a LAN host, the router, metadata), passed to conformance with this machine's own addresses (repeat)")
	fl.StringArrayVar(&o.NetMail, "net-mail", nil, "an outside mail server (host, checked on 25, 465 and 587, or host:port) no box may open, passed to conformance (repeat)")
	fl.IntVar(&o.NetMbit, "net-mbit", 0, "oci: cap what all boxes on zm's network download together, in Mbit/s, for a machine whose uplink you guard; a root step (default: what an earlier share set; 0 removes the cap)")
	fl.IntVar(&s.shelfGB, "shelf-gb", 0, "oci: turn the package shelf on: make the potluck-cache volume (if missing) and cap what it holds at this many GiB, on every size, so boxes find public npm packages on a read-only shelf (Potluck docs/shelf.md); 0 fills nothing (default: what an earlier share set)")
	fl.StringVar(&o.ShelfFill, "shelf-fill", "", "oci: what fills the shelf: seed (only what the host seeds; the default), boxes (also the public packages lockfiles in boxes name, so a box can tell someone on this host recently used one) or off; needs --shelf-gb above 0 (default: what an earlier share set)")
	fl.IntVar(&s.volumeMaxGB, "volume-max-gb", 0, "oci: the most each renter's named volumes may hold on each size, in GiB (default 20; 0 turns volumes off; default: what an earlier share set)")
	fl.BoolVar(&s.dryRun, "dry-run", false, "print the plan; pull, test and bind nothing")
	fl.BoolVar(&s.noAutoUpdate, "no-auto-update", false, "lend without installing zm's daily update (the newest signed release, once a day; zm daemon update disable removes it later)")
	fl.BoolVar(&s.noSudo, "no-sudo", false, "never run the root step zm host doctor prints with sudo: print it, and lend without it")
	fl.BoolVar(&o.Full, "full", false, "run the whole conformance suite on every size (minutes per size); default: the security checks once, a boot check per size")
	fl.IntVar(&o.ReclaimIdleS, "reclaim-idle-s", 0, fmt.Sprintf("macOS: a box ends when you touch the keyboard or pointer and starts only after this many idle seconds; 0 lends all the time (default %d on macOS, 0 elsewhere)", hostprobe.DefaultReclaimIdleS))
	s.addAdmission(cmd)
}

// afterLend is what runs once the machine is lent: zm's daily update (to
// stderr under --json, so stdout stays JSON), nil with --no-auto-update or
// in a dry run.
func (s *shareFlags) afterLend() func(io.Writer) {
	if s.noAutoUpdate || s.dryRun {
		return nil
	}
	return func(w io.Writer) {
		if flagJSON {
			w = os.Stderr
		}
		autoUpdateAfterLend(w)
	}
}

// root is the root step for share and pool host to run once their own
// checks pass (hostprobe.Precheck, so zm never asks for root and then
// refuses): rootStep.first, its words to w.
func (s *shareFlags) root(ctx context.Context, w io.Writer) func(*hostprobe.ShareOptions) error {
	return func(o *hostprobe.ShareOptions) error {
		if len(o.RootSteps()) == 0 {
			return nil
		}
		if err := hostprobe.Precheck(ctx, shareRunner, *o); err != nil {
			return err
		}
		return hostRoot.first(ctx, o, s.noSudo, func() hostprobe.Report { return detectHost(ctx, s.f.dir()) }, w)
	}
}

// options are the flags as ShareOptions, with this machine's facts.
func (s *shareFlags) options(cmd *cobra.Command) (hostprobe.ShareOptions, error) {
	o := s.o
	lim, err := s.f.limits()
	if err != nil {
		return o, err
	}
	o.Limits, o.Sizes, o.DryRun = lim, s.sizes, s.dryRun
	if !cmd.Flags().Changed("reclaim-idle-s") {
		o.ReclaimIdleS = defaultReclaimIdleS()
	}
	// Without --docker a re-run keeps an earlier share's (--docker=false turns it off).
	o.DockerSet = cmd.Flags().Changed("docker")
	o.RootSet = cmd.Flags().Changed("root")
	// Without --net-mbit a re-run keeps the cap the network's unit has (--net-mbit 0 removes it).
	o.NetMbitSet = cmd.Flags().Changed("net-mbit")
	if cmd.Flags().Changed("shelf-gb") {
		o.ShelfGB = &s.shelfGB
	}
	if cmd.Flags().Changed("volume-max-gb") {
		o.VolumeMaxGB = &s.volumeMaxGB
	}

	cfg, err := config.LoadClient() // read without touching the daemon
	if err != nil {
		return o, err
	}
	if o.NamePrefix == "" { // the machine name
		o.NamePrefix = strings.ToLower(cfg.Machine)
	}
	// Conformance runs the driver file the daemon will run and pin (its
	// program's directory, then $ZM_HOME/drivers: DRIVERS.md), not a name
	// looked up on PATH.
	var note string
	o.DriverDirs, note = daemonDriverDirs(cmd.Context(), cfg.Home, cfg.SocketPath())
	if note != "" {
		fmt.Fprintln(os.Stderr, "zm:", note)
	}
	s.earlier(cmd.Context(), &o) // a re-run keeps an earlier share's admission keys
	o.Report = detectHost(cmd.Context(), s.f.dir())
	o.HostAddrs = hostprobe.HostAddrs()
	if !flagJSON {
		o.Progress = func(s string) { fmt.Fprintln(os.Stderr, s) }
	}
	return o, nil
}

// runShare shares, prints the result and fails when a size stayed unbound.
func runShare(ctx context.Context, run hostprobe.Runner, b hostprobe.Binder, o hostprobe.ShareOptions, w io.Writer) (hostprobe.ShareResult, error) {
	res, err := hostprobe.Share(ctx, run, b, o)
	if err != nil {
		return res, err
	}
	return res, reportShare(res, w)
}

// reportShare prints a share's result and fails when a size stayed unbound.
func reportShare(res hostprobe.ShareResult, w io.Writer) error {
	if flagJSON {
		if err := printJSON(res); err != nil {
			return err
		}
	} else {
		printShare(w, res)
	}
	if res.Failed() {
		return errors.New("some sizes stayed unbound")
	}
	return nil
}

func hostShareCmd() *cobra.Command {
	var s shareFlags
	cmd := &cobra.Command{
		Use:   "share [--image name@sha256:<digest>]... [flags]",
		Short: "Pull the pinned images, run conformance per size and bind each size that passed (the machine's owner only)",
		Long: `Share pulls each digest-pinned image (without --image, zm's default
for the layer, pinned inside zm) through the layer's CLI, runs
potluck-conformance on each size's box config and binds <prefix>-<size> as an
ephemeral, credential-free box only when it passed. A size that fails stays
unbound and is reported. By default the security checks run once, on the
first size (a box sees nothing of the last, secrets, refusals, and the
network: no box reaches another, nor any of this machine's addresses, share
listening on one port for the checks, nor a --net-forbid address, nor a
--net-mail server), and each other size gets a boot check; --full runs the
whole suite on every size. On a Mac a box ends (reclaimed, never billed) when
you touch the keyboard or pointer, and none starts until you have left them
alone for --reclaim-idle-s. A box serves GitHub Actions jobs when its image
carries GitHub's runner (zm's default images do; --runner-dir for another);
on oci such a box needs a network: zm's filtered one, made by the root line
zm host doctor prints, else it is lent for jobs only and share says why.
At a terminal share runs that root line itself with sudo, which asks for
your password (zm never sees it); --no-sudo prints it instead. Without
--share, a machine too small for two boxes of any size inside half of it
lends what fits once, one box at a time.
--dry-run prints the plan and does nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := s.options(cmd)
			if err != nil {
				return err
			}
			var b hostprobe.Binder // a dry run makes no client, so it never autostarts the daemon
			if !o.DryRun {
				c, _, err := newClient()
				if err != nil {
					return err
				}
				// A project binding: know which project before a sudo prompt and minutes of conformance.
				if o.Pool == "" {
					if err := shareProject(cmd.Context(), c, cmd.ErrOrStderr()); err != nil {
						return err
					}
				}
				b = computeclient.Owner{C: c}
			}
			if err := s.root(cmd.Context(), cmd.ErrOrStderr())(&o); err != nil {
				return err
			}
			res, err := runShare(cmd.Context(), shareRunner, b, o, cmd.OutOrStdout())
			if after := s.afterLend(); after != nil && lent(res) { // a size that failed leaves the rest lent
				after(cmd.OutOrStdout())
			}
			return err
		},
	}
	s.add(cmd, true)
	return cmd
}

// defaultReclaimIdleS: a Mac is lent only while its owner is away from it.
func defaultReclaimIdleS() int {
	if runtime.GOOS == "darwin" {
		return hostprobe.DefaultReclaimIdleS
	}
	return 0
}

func printShare(w io.Writer, res hostprobe.ShareResult) {
	verb := "ran"
	if res.DryRun {
		verb = "would run"
	}
	fmt.Fprintf(w, "layer %s\n", res.Layer)
	switch {
	case res.DefaultImage && len(res.Images) > 1: // the default, then its templates'
		fmt.Fprintf(w, "images %s (zm's default, then its templates for zm sandbox create --template; --image to choose another)\n", strings.Join(res.Images, ", "))
	case res.DefaultImage:
		fmt.Fprintf(w, "image %s (zm's default; --image to choose another)\n", strings.Join(res.Images, ", "))
	}
	if !res.Full {
		fmt.Fprintln(w, "quick check: the security checks once, a boot check per size (--full for the whole suite)")
	}
	if res.ReclaimIdleS > 0 {
		fmt.Fprintf(w, "boxes end when you touch the keyboard or pointer, and start after %ds idle (--reclaim-idle-s 0: always lent)\n", res.ReclaimIdleS)
	}
	printAdmission(w, res)
	if res.RunnerDir != "" {
		net := ""
		if res.Network != "" && res.Network != "none" {
			net = ", network " + res.Network
		}
		fmt.Fprintf(w, "GitHub Actions: each box serves a runner (runner_dir %s%s)\n", res.RunnerDir, net)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, p := range res.Pulls {
		fmt.Fprintf(w, "%s: %s\n", verb, strings.Join(p.Command, " "))
	}
	for _, s := range res.Sizes {
		fmt.Fprintf(w, "%s: %s\n", verb, strings.Join(s.Conformance, " "))
		switch {
		case res.DryRun:
			fmt.Fprintf(w, "  would bind %s (%d vCPU, %d MB) if it passes\n", s.Binding, s.Size.CPU, s.Size.MemoryMB)
		case s.Bound:
			fmt.Fprintf(w, "  bound %s\n", s.Binding)
		default:
			fmt.Fprintf(w, "  %s\n", s.Error)
		}
	}
}

// shareProject is the project a share binds into, said before minutes of
// conformance ("binding into acme (from $ZM_PROJECT)"), so a $ZM_PROJECT left
// in the shell (a farm box's, say) is never read as a built-in default.
// With none it refuses, naming the rule that failed and
// the next step; a joined project's boxes go to its pool (--pool). When the
// daemon cannot answer, share goes on: its option checks come first and the
// bind itself still refuses without a project.
func shareProject(ctx context.Context, c *client.Client, w io.Writer) error {
	res, err := c.Resolve(ctx)
	if err != nil {
		return nil
	}
	hint := "run zm host share in a project's directory (zm init makes one), pass -P <project>, or --pool <pool> to lend machine-level"
	if p := res.Project; p != nil {
		if res.Rule == "team" { // a joined project: its pool lends, not a project binding
			return joinedShare(p.Slug)
		}
		fmt.Fprintf(w, "zm: binding into %s (from %s)\n", p.Slug, ruleSource(res.Rule)) // a binding has no area
		return nil
	}
	if n := len(res.Steps); n > 0 {
		st := res.Steps[n-1]
		if st.Rule == project.RuleFlag || st.Rule == project.RuleEnv {
			// The resolver knows this machine's projects only; a joined slug
			// answers through the team proxy, and its boxes go to its pool.
			slug, _, _ := strings.Cut(st.Input, "/")
			if strings.HasPrefix(st.Result, "no project ") {
				if _, err := c.GetProject(ctx, slug); err == nil {
					return joinedShare(slug)
				}
			}
		}
		switch st.Rule {
		case project.RuleEnv:
			return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("%s: %q comes from $ZM_PROJECT in your environment", st.Result, st.Input),
				Hint: "unset ZM_PROJECT, or set it to a project of this machine (zm project ls); or " + hint}
		case project.RuleFlag, project.RuleCwdFile, project.RuleDefaultProject:
			if st.Input == "" { // default_project: not set
				break
			}
			return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("%s (from %s %s)", st.Result, ruleSource(st.Rule), st.Input), Hint: "zm project why explains; or " + hint}
		}
	}
	return &api.Error{Code: api.CodeBadRequest, Message: "no project to bind the boxes into", Hint: hint}
}

// joinedShare refuses a project binding in a joined project, as the bind
// itself would: boxes are lent to its pool, machine-level.
func joinedShare(slug string) error {
	return &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("%s is a joined project: lend to its pool instead", slug), Hint: "zm host share --pool " + slug}
}

// ruleSource names a resolution rule as a person reads it.
func ruleSource(rule string) string {
	switch rule {
	case project.RuleFlag:
		return "-P"
	case project.RuleEnv:
		return "$ZM_PROJECT"
	case project.RuleCwdFile:
		return ".zm/project.toml"
	case project.RuleCwdPath, project.RuleCwdRemote:
		return "this directory"
	case project.RuleSession:
		return "this session"
	case project.RuleDefaultProject:
		return "config default_project"
	}
	return rule
}
