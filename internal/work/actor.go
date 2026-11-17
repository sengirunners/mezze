package work

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

// The actor of a tracker write (SPEC 11 "actor"): the zm identity a write
// verb is made for, so an adapter can attribute it (write through that
// identity's own account on the tracker).

type actorKey struct{}

// withActor marks ctx as acting for sub's identity (not for the daemon's
// own subject).
func withActor(ctx context.Context, sub *authz.Subject) context.Context {
	if sub == nil || sub.Via == authz.ViaSystem || sub.Identity.Name == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey{}, sub.Identity.Name)
}

// actorOf is ctx's actor, else the launcher of the claim or release's
// holder (a team write-back, or an expiry, has no caller), else "".
func (s *Service) actorOf(ctx context.Context, args any) string {
	if a, _ := ctx.Value(actorKey{}).(string); a != "" {
		return a
	}
	var holder string
	switch v := args.(type) {
	case orchapi.AdapterClaimArgs:
		holder = v.Holder
	case orchapi.AdapterReleaseArgs:
		holder = v.Holder
	}
	if holder == "" || orchcore.Sessions(s.d) == nil {
		return ""
	}
	ss, ok := orchcore.Sessions(s.d).Lookup(ctx, holder)
	if !ok || ss.Launcher == "" {
		return ""
	}
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return ""
	}
	for _, id := range ids {
		if id.ID == ss.Launcher || id.Name == ss.Launcher {
			return id.Name
		}
	}
	return ""
}
