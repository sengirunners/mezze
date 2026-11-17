package main

// `zm job run`: submit a script at a commit to a box and
// wait for it, outside GitHub. A thin wrapper over boxes, no new kind: place
// and acquire a box (as zm sandbox create), push this checkout's commit
// (potluck/boxpush), spawn the script, follow its output, answer its
// exit code, release the box. Owned by sandboxes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/placement"
)

// JobFile is the script a job runs when no command is given, at the
// shipped commit: it lives with the code, so a commit always means one job.
const JobFile = ".zm/job"

// Job timing: the lease planned for a job covers the push and the script
// with room to spare (--timeout plus jobLeaseSlack, within the target's
// cap), but the box is only asked for jobLeaseTTL of it and the client
// renews that every jobRenewEvery while it lives: a client that dies (kill
// -9, a lost laptop, a dropped authority) leaves a box that ends within
// jobLeaseTTL, not at the planned end, so it never holds its host's slot for
// the length of a --timeout. A script the driver did not end by its timeout
// is killed jobKillGrace later; output calls failing in a row for
// jobLostAfter mean the box is gone; the release at the end is retried for
// up to jobReleaseFor (it still ends at its lease when none got through).
var (
	jobLeaseSlack = 10 * time.Minute
	jobLeaseTTL   = 5 * time.Minute
	jobRenewEvery = time.Minute
	jobReleaseFor = 3 * time.Minute
	jobKillGrace  = 90 * time.Second
	jobLostAfter  = 2 * time.Minute
)

func init() {
	j := &cobra.Command{Use: "job", Short: "Run a script at a commit on a pool machine and wait for its result"}
	j.AddCommand(jobRunCmd())
	rootCmd.AddCommand(j)
}

type jobOptions struct {
	want    computeclient.BoxWant
	image   string
	repo    string // the checkout ("" = the working directory)
	ref     string
	push    pushFlags
	argv    []string
	timeout time.Duration
	keep    bool
	net     string // --net, as given
	df      depscli.Flags
	// deps are the dependency layers the box is acquired with, filled
	// from df.
	deps      []computeapi.BoxDeps
	depsWaitS *int
	// under is the project's directory in the checkout ("" its top) when a
	// layer is mounted there by default: the command runs there, beside it.
	under string
	out   io.Writer // the script's stdout
	errw  io.Writer // its stderr and zm's own lines
}

// jobResult is --json's answer. State: exited (Exit is the script's),
// timeout (killed at --timeout, exit 124), lost (it ended with no exit code:
// its box or holder went; never a pass), interrupted (Ctrl-C), failed (zm
// could not start it; Error says why).
type jobResult struct {
	Handle string `json:"handle,omitempty"`
	Target string `json:"target,omitempty"`
	Commit string `json:"commit,omitempty"`
	PID    string `json:"pid,omitempty"`
	State  string `json:"state"`
	Exit   *int   `json:"exit,omitempty"`
	Error  string `json:"error,omitempty"`
	// Deps are the acquire's answer for each layer asked for (--deps).
	Deps     []computeapi.BoxDepsAnswer `json:"deps,omitempty"`
	Kept     bool                       `json:"kept"`
	Released bool                       `json:"released"`
	Seconds  float64                    `json:"seconds"`
}

// code is the command's exit code for r.
func (r jobResult) code() int {
	switch {
	case r.State == "exited" && r.Exit != nil:
		return *r.Exit
	case r.State == "timeout":
		return 124
	case r.State == "interrupted":
		return 130
	}
	return 1
}

func jobRunCmd() *cobra.Command {
	o := jobOptions{out: os.Stdout, errw: os.Stderr}
	cmd := &cobra.Command{Use: "run [--on <target> | --size S --os O] [--ref HEAD] [--dirty] [--timeout 30m] [--keep] [-- <argv...>]",
		Short: "Run a command (default: the commit's .zm/job) at this checkout's commit in a fresh box; exits with its code",
		Long: `Places a box as zm sandbox create does, sends the commit (default HEAD; with
--dirty your uncommitted changes too) as a git bundle, starts the command in
the background in the checkout, streams its output as it comes and exits
with its exit code. The box is released at the end, also on Ctrl-C, an
error or the timeout (exit 124), unless --keep. A command that ends with no
exit code (its box went) fails. No command: the commit's .zm/job.

--deps npm[:path] runs it beside a dependency layer of yours (node_modules
built once from this directory's package-lock.json and package.json files,
then reused; zm sandbox create --help says more), mounted copy-on-write at
the checkout plus this directory's place in its repository, where the push
puts the code; the command (the commit's .zm/job too) runs there. On a
binding another machine serves, name a --template.`,
		Example: `  zm job run -- make test
  zm job run --deps npm --template agent -- npm test
  zm job run --os macos --timeout 45m --dirty
  zm job run --json -- go test ./... 2>log`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			o.argv = args
			if o.want.Net, err = netFlag(o.net); err != nil {
				return err
			}
			var dreq computeapi.AcquireRunTargetRequest
			if err := o.df.Request(cmd.Context(), cmd.Flags(), o.repo, o.push.dir, &dreq); err != nil {
				return err
			}
			o.deps, o.depsWaitS = dreq.Deps, dreq.DepsWaitS
			o.under = depsUnder(cmd.Context(), o.repo, o.push.dir, o.deps)
			if len(o.deps) > 0 {
				depscli.WarnMismatch(cmd.Context(), os.Stderr, "zm", o.repo, o.ref, o.push.dirty, o.push.untracked, o.deps)
			}
			if flagJSON {
				o.out = os.Stderr // stdout carries only the result
			}
			r, err := jobRun(cmd.Context(), c, o)
			if flagJSON {
				if err != nil && r.Error == "" {
					r.Error = err.Error()
					if e := (*api.Error)(nil); errors.As(err, &e) {
						r.Error = mcp.ZmCommands(e).Error() // a limit refusal names zm's command
					}
				}
				if perr := printJSON(r); perr != nil {
					return perr
				}
				if r.code() == 0 {
					return nil
				}
				return exitSilently(r.code())
			}
			if err != nil {
				return exitWith{code: r.code(), err: err}
			}
			if r.code() != 0 {
				return exitSilently(r.code())
			}
			return nil
		}}
	f := cmd.Flags()
	f.StringVar(&o.want.On, "on", "", "this box run target, as named (default: placement)")
	f.StringVar(&o.want.Size, "size", "", "a size the target states")
	f.StringVar(&o.want.OS, "os", "", "linux or macos")
	f.StringVar(&o.image, "image", "", "one of the target's images, by name")
	f.StringVar(&o.want.Template, "template", "", "one of zm's box templates: "+strings.Join(computeapi.BoxTemplates(), ", ")+" (default: the target's first image)")
	f.StringVar(&o.ref, "ref", "", "the commit to run, in this checkout (default HEAD)")
	f.DurationVar(&o.timeout, "timeout", 30*time.Minute, "kill the command after this long (exit 124)")
	f.BoolVar(&o.keep, "keep", false, "leave the box up afterwards, for zm sandbox exec (it ends at its lease)")
	f.StringVar(&o.net, "net", "on", netUsage)
	o.push.add(f)
	o.df.Add(f)
	return cmd
}

// jobRun runs one job. The result is filled as far as it got; err is set
// when zm itself failed (state failed, or the box went: lost).
func jobRun(ctx context.Context, c *client.Client, o jobOptions) (r jobResult, err error) {
	start := time.Now()
	defer func() { r.Seconds = time.Since(start).Round(time.Millisecond).Seconds() }()
	r.State = "failed"
	if o.timeout < time.Second {
		return r, fmt.Errorf("--timeout must be at least 1s")
	}
	if o.push.maxMiB <= 0 {
		o.push.maxMiB = boxpush.DefaultMax >> 20
	}
	o.push.repo = o.repo
	b, err := o.push.bundle(ctx, o.ref, o.errw)
	if err != nil {
		return r.stopped(ctx, err)
	}
	defer b.Close()
	r.Commit = b.Head
	argv := o.argv
	if len(argv) == 0 {
		if argv, err = jobFileArgv(ctx, b, o.under); err != nil {
			return r.stopped(ctx, err)
		}
	}
	if len(o.deps) > 0 {
		o.want.Volumes = true // a layer mounts as a volume does
	}
	key := "job-" + strings.ToLower(ids.New())
	areq := computeapi.AcquireRunTargetRequest{RequestID: key, Image: o.image, Template: o.want.Template, Net: o.want.Net,
		Deps: o.deps, DepsWaitS: o.depsWaitS}
	// Placement ranks the bindings that fit and AcquireBox asks them in
	// turn (Potluck #161, #177); each is asked for a lease that fits its own
	// cap, and one whose cap is too short passes to the next (a Misfit).
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	// planned is the lease the job is planned for (the end of the box's
	// life); it is asked for a short lease of jobLeaseTTL that this client
	// renews up to planned, when the binding's driver renews and the box is
	// not kept. leased says so.
	var planned time.Duration
	var leased, capped bool
	prep := func(ctx context.Context, target string, ar *computeapi.AcquireRunTargetRequest) error {
		prepErr = nil
		leased, capped = false, false
		ttl := o.timeout + jobLeaseSlack
		v, err := computeclient.DescribeRunTarget(ctx, c, target)
		if ctx.Err() != nil { // Ctrl-C before the acquire: nothing was asked
			prepErr = ctx.Err()
			return prepErr
		}
		if err == nil {
			lim := v.TTLMaxS
			if v.Describe != nil {
				lim = placement.LifeCap(lim, v.Describe.TTLMaxS)
			}
			if capd := time.Duration(lim) * time.Second; lim > 0 && ttl > capd {
				if o.timeout+2*time.Minute > capd {
					// Nothing asked of it: the next binding may fit.
					return &computeclient.Misfit{Err: fmt.Errorf("--timeout %v does not fit run target %s's lifetime cap of %v (with two minutes to send the code)", o.timeout, target, capd)}
				}
				ttl, capped = capd, true
			}
			leased = !o.keep && ttl > jobLeaseTTL && v.Describe != nil && slices.Contains(v.Describe.Verbs, "renew")
		}
		planned = ttl
		if leased {
			ttl = jobLeaseTTL
		}
		ar.TTLS = int(ttl / time.Second)
		// This client renews the short lease itself: say so, so the daemon's
		// sweep (which renews a handle whose holder session lives) does not
		// keep the box up after this process dies (Potluck #200).
		ar.SelfRenew = leased
		areq.TTLS, areq.SelfRenew = ar.TTLS, ar.SelfRenew // as this binding was asked: the interrupt below asks it again, the same
		return nil
	}
	acquiring := time.Now()
	target, h, err := computeclient.AcquireBox(ctx, c, o.want, areq, prep)
	r.Target = target
	if err != nil && (target == "" || err == prepErr || computeclient.MadeNothing(err)) { // nothing was made: nothing to release
		return r.stopped(ctx, err)
	}
	if err != nil && (ctx.Err() != nil || mayHaveMade(err)) {
		// Ctrl-C mid-acquire, or a failure after which the daemon kept the
		// request (Potluck #177): the box may come up all the same. The same
		// key answers that same box: end it.
		interrupted := ctx.Err() != nil
		what := err.Error()
		if interrupted {
			r.State = "interrupted"
			what = "interrupted while the box was being made"
		}
		first := err
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute+depsTimeout(o.deps, o.depsWaitS))
		defer cancel()
		if h, err = computeclient.AcquireRunTarget(actx, c, target, areq); err != nil {
			return r, fmt.Errorf("%s; it may still come up (request id %s, it ends at its lease): %w", what, key, err)
		}
		r.Handle = h.Handle
		_, id, _ := computeclient.SplitBoxHandle(h.Handle)
		if err := releaseBox(ctx, c, target, id); err != nil {
			return r, fmt.Errorf("%s; could not release %s (it ends at its lease): %w", what, h.Handle, err)
		}
		r.Released = true
		if interrupted {
			return r, errInterrupted
		}
		return r, fmt.Errorf("%w (the box it made is released)", first)
	}
	if err != nil {
		return r, err
	}
	r.Handle = h.Handle
	r.Deps = h.Deps
	depscli.Print(o.errw, "zm", o.deps, h.Deps)
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	stopRenew := func() {}
	if leased {
		// Until the end of the planned lease, at most.
		end := time.Now().Add(planned) // the box's life counts from when it was made, not from the ask
		if capped {
			end = acquiring.Add(planned) // the cap counts from the acquire, whenever the box came up
		}
		stopRenew = keepLease(ctx, c, target, id, end, o.errw)
	}
	defer func() {
		stopRenew()
		if o.keep {
			r.Kept = true
			fmt.Fprintf(o.errw, "zm: kept %s (zm sandbox exec %s -- ...; zm sandbox rm %s ends it)\n", h.Handle, h.Handle, h.Handle)
			return
		}
		// Released whatever happened, Ctrl-C included: on a context of its own.
		if rerr := releaseBox(ctx, c, target, id); rerr != nil {
			fmt.Fprintf(o.errw, "zm: could not release %s (it ends at its lease, within %v): %v\n", h.Handle, jobLeaseTTL, rerr)
			return
		}
		r.Released = true
	}()
	fmt.Fprintf(o.errw, "zm: %s on %s at %s\n", h.Handle, target, shortSHA(b.Head))
	pushed, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{Dest: o.push.dir})
	if err != nil {
		return r.stopped(ctx, err)
	}
	// The checkout's path as the box's shell has it ($HOME/<dir>): a guest
	// path in the driver's terms may differ (the reference driver roots them).
	cwd := pushed.Dir
	if o.under != "" {
		cwd = strings.TrimSuffix(cwd, "/") + "/" + o.under
	}
	sp, err := computeclient.SpawnRunTarget(ctx, c, target, id, computeapi.SpawnRunTargetRequest{Argv: append([]string{"sh", "-c", jobCd, "zm-job", cwd}, argv...),
		TimeoutS: int((o.timeout + time.Second - 1) / time.Second), RequestID: key})
	if err != nil {
		return r.stopped(ctx, err)
	}
	r.PID = sp.PID
	return follow(ctx, c, target, id, o, time.Now().Add(o.timeout+jobKillGrace), r)
}

// releaseBox releases the box on a context of its own (Ctrl-C and a
// failed run included), asking again for up to jobReleaseFor when the
// authority does not answer: a blip of the network must not leave a box up.
func releaseBox(ctx context.Context, c *client.Client, target, id string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobReleaseFor)
	defer cancel()
	for wait := time.Second; ; wait = min(wait*2, 15*time.Second) {
		actx, acancel := context.WithTimeout(rctx, 60*time.Second)
		_, err := computeclient.ReleaseRunTarget(actx, c, target, id)
		acancel()
		if err == nil || !askAgain(err) {
			return err
		}
		select {
		case <-rctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// askAgain says whether a lease call that failed is sent again: no answer, a
// dropped relay or a call that outlasted its own time limit (the daemon's
// wait on a slow driver is longer than a client's guess), not a refusal.
func askAgain(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || boxpush.Transient(err)
}

// keepLease renews the box's short lease every jobRenewEvery until stopped
// (the returned func, which waits for the last renewal), by jobLeaseTTL from
// now and never past end. A renewal that fails is tried again at the next
// tick: the lease outlasts a few missed ones, and when none gets through the
// box ends at the lease, which is the point.
func keepLease(ctx context.Context, c *client.Client, target, id string, end time.Time, errw io.Writer) (stop func()) {
	kctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(jobRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-kctx.Done():
				return
			case <-t.C:
			}
			ttl := min(jobLeaseTTL, time.Until(end))
			if ttl < time.Second {
				return // the planned end: the lease runs out there, as it always did
			}
			rctx, rcancel := context.WithTimeout(kctx, 60*time.Second) // the daemon allows a driver's renew up to 50 s
			_, err := computeclient.RenewRunTarget(rctx, c, target, id, int(ttl/time.Second))
			rcancel()
			if err != nil && kctx.Err() == nil && !askAgain(err) {
				fmt.Fprintf(errw, "zm: could not renew the box's lease (it ends within %v): %v\n", jobLeaseTTL, err)
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}

// jobCd runs "$@" in the checkout ($1, absolute or under HOME).
const jobCd = `case "$1" in /*) d="$1" ;; *) d="$HOME/$1" ;; esac
cd "$d" || exit 127
shift
exec "$@"`

var errInterrupted = errors.New("interrupted before the command started")

// stopped is r for an error before the script ran: on Ctrl-C it is
// interrupted (exit 130), whatever the cut-off step said.
func (r jobResult) stopped(ctx context.Context, err error) (jobResult, error) {
	if ctx.Err() != nil {
		r.State = "interrupted"
		return r, errInterrupted
	}
	return r, err
}

// follow pages the script's output until it ends, Ctrl-C, or deadline.
func follow(ctx context.Context, c *client.Client, target, id string, o jobOptions, deadline time.Time, r jobResult) (jobResult, error) {
	req := computeapi.OutputRunTargetRequest{PID: r.PID, WaitS: 20, MaxBytes: 256 << 10}
	signal := func(sig string) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_, _ = computeclient.SignalRunTarget(sctx, c, target, id, computeapi.SignalRunTargetRequest{PID: r.PID, Signal: sig})
	}
	var failing time.Time
	for {
		if ctx.Err() != nil {
			signal("TERM")
			r.State = "interrupted"
			return r, errors.New("interrupted: the command was stopped")
		}
		if time.Now().After(deadline) {
			signal("KILL")
			r.State = "timeout"
			return r, fmt.Errorf("the command outlived --timeout %v and was killed", o.timeout)
		}
		req.WaitS = max(min(20, int(time.Until(deadline)/time.Second)), 0)
		p, err := computeclient.RunTargetOutput(ctx, c, target, id, req)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			if failing.IsZero() {
				failing = time.Now()
			}
			if !boxpush.Transient(err) || time.Since(failing) > jobLostAfter {
				r.State = "lost"
				return r, fmt.Errorf("the command's output is gone (lost, never a pass): %w", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		failing = time.Time{}
		for _, d := range []struct {
			n    int64
			what string
		}{{p.DroppedStdout, "stdout"}, {p.DroppedStderr, "stderr"}} {
			if d.n > 0 {
				fmt.Fprintf(o.errw, "zm: %d bytes of %s are no longer kept\n", d.n, d.what)
			}
		}
		io.WriteString(o.out, p.Stdout)
		io.WriteString(o.errw, p.Stderr)
		req.StdoutFrom, req.StderrFrom = p.StdoutNext, p.StderrNext
		switch {
		case p.Running:
		case p.Exit == nil:
			r.State = "lost"
			return r, errors.New("the command ended with no exit code (lost: its box or holder went); not a pass")
		case *p.Exit == 124:
			r.State, r.Exit = "timeout", p.Exit
			return r, fmt.Errorf("the command ran past --timeout %v (exit 124)", o.timeout)
		default:
			r.State, r.Exit = "exited", p.Exit
			return r, nil
		}
	}
}

// jobFileArgv is the argv of the commit's JobFile: run directly when
// committed executable (its shebang decides), else with sh; from under (the
// command's directory below the checkout's top), by a path up to the top.
func jobFileArgv(ctx context.Context, b *boxpush.Bundle, under string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", b.Repo, "ls-tree", b.Head, "--", JobFile).Output()
	mode, _, _ := strings.Cut(string(out), " ")
	switch {
	case err != nil || mode == "":
		return nil, fmt.Errorf("no command given and no %s at %s: pass one after -- (zm job run -- make test) or commit a %s script", JobFile, shortSHA(b.Head), JobFile)
	}
	path := "./" + JobFile
	if under != "" {
		path = strings.Repeat("../", strings.Count(under, "/")+1) + JobFile
	}
	if mode == "100755" {
		return []string{path}, nil
	}
	return []string{"sh", path}, nil
}
