package session

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
)

var _ orchcore.SessionMetadataDelivery = (*Service)(nil)

// DeliverSessionMetadata closes the lookup-to-write gap for other bricks. Each
// cached snapshot is projected again under the same barrier as session output;
// the callback finishes the actual send before a redactor can change that view.
func (s *Service) DeliverSessionMetadata(ctx context.Context, snapshots []orchapi.Session, consume func([]orchapi.Session) error) error {
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	projected := make([]orchapi.Session, len(snapshots))
	for i, snapshot := range snapshots {
		projected[i] = s.maskSessionOutputMetadata(ctx, snapshot)
	}
	return consume(projected)
}
