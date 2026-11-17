package orchclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// CIDoctor is GET /v0/runners/doctor[?repo=]: the broker's read-only checks,
// as zm ci doctor and MCP compute_doctor answer them (one result, one error).
func CIDoctor(ctx context.Context, c *client.Client, repo string) (runners.DoctorResult, error) {
	path := "/v0/runners/doctor"
	if repo != "" {
		path += "?repo=" + url.QueryEscape(repo)
	}
	var out runners.DoctorResult
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusNotFound || ae.Code != api.CodeNotFound {
			return out, err
		}
		// A daemon older than the doctor answers 404 for its route.
		return out, errors.New("this daemon has no CI doctor: zm daemon restart, once this zm is installed")
	}
	return out, nil
}
