package orchclient

// Owned by the work builder. One method per /v0/work and /v0/leases endpoint in API.md.

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func workPath(id, suffix string) string { return "/v0/work/" + client.Seg(id) + suffix }

// WorkSources is GET /v0/work/sources.
func WorkSources(ctx context.Context, c *client.Client, project string) ([]orchapi.WorkSource, error) {
	var l orchapi.WorkSourceList
	return l.Sources, c.Do(ctx, http.MethodGet, client.Query("/v0/work/sources", "project", project), nil, &l)
}

// Work is GET /v0/work, items only. Callers that show a list to someone
// should use WorkList, whose Skipped names trackers that could not be listed.
func Work(ctx context.Context, c *client.Client, q orchapi.WorkQuery) ([]orchapi.WorkItem, error) {
	l, err := WorkList(ctx, c, q)
	return l.Items, err
}

// WorkList is GET /v0/work, with the trackers that were skipped.
func WorkList(ctx context.Context, c *client.Client, q orchapi.WorkQuery) (orchapi.WorkList, error) {
	var l orchapi.WorkList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/work", "project", q.Project, "area", q.Area, "source", q.Source,
		"status", string(q.Status), "owner", q.Owner, "q", q.Q, "limit", client.Itoa(int64(q.Limit))), nil, &l)
	return l, err
}

// CreateWork is POST /v0/work.
func CreateWork(ctx context.Context, c *client.Client, req orchapi.CreateWorkRequest) (orchapi.WorkItem, error) {
	var w orchapi.WorkItem
	return w, c.Do(ctx, http.MethodPost, "/v0/work", req, &w)
}

// WorkRollup is GET /v0/work/rollup: counts per theme and epic.
func WorkRollup(ctx context.Context, c *client.Client, project, area string) (orchapi.WorkRollupList, error) {
	var l orchapi.WorkRollupList
	return l, c.Do(ctx, http.MethodGet, client.Query("/v0/work/rollup", "project", project, "area", area), nil, &l)
}

// Inflight is GET /v0/work/inflight.
func Inflight(ctx context.Context, c *client.Client, project string) ([]orchapi.Inflight, error) {
	var l orchapi.InflightList
	return l.Inflight, c.Do(ctx, http.MethodGet, client.Query("/v0/work/inflight", "project", project), nil, &l)
}

// GetWork is GET /v0/work/{id}.
func GetWork(ctx context.Context, c *client.Client, id string) (orchapi.WorkItem, error) {
	var w orchapi.WorkItem
	return w, c.Do(ctx, http.MethodGet, workPath(id, ""), nil, &w)
}

// Holder is GET /v0/work/{id}/holder.
func Holder(ctx context.Context, c *client.Client, id string) (orchapi.HolderResponse, error) {
	var h orchapi.HolderResponse
	return h, c.Do(ctx, http.MethodGet, workPath(id, "/holder"), nil, &h)
}

// Comments is GET /v0/work/{id}/comments.
func Comments(ctx context.Context, c *client.Client, id string) ([]orchapi.Comment, error) {
	var l orchapi.CommentList
	return l.Comments, c.Do(ctx, http.MethodGet, workPath(id, "/comments"), nil, &l)
}

// Claim is POST /v0/work/{id}/claim (claiming again renews).
func Claim(ctx context.Context, c *client.Client, id string, req orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	var r orchapi.ClaimResponse
	return r, c.Do(ctx, http.MethodPost, workPath(id, "/claim"), req, &r)
}

// Release is POST /v0/work/{id}/release.
func Release(ctx context.Context, c *client.Client, id string, req orchapi.ReleaseRequest) (orchapi.ReleaseResponse, error) {
	var r orchapi.ReleaseResponse
	return r, c.Do(ctx, http.MethodPost, workPath(id, "/release"), req, &r)
}

// Comment is POST /v0/work/{id}/comment.
func Comment(ctx context.Context, c *client.Client, id string, req orchapi.CommentRequest) (orchapi.Comment, error) {
	var cm orchapi.Comment
	return cm, c.Do(ctx, http.MethodPost, workPath(id, "/comment"), req, &cm)
}

// SetStatus is POST /v0/work/{id}/status.
func SetStatus(ctx context.Context, c *client.Client, id string, req orchapi.StatusRequest) (orchapi.WorkItem, error) {
	var w orchapi.WorkItem
	return w, c.Do(ctx, http.MethodPost, workPath(id, "/status"), req, &w)
}

// Link is POST /v0/work/{id}/link.
func Link(ctx context.Context, c *client.Client, id string, req orchapi.LinkRequest) (orchapi.WorkItem, error) {
	var w orchapi.WorkItem
	return w, c.Do(ctx, http.MethodPost, workPath(id, "/link"), req, &w)
}

// Leases is GET /v0/leases.
func Leases(ctx context.Context, c *client.Client, q orchapi.LeaseQuery) ([]orchapi.Lease, error) {
	var l orchapi.LeaseList
	return l.Leases, c.Do(ctx, http.MethodGet, client.Query("/v0/leases", "project", q.Project, "holder", q.Holder), nil, &l)
}

// Lease is POST /v0/leases (any key, e.g. lock:migrations; again = renew).
func Lease(ctx context.Context, c *client.Client, req orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	var r orchapi.ClaimResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/leases", req, &r)
}

// ReleaseLease is DELETE /v0/leases/{key}.
func ReleaseLease(ctx context.Context, c *client.Client, key string, req orchapi.ReleaseRequest) (orchapi.ReleaseResponse, error) {
	var r orchapi.ReleaseResponse
	return r, c.Do(ctx, http.MethodDelete, "/v0/leases/"+client.Seg(key), req, &r)
}
