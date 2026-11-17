package orchclient

// Connections (SPEC 24).

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Connections is GET /v0/connections (project "" = every one the caller may read).
func Connections(ctx context.Context, c *client.Client, project string) ([]orchapi.Connection, error) {
	var l orchapi.ConnectionList
	p := "/v0/connections"
	if project != "" {
		p += "?project=" + url.QueryEscape(project)
	}
	return l.Connections, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// AddConnection is POST /v0/connections.
func AddConnection(ctx context.Context, c *client.Client, req orchapi.NewConnectionRequest) (orchapi.Connection, error) {
	var r orchapi.Connection
	return r, c.Do(ctx, http.MethodPost, "/v0/connections", req, &r)
}

// RemoveConnection is DELETE /v0/connections/{id}.
func RemoveConnection(ctx context.Context, c *client.Client, id string) (orchapi.Connection, error) {
	var r orchapi.Connection
	return r, c.Do(ctx, http.MethodDelete, "/v0/connections/"+client.Seg(id), nil, &r)
}

// ShowConnection is GET /v0/connections/{id}/show (account "" = every account).
func ShowConnection(ctx context.Context, c *client.Client, id, account string) (orchapi.ConnectionShow, error) {
	var r orchapi.ConnectionShow
	p := "/v0/connections/" + client.Seg(id) + "/show"
	if account != "" {
		p += "?account=" + url.QueryEscape(account)
	}
	return r, c.Do(ctx, http.MethodGet, p, nil, &r)
}
