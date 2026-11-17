package mail

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// followMoved rewrites a recipient or ref that names a native issue by the id
// it had before it moved, and answers the project the mail now belongs to: the
// issue's, where its holder and area are. An id that did not move, or that sub
// cannot see, stays as typed and gets the usual answer. Mail to everyone or to
// the owners stays in the project it was sent in.
func (s *Service) followMoved(ctx context.Context, sub *authz.Subject, req *orchapi.SendMailRequest) (api.ProjectRef, bool) {
	mv, ok := orchcore.Work(s.d).(orchcore.WorkMoves)
	if !ok || sub == nil {
		return api.ProjectRef{}, false
	}
	if in := core.ResolveInputOf(ctx); in.Explicit != "" {
		// A project the caller cannot name: WriteTarget answers, the same for
		// a hidden one and a missing one.
		if _, err := s.d.Projects.Get(sub.Scope, strings.SplitN(in.Explicit, "/", 2)[0]); err != nil {
			return api.ProjectRef{}, false
		}
	}
	var p api.ProjectRef
	moved := false
	follow := func(id *string) {
		if a, err := api.ParseAddress(*id); *id == "" || err != nil || a.Kind != api.KindWork {
			return
		}
		if to, nid, ok, err := mv.MovedItem(ctx, sub, *id); err == nil && ok {
			*id = nid
			if !moved {
				p, moved = to, true
			}
		}
	}
	if a, err := api.ParseAddress(req.To); err == nil && (a.Kind == api.KindBroadcast || a.Kind == api.KindOwners) {
		return api.ProjectRef{}, false
	}
	follow(&req.To)
	follow(&req.Ref)
	return p, moved
}
