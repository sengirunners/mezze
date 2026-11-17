package browse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

type fakeTeam struct{ joined []core.JoinedRepos }

func (f fakeTeam) Proxy(http.ResponseWriter, *http.Request, http.Handler) bool { return false }
func (f fakeTeam) JoinedRepos() []core.JoinedRepos                             { return f.joined }

const joinedID = "01JOINEDHARBOR0000000000AA"

func withJoined(s *Service) {
	s.d.Team = fakeTeam{joined: []core.JoinedRepos{{
		Project:   api.ProjectRef{ID: joinedID, Slug: "harbor"},
		Repos:     api.RepoRules{Remotes: []string{"github.com/example-org/harbor"}},
		AreaRepos: []api.NamedRepos{{Area: "solver", RepoRules: api.RepoRules{Remotes: []string{"git@github.com:example-org/solver.git"}}}},
	}}}
}

func repos(t *testing.T, s *Service, sub *authz.Subject, local bool, q url.Values) ([]orchapi.GitHubRepository, error) {
	t.Helper()
	r := httptest.NewRequest("GET", "/v0/github/repos?"+q.Encode(), nil)
	ctx := core.WithSubject(r.Context(), sub)
	if local {
		ctx = core.WithLocal(ctx)
	}
	r = r.WithContext(ctx)
	list, err := s.repositories(r, sub)
	return list.Repositories, err
}

func has(list []orchapi.GitHubRepository, project, area, repo string) bool {
	for _, m := range list {
		if m.Project == project && m.Area == area && m.Repo == repo {
			return true
		}
	}
	return false
}

// A joined team project (authority elsewhere) lists its repositories from
// the authority's last header, by itself and next to the local projects;
// its data reads resolve by slug or ULID and are fetched here, with this
// daemon's own GitHub access. Dogfood: the cockpit's PR and CI tabs said "No
// GitHub repositories." on a Mac whose projects were all joined.
func TestJoinedProjectRepositories(t *testing.T) {
	var fetched []string
	s, owner, guest, p := fixture(t, func(_ context.Context, project string, _ ...string) ([]byte, error) {
		fetched = append(fetched, project)
		return []byte(`{"workflow_runs":[],"total_count":0}`), nil
	})
	withJoined(s)

	all, err := repos(t, s, owner, true, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if !has(all, joinedID, "", "example-org/harbor") || !has(all, joinedID, "solver", "example-org/solver") || !has(all, p.ID, "", "acme/private") {
		t.Fatalf("local and joined mappings: %+v", all)
	}
	for _, name := range []string{"harbor", joinedID, "harbor/solver"} {
		only, err := repos(t, s, owner, true, url.Values{"project": {name}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(only) != 2 || has(only, p.ID, "", "acme/private") {
			t.Fatalf("%s: %+v", name, only)
		}
	}
	area, err := repos(t, s, owner, true, url.Values{"project": {"harbor"}, "area": {"solver"}})
	if err != nil || len(area) != 1 || area[0].Repo != "example-org/solver" || area[0].Slug != "harbor" {
		t.Fatalf("area filter: %+v %v", area, err)
	}
	// A local project still answers alone when named.
	if mine, err := repos(t, s, owner, true, url.Values{"project": {"main"}}); err != nil || has(mine, joinedID, "", "example-org/harbor") {
		t.Fatalf("local project named: %+v %v", mine, err)
	}

	// Data: the mapping resolves; the fetch runs here for the joined ULID.
	for _, name := range []string{"harbor", joinedID} {
		r := httptest.NewRequest("GET", "/v0/github/data?"+url.Values{"project": {name}, "repo": {"example-org/harbor"}, "resource": {"runs"}}.Encode(), nil)
		r = r.WithContext(core.WithLocal(core.WithSubject(r.Context(), owner)))
		v, err := s.data(r, owner)
		if err != nil || v.Project != joinedID || v.Repo != "example-org/harbor" {
			t.Fatalf("%s data: %+v %v", name, v, err)
		}
	}
	if len(fetched) != 1 || fetched[0] != joinedID {
		t.Fatalf("fetched %v, want one fetch for the joined project (cached after)", fetched)
	}

	// A token caller on the socket, or a caller off the socket, sees only
	// this machine's projects: the joined one is missing, as with the proxy.
	for _, c := range []struct {
		sub   *authz.Subject
		local bool
	}{{guest, true}, {owner, false}} {
		list, err := repos(t, s, c.sub, c.local, url.Values{})
		if err != nil || has(list, joinedID, "", "example-org/harbor") || has(list, joinedID, "solver", "example-org/solver") {
			t.Fatalf("%s local=%v: %+v %v", c.sub.Via, c.local, list, err)
		}
		if _, err := repos(t, s, c.sub, c.local, url.Values{"project": {"harbor"}}); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%s local=%v named joined: %v", c.sub.Via, c.local, err)
		}
	}
}
