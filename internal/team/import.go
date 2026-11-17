package team

import (
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// importReplayTarget follows the import handler's query-over-header selection
// and ordinary WriteTarget authorization before LIVE replay. It performs no
// import, grant, source read or destination mutation.
func (s *Service) importReplayTarget(r *http.Request, sub *authz.Subject) (core.Target, error) {
	ctx := r.Context()
	if named := r.URL.Query().Get("project"); named != "" {
		ctx = core.WithResolveInput(ctx, project.ResolveInput{Explicit: named, ExplicitFrom: project.RuleFlag})
	}
	in := core.ResolveInputOf(ctx)
	if in.Explicit == "" || in.ExplicitFrom != project.RuleFlag {
		return core.Target{}, api.Errorf(api.CodeBadRequest, "authority import requires an explicit project")
	}
	target, err := s.d.WriteTarget(ctx, "", authz.Write)
	if err != nil {
		return core.Target{}, err
	}
	if target.Project.ID != sub.Cap.Project {
		return core.Target{}, api.Errorf(api.CodeForbidden, "import requires the authenticated project authority")
	}
	return target, nil
}

// importRoute forwards one explicit project to its existing authority. Imports
// have no replica sink/outbox, cwd resolution, source lookups or local fallback.
func (s *Service) importRoute(w http.ResponseWriter, r *http.Request) bool {
	sub := core.SubjectOf(r.Context())
	local := s.d.Projects.LocalIdentity()
	if sub == nil || sub.Via != authz.ViaLocal || sub.Session != "" || sub.Identity.Kind != api.IdentityHuman || sub.Identity.ID != local.ID || sub.Address != "human:"+local.Name {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "only this machine's owner human can forward a self import"))
		return true
	}
	named := r.URL.Query().Get("project")
	if named == "" && api.GetHeader(r.Header, api.HeaderProjectFrom) != project.RuleEnv {
		named = api.GetHeader(r.Header, api.HeaderProject)
	}
	if named == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "authority import requires an explicit project"))
		return true
	}
	slug, area, _ := strings.Cut(named, "/")
	if _, local := s.d.Projects.Lookup(slug); local {
		return false
	}
	x := s.ByName(slug)
	if x == nil {
		return false
	}
	resp, err := s.ForwardResp(r, x, area, true, nil)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	defer resp.Body.Close()
	teamlink.CopyResponse(w, resp)
	return true
}
