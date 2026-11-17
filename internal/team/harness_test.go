package team_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// daemon is one zm daemon on this machine, serving its Unix socket and a
// node on the loopback tailnet.
type daemon struct {
	t     *testing.T
	dm    *server.Daemon
	cfg   *config.Config
	owner *client.Client // the machine's owner on its socket (in-process)
	sock  *client.Client // the same over the real Unix socket
	stop  func()
	halt  func() // stops the daemon, keeping its home
}

// startDaemon starts a daemon for machine (human ivo unless given) on
// tn with identity id, and brings its tailnet node up.
func startDaemon(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity) *daemon {
	t.Helper()
	return startDaemonLog(t, tn, machine, id, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// startDaemonLog is startDaemon logging to log.
func startDaemonLog(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, log *slog.Logger) *daemon {
	t.Helper()
	return startDaemonWith(t, tn, machine, id, log, nil)
}

// startDaemonWith is startDaemonLog with mut applied to the config first.
func startDaemonWith(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, log *slog.Logger, mut func(*config.Config)) *daemon {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	if mut != nil {
		mut(cfg)
	}
	d := &daemon{t: t, cfg: cfg}
	d.boot(tn, id, log)
	var once sync.Once
	d.stop = func() {
		once.Do(func() {
			d.halt()
			os.RemoveAll(home)
		})
	}
	t.Cleanup(d.stop)
	return d
}

// boot serves d.cfg's home with a new daemon (its node on tn as id).
func (d *daemon) boot(tn *tailnettest.Net, id tailnettest.Identity, log *slog.Logger) {
	t := d.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, d.cfg, log, wiring.Full)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			if tn != nil {
				s.NewNode = tn.NodeFunc(id)
			} else {
				// No tailnet: still never a real tsnet node in a test.
				s.NewNode = tailnettest.New().NodeFunc(tailnettest.Identity{NodeID: "nNONE-" + d.cfg.Machine})
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	d.dm, d.owner = dm, client.WithHandler(dm.SocketHandler())
	d.halt = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", d.cfg.Machine)
		}
	}
	// The socket answers once Serve listens.
	d.sock = client.NewUnix(d.cfg.SocketPath())
	for i := 0; ; i++ {
		if _, err := d.sock.Health(ctx); err == nil {
			break
		}
		if i > 400 {
			t.Fatalf("%s: socket never answered", d.cfg.Machine)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tn == nil {
		return // no tailnet at all (a tunnel-only machine, SPEC 17.11)
	}
	st, err := d.owner.TailnetUp(ctx, api.TailnetUpRequest{})
	if err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", d.cfg.Machine, st, err)
	}
}

// restart stops d's daemon and serves the same home with a new one, as a
// machine's daemon restarting does (its managed sessions end).
func (d *daemon) restart(tn *tailnettest.Net, id tailnettest.Identity) {
	d.t.Helper()
	d.halt()
	d.boot(tn, id, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// mustf(f())(t) is f's value, failing t on its error.
func mustf[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// at is c narrowed to a project and acting as address as.
func at(c *client.Client, project, as string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom, x.As = project, "flag", as
	return &x
}

// authority starts "mini" with project harbor (areas solver, billing) as a
// team project.
func authority(t *testing.T, tn *tailnettest.Net) *daemon {
	t.Helper()
	ctx := context.Background()
	a := startDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Remotes: []string{"github.com/example-org/harbor"}}}))(t)
	for _, ar := range []string{"solver", "billing"} {
		mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: ar, Work: []string{"local-" + ar + "-*"}}))(t)
	}
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	return a
}

// replica starts machine with node id nodeID and joins it with an invite
// for as/role (project may be slug/area).
func replica(t *testing.T, tn *tailnettest.Net, a *daemon, machine, nodeID, proj, as, role string) (*daemon, api.JoinResponse) {
	t.Helper()
	ctx := context.Background()
	r := startDaemon(t, tn, machine, tailnettest.Identity{NodeID: nodeID, NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: proj, As: as, Role: role}))(t)
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if j.Remote == nil {
		t.Fatalf("join: %+v", j)
	}
	return r, j
}

// tailnetNode starts a bare node (no daemon) on tn.
func tailnetNode(t *testing.T, tn *tailnettest.Net, id tailnettest.Identity, host string) tailnet.Node {
	t.Helper()
	n := tn.NodeFunc(id)(tailnet.NodeConfig{Hostname: host})
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// tailnetClientAs is an HTTP client dialling from machine's daemon's node.
func tailnetClientAs(t *testing.T, tn *tailnettest.Net, machine string) *http.Client {
	t.Helper()
	n := tn.Node(api.NodePrefixNew + machine)
	if n == nil {
		t.Fatalf("no node %s%s", api.NodePrefixNew, machine)
	}
	return n.HTTPClient()
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// eventually retries f until it returns nil or d passes (round C: a change
// reaches a replica's copy through sync, within the propagation budget).
func eventually(t *testing.T, d time.Duration, f func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
