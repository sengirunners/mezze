package orchclient

import (
	"context"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func GitHubRepositories(ctx context.Context, c *client.Client, project string) (out orchapi.GitHubRepositories, err error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	err = c.Do(ctx, "GET", "/v0/github/repos?"+q.Encode(), nil, &out)
	return
}

func GitHubData(ctx context.Context, c *client.Client, q url.Values) (out orchapi.GitHubData, err error) {
	err = c.Do(ctx, "GET", "/v0/github/data?"+q.Encode(), nil, &out)
	return
}
