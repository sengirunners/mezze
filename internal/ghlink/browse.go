package ghlink

import (
	"context"

	"github.com/sengirunners/potluck/kit/api"
)

func (s *Service) browseFetch(ctx context.Context, project string, args ...string) ([]byte, error) {
	s.reap(ctx, project)
	l, err := s.link(ctx, project)
	if err != nil && !api.IsCode(err, api.CodeNotFound) {
		return nil, err
	}
	if err != nil {
		l = link{Project: project}
	}
	if l.Host != "" && l.Host != "github.com" {
		return nil, api.Errorf(api.CodeUnavailable, "GitHub browsing supports github.com mappings only")
	}
	l.Host = "github.com"
	// Browsing reuses the daemon's existing gh login/token-file path. It does
	// not create a linker, mint a token, or accept credentials from a client.
	return s.ghLimit(ctx, l, (2<<20)+1, args...)
}
