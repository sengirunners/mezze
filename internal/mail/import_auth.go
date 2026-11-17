package mail

import (
	"context"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

func (s *Service) authorizeImporter(ctx context.Context, sub *authz.Subject) error {
	deny := func() error {
		return api.Errorf(api.CodeForbidden, "legacy import requires an authenticated owner or aware node-bound human importing to self")
	}
	if sub == nil || sub.Identity.ID == "" || sub.Identity.Kind != api.IdentityHuman || sub.Session != "" || sub.Address != "human:"+sub.Identity.Name {
		return deny()
	}
	local := s.d.Projects.LocalIdentity()
	if sub.Via == authz.ViaLocal && !sub.Remote && sub.Identity.ID == local.ID && sub.Node == "" && sub.TokenID == "" {
		return nil
	}
	remote := core.RemoteOf(ctx)
	if sub.Via != authz.ViaToken || !sub.Remote || sub.TokenID == "" || sub.Node == "" || sub.Machine == "" || sub.Cap == nil || sub.Cap.Project == "" || !sub.PersonalMail || remote == nil || remote.NodeID != sub.Node {
		return deny()
	}
	in := core.ResolveInputOf(ctx)
	if in.Explicit == "" || in.ExplicitFrom != project.RuleFlag {
		return api.Errorf(api.CodeBadRequest, "authority import requires an explicit project")
	}
	return nil
}
