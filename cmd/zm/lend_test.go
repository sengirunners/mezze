package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// bannedLend are the words zm lend never prints.
var bannedLend = regexp.MustCompile(`(?i)\b(pools?|projects?|bindings?|box(es)?|digests?|labels?|runners?|installations?)\b`)

// a Mac like the one Stage 0 lent by hand: 18 cores, 48 GB.
func lendMac(freeGB float64) hostprobe.Report {
	return hostprobe.Report{OS: "darwin", Arch: "arm64", Cores: 18, RAMMB: 48 << 10, FreeDiskGB: freeGB, TartDir: "/fake/.tart", TartFreeGB: freeGB,
		Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true}}}
}

func lendRig(t *testing.T) (*server.Daemon, *client.Client, string) {
	old, oldAdvert := teamlink.BeatEvery, advertWait
	teamlink.BeatEvery, advertWait = time.Hour, 2*time.Second
	t.Cleanup(func() { teamlink.BeatEvery, advertWait = old, oldAdvert })
	dm, c := poolDaemon(t, tailnettest.New(), "mac", "ivo@example.com")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "box-any-image") // a Tart image name the ref driver does not boot
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return dm, c, home
}

type lendRun struct {
	mu  sync.Mutex
	ran []string
}

func (r *lendRun) runner(pullDelay time.Duration) hostprobe.Runner {
	return runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		r.mu.Lock()
		r.ran = append(r.ran, name+" "+strings.Join(args, " "))
		r.mu.Unlock()
		if name == "tart" && len(args) > 0 && args[0] == "pull" {
			time.Sleep(pullDelay)
		}
		if name == "tart" && len(args) > 0 && args[0] == "list" {
			return hostprobe.Result{Stdout: "[]"}, nil
		}
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
}

func (r *lendRun) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.ran, "\n")
}

func lendOpts(rep hostprobe.Report) hostprobe.ShareOptions {
	return hostprobe.ShareOptions{Report: rep, NamePrefix: "mac", Driver: "ref", ReclaimIdleS: hostprobe.DefaultReclaimIdleS}
}

func xcodePin(t *testing.T) imagepins.DefaultPin {
	p, ok := imagepins.NamedPin(hostprobe.DefaultPins(), "tart", "xcode")
	if !ok {
		t.Fatal("no xcode pin")
	}
	return p
}

// On a Mac lent by hand in Stage 0 (zm pool host home --image <Xcode pin>
// --share 0.7 --reclaim-idle-s 0, so c4m16), zm lend with no flags lends
// the same: the Xcode image by name, c4m16, the share it needs, any time;
// it says so in plain words, and status and stop do too.
func TestLendReproducesTheHandLend(t *testing.T) {
	_, c, home := lendRig(t)
	ctx := context.Background()
	xc := xcodePin(t)
	var r lendRun
	var out bytes.Buffer
	// The hand lend.
	hand := lendOpts(lendMac(250))
	hand.Images, hand.Sizes, hand.Limits.Share, hand.ReclaimIdleS = []string{xc.Image}, []string{"c4m16"}, 0.7, 0
	if err := poolCreate(ctx, c, lendPool, poolInvites{Via: "tunnel"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := poolHost(ctx, c, r.runner(0), lendPool, hand, &out, nil, nil); err != nil {
		t.Fatalf("hand lend: %v\n%s", err, out.String())
	}
	// zm lend, no flags.
	r = lendRun{}
	out.Reset()
	o := lendOpts(lendMac(250))
	if err := lend(ctx, c, r.runner(0), o, lendChoice{}, home, &out, nil, nil); err != nil {
		t.Fatalf("lend: %v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{"18 cores, 48 GB memory, 250 GB free", "4 cores and 16 GB", "within 70% of this machine", "macOS with Xcode", "needs about 200 GB", "any time", "Lending 4 cores and 16 GB (size c4m16)", "\ndone\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("lend does not say %q:\n%s", want, s)
		}
	}
	if m := bannedLend.FindAllString(strings.ReplaceAll(s, "zm ", ""), -1); len(m) > 0 {
		t.Errorf("lend says %v:\n%s", m, s)
	}
	ran := r.all()
	if !strings.Contains(ran, xc.Image) || strings.Contains(ran, "macos-tahoe-base") || !strings.Contains(ran, "root=on") {
		t.Errorf("lend ran:\n%s", ran)
	}
	vs, err := lentHere(ctx, c, "mac")
	if err != nil || len(vs) != 1 || vs[0].Name != "mac-c4m16" || vs[0].Images[0] != xc.Image {
		t.Fatalf("lent: %+v %v", vs, err)
	}
	if vs[0].Config["reclaim_idle_s"] != "" || vs[0].Config["runner_dir"] != "actions-runner" {
		t.Errorf("bound config: %v", vs[0].Config)
	}
	if st := readLendSettings(home); st == nil || st.With != "xcode" || st.Share != 0.7 || st.When != "any" {
		t.Errorf("settings: %+v", st)
	}

	out.Reset()
	if err := lendStatus(ctx, c, "mac", home, &out); err != nil || !strings.Contains(out.String(), "Lending 4 cores and 16 GB (size c4m16), macOS with Xcode.") || !strings.Contains(out.String(), "Running now: 0.") {
		t.Errorf("status: %v\n%s", err, out.String())
	}
	// A running job: status counts it, and pause and stop refuse to cut it off.
	// (on a second size of the ref driver's own image: the ref driver cannot boot the Xcode pin)
	const refImage = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	if _, err := computeclient.BindRunTarget(ctx, inPool(c, lendPool), computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{
		Name: "mac-c2m4", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refImage}, Config: map[string]string{"size": "c2m4"}}}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, inPool(c, lendPool), "mac-c2m4", computeapi.AcquireRunTargetRequest{RequestID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := lendStatus(ctx, c, "mac", home, &out); err != nil || !strings.Contains(out.String(), "Running now: 1.") {
		t.Errorf("status with a job: %v\n%s", err, out.String())
	}
	for _, pause := range []bool{true, false} {
		if err := lendOff(ctx, c, "mac", home, pause, &out); err == nil || !strings.Contains(err.Error(), "a job is running on size c2m4") {
			t.Fatalf("off (pause %v) with a job: %v", pause, err)
		}
	}
	// zm lend pause --json (and compute_pause): the refusal is data, nothing changed.
	if res, err := lendPauseResult(ctx, c, "mac", home); err != nil || res.State != "refused" || res.Refused == nil || res.Refused.Reason != "job_running" || res.Refused.Size != "c2m4" || res.Next != "zm lend pause" {
		t.Fatalf("pause --json with a job: %+v %v", res, err)
	}
	if vs, _ := lentHere(ctx, c, "mac"); len(vs) != 2 {
		t.Fatalf("a refused pause took sizes off: %+v", vs)
	}
	_, id, err := computeclient.SplitBoxHandle(h.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, inPool(c, lendPool), "mac-c2m4", id); err != nil {
		t.Fatal(err)
	}
	if err := computeclient.UnbindRunTarget(ctx, inPool(c, lendPool), "mac-c2m4"); err != nil {
		t.Fatal(err)
	}
	// Pause keeps the choice; resume lends the same with no flags.
	if res, err := lendPauseResult(ctx, c, "mac", home); err != nil || res.State != "paused" || strings.Join(res.Sizes, ",") != "c4m16" || res.Refused != nil || res.Next != "zm lend resume" {
		t.Fatalf("pause --json: %+v %v", res, err)
	}
	if vs, _ := lentHere(ctx, c, "mac"); len(vs) != 0 {
		t.Fatalf("paused but lent: %+v", vs)
	}
	st := readLendSettings(home)
	if st == nil || !st.Paused || st.With != "xcode" {
		t.Fatalf("paused settings: %+v", st)
	}
	r = lendRun{}
	if err := lend(ctx, c, r.runner(0), lendOpts(lendMac(250)), lendChoice{Saved: st}, home, &out, nil, nil); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if vs, _ := lentHere(ctx, c, "mac"); len(vs) != 1 || vs[0].Images[0] != xc.Image {
		t.Fatalf("resumed: %+v", vs)
	}
	out.Reset()
	if err := lendOff(ctx, c, "mac", home, false, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "tart delete "+xc.Image) || bannedLend.MatchString(strings.ReplaceAll(s, "zm ", "")) {
		t.Errorf("stop:\n%s", s)
	}
	if readLendSettings(home) != nil {
		t.Error("stop kept the settings")
	}
}

// A Mac with 60 GB free refuses the Xcode image before any download, in
// plain words.
func TestLendRefusesASmallDiskBeforeDownloading(t *testing.T) {
	_, c, home := lendRig(t)
	var r lendRun
	var out bytes.Buffer
	err := lend(context.Background(), c, r.runner(0), lendOpts(lendMac(60)), lendChoice{With: "xcode", Set: lendFlagsSet{with: true}}, home, &out, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not enough disk") {
		t.Fatalf("lend: %v\n%s", err, out.String())
	}
	if ran := r.all(); strings.Contains(ran, "pull") {
		t.Errorf("downloaded first:\n%s", ran)
	}
	if m := bannedLend.FindAllString(strings.ReplaceAll(plainErr(err).Error(), "zm ", ""), -1); len(m) > 0 {
		t.Errorf("refusal says %v: %v", m, plainErr(err))
	}
}

// A quiet download still prints a line every lendBeat.
func TestLendSaysSomethingWhileQuiet(t *testing.T) {
	old := lendBeat
	lendBeat = 40 * time.Millisecond
	t.Cleanup(func() { lendBeat = old })
	_, c, home := lendRig(t)
	var r lendRun
	var out syncBuf
	if err := lend(context.Background(), c, r.runner(300*time.Millisecond), lendOpts(lendMac(250)), lendChoice{}, home, &out, nil, nil); err != nil {
		t.Fatalf("lend: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Still downloading the image (") {
		t.Errorf("no heartbeat:\n%s", out.String())
	}
}

func TestShareFor(t *testing.T) {
	m := lendOpts(lendMac(250))
	for _, c := range []struct {
		sizes []string
		want  float64
	}{{[]string{"c4m16"}, 0.7}, {[]string{"c2m8"}, 0}, {[]string{"c2m8", "c4m16"}, 0.7}, {[]string{"nope"}, 0}} {
		if got := shareFor(m, c.sizes); got != c.want {
			t.Errorf("shareFor(%v) = %v, want %v", c.sizes, got, c.want)
		}
	}
}

func TestPlainWords(t *testing.T) {
	in := "Box c4m16: the pool's boxes and their runner_dir; run zm pool host or --runner-dir x; image ghcr.io/a/b@sha256:" + strings.Repeat("a", 64)
	want := "VM c4m16: the group's VMs and their runner_dir; run zm pool host or --runner-dir x; image ghcr.io/a/b"
	if got := plain(in); got != want {
		t.Errorf("plain:\n got %q\nwant %q", got, want)
	}
}

func TestLendChildArgs(t *testing.T) {
	got := strings.Join(lendChildArgs([]string{"lend", "--detach", "--with", "xcode"}), " ")
	if got != "lend --with xcode --no-sudo" {
		t.Errorf("child args: %q", got)
	}
}

// zm lend's help shows only its own few flags.
func TestLendHelpIsShort(t *testing.T) {
	var shown []string
	lendCmd().Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			shown = append(shown, f.Name)
		}
	})
	if strings.Join(shown, " ") != "detach dry-run no-auto-update no-sudo share sizes when with" {
		t.Errorf("shown flags: %v", shown)
	}
}

// An earlier lend only while away stays only while away.
func TestLendKeepsAnEarlierAway(t *testing.T) {
	_, c, home := lendRig(t)
	ctx := context.Background()
	var r lendRun
	var out bytes.Buffer
	if err := poolCreate(ctx, c, lendPool, poolInvites{Via: "tunnel"}, &out); err != nil {
		t.Fatal(err)
	}
	hand := lendOpts(lendMac(250))
	hand.Sizes = []string{"c2m4"}
	if err := poolHost(ctx, c, r.runner(0), lendPool, hand, &out, nil, nil); err != nil {
		t.Fatalf("hand lend: %v\n%s", err, out.String())
	}
	r, out = lendRun{}, bytes.Buffer{}
	if err := lend(ctx, c, r.runner(0), lendOpts(lendMac(250)), lendChoice{}, home, &out, nil, nil); err != nil {
		t.Fatalf("lend: %v\n%s", err, out.String())
	}
	vs, err := lentHere(ctx, c, "mac")
	if err != nil || len(vs) != 1 || vs[0].Config["reclaim_idle_s"] == "" || !strings.Contains(out.String(), "only while you are away") {
		t.Errorf("lend: %+v %v\n%s", vs, err, out.String())
	}
}

// A lend of an older version of the default image (the pin table moved on)
// is lent again as the default, as Potluck upgrades it, with its runner
// folder: never kept as an unknown image that serves no GitHub jobs.
func TestLendUpgradesAnOlderPin(t *testing.T) {
	_, c, home := lendRig(t)
	ctx := context.Background()
	def, err := hostprobe.DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	repo, _, _ := strings.Cut(def.Image, "@")
	var r lendRun
	var out bytes.Buffer
	if err := poolCreate(ctx, c, lendPool, poolInvites{Via: "tunnel"}, &out); err != nil {
		t.Fatal(err)
	}
	hand := lendOpts(lendMac(250))
	hand.Images, hand.Sizes, hand.ReclaimIdleS = []string{repo + "@sha256:" + strings.Repeat("b", 64)}, []string{"c2m4"}, 0
	if err := poolHost(ctx, c, r.runner(0), lendPool, hand, &out, nil, nil); err != nil {
		t.Fatalf("hand lend: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := lend(ctx, c, r.runner(0), lendOpts(lendMac(250)), lendChoice{}, home, &out, nil, nil); err != nil {
		t.Fatalf("lend: %v\n%s", err, out.String())
	}
	vs, err := lentHere(ctx, c, "mac")
	if err != nil || len(vs) != 1 || vs[0].Images[0] != def.Image || vs[0].Config["runner_dir"] == "" {
		t.Fatalf("lent: %+v %v\n%s", vs, err, out.String())
	}
}

// One lend, pause or stop at a time: the lock, not a pid, says so, and it
// ends with the run (a crashed lend leaves nothing to delete by hand).
func TestLendLock(t *testing.T) {
	home := t.TempDir()
	unlock, err := lendLock(home)
	if err != nil {
		t.Fatal(err)
	}
	if !lendBusy(home) {
		t.Error("held lock not seen")
	}
	if _, err := lendLock(home); err == nil || !strings.Contains(err.Error(), "another zm lend is going on (process "+strconv.Itoa(os.Getpid())+")") {
		t.Errorf("second lock: %v", err)
	}
	if err := lendOff(context.Background(), nil, "mac", home, true, io.Discard); err == nil || !strings.Contains(err.Error(), "another zm lend") {
		t.Errorf("pause while a lend runs: %v", err)
	}
	if res, err := lendPauseResult(context.Background(), nil, "mac", home); err != nil || res.State != "refused" || res.Refused.Reason != "lend_running" || strings.Contains(res.Refused.Message, "kill") {
		t.Errorf("pause --json while a lend runs: %+v %v", res, err)
	}
	unlock()
	if lendBusy(home) {
		t.Error("lock still held")
	}
	if _, err := os.Stat(filepath.Join(home, "lend.pid")); err == nil {
		t.Error("lend.pid left behind")
	}
	// A stale pid file of a dead or other process locks nothing out.
	_ = os.WriteFile(filepath.Join(home, "lend.pid"), []byte("1\n"), 0o600)
	if lendBusy(home) {
		t.Error("a stale pid file locks lend out")
	}
}

// An --image lend keeps its runner folder; a part-way pause resumes every size.
func TestLendKeepsOwnImageAndPausedSizes(t *testing.T) {
	_, c, home := lendRig(t)
	ctx := context.Background()
	const own = "example.com/own/mac@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	var r lendRun
	var out bytes.Buffer
	if err := poolCreate(ctx, c, lendPool, poolInvites{Via: "tunnel"}, &out); err != nil {
		t.Fatal(err)
	}
	hand := lendOpts(lendMac(250))
	hand.Images, hand.Sizes, hand.RunnerDir, hand.ReclaimIdleS = []string{own}, []string{"c2m8"}, "actions-runner", 0
	if err := poolHost(ctx, c, r.runner(0), lendPool, hand, &out, nil, nil); err != nil {
		t.Fatalf("hand lend: %v\n%s", err, out.String())
	}
	saved := &lendSettings{Images: []string{own}, RunnerDir: "actions-runner", Sizes: []string{"c2m4", "c2m8"}, When: "any", Paused: true}
	out.Reset()
	if err := lend(ctx, c, r.runner(0), lendOpts(lendMac(250)), lendChoice{Saved: saved}, home, &out, nil, nil); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	vs, err := lentHere(ctx, c, "mac")
	if err != nil || len(vs) != 2 {
		t.Fatalf("resumed: %+v %v\n%s", vs, err, out.String())
	}
	for _, v := range vs {
		if v.Images[0] != own || v.Config["runner_dir"] != "actions-runner" {
			t.Errorf("%s: %v %v", v.Name, v.Images, v.Config)
		}
	}
}

// A "home" shared with anyone else is never lent to quietly.
func TestLendRefusesASharedHome(t *testing.T) {
	_, c, home := lendRig(t)
	ctx := context.Background()
	if err := poolCreate(ctx, c, lendPool, poolInvites{Via: "tunnel"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Grant(ctx, api.GrantRequest{Identity: "friend", Role: "member", Project: lendPool}); err != nil {
		t.Fatal(err)
	}
	var r lendRun
	var out bytes.Buffer
	err := lend(ctx, c, r.runner(0), lendOpts(lendMac(250)), lendChoice{}, home, &out, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "shared with friend") || r.all() != "" {
		t.Fatalf("lend to a shared home: %v (ran %q)", err, r.all())
	}
	if m := bannedLend.FindAllString(strings.ReplaceAll(plainErr(err).Error(), "zm ", ""), -1); len(m) > 0 {
		t.Errorf("refusal says %v: %v", m, err)
	}
}

// A binding with no image keeps the current image choice instead of
// panicking in hostprobe.KeepEarlierImage; its away
// setting still counts.
func TestKeepEarlierNoImage(t *testing.T) {
	st := lendSettings{With: "xcode", RunnerDir: "r"}
	keepEarlier(&st, computeapi.RunTarget{Config: map[string]string{"reclaim_idle_s": "60"}})
	if st.With != "xcode" || st.RunnerDir != "r" || st.Images != nil || st.When != "away" || st.IdleS != 60 {
		t.Fatalf("settings = %+v", st)
	}
}
