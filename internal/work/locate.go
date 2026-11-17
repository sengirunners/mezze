package work

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

// loc is a work item found inside one project the caller can read.
type loc struct {
	project string // ULID
	area    string
	id      string // canonical "<source>:<ref>"
	ref     string
	db      *sql.DB      // the project's project.db
	tr      *api.Tracker // nil for local items
	pre     *prefetched  // the tracker's answer when locate already asked it
}

// prefetched is the mirrored item found while locating it; the first load
// uses it instead of reading it again.
type prefetched struct {
	it   orchapi.WorkItem
	used bool
}

func (l loc) local() bool       { return l.tr == nil }
func (l loc) obj() authz.Object { return authz.Object{Project: l.project, Area: l.area} }

// canonID is a work id's canonical form: lowercase source; a local ref is
// lowercase too (they are generated that way); other refs keep their case.
func canonID(a api.Address) string {
	if a.Source == LocalSource {
		return LocalSource + ":" + strings.ToLower(a.Ref)
	}
	return a.Source + ":" + a.Ref
}

// parseWorkID parses id as a work item id.
func parseWorkID(id string) (api.Address, bool) {
	a, err := api.ParseAddress(id)
	return a, err == nil && a.Kind == api.KindWork
}

// locate finds work item id in the projects sc may read. Anything that is
// not a readable item (bad id, unknown source, hidden project or area,
// missing) is exactly api.NotFound("work item", id). An id that exists in
// several visible projects is a bad_request naming them (pass --project).
func (s *Service) locate(ctx context.Context, sub *authz.Subject, sc authz.Scope, id string) (loc, error) {
	nf := api.NotFound("work item", id)
	a, explicit := parseWorkID(id)
	if !explicit && (id == "" || strings.ContainsAny(id, ":/ \t\r\n")) {
		return loc{}, nf
	}
	var cands []loc
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		addresses := []api.Address{a}
		if !explicit {
			// Bare issue ids are aliases only for the project's native issue
			// source. Never guess an external tracker's prefix or a local id.
			addresses = nil
			for _, tr := range orchcore.ProjectTrackers(s.projectFile(p.ID)) {
				if tr.Adapter == orchapi.AdapterNative {
					addresses = append(addresses, api.Address{Kind: api.KindWork, Source: tr.Prefix, Ref: id})
				}
			}
		}
		for _, address := range addresses {
			l, ok, err := s.locateIn(ctx, sc, p.ID, p.DB, address)
			if td, isDown := err.(trackerDown); isDown {
				if localCaller(sub) {
					return loc{}, td.err
				}
				return loc{}, api.Errorf(api.CodeUnavailable, "the %s tracker is unavailable", td.adapter)
			}
			if err != nil {
				return loc{}, err
			}
			if ok {
				cands = append(cands, l)
			}
		}
	}
	switch len(cands) {
	case 0:
		return loc{}, nf
	case 1:
		return cands[0], nil
	}
	var slugs []string
	for _, c := range cands {
		slugs = append(slugs, s.d.Projects.Slug(c.project))
	}
	sort.Strings(slugs)
	return loc{}, api.Errorf(api.CodeBadRequest, "%s exists in several projects: %s", id, strings.Join(slugs, ", ")).
		WithHint("pass --project <slug>")
}

// locateIn looks for a in one project. Local items must exist in a readable
// area; tracker items must be in the tracker's mirror, in a readable area.
func (s *Service) locateIn(ctx context.Context, sc authz.Scope, project string, db *sql.DB, a api.Address) (loc, bool, error) {
	id := canonID(a)
	if a.Source == LocalSource {
		ref := strings.ToLower(a.Ref)
		where, args := sc.AreaSQL(project, "area", authz.Read)
		var area string
		err := db.QueryRowContext(ctx, `SELECT area FROM work_items WHERE ref = ? AND `+where, append([]any{ref}, args...)...).Scan(&area)
		if errors.Is(err, sql.ErrNoRows) {
			return loc{}, false, nil
		}
		if err != nil {
			return loc{}, false, err
		}
		return loc{project: project, area: area, id: id, ref: ref, db: db}, true, nil
	}
	tr, ok := orchcore.Tracker(s.d.Projects, project, a.Source)
	if !ok {
		return loc{}, false, nil
	}
	// The mirror answers: existence and area (labels included) without
	// asking the tracker.
	sy := s.m.syncer(project, tr)
	if err := s.m.fresh(ctx, project, db); err != nil {
		return loc{}, false, err
	}
	it, found, err := mirrorGet(ctx, db, project, tr.Prefix, a.Ref)
	if err != nil {
		return loc{}, false, err
	}
	if !found && tr.Adapter == orchapi.AdapterNative && s.mirrorOne(ctx, project, id) == nil {
		// A native issue exists the moment it is made (bd create, then zm
		// work comment): read it through instead of waiting for the next
		// sync. In process, no network; the caller's scope still decides.
		if it, found, err = mirrorGet(ctx, db, project, tr.Prefix, a.Ref); err != nil {
			return loc{}, false, err
		}
	}
	if found && sc.Can(authz.Read, authz.Object{Project: project, Area: it.Area}) {
		return loc{project: project, area: it.Area, id: id, ref: a.Ref, db: db, tr: &tr, pre: &prefetched{it: it}}, true, nil
	}
	if v := sy.view(time.Now()); v.state == orchapi.SyncLoading && s.trackerVisible(sc, project, tr, s.projectFile(project)) {
		// No full sync yet and the caller may see some of this tracker's
		// items: say so rather than "no work item", for a missing and a
		// hidden id alike (so it learns nothing about either).
		err := v.err
		if err == nil {
			err = api.Errorf(api.CodeUnavailable, "the %s tracker is still loading (its first sync); try again shortly", tr.Adapter)
		}
		return loc{}, false, trackerDown{adapter: tr.Adapter, err: err}
	}
	return loc{}, false, nil
}

// trackerDown is a tracker failure met while locating an item; locate
// shapes it for the caller (raw for the machine owner, generic otherwise).
type trackerDown struct {
	adapter string
	err     error
}

func (e trackerDown) Error() string { return e.err.Error() }

func (s *Service) projectFile(project string) project.File {
	f, _ := s.d.Projects.File(project)
	return f
}

// trackerItem is a tracker item as its mirror holds it.
func (s *Service) trackerItem(ctx context.Context, sub *authz.Subject, l loc) (orchapi.WorkItem, error) {
	var it orchapi.WorkItem
	if l.pre != nil && !l.pre.used {
		l.pre.used = true
		it = l.pre.it
	} else {
		var found bool
		var err error
		if it, found, err = mirrorGet(ctx, l.db, l.project, l.tr.Prefix, l.ref); err != nil {
			return it, err
		}
		if !found || it.Area != l.area {
			return orchapi.WorkItem{}, api.NotFound("work item", l.id)
		}
	}
	it.Stale = s.staleNote(sub, l.project, *l.tr)
	return it, nil
}

// keyItem reports whether a lease key names a work item of project (local,
// or a prefix with a tracker there), returning its location when the item
// is readable by sc (local items must exist).
func (s *Service) keyItem(ctx context.Context, sc authz.Scope, project, key string) (loc, bool, error) {
	a, ok := parseWorkID(key)
	if !ok {
		return loc{}, false, nil
	}
	if a.Source != LocalSource {
		if _, ok := orchcore.Tracker(s.d.Projects, project, a.Source); !ok {
			return loc{}, false, nil
		}
	}
	// Store.Project is an in-memory lookup (no I/O) whose only error is
	// NotFound, so a missing project means a missing item, not a failure.
	db, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return loc{}, false, nil
	}
	l, found, err := s.locateIn(ctx, sc, project, db, a)
	if err != nil {
		return loc{}, true, err
	}
	if !found {
		return loc{}, true, api.NotFound("work item", key)
	}
	return l, true, nil
}

// hintLinkTarget adds what a link can reach to a not_found link target, the
// same for a missing and a hidden one.
func hintLinkTarget(err error) error {
	var nf *api.Error
	if errors.As(err, &nf) && nf.Code == api.CodeNotFound && nf.Hint == "" {
		return nf.WithHint("a link reaches an item this project's home can read (here, or in another project that home keeps); for anything else, link its URL")
	}
	return err
}
