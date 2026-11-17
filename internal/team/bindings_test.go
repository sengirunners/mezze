package team_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A replica joined to two projects resolves a cwd by their path and remote
// rules as the authority sets them (the team header), exactly like a local
// project's: the longest path wins, local or joined, an area's over its
// project's; remotes when no path matched; a change at the authority counts
// after the next answer. Dogfood: in a checkout of the zm repository, zm project
// why said harbor (the session rule) although the authority's zm project
// listed the checkout's path and remote.
func TestJoinedProjectBindings(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := func(p string) string {
		d := filepath.Join(base, p)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}
	pcSub, worm, solver, later := dir("pc-feature/sub"), dir("pc-main/worm"), dir("work/solver"), dir("later")
	// A git checkout outside every path (the local project's too) whose
	// origin is harbor's remote.
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo+"/.git", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/.git/config", []byte("[remote \"origin\"]\n\turl = git@github.com:example-org/harbor.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm", RepoRules: api.RepoRules{Paths: []string{base + "/pc-*"}}}))(t)
	mustf(a.owner.AddArea(ctx, "zm", api.Area{Name: "worm", RepoRules: api.RepoRules{Paths: []string{worm}}}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	mustf(a.owner.UpdateArea(ctx, "harbor", "solver", api.UpdateAreaRequest{Paths: &[]string{solver}}))(t)

	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	jz := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if len(j.Remote.Paths)+len(jz.Remote.Paths) != 0 {
		t.Fatalf("no checkout should be recorded: %v %v", j.Remote.Paths, jz.Remote.Paths)
	}
	// A local project whose path is shorter than zm's rule.
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "mine", RepoRules: api.RepoRules{Paths: []string{base}}}))(t)
	heartbeat(t, r) // every joined project's header, fresh

	in := func(d string) *client.Client { c := *r.sock; c.Cwd = d; return &c }
	why := func(d, wantSlug, wantArea, wantRule string) {
		t.Helper()
		res := mustf(in(d).Resolve(ctx))(t)
		if res.Project == nil || res.Project.Slug != wantSlug || res.Area != wantArea {
			t.Fatalf("%s: resolved %+v area %q, want %s/%s (%+v)", d, res.Project, res.Area, wantSlug, wantArea, res.Steps)
		}
		if last := res.Steps[len(res.Steps)-1]; wantRule != "" && !strings.HasSuffix(last.Result, "by "+wantRule) {
			t.Fatalf("%s: last step %+v, want by %s", d, last, wantRule)
		}
	}
	why(pcSub, "zm", "", "cwd-path")    // zm's glob beats the local project's shorter path
	why(worm, "zm", "worm", "cwd-path") // the area's longer path
	why(solver, "harbor", "solver", "cwd-path")
	why(repo, "harbor", "", "cwd-remote") // no path: harbor's remote
	why(later, "mine", "", "")            // only the local project's path
	// A write goes where the rule says.
	it := mustf(orchclient.CreateWork(ctx, in(worm), orchapi.CreateWorkRequest{Title: "from a zm checkout"}))(t)
	if it.Project != jz.Remote.Project.ID || it.Area != "worm" {
		t.Fatalf("write from the worm path: %+v", it)
	}
	// The authority adds a path; the next answer brings it.
	mustf(a.owner.UpdateProject(ctx, "zm", api.UpdateProjectRequest{Repos: &api.RepoRules{Paths: []string{base + "/pc-*", later}}}))(t)
	heartbeat(t, r)
	why(later, "zm", "", "cwd-path")

	// Ties go local: a local project's exact path as long as zm's glob
	// match keeps its writes here. A longer local path beats a shorter
	// joined one.
	scratch, deep := dir("pc-scratch"), dir("pc-main/worm/local")
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "scratch", RepoRules: api.RepoRules{Paths: []string{scratch}}}))(t)
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "deep", RepoRules: api.RepoRules{Paths: []string{deep}}}))(t)
	why(scratch, "scratch", "", "")
	why(deep, "deep", "", "")
	if it := mustf(orchclient.CreateWork(ctx, in(scratch), orchapi.CreateWorkRequest{Title: "local only"}))(t); it.Project == jz.Remote.Project.ID {
		t.Fatalf("a local project's write went to the authority: %+v", it)
	}
	// The authority clears zm's paths: after the next answer they match
	// nothing (the area's own path still does).
	mustf(a.owner.UpdateProject(ctx, "zm", api.UpdateProjectRequest{Repos: &api.RepoRules{Paths: []string{}}}))(t)
	heartbeat(t, r)
	why(pcSub, "mine", "", "")
	why(later, "mine", "", "")
	why(worm, "zm", "worm", "cwd-path")
}
