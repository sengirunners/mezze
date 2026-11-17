package orchapi

import (
	"time"
)

// Webhook event types. A ping is never on the bus: it is what
// POST /v0/webhooks/{id}/test sends. A disabled hook is announced on the
// machine log (owner only).
const (
	EventWebhookPing     = "dev.zm.webhook.ping"
	EventWebhookDisabled = "dev.zm.webhook.disabled" // machine-level; data Webhook
)

// WebhookPing is dev.zm.webhook.ping's data: the hook tested and who asked.
// Fields in key order, so the bytes match the map it replaced.
type WebhookPing struct {
	By      string `json:"by"`
	Webhook string `json:"webhook"`
}

// Headers on every webhook delivery.
const (
	HeaderWebhookID        = "Zm-Webhook-Id"
	HeaderWebhookDelivery  = "Zm-Webhook-Delivery"  // stable across retries: dedupe on it
	HeaderWebhookEvent     = "Zm-Webhook-Event"     // the event type
	HeaderWebhookTimestamp = "Zm-Webhook-Timestamp" // Unix seconds when this attempt was signed
	// HeaderWebhookSignature is "sha256=" + hex HMAC-SHA256(secret,
	// timestamp + "." + body). Deprecated: use the Standard Webhooks headers.
	HeaderWebhookSignature = "Zm-Webhook-Signature"
	// Standard Webhooks (standardwebhooks.com): the message id (the delivery
	// id, the same across retries), Unix seconds of this attempt, and
	// "v1,<base64 HMAC-SHA256(key, id.timestamp.body)>".
	HeaderStdWebhookID        = "webhook-id"
	HeaderStdWebhookTimestamp = "webhook-timestamp"
	HeaderStdWebhookSignature = "webhook-signature"
	// HeaderHubSignature is GitHub's header, "sha256=" + hex HMAC-SHA256
	// (secret, body), for receivers that already verify GitHub webhooks.
	HeaderHubSignature = "X-Hub-Signature-256"
)

// Webhook states.
const (
	WebhookActive   = "active"
	WebhookDisabled = "disabled"
)

// Webhook is one outgoing hook as its admins see it (never the secret).
type Webhook struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Project string `json:"project,omitempty"` // slug; "" = every project its creator sees
	Area    string `json:"area,omitempty"`
	// Events are type prefixes ("dev.zm.work.", "dev.zm.mail.sent"); empty = all.
	Events       []string   `json:"events,omitempty"`
	AllowPrivate bool       `json:"allow_private,omitempty"`
	CreatedBy    string     `json:"created_by"` // the creator's address
	Note         string     `json:"note,omitempty"`
	State        string     `json:"state"` // active | disabled
	Disabled     string     `json:"disabled_reason,omitempty"`
	Created      time.Time  `json:"created"`
	LastOK       *time.Time `json:"last_ok,omitempty"`
	FailingSince *time.Time `json:"failing_since,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	Pending      int        `json:"pending"` // deliveries queued
}

// WebhookList is GET /v0/webhooks.
type WebhookList struct {
	Webhooks []Webhook `json:"webhooks"`
}

// NewWebhookRequest is POST /v0/webhooks.
type NewWebhookRequest struct {
	URL string `json:"url"`
	// Project is a slug, ULID or slug/area. Required for every caller but
	// the machine owner on its socket (who may leave it empty: every project).
	Project string   `json:"project,omitempty"`
	Events  []string `json:"events,omitempty"` // "work.*", "mail.sent", "dev.zm.work.claimed"
	// Secret signs deliveries; empty = the daemon makes one and returns it once.
	Secret string `json:"secret,omitempty"`
	// AllowPrivate lets the hook reach loopback, private, link-local and
	// tailnet addresses. Only the machine owner on its socket may set it.
	AllowPrivate bool   `json:"allow_private,omitempty"`
	Note         string `json:"note,omitempty"`
}

// NewWebhookResponse carries the secret once.
type NewWebhookResponse struct {
	Webhook Webhook `json:"webhook"`
	Secret  string  `json:"secret,omitempty"` // only when the daemon made it
}

// UpdateWebhookRequest is PATCH /v0/webhooks/{id}.
type UpdateWebhookRequest struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// WebhookDelivery is one attempt in a hook's delivery log.
type WebhookDelivery struct {
	Delivery string    `json:"delivery"`
	Event    string    `json:"event,omitempty"` // event id
	Type     string    `json:"type"`
	Attempt  int       `json:"attempt"`
	Time     time.Time `json:"time"`
	Status   int       `json:"status,omitempty"` // HTTP status, 0 = no answer
	// Outcome: delivered | retry | failed (gave up) | dropped (queue full) |
	// skipped (no longer visible to the hook's creator) | ping.
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// WebhookDetail is GET /v0/webhooks/{id}: the hook and its latest attempts.
type WebhookDetail struct {
	Webhook    Webhook           `json:"webhook"`
	Deliveries []WebhookDelivery `json:"deliveries"`
}

// WebhookTestResponse is POST /v0/webhooks/{id}/test.
type WebhookTestResponse struct {
	OK       bool            `json:"ok"`
	Delivery WebhookDelivery `json:"delivery"`
}

// GitHub link transitions (what a PR does to the items it names).
const (
	GHOnOpen  = "open"  // PR opened or reopened
	GHOnMerge = "merge" // PR merged
	GHOnClose = "close" // PR closed without merging
)

// GitHubLink is one project's GitHub linking as the machine owner sees it.
type GitHubLink struct {
	Project string   `json:"project"` // slug
	Repos   []string `json:"repos"`   // owner/name
	// Prefixes are the bare id prefixes recognised in branch names, PR
	// titles and bodies ("acme" matches acme-abc12); Sources are the
	// tracker sources a bare id is looked up in (bd -> bd:acme-abc12).
	// "<source>:<ref>" written out is always recognised.
	Prefixes []string `json:"prefixes"`
	Sources  []string `json:"sources"`
	// OnOpen, OnMerge, OnClose: the zm status to move an item to, or "none".
	OnOpen      string     `json:"on_open"`
	OnMerge     string     `json:"on_merge"`
	OnClose     string     `json:"on_close"`
	PollSeconds int        `json:"poll_seconds"` // 0 = webhook receiver only
	Bin         string     `json:"bin,omitempty"`
	Host        string     `json:"host,omitempty"`
	TokenFile   string     `json:"token_file,omitempty"`
	Identity    string     `json:"identity"` // who the item writes are attributed to
	Created     time.Time  `json:"created"`
	LastPoll    *time.Time `json:"last_poll,omitempty"`
	LastEvent   *time.Time `json:"last_event,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Pending     int        `json:"pending"`  // PR snapshots waiting to be applied
	Receiver    string     `json:"receiver"` // the receiver's path
	// Match is the PR texts the matcher reads (SPEC 18.2); Gate the link's
	// gate (18.3), with its describe name and version or the error, and
	// Waiting the item decisions it holds.
	Match     []string `json:"match"`
	Gate      string   `json:"gate,omitempty"`
	GateName  string   `json:"gate_name,omitempty"`
	GateError string   `json:"gate_error,omitempty"`
	Waiting   int      `json:"waiting"`
}

// GitHubLinkList is GET /v0/github/links.
type GitHubLinkList struct {
	Links []GitHubLink `json:"links"`
}

// GitHubLinkRequest is POST /v0/github/links (create or replace).
type GitHubLinkRequest struct {
	Project  string   `json:"project"`
	Repos    []string `json:"repos"`
	Prefixes []string `json:"prefixes,omitempty"`
	Sources  []string `json:"sources,omitempty"`
	OnOpen   string   `json:"on_open,omitempty"`
	OnMerge  string   `json:"on_merge,omitempty"`
	OnClose  string   `json:"on_close,omitempty"`
	// PollSeconds > 0 polls with `gh api` (no inbound port); 0 = receiver only.
	PollSeconds int `json:"poll_seconds,omitempty"`
	// Match: which PR texts name items ("title", "branch", "body"; empty =
	// all three). Gate: an executable asked before every item write (18.3).
	Match     []string `json:"match,omitempty"`
	Gate      string   `json:"gate,omitempty"`
	Bin       string   `json:"bin,omitempty"`
	Host      string   `json:"host,omitempty"`
	TokenFile string   `json:"token_file,omitempty"`
	// Secret verifies the receiver's deliveries (X-Hub-Signature-256);
	// empty = keep the current one, or make one (returned once).
	Secret string `json:"secret,omitempty"`
}

// GitHubLinkResponse carries the receiver secret once, when the daemon made it.
type GitHubLinkResponse struct {
	Link   GitHubLink `json:"link"`
	Secret string     `json:"secret,omitempty"`
}

// GitHubSyncResponse is POST /v0/github/links/{project}/sync.
type GitHubSyncResponse struct {
	PRs     int      `json:"prs"`     // PR snapshots seen
	Applied int      `json:"applied"` // item moves, comments and links made
	Errors  []string `json:"errors,omitempty"`
}
