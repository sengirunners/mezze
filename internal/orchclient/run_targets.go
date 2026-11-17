package orchclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// RunTargets is GET /v0/run-targets: the project's machines an approved
// action may run on and whose they are; person ("" = everyone) narrows it to
// one person's.
func RunTargets(ctx context.Context, c *client.Client, person string) (orchapi.RunTargetList, error) {
	var out orchapi.RunTargetList
	path := "/v0/run-targets"
	if person != "" {
		path += "?person=" + url.QueryEscape(person)
	}
	return out, c.Do(ctx, http.MethodGet, path, nil, &out)
}
