package team_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Secrets and jit_config cross the Cloudflare tunnel only sealed
// (SPEC 17.11 "Sealed secrets", SPEC 20 "Boxes"): a member
// joined over the tunnel seals them to the home's key, and the authority
// seals a relayed op holding them to the key the host's own request
// certified. Every case works over the tunnel and the tailnet alike; a peer
// too old to seal (no certified key) keeps the 403 naming the tailnet; and
// the stand-in for Cloudflare (the proxy, which reads every byte) never
// sees a secret, its name or the jit_config.
func TestRunTargetSecretsCrossTheTunnelOnlySealed(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, cf := tunnelHomeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, roomy)
	wire := &wireTap{}
	cf.Tap(wire)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	ts := runTargetSvc(t, a)

	// Hosts lend a box to harbor: one joined over the tailnet, one over the
	// tunnel, and one over the tunnel running a zm from before sealing.
	project := ""
	host := func(machine string, tailnet, old bool, bug string) {
		var d *daemon
		via := api.ViaTunnel
		if tailnet {
			via = api.ViaTailnet
			d = startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + machine, NodeKey: "nodekey:" + machine, Login: "bea@example.com", Host: machine}, discard, roomy)
		} else {
			d = startDaemonWith(t, nil, machine, tailnettest.Identity{}, discard, roomy)
		}
		if old {
			t.Cleanup(tunnelOf(t, d).SealOff())
		}
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member", Via: via}))(t)
		j := mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		if j.Remote == nil || j.Remote.Via != via {
			t.Fatalf("%s joined: %+v", machine, j.Remote)
		}
		project = j.Remote.Project.ID
		fix, _ := filepath.EvalSymlinks(t.TempDir())
		state := filepath.Join(fix, "state")
		drv, err := refdriver.Install(fix, state, bug)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		runTargetSvc(t, d).SetLookPath(func(string) (string, error) { return drv, nil })
		mustf(computeclient.BindRunTarget(ctx, d.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: computeapi.RunTarget{
			Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin},
			Config: map[string]string{"jit": "1"}}}))(t) // the box starts runners: only a github-jit driver is sent a jit_config
		beating(t, d)
	}
	host("tail-host", true, false, "")
	host("tun-host", false, false, "")
	host("old-host", false, true, "")
	host("echo-host", false, false, "secrets-refuse-echo") // a driver quoting what it was sent in its refusal
	waitFor(t, "every host's box advertised", func() bool { return len(ts.RunTargetAdverts(context.Background(), project)) == 4 })

	// Renters: ana at the authority's door, a member over the tailnet, one over the tunnel.
	tok := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "harbor", Role: "member"}))(t)
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "harbor", "flag"
	tailMem, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor", "vida", "member")
	tunMem, _ := tunnelMember(t, a, "bo-mac", "harbor", "bo", "member")

	const secret = "hunter2hunter2"
	jit := strings.Repeat("QUJD", 8)
	// The command reads the secret without naming it, and answers it
	// upper-cased: its output crosses the tunnel in the clear (outputs are
	// not sealed), so the wire is checked for the secret as sent.
	readSecret := []string{"sh", "-c", `cat "$ZM_SECRETS"/* | tr a-z A-Z`}
	for _, tc := range []struct {
		name    string
		c       *client.Client
		target  string
		ok      bool // secrets and jit_config go
		beat    bool // ops ride the heartbeat answer alone (no poll)
		oldHome bool // the authority runs a zm from before sealing
	}{
		{"door to tailnet host", ana, "sandbox@tail-host", true, false, false},
		{"door to tunnel host", ana, "sandbox@tun-host", true, false, false},
		{"door to tunnel host by heartbeat", ana, "sandbox@tun-host", true, true, false},
		{"tailnet member", at(tailMem.owner, "harbor", ""), "sandbox@tail-host", true, false, false},
		{"tunnel member", at(tunMem.owner, "harbor", ""), "sandbox@tail-host", true, false, false},
		{"tunnel member to tunnel host", at(tunMem.owner, "harbor", ""), "sandbox@tun-host", true, false, false},
		{"tunnel member to tunnel host by heartbeat", at(tunMem.owner, "harbor", ""), "sandbox@tun-host", true, true, false},
		{"door to older tunnel host", ana, "sandbox@old-host", false, false, false},
		{"tunnel member to older tunnel host", at(tunMem.owner, "harbor", ""), "sandbox@old-host", false, false, false},
		{"tunnel member to older home", at(tunMem.owner, "harbor", ""), "sandbox@tail-host", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.beat {
				t.Cleanup(team.OpsPollOff())
			}
			if tc.oldHome {
				t.Cleanup(tunnelOf(t, a).SealOff())
			}
			refused := func(what string, err error) {
				t.Helper()
				if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "too old to seal") || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "API_TOKEN") {
					t.Errorf("%s: want 403 naming the peer as too old to seal (and no secret), got %v", what, err)
				}
			}
			h, err := computeclient.AcquireRunTarget(ctx, tc.c, tc.target, computeapi.AcquireRunTargetRequest{RequestID: strings.ReplaceAll(tc.name, " ", "-") + "-s", TTLS: 120})
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
			if out, err := computeclient.ExecRunTarget(ctx, tc.c, tc.target, id, computeapi.ExecRunTargetRequest{Argv: []string{"echo", "ok"}}); err != nil || out.Stdout != "ok\n" {
				t.Fatalf("secret-free exec: %+v %v", out, err)
			}
			out, err := computeclient.ExecRunTarget(ctx, tc.c, tc.target, id, computeapi.ExecRunTargetRequest{Argv: readSecret, Secrets: map[string]string{"API_TOKEN": secret}})
			if tc.ok {
				if err != nil || out.Stdout != strings.ToUpper(secret) {
					t.Errorf("exec with secrets: %+v %v", out, err)
				}
			} else {
				refused("exec with secrets", err)
			}
			sp, err := computeclient.SpawnRunTarget(ctx, tc.c, tc.target, id, computeapi.SpawnRunTargetRequest{RequestID: "s", Argv: readSecret, Secrets: map[string]string{"API_TOKEN": secret}})
			if tc.ok {
				var o computeapi.RunTargetOutput
				if err == nil {
					o, err = computeclient.RunTargetOutput(ctx, tc.c, tc.target, id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 10})
					for err == nil && o.Running && o.Stdout == "" {
						o, err = computeclient.RunTargetOutput(ctx, tc.c, tc.target, id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 10})
					}
				}
				if err != nil || o.Stdout != strings.ToUpper(secret) {
					t.Errorf("spawn with secrets: the process read %q (%+v %v), want the secret", o.Stdout, sp, err)
				}
			} else {
				refused("spawn with secrets", err)
			}
			j, err := computeclient.AcquireRunTarget(ctx, tc.c, tc.target, computeapi.AcquireRunTargetRequest{RequestID: strings.ReplaceAll(tc.name, " ", "-") + "-j", TTLS: 120, JITConfig: jit})
			if tc.ok {
				if err != nil {
					t.Errorf("acquire with jit_config: %v", err)
				} else {
					_, _ = computeclient.ReleaseRunTarget(ctx, tc.c, tc.target, j.Handle[strings.LastIndex(j.Handle, "/")+1:])
				}
			} else {
				refused("acquire with jit_config", err)
			}
			if _, err := computeclient.ReleaseRunTarget(ctx, tc.c, tc.target, id); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A tunnel host's driver that quotes the secrets in its refusal: the
	// host scrubs it before its report crosses back.
	for _, c := range []*client.Client{ana, at(tunMem.owner, "harbor", "")} {
		h, err := computeclient.AcquireRunTarget(ctx, c, "sandbox@echo-host", computeapi.AcquireRunTargetRequest{RequestID: ids.New(), TTLS: 120})
		if err != nil {
			t.Fatalf("echo-host acquire: %v", err)
		}
		id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
		_, err = computeclient.ExecRunTarget(ctx, c, "sandbox@echo-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"true"}, Secrets: map[string]string{"API_TOKEN": secret}})
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "API_TOKEN") {
			t.Errorf("echo-host exec: want a scrubbed refusal, got %v", err)
		}
		_, _ = computeclient.ReleaseRunTarget(ctx, c, "sandbox@echo-host", id)
	}
	// What Cloudflare read: the tunnel's traffic, op arguments in the
	// clear where they hold no secret, sealed where they do, and never a
	// secret, its name or a jit_config.
	if !wire.has(`"echo","ok"`) || !wire.has(`"sealed":"zs1.`) || !wire.has(`"zm_sealed":"zs1.`) {
		t.Fatal("the tap did not see the tunnel's traffic (secret-free op arguments, sealed ops and sealed requests)")
	}
	for _, leak := range []string{secret, jit, "API_TOKEN"} {
		if wire.has(leak) {
			t.Errorf("%q crossed the tunnel in the clear", leak)
		}
	}
}

// wireTap holds every byte the stand-in for Cloudflare carried.
type wireTap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wireTap) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *wireTap) has(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Contains(w.buf.Bytes(), []byte(s))
}
