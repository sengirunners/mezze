package work

import (
	"context"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

var _ orchcore.WorkReader = (*Service)(nil)

func (s *Service) ReadWork(ctx context.Context, sub *authz.Subject, q orchapi.WorkQuery) (orchapi.WorkList, error) {
	ctx = core.WithSubject(ctx, sub)
	sc, err := s.d.ReadScope(ctx, q.Project)
	if err != nil {
		return orchapi.WorkList{}, err
	}
	return s.list(ctx, sub, sc, q)
}
