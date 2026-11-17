package session

// Session names (SPEC 8 "Names"; identifiers-2026-11-01 stage 3): an
// editable alias beside the handle <machine>/<session8>. A name is unique
// among this machine's live sessions (the sessions_live_name index) and
// resolves only while its session is live: every route that takes a
// session takes <name> or <this machine>/<name>, and mail stores the
// address the name resolved to, so a name used again later never
// redirects older mail.

import (
	"context"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// loadRef is load for a caller's input (a route's {id}): a session8, an
// address, else a live session's name (the two never overlap: a name is
// never a session8).
func (s *Service) loadRef(ctx context.Context, id string) (row, bool) {
	if r, ok := s.load(ctx, id); ok {
		return r, true
	}
	r, ok, err := s.loadByName(ctx, id)
	if err != nil {
		s.d.Log.Warn("session name", "id", id, "err", err)
	}
	return r, ok
}

// ResolveName implements orchcore.SessionNames: the live session on this
// machine that ref (<name> or <machine>/<name>) names now.
func (s *Service) ResolveName(ctx context.Context, ref string) (orchapi.Session, bool) {
	r, ok, err := s.loadByName(ctx, ref)
	if err != nil {
		s.d.Log.Warn("session name", "ref", ref, "err", err)
	}
	return s.maskSessionOutputMetadata(ctx, r.Session), ok
}

// loadByName returns the live session on this machine that id names
// (<name> or <machine>/<name>); false when id is no name or names none.
func (s *Service) loadByName(ctx context.Context, id string) (row, bool, error) {
	m, name, ok := api.SessionNameRef(id)
	if !ok || (m != "" && m != s.d.Config.Machine) {
		return row{}, false, nil
	}
	return byName(ctx, s.d.Store.Registry(), name)
}

// cleanName lowercases and checks a requested name ("" stays "": none).
func cleanName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || api.ValidSessionName(name) {
		return name, nil
	}
	return "", api.Errorf(api.CodeBadRequest, "bad session name %q", name).
		WithHint("a-z, 0-9, '.', '_' and '-', up to 63; not 8 letters and digits alone (that is a session id: add a '-'), not none, all or owners")
}

// nameConflict is the answer when name belongs to another live session;
// it names that session only to a caller who may read it.
func (s *Service) nameConflict(ctx context.Context, sub *authz.Subject, name string) error {
	e := api.Errorf(api.CodeConflict, "the name %s is taken by a live session on %s", name, s.d.Config.Machine)
	if r, ok, _ := byName(ctx, s.d.Store.Registry(), name); ok && sub != nil && sub.Can(authz.Read, object(r.Session)) {
		e = api.Errorf(api.CodeConflict, "the name %s is taken by %s", name, r.Address)
	}
	return e.WithHint("pick another name, or clear that one: zm session name <session> \"\"")
}

// setName names session id8 (name "" clears it). An ended session keeps
// the name it had and takes no new one.
func (s *Service) setName(ctx context.Context, sub *authz.Subject, id8, name string) (orchapi.Session, error) {
	name, err := cleanName(name)
	if err != nil {
		return orchapi.Session{}, err
	}
	ss, err := s.update(ctx, id8, orchapi.EventSessionUpdated, func(r *row) (bool, error) {
		if r.Name == name {
			return false, nil
		}
		if r.State == orchapi.StateEnded {
			return false, api.Errorf(api.CodeConflict, "session %s has ended", r.Address)
		}
		r.Name = name
		return true, nil
	})
	if nameTaken(err) {
		return orchapi.Session{}, s.nameConflict(ctx, sub, name)
	}
	return ss, err
}

func (s *Service) hName(w http.ResponseWriter, r *http.Request) {
	var req orchapi.NameRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub, id := core.SubjectOf(r.Context()), r.PathValue("id")
	row, err := s.visible(r.Context(), sub, id)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if sub.Session != row.Address {
		if err := sub.Check(authz.Operate, object(row.Session), "session", id); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	if !s.capGate(sub).allows(r.Context(), row, authz.Operate, true) { // a capped process: its own sessions only
		api.WriteError(w, api.NotFound("session", id))
		return
	}
	ss, err := s.setName(r.Context(), sub, row.ID, req.Name)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.writeSessionResponse(w, r, http.StatusOK, ss)
}
