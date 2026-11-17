package fulltest

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	. "github.com/sengirunners/potluck/kit/server"
)

// countingTransport counts round trips (one hook = one request).
type countingTransport struct {
	n  atomic.Int32
	rt http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.rt.RoundTrip(r)
}

// TestHookRoute: an agent hook registers its session, reports state and gets
// its own notice line in one request, and a second hook finds the session.
func TestHookRoute(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{repo}}}); err != nil {
		t.Fatal(err)
	}
	reg := orchapi.RegisterRequest{Agent: "claude", RuntimeID: "4bb7545a-1111", Cwd: repo, State: orchapi.StateIdle}
	r, err := orchclient.Hook(ctx, c, orchapi.HookRequest{RegisterRequest: reg, Notice: true})
	if err != nil || r.Session.Address != "mini/4bb7545a" || r.Session.State != orchapi.StateIdle || r.Notice == nil || r.Notice.Line != "" {
		t.Fatalf("first hook: %+v %v", r, err)
	}
	cp := *c
	cp.Project, cp.ProjectFrom = "acme", "flag"
	if _, err := orchclient.Send(ctx, &cp, orchapi.SendMailRequest{To: r.Session.Address, Body: "look at the bug"}); err != nil {
		t.Fatal(err)
	}

	ct := &countingTransport{rt: unixTransport(cfg.SocketPath())}
	hc := client.NewRemote("http://zm", &http.Client{Transport: ct})
	hc.Socket = cfg.SocketPath() // local: the Unix listener marks it
	reg.State = orchapi.StateRunning
	r, err = orchclient.Hook(ctx, hc, orchapi.HookRequest{RegisterRequest: reg, Notice: true})
	if err != nil || r.Session.Address != "mini/4bb7545a" || r.Session.State != orchapi.StateRunning {
		t.Fatalf("prompt hook: %+v %v", r, err)
	}
	if r.Notice == nil || !strings.Contains(r.Notice.Line, "1 unread") || strings.Contains(r.Notice.Line, "look at the bug") {
		t.Fatalf("notice: %+v", r.Notice)
	}
	if n := ct.n.Load(); n != 1 {
		t.Fatalf("%d requests for one hook", n)
	}
	// Prompt and session-start share the delivery set with CLI/MCP polling.
	if r, err = orchclient.Hook(ctx, hc, orchapi.HookRequest{RegisterRequest: reg, Notice: true}); err != nil || r.Notice == nil || r.Notice.Line != "" || r.Notice.Unread != 1 {
		t.Fatalf("repeated hook: %+v %v", r, err)
	}
	asSession := *c
	asSession.As = r.Session.Address
	if n, err := orchclient.HookNotice(ctx, &asSession); err != nil || n.Line != "" || n.Unread != 1 {
		t.Fatalf("CLI/MCP repeated hook notice: %+v %v", n, err)
	}
	// Without notice: none in the answer.
	reg.State = orchapi.StateIdle
	if r, err = orchclient.Hook(ctx, c, orchapi.HookRequest{RegisterRequest: reg}); err != nil || r.Notice != nil || r.Session.State != orchapi.StateIdle {
		t.Fatalf("stop hook: %+v %v", r, err)
	}
	// A token caller is not local: refused like every local route.
	tok, err := c.MintToken(ctx, api.MintTokenRequest{As: "vida", Role: "guest", Project: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	g := client.NewUnix(cfg.SocketPath())
	g.Token = tok.Secret
	if _, err := orchclient.Hook(ctx, g, orchapi.HookRequest{RegisterRequest: reg, Notice: true}); err == nil {
		t.Fatal("a token caller used the hook route")
	}
}

// unixTransport dials the daemon's socket whatever the URL's host.
func unixTransport(sock string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
}

// shortHome is a temp ZM_HOME short enough for a Unix socket path.
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
