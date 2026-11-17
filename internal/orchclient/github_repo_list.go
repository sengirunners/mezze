package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// GitHubRepoList is GET /v0/github/repo-list: project's repo list (from a
// member, its authority's).
func GitHubRepoList(ctx context.Context, c *client.Client, project string) (orchapi.GitHubRepoList, error) {
	var out orchapi.GitHubRepoList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/github/repo-list", "project", project), nil, &out)
	return out, err
}

// SetGitHubRepoList is PUT /v0/github/repo-list: repos replaces the list;
// nil clears it (back to the project's GitHub remotes).
func SetGitHubRepoList(ctx context.Context, c *client.Client, project string, repos *[]string) (orchapi.GitHubRepoList, error) {
	var out orchapi.GitHubRepoList
	err := c.Do(ctx, http.MethodPut, client.Query("/v0/github/repo-list", "project", project), orchapi.GitHubRepoListUpdate{Repos: repos}, &out)
	return out, err
}
