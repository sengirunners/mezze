package ghlink

// A project's GitHub repo list (SPEC "Webhooks and GitHub linking"): the
// repositories automations scoped to the project act on.
// Unlike a link (which runs gh on its machine) it is project config: it
// lives at the project's authority, an admin sets it from any member
// machine (the team proxy forwards both routes there) and every member
// reads the same list. Never set, it is the project's and its areas'
// declared GitHub remotes. Setting it moves no item and polls nothing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

const schema5 = `
CREATE TABLE ghlink_repo_lists (
	project TEXT PRIMARY KEY,
	repos   TEXT NOT NULL,
	updated INTEGER NOT NULL
);`

var repoListMigrations = []store.Migration{{Kind: store.KindRegistry, Name: "ghlink/0005_repo_lists", Script: schema5}}

// MaxRepoList bounds a project's repo list.
const MaxRepoList = 200

func (s *Service) handleRepoList(r *http.Request, _ *authz.Subject) (any, error) {
	p, err := s.repoListProject(r, false)
	if err != nil {
		return nil, err
	}
	return s.repoList(r.Context(), p)
}

func (s *Service) handleSetRepoList(r *http.Request, _ *authz.Subject) (any, error) {
	p, err := s.repoListProject(r, true)
	if err != nil {
		return nil, err
	}
	var req orchapi.GitHubRepoListUpdate
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Repos == nil {
		if _, err := s.db().ExecContext(r.Context(), `DELETE FROM ghlink_repo_lists WHERE project = ?`, p.ID); err != nil {
			return nil, err
		}
		return s.repoList(r.Context(), p)
	}
	if len(*req.Repos) > 2*MaxRepoList { // before the dedupe below
		return nil, api.Errorf(api.CodeBadRequest, "at most %d repos in a project's list", MaxRepoList)
	}
	repos := []string{}
	for _, raw := range *req.Repos {
		repo, ok := remoteRepo(raw) // git@github.com:o/r, https://github.com/o/r.git
		if !ok {
			repo, ok = NormRepo(raw)
		}
		if !ok {
			return nil, api.Errorf(api.CodeBadRequest, "bad repo %q (want owner/name)", raw)
		}
		if !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) > MaxRepoList {
		return nil, api.Errorf(api.CodeBadRequest, "at most %d repos in a project's list", MaxRepoList)
	}
	js, _ := json.Marshal(repos)
	if _, err := s.db().ExecContext(r.Context(), `INSERT INTO ghlink_repo_lists (project, repos, updated) VALUES (?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET repos = excluded.repos, updated = excluded.updated`, p.ID, string(js), s.now().UnixMilli()); err != nil {
		return nil, err
	}
	return s.repoList(r.Context(), p)
}

// repoListProject is the whole project the call names (?project=, else -P's
// Zm-Project) that the caller may read, and with admin set, administer.
// Hidden, area-only and missing answer alike. Admin is core.Deps.ProjectAdmin:
// judged by the identity's own grants, so an owner holds it from a member
// machine's link (its node-bound token is capped at member).
func (s *Service) repoListProject(r *http.Request, admin bool) (api.Project, error) {
	ctx := r.Context()
	raw := r.URL.Query().Get("project")
	if in := core.ResolveInputOf(ctx); raw == "" && in.ExplicitFrom == project.RuleFlag {
		raw = in.Explicit
	}
	if raw == "" {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "project is required").WithHint("zm github ls -P <project>")
	}
	if strings.Contains(raw, "/") {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "a repo list is the whole project's: name the project without an area")
	}
	sc, err := s.d.ReadScope(ctx, raw)
	if err != nil {
		return api.Project{}, err
	}
	p, err := s.d.Projects.Get(sc, raw)
	if err != nil {
		return api.Project{}, err
	}
	sub := core.SubjectOf(ctx)
	if !sc.WholeProject(p.ID) || sub.Check(authz.Read, authz.Object{Project: p.ID}, "project", raw) != nil {
		return api.Project{}, api.NotFound("project", raw)
	}
	if !admin {
		return p, nil
	}
	// By slug: a member's proxy names the project by the authority's ULID.
	target, err := s.d.WriteTarget(core.WithResolveInput(ctx, project.ResolveInput{Explicit: p.Slug, ExplicitFrom: project.RuleFlag}), "", authz.Write)
	if err != nil || target.Project.ID != p.ID || target.Area != "" {
		return api.Project{}, api.NotFound("project", raw)
	}
	if ok, err := s.d.ProjectAdmin(ctx, sub, p.ID); err != nil || !ok {
		return api.Project{}, api.NotFound("project", raw)
	}
	return p, nil
}

// repoList is p's list: the one set, else its GitHub remotes.
func (s *Service) repoList(ctx context.Context, p api.Project) (orchapi.GitHubRepoList, error) {
	out := orchapi.GitHubRepoList{Project: p.Slug, ID: p.ID, Repos: []string{}, Source: orchapi.RepoListSet}
	var js string
	var updated int64
	err := s.db().QueryRowContext(ctx, `SELECT repos, updated FROM ghlink_repo_lists WHERE project = ?`, p.ID).Scan(&js, &updated)
	switch {
	case err == nil:
		if err := json.Unmarshal([]byte(js), &out.Repos); err != nil || out.Repos == nil {
			out.Repos = []string{}
		}
		t := time.UnixMilli(updated).UTC()
		out.Updated = &t
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return out, err
	}
	out.Source = orchapi.RepoListRemotes
	remotes := slices.Clone(p.Remotes)
	for _, a := range p.Areas {
		remotes = append(remotes, a.Remotes...)
	}
	for _, raw := range remotes {
		if repo, ok := remoteRepo(raw); ok && !slices.Contains(out.Repos, repo) {
			out.Repos = append(out.Repos, repo)
		}
	}
	return out, nil
}

// remoteRepo is a declared git remote's owner/name when it is on github.com
// (remotes are stored as project.NormalizeRemote leaves them: host/path).
func remoteRepo(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(project.NormalizeRemote(raw), "github.com/")
	if !ok {
		return "", false
	}
	return NormRepo(rest)
}
