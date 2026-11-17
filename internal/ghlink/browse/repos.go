// Package browse owns the scoped, cached GitHub reads used by zm clients.
package browse

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

var repoPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)

func repository(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return ""
		}
		s = strings.Trim(u.Path, "/")
	} else {
		s = strings.TrimPrefix(s, "git@github.com:")
		s = strings.TrimPrefix(s, "github.com/")
	}
	s = strings.TrimSuffix(s, ".git")
	if !repoPattern.MatchString(s) {
		return ""
	}
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." {
			return ""
		}
	}
	return strings.ToLower(s)
}

func (s *Service) repositories(r *http.Request, sub *authz.Subject) (orchapi.GitHubRepositories, error) {
	named := r.URL.Query().Get("project")
	if in := core.ResolveInputOf(r.Context()); named == "" && in.ExplicitFrom == project.RuleFlag {
		named = in.Explicit
	}
	joined := s.joined(r, sub)
	local := true
	sc, err := s.d.ReadScope(r.Context(), named)
	if err != nil {
		// Not a local project: a joined team project's mappings come from
		// its authority's last header (a local project of the same name
		// wins, as in the team proxy).
		j, ok := joinedNamed(joined, named)
		if !ok || !api.IsCode(err, api.CodeNotFound) {
			return orchapi.GitHubRepositories{}, err
		}
		joined, local, sc = []core.JoinedRepos{j}, false, sub.Scope.Only(j.Project.ID)
	} else if named != "" {
		joined = nil
	}
	area := r.URL.Query().Get("area")
	out := orchapi.GitHubRepositories{Repositories: []orchapi.GitHubRepository{}}
	seen := map[string]bool{}
	add := func(p api.Project, a string, remotes []string) {
		if area != "" && area != a {
			return
		}
		if sub.Check(authz.Read, authz.Object{Project: p.ID, Area: a}, "repository", "") != nil {
			return
		}
		for _, raw := range remotes {
			repo := repository(raw)
			key := p.ID + "\x00" + a + "\x00" + repo
			if repo == "" || seen[key] {
				continue
			}
			seen[key] = true
			out.Repositories = append(out.Repositories, orchapi.GitHubRepository{Project: p.ID, Slug: p.Slug, Area: a, Repo: repo})
		}
	}
	if local {
		for _, p := range s.d.Projects.List(sc) {
			if sc.WholeProject(p.ID) {
				add(p, "", p.Remotes)
			}
			for _, a := range p.Areas {
				add(p, a.Name, a.Remotes)
			}
		}
	}
	for _, j := range joined {
		p := api.Project{ID: j.Project.ID, Slug: j.Project.Slug}
		if _, isLocal := s.d.Projects.Lookup(p.ID); isLocal || !sc.SeesProject(p.ID) {
			continue
		}
		if sc.WholeProject(p.ID) {
			add(p, "", j.Repos.Remotes)
		}
		for _, a := range j.AreaRepos {
			add(p, a.Area, a.Remotes)
		}
	}
	sort.Slice(out.Repositories, func(i, j int) bool {
		a, b := out.Repositories[i], out.Repositories[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Area < b.Area
	})
	return out, nil
}

// joined is this machine's joined team projects for a caller on the socket
// as the machine's user (a token on the socket acts in this machine's
// projects only, as in the team proxy), or nil.
func (s *Service) joined(r *http.Request, sub *authz.Subject) []core.JoinedRepos {
	tr, ok := s.d.Team.(core.TeamRepos)
	if !ok || sub == nil || sub.Via != authz.ViaLocal || !core.IsLocal(r.Context()) {
		return nil
	}
	return tr.JoinedRepos()
}

// joinedNamed is the joined project named (its slug or the authority's
// ULID, with an optional /area).
func joinedNamed(joined []core.JoinedRepos, named string) (core.JoinedRepos, bool) {
	name, _, _ := strings.Cut(named, "/")
	if name == "" {
		return core.JoinedRepos{}, false
	}
	for _, j := range joined {
		if strings.EqualFold(j.Project.ID, name) || j.Project.Slug == strings.ToLower(name) {
			return j, true
		}
	}
	return core.JoinedRepos{}, false
}

func (s *Service) mapped(r *http.Request, sub *authz.Subject) (orchapi.GitHubRepository, error) {
	want := strings.ToLower(r.URL.Query().Get("repo"))
	if r.URL.Query().Get("project") == "" {
		return orchapi.GitHubRepository{}, api.Errorf(api.CodeBadRequest, "project is required")
	}
	list, err := s.repositories(r, sub)
	if err != nil {
		return orchapi.GitHubRepository{}, err
	}
	for _, repo := range list.Repositories {
		if repo.Repo == want {
			return repo, nil
		}
	}
	return orchapi.GitHubRepository{}, api.NotFound("repository", want)
}
