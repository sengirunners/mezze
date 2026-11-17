package orchclient

import (
	"context"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
	"net/http"
)

func Summaries(ctx context.Context, c *client.Client, project, q string, limit int) (orchapi.SummaryList, error) {
	var out orchapi.SummaryList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/summaries", "project", project, "q", q, "limit", client.Itoa(int64(limit))), nil, &out)
	return out, err
}
func GenerateSummary(ctx context.Context, c *client.Client, project, day string) (orchapi.ProjectSummary, error) {
	var out orchapi.ProjectSummary
	err := c.Do(ctx, http.MethodPost, client.Query("/v0/summaries", "project", project), orchapi.SummaryRequest{Day: day}, &out)
	return out, err
}
func SummaryConfig(ctx context.Context, c *client.Client, project string) (orchapi.SummaryConfig, error) {
	var out orchapi.SummaryConfig
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/summaries/config", "project", project), nil, &out)
	return out, err
}
func SetSummaryConfig(ctx context.Context, c *client.Client, project string, config orchapi.SummaryConfig) (orchapi.SummaryConfig, error) {
	var out orchapi.SummaryConfig
	err := c.Do(ctx, http.MethodPut, client.Query("/v0/summaries/config", "project", project), config, &out)
	return out, err
}
