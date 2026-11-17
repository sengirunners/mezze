package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// poolDaemon is one machine's daemon on the fake tailnet; its tailnet node
// is not up until a command brings it up.
func poolDaemon(t *testing.T, tn *tailnettest.Net, machine, login string) (*server.Daemon, *client.Client) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	cfg.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(tailnettest.Identity{NodeID: "n" + machine, NodeKey: "nodekey:" + machine, Login: login, Host: machine})
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", machine)
		}
	})
	sock := client.NewUnix(cfg.SocketPath())
	for i := 0; ; i++ {
		if _, err := sock.Health(ctx); err == nil {
			break
		}
		if i > 400 {
			t.Fatalf("%s: socket never answered", machine)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return dm, client.WithHandler(dm.SocketHandler())
}

// beat sends d's heartbeats to its authorities until the test ends.
func beat(t *testing.T, dm *server.Daemon) {
	for _, m := range dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ctx.Err() == nil {
					s.Heartbeat(ctx)
					select {
					case <-ctx.Done():
					case <-time.After(50 * time.Millisecond):
					}
				}
			}()
			t.Cleanup(func() { cancel(); <-done })
			return
		}
	}
	t.Fatal("no team module")
}

var (
	joinRe = regexp.MustCompile(`zm pool join '([^']+)'`)
	hostRe = regexp.MustCompile(`zm pool host '([^']+)'`)
)

// The co-op's front door, on daemons with no tailnet up,
// with the roles of SPEC's pools section: the
// owner starts a pool and prints a renter's and two hosts' invites, and is
// done: nothing waits on the owner's side. A friend and a stranger each lend
// a machine with one command (guests of the hosts area, classed trusted and
// public by their invites as they join), a renter joins
// the team area with one command and runs sandboxes on both; a renter of
// another team's area sees none of hers.
func TestPoolCreateHostJoinRunsABoxAcross(t *testing.T) {
	old, oldAdvert := teamlink.BeatEvery, advertWait
	teamlink.BeatEvery, advertWait = time.Hour, 10*time.Second // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery, advertWait = old, oldAdvert })
	ctx := context.Background()
	tn := tailnettest.New()
	ownerDM, owner := poolDaemon(t, tn, "mini", "ivo@example.com")
	hostDM, host := poolDaemon(t, tn, "lender", "bea@example.com")
	strangerDM, stranger := poolDaemon(t, tn, "stranger", "sam@example.com")
	_, user := poolDaemon(t, tn, "laptop", "ana@example.com")
	_, other := poolDaemon(t, tn, "desk", "bo@example.com")

	const pin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	var out bytes.Buffer
	err := poolCreate(ctx, owner, "friends", poolInvites{Renters: []string{"ana"}, Hosts: []string{"bea"}, PublicHosts: []string{"sam"}, Images: []string{pin}}, &out)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	renterCode, hostCodes := joinRe.FindAllStringSubmatch(out.String(), -1), hostRe.FindAllStringSubmatch(out.String(), -1)
	if len(renterCode) != 1 || len(hostCodes) != 2 || !strings.Contains(out.String(), "zm pool host '"+hostCodes[0][1]+"' --image "+pin) {
		t.Fatalf("create printed:\n%s", out.String())
	}
	// Next to each invite: what the host's class means, in plain words, and
	// the tailnet step its friend needs (SPEC 17.4), naming this node.
	for _, want := range []string{
		"their machine will be " + classMeans[api.MachineTrusted],
		"their machine will be " + classMeans[api.MachinePublic],
		"never GitHub Actions jobs",
		"before ana runs it", "before bea runs it", "before sam runs it",
		"https://login.tailscale.com/admin/machines (potluck-mini, then the ... menu, Share)", // a fresh home's node
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("create does not print %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "waiting") {
		t.Fatalf("create waits:\n%s", out.String())
	}
	// A friend who comes later: the same command again adds their invite.
	var later bytes.Buffer
	if err := poolCreate(ctx, owner, "friends", poolInvites{Hosts: []string{"late"}}, &later); err != nil || len(hostRe.FindAllString(later.String(), -1)) != 1 {
		t.Fatalf("create again: %v\n%s", err, later.String())
	}

	// The lenders: the reference driver stands in for Tart; conformance is
	// hostprobe's to test, so it passes here.
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, dm := range []*server.Daemon{hostDM, strangerDM, ownerDM} {
		for _, m := range dm.Modules() {
			if s, ok := m.(*runtarget.Service); ok {
				s.SetLookPath(func(string) (string, error) { return drv, nil })
			}
		}
	}
	beat(t, hostDM)
	beat(t, strangerDM)
	var ran []string
	run := runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return hostprobe.Result{Stdout: "PASS\nPASS  a box cannot send mail: smtp.gmail.com:587\n"}, nil // runner boxes prove the mail check
	})
	o := hostprobe.ShareOptions{Report: hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
		Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}, Network: hostprobe.BoxNet}}},
		Images: []string{pin}, Sizes: []string{"c1m2"}, NamePrefix: "lender", Driver: "ref", RunnerDir: "/home/runner"}
	// A machine that fails the security checks never joins: the code stays unspent.
	failing := runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		if name == "potluck-conformance" || name == "zm-run-conformance" { // the suite by either name (hostprobe takes the one on PATH)
			return hostprobe.Result{Exit: 1, Stdout: "FAIL  a new box does not see the last box's files"}, nil
		}
		return hostprobe.Result{}, nil
	})
	// zm's daily update is installed only once the machine is lent, after
	// the pool lists its boxes.
	afters := 0
	after := func(w io.Writer) { afters++; fmt.Fprintln(w, "updates: AFTER") }
	if err := poolHost(ctx, host, failing, hostCodes[0][1], o, &bytes.Buffer{}, nil, after); err == nil || !strings.Contains(err.Error(), "did not join") || afters != 0 {
		t.Fatalf("a failing host: %v (after ran %d)", err, afters)
	}
	if rems, err := host.Remotes(ctx); err != nil || len(rems) != 0 {
		t.Fatalf("a failing host joined: %+v %v", rems, err)
	}
	out.Reset()
	if err := poolHost(ctx, host, run, hostCodes[0][1], o, &out, nil, after); err != nil {
		t.Fatalf("host: %v\n%s", err, out.String())
	}
	if s := out.String(); afters != 1 || !strings.HasSuffix(s, "updates: AFTER\n") || !strings.Contains(s, "lent to pool friends") {
		t.Fatalf("after ran %d:\n%s", afters, s)
	}
	if !strings.Contains(out.String(), "joined pool friends") || !strings.Contains(out.String(), "lent to pool friends: lender-c1m2@lender") ||
		strings.Contains(out.String(), "serve no GitHub Actions jobs") {
		t.Fatalf("host printed:\n%s", out.String())
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "docker pull") || !strings.Contains(ran[1], "--only "+hostprobe.QuickGroups) {
		t.Errorf("host ran %v", ran)
	}
	o.NamePrefix = "stranger"
	out.Reset()
	if err := poolHost(ctx, stranger, run, hostCodes[1][1], o, &out, nil, nil); err != nil || !strings.Contains(out.String(), "lent to pool friends: stranger-c1m2@stranger") {
		t.Fatalf("stranger host: %v\n%s", err, out.String())
	}
	// A public host's runner boxes get no jobs from the broker: host says so,
	// with the owner's command, instead of leaving jobs queued.
	if s := out.String(); !strings.Contains(s, "pool friends holds stranger public, so its boxes serve no GitHub Actions jobs") ||
		!strings.Contains(s, "zm machine-class stranger trusted -P friends") {
		t.Fatalf("stranger host does not say it serves no runners:\n%s", s)
	}
	// zm target unbind -P friends <binding>@<machine> on the host itself: the
	// authority refuses it, and the refusal names the form that works.
	sp := inPool(stranger, "friends")
	err = unbindHere(ctx, sp, "stranger-c1m2@stranger", computeclient.UnbindRunTarget(ctx, sp, "stranger-c1m2@stranger"))
	if e := (*api.Error)(nil); !errors.As(err, &e) || e.Hint != "zm target unbind --host stranger-c1m2" {
		t.Fatalf("unbind by the pool's name on its host: %v", err)
	}
	for _, n := range []string{"lender-c1m2@lender", "stranger-c1m2@lender"} { // another machine's box, or its name for this binding
		if err := unbindHere(ctx, sp, n, computeclient.UnbindRunTarget(ctx, sp, n)); !api.IsCode(err, api.CodeBadRequest) || strings.Contains(err.Error(), "unbind --host") {
			t.Fatalf("unbind %s: %v", n, err)
		}
	}
	// The same code again (the invite is spent): it fails, names the pool this
	// machine already joined and the command that re-shares; it shares nothing
	// itself.
	o.NamePrefix = "lender"
	out.Reset()
	err = poolHost(ctx, host, run, hostCodes[0][1], o, &out, nil, nil)
	var spent *spentCodeError
	if !errors.As(err, &spent) || !strings.Contains(err.Error(), "invalid, used or expired join code") || !strings.Contains(err.Error(), "zm host share --pool friends") || strings.Contains(out.String(), "lent to") {
		t.Fatalf("host again: %v\n%s", err, out.String())
	}
	// Hosts are guests of the hosts area, never project members; their class
	// is the owner's choice at invite time.
	for _, h := range []*client.Client{host, stranger} {
		rems, err := h.Remotes(ctx)
		if err != nil || len(rems) != 1 || rems[0].Cap.Area != HostsArea || rems[0].Cap.Role != "guest" {
			t.Fatalf("a host's join: %+v %v", rems, err)
		}
	}
	classes, err := owner.MachineClasses(ctx, "friends")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range classes.Machines {
		got[m.Machine] = m.Class
	}
	if got["lender"] != api.MachineTrusted || got["stranger"] != api.MachinePublic {
		t.Fatalf("classes %v", got)
	}

	// The renter: a member of the team area, one command.
	out.Reset()
	rem, err := poolJoin(ctx, user, joinCode(renterCode), &out)
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out.String())
	}
	if rem.Cap.Area != "team" || rem.Cap.Role != "member" {
		t.Fatalf("a renter's join: %+v", rem.Cap)
	}
	if names := poolBoxes(ctx, user, rem.Project.Slug, &out); len(names) != 2 {
		t.Fatalf("boxes %v:\n%s", names, out.String())
	}
	u := inPool(user, "friends")
	box := func(target, key string) string {
		t.Helper()
		h, err := computeclient.AcquireRunTarget(ctx, u, target, computeapi.AcquireRunTargetRequest{RequestID: key, TTLS: 120})
		if err != nil {
			t.Fatalf("acquire on %s: %v", target, err)
		}
		return h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	}
	secret := map[string]string{"API_TOKEN": "hunter2hunter2"}
	// A friend's (trusted) machine: placed unasked, and it gets secrets.
	targets, err := computeclient.PlaceBox(ctx, u, computeclient.BoxWant{})
	if err != nil || len(targets) != 1 || targets[0] != "lender-c1m2@lender" {
		t.Fatalf("placement: %q %v", targets, err)
	}
	target := targets[0]
	id := box(target, "s1")
	x, err := computeclient.ExecRunTarget(ctx, u, target, id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", `cat "$ZM_SECRETS/API_TOKEN"`}, Secrets: secret})
	if err != nil || x.Exit != 0 || x.Stdout != "hunter2hunter2" {
		t.Fatalf("exec with a secret on a trusted host: %+v %v", x, err)
	}
	// A stranger's (public) machine: placed when named, never given secrets.
	pubs, err := computeclient.PlaceBox(ctx, u, computeclient.BoxWant{On: "stranger-c1m2@stranger"})
	if err != nil || len(pubs) != 1 || pubs[0] != "stranger-c1m2@stranger" {
		t.Fatalf("placement on the public host: %q %v", pubs, err)
	}
	pub := pubs[0]
	pid := box(pub, "s2")
	if _, err := computeclient.ExecRunTarget(ctx, u, pub, pid, computeapi.ExecRunTargetRequest{Argv: []string{"true"}, Secrets: secret}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("secrets on a public host: %v", err)
	}
	if x, err := computeclient.ExecRunTarget(ctx, u, pub, pid, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "hello from the pool"}}); err != nil || x.Stdout != "hello from the pool\n" {
		t.Fatalf("exec on the public host: %+v %v", x, err)
	}

	// Another team's renter (an area the owner adds with zm area add and
	// zm invite) sees none of this team's boxes.
	if _, err := owner.AddArea(ctx, "friends", api.AddAreaRequest{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	inv, err := owner.Invite(ctx, api.InviteRequest{Project: "friends", Area: "other", As: "bo", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poolJoin(ctx, other, inv.Code, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ob := inPool(other, "friends")
	if _, err := computeclient.RunTargetStatus(ctx, ob, target, id); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("another team's renter reads a box: %v", err)
	}
	if hs, _, err := computeclient.Sandboxes(ctx, ob, false); err != nil || len(hs) != 0 {
		t.Errorf("another team's renter lists %+v %v", hs, err)
	}
	for tg, i := range map[string]string{target: id, pub: pid} {
		if rel, err := computeclient.ReleaseRunTarget(ctx, u, tg, i); err != nil || !rel.Released {
			t.Fatalf("release: %+v %v", rel, err)
		}
	}

	// The owner lends the pool's own machine by name: a binding in the pool.
	out.Reset()
	o.NamePrefix = "mini"
	if err := poolHost(ctx, owner, run, "friends", o, &out, nil, nil); err != nil || !strings.Contains(out.String(), "lent to pool friends: mini-c1m2\n") {
		t.Fatalf("the owner's host: %v\n%s", err, out.String())
	}
	if err := poolHost(ctx, user, run, "elsewhere", o, &bytes.Buffer{}, nil, nil); err == nil || !strings.Contains(err.Error(), "pass the invite code") {
		t.Errorf("a pool neither joined nor run: %v", err)
	}

	// A fresh code for a second pool on the same authority joins that pool:
	// a code names no project, so the first pool's remote must not stand in.
	out.Reset()
	if err := poolCreate(ctx, owner, "family", poolInvites{Hosts: []string{"bea"}}, &out); err != nil {
		t.Fatalf("create family: %v\n%s", err, out.String())
	}
	fam := hostRe.FindAllStringSubmatch(out.String(), -1)
	if len(fam) != 1 {
		t.Fatalf("family invites:\n%s", out.String())
	}
	out.Reset()
	if err := poolHost(ctx, host, run, fam[0][1], o, &out, nil, nil); err != nil || !strings.Contains(out.String(), "joined pool family") || !strings.Contains(out.String(), "lent to pool family") {
		t.Fatalf("host joins a second pool: %v\n%s", err, out.String())
	}
}

func joinCode(m [][]string) string { return m[0][1] }

// A tunnel code needs no tailnet, so no tailnet step is printed for it.
func TestPoolTailnetStepOnlyForTailnetCodes(t *testing.T) {
	if s := tailnetStep("ana", "https://zm.example.com#ABCDEFGHJKMNPQRSTVWXYZ0123:XXXXX-XXXXX-XXXXX-XXXXX-XXXXX"); s != "" {
		t.Errorf("a tunnel code: %q", s)
	}
	if s := tailnetStep("ana", "zm-dev-05.tail1234.ts.net:0123456789abcdef:K7QM-3XRT"); !strings.Contains(s, "(zm-dev-05, then") {
		t.Errorf("a tailnet code: %q", s)
	}
	// Rename step 1: a code naming a potluck-<machine> node names that node.
	if s := tailnetStep("ana", "potluck-dev-05.tail1234.ts.net:0123456789abcdef:K7QM-3XRT"); !strings.Contains(s, "(potluck-dev-05, then") {
		t.Errorf("a potluck tailnet code: %q", s)
	}
}

// A machine that cannot lend is refused before its invite is spent: the
// root step comes back as one line and nothing is joined.
func TestPoolHostChecksBeforeJoining(t *testing.T) {
	o := hostprobe.ShareOptions{Report: hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, Layers: []hostprobe.Layer{
		{Name: "oci", Supported: true, Problems: []string{"no container CLI"}, RootSteps: []string{"apt-get install -y docker.io"}}}},
		Images: []string{"x@sha256:" + strings.Repeat("a", 64)}, NamePrefix: "h"}
	var joined bool
	err := poolHost(context.Background(), nil, runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		joined = true
		return hostprobe.Result{}, nil
	}), "zm-mini:abcd:ABCD-EFGH", o, &bytes.Buffer{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sudo sh -c 'apt-get install -y docker.io'") || joined {
		t.Fatalf("%v (ran %v)", err, joined)
	}
}

// zm host share and zm pool host take --runner-dir; a runner image on an oci
// host without Potluck's filtered network is lent for jobs only, and the plan says
// so with the one root line.
func TestPoolHostRunnerBoxesNeedTheNetwork(t *testing.T) {
	for _, c := range []*cobra.Command{hostShareCmd(), poolHostCmd()} {
		if c.Flags().Lookup("runner-dir") == nil {
			t.Errorf("%s has no --runner-dir", c.Name())
		}
	}
	oci := hostprobe.Layer{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"},
		NetworkProblem: "not set up", NetworkSteps: hostprobe.BoxNetSteps()}
	o := hostprobe.ShareOptions{Report: hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, Layers: []hostprobe.Layer{oci}},
		Images: []string{"x@sha256:" + strings.Repeat("a", 64)}, NamePrefix: "h", Sizes: []string{"c1m2"}, RunnerDir: "/home/runner", DryRun: true}
	var out bytes.Buffer
	if err := poolHost(context.Background(), nil, runnerFunc(nil), "pool", o, &out, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "note: boxes serve jobs only, not GitHub Actions") ||
		!strings.Contains(s, "  sudo sh -c 'printf \"%s\\n\" \"[Unit]\"") || strings.Contains(s, "runner_dir=") {
		t.Errorf("plan without the network:\n%s", s)
	}
	o.Report.Layers[0].Network, o.Report.Layers[0].NetworkSteps = hostprobe.BoxNet, nil
	out.Reset()
	if err := poolHost(context.Background(), nil, runnerFunc(nil), "pool", o, &out, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "GitHub Actions: each box serves a runner (runner_dir /home/runner, network "+hostprobe.BoxNet+")") ||
		!strings.Contains(s, "--net-allow "+hostprobe.RunnerNetAllow) || strings.Contains(s, "--config runner_dir") || !strings.Contains(s, "--net-mail "+hostprobe.DefaultNetMail) || strings.Contains(s, "note: boxes serve jobs only") {
		t.Errorf("plan with the network:\n%s", s)
	}
	// The plan says up front that runners need a trusted host.
	if s := out.String(); !strings.Contains(s, "note: runners only on a trusted host") || !strings.Contains(s, "zm machine-class <this machine> trusted -P pool") {
		t.Errorf("plan does not say runners need a trusted host:\n%s", s)
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) (hostprobe.Result, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
	return f(ctx, name, args...)
}
