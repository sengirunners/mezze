package work

// A native issue by the id it had before it moved (zm issues move, SPEC "Native
// issues": moves). Reads and mail follow the redirect; claim, comment and status
// name the id it has now.

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

var _ orchcore.WorkMoves = (*Service)(nil)

// readH is itemH for a read, which also finds an item by its old id. The
// redirect is asked first: a sync under way can bring the old row back for a while.
func (s *Service) readH(fn func(r *http.Request, sub *authz.Subject, sc authz.Scope, l loc) (any, error)) http.HandlerFunc {
	return s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		id := r.PathValue("id")
		l, found, err := s.locateMoved(r.Context(), sub, id)
		if err == nil && !found {
			l, err = s.locate(r.Context(), sub, sc, id)
		}
		if err != nil {
			return nil, err
		}
		return fn(r, sub, sc, l)
	})
}

// locateMoved finds what id became, wherever sub sees it. Hidden there is the
// same as no move. Only a native tracker's id follows a redirect.
func (s *Service) locateMoved(ctx context.Context, sub *authz.Subject, id string) (loc, bool, error) {
	a, ok := parseWorkID(id)
	if !ok || sub == nil || orchcore.Issues(s.d) == nil || a.Source == LocalSource {
		return loc{}, false, nil
	}
	ref, from, to, moved, err := orchcore.Issues(s.d).MovedTo(ctx, a.Ref)
	if err != nil || !moved {
		return loc{}, false, err
	}
	if tr, known := orchcore.Tracker(s.d.Projects, from, a.Source); !known || tr.Adapter != orchapi.AdapterNative {
		return loc{}, false, nil
	}
	f, known := s.d.Projects.File(to)
	if !known {
		return loc{}, false, nil
	}
	for _, tr := range orchcore.ProjectTrackers(f) {
		if tr.Adapter != orchapi.AdapterNative {
			continue
		}
		l, err := s.locate(ctx, sub, sub.Scope.Only(to), tr.Prefix+":"+ref)
		if api.IsCode(err, api.CodeNotFound) {
			return loc{}, false, nil
		}
		return l, err == nil, err
	}
	return loc{}, false, nil
}

// MovedItem implements orchcore.WorkMoves.
func (s *Service) MovedItem(ctx context.Context, sub *authz.Subject, id string) (api.ProjectRef, string, bool, error) {
	l, found, err := s.locateMoved(ctx, sub, id)
	if err != nil || !found {
		return api.ProjectRef{}, "", false, err
	}
	p, ok := s.d.Projects.Lookup(l.project)
	return p, l.id, ok, nil
}

// ItemMoved implements orchcore.WorkMoves. The event goes out even when the row is
// already gone (a rerun of a move that stopped), for copies that still hold it.
func (s *Service) ItemMoved(ctx context.Context, project, id, to, newID string) {
	a, ok := parseWorkID(id)
	db, err := s.d.Store.Project(authz.All(), authz.Write, project)
	if !ok || err != nil {
		return
	}
	l := s.m.lock(project)
	l.Lock()
	area := ""
	if it, found, err := mirrorGet(ctx, db, project, a.Source, a.Ref); err == nil && found {
		area = it.Area
	}
	if _, err := mirrorDelete(ctx, db, a.Source, []string{id}); err != nil {
		l.Unlock()
		s.warn("moved issue: mirror", "id", id, "err", err)
		return
	}
	s.publish(ctx, orchapi.EventWorkMoved, id, project, area, orchapi.WorkMoved{ID: id})
	l.Unlock()
	if to == "" {
		return
	}
	// A read by the old id may mirror the new one any moment, and a row that
	// reached the mirror that way is no change to its next sync: nothing would
	// tell the copies. So it is mirrored and announced here.
	if err := s.mirrorOne(ctx, to, newID); err != nil {
		s.warn("moved issue: new mirror", "id", newID, "err", err)
		return
	}
	if l, isItem, err := s.keyItem(ctx, authz.All(), to, newID); isItem && err == nil {
		if it, err := s.load(ctx, s.d.System(), l); err == nil {
			s.publishItem(ctx, orchapi.EventWorkCreated, l, it)
		}
	}
}
