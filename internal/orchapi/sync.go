package orchapi

import (
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// SyncResponse is GET /v0/sync: the caller's visible changes after since,
// oldest first. Cursor is where to resume (an id the caller was sent, or
// since unchanged); More says another page is waiting. Now is the
// authority's clock (stale ages use it).
type SyncResponse struct {
	Changes []api.Event `json:"changes"`
	Cursor  string      `json:"cursor"`
	More    bool        `json:"more,omitempty"`
	Now     time.Time   `json:"now"`
	// Backup is the authority's off-site home backup (SPEC 17.9), for a
	// replica that reads the project whole; a replica shows it on its
	// authority row of GET /v0/work/sources.
	Backup *HomeBackup `json:"backup,omitempty"`
	// Started is when the authority's daemon started, for the same reader.
	Started time.Time `json:"started,omitzero"`
}

// Snapshot line kinds (GET /v0/sync/snapshot, NDJSON).
const (
	SnapItem    = "item"    // WorkItem (no comments)
	SnapComment = "comment" // Comment
	SnapLease   = "lease"   // Lease
	SnapMail    = "mail"    // Mail
)
