package team_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A terminal in a box on another machine: spawn with a
// pty, input and output relayed by the authority to a host joined over the
// tailnet and one joined over the tunnel, the box's echo of each key, a
// resize and a Ctrl-C. It logs the keystroke echo latency on each path.
func TestRunTargetTerminalAcrossMachines(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, _ := tunnelHomeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	ts := runTargetSvc(t, a)
	project := ""
	host := func(machine string, tailnet bool) {
		var d *daemon
		via := api.ViaTunnel
		if tailnet {
			via = api.ViaTailnet
			d = startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + machine, NodeKey: "nodekey:" + machine, Login: "bea@example.com", Host: machine}, discard, roomy)
		} else {
			d = startDaemonWith(t, nil, machine, tailnettest.Identity{}, discard, roomy)
		}
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member", Via: via}))(t)
		j := mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		project = j.Remote.Project.ID
		fix, _ := filepath.EvalSymlinks(t.TempDir())
		state := filepath.Join(fix, "state")
		drv, err := refdriver.Install(fix, state, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		runTargetSvc(t, d).SetLookPath(func(string) (string, error) { return drv, nil })
		mustf(computeclient.BindRunTarget(ctx, d.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"harbor"}, Target: computeapi.RunTarget{
			Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{acrossPin}}}))(t)
		beating(t, d)
	}
	host("tail-host", true)
	host("tun-host", false)
	waitFor(t, "both hosts' boxes advertised", func() bool { return len(ts.RunTargetAdverts(context.Background(), project)) == 2 })
	tok := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "harbor", Role: "member"}))(t)
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "harbor", "flag"

	for _, target := range []string{"sandbox@tail-host", "sandbox@tun-host"} {
		t.Run(target, func(t *testing.T) {
			h, err := computeclient.AcquireRunTarget(ctx, ana, target, computeapi.AcquireRunTargetRequest{RequestID: "t-" + target, TTLS: 120})
			if err != nil {
				t.Fatal(err)
			}
			id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
			sp, err := computeclient.SpawnRunTarget(ctx, ana, target, id, computeapi.SpawnRunTargetRequest{RequestID: "sh", Argv: []string{"sh"},
				PTY: &computeapi.RunTargetPTY{Rows: 24, Cols: 80}})
			if err != nil {
				t.Fatal(err)
			}
			// The output side, as zm sandbox shell runs it: one waiting call after another.
			var mu sync.Mutex
			var seen strings.Builder
			var exit *int
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 20}
				for {
					o, err := computeclient.RunTargetOutput(ctx, ana, target, id, req)
					if err != nil {
						return
					}
					mu.Lock()
					seen.WriteString(o.Stdout)
					exit = o.Exit
					mu.Unlock()
					if !o.Running {
						return
					}
					req.StdoutFrom = o.StdoutNext
				}
			}()
			from := 0
			until := func(want string) {
				t.Helper()
				for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(2 * time.Millisecond) {
					mu.Lock()
					s := seen.String()
					mu.Unlock()
					if i := strings.Index(s[from:], want); i >= 0 {
						from += i + len(want)
						return
					}
					if !time.Now().Before(deadline) {
						t.Fatalf("no %q in %q", want, s[from:])
					}
				}
			}
			in := func(r computeapi.InputRunTargetRequest) {
				t.Helper()
				r.PID = sp.PID
				if _, err := computeclient.InputRunTarget(ctx, ana, target, id, r); err != nil {
					t.Fatal(err)
				}
			}
			in(computeapi.InputRunTargetRequest{Data: "stty size\n"})
			until("24 80")
			var lat []time.Duration
			for _, k := range "abcdefghijklmnop" {
				start := time.Now()
				in(computeapi.InputRunTargetRequest{Data: string(k)})
				until(string(k))
				lat = append(lat, time.Since(start))
			}
			slices.Sort(lat)
			t.Logf("keystroke echo through the authority to %s: p50 %s, p90 %s, max %s", target, lat[len(lat)/2], lat[len(lat)*9/10], lat[len(lat)-1])
			in(computeapi.InputRunTargetRequest{Data: "\x15stty size\n", Rows: 40, Cols: 120})
			until("40 120")
			in(computeapi.InputRunTargetRequest{Data: "sleep 30 && echo slept\n"})
			time.Sleep(300 * time.Millisecond)
			in(computeapi.InputRunTargetRequest{Data: "\x03"})
			time.Sleep(300 * time.Millisecond)
			in(computeapi.InputRunTargetRequest{Data: "exit 5\n"})
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("the shell did not end: %q", seen.String())
			}
			if exit == nil || *exit != 5 || strings.Contains(seen.String(), "\nslept") {
				t.Fatalf("exit %v, want 5 after a Ctrl-C to sleep; terminal %q", exit, seen.String())
			}
			_, _ = computeclient.ReleaseRunTarget(ctx, ana, target, id)
		})
	}
}
