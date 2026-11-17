package team

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// Routes registers the team routes (API.md "Teams"); being joined (join,
// leave, invites, remotes, people, operator classes) is teamlink's.
func (s *Service) Routes(r *core.Router) {
	r = r.For("team")
	// The authority's side.
	r.HandleFunc("GET /v0/sync", core.Authed, "read on a team project; the caller's visible changes only (a guest's tombstones for items that left its area)", jsonH(s.handleSync))
	r.HandleFunc("GET /v0/sync/snapshot", core.Authed, "read on a team project; what the caller may read, redacted for it", s.handleSnapshot)
	r.HandleFunc("POST /v0/team/writes", core.Authed, "a replica's node-bound token only; each write runs as its own route with that route's checks", jsonH(s.handleWrites))
	r.HandleFunc("POST /v0/team/writes/{id}/cancel", core.Authed, "a replica's node-bound token only (settles that write id: ran, else cancelled for good), or this machine's owner on its socket (sent on to a joined project's authority)", jsonH(s.handleCancel))
	// The replica's side.
	r.HandleFunc("GET /v0/team/outbox", core.Local, "local", jsonH(s.handleOutbox))
	s.taskRoutes(r)    // machine choice (SPEC 8): the authority's side
	s.lineRoutes(r)    // its waiting line's list (SPEC 8 "Waiting line")
	s.relayRoutes(r)   // structured sessions on other machines
	s.sessionRoutes(r) // starting one there
	s.t3Routes(r)      // the Mezze app on a machine (SPEC 8)
	s.personRoutes(r)  // a person's own machines
}

// moveLeases is teamlink's Hooks.MoveLeases: zm project team|solo moves
// the project's leases (the work module records the mode).
func (s *Service) moveLeases(ctx context.Context, project string, toTeam bool) (int, error) {
	m, ok := orchcore.Work(s.d).(orchcore.LeaseMover)
	if !ok {
		return 0, api.Errorf(api.CodeUnavailable, "the work module cannot move leases")
	}
	return m.MoveLeases(ctx, project, toTeam)
}

// remoteView is teamlink's Hooks.View: a remote's copy and its unsent
// writes in GET /v0/team/remotes.
func (s *Service) remoteView(x *remote, rm *api.Remote) {
	if rp := rep(x); rp != nil {
		v := rp.view()
		rm.Replica = &v
	}
	rm.Unsent, rm.UnsentCount = unsentSaved(s.replicaPath(rm.Project.ID))
}
