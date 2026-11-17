package orchapi

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// DoorStatus is GET and PUT /v0/door: the door for the caller's machine in
// a team project.
type DoorStatus struct {
	Project string `json:"project"` // the authority's ULID
	Machine string `json:"machine"` // the caller's machine
	On      bool   `json:"on"`
	// Available: the authority serves the door (door.listen is set);
	// BaseURL is where task URLs start ("" when it has no public URL).
	Available bool   `json:"available"`
	BaseURL   string `json:"base_url,omitempty"`
	Tasks     int    `json:"tasks"`
}

// DoorToggle is PUT /v0/door's body.
type DoorToggle struct {
	On bool `json:"on"`
}

// DoorTaskRequest is PUT /v0/door/tasks/{task}'s body.
type DoorTaskRequest struct {
	// Target is the task's own hook URL on the caller's machine: loopback
	// HTTP, path under /api/hooks/.
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
}

// DoorTask is one webhook task's door: URL is "" while the door is off or
// the authority has no public URL.
type DoorTask struct {
	Task      string `json:"task"`
	Machine   string `json:"machine"`
	Label     string `json:"label,omitempty"`
	Target    string `json:"target"`
	URL       string `json:"url,omitempty"`
	Created   int64  `json:"created"` // unix ms
	Rotated   int64  `json:"rotated"` // unix ms: when the current URL was minted
	Queued    int    `json:"queued"`  // deliveries waiting for the machine
	LastAt    int64  `json:"last_at,omitempty"`
	LastState string `json:"last_state,omitempty"` // the newest delivery's state
}

// DoorTaskList is GET /v0/door/tasks.
type DoorTaskList struct {
	Tasks []DoorTask `json:"tasks"`
}

// Door delivery states.
const (
	DoorQueued    = "queued"
	DoorDelivered = "delivered"
	DoorRefused   = "refused"
	DoorDropped   = "dropped"
)

// DoorDelivery is one delivery as the door keeps it (never its body).
type DoorDelivery struct {
	ID        string `json:"id"`
	Task      string `json:"task"`
	Delivery  string `json:"delivery,omitempty"` // the sender's delivery id
	Event     string `json:"event,omitempty"`
	Bytes     int    `json:"bytes"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Status    int    `json:"status,omitempty"` // the Mezze app's last HTTP status
	Error     string `json:"error,omitempty"`
	Received  int64  `json:"received"`
	Ended     int64  `json:"ended,omitempty"`
	NextTryAt int64  `json:"next_try_at,omitempty"`
}

// DoorDeliveryList is GET /v0/door/tasks/{task}/deliveries.
type DoorDeliveryList struct {
	Deliveries []DoorDelivery `json:"deliveries"`
}

// DoorDeliveryOp is a door_delivery machine op: POST Body with Headers to
// Target on the machine. Body travels in memory, never in team_ops.
type DoorDeliveryOp struct {
	Task    string              `json:"task"`
	Target  string              `json:"target"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// DoorDeliveryResult is the machine's answer: the Mezze app's HTTP status.
type DoorDeliveryResult struct {
	Status int `json:"status"`
}

// DoorTargetMax bounds a task's target URL.
const DoorTargetMax = 1000

// CheckDoorTarget refuses a door target that is not the task's own hook
// URL on its machine: http to a loopback address (or localhost) with a
// path under /api/hooks/, no user, query kept, no fragment. The door
// checks it when a task is registered, the machine again before posting.
func CheckDoorTarget(raw string) error {
	if raw == "" || len(raw) > DoorTargetMax {
		return api.Errorf(api.CodeBadRequest, "target must be 1-%d bytes", DoorTargetMax)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return api.Errorf(api.CodeBadRequest, "target must be an http URL on this machine, like http://127.0.0.1:3773/api/hooks/<task>/<token>")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); !(host == "localhost" || ip != nil && ip.IsLoopback()) {
		return api.Errorf(api.CodeBadRequest, "target host %q is not loopback: the door posts only to the Mezze app on the task's own machine", host)
	}
	if !strings.HasPrefix(u.EscapedPath(), "/api/hooks/") || strings.Contains(u.Path, "/../") || strings.HasSuffix(u.Path, "/..") {
		return api.Errorf(api.CodeBadRequest, "target path must be under /api/hooks/ (the Mezze app task's own hook URL)")
	}
	return nil
}

// DoorDropHeader reports whether a delivery's header stays at the door:
// hop-by-hop and framing headers, Host, cookies, Cloudflare's and proxies'
// own, and zm's under either name (Zm-*, Potluck-*; SPEC 18.4). The rest go to the Mezze app as they came.
func DoorDropHeader(name string) bool {
	k := http.CanonicalHeaderKey(name)
	switch k {
	case "Host", "Content-Length", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Cookie", "Cdn-Loop", "X-Real-Ip":
		return true
	}
	return strings.HasPrefix(k, "Cf-") || strings.HasPrefix(k, "X-Forwarded-") || strings.HasPrefix(k, "Zm-") || strings.HasPrefix(k, "X-Zm-") ||
		strings.HasPrefix(k, "Potluck-") || strings.HasPrefix(k, "X-Potluck-")
}
