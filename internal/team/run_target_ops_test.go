package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// boxRig is an authority ("mini", project harbor) and a host ("box-host",
// joined as hostRole) lending binding "sandbox" (the reference driver) to
// harbor; ana is a member calling the authority with a token.
type boxRig struct {
	tn      *tailnettest.Net
	a, host *daemon
	ts      *team.Service      // the authority's team module
	rts     *runtarget.Service // the authority's run targets (the relay)
	hostTS  *team.Service
	project string // harbor's ULID at the authority
	ana     *client.Client
	slow    string     // a file that, while it exists, makes every driver call take 3 s more
	wire    *switchTap // every byte between the machines, once a test turns it on
}

// switchTap is the tailnet's tap, set from the rig's start so even the
// connections its machines keep open are seen; it records nothing until on.
type switchTap struct {
	on  atomic.Bool
	tap wireTap
}

func (w *switchTap) Write(p []byte) (int, error) {
	if w.on.Load() {
		return w.tap.Write(p)
	}
	return len(p), nil
}

// count is how many times s crossed since the tap was turned on.
func (w *switchTap) count(s string) int {
	w.tap.mu.Lock()
	defer w.tap.mu.Unlock()
	return bytes.Count(w.tap.buf.Bytes(), []byte(s))
}

func newBoxRig(t *testing.T, hostRole string) *boxRig {
	t.Helper()
	ctx := context.Background()
	g := &boxRig{tn: tailnettest.New(), wire: &switchTap{}}
	g.tn.Tap(g.wire)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	g.a = startDaemonWith(t, g.tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(g.a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(g.a.owner.AddArea(ctx, "harbor", api.Area{Name: "solver"}))(t)
	mustf(g.a.owner.ProjectTeam(ctx, "harbor"))(t)
	g.host = startDaemonWith(t, g.tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "bea@example.com", Host: "box-host"}, discard, roomy)
	proj := "harbor"
	if hostRole == "guest" {
		proj = "harbor/solver"
	}
	inv := mustf(g.a.owner.Invite(ctx, api.InviteRequest{Project: proj, As: "bea", Role: hostRole}))(t)
	j := mustf(g.host.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	g.project = j.Remote.Project.ID
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	g.slow = filepath.Join(fix, "slow")
	wrap := filepath.Join(fix, "zm-run-slowable")
	if err := os.WriteFile(wrap, []byte("#!/bin/sh\n[ -f '"+g.slow+"' ] && sleep 3\nexec '"+drv+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runTargetSvc(t, g.host).SetLookPath(func(string) (string, error) { return wrap, nil })
	mustf(computeclient.BindRunTarget(ctx, g.host.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}}))(t)
	g.ts, g.rts, g.hostTS = teamService(t, g.a), runTargetSvc(t, g.a), teamService(t, g.host)
	tok := mustf(g.a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "harbor", Role: "member"}))(t)
	g.ana = client.WithHandler(g.a.dm.Handler)
	g.ana.Token, g.ana.Project, g.ana.ProjectFrom = tok.Secret, "harbor", "flag"
	return g
}

// advertised beats the host by hand until the authority holds its advert
// (the heartbeat's own period is left alone).
func (g *boxRig) advertised(t *testing.T) {
	t.Helper()
	waitFor(t, "the host's box advertised", func() bool {
		g.hostTS.Heartbeat(context.Background())
		return len(g.rts.RunTargetAdverts(context.Background(), g.project)) > 0
	})
}

// beatEvery beats the host by hand every d (the test's quiet presence turned
// the heartbeat's own period off) until the returned stop is called.
func (g *boxRig) beatEvery(t *testing.T, d time.Duration) (stop func()) {
	t.Helper()
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		for tk := time.NewTicker(d); ; {
			select {
			case <-done:
				tk.Stop()
				return
			case <-tk.C:
				g.hostTS.Heartbeat(context.Background())
			}
		}
	}()
	var once sync.Once
	stop = func() { once.Do(func() { close(done); <-exited }) }
	t.Cleanup(stop)
	return stop
}

// box acquires a box with request id rid as c and answers its id.
func (g *boxRig) box(t *testing.T, c *client.Client, rid string) string {
	t.Helper()
	h, err := computeclient.AcquireRunTarget(context.Background(), c, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: rid, TTLS: 300})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return h.Handle[strings.LastIndex(h.Handle, "/")+1:]
}

// An exec printing 2 MiB of zero bytes (12 MiB once JSON escapes them)
// answers truncated, and the host's heartbeats keep going through:
// its report posted at once, or with the heartbeat
// alone.
func TestRunTargetBigOutputKeepsBeating(t *testing.T) {
	for _, mode := range []string{"direct", "heartbeat"} {
		t.Run(mode, func(t *testing.T) {
			quietPresence(t)
			if mode == "heartbeat" {
				t.Cleanup(team.OpsPollOff())
			}
			ctx := context.Background()
			g := newBoxRig(t, "member")
			beating(t, g.host)
			g.advertised(t)
			id := g.box(t, g.ana, "big")
			out, err := computeclient.ExecRunTarget(ctx, g.ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{
				Argv: []string{"sh", "-c", "head -c 1048576 /dev/zero; head -c 1048576 /dev/zero >&2"}})
			if err != nil || !out.Truncated || out.Exit != 0 || len(out.Stdout)+len(out.Stderr) == 0 {
				t.Fatalf("exec of 2 MiB of zero bytes: truncated=%v exit=%d out=%d+%d %v", out.Truncated, out.Exit, len(out.Stdout), len(out.Stderr), err)
			}
			// The next op travels too: the beats after the big one were accepted.
			if out, err := computeclient.ExecRunTarget(ctx, g.ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "ok"}}); err != nil || out.Stdout != "ok\n" {
				t.Fatalf("exec after the big one: %+v %v", out, err)
			}
			if _, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A relayed call waits for no heartbeat: with beats an
// hour apart (PresenceEvery is not shortened: lengthened, so nothing below
// can ride one), the host's held-open poll takes each op and posts its
// report. Measured against the driver's own time for the same verb on the
// host: the first call on a cold host, execs back to back, and an exec after
// the host sat idle.
func TestRunTargetRelayWaitsForNoHeartbeat(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	g := newBoxRig(t, "member")
	g.advertised(t) // beats by hand until the advert is in; none after
	host := runTargetSvc(t, g.host)
	// The driver's own times, on the host (not relayed).
	t0 := time.Now()
	direct, err := host.HostCall(ctx, g.project, "sandbox", "acquire", "", []byte(`{"request_id":"direct","actor":"x","ttl_s":300}`))
	drvAcquire := time.Since(t0)
	var dh struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(direct, &dh) != nil {
		t.Fatalf("direct acquire: %s %v", direct, err)
	}
	ex, _ := json.Marshal(map[string]any{"argv": []string{"echo", "hi"}, "timeout_s": 30})
	var drv []time.Duration
	for range 5 {
		t0 := time.Now()
		if _, err := host.HostCall(ctx, g.project, "sandbox", "exec", dh.Handle, ex); err != nil {
			t.Fatal(err)
		}
		drv = append(drv, time.Since(t0))
	}
	// Cold: the first relayed op since the host joined.
	t0 = time.Now()
	id := g.box(t, g.ana, "lat")
	coldAcquire := time.Since(t0)
	exec := func() time.Duration {
		t0 := time.Now()
		out, err := computeclient.ExecRunTarget(ctx, g.ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "hi"}})
		if err != nil || out.Stdout != "hi\n" {
			t.Fatalf("exec: %+v %v", out, err)
		}
		return time.Since(t0)
	}
	var warm []time.Duration
	for range 5 {
		warm = append(warm, exec())
	}
	time.Sleep(2 * time.Second) // idle: the poll is mid-wait
	idle := exec()
	median := func(ds []time.Duration) time.Duration {
		ds = slices.Clone(ds)
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	over := median(warm) - median(drv)
	t.Logf("driver exec %v (median of %v); relayed exec warm %v (%v), added %v; after 2 s idle %v; acquire: driver %v, relayed cold %v",
		median(drv), drv, median(warm), warm, over, idle, drvAcquire, coldAcquire)
	if over > 200*time.Millisecond || idle-median(drv) > 500*time.Millisecond || coldAcquire-drvAcquire > 500*time.Millisecond {
		t.Errorf("a relayed call waited: exec added %v, idle exec %v, cold acquire %v (driver %v)", over, idle, coldAcquire, drvAcquire)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil {
		t.Fatal(err)
	}
	if _, err := host.HostCall(ctx, g.project, "sandbox", "release", dh.Handle, nil); err != nil {
		t.Fatal(err)
	}
}

// With the poll down, the heartbeat still carries ops and their reports.
func TestRunTargetHeartbeatFallback(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.OpsPollOff())
	ctx := context.Background()
	g := newBoxRig(t, "member")
	beating(t, g.host)
	g.advertised(t)
	id := g.box(t, g.ana, "fb")
	if out, err := computeclient.ExecRunTarget(ctx, g.ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "ok"}}); err != nil || out.Stdout != "ok\n" {
		t.Fatalf("exec: %+v %v", out, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil {
		t.Fatal(err)
	}
}

// A 30 s exec riding the heartbeat (the poll down) crosses to the host in
// one heartbeat answer, not in every answer while it runs: once the host
// reports it took the op, no later answer carries its argv or stdin
// (Potluck's Took).
func TestRunTargetHeartbeatSendsArgsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("a 30 s exec")
	}
	quietPresence(t)
	t.Cleanup(team.OpsPollOff())
	ctx := context.Background()
	g := newBoxRig(t, "member")
	beating(t, g.host)
	g.advertised(t)
	id := g.box(t, g.ana, "once")
	const stdin, argv = "STDIN-MARK-6c1f0e", "ARGV-MARK-9d2a47"
	g.wire.on.Store(true)
	t0 := time.Now()
	out, err := computeclient.ExecRunTarget(ctx, g.ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{
		Argv: []string{"sh", "-c", "sleep 30 # " + argv}, Stdin: stdin, TimeoutS: 90})
	took := time.Since(t0)
	g.wire.on.Store(false)
	if err != nil || out.Exit != 0 {
		t.Fatalf("a 30 s exec: %+v %v (%v)", out, err, took)
	}
	beats, withStdin, withArgv := g.wire.count("POST /v0/team/presence"), g.wire.count(stdin), g.wire.count(argv)
	t.Logf("30 s exec took %v: %d heartbeats, stdin in %d, argv in %d", took, beats, withStdin, withArgv)
	if took < 30*time.Second || beats < 100 {
		t.Fatalf("the exec did not span many beats: %v, %d beats", took, beats)
	}
	if withStdin != 1 || withArgv != 1 {
		t.Errorf("the op's args crossed %d (stdin) and %d (argv) times in %d heartbeats, want once", withStdin, withArgv, beats)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil {
		t.Fatal(err)
	}
}

// member joins a member's machine ("ana-mac") and answers its socket client
// for harbor.
func (g *boxRig) member(t *testing.T) *client.Client {
	t.Helper()
	ctx := context.Background()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	mem := startDaemonWith(t, g.tn, "ana-mac", tailnettest.Identity{NodeID: "nANA", NodeKey: "nodekey:ana-mac", Login: "ana@example.com", Host: "ana-mac"}, discard, roomy)
	mustf(mem.owner.Join(ctx, api.JoinRequest{Code: mustf(g.a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "ana", Role: "member"}))(t).Code}))(t)
	return at(mem.owner, "harbor", "")
}

// From a member's machine, a relayed exec or acquire that takes longer than
// the proxy's CallTimeout still answers (it waits the verb's own limit), and
// an acquire retried with its request id answers the same box.
func TestRunTargetRelayOutlivesCallTimeout(t *testing.T) {
	quietPresence(t)
	old := teamlink.CallTimeout
	teamlink.CallTimeout = 2 * time.Second
	t.Cleanup(func() { teamlink.CallTimeout = old })
	ctx := context.Background()
	g := newBoxRig(t, "member")
	ana := g.member(t)
	g.advertised(t)
	// An acquire slower than CallTimeout: one box.
	if err := os.WriteFile(g.slow, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "slow", TTLS: 300})
	took := time.Since(t0)
	if err != nil || took < teamlink.CallTimeout {
		t.Fatalf("a slow acquire from a member's machine: %+v %v (%v)", h, err, took)
	}
	if err := os.Remove(g.slow); err != nil {
		t.Fatal(err)
	}
	again, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "slow", TTLS: 300})
	if err != nil || again.Handle != h.Handle {
		t.Fatalf("the retry with the same request id: %+v %v (first %s)", again, err, h.Handle)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, ana, "sandbox@box-host", true); err != nil || len(hs) != 1 {
		t.Fatalf("live handles after the retry: %+v %v", hs, err)
	}
	// An exec running twice CallTimeout.
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	t0, polls := time.Now(), team.OpsPolls()
	out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "sleep 4; echo done"}, TimeoutS: 20})
	if err != nil || out.Exit != 0 || out.Stdout != "done\n" {
		t.Fatalf("a 4 s exec with a 2 s CallTimeout: %+v %v (%v)", out, err, time.Since(t0))
	}
	// The poll waits while the op runs: it does not spin on the op it carried.
	if n := team.OpsPolls() - polls; n > 5 {
		t.Errorf("%d ops polls during one 4 s exec", n)
	}
	t.Logf("slow acquire %v; 4 s exec %v, %d polls", took, time.Since(t0), team.OpsPolls()-polls)
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id); err != nil {
		t.Fatal(err)
	}
}

// Only enrolled hosts lend boxes: a guest's machine
// advertising a box is neither listed nor placed on; a member's machine the
// owner made public is listed, but placement picks it only by name.
func TestRunTargetUnenrolledHostNotListed(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	g := newBoxRig(t, "guest")
	host := runTargetSvc(t, g.host)
	waitFor(t, "the guest's machine describes its box", func() bool {
		g.hostTS.Heartbeat(ctx)
		return len(host.HostAdverts(ctx, g.project)) > 0
	})
	g.hostTS.Heartbeat(ctx) // a beat carrying the advert
	if ads := g.rts.RunTargetAdverts(context.Background(), g.project); len(ads) != 0 {
		t.Fatalf("a guest's adverts kept: %+v", ads)
	}
	l := mustf(computeclient.RunTargets(ctx, g.ana, false))(t)
	for _, v := range l.Targets {
		if strings.HasSuffix(v.Name, "@box-host") {
			t.Fatalf("a guest's box listed: %+v", v)
		}
	}
	if _, err := computeclient.AcquireRunTarget(ctx, g.ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "g1"}); err == nil {
		t.Fatal("an acquire on a guest's box")
	}
	if _, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: "acquire", Args: []byte(`{"request_id":"g2"}`)}, 5*time.Second); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a relayed acquire to a guest's machine: %v", err)
	}
	if _, err := computeclient.PlaceBox(ctx, g.ana, computeclient.BoxWant{}); err == nil {
		t.Error("placement found a box on a guest's machine")
	}

	// A member's machine the owner made public: listed, placed on only by name.
	m := newBoxRig(t, "member")
	m.advertised(t)
	mustf(m.a.owner.SetMachineClass(ctx, "box-host", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}))(t)
	if _, err := computeclient.PlaceBox(ctx, m.ana, computeclient.BoxWant{}); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "public") {
		t.Errorf("placement unasked on a public host: %v", err)
	}
	if got, err := computeclient.PlaceBox(ctx, m.ana, computeclient.BoxWant{On: "sandbox@box-host"}); err != nil || len(got) != 1 || got[0] != "sandbox@box-host" {
		t.Errorf("a public host by name: %q %v", got, err)
	}
}

// One host lending one binding to two projects: the
// same actor and request id in each get two boxes, and neither project's
// relay can see, exec in or end the other's.
func TestRunTargetTwoProjectsOneHost(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	g := newBoxRig(t, "member")
	mustf(g.a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}))(t)
	mustf(g.a.owner.ProjectTeam(ctx, "acme"))(t)
	j := mustf(g.host.owner.Join(ctx, api.JoinRequest{Code: mustf(g.a.owner.Invite(ctx, api.InviteRequest{Project: "acme", As: "bea", Role: "member"}))(t).Code}))(t)
	acme := j.Remote.Project.ID
	mustf(computeclient.BindRunTarget(ctx, g.host.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{"harbor", "acme"}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}}))(t)
	waitFor(t, "the box advertised to both projects", func() bool {
		g.hostTS.Heartbeat(ctx)
		return len(g.rts.RunTargetAdverts(context.Background(), g.project)) > 0 && len(g.rts.RunTargetAdverts(context.Background(), acme)) > 0
	})
	req := computeapi.AcquireRunTargetRequest{RequestID: "same", Actor: "mini/agent", TTLS: 300}
	ho := mustf(computeclient.AcquireRunTarget(ctx, at(g.a.owner, "harbor", ""), "sandbox@box-host", req))(t)
	ha := mustf(computeclient.AcquireRunTarget(ctx, at(g.a.owner, "acme", ""), "sandbox@box-host", req))(t)
	hostID := func(h computeapi.RunTargetHandle) string {
		return "box:sandbox/" + h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	}
	if hostID(ho) == hostID(ha) {
		t.Fatalf("one box for both projects: %s", ho.Handle)
	}
	ex := []byte(`{"argv":["id"],"timeout_s":5}`)
	for verb, args := range map[string][]byte{"status": nil, "exec": ex, "renew": []byte(`{"ttl_s":60}`), "release": nil} {
		if _, err := g.rts.RelayRunTarget(ctx, acme, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: verb, ID: hostID(ho), Args: args}, 10*time.Second); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("acme's %s of harbor's box: %v", verb, err)
		}
	}
	res, err := g.rts.RelayRunTarget(ctx, acme, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: "list", Args: []byte(`{"request_id":"` + ha.RequestID + `"}`)}, 10*time.Second)
	if err != nil || strings.Contains(string(res), hostID(ho)) || !strings.Contains(string(res), hostID(ha)) {
		t.Errorf("acme's list: %s %v", res, err)
	}
	if st, err := computeclient.RunTargetStatus(ctx, at(g.a.owner, "harbor", ""), "sandbox@box-host", ho.Handle[strings.LastIndex(ho.Handle, "/")+1:]); err != nil || st.State != "ready" {
		t.Errorf("harbor's box after acme's tries: %+v %v", st, err)
	}
	for proj, h := range map[string]computeapi.RunTargetHandle{"harbor": ho, "acme": ha} {
		if _, err := computeclient.ReleaseRunTarget(ctx, at(g.a.owner, proj, ""), "sandbox@box-host", h.Handle[strings.LastIndex(h.Handle, "/")+1:]); err != nil {
			t.Errorf("%s's release: %v", proj, err)
		}
	}
}

// A member's relayed run-target calls leave no live-write record at the
// authority: an exec's output and an acquire's answer are never stored.
func TestRunTargetAnswersNotRecorded(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	g := newBoxRig(t, "member")
	ana := g.member(t)
	g.advertised(t)
	h := mustf(computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "rec", TTLS: 300}))(t)
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	if out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "SECRET-OUTPUT-xyz"}}); err != nil || out.Stdout != "SECRET-OUTPUT-xyz\n" {
		t.Fatalf("exec: %+v %v", out, err)
	}
	db := mustf(g.a.dm.Deps.Store.Project(authz.All(), authz.Read, g.project))(t)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM team_writes WHERE body LIKE '%SECRET-OUTPUT%' OR body LIKE '%box-host/%' OR req LIKE '%run_targets%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("run-target answers recorded at the authority: %d %v", n, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id); err != nil {
		t.Fatal(err)
	}
}

// A host that stopped beating is not waited on: status and renew relayed to
// it answer unavailable at once, not after the verb's limit.
func TestRunTargetSilentHostFailsFast(t *testing.T) {
	quietPresence(t)
	t.Cleanup(runtarget.SetAdvertStale(time.Second))
	ctx := context.Background()
	g := newBoxRig(t, "member")
	g.advertised(t)
	id := g.box(t, g.ana, "quiet")
	g.tn.Cut("potluck-box-host", "potluck-mini", true) // its laptop lost the network: no beat, no poll
	time.Sleep(1200 * time.Millisecond)                // past the stale window
	for verb, args := range map[string][]byte{"status": nil, "renew": []byte(`{"ttl_s":60}`)} {
		t0 := time.Now()
		_, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: verb, ID: "box:sandbox/" + id, Args: args}, 30*time.Second)
		if !api.IsCode(err, api.CodeUnavailable) || time.Since(t0) > time.Second {
			t.Errorf("%s to a silent host: %v after %v", verb, err, time.Since(t0))
		}
	}
	// An acquire it may have taken before going quiet is a timeout, not a
	// refusal: the authority keeps the row and says to retry with the same id.
	t0 := time.Now()
	_, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: "acquire", Args: []byte(`{"request_id":"q"}`)}, 30*time.Second)
	if !api.IsCode(err, rt.CodeDriverTimeout) || time.Since(t0) > time.Second {
		t.Errorf("acquire to a silent host: %v after %v", err, time.Since(t0))
	}
}

// A host that is no longer enrolled (it came back as an area guest) gets no
// exec, renew or acquire relayed; its box can still be looked at and ended.
func TestRunTargetUnenrolledHostOnlyEnds(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	g := newBoxRig(t, "member")
	g.advertised(t)
	id := g.box(t, g.ana, "was")
	// The host leaves and comes back as an area guest: unenrolled.
	mustf(g.host.owner.Leave(ctx, "harbor"))(t)
	inv := mustf(g.a.owner.Invite(ctx, api.InviteRequest{Project: "harbor/solver", As: "bea-guest", Role: "guest"}))(t)
	mustf(g.host.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	g.hostTS.Heartbeat(ctx) // still heard from
	if c, err := g.ts.MachineClass(ctx, g.project, "box-host"); err != nil || c.Class != api.MachinePublic || c.SetBy != "" {
		t.Fatalf("class after the downgrade: %+v %v", c, err)
	}
	hid := "box:sandbox/" + id
	for verb, args := range map[string][]byte{"exec": []byte(`{"argv":["echo","x"],"timeout_s":5}`), "renew": []byte(`{"ttl_s":60}`), "acquire": []byte(`{"request_id":"n"}`)} {
		if _, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: verb, ID: hid, Args: args}, 10*time.Second); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("%s relayed to an unenrolled host: %v", verb, err)
		}
	}
	if _, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: "status", ID: hid}, 10*time.Second); err != nil {
		t.Errorf("status on an unenrolled host: %v", err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil || !rel.Released {
		t.Errorf("release on an unenrolled host: %+v %v", rel, err)
	}
}

// A grant downgrade unenrolls a member's default host immediately, without
// leaving, rejoining or waiting for another heartbeat.
func TestRunTargetGrantDowngradeUnenrollsHost(t *testing.T) {
	for _, area := range []string{"", "solver"} {
		name := "project guest"
		if area != "" {
			name = "area guest"
		}
		t.Run(name, func(t *testing.T) {
			quietPresence(t)
			ctx := context.Background()
			g := newBoxRig(t, "member")
			g.advertised(t)
			id := g.box(t, g.ana, "before-downgrade")

			mustf(g.a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: area, Role: "guest"}))(t)
			if area != "" {
				mustf(g.a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
			}
			if c, err := g.ts.MachineClass(ctx, g.project, "box-host"); err != nil || c.Class != api.MachinePublic || c.SetBy != "" || c.FromInvite {
				t.Errorf("class after grant downgrade: %+v %v; want default public", c, err)
			}
			if ads := g.rts.RunTargetAdverts(context.Background(), g.project); len(ads) != 0 {
				t.Errorf("cached adverts after grant downgrade: %+v", ads)
			}
			for _, target := range mustf(computeclient.RunTargets(ctx, g.ana, false))(t).Targets {
				if target.Name == "sandbox@box-host" {
					t.Errorf("unenrolled host still listed: %+v", target)
				}
			}

			hid := "box:sandbox/" + id
			for verb, args := range map[string][]byte{
				"exec":    []byte(`{"argv":["echo","x"],"timeout_s":5}`),
				"renew":   []byte(`{"ttl_s":60}`),
				"acquire": []byte(`{"request_id":"after-downgrade"}`),
			} {
				if _, err := g.rts.RelayRunTarget(ctx, g.project, "box-host", computeapi.RunTargetOp{Binding: "sandbox", Verb: verb, ID: hid, Args: args}, 10*time.Second); !api.IsCode(err, api.CodeNotFound) {
					t.Errorf("%s relayed after grant downgrade: %v; want not_found", verb, err)
				}
			}
			g.hostTS.Heartbeat(ctx)
			if ads := g.rts.RunTargetAdverts(context.Background(), g.project); len(ads) != 0 {
				t.Errorf("host advertised again after grant downgrade: %+v", ads)
			}
			// An unenrolled host's ops ride its heartbeats (its ops poll idles once its
			// access ends), so beat the way the running heartbeat would while they wait.
			beating := g.beatEvery(t, 50*time.Millisecond)
			if _, err := computeclient.RunTargetStatus(ctx, g.ana, "sandbox@box-host", id); err != nil {
				t.Errorf("status after grant downgrade: %v", err)
			}
			if rel, err := computeclient.ReleaseRunTarget(ctx, g.ana, "sandbox@box-host", id); err != nil || !rel.Released {
				t.Errorf("release after grant downgrade: %+v %v", rel, err)
			}
			beating()
		})
	}
}
