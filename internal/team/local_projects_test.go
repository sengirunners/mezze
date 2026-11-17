package team_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Hold a real joined authority's project read, not its connectivity bit.
// Local metadata must complete while ordinary discovery remains in flight.
func TestLocalProjectsDoesNotWaitForJoinedDiscovery(t *testing.T) {
	quietPresence(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, home := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	target, err := url.Parse(home.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	entered, release := make(chan struct{}), make(chan struct{})
	var projectID string
	var armed atomic.Bool
	var seen, released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if armed.Load() && req.Method == http.MethodGet && req.URL.Path == "/v0/projects/"+projectID {
			seen.Do(func() { close(entered) })
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, req)
	}))
	// Cleanups run last first: the member daemon (started below) stops
	// before the edge closes, so its long-polling sync through the edge never
	// holds edge.Close. Handlers are unblocked first, even on failure.
	t.Cleanup(edge.Close)
	defer unblock()
	mustf(a.owner.TunnelUp(ctx, api.TunnelUpRequest{Listen: tunnelOf(t, a).Status().Listen, URL: edge.URL}))(t)
	r, joined := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	projectID = joined.Remote.Project.ID
	waitCopyOK(t, r)
	local := mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "local-visible"}))(t)
	mustf(r.owner.AddArea(ctx, local.ID, api.Area{Name: "solver"}))(t)
	mustf(r.owner.AddArea(ctx, local.ID, api.Area{Name: "billing"}))(t)
	hidden := mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "local-hidden"}))(t)
	token := mustf(r.owner.MintToken(ctx, api.MintTokenRequest{As: "viewer", Project: local.ID, Area: "solver", Role: "guest"}))(t)
	scoped := client.WithHandler(r.dm.Handler)
	scoped.Token = token.Secret

	armed.Store(true)
	type result struct {
		list api.ProjectList
		err  error
	}
	full := make(chan result, 1)
	go func() { list, err := r.owner.ProjectList(ctx); full <- result{list, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("ordinary discovery never reached authority", ctx.Err())
	}
	locals, err := r.owner.LocalProjects(ctx)
	if err != nil || len(locals) != 2 {
		t.Fatalf("local metadata: %+v, %v", locals, err)
	}
	ids := map[string]bool{}
	for _, p := range locals {
		ids[p.ID] = true
	}
	if !ids[local.ID] || !ids[hidden.ID] || ids[joined.Remote.Project.ID] {
		t.Fatalf("local inventory: %+v", locals)
	}
	visible, err := scoped.LocalProjects(ctx)
	if err != nil || len(visible) != 1 || visible[0].ID != local.ID || len(visible[0].Areas) != 1 || visible[0].Areas[0].Name != "solver" {
		t.Fatalf("scoped local metadata leaked/omitted project or area: %+v, %v", visible, err)
	}
	select {
	case got := <-full:
		t.Fatalf("discovery finished before release: %+v", got)
	default:
	}
	unblock()
	select {
	case got := <-full:
		if got.err != nil || len(got.list.Skipped) != 0 || len(got.list.Projects) != 3 {
			t.Fatalf("ordinary discovery: %+v, %v", got.list, got.err)
		}
		found := false
		for _, p := range got.list.Projects {
			if p.ID == joined.Remote.Project.ID && p.Slug == "harbor" {
				found = true
			}
		}
		if !found {
			t.Fatalf("ordinary discovery lost joined project: %+v", got.list)
		}
	case <-ctx.Done():
		t.Fatal("discovery did not finish after release", ctx.Err())
	}
}
