package main

// `zm sandbox`: throwaway VMs (SPEC 20 kind box).
// A thin layer over `zm target`: it picks the box binding and lists boxes
// across bindings. Owned by sandboxes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

func init() {
	sb := &cobra.Command{Use: "sandbox", Short: "Throwaway VMs you create, run commands in and stop",
		Long: `A sandbox is a box run target's handle: a VM kept up for its lease, which
only you (its holder) run commands in. It ends at release, at its deadline, or
when its machine's owner takes the machine back. Nothing long-lived enters it.
One created with --persist on a trusted host stops instead (zm sandbox stop,
or its lease running out) and keeps its disk for zm sandbox start.`}
	exec := boxOnly(targetExecCmd(), cobra.MinimumNArgs(2))
	exec.Use, exec.Example = "exec <box handle> -- <argv...>", `  zm sandbox exec box:linux/b3 -- sh -c 'uname -a'`
	renew := boxOnly(targetRenewCmd(), cobra.ExactArgs(1))
	renew.Use = "renew <box handle> --ttl 10m"
	rm := boxOnly(targetReleaseCmd(), cobra.ExactArgs(1))
	rm.Use, rm.Short = "rm <box handle>", "End a sandbox now"
	rm.Long = "Ends the sandbox; a stopped one's kept disk is deleted."
	sb.AddCommand(sandboxCreateCmd(), sandboxLsCmd(), exec, renew, rm, sandboxStopCmd(), sandboxStartCmd(), sandboxUsageCmd(),
		sandboxPreviewCmd(), sandboxSpawnCmd(), sandboxOutputCmd(), sandboxKillCmd(), sandboxPushCmd(), sandboxCpCmd(), sandboxShellCmd())
	rootCmd.AddCommand(sb)
}

// boxOnly narrows one of zm target's handle commands to a box handle given
// as one argument (box:<target>/<id>, the target a box binding), so a
// sandbox command never renews or ends a run or a database.
func boxOnly(cmd *cobra.Command, args cobra.PositionalArgs) *cobra.Command {
	run := cmd.RunE
	cmd.Args = args
	cmd.RunE = func(cmd *cobra.Command, a []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, _, err := computeclient.BoxHandle(cmd.Context(), c, a[0]); err != nil {
			return err
		}
		return run(cmd, a)
	}
	return cmd
}

func sandboxCreateCmd() *cobra.Command {
	var req computeapi.AcquireRunTargetRequest
	var want computeclient.BoxWant
	var ttl time.Duration
	var push pushFlags
	var netOpt string
	var vols []string
	var df depscli.Flags
	cmd := &cobra.Command{Use: "create [--size c2m4] [--os linux] [--on <target>] [--ttl 1h] [--ref HEAD [--dirty]] [--image <name> | --template agent|browser] [--net off] [--persist] [--volume name:path[:rw|ro|cow]]... [--deps npm[:path]]...",
		Short: "Start a sandbox: a fresh VM on the smallest available box run target that fits (or --on)",
		Long: `With --ref, the box holds a checkout of that commit. A box bound with a repo
checks out a pushed branch or tag itself (exec's default cwd). Any other box,
every box on another machine among them, gets the commit from this checkout:
zm sends it as a git bundle through exec and the box checks it out in
~/checkout (--dir); no credential enters the box.

--template agent boots zm's coding-agent image (git, gh, Node, Python, the
Claude Code and Codex CLIs). --template browser adds sandboxed Chromium,
Playwright and PNG screenshots. The host must lend the selected image.

On a trusted host a Linux (oci) box reaches the internet through the host's
filtered network (never the host, its LAN, cloud metadata or outbound mail);
--net off makes it with no network at all. A public host's box has none.

--persist keeps the box's disk when it stops (zm sandbox stop, or its lease
running out): zm sandbox start boots the same disk on the same host, for you
only, until the host's keep runs out (30 days by default; zm sandbox ls shows
when). zm sandbox rm deletes it. Trusted hosts only.

--volume name:path[:mode] mounts a named volume of yours at path (relative
to the box's HOME) that outlives the box: the next box that names it on
the same host sees what this one left. rw (the default) is one box's at a
time; ro (read-only) and cow (a copy of the box's own, dropped with it) are
shared by any number of your boxes. A volume is yours alone (another identity
naming the same volume gets its own); the host deletes one no box named for
30 days, and the least recently used when you pass its size cap. The answer
says which volumes are new (empty). Trusted hosts only; repeat for more.

--deps npm[:path] mounts a dependency layer of yours at path (default: the
checkout, --dir, plus this directory's place in its repository, where
--ref's push puts that project's files): node_modules built once from this
directory's package-lock.json and package.json files (and each workspace's
package.json) in a box of yours with the network and no secrets, then
reused by your later boxes on that host. zm prints whether it was a hit
(already built) or built. The box sees it copy-on-write: nothing it writes
reaches the layer or another box. Trusted hosts only; on a binding another
machine serves, name a template, not an image. --deps-wait S waits at most S
seconds while another box builds it (default 600; 0 takes the box at once
without it); --deps-scripts off skips dependencies' install scripts. The
files come from this directory, not from --ref.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl must not be negative")
			}
			req.TTLS = int(ttl / time.Second)
			if req.Net, err = netFlag(netOpt); err != nil {
				return err
			}
			want.Net = req.Net
			if req.Volumes, err = volumeFlags(vols); err != nil {
				return err
			}
			if err := df.Request(cmd.Context(), cmd.Flags(), push.repo, push.dir, &req); err != nil {
				return err
			}
			want.Volumes = len(req.Volumes) > 0 || len(req.Deps) > 0
			if req.RequestID == "" {
				req.RequestID = "cli-" + strings.ToLower(ids.New())
			}
			h, res, err := sandboxCreate(cmd.Context(), c, want, req, push, os.Stderr)
			switch {
			case err != nil:
				return err
			case flagJSON && res != nil:
				return printJSON(struct {
					computeapi.RunTargetHandle
					Push boxpush.Result `json:"push"`
				}{h, *res})
			case res != nil:
				return nil // printed before the push
			}
			return printHandle(h)
		}}
	f := cmd.Flags()
	f.StringVar(&want.On, "on", "", "this box run target, as named (default: placement)")
	f.StringVar(&want.Size, "size", "", "a size a box run target states (zm target ls --describe); zm's own: "+ladderNames()+" (c<vCPU>m<GiB>)")
	f.StringVar(&want.OS, "os", "", "linux or macos")
	f.DurationVar(&ttl, "ttl", 0, "lease length (default: the driver's)")
	f.StringVar(&req.Ref, "ref", "", "a commit to check out: a pushed branch or tag for a box with a repo, else a ref in this checkout (HEAD)")
	f.StringVar(&req.Image, "image", "", "one of the target's images, by name (default the first)")
	f.StringVar(&want.Template, "template", "", "one of zm's box templates: "+strings.Join(computeapi.BoxTemplates(), ", ")+" (default: the target's first image)")
	f.StringVar(&req.RequestID, "request-id", "", "idempotency key: the same key answers the same sandbox")
	f.StringVar(&netOpt, "net", "on", netUsage)
	f.BoolVar(&req.Persist, "persist", false, "keep the box's disk when it stops, for zm sandbox start (a trusted host's box target listing persist)")
	f.StringArrayVar(&vols, "volume", nil, "name:path[:rw|ro|cow]: a named volume of yours that outlives the box, mounted at path (repeatable)")
	push.add(f)
	df.Add(f)
	return cmd
}

// volumeFlags reads --volume name:path[:mode] (the daemon and the driver
// check the name and path).
func volumeFlags(vs []string) ([]computeapi.BoxVolume, error) {
	var out []computeapi.BoxVolume
	for _, v := range vs {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--volume %q: name:path[:rw|ro|cow]", v)
		}
		bv := computeapi.BoxVolume{Name: parts[0], Path: parts[1], Mode: computeapi.VolumeRW}
		if len(parts) == 3 {
			bv.Mode = parts[2]
		}
		switch bv.Mode {
		case computeapi.VolumeRW, computeapi.VolumeRO, computeapi.VolumeCOW:
		default:
			return nil, fmt.Errorf("--volume %q: the mode is rw, ro or cow", v)
		}
		out = append(out, bv)
	}
	return out, nil
}

const netUsage = "on: the host's filtered network on a trusted host (internet, never the host or its LAN); off: no network at all"

// netFlag is --net as acquire's net: on is the default, sent as nothing.
func netFlag(v string) (string, error) {
	switch v {
	case "", computeapi.NetOn:
		return "", nil
	case computeapi.NetOff:
		return computeapi.NetOff, nil
	}
	return "", fmt.Errorf("--net is on or off, not %q", v)
}

// sandboxCreate places and acquires a box; with a ref (or --dirty) on a
// box that does not check out refs itself, it pushes this checkout's commit
// into it, printing the handle first (not with --json).
func sandboxCreate(ctx context.Context, c *client.Client, want computeclient.BoxWant, req computeapi.AcquireRunTargetRequest, push pushFlags, w io.Writer) (computeapi.RunTargetHandle, *boxpush.Result, error) {
	var h computeapi.RunTargetHandle
	req.Template = want.Template
	var b *boxpush.Bundle
	defer func() {
		if b != nil {
			b.Close()
		}
	}()
	// Placement ranks the bindings that fit and AcquireBox asks them in
	// turn (Potluck #161): prep runs per binding, so whether this
	// checkout's commit is pushed in is decided by the one that answered.
	ref := req.Ref    // prep clears the ref of a binding that does not check it out
	pushing := false  // the binding asked checks out no refs: this checkout's commit is pushed in
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	prep := func(ctx context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		pushing, prepErr = false, nil
		if prepErr = depsImageOK(ctx, c, target, r.Image, r.Deps); prepErr != nil {
			return prepErr
		}
		if ref == "" && !push.dirty {
			return nil
		}
		self, err := boxpush.ChecksOutItself(ctx, c, target)
		switch {
		case err != nil:
			prepErr = err
		case self: // one that does not fit passes to the next binding (Potluck #177)
			if prepErr = boxpush.SelfMisfit(target, push.dirty, len(r.Deps) > 0); prepErr != nil {
				return prepErr
			}
		case b == nil: // the renter's bundle, made before the box so a refusal costs nothing
			b, prepErr = push.bundle(ctx, ref, w)
		}
		if prepErr == nil && !self {
			r.Ref, pushing = "", true
		}
		return prepErr
	}
	if len(req.Deps) > 0 && (ref != "" || push.dirty) {
		depscli.WarnMismatch(ctx, w, "zm", push.repo, ref, push.dirty, push.untracked, req.Deps)
	}
	target, h, err := computeclient.AcquireBox(ctx, c, want, req, prep)
	switch {
	case err != nil && (target == "" || err == prepErr || computeclient.MadeNothing(err)): // placement, or nothing made: a retry needs no key
		return h, nil, err
	case err != nil:
		return h, nil, retryWithKey(err, req.RequestID)
	}
	depscli.Print(w, "zm", req.Deps, h.Deps)
	if !pushing {
		return h, nil, nil
	}
	if !flagJSON {
		printHandle(h)
	}
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	res, err := push.push(ctx, c, target, id, b, w)
	if err != nil {
		return h, nil, fmt.Errorf("%w (the box is kept: zm sandbox push %s to resume, zm sandbox rm %s to end it)", err, h.Handle, h.Handle)
	}
	return h, &res, nil
}

// pushFlags are the flags of a push of this checkout's code into a box
// (potluck/boxpush).
type pushFlags struct {
	dirty, full bool
	untracked   bool // with dirty: untracked files too, each listed
	dir         string
	maxMiB      int
	repo        string // the checkout ("" = the working directory)
}

func (p *pushFlags) add(f *pflag.FlagSet) {
	f.BoolVar(&p.dirty, "dirty", false, "also send uncommitted changes to tracked files (a second commit on HEAD; implies --ref HEAD)")
	f.BoolVar(&p.untracked, "include-untracked", false, "with --dirty, also untracked files git does not ignore (each listed)")
	f.BoolVar(&p.full, "full", false, "send the whole history, not one commit")
	f.StringVar(&p.dir, "dir", boxpush.DefaultDest, "the checkout in the box (relative to its HOME; not HOME itself, nor in its .zm)")
	f.IntVar(&p.maxMiB, "max-mib", boxpush.DefaultMax>>20, "refuse a bundle over this many MiB")
}

// bundle makes the bundle of ref (default HEAD) in the checkout, listing
// on w every untracked file it carries.
func (p *pushFlags) bundle(ctx context.Context, ref string, w io.Writer) (*boxpush.Bundle, error) {
	switch {
	case p.maxMiB <= 0:
		return nil, fmt.Errorf("--max-mib must be positive")
	case p.untracked && !p.dirty:
		return nil, fmt.Errorf("--include-untracked goes with --dirty")
	}
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: p.repo, Ref: ref, Dirty: p.dirty, Untracked: p.untracked, Full: p.full, Max: int64(p.maxMiB) << 20})
	if err != nil {
		return nil, err
	}
	for _, f := range b.Untracked {
		fmt.Fprintf(w, "zm: sending untracked %s\n", f)
	}
	return b, nil
}

// push sends b into box target/id, with progress on stderr for a big one.
func (p *pushFlags) push(ctx context.Context, c *client.Client, target, id string, b *boxpush.Bundle, w io.Writer) (boxpush.Result, error) {
	o := boxpush.PushOptions{Dest: p.dir}
	if b.Size > 4*boxpush.ChunkSize {
		o.Progress = func(done, total int) {
			fmt.Fprintf(w, "\rzm: sending %s: %d/%d chunks", shortSHA(b.Head), done, total)
		}
	}
	res, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, o)
	if o.Progress != nil {
		fmt.Fprintln(w)
	}
	if err == nil {
		fmt.Fprintf(w, "zm: checked out %s at %s in the box (%.1f MiB sent; exec --cwd %s)\n", shortSHA(res.Commit), res.Dest, float64(b.Size)/(1<<20), res.Dest)
	}
	return res, err
}

func shortSHA(c string) string { return c[:min(len(c), 12)] }

func sandboxPushCmd() *cobra.Command {
	var push pushFlags
	return withFlags(&cobra.Command{Use: "push <box handle> [ref]", Short: "Check out a commit of this checkout in a box you hold (default HEAD), sent as a git bundle",
		Long: `Sends the commit (and with --dirty your uncommitted changes) from this
checkout into the box as a git bundle in chunks through exec, checked against
its SHA-256 in the box, and checks it out in ~/checkout (--dir). No credential
enters the box. A push cut off resumes when run again.`,
		Example: "  zm sandbox push box:linux@lender/b3 --dirty", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			b, err := push.bundle(cmd.Context(), append(args[1:], "")[0], os.Stderr)
			if err != nil {
				return err
			}
			defer b.Close()
			res, err := push.push(cmd.Context(), c, t, id, b, os.Stderr)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			return nil
		}}, push.add)
}

func withFlags(cmd *cobra.Command, add func(*pflag.FlagSet)) *cobra.Command {
	add(cmd.Flags())
	return cmd
}

func sandboxLsCmd() *cobra.Command {
	var live bool
	cmd := &cobra.Command{Use: "ls [--live]", Short: "Sandboxes you can see, across every box run target, newest first",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			hs, skipped, err := computeclient.Sandboxes(cmd.Context(), c, live)
			if err != nil {
				return err
			}
			for _, s := range skipped {
				fmt.Fprintln(os.Stderr, "zm: skipped", s)
			}
			if len(hs) == 0 && len(skipped) > 0 {
				return exitWith{code: 1, err: fmt.Errorf("no box run target answered")}
			}
			if flagJSON {
				return printJSON(computeapi.RunTargetHandles{Handles: hs})
			}
			for _, h := range hs {
				if err := printHandle(h); err != nil {
					return err
				}
			}
			return nil
		}}
	cmd.Flags().BoolVar(&live, "live", false, "only sandboxes not yet ended")
	return cmd
}

func sandboxUsageCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{Use: "usage [--since 2026-11-01T00:00:00Z]", Short: "Ended sandboxes' minutes, in the areas you read, newest first",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			l, err := computeclient.Usage(cmd.Context(), c, since)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(l)
			}
			for _, u := range l.Boxes {
				billed := ""
				if !u.Billed {
					billed = " (not billed)"
				}
				fmt.Printf("%s %s %s %dm%s ended=%s\n", u.Handle, u.EndState, u.Actor, u.BillableMinutes, billed, u.Ended)
			}
			more := ""
			if l.Truncated {
				more = " (more rows than listed)"
			}
			fmt.Printf("billed minutes: %d%s\n", l.BilledMinutes, more)
			return nil
		}}
	cmd.Flags().StringVar(&since, "since", "", "only boxes that ended at or after this RFC 3339 time")
	cmd.AddCommand(sandboxUsageLimitCmd())
	return cmd
}

// mayHaveMade: an acquire that failed so may have made a box all the
// same (no answer in time, or an internal driver failure after which the
// daemon keeps the request: Potluck #177).
func mayHaveMade(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && (e.Code == api.CodeUnavailable || e.Code == api.CodeInternal)
}

// retryWithKey names the request id of an acquire that mayHaveMade: it
// may have made something all the same, and a retry with
// that id answers it, never a second box or run.
func retryWithKey(err error, id string) error {
	var e *api.Error
	if mayHaveMade(err) && errors.As(err, &e) {
		return e.WithHint(strings.TrimSpace(e.Hint + " (to retry: --request-id " + id + ", which answers what this one made, never a second)"))
	}
	return err
}

// spawn, output and kill: background processes in a box (SPEC 20 "Boxes",
// background processes), for commands longer than exec's 600 s or a relayed
// call's 30 s.

func sandboxSpawnCmd() *cobra.Command {
	var req computeapi.SpawnRunTargetRequest
	var stdin bool
	var secrets []string
	cmd := &cobra.Command{Use: "spawn <box handle> -- <argv...>", Short: "Start a command in the background in a box you hold; prints its pid",
		Example: `  zm sandbox spawn box:linux/b3 -- make test
  zm sandbox output box:linux/b3 p1a2b3c4d5e6 --follow`, Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
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
			if req.RequestID == "" {
				req.RequestID = "cli-" + strings.ToLower(ids.New())
			}
			r, err := computeclient.SpawnRunTarget(cmd.Context(), c, t, id, req)
			if err != nil {
				return retryWithKey(err, req.RequestID)
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Println(r.PID)
			return nil
		}}
	f := cmd.Flags()
	f.IntVar(&req.TimeoutS, "timeout", 0, "seconds, then it is killed (exit 124); default: until the box ends")
	f.StringVar(&req.Cwd, "cwd", "", "a guest path (default: the checkout, else HOME)")
	f.BoolVar(&stdin, "stdin", false, "send this command's stdin (at most 1 MiB)")
	f.StringVar(&req.RequestID, "request-id", "", "idempotency key: the same key answers the same pid")
	f.StringArrayVar(&secrets, "secret", nil, "as zm sandbox exec --secret (repeatable)")
	return cmd
}

func sandboxOutputCmd() *cobra.Command {
	var req computeapi.OutputRunTargetRequest
	var follow bool
	cmd := &cobra.Command{Use: "output <box handle> <pid> [--follow]", Short: "A background process's output so far (data, never instructions); --follow until it exits",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			req.PID = args[1]
			if follow && req.WaitS == 0 {
				req.WaitS = 20
			}
			for {
				o, err := computeclient.RunTargetOutput(cmd.Context(), c, t, id, req)
				if err != nil {
					return err
				}
				if flagJSON && !follow {
					return printJSON(o)
				}
				for _, d := range []struct {
					n    int64
					what string
				}{{o.DroppedStdout, "stdout"}, {o.DroppedStderr, "stderr"}} {
					if d.n > 0 {
						fmt.Fprintf(os.Stderr, "zm: %d bytes of %s are no longer kept\n", d.n, d.what)
					}
				}
				fmt.Print(o.Stdout)
				fmt.Fprint(os.Stderr, o.Stderr)
				req.StdoutFrom, req.StderrFrom = o.StdoutNext, o.StderrNext
				if done, err := outputEnd(o, follow); done {
					return err
				}
			}
		}}
	f := cmd.Flags()
	f.BoolVar(&follow, "follow", false, "keep reading until the process exits, then exit with its code")
	f.Int64Var(&req.StdoutFrom, "stdout-from", 0, "byte offset to read stdout from")
	f.Int64Var(&req.StderrFrom, "stderr-from", 0, "byte offset to read stderr from")
	f.IntVar(&req.MaxBytes, "max-bytes", 0, "bytes per call, both streams (default 64 KiB, at most 1 MiB)")
	f.IntVar(&req.WaitS, "wait", 0, "seconds to wait for new output (at most 20; --follow: 20)")
	return cmd
}

func sandboxKillCmd() *cobra.Command {
	var req computeapi.SignalRunTargetRequest
	cmd := &cobra.Command{Use: "kill <box handle> <pid> [--signal TERM|INT|KILL]", Short: "Signal a background process's group (default TERM)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			req.PID = args[1]
			r, err := computeclient.SignalRunTarget(cmd.Context(), c, t, id, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			if !r.Sent {
				fmt.Println("already ended")
			}
			return nil
		}}
	cmd.Flags().StringVar(&req.Signal, "signal", "TERM", "TERM, INT or KILL")
	return cmd
}

// outputEnd says whether `zm sandbox output` is done after page o, and how
// it exits: the process's own code, an error for a process that ended with
// no exit (its holder died: never read as a pass), or, without --follow, 0
// while it runs.
func outputEnd(o computeapi.RunTargetOutput, follow bool) (bool, error) {
	switch {
	case !o.Running && o.Exit == nil:
		return true, exitWith{code: 1, err: fmt.Errorf("the process ended with no exit code (lost: its box or holder went)")}
	case !o.Running && *o.Exit != 0:
		return true, exitWith{code: *o.Exit, err: fmt.Errorf("exit %d", *o.Exit)}
	case !o.Running:
		return true, nil
	case !follow:
		fmt.Fprintf(os.Stderr, "zm: running; go on with --stdout-from %d --stderr-from %d\n", o.StdoutNext, o.StderrNext)
		return true, nil
	}
	return false, nil
}

// ladderNames is zm's box sizes (hostprobe.Ladder), for help text.
func ladderNames() string {
	var n []string
	for _, s := range hostprobe.Ladder {
		n = append(n, s.Name)
	}
	return strings.Join(n, ", ")
}

// stop and start: a persistent box's kept disk (SPEC 20 "Boxes", Kept disks).
func sandboxStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop <box handle>", Short: "Stop a sandbox created with --persist: nothing runs, its disk is kept for zm sandbox start",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, a[0])
			if err != nil {
				return err
			}
			h, err := computeclient.StopRunTarget(cmd.Context(), c, t, id)
			if err != nil {
				return err
			}
			return printHandle(h)
		}}
}

func sandboxStartCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{Use: "start <box handle> [--ttl 1h]", Short: "Start a stopped sandbox again: the same disk, on the same host, with a new lease",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl must not be negative")
			}
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, a[0])
			if err != nil {
				return err
			}
			h, err := computeclient.StartRunTarget(cmd.Context(), c, t, id, int(ttl/time.Second))
			if err != nil {
				return err
			}
			return printHandle(h)
		}}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "lease length (default: the driver's)")
	return cmd
}
