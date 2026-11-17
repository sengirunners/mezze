package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/repoguard"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runtarget"
)

func TestMain(m *testing.M) {
	// Tests never read this machine's swap: the authority gives box secrets only to a host whose
	// swap is safe, and a plain-disk swap here would turn every secrets test into a refusal.
	hostswap.Local = func(context.Context) hostswap.Reading { return hostswap.Reading{State: hostswap.Off} }
	if code, ok := refdriver.Main(); ok { // the reference driver runs as this binary
		os.Exit(code)
	}
	os.Exit(repoguard.Main(m)) // fails the run if a test rewrote the git config of the repository around the package (zm-jmep)
}

const acrossPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func runTargetSvc(t *testing.T, d *daemon) *runtarget.Service {
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			return s
		}
	}
	t.Fatal("no run target module")
	return nil
}

// Boxes across machines (SPEC 20 "Across machines"): a host
// replica advertises the box bindings it shares with the project; the
// authority lists them as <binding>@<machine> and relays the driver verbs.
func TestRunTargetBoxesAcrossMachines(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	r := startDaemonWith(t, tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "bea@example.com", Host: "box-host"}, discard, roomy)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)

	// The host: the reference driver, two machine-level bindings, one shared
	// with harbor and one with another project.
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	// The host's driver keeps each request it is sent, then answers as refdriver.
	seen := filepath.Join(fix, "requests")
	tap := filepath.Join(fix, "zm-run-tap")
	if err := os.WriteFile(tap, []byte(fmt.Sprintf("#!/bin/sh\ntee -a %q | exec %q\n", seen, drv)), 0o755); err != nil {
		t.Fatal(err)
	}
	runTargetSvc(t, r).SetLookPath(func(string) (string, error) { return tap, nil })
	sentNet := func(key string) string { // the net the host's driver was sent with this request_id
		b, _ := os.ReadFile(seen)
		dec := json.NewDecoder(strings.NewReader(string(b))) // one envelope per call, newline or not
		for {
			var env struct {
				Verb string `json:"verb"`
				Args struct {
					RequestID string `json:"request_id"`
					Net       string `json:"net"`
				} `json:"args"`
			}
			if dec.Decode(&env) != nil {
				break
			}
			if env.Verb == "acquire" && strings.HasSuffix(env.Args.RequestID, "-"+key) {
				return env.Args.Net
			}
		}
		return "(not sent)"
	}
	bind := func(name string, share ...string) {
		_, err := computeclient.BindRunTarget(ctx, r.owner, computeapi.BindRunTargetRequest{Host: true, Share: share, Target: computeapi.RunTarget{
			Name: name, Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin},
			Config: map[string]string{"net_off": "1"}}}) // the driver honours net (it lists net-off)
		if err != nil {
			t.Fatalf("bind %s: %v", name, err)
		}
	}
	bind("sandbox", "harbor")
	bind("private", ids.New()) // a project id this machine is not in
	if info, err := os.Stat(filepath.Join(r.cfg.Home, runtarget.HostFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("run_targets.toml: %v %v", info, err)
	}
	if _, err := computeclient.BindRunTarget(ctx, a.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: computeapi.RunTarget{
		Name: "x", Driver: "ref", Kind: "run", Isolation: "ephemeral"}}); err == nil {
		t.Error("a machine-level run binding was accepted")
	}

	var mu sync.Mutex
	var ended []computeapi.RunTargetHandle
	runTargetSvc(t, a).OnEnd(func(_ context.Context, _ string, h computeapi.RunTargetHandle) {
		mu.Lock()
		ended = append(ended, h)
		mu.Unlock()
	})
	tok := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "harbor", Role: "member"}))(t)
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "harbor", "flag"
	beating(t, r)

	var view computeapi.RunTargetView
	waitFor(t, "the host's box listed at the authority", func() bool {
		l, err := computeclient.RunTargets(ctx, ana, true)
		if err != nil {
			return false
		}
		for _, v := range l.Targets {
			if v.Name == "sandbox@box-host" {
				view = v
			}
			if strings.HasPrefix(v.Name, "private") {
				t.Fatalf("an unshared binding was advertised: %+v", v)
			}
		}
		return view.Name != ""
	})
	if view.Host != "box-host" || view.HostClass != api.MachineTrusted || view.Describe == nil || view.Describe.Size == nil || !view.Describe.Available ||
		view.Effective.Isolation != "ephemeral" || view.Effective.Credentials || len(view.Config) != 0 {
		t.Fatalf("remote view: %+v", view)
	}
	if _, err := computeclient.DescribeRunTarget(ctx, ana, "sandbox@box-host"); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if _, err := computeclient.DescribeRunTarget(ctx, ana, "private@box-host"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("describe of an unshared binding: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "private@box-host", computeapi.AcquireRunTargetRequest{RequestID: "p1"}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("acquire of an unshared binding: %v", err)
	}
	// A relay for it is refused on the host too, as a binding that is not there.
	ts := runTargetSvc(t, a)
	_, err = ts.RelayRunTarget(ctx, j.Remote.Project.ID, "box-host", computeapi.RunTargetOp{Binding: "private", Verb: "acquire"}, 20*time.Second)
	if !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a relay for an unshared binding: %v", err)
	}

	// Acquire, exec, release: the authority's rows and hook, the host's driver.
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "b1", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !strings.HasPrefix(h.Handle, "box:sandbox@box-host/") || h.State != "ready" || h.Size != "micro" {
		t.Fatalf("handle: %+v", h)
	}
	// A trusted host's box gets the binding's filtered network.
	if n := sentNet("b1"); n != "on" {
		t.Errorf("a trusted host's box was sent net %q, want on", n)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "echo hi"}})
	if err != nil || out.Exit != 0 || out.Stdout != "hi\n" {
		t.Fatalf("exec: %+v %v", out, err)
	}
	bo := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "harbor", Role: "member"}))(t)
	other := client.WithHandler(a.dm.Handler)
	other.Token, other.Project, other.ProjectFrom = bo.Secret, "harbor", "flag"
	// A background process on the host, relayed: spawn, a page while it
	// runs, a wait for more, TERM to its group.
	sp, err := computeclient.SpawnRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.SpawnRunTargetRequest{RequestID: "s1",
		Argv: []string{"sh", "-c", `trap : TERM; echo one; sleep 30 & wait; wait; echo two; exit 3`}})
	if err != nil || sp.PID == "" {
		t.Fatalf("relayed spawn: %+v %v", sp, err)
	}
	o, err := computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 5})
	if err != nil || o.Stdout != "one\n" || !o.Running || o.StdoutNext != 4 {
		t.Fatalf("relayed output while running: %+v %v", o, err)
	}
	if s, err := computeclient.SignalRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.SignalRunTargetRequest{PID: sp.PID, Signal: "TERM"}); err != nil || !s.Sent {
		t.Fatalf("relayed signal: %+v %v", s, err)
	}
	if o, err = computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: 4, WaitS: 10}); err == nil && o.Running {
		o, err = computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: sp.PID, StdoutFrom: o.StdoutNext, WaitS: 10})
	}
	if err != nil || o.Running || o.Exit == nil || *o.Exit != 3 || !strings.HasSuffix(o.Stdout, "two\n") {
		t.Fatalf("relayed output after TERM: %+v %v", o, err)
	}
	if _, err := computeclient.SignalRunTarget(ctx, other, "sandbox@box-host", id, computeapi.SignalRunTargetRequest{PID: sp.PID, Signal: "KILL"}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("relayed signal by another member: %v", err)
	}
	// A relayed page stays within the host's answer limit: max_bytes is cut
	// to 256 KiB there, and stdout_next says where to go on.
	big, err := computeclient.SpawnRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.SpawnRunTargetRequest{RequestID: "s2",
		Argv: []string{"awk", `BEGIN { for (i = 0; i < 50000; i++) printf "%07d\n", i }`}})
	if err != nil {
		t.Fatal(err)
	}
	for ; ; time.Sleep(100 * time.Millisecond) {
		if o, err = computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: big.PID, MaxBytes: 1}); err != nil || !o.Running {
			break
		}
	}
	o, err = computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: big.PID, MaxBytes: 1 << 20})
	if err != nil || len(o.Stdout) != 256<<10 || o.StdoutNext != 256<<10 || !strings.HasPrefix(o.Stdout, "0000000\n") {
		t.Errorf("a relayed 1 MiB page: %d bytes, next %d, %v; want 256 KiB", len(o.Stdout), o.StdoutNext, err)
	}
	hs, err := computeclient.RunTargetHandles(ctx, ana, "sandbox@box-host", true)
	if err != nil || len(hs) != 1 || hs[0].Handle != h.Handle {
		t.Fatalf("live handles: %+v %v", hs, err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, other, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"id"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("exec by another member: %v", err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}
	st, err := computeclient.RunTargetStatus(ctx, ana, "sandbox@box-host", id)
	if err != nil || st.State != "released" || st.Ended == "" {
		t.Fatalf("status after release: %+v %v", st, err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(ended) != 1 || ended[0].Handle != h.Handle || ended[0].State != "released" {
		t.Errorf("end hook: %+v", ended)
	}
	mu.Unlock()

	// A kept disk on a trusted host: stop and start relayed, the same files back.
	kh, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "k1", TTLS: 120, Persist: true})
	if err != nil || !kh.Persist {
		t.Fatalf("persist on a trusted host: %+v %v", kh, err)
	}
	kid := kh.Handle[strings.LastIndex(kh.Handle, "/")+1:]
	mustf(computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", kid, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", `echo kept > "$HOME/f"`}}))(t)
	if st, err := computeclient.StopRunTarget(ctx, ana, "sandbox@box-host", kid); err != nil || st.State != computeapi.StateStopped {
		t.Fatalf("relayed stop: %+v %v", st, err)
	}
	if _, err := computeclient.StartRunTarget(ctx, other, "sandbox@box-host", kid, 0); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("relayed start by another member: %v", err)
	}
	if st, err := computeclient.StartRunTarget(ctx, ana, "sandbox@box-host", kid, 120); err != nil || st.State != "ready" {
		t.Fatalf("relayed start: %+v %v", st, err)
	}
	if out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", kid, computeapi.ExecRunTargetRequest{Argv: []string{"cat", "f"}}); err != nil || out.Stdout != "kept\n" {
		t.Fatalf("the kept file after a relayed start: %+v %v", out, err)
	}
	mustf(computeclient.StopRunTarget(ctx, ana, "sandbox@box-host", kid))(t)

	// A public host: no secrets, no jit_config; its class is on the view.
	mustf(a.owner.SetMachineClass(ctx, "box-host", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}))(t)
	l := mustf(computeclient.RunTargets(ctx, ana, false))(t)
	for _, v := range l.Targets {
		if v.Name == "sandbox@box-host" && v.HostClass != api.MachinePublic {
			t.Errorf("host_class after public: %+v", v)
		}
	}
	h2, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "b2", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire on a public host: %v", err)
	}
	// ... and a public host's none: no egress allowlist yet (SPEC 20 open question 6).
	if n := sentNet("b2"); n != "off" {
		t.Errorf("a public host's box was sent net %q, want off", n)
	}
	id2 := h2.Handle[strings.LastIndex(h2.Handle, "/")+1:]
	_, err = computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id2, computeapi.ExecRunTargetRequest{Argv: []string{"id"}, Secrets: map[string]string{"API_TOKEN": "hunter2hunter2"}})
	if !api.IsCode(err, api.CodeForbidden) || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("secrets on a public host: %v", err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "b3", JITConfig: strings.Repeat("QUJD", 8)})
	if !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("jit_config on a public host: %v", err)
	}
	if out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id2, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "ok"}}); err != nil || out.Stdout != "ok\n" {
		t.Errorf("secret-free exec on a public host: %+v %v", out, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id2); err != nil {
		t.Fatal(err)
	}
	// ... and no kept disk: persist is refused, and a disk kept while it was trusted is not started (release deletes it).
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "k2", Persist: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("persist on a public host: %v", err)
	}
	if _, err := computeclient.StartRunTarget(ctx, ana, "sandbox@box-host", kid, 0); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("start of a kept disk on a host now public: %v", err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", kid); err != nil || !rel.Released {
		t.Fatalf("release of a stopped box on a public host: %+v %v", rel, err)
	}

	// Unsharing drops the advert at the authority on the next beat.
	if _, err := computeclient.BindRunTarget(ctx, r.owner, computeapi.BindRunTargetRequest{Host: true, Replace: true, Share: []string{ids.New()}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the unshared binding dropped", func() bool {
		return len(ts.RunTargetAdverts(context.Background(), j.Remote.Project.ID)) == 0
	})
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "b4"}); !api.IsCode(err, api.CodeUnavailable) { // its handles keep the name resolvable, but it advertises nothing
		t.Errorf("acquire after unsharing: %v", err)
	}
}

// A lent Mac takes macOS guests for others (cleared,
// by the owners): a darwin box one pool member
// lends serves another member, and a runner for an Actions job of a repo in
// an org that is neither the lender's nor the pool's. Nothing asks whose work
// a macOS guest does; the host's checks (trusted, jit only on trusted) are the
// same as for Linux. Per-minute Mac rental and pooled Simulator time are
// Sengi's paid layer, not built here.
func TestLentMacServesOtherMembersAndCrossOrgJobs(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "pool"))(t)
	mac := startDaemonWith(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nMAC", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"}, discard, roomy)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "pool", As: "bea", Role: "member"}))(t).Code}))(t)

	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	seen := filepath.Join(fix, "requests")
	tap := filepath.Join(fix, "zm-run-tap")
	if err := os.WriteFile(tap, []byte(fmt.Sprintf("#!/bin/sh\ntee -a %q | exec %q\n", seen, drv)), 0o755); err != nil {
		t.Fatal(err)
	}
	runTargetSvc(t, mac).SetLookPath(func(string) (string, error) { return tap, nil })
	if _, err := computeclient.BindRunTarget(ctx, mac.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"pool"}, Target: computeapi.RunTarget{
		Name: "mac", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin},
		Config: map[string]string{"os": "darwin", "size": "c1m2", "jit": "1", "net_off": "1"}}}); err != nil {
		t.Fatal(err)
	}
	beating(t, mac)
	tok := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "pool", Role: "member"}))(t)
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "pool", "flag"
	var view computeapi.RunTargetView
	waitFor(t, "the lent Mac listed at the authority", func() bool {
		l, err := computeclient.RunTargets(ctx, ana, true)
		if err != nil {
			return false
		}
		for _, v := range l.Targets {
			if v.Name == "mac@bea-mac" {
				view = v
			}
		}
		return view.Name != ""
	})
	if view.HostClass != api.MachineTrusted || view.Describe == nil || view.Describe.Size == nil || view.Describe.Size.OS != "darwin" {
		t.Fatalf("the lent Mac's view: %+v", view)
	}

	// Another pool member's macOS guest.
	h, err := computeclient.AcquireRunTarget(ctx, ana, "mac@bea-mac", computeapi.AcquireRunTargetRequest{RequestID: "g1", TTLS: 120})
	if err != nil || h.State != "ready" {
		t.Fatalf("a macOS guest for another member: %+v %v", h, err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	if out, err := computeclient.ExecRunTarget(ctx, ana, "mac@bea-mac", id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "hi"}}); err != nil || out.Stdout != "hi\n" {
		t.Fatalf("exec in the other member's macOS guest: %+v %v", out, err)
	}
	mustf(computeclient.ReleaseRunTarget(ctx, ana, "mac@bea-mac", id))(t)

	// A runner for a job of another org's repo, as the broker places it.
	jh, err := computeclient.AcquireRunTarget(ctx, ana, "mac@bea-mac", computeapi.AcquireRunTargetRequest{RequestID: "j1", TTLS: 120,
		JITConfig: strings.Repeat("QUJD", 8), JITGuard: &computeapi.JITGuard{Repo: "other-org/app", Events: []string{"push"}}})
	if err != nil || jh.State != "ready" {
		t.Fatalf("a cross-org Actions runner on the lent Mac: %+v %v", jh, err)
	}
	b, _ := os.ReadFile(seen)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	guard := ""
	for {
		var env struct {
			Verb string `json:"verb"`
			Args struct {
				RequestID string               `json:"request_id"`
				JITGuard  *computeapi.JITGuard `json:"jit_guard"`
			} `json:"args"`
		}
		if dec.Decode(&env) != nil {
			break
		}
		if env.Verb == "acquire" && strings.HasSuffix(env.Args.RequestID, "-j1") && env.Args.JITGuard != nil {
			guard = env.Args.JITGuard.Repo
		}
	}
	if guard != "other-org/app" {
		t.Errorf("the Mac's driver was sent jit_guard repo %q, want other-org/app", guard)
	}
	mustf(computeclient.ReleaseRunTarget(ctx, ana, "mac@bea-mac", jh.Handle[strings.LastIndex(jh.Handle, "/")+1:]))(t)
}

// A member's own replica reaches the authority's run targets through its
// socket; bind and unbind stay local and are refused for a joined project.
func TestRunTargetsFromMemberReplica(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	host := startDaemonWith(t, tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "bea@example.com", Host: "box-host"}, discard, roomy)
	mustf(host.owner.Join(ctx, api.JoinRequest{Code: mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t).Code}))(t)
	mem := startDaemonWith(t, tn, "ana-mac", tailnettest.Identity{NodeID: "nANA", NodeKey: "nodekey:ana-mac", Login: "ana@example.com", Host: "ana-mac"}, discard, roomy)
	mustf(mem.owner.Join(ctx, api.JoinRequest{Code: mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "ana", Role: "member"}))(t).Code}))(t)

	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	runTargetSvc(t, host).SetLookPath(func(string) (string, error) { return drv, nil })
	if _, err := computeclient.BindRunTarget(ctx, host.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}}); err != nil {
		t.Fatal(err)
	}
	beating(t, host)
	ana := at(mem.owner, "harbor", "")
	waitFor(t, "the host's box listed through the member's socket", func() bool {
		l, err := computeclient.RunTargets(ctx, ana, true)
		if err != nil {
			return false
		}
		for _, v := range l.Targets {
			if v.Name == "sandbox@box-host" {
				return true
			}
		}
		return false
	})
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "m1", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "echo hi"}})
	if err != nil || out.Stdout != "hi\n" {
		t.Fatalf("exec: %+v %v", out, err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}
	// Bind and unbind are for the machine that serves the binding.
	bad := computeapi.RunTarget{Name: "x", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}
	if _, err := computeclient.BindRunTarget(ctx, ana, computeapi.BindRunTargetRequest{Target: bad}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "serves them") {
		t.Errorf("bind for a joined project: %v", err)
	}
	if err := computeclient.UnbindRunTarget(ctx, ana, "x"); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("unbind for a joined project: %v", err)
	}
	// zm target bind --host from the joined project's folder (host in the
	// body) is the host machine's own bind.
	if _, err := computeclient.BindRunTarget(ctx, at(host.owner, "harbor", ""), computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: bad}); err != nil {
		t.Errorf("bind --host for a joined project: %v", err)
	}
	if err := computeclient.UnbindHostRunTarget(ctx, at(host.owner, "harbor", ""), "x"); err != nil {
		t.Errorf("unbind --host for a joined project: %v", err)
	}
}
