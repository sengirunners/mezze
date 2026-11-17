package orchclient

// Webhooks and GitHub linking (SPEC "Webhooks and GitHub linking").

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Webhooks is GET /v0/webhooks (project "" = every hook the caller administers).
func Webhooks(ctx context.Context, c *client.Client, project string) ([]orchapi.Webhook, error) {
	var l orchapi.WebhookList
	p := "/v0/webhooks"
	if project != "" {
		p += "?project=" + url.QueryEscape(project)
	}
	return l.Webhooks, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// AddWebhook is POST /v0/webhooks.
func AddWebhook(ctx context.Context, c *client.Client, req orchapi.NewWebhookRequest) (orchapi.NewWebhookResponse, error) {
	var r orchapi.NewWebhookResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/webhooks", req, &r)
}

// Webhook is GET /v0/webhooks/{id}.
func Webhook(ctx context.Context, c *client.Client, id string, limit int) (orchapi.WebhookDetail, error) {
	var d orchapi.WebhookDetail
	p := "/v0/webhooks/" + client.Seg(id)
	if limit > 0 {
		p += "?limit=" + url.QueryEscape(itoa(limit))
	}
	return d, c.Do(ctx, http.MethodGet, p, nil, &d)
}

// UpdateWebhook is PATCH /v0/webhooks/{id}.
func UpdateWebhook(ctx context.Context, c *client.Client, id string, req orchapi.UpdateWebhookRequest) (orchapi.Webhook, error) {
	var w orchapi.Webhook
	return w, c.Do(ctx, http.MethodPatch, "/v0/webhooks/"+client.Seg(id), req, &w)
}

// RemoveWebhook is DELETE /v0/webhooks/{id}.
func RemoveWebhook(ctx context.Context, c *client.Client, id string) (orchapi.Webhook, error) {
	var w orchapi.Webhook
	return w, c.Do(ctx, http.MethodDelete, "/v0/webhooks/"+client.Seg(id), nil, &w)
}

// TestWebhook is POST /v0/webhooks/{id}/test.
func TestWebhook(ctx context.Context, c *client.Client, id string) (orchapi.WebhookTestResponse, error) {
	var r orchapi.WebhookTestResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/webhooks/"+client.Seg(id)+"/test", struct{}{}, &r)
}

// GitHubLinks is GET /v0/github/links.
func GitHubLinks(ctx context.Context, c *client.Client) ([]orchapi.GitHubLink, error) {
	var l orchapi.GitHubLinkList
	return l.Links, c.Do(ctx, http.MethodGet, "/v0/github/links", nil, &l)
}

// LinkGitHub is POST /v0/github/links.
func LinkGitHub(ctx context.Context, c *client.Client, req orchapi.GitHubLinkRequest) (orchapi.GitHubLinkResponse, error) {
	var r orchapi.GitHubLinkResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/github/links", req, &r)
}

// UnlinkGitHub is DELETE /v0/github/links/{project}.
func UnlinkGitHub(ctx context.Context, c *client.Client, project string) (orchapi.GitHubLink, error) {
	var l orchapi.GitHubLink
	return l, c.Do(ctx, http.MethodDelete, "/v0/github/links/"+client.Seg(project), nil, &l)
}

// SyncGitHub is POST /v0/github/links/{project}/sync.
func SyncGitHub(ctx context.Context, c *client.Client, project string) (orchapi.GitHubSyncResponse, error) {
	var r orchapi.GitHubSyncResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/github/links/"+client.Seg(project)+"/sync", struct{}{}, &r)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
