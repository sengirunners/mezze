package orchclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Rulings is the resolved project's owner rulings (GET /v0/rulings).
func Rulings(ctx context.Context, c *client.Client) (orchapi.RulingList, error) {
	var l orchapi.RulingList
	err := c.Do(ctx, http.MethodGet, "/v0/rulings", nil, &l)
	return l, err
}

// AddRuling adds one on the strength of an approved ask (POST /v0/rulings;
// a person who owns or administers the project).
func AddRuling(ctx context.Context, c *client.Client, req orchapi.RulingAddRequest) (orchapi.RulingList, error) {
	var l orchapi.RulingList
	err := c.Do(ctx, http.MethodPost, "/v0/rulings", req, &l)
	return l, err
}

// RemoveRuling removes one by name (DELETE /v0/rulings/{name}).
func RemoveRuling(ctx context.Context, c *client.Client, name string) (orchapi.RulingList, error) {
	var l orchapi.RulingList
	err := c.Do(ctx, http.MethodDelete, "/v0/rulings/"+url.PathEscape(name), nil, &l)
	return l, err
}
