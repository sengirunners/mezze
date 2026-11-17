package team

import (
	"context"

	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// linked is a bare teamlink holding xs (empty deps, no hooks; New reads
// d.Projects, so it is never nil).
func linked(xs ...*remote) *teamlink.Service {
	l := teamlink.New(&core.Deps{})
	for _, x := range xs {
		l.Add(x)
	}
	return l
}

// link gives s a teamlink over s.d with team's hooks, holding xs, its
// recovery running under ctx.
func (s *Service) link(ctx context.Context, xs ...*remote) {
	s.Service = teamlink.New(s.d)
	s.hook()
	for _, x := range xs {
		s.Add(x)
	}
	s.Run(ctx)
}
