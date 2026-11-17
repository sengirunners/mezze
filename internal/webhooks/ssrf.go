package webhooks

import (
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/sengirunners/potluck/kit/netguard"
)

// The SSRF guard lives in potluck/kit/netguard (shared with connections).

// ErrBlockedAddress is a dial refused because the address is not public.
var ErrBlockedAddress = netguard.ErrBlockedAddress

// Blocked reports whether a webhook may not reach ip unless the hook allows
// private addresses.
func Blocked(ip netip.Addr) bool { return netguard.Blocked(ip) }

// CheckURL validates a hook URL at creation (netguard.CheckURL).
func CheckURL(raw string, allowPrivate bool) (*url.URL, error) {
	return netguard.CheckURL(raw, allowPrivate, "webhook url")
}

func newClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return netguard.Client(timeout, allowPrivate)
}
