package orchcore

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// SessionMetadataDelivery is an optional SessionService capability for consumers
// returning cached session snapshots. The session owner projects their display
// text again and serializes the callback's actual delivery with redaction.
// Length, order, identities, scope and lifecycle state remain unchanged. The
// caller still checks visibility and reapplies its usual field projection.
// consume must finish the write before returning and must not reenter delivery.
type SessionMetadataDelivery interface {
	DeliverSessionMetadata(context.Context, []orchapi.Session, func([]orchapi.Session) error) error
}

// DeliverSessionMetadata uses the session owner's delivery barrier when it is
// available. Legacy services and fakes retain their existing values and behavior.
func DeliverSessionMetadata(ctx context.Context, service SessionService, snapshots []orchapi.Session, consume func([]orchapi.Session) error) error {
	if delivery, ok := service.(SessionMetadataDelivery); ok {
		return delivery.DeliverSessionMetadata(ctx, snapshots, consume)
	}
	return consume(snapshots)
}
