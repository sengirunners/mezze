package orchcore

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// PresenceSource is the session module's daemon-internal heartbeat inventory.
// It includes only this machine's sessions, across all projects and none;
// never expose it to callers as a scoped list or replace UI pagination with it.
// Non-ended sessions are complete or the call fails with no partial result.
// Ended sessions with outcomes from the last 24 hours are capped separately,
// newest first. liveLimit must be 1..10000; endedLimit may be 0..500.
// Stored fields and lineage come from one snapshot; InUse is a best-effort
// local process overlay. No remote presence, prompts, or process samples.
type PresenceSource interface {
	PresenceSessions(ctx context.Context, liveLimit, endedLimit int) ([]orchapi.Session, error)
}
