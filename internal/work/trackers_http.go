package work

import (
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// trackerRoutes registers /v0/trackers (SPEC 17.2). All local: a binding
// is what the daemon runs and reads on this machine.
func (s *Service) trackerRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/trackers", core.Local, "local: every binding in ReadScope (?project=), never a password", s.jsonH(func(r *http.Request, _ *authz.Subject, sc authz.Scope) (any, error) {
		return s.trackerList(r.Context(), sc), nil
	}))
	r.HandleFunc("POST /v0/trackers", core.Local, "local: admin on the resolved project; applied live, checked, rolled back on a failing check", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.AddTrackerRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.addTracker(r.Context(), req)
	}))
	r.HandleFunc("POST /v0/trackers/{prefix}/test", core.Local, "local: admin on the resolved project; read-only checks and a sync", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.testTracker(r.Context(), r.PathValue("prefix"))
	}))
	r.HandleFunc("DELETE /v0/trackers/{prefix}", core.Local, "local: admin on the resolved project; unbinds live, keeps the tracker's data", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.removeTracker(r.Context(), r.PathValue("prefix"))
	}))
}
