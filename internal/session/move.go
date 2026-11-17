package session

// Moving a session between projects (SPEC 8). A session started in no
// project (a shell opened in ~) follows its agent: when a hook reports a cwd
// that a cwd rule (cwd-file, cwd-path, cwd-remote, or a joined team
// project's checkout) resolves, a session still in "none" moves there. A
// session with a real project only moves by hand (POST
// /v0/sessions/{id}/project), never while it holds a lease.

import (
	"context"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// cwdRules are the resolution rules a session may follow from its agent's
// cwd: never an explicit project, the session's own, or default_project.
var cwdRules = map[string]bool{project.RuleCwdFile: true, project.RuleCwdPath: true, project.RuleCwdRemote: true}

// cwdTarget is the project (and area) cwd alone resolves to for the caller,
// if the caller may write there.
func (s *Service) cwdTarget(ctx context.Context, cwd string) (api.ProjectRef, string, bool) {
	sub := core.SubjectOf(ctx)
	if sub == nil || cwd == "" || !core.IsLocal(ctx) {
		return api.ProjectRef{}, "", false
	}
	// As the launcher, not as the session (whose own project is none), with
	// nothing but the cwd.
	bare := *sub
	bare.Session = ""
	in := project.ResolveInput{Cwd: cwd}
	bctx := core.WithResolveInput(core.WithSubject(ctx, &bare), in)
	if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok && bare.Via == authz.ViaLocal {
		if p, a, ok, err := tr.TeamTarget(bctx, cwd, ""); err == nil && ok {
			return p, a, true
		}
	}
	res := s.d.Projects.Resolve(bare.Scope, in)
	if res.Project == nil || !cwdRules[res.Rule] || !bare.Can(authz.Write, authz.Object{Project: res.Project.ID, Area: res.Area}) {
		return api.ProjectRef{}, "", false
	}
	return *res.Project, res.Area, true
}

// follow moves a session still in project none to the project its agent's
// cwd resolves to (a hook reported it). It never moves a session with a
// project, nor one holding a lease; a cwd already tried is not resolved again.
func (s *Service) follow(ctx context.Context, r row, cwd string) {
	if r.Project != api.ProjectNone || r.State == orchapi.StateEnded || cwd == "" {
		return
	}
	if last, ok := s.followed.Load(r.ID); ok && last.(string) == cwd {
		return
	}
	s.followed.Store(r.ID, cwd)
	p, area, ok := s.cwdTarget(ctx, cwd)
	if !ok || p.ID == api.ProjectNone || s.holdsLease(ctx, r) {
		return
	}
	if _, err := s.move(ctx, r.ID, p.ID, area, true); err != nil && !api.IsCode(err, api.CodeConflict) {
		s.d.Log.Warn("session follows its cwd", "session", r.Address, "err", err)
	}
}

// holdsLease reports a live lease held by the session in its project.
func (s *Service) holdsLease(ctx context.Context, r row) bool {
	if lh, ok := orchcore.Work(s.d).(orchcore.LeaseHolders); ok {
		keys, err := lh.HeldBy(ctx, r.Project, r.Address)
		return err != nil || len(keys) > 0
	}
	return r.Work != ""
}

// move puts a session in project/area and publishes dev.zm.session.updated.
// onlyFromNone refuses (conflict) a session that has a project by then.
func (s *Service) move(ctx context.Context, id8, projectID, area string, onlyFromNone bool) (orchapi.Session, error) {
	var task bool
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_tasks WHERE id=?)`, id8).Scan(&task); err != nil {
		return orchapi.Session{}, err
	}
	moved := false
	var fromProject, fromArea string
	ss, err := s.update(ctx, id8, "", func(r *row) (bool, error) {
		switch {
		case r.State == orchapi.StateEnded:
			return false, api.Errorf(api.CodeConflict, "session %s has ended", r.Address)
		case task && r.Project != projectID:
			return false, api.Errorf(api.CodeConflict, "task sessions cannot move between projects")
		case onlyFromNone && r.Project != api.ProjectNone:
			return false, api.Errorf(api.CodeConflict, "session %s already has a project", r.Address)
		case r.Project == projectID && r.Area == area:
			return false, nil
		}
		fromProject, fromArea = r.Project, r.Area
		r.Project, r.Area, moved = projectID, area, true
		return true, nil
	})
	if err == nil {
		s.scopes.Store(ss.Address, object(ss))
		s.followed.Delete(id8)
		if moved {
			s.publishLeft(ss.Address, fromProject, fromArea) // the old log: it left, not where to
			s.updates.settle(ss.Address, false)
			s.publishNow(orchapi.EventSessionUpdated, ss) // a move is never held back
		}
	}
	return ss, err
}

// hMove is POST /v0/sessions/{id}/project: a session moved by hand.
func (s *Service) hMove(w http.ResponseWriter, r *http.Request) {
	var req orchapi.MoveRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	row, err := s.operable(ctx, sub, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	ss, err := s.moveTo(ctx, sub, row, req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.writeSessionResponse(w, r, http.StatusOK, ss)
}

// moveTo checks and applies a move by hand.
func (s *Service) moveTo(ctx context.Context, sub *authz.Subject, r row, req orchapi.MoveRequest) (orchapi.Session, error) {
	ref, area := strings.TrimSpace(req.Project), strings.TrimSpace(req.Area)
	if slug, a, ok := strings.Cut(ref, "/"); ok {
		if area != "" && area != a {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "area given twice (%q and %q)", a, area)
		}
		ref, area = slug, a
	}
	if ref == "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "project is required (a slug, ULID, slug/area, or none)")
	}
	pid := ""
	switch {
	case ref == api.ProjectNone:
		if area != "" {
			return orchapi.Session{}, api.NotFound("area", area)
		}
		if err := sub.Check(authz.Write, authz.Object{Project: api.ProjectNone}, "project", api.ProjectNone); err != nil {
			return orchapi.Session{}, err
		}
		pid = api.ProjectNone
	default:
		explicit := ref
		if area != "" {
			explicit += "/" + area
		}
		if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok && sub.Via == authz.ViaLocal {
			// A team project this machine joined (SPEC 17.3).
			in := project.ResolveInput{Explicit: explicit, ExplicitFrom: project.RuleFlag}
			p, a, ok, err := tr.TeamTarget(core.WithResolveInput(ctx, in), "", area)
			if err != nil {
				return orchapi.Session{}, err
			}
			if ok {
				pid, area = p.ID, a
			}
		}
		if pid == "" {
			p, err := s.d.Projects.Get(sub.Scope, ref)
			if err != nil {
				return orchapi.Session{}, err
			}
			if area != "" && (!s.d.Projects.HasArea(p.ID, area) || !sub.Can(authz.Read, authz.Object{Project: p.ID, Area: area})) {
				return orchapi.Session{}, api.NotFound("area", area)
			}
			if err := sub.Check(authz.Write, authz.Object{Project: p.ID, Area: area}, "project", p.Slug); err != nil {
				return orchapi.Session{}, err
			}
			pid = p.ID
		}
	}
	if r.Project == pid && r.Area == area {
		return r.Session, nil
	}
	if s.holdsLease(ctx, r) {
		return orchapi.Session{}, api.Errorf(api.CodeConflict, "session %s holds a lease in its project", r.Address).
			WithHint("release it first (zm release), then move the session")
	}
	return s.move(ctx, r.ID, pid, area, false)
}
