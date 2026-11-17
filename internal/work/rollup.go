package work

import (
	"context"
	"database/sql"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/rollup"
	"github.com/sengirunners/mezze/internal/work/rollupmirror"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// rollup is GET /v0/work/rollup: one per project in sc (internal/rollup has the
// rules). It is here for the trackers' visibility and sync state.
func (s *Service) rollup(ctx context.Context, sub *authz.Subject, sc authz.Scope, area string) (orchapi.WorkRollupList, error) {
	out := orchapi.WorkRollupList{Rollups: []orchapi.WorkRollup{}}
	leases, err := s.liveLeases(ctx, sc)
	if err != nil {
		return out, err
	}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		if err := s.m.fresh(ctx, p.ID, p.DB); err != nil {
			return out, err
		}
		where, args := sc.AreaSQL(p.ID, "area", authz.Read)
		sources := []string{}
		if f, ok := s.d.Projects.File(p.ID); ok {
			at := time.Now()
			for _, tr := range orchcore.ProjectTrackers(f) {
				if s.trackerVisible(sc, p.ID, tr, f) {
					sources = append(sources, tr.Prefix)
					if v := s.m.syncer(p.ID, tr).view(at); v.state != orchapi.SyncOK {
						out.Behind = append(out.Behind, s.behindOf(sub, p.ID, tr, v))
					}
				}
			}
		}
		var claims []rollup.Claim
		for _, l := range leases {
			if l.Project == p.ID && (area == "" || l.Area == area) {
				mine := rollupmirror.Mine(ctx, orchcore.Sessions(s.d), sub, l.Holder)
				view := s.leaseView(ctx, sub, l)
				claims = append(claims, rollup.Claim{Lease: view, Mine: mine, Holder: rollupmirror.HolderState(ctx, s.d, sub, view.Holder)})
			}
		}
		src := rollupmirror.New(p.DB, where, args, area, sources)
		r, err := rollup.Build(ctx, src, p.ID, s.d.Projects.Slug(p.ID), claims, s.now())
		if err != nil {
			return out, err
		}
		out.Rollups = append(out.Rollups, r)
	}
	return out, nil
}

// epicOwns is what the open epic owns in project pid for sc (page's epic=, rollup.SQL.Owned) in the trackers sc
// sees and in= areas (nil: all; as rollup?area= narrows the counts); nothing outside project in.
func (s *Service) epicOwns(ctx context.Context, sc authz.Scope, pid string, db *sql.DB, in string, areas map[string]bool, epic api.Address) ([]string, error) {
	f, ok := s.d.Projects.File(pid)
	if !ok || (in != "" && in != pid) {
		return nil, nil
	}
	sources := []string{} // not nil: nil would be every tracker
	for _, tr := range orchcore.ProjectTrackers(f) {
		if s.trackerVisible(sc, pid, tr, f) {
			sources = append(sources, tr.Prefix)
		}
	}
	if err := s.m.fresh(ctx, pid, db); err != nil {
		return nil, err
	}
	where, args := rollupmirror.InAreas(sc, pid, areas)
	return rollupmirror.New(db, where, args, "", sources).Owned(ctx, epic.Source, epic.Ref)
}
