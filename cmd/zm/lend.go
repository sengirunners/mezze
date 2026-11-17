package main

// `zm lend` (stage 1): lend this Mac to its owner's own
// repos with no flags. A front over zm pool host: the personal pool "home" is
// made silently; the image, root, runner folder, floor and disk budget come
// from Potluck's pin table (hostprobe); an earlier lend's image and
// sizes are kept. It speaks plain words: none of pool, project, binding, box,
// digest, label, runner or installation. Owned by run targets.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// lendPool is the personal pool zm lend lends to: this machine is its
// authority, its broker and its host.
const lendPool = "home"

// lendBeat is the longest zm lend stays quiet: a lane watching its output
// must never take it for dead.
var lendBeat = 20 * time.Second

// lendSpawn starts the detached lend (tests replace it).
var lendSpawn = func(args []string, log *os.File) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, c.Process.Release()
}

// lendSettings are what a lend chose, kept so pause and resume (and a lend
// after them) lend the same again.
type lendSettings struct {
	With   string   `json:"with,omitempty"`
	Images []string `json:"images,omitempty"` // an --image lend's own images
	Sizes  []string `json:"sizes,omitempty"`
	Share  float64  `json:"share,omitempty"`
	When   string   `json:"when,omitempty"`
	IdleS  int      `json:"idle_s,omitempty"` // away: the idle seconds an earlier lend used
	// RunnerDir is an --image lend's runner folder (the pin table has none for it).
	RunnerDir string `json:"runner_dir,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
}

type lendFlags struct {
	s      shareFlags
	with   string
	when   string
	detach bool
}

func init() {
	cmd := lendCmd()
	cmd.AddCommand(lendStatusCmd(), lendSwitchCmd(), lendPauseCmd(), lendResumeCmd(), lendStopCmd())
	rootCmd.AddCommand(cmd)
}

func lendCmd() *cobra.Command {
	var l lendFlags
	cmd := &cobra.Command{
		Use:   "lend",
		Short: "Lend this Mac to your own GitHub repos' jobs: no flags needed",
		Long: `Lend checks this Mac, says what it will lend and how much disk that needs,
asks once for your password when macOS needs an admin step, downloads the
macOS image, checks that jobs are fenced in, and switches it on. Jobs run in
a fresh macOS VM that is deleted afterwards. Running it again keeps what an
earlier lend chose. zm lend status, switch, pause, resume and stop manage it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return plainErr(l.run(cmd, nil))
		},
	}
	l.add(cmd)
	return cmd
}

func (l *lendFlags) add(cmd *cobra.Command) {
	l.s.add(cmd, false)
	fl := cmd.Flags()
	fl.StringVar(&l.with, "with", "", "a named macOS image, such as xcode (default: what an earlier lend used, else plain macOS)")
	fl.StringVar(&l.when, "when", "", "any: jobs run any time; away: only while you are away from the keyboard (default: any)")
	fl.BoolVar(&l.detach, "detach", false, "after the checks and the admin step, go on in the background (zm lend status follows it)")
	keep := []string{"with", "when", "detach", "share", "sizes", "dry-run", "no-sudo", "no-auto-update"}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !slices.Contains(keep, f.Name) {
			f.Hidden = true
		}
	})
}

// run lends; saved, when set, are a resume's settings.
func (l *lendFlags) run(cmd *cobra.Command, saved *lendSettings) error {
	ctx := cmd.Context()
	if flagJSON {
		return errors.New("zm lend has no --json yet: zm host share --json gives share's result")
	}
	unlock, err := lendLock(l.s.f.dir())
	if err != nil {
		return err
	}
	defer unlock()
	if l.with != "" && len(l.s.o.Images) > 0 {
		return errors.New("--with and --image cannot be combined")
	}
	o, err := l.s.options(cmd)
	if err != nil {
		return err
	}
	home := l.s.f.dir()
	if saved == nil {
		saved = readLendSettings(home)
	}
	c, _, err := newClient()
	if err != nil {
		return err
	}
	fl := cmd.Flags()
	set := lendFlagsSet{with: fl.Changed("with"), share: fl.Changed("share"), sizes: fl.Changed("sizes"), when: fl.Changed("when"), reclaim: fl.Changed("reclaim-idle-s")}
	w, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if l.detach && !o.DryRun {
		return l.detachRun(ctx, c, o, set, saved, home, w, errw, unlock)
	}
	return lend(ctx, c, shareRunner, o, lendChoice{With: l.with, When: l.when, Set: set, Saved: saved}, home, w, l.s.root(ctx, errw), l.s.afterLend())
}

type lendFlagsSet struct{ with, share, sizes, when, reclaim bool }

// lendChoice is what the owner asked for (flags), what an earlier lend
// chose (Saved) and which flags were given.
type lendChoice struct {
	With, When string
	Set        lendFlagsSet
	Saved      *lendSettings
}

// lend is zm lend: the pool, the defaults, then zm pool host, in plain words.
func lend(ctx context.Context, c *client.Client, run hostprobe.Runner, o hostprobe.ShareOptions, ch lendChoice, home string, w io.Writer, root func(*hostprobe.ShareOptions) error, after func(io.Writer)) error {
	beat := newLendBeat(w)
	defer beat.stop()
	if !o.DryRun {
		if err := ensureLendPool(ctx, c); err != nil {
			return err
		}
	}
	st, err := lendDefaults(ctx, c, &o, ch)
	if err != nil {
		return err
	}
	beat.mu.Lock()
	err = sayLendPlan(w, o)
	beat.mu.Unlock()
	if err != nil {
		return err
	}
	o.Progress = beat.say
	if root != nil { // quiet while sudo asks for the password
		r := root
		root = func(o *hostprobe.ShareOptions) error { beat.hold(true); defer beat.hold(false); return r(o) }
	}
	if after != nil { // zm's daily update says what it did
		a := after
		after = func(io.Writer) { beat.mu.Lock(); defer beat.mu.Unlock(); a(w) }
	}
	res, err := poolHostResult(ctx, c, run, lendPool, o, io.Discard, root, after)
	beat.stop()
	sayLendResult(w, res)
	if err != nil {
		return err
	}
	if !o.DryRun {
		if lent(res) {
			if err := writeLendSettings(home, st); err != nil {
				fmt.Fprintf(w, "Note: could not save this lend's choices (%v); zm lend pause then cannot keep them.\n", err)
			}
		}
		fmt.Fprintln(w, "See it: zm lend status. Point a repo's jobs at it: zm ci status.")
	}
	fmt.Fprintln(w, "done")
	return nil
}

// ensureLendPool makes the personal pool when this machine has none.
func ensureLendPool(ctx context.Context, c *client.Client) error {
	rems, err := c.Remotes(ctx)
	if err != nil {
		return err
	}
	for _, r := range rems {
		if r.Project.Slug == lendPool {
			return fmt.Errorf("this Mac joined someone else's %q at %s, so zm lend cannot make its own here", lendPool, r.Authority)
		}
	}
	_, err = c.GetProject(ctx, lendPool)
	if api.IsCode(err, api.CodeNotFound) {
		return poolCreate(ctx, c, lendPool, poolInvites{Via: api.ViaTunnel}, io.Discard)
	}
	if err != nil {
		return err
	}
	return lendPoolIsOwn(ctx, c)
}

// lendPoolIsOwn refuses a "home" anyone but this machine's owner is in, or
// invited to: zm lend lends only to the owner's own repos, never quietly to
// a shared group (fails closed when it cannot tell).
func lendPoolIsOwn(ctx context.Context, c *client.Client) error {
	me, err := c.Whoami(ctx)
	if err != nil {
		return err
	}
	gs, err := c.Grants(ctx, lendPool)
	if err != nil {
		return fmt.Errorf("could not check who else is in %q: %w", lendPool, err)
	}
	var others []string
	for _, g := range gs {
		if g.Identity != me.Identity.Name && !slices.Contains(others, g.Identity) {
			others = append(others, g.Identity)
		}
	}
	invs, err := c.Invites(ctx)
	if err != nil {
		return fmt.Errorf("could not check who is invited to %q: %w", lendPool, err)
	}
	for _, in := range invs {
		if in.Project == lendPool && !slices.Contains(others, in.As) {
			others = append(others, in.As+" (invited)")
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("%q on this Mac is shared with %s, and zm lend lends only to your own repos", lendPool, strings.Join(others, ", "))
	}
	return nil
}

// lendDefaults fills o with what the owner did not choose: an earlier lend's
// image and sizes (its bindings, else its saved settings), else the pin
// table's default; jobs any time.
func lendDefaults(ctx context.Context, c *client.Client, o *hostprobe.ShareOptions, ch lendChoice) (lendSettings, error) {
	st := lendSettings{}
	if ch.Saved != nil {
		st = *ch.Saved
		st.Paused = false
	}
	var earlier []computeapi.RunTarget
	if c != nil {
		if l, err := computeclient.RunTargets(ctx, inPool(c, lendPool), false); err == nil {
			earlier = targetsOf(l)
		} else if !api.IsCode(err, api.CodeNotFound) {
			return st, fmt.Errorf("could not read what this Mac lends now: %w", err)
		}
	}
	var sizes []string
	for _, t := range earlier {
		size, ok := strings.CutPrefix(t.Name, o.NamePrefix+"-")
		if !ok || len(t.Images) == 0 {
			continue
		}
		sizes = append(sizes, size)
		keepEarlier(&st, t)
	}
	if ch.Saved != nil && ch.Saved.Paused { // a part-way pause left some lent: resume them all
		for _, n := range ch.Saved.Sizes {
			if !slices.Contains(sizes, n) {
				sizes = append(sizes, n)
			}
		}
	}
	if len(sizes) > 0 {
		slices.Sort(sizes)
		st.Sizes, st.Share = sizes, 0
	}
	if ch.Set.with {
		st.With, st.Images = ch.With, nil
	}
	switch {
	case len(o.Images) > 0:
		st.With, st.Images = "", o.Images
	case len(st.Images) > 0:
		o.Images = st.Images
		if o.RunnerDir == "" {
			o.RunnerDir = st.RunnerDir
		}
	default:
		o.With = st.With
	}
	if ch.Set.sizes || ch.Set.share {
		if !ch.Set.share { // the share those sizes need
			o.Limits.Share = shareFor(*o, o.Sizes)
		}
		st.Sizes, st.Share = o.Sizes, o.Limits.Share
	} else if len(st.Sizes) > 0 {
		o.Sizes = st.Sizes
		o.Limits.Share = st.Share
		if o.Limits.Share == 0 {
			o.Limits.Share = shareFor(*o, st.Sizes)
		}
		st.Share = o.Limits.Share
	}
	if ch.Set.when {
		st.When, st.IdleS = ch.When, 0
	}
	switch st.When {
	case "", "any":
		st.When = "any"
		if !ch.Set.reclaim {
			o.ReclaimIdleS = 0
		}
	case "away":
		if !ch.Set.reclaim {
			o.ReclaimIdleS = cmp.Or(st.IdleS, hostprobe.DefaultReclaimIdleS)
		}
	default:
		return st, fmt.Errorf("--when %q: want any or away", st.When)
	}
	return st, nil
}

// keepEarlier is the one place zm lend reads an earlier binding's choice:
// its image (hostprobe.KeepEarlierImage, the rule share's Check enforces),
// and whether it lent only while away. A binding with no image keeps the
// current image choice (KeepEarlierImage's ok is false).
func keepEarlier(st *lendSettings, t computeapi.RunTarget) {
	if with, images, dir, ok := hostprobe.KeepEarlierImage(t); ok {
		st.With, st.Images, st.RunnerDir = with, images, dir
	}
	st.When, st.IdleS = "any", 0
	if n, _ := strconv.Atoi(t.Config["reclaim_idle_s"]); n > 0 {
		st.When, st.IdleS = "away", n
	}
}

// shareFor is the smallest share above the default, in twentieths, at
// which hostprobe.Lend (share's own rule, with the owner's caps and the
// image's floor) lends every size: what an earlier lend of them must have
// used. 0 when the default lends them, or no share does (share then says
// which size does not fit).
func shareFor(o hostprobe.ShareOptions, sizes []string) float64 {
	images := o.Images
	if p, _, ok := hostprobe.SelectedPin(o); ok {
		images = []string{p.Image}
	}
	for i := 10; i <= 20; i++ {
		lim := o.Limits
		lim.Share = float64(i) / 20
		if i == 10 {
			lim.Share = 0 // zm's default
		}
		lent := hostprobe.Lend(o.Report, lim, images).Sizes
		if !slices.ContainsFunc(sizes, func(n string) bool {
			return !slices.ContainsFunc(lent, func(s hostprobe.Size) bool { return s.Name == n })
		}) {
			return lim.Share
		}
	}
	return 0
}

// imageWords names an image in plain words: "macOS with Xcode".
func imageWords(p hostprobe.DefaultPin) string {
	osw := map[string]string{"darwin": "macOS", "linux": "Linux"}[p.OS]
	if osw == "" {
		osw = "the " + p.OS + " image"
	}
	if p.Name == "" {
		return osw
	}
	r := []rune(p.Name)
	if p.Name == "xcode" {
		return osw + " with Xcode"
	}
	r[0] = unicode.ToUpper(r[0])
	return osw + " with " + string(r)
}

// sayLendPlan says what this machine has, what it will lend and the disk
// that needs, before anything runs.
func sayLendPlan(w io.Writer, o hostprobe.ShareOptions) error {
	r := o.Report
	free := ""
	if r.TartFreeGB > 0 {
		free = fmt.Sprintf(", %.0f GB free", r.TartFreeGB)
	} else if r.FreeDiskGB > 0 {
		free = fmt.Sprintf(", %.0f GB free", r.FreeDiskGB)
	}
	fmt.Fprintf(w, "This machine: %d cores, %d GB memory%s.\n", r.Cores, r.RAMMB>>10, free)
	p, layer, ok := hostprobe.SelectedPin(o)
	images := o.Images
	if ok {
		images = []string{p.Image}
	}
	if len(o.Sizes) == 0 {
		lending := hostprobe.Lend(r, o.Limits, images)
		if len(lending.Sizes) > 0 {
			big := lending.Sizes[len(lending.Sizes)-1]
			n := hostprobe.Slots
			if lending.OneAtATime {
				n = 1
			}
			fmt.Fprintf(w, "Will lend up to %d jobs at a time (up to %d cores and %d GB each), within %s.\n", n, big.CPU, big.MemoryMB>>10, o.Limits.Within())
		}
	} else {
		var parts []string
		for _, n := range o.Sizes {
			if s, err := hostprobe.SizeByName(n); err == nil {
				parts = append(parts, fmt.Sprintf("%d cores and %d GB", s.CPU, s.MemoryMB>>10))
			}
		}
		fmt.Fprintf(w, "Will lend up to %d jobs at a time (%s each), within %s.\n", hostprobe.Slots, strings.Join(parts, ", or "), o.Limits.Within())
	}
	if ok {
		need := ""
		if gb, _ := hostprobe.DiskNeedGB([]string{p.Image}); layer == "tart" && gb > 0 {
			need = fmt.Sprintf(": needs about %d GB of free disk for the download and the jobs", gb)
		}
		fmt.Fprintf(w, "Image: %s%s.\n", imageWords(p), need)
	}
	if o.ReclaimIdleS > 0 {
		fmt.Fprintf(w, "Jobs run only while you are away: touching the keyboard or pointer ends them, and they start after %d s idle.\n", o.ReclaimIdleS)
	} else {
		fmt.Fprintln(w, "Jobs may run any time (--when away: only while you are away).")
	}
	fmt.Fprintln(w, "Each job runs in a fresh VM that is deleted afterwards; it cannot reach your files or your home network.")
	return nil
}

// sayLendResult says what was lent, or why not, in plain words.
func sayLendResult(w io.Writer, res hostprobe.ShareResult) {
	for _, n := range res.Notes {
		fmt.Fprintf(w, "Note: %s\n", plain(n))
	}
	for _, s := range res.Sizes {
		size := fmt.Sprintf("%d cores and %d GB", s.Size.CPU, s.Size.MemoryMB>>10)
		switch {
		case res.DryRun:
			fmt.Fprintf(w, "Would check and lend %s.\n", size)
		case s.Bound:
			fmt.Fprintf(w, "Lending %s (size %s).\n", size, s.Size.Name)
		default:
			fmt.Fprintf(w, "Not lending %s: %s\n", size, plain(s.Error))
		}
	}
}

// lendBeat prints share's progress in plain words, and a line of its own
// whenever share has said nothing for lendBeat.
type lendBeatW struct {
	mu    sync.Mutex
	w     io.Writer
	last  string
	at    time.Time
	start time.Time
	held  bool
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
}

func newLendBeat(w io.Writer) *lendBeatW {
	b := &lendBeatW{w: w, last: "checking this machine", at: time.Now(), start: time.Now(), done: make(chan struct{})}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		t := time.NewTicker(lendBeat / 4)
		defer t.Stop()
		for {
			select {
			case <-b.done:
				return
			case now := <-t.C:
				b.mu.Lock()
				if !b.held && now.Sub(b.at) >= lendBeat-lendBeat/4 {
					fmt.Fprintf(b.w, "Still %s (%s so far).\n", b.last, now.Sub(b.start).Round(time.Second))
					b.at = now
				}
				b.mu.Unlock()
			}
		}
	}()
	return b
}

func (b *lendBeatW) say(s string) {
	line, step := progressWords(s)
	b.mu.Lock()
	defer b.mu.Unlock()
	if step != "" {
		b.last = step
	}
	if line != "" {
		fmt.Fprintln(b.w, line)
		b.at = time.Now()
	}
}

func (b *lendBeatW) stop() { b.once.Do(func() { close(b.done); b.wg.Wait() }) }

func (b *lendBeatW) hold(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held, b.at = on, time.Now()
}

var pullPct = regexp.MustCompile(`^pulling (\S+): ([\d.]+%)$`)

// progressWords is one of share's progress lines in plain words (line, ""
// to stay quiet) and the step it starts, for the heartbeat ("" keeps the
// last).
func progressWords(s string) (line, step string) {
	switch {
	case strings.HasPrefix(s, "no --image:"), strings.HasPrefix(s, "--with "), strings.HasPrefix(s, "and zm's "), strings.HasPrefix(s, "GitHub runners and sandboxes:"):
		return "", ""
	case strings.HasPrefix(s, "reuse "):
		return "The image is already downloaded.", "checking the image"
	case strings.HasPrefix(s, "pull "):
		return "Downloading the image (this can take an hour or more).", "downloading the image"
	case pullPct.MatchString(s):
		m := pullPct.FindStringSubmatch(s)
		return "Downloading: " + m[2], "downloading the image"
	case strings.HasPrefix(s, "conformance on "):
		size := s[strings.LastIndex(s, "-")+1:]
		return "Checking that jobs are fenced in (size " + size + ") ...", "checking that jobs are fenced in"
	case strings.HasPrefix(s, "bind "):
		return "Switching on size " + s[strings.LastIndex(s, "-")+1:] + ".", "switching it on"
	}
	return plain(s), ""
}

// The words zm lend never says, and what it says instead.
var lendWordFor = map[string]string{
	"box": "VM", "boxes": "VMs", "binding": "lent size", "bindings": "lent sizes",
	"runner": "job agent", "runners": "job agents", "pool": "group", "pools": "groups",
	"project": "workspace", "projects": "workspaces", "digest": "version", "digests": "versions",
	"label": "tag", "labels": "tags", "installation": "GitHub connection", "installations": "GitHub connections",
}

var (
	wordRe   = regexp.MustCompile(`[A-Za-z]+`)
	digestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}`)
)

// plain rewrites Potluck's words for zm lend: image references lose their
// digest, and each banned word outside a command, flag or path is replaced.
// A word right after "zm " or "potluck " is a command and stays.
func plain(s string) string {
	s = digestRe.ReplaceAllString(s, "")
	var b strings.Builder
	last := 0
	for _, m := range wordRe.FindAllStringIndex(s, -1) {
		word := s[m[0]:m[1]]
		to, ok := lendWordFor[strings.ToLower(word)]
		if !ok || m[0] > 0 && strings.ContainsRune("-_/.", rune(s[m[0]-1])) || m[1] < len(s) && (strings.ContainsRune("-_/", rune(s[m[1]])) || s[m[1]] == '.' && m[1]+1 < len(s) && unicode.IsLetter(rune(s[m[1]+1]))) ||
			strings.HasSuffix(s[:m[0]], "zm ") || strings.HasSuffix(s[:m[0]], "potluck ") {
			continue
		}
		if unicode.IsUpper(rune(word[0])) {
			to = strings.ToUpper(to[:1]) + to[1:]
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(to)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func plainErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.New(plain(err.Error()))
}

// detachRun does the admin step and every check share makes before a
// download here, then goes on in the background, its output to lend.log.
func (l *lendFlags) detachRun(ctx context.Context, c *client.Client, o hostprobe.ShareOptions, set lendFlagsSet, saved *lendSettings, home string, w, errw io.Writer, unlock func()) error {
	if err := ensureLendPool(ctx, c); err != nil {
		return err
	}
	if _, err := lendDefaults(ctx, c, &o, lendChoice{With: l.with, When: l.when, Set: set, Saved: saved}); err != nil {
		return err
	}
	if err := l.s.root(ctx, errw)(&o); err != nil {
		return err
	}
	dry := o
	dry.DryRun, dry.Progress = true, nil
	if l, err := computeclient.RunTargets(ctx, inPool(c, lendPool), false); err == nil {
		dry.Earlier = targetsOf(l)
	}
	if _, err := hostprobe.Check(ctx, shareRunner, dry); err != nil {
		return err
	}
	args := lendChildArgs(os.Args[1:])
	logPath := filepath.Join(home, "lend.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	unlock() // the background lend takes the lock itself
	pid, err := lendSpawn(args, log)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Lending in the background (process %d). Follow it: zm lend status, or tail -f %s\ndone\n", pid, logPath)
	return nil
}

// lendChildArgs are the detached lend's arguments: the same, without
// --detach, and never asking for a password (the parent did the admin step).
func lendChildArgs(args []string) []string {
	out := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--detach" || strings.HasPrefix(a, "--detach=") })
	if !slices.Contains(out, "--no-sudo") {
		out = append(out, "--no-sudo")
	}
	return out
}

func readLendSettings(home string) *lendSettings {
	b, err := os.ReadFile(filepath.Join(home, "lend.json"))
	if err != nil {
		return nil
	}
	var st lendSettings
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

func writeLendSettings(home string, st lendSettings) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(home, "lend.json.tmp") // written whole, synced, then renamed: never half a file
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, "lend.json"))
}

// lentHere is this machine's lent sizes in the personal pool.
func lentHere(ctx context.Context, c *client.Client, prefix string) ([]computeapi.RunTargetView, error) {
	l, err := computeclient.RunTargets(ctx, inPool(c, lendPool), true)
	if api.IsCode(err, api.CodeNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []computeapi.RunTargetView
	for _, v := range l.Targets {
		if v.RunTarget.Host == "" && strings.HasPrefix(v.Name, prefix+"-") {
			out = append(out, v)
		}
	}
	return out, nil
}

// lendCtx is what status, pause, resume and stop need.
func lendCtx(cmd *cobra.Command) (*client.Client, string, string, error) {
	var f hostFlags
	c, _, err := newClient()
	if err != nil {
		return nil, "", "", err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, "", "", err
	}
	return c, strings.ToLower(cfg.Machine), f.dir(), nil
}

func lendStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "What this Mac lends, what is running on it, its switch, and a lend still going on in the background", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, prefix, home, err := lendCtx(cmd)
			if err != nil {
				return plainErr(err)
			}
			if flagJSON {
				st, err := lendStatusResult(cmd.Context(), c, prefix, home)
				if err != nil {
					return plainErr(err)
				}
				return printJSON(st)
			}
			return plainErr(lendStatus(cmd.Context(), c, prefix, home, cmd.OutOrStdout()))
		}}
}

// LendStatus is zm lend status --json.
type LendStatus struct {
	Lent       []LendSize `json:"lent"` // [] when nothing is lent
	Paused     bool       `json:"paused"`
	Running    int        `json:"running"` // jobs running now
	Switch     LendSwitch `json:"switch"`
	Background string     `json:"background,omitempty"` // a lend going on in the background (running), or the last one's end (ended)
	Last       string     `json:"last,omitempty"`       // the background log's last line
}

// LendSize is one lent size.
type LendSize struct {
	Size     string `json:"size"`
	CPU      int    `json:"cpu,omitempty"`
	MemoryGB int    `json:"memory_gb,omitempty"`
	Image    string `json:"image,omitempty"` // in words
	Running  int    `json:"running"`
}

func lendStatusResult(ctx context.Context, c *client.Client, prefix, home string) (LendStatus, error) {
	vs, err := lentHere(ctx, c, prefix)
	if err != nil {
		return LendStatus{}, err
	}
	out := LendStatus{Lent: []LendSize{}}
	if st := readLendSettings(home); st != nil {
		out.Paused = st.Paused
	}
	for _, v := range vs {
		ls := LendSize{Size: strings.TrimPrefix(v.Name, prefix+"-")}
		if s, err := hostprobe.SizeByName(ls.Size); err == nil {
			ls.CPU, ls.MemoryGB = s.CPU, s.MemoryMB>>10
		}
		if len(v.Images) > 0 {
			if p, ok := hostprobe.PinForImage(v.Images[0]); ok {
				ls.Image = imageWords(p)
			}
		}
		hs, err := computeclient.RunTargetHandles(ctx, inPool(c, lendPool), v.Name, true)
		if err != nil {
			return LendStatus{}, err
		}
		ls.Running = len(hs)
		out.Running += ls.Running
		out.Lent = append(out.Lent, ls)
	}
	out.Switch = readLendSwitch(vs)
	logPath := filepath.Join(home, "lend.log")
	if lendBusy(home) {
		out.Background, out.Last = "running", lendLogLast(logPath)
	} else if _, err := os.Stat(logPath); err == nil {
		out.Background, out.Last = "ended", lendLogLast(logPath)
	}
	return out, nil
}

func lendStatus(ctx context.Context, c *client.Client, prefix, home string, w io.Writer) error {
	st, err := lendStatusResult(ctx, c, prefix, home)
	if err != nil {
		return err
	}
	switch {
	case len(st.Lent) == 0 && st.Paused:
		fmt.Fprintln(w, "Paused: this Mac lends nothing now. zm lend resume lends it again.")
	case len(st.Lent) == 0:
		fmt.Fprintln(w, "This Mac lends nothing now. zm lend lends it.")
	}
	for _, ls := range st.Lent {
		words := "size " + ls.Size
		if ls.CPU > 0 {
			words = fmt.Sprintf("%d cores and %d GB (size %s)", ls.CPU, ls.MemoryGB, ls.Size)
		}
		img := ""
		if ls.Image != "" {
			img = ", " + ls.Image
		}
		fmt.Fprintf(w, "Lending %s%s.\n", words, img)
	}
	if len(st.Lent) > 0 {
		fmt.Fprintf(w, "Running now: %d.\n", st.Running)
	}
	if len(st.Lent) > 0 || st.Switch.Mode != "auto" {
		fmt.Fprintln(w, st.Switch.Words)
	}
	switch st.Background {
	case "running":
		fmt.Fprintf(w, "A lend is still going on%s. Last in the background log: %s\n", lendPIDWords(home), st.Last)
	case "ended":
		fmt.Fprintf(w, "The last background lend ended with: %s\n", st.Last)
	}
	return nil
}

// errLendBusy: another zm lend holds the lock.
var errLendBusy = errors.New("another zm lend is going on")

// lendLock takes $ZM_HOME/lend.lock for this run (a lock, never a bare pid:
// it ends with the process) and records the pid beside it for status.
func lendLock(home string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(home, "lend.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			off := ""
			if pid, err := os.ReadFile(filepath.Join(home, "lend.pid")); err == nil {
				off = ", or call it off with kill " + strings.TrimSpace(string(pid))
			}
			return nil, fmt.Errorf("%w%s: zm lend status follows it; run this again once it ends%s", errLendBusy, lendPIDWords(home), off)
		}
		return nil, err
	}
	pidPath := filepath.Join(home, "lend.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	var once sync.Once
	return func() {
		once.Do(func() {
			if b, _ := os.ReadFile(pidPath); strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
				_ = os.Remove(pidPath)
			}
			f.Close()
		})
	}, nil
}

// lendBusy reports whether another lend holds the lock now. It only probes
// (a shared try-lock, no pid written), so a status never looks like a lend.
func lendBusy(home string) bool {
	f, err := os.Open(filepath.Join(home, "lend.lock"))
	if err != nil {
		return false // never locked: no lend has run here
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

func lendPIDWords(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "lend.pid"))
	if err != nil {
		return ""
	}
	return " (process " + strings.TrimSpace(string(b)) + ")"
}

func lendLogLast(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return plain(lines[len(lines)-1])
}

func lendPauseCmd() *cobra.Command {
	return &cobra.Command{Use: "pause", Short: "Stop lending for now, keeping what the lend chose (zm lend resume)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, prefix, home, err := lendCtx(cmd)
			if err != nil {
				return plainErr(err)
			}
			if flagJSON {
				res, err := lendPauseResult(cmd.Context(), c, prefix, home)
				if err != nil {
					return plainErr(err)
				}
				return printJSON(res)
			}
			return plainErr(lendOff(cmd.Context(), c, prefix, home, true, cmd.OutOrStdout()))
		}}
}

// LendPause is zm lend pause --json (and MCP compute_pause). A refusal
// (a job running, a lend going on, admission keys) is state refused with
// its reason, exit 0, never an error to read: nothing was changed.
type LendPause struct {
	State   string       `json:"state"` // paused or refused
	Sizes   []string     `json:"sizes"` // the sizes no longer lent ([] when none were, or refused)
	Refused *lendRefusal `json:"refused,omitempty"`
	Next    string       `json:"next,omitempty"` // the command that undoes or retries it
}

// lendRefusal is a pause or stop refused before anything changed.
type lendRefusal struct {
	Reason  string `json:"reason"`         // job_running, lend_running or admission
	Size    string `json:"size,omitempty"` // job_running: the size the job runs on
	Message string `json:"message"`
	text    string // the owner's words, when longer than Message
}

func (r *lendRefusal) Error() string {
	if r.text != "" {
		return r.text
	}
	return r.Message
}

// lendPauseResult runs lendOff's pause and answers it as LendPause.
func lendPauseResult(ctx context.Context, c *client.Client, prefix, home string) (LendPause, error) {
	vs, err := lendOffSizes(ctx, c, prefix, home, true, io.Discard)
	var ref *lendRefusal
	switch {
	case errors.As(err, &ref):
		next := "zm lend pause"
		if ref.Reason == "admission" {
			next = "zm lend stop"
		}
		return LendPause{State: "refused", Sizes: []string{}, Refused: ref, Next: next}, nil
	case err != nil:
		return LendPause{}, err
	}
	return LendPause{State: "paused", Sizes: append([]string{}, vs...), Next: "zm lend resume"}, nil
}

func lendStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop lending this Mac and forget what the lend chose", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, prefix, home, err := lendCtx(cmd)
			if err != nil {
				return plainErr(err)
			}
			return plainErr(lendOff(cmd.Context(), c, prefix, home, false, cmd.OutOrStdout()))
		}}
}

// lendOff stops lending: refused while a job runs (it would be cut off).
// pause keeps the settings for resume; stop forgets them and says how to
// delete the downloaded image.
func lendOff(ctx context.Context, c *client.Client, prefix, home string, pause bool, w io.Writer) error {
	_, err := lendOffSizes(ctx, c, prefix, home, pause, w)
	return err
}

// lendOffSizes is lendOff, also answering the sizes it took off.
func lendOffSizes(ctx context.Context, c *client.Client, prefix, home string, pause bool, w io.Writer) ([]string, error) {
	unlock, err := lendLock(home)
	if errors.Is(err, errLendBusy) {
		// an agent reads Message: no process id or kill hint (the owner's, in the text)
		msg := errLendBusy.Error() + ": zm lend status follows it; run this again once it ends"
		return nil, &lendRefusal{Reason: "lend_running", Message: msg, text: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	vs, err := lentHere(ctx, c, prefix)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		hs, err := computeclient.RunTargetHandles(ctx, inPool(c, lendPool), v.Name, true)
		if err != nil {
			return nil, err
		}
		if len(hs) > 0 {
			size := strings.TrimPrefix(v.Name, prefix+"-")
			return nil, &lendRefusal{Reason: "job_running", Size: size,
				Message: fmt.Sprintf("a job is running on size %s: wait for it to finish, then run this again", size)}
		}
		if pause && v.Config["admission_url"] != "" {
			return nil, &lendRefusal{Reason: "admission",
				Message: "this lend asks another program before each job, which pause cannot keep: zm lend stop, then lend again with the same flags"}
		}
	}
	st := readLendSettings(home)
	if st == nil {
		st = &lendSettings{}
	}
	var images, off []string
	if len(vs) > 0 && !(pause && st.Paused) { // what is lent now is the lend's choice
		st.Sizes, st.Share = nil, 0 // (after a part-way pause, the sizes it took off stay chosen)
	}
	for _, v := range vs {
		if len(v.Images) > 0 && !slices.Contains(images, v.Images[0]) {
			images = append(images, v.Images[0])
		}
		if size := strings.TrimPrefix(v.Name, prefix+"-"); !slices.Contains(st.Sizes, size) {
			st.Sizes = append(st.Sizes, size)
		}
		off = append(off, strings.TrimPrefix(v.Name, prefix+"-"))
		if len(v.Images) > 0 {
			keepEarlier(st, v.RunTarget)
		}
	}
	if pause { // saved before anything is undone, so a part-way pause still resumes
		st.Paused = true
		if err := writeLendSettings(home, *st); err != nil {
			return nil, err
		}
	}
	for i, v := range vs {
		if err := lendUnbind(ctx, inPool(c, lendPool), v.Name); err != nil {
			return nil, lendPartWay(off[:i], pause, err)
		}
	}
	if pause {
		fmt.Fprintln(w, "Paused: this Mac lends nothing now. zm lend resume lends it again.")
		fmt.Fprintln(w, "done")
		return off, nil
	}
	_ = os.Remove(filepath.Join(home, "lend.json"))
	fmt.Fprintln(w, "Stopped: this Mac lends nothing now.")
	for _, im := range images {
		gb := ""
		if p, ok := hostprobe.PinForImage(im); ok && p.MinFreeGB > 0 {
			gb = fmt.Sprintf(" (it holds up to %d GB)", p.MinFreeGB)
		}
		fmt.Fprintf(w, "The image stays downloaded%s; to delete it: tart delete %s\n", gb, im)
	}
	fmt.Fprintln(w, "done")
	return off, nil
}

// lendUnbind takes one lent size off (a test makes it fail part-way).
var lendUnbind = computeclient.UnbindRunTarget

// lendPartWay is an unbind's error once some sizes are off already: it
// says which, and that running the same command again finishes it.
func lendPartWay(off []string, pause bool, err error) error {
	if len(off) == 0 {
		return err
	}
	verb := "stop"
	if pause {
		verb = "pause"
	}
	words := "sizes " + strings.Join(off, ", ") + " are no longer lent, then: "
	if e := (*api.Error)(nil); errors.As(err, &e) {
		e2 := *e
		e2.Message = words + e.Message
		if e2.Hint == "" {
			e2.Hint = "zm lend " + verb + " again finishes it"
		}
		return &e2
	}
	return fmt.Errorf("%s%w; zm lend %s again finishes it", words, err, verb)
}

func lendResumeCmd() *cobra.Command {
	var l lendFlags
	cmd := &cobra.Command{Use: "resume", Short: "Lend this Mac again as it was before zm lend pause", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := readLendSettings(l.s.f.dir())
			if st == nil {
				return errors.New("nothing to resume: zm lend lends this Mac")
			}
			return plainErr(l.run(cmd, st))
		}}
	l.add(cmd)
	return cmd
}
