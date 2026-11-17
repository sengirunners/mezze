package work

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

var _ orchcore.Snapshotter = (*Service)(nil)

// snapshotLimit is "every row": a snapshot has no page size.
const snapshotLimit = 1 << 30

// Snapshot emits everything of project that sub may read, for a team
// replica's copy (SPEC 17.6): every item ("item", api.WorkItem, every
// status, owners from live leases, links checked per target, no comments),
// every comment on those items ("comment", api.Comment) and every live lease
// ("lease", api.Lease), all redacted for sub. The caller reads its cursor
// before calling (a change committed meanwhile is sent again by sync, and
// applying it twice is harmless).
func (s *Service) Snapshot(ctx context.Context, sub *authz.Subject, project string, emit func(kind string, v any) error) error {
	sc := sub.Scope
	db, err := s.d.Store.Project(sc, authz.Read, project)
	if err != nil {
		return err
	}
	items, err := listLocal(ctx, db, sc, project, localQuery{limit: snapshotLimit})
	if err != nil {
		return err
	}
	if f, ok := s.d.Projects.File(project); ok {
		for _, tr := range orchcore.ProjectTrackers(f) {
			if !s.trackerVisible(sc, project, tr, f) {
				continue
			}
			for _, st := range []orchapi.WorkStatus{"", orchapi.WorkDone} { // "" = every status but done
				rows, err := mirrorList(ctx, db, sc, project, mirrorQuery{source: tr.Prefix, status: st, limit: snapshotLimit})
				if err != nil {
					return err
				}
				items = append(items, rows...)
			}
		}
	}
	if err := s.overlay(ctx, sub, project, items); err != nil {
		return err
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		it := &items[i]
		seen[it.ID] = true
		l := loc{project: project, area: it.Area, id: it.ID, ref: strings.TrimPrefix(it.ID, LocalSource+":"), db: db}
		if it.Links, err = s.links(ctx, sub, l, it.Links); err != nil {
			return err
		}
		it.Comments = nil
		if err := emit("item", *it); err != nil {
			return err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id, item, author, body, created FROM work_comments ORDER BY id`)
	if err != nil {
		return err
	}
	var cs []orchapi.Comment
	for rows.Next() {
		var c orchapi.Comment
		var ms int64
		if err := rows.Scan(&c.ID, &c.Item, &c.Author, &c.Body, &ms); err != nil {
			rows.Close()
			return err
		}
		if !seen[c.Item] {
			continue // on an item sub cannot read (comments follow the item's area now)
		}
		c.Created = store.FromMillis(ms)
		cs = append(cs, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range cs {
		c.Author = s.d.Redact(ctx, sub, c.Author)
		if err := emit("comment", c); err != nil {
			return err
		}
	}
	ls, err := s.liveLeases(ctx, sc)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if l.Project != project {
			continue
		}
		if err := emit("lease", s.leaseView(ctx, sub, l)); err != nil {
			return err
		}
	}
	return nil
}
