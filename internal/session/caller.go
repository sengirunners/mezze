package session

import (
	"context"
	"os"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

var _ orchcore.SessionCaller = (*Service)(nil)

// CallerSession implements orchcore.SessionCaller: the nearest registered
// session among the socket caller's process and its ancestors, by the kernel
// peer pid. It walks only the caller's own ancestors (hookOrigin) and waits
// for a spawn in flight only when that walk cannot answer, so it fails closed;
// it errs when the peer cannot be read or no longer exists.
func (s *Service) CallerSession(ctx context.Context, sub *authz.Subject) (string, error) {
	return s.hookOrigin(ctx, sub)
}

// CallerSessionNow implements orchcore.SessionCaller: CallerSession that never
// waits; sure is false when the walk could not answer.
func (s *Service) CallerSessionNow(ctx context.Context, sub *authz.Subject) (origin string, sure bool, err error) {
	pid := core.PeerPIDOf(ctx)
	if pid <= 0 {
		if pid == 0 && sub != nil {
			return sub.Session, true, nil
		}
		return "", pid == 0, nil
	}
	origin, unsure, err := s.ancestryOrigin(ctx, pid, os.Getpid())
	return origin, err == nil && (origin != "" || !unsure), err
}
