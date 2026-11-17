package connections

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/netguard"
)

// Fetcher reads one provider's summary: the raw body, checked by Validate
// afterwards. Drivers: https (any caller; netguard keeps it off private
// networks unless the machine owner allowed them) and a Unix socket (the
// machine owner's only). A third, a process speaking SPEC 11's envelope,
// would plug in here.
type Fetcher interface {
	Fetch(ctx context.Context, token string) ([]byte, error)
}

// FetchTimeout bounds one fetch, connect to last byte.
const FetchTimeout = 10 * time.Second

// Endpoint is a parsed provider address.
type Endpoint struct {
	Raw    string
	Socket string // the socket path for unix:
	Path   string // the request path on a socket
	URL    *url.URL
}

// ParseEndpoint checks raw at registration. unix:/abs/path.sock[?path=/show]
// is a socket on this machine; anything else must pass netguard.
func ParseEndpoint(raw string, allowPrivate bool) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(raw, "unix:"); ok {
		sock, q, _ := strings.Cut(rest, "?")
		sock = strings.TrimPrefix(sock, "//")
		if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock || len(sock) > 103 {
			return Endpoint{}, api.Errorf(api.CodeBadRequest, "bad connection endpoint: want unix:/absolute/path.sock (at most 103 bytes)")
		}
		path := "/"
		if q != "" {
			v, err := url.ParseQuery(q)
			if err != nil || len(v) != 1 || !strings.HasPrefix(v.Get("path"), "/") {
				return Endpoint{}, api.Errorf(api.CodeBadRequest, "bad connection endpoint: the only query is ?path=/...")
			}
			path = v.Get("path")
		}
		e := Endpoint{Raw: "unix:" + sock, Socket: sock, Path: path}
		if path != "/" {
			e.Raw += "?path=" + url.QueryEscape(path)
		}
		return e, nil
	}
	u, err := netguard.CheckURL(raw, allowPrivate, "connection endpoint")
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Raw: u.String(), URL: u}, nil
}

// Fetcher is the driver for e.
func (e Endpoint) Fetcher(allowPrivate bool) Fetcher {
	if e.Socket != "" {
		sock := e.Socket
		tr := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			ResponseHeaderTimeout: FetchTimeout,
		}
		c := &http.Client{Transport: tr, Timeout: FetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		return httpFetcher{c: c, url: "http://zm-connection" + e.Path}
	}
	return httpFetcher{c: netguard.Client(FetchTimeout, allowPrivate), url: e.URL.String()}
}

type httpFetcher struct {
	c   *http.Client
	url string
}

func (f httpFetcher) Fetch(ctx context.Context, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zm-connections/1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.c.Do(req)
	if err != nil {
		return nil, plainFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the provider refused zm's token (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("the provider answered HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading the provider's answer: %v", err)
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("the provider's answer is over %d KiB", MaxBody>>10)
	}
	return b, nil
}

// plainFetchError says what went wrong without the provider's URL or token.
func plainFetchError(err error) error {
	switch {
	case strings.Contains(err.Error(), netguard.ErrBlockedAddress.Error()):
		return fmt.Errorf("the provider's address is not public")
	case strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "deadline exceeded"):
		return fmt.Errorf("the provider did not answer within %s", FetchTimeout)
	case strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such file"):
		return fmt.Errorf("nothing is listening at the provider's address")
	}
	return fmt.Errorf("could not reach the provider")
}
