package orchapi

import (
	"time"
)

// ConnectionSchema is the summary's schema name. Fields are added, never
// repurposed; a new major version is a new name.
const ConnectionSchema = "zm.connection.show/1"

// Headline row states.
const (
	ConnectionOK   = "ok"
	ConnectionWarn = "warn"
	ConnectionRed  = "red"
)

// What zm makes of a connection's last answers (ConnectionShow.State).
const (
	ConnectionFresh       = "fresh"       // as_of is within the provider's stale_after_s
	ConnectionStale       = "stale"       // older, or the last fetch failed: the last good summary, marked
	ConnectionUnavailable = "unavailable" // nothing good within ConnectionMaxAge: no summary
)

// ConnectionSummary is what a provider answers. Every item carries an
// account: whose slice of the provider it is (a customer, an org, a pool).
type ConnectionSummary struct {
	Schema string `json:"schema"`
	Title  string `json:"title,omitempty"`
	// AsOf is when the provider's numbers were true (RFC 3339).
	AsOf time.Time `json:"as_of"`
	// StaleAfterS: seconds after as_of that the summary counts as stale (default 300, 30 to 86400).
	StaleAfterS int                 `json:"stale_after_s,omitempty"`
	Headline    []ConnectionRow     `json:"headline"`
	Meters      []ConnectionMeter   `json:"meters"`
	Running     []ConnectionRunning `json:"running"`
}

// ConnectionRow is one headline: "Queue: warn, 14, since 10:02".
type ConnectionRow struct {
	Key     string     `json:"key"`
	Label   string     `json:"label"`
	State   string     `json:"state"` // ok | warn | red
	Count   *int64     `json:"count,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	Detail  string     `json:"detail,omitempty"`
	Account string     `json:"account"`
}

// ConnectionMeter is a capacity: used and reserved out of total, in unit.
// used + reserved may pass total (overcommit); clients clamp the bar.
type ConnectionMeter struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Used     float64 `json:"used"`
	Reserved float64 `json:"reserved,omitempty"`
	Total    float64 `json:"total"`
	Unit     string  `json:"unit"`
	Account  string  `json:"account"`
}

// ConnectionRunning is one thing in progress (a job, a deploy). Its age is
// now - started, worked out by the client.
type ConnectionRunning struct {
	ID      string           `json:"id"`
	Title   string           `json:"title"`
	Owner   string           `json:"owner,omitempty"`
	Started *time.Time       `json:"started,omitempty"`
	Links   []ConnectionLink `json:"links,omitempty"`
	Account string           `json:"account"`
}

// ConnectionLink is an http(s) link shown on an item.
type ConnectionLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Connection is one registered provider (never its token).
type Connection struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project"` // slug
	Area    string `json:"area,omitempty"`
	// Endpoint is shown to the connection's admins only.
	Endpoint     string    `json:"endpoint,omitempty"`
	AllowPrivate bool      `json:"allow_private,omitempty"`
	Note         string    `json:"note,omitempty"`
	CreatedBy    string    `json:"created_by,omitempty"` // admins only
	Created      time.Time `json:"created"`
}

// ConnectionList is GET /v0/connections.
type ConnectionList struct {
	Connections []Connection `json:"connections"`
}

// NewConnectionRequest is POST /v0/connections.
type NewConnectionRequest struct {
	Project  string `json:"project"` // slug[/area]
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"` // https://... or unix:/abs/path.sock[?path=/show]
	// Token is sent as "Authorization: Bearer <token>". TokenRef (a secrets
	// reference: file:, keychain:, env:) is the machine owner's on its socket only.
	Token        string `json:"token,omitempty"`
	TokenRef     string `json:"token_ref,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
	Note         string `json:"note,omitempty"`
}

// ConnectionShow is GET /v0/connections/{id}/show.
type ConnectionShow struct {
	Connection Connection `json:"connection"`
	State      string     `json:"state"` // fresh | stale | unavailable
	// Reason says in plain words why it is not fresh.
	Reason    string             `json:"reason,omitempty"`
	CheckedAt *time.Time         `json:"checked_at,omitempty"` // zm's last fetch attempt
	FetchedAt *time.Time         `json:"fetched_at,omitempty"` // zm's last good fetch
	Summary   *ConnectionSummary `json:"summary,omitempty"`
}
