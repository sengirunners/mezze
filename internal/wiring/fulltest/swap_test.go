package fulltest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
)

// TestPreflightRefusesBuildMissingRoute: a running daemon's manifest
// against a build like it but without GET /v0/issues: refused while that
// route was answered, and while a project is joined (it is forwarded).
func TestPreflightRefusesBuildMissingRoute(t *testing.T) {
	ctx := context.Background()
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	c := client.WithHandler(dm.SocketHandler())
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	_, _ = orchclient.Issues(ctx, c, orchapi.IssueQuery{Project: "harbor", Limit: 1}) // answered, whatever it says
	m, err := c.Manifest(ctx)                                                         // m.Build is the daemon's own build manifest
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Used, "GET /v0/issues") || !slices.Contains(m.Build.Proxied, "GET /v0/issues") || m.Exe == "" {
		t.Fatalf("manifest: used %v, proxied has GET /v0/issues: %v, exe %q", m.Used, slices.Contains(m.Build.Proxied, "GET /v0/issues"), m.Exe)
	}
	for _, p := range swap.LegacyProxied {
		if !slices.Contains(m.Build.Proxied, p) {
			t.Errorf("this build does not forward %s, which the legacy check requires", p)
		}
	}
	h, _ := c.Health(ctx)
	run := &swap.Running{Health: h, Manifest: &m}
	same := swap.Check(m.Build, run, nil, nil)
	if !same.OK {
		t.Fatalf("the same build must pass: %+v", same)
	}
	nu := m.Build
	nu.Routes = slices.DeleteFunc(slices.Clone(nu.Routes), func(p string) bool { return p == "GET /v0/issues" })
	nu.Proxied = slices.DeleteFunc(slices.Clone(nu.Proxied), func(p string) bool { return p == "GET /v0/issues" })
	pf := swap.Check(nu, run, nil, nil)
	if pf.OK || !strings.Contains(strings.Join(pf.Problems, "\n"), "GET /v0/issues was answered since the daemon started; this build lacks it") {
		t.Fatalf("want refused for the missing route: %+v", pf)
	}
	m.Joined, m.Used = []string{"harbor"}, nil
	pf = swap.Check(nu, run, nil, nil)
	if pf.OK || !strings.Contains(strings.Join(pf.Problems, "\n"), "joined projects (harbor): the running build forwards GET /v0/issues") {
		t.Fatalf("want refused for the forward: %+v", pf)
	}
	// ThisBuild (what `zm daemon preflight` uses) lists the same routes.
	tb, err := ThisBuild(ctx, wiring.Full)
	if err != nil || !slices.Equal(tb.Routes, m.Build.Routes) || !slices.Equal(tb.Proxied, m.Build.Proxied) {
		t.Fatalf("ThisBuild: %v", err)
	}
}
