package team_test

import (
	"context"
	"slices"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A project's repo list (SPEC 18.5) is project config at the
// authority: every member machine reads the same list, an owner sets it from
// a member machine, and the replica keeps no copy of its own.
func TestRepoListIsProjectConfigAtAuthority(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	mustf(a.owner.UpdateArea(ctx, "harbor", "solver", api.UpdateAreaRequest{Remotes: &[]string{"git@github.com:example-org/Solver.git", "gitlab.com/acme/other"}}))(t)

	adm, j := replica(t, tn, a, "repo-admin", "nREPOADMIN", "harbor", "bea", "member")
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t) // owners are granted after they join
	c := at(adm.owner, "harbor", "")

	// Never set: the authority's GitHub remotes (the project's, then its areas').
	l := mustf(orchclient.GitHubRepoList(ctx, c, "harbor"))(t)
	if l.Source != orchapi.RepoListRemotes || !slices.Equal(l.Repos, []string{"example-org/harbor", "example-org/solver"}) || l.Updated != nil || l.ID != j.Remote.Project.ID {
		t.Fatalf("remotes from a member: %+v", l)
	}

	want := []string{"example-org/harbor", "sengirunners/potluck"}
	set := mustf(orchclient.SetGitHubRepoList(ctx, c, "harbor", &[]string{"example-org/Harbor", "https://github.com/sengirunners/potluck.git", "EXAMPLE-ORG/harbor"}))(t)
	if set.Source != orchapi.RepoListSet || !slices.Equal(set.Repos, want) || set.Updated == nil || set.Project != "harbor" {
		t.Fatalf("owner set from a member: %+v", set)
	}
	if l := mustf(orchclient.GitHubRepoList(ctx, ao, "harbor"))(t); !slices.Equal(l.Repos, want) || l.Source != orchapi.RepoListSet {
		t.Fatalf("authority's list: %+v", l)
	}
	var rows int
	if err := adm.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM ghlink_repo_lists`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("replica kept its own list: %d %v", rows, err)
	}
	if _, err := orchclient.SetGitHubRepoList(ctx, c, "harbor", &[]string{"not a repo"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad repo: %v", err)
	}

	// A plain member reads the same list but may not set it; an area guest
	// sees no project-wide list.
	m, _ := replica(t, tn, a, "repo-member", "nREPOMEMBER", "harbor", "dana", "member")
	mc := at(m.owner, "harbor", "")
	if l := mustf(orchclient.GitHubRepoList(ctx, mc, "harbor"))(t); !slices.Equal(l.Repos, want) {
		t.Fatalf("member read: %+v", l)
	}
	if _, err := orchclient.SetGitHubRepoList(ctx, mc, "harbor", &[]string{"acme/evil"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("member set the list: %v", err)
	}
	g, _ := replica(t, tn, a, "repo-guest", "nREPOGUEST", "harbor/solver", "reader", "guest")
	if _, err := orchclient.GitHubRepoList(ctx, at(g.owner, "harbor", ""), "harbor"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("area guest read the project's list: %v", err)
	}

	// An empty list is a list; reset goes back to the remotes.
	if l := mustf(orchclient.SetGitHubRepoList(ctx, c, "harbor", &[]string{}))(t); len(l.Repos) != 0 || l.Repos == nil || l.Source != orchapi.RepoListSet {
		t.Fatalf("empty list: %+v", l)
	}
	if l := mustf(orchclient.SetGitHubRepoList(ctx, c, "harbor", nil))(t); l.Source != orchapi.RepoListRemotes || len(l.Repos) != 2 {
		t.Fatalf("reset: %+v", l)
	}
}
