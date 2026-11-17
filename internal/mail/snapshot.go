package mail

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

var _ orchcore.Snapshotter = (*Service)(nil)

// Snapshot emits every mail of project that sub may read ("mail",
// orchapi.Mail, redacted for sub), as GET /v0/mail would list it with no
// filter and no limit: a team replica's snapshot (SPEC 17.6).
func (s *Service) Snapshot(ctx context.Context, sub *authz.Subject, project string, emit func(kind string, v any) error) error {
	if _, err := s.d.Store.Project(sub.Scope, authz.Read, project); err != nil {
		return err
	}
	rs, err := s.query(ctx, sub.Scope.Only(project), sub.Address, 1<<30, func(scoped) (string, []any, bool) {
		if !authz.PersonalMailReader(sub) {
			return "audience = ''", nil, true
		}
		return "1=1", nil, true
	})
	if err != nil {
		return err
	}
	for _, m := range s.redactAll(ctx, sub, rs) {
		if err := emit("mail", m); err != nil {
			return err
		}
	}
	return nil
}
