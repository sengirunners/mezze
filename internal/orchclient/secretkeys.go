package orchclient

// The key directory (SPEC 17.12).

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// SecretKeys is GET /v0/secret-keys: active keys, or every key with all;
// to is "", machine:<name> or human:<name>.
func SecretKeys(ctx context.Context, c *client.Client, to string, all bool) ([]orchapi.SecretKey, error) {
	q := url.Values{}
	if to != "" {
		q.Set("to", to)
	}
	if all {
		q.Set("all", "1")
	}
	p := "/v0/secret-keys"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var l orchapi.SecretKeyList
	return l.Keys, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// PublishSecretKey is PUT /v0/secret-keys: this machine's seal key, as its
// daemon's transport certifies it.
func PublishSecretKey(ctx context.Context, c *client.Client) (orchapi.SecretKey, error) {
	var k orchapi.SecretKey
	return k, c.Do(ctx, http.MethodPut, "/v0/secret-keys", nil, &k)
}

// RevokeSecretKey is POST /v0/secret-keys/{fingerprint}/revoke.
func RevokeSecretKey(ctx context.Context, c *client.Client, fingerprint string) (orchapi.SecretKey, error) {
	var k orchapi.SecretKey
	return k, c.Do(ctx, http.MethodPost, "/v0/secret-keys/"+client.Seg(fingerprint)+"/revoke", nil, &k)
}
