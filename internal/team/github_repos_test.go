package team_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// GET /v0/github/repos on a replica lists a joined project's repository
// mappings from its authority's header (no call to the authority): the
// project's remotes for a whole-project member, only the visible area's for
// an area member. Dogfood: the cockpit's PR and CI tabs said "No GitHub
// repositories." on a Mac whose projects were all joined.
func TestJoinedProjectGitHubRepositories(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.UpdateArea(ctx, "harbor", "solver", api.UpdateAreaRequest{Remotes: &[]string{"github.com/example-org/solver"}}))(t)
	mustf(a.owner.UpdateArea(ctx, "harbor", "billing", api.UpdateAreaRequest{Remotes: &[]string{"github.com/example-org/billing"}}))(t)

	type want struct{ area, repo string }
	check := func(name string, got []orchapi.GitHubRepository, id string, ws ...want) {
		t.Helper()
		if len(got) != len(ws) {
			t.Fatalf("%s: %+v, want %v", name, got, ws)
		}
		for i, w := range ws {
			g := got[i]
			if g.Project != id || g.Slug != "harbor" || g.Area != w.area || g.Repo != w.repo {
				t.Fatalf("%s: %+v, want %v", name, got, ws)
			}
		}
	}

	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	heartbeat(t, r)
	id := j.Remote.Project.ID
	all := []want{{"billing", "example-org/billing"}, {"", "example-org/harbor"}, {"solver", "example-org/solver"}}
	for _, name := range []string{"", "harbor", id} {
		got := mustf(orchclient.GitHubRepositories(ctx, r.sock, name))(t)
		check("member "+name, got.Repositories, id, all...)
	}

	g, jg := replica(t, tn, a, "pat-mac", "nPAT", "harbor/solver", "pat", "member")
	heartbeat(t, g)
	got := mustf(orchclient.GitHubRepositories(ctx, g.sock, ""))(t)
	check("area member", got.Repositories, jg.Remote.Project.ID, want{"solver", "example-org/solver"})
}
