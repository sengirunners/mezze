package orchclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Permits is GET /v0/permits (SPEC 16 "Standing permissions").
func Permits(ctx context.Context, c *client.Client) (orchapi.PermitList, error) {
	var out orchapi.PermitList
	return out, c.Do(ctx, http.MethodGet, "/v0/permits", nil, &out)
}

// PermitCard is GET /v0/permits/card: the frozen card for a kind or a box.
func PermitCard(ctx context.Context, c *client.Client, req orchapi.PermitCardRequest) (orchapi.AskCard, error) {
	q := url.Values{}
	for k, v := range map[string]string{"kind": req.Kind, "machine": req.Machine, "class": req.Class} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.Days != 0 {
		q.Set("days", strconv.Itoa(req.Days))
	}
	var out orchapi.AskCard
	return out, c.Do(ctx, http.MethodGet, "/v0/permits/card?"+q.Encode(), nil, &out)
}

// GrantPermit is POST /v0/permits.
func GrantPermit(ctx context.Context, c *client.Client, req orchapi.PermitGrantRequest) (orchapi.PermitGrant, error) {
	var out orchapi.PermitGrant
	return out, c.Do(ctx, http.MethodPost, "/v0/permits", req, &out)
}

// RevokePermit is POST /v0/permits/{id}/revoke.
func RevokePermit(ctx context.Context, c *client.Client, id string) (orchapi.Permit, error) {
	var out orchapi.Permit
	return out, c.Do(ctx, http.MethodPost, "/v0/permits/"+url.PathEscape(id)+"/revoke", struct{}{}, &out)
}

// UnmarkBox is POST /v0/permits/boxes/unmark.
func UnmarkBox(ctx context.Context, c *client.Client, req orchapi.BoxUnmarkRequest) (orchapi.BoxUnmarkRequest, error) {
	var out orchapi.BoxUnmarkRequest
	return out, c.Do(ctx, http.MethodPost, "/v0/permits/boxes/unmark", req, &out)
}

// PermitDryRun is GET /v0/permits/dryrun over [from, to) (zero: the daemon's
// default, the last 7 days).
func PermitDryRun(ctx context.Context, c *client.Client, from, to time.Time) (orchapi.PermitDryRunReport, error) {
	q := url.Values{}
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	var out orchapi.PermitDryRunReport
	return out, c.Do(ctx, http.MethodGet, "/v0/permits/dryrun?"+q.Encode(), nil, &out)
}
