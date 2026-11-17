package work

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/unheld"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// list is GET /v0/work: local items (AreaSQL) and tracker items from the
// trackers' mirrors (AreaSQL on the mirrored area), of every project in sc,
// owners overlaid from live leases. No tracker is asked: a tracker whose
// mirror never loaded and whose sync fails is never dropped silently (when
// q.Source names it the call fails with its error, otherwise it is in
// WorkList.Skipped), and one whose mirror is still loading or stale is
// named in WorkList.Behind.
func (s *Service) list(ctx context.Context, sub *authz.Subject, sc authz.Scope, q orchapi.WorkQuery) (orchapi.WorkList, error) {
	if q.Status != "" && !orchapi.ValidStatus(q.Status) {
		return orchapi.WorkList{}, api.Errorf(api.CodeBadRequest, "bad status %q (open, claimed, in_review, done)", q.Status)
	}
	if q.Owner != "" {
		if a, err := api.ParseAddress(q.Owner); err == nil {
			q.Owner = a.String()
		}
	}
	q.Limit = clampLimit(q.Limit)
	source := strings.ToLower(strings.TrimSpace(q.Source))
	var out []orchapi.WorkItem
	var skipped []api.WorkSkipped
	var behind []orchapi.WorkBehind
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		if q.Area != "" && !sc.Can(authz.Read, authz.Object{Project: p.ID, Area: q.Area}) {
			continue // a hidden or missing area: nothing, like an empty one
		}
		var items []orchapi.WorkItem
		if source == "" || source == LocalSource {
			local, err := listLocal(ctx, p.DB, sc, p.ID, localQuery{area: q.Area, text: q.Q, status: q.Status, limit: q.Limit})
			if err != nil {
				return orchapi.WorkList{}, err
			}
			items = append(items, local...)
		}
		tr, sk, bh, err := s.listTrackers(ctx, sub, sc, p, source, q)
		if err != nil {
			return orchapi.WorkList{}, err
		}
		items, skipped, behind = append(items, tr...), append(skipped, sk...), append(behind, bh...)
		if err := s.overlay(ctx, sub, p.ID, items); err != nil {
			return orchapi.WorkList{}, err
		}
		for _, it := range items {
			if q.Owner != "" && it.Owner != q.Owner {
				continue
			}
			out = append(out, it)
		}
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return orchapi.WorkList{Items: nonNil(out), Skipped: skipped, Behind: behind}, nil
}

// listTrackers lists the mirrored items of every tracker of project that sc
// may see. A tracker with nothing to serve (its first sync failed) is in
// skipped, its error sanitised for sub; when source names trackers
// explicitly that error is returned instead.
func (s *Service) listTrackers(ctx context.Context, sub *authz.Subject, sc authz.Scope, p store.ProjectDB, source string,
	q orchapi.WorkQuery) (items []orchapi.WorkItem, skipped []api.WorkSkipped, behind []orchapi.WorkBehind, err error) {
	f, ok := s.d.Projects.File(p.ID)
	if !ok {
		return nil, nil, nil, nil
	}
	var trs []api.Tracker
	for _, tr := range orchcore.ProjectTrackers(f) {
		if (source == "" || source == tr.Prefix || source == tr.Adapter) && s.trackerVisible(sc, p.ID, tr, f) {
			trs = append(trs, tr)
		}
	}
	if len(trs) == 0 {
		return nil, nil, nil, nil
	}
	if err := s.m.fresh(ctx, p.ID, p.DB); err != nil {
		return nil, nil, nil, err
	}
	var leased []string
	if q.Owner != "" {
		ls, err := queryLeases(ctx, s.leaseReadDB(p.ID), "project = ? AND holder = ? AND expires > ?", p.ID, q.Owner, store.Millis(s.now()))
		if err != nil {
			return nil, nil, nil, err
		}
		for _, l := range ls {
			leased = append(leased, l.Key)
		}
	}
	now := time.Now()
	for _, tr := range trs {
		v := s.m.syncer(p.ID, tr).view(now)
		if v.failing {
			e := s.adapterErr(sub, tracked{project: p.ID, tr: tr}, "", v.err)
			if source != "" && source != LocalSource {
				return nil, nil, nil, e
			}
			skipped = append(skipped, api.WorkSkipped{Project: p.ID, Source: tr.Prefix, Error: e.Error()})
			continue
		}
		rows, err := mirrorList(ctx, p.DB, sc, p.ID, mirrorQuery{source: tr.Prefix, area: q.Area, status: q.Status, text: q.Q,
			owner: q.Owner, leased: leased, limit: q.Limit})
		if err != nil {
			return nil, nil, nil, err
		}
		for i := range rows {
			rows[i].Links = nil // links need per-target checks: zm work show has them
		}
		items = append(items, rows...)
		if v.state != orchapi.SyncOK {
			behind = append(behind, s.behindOf(sub, p.ID, tr, v))
		}
	}
	return items, skipped, behind, nil
}

// behindOf describes a mirror that is not current, sanitised for sub.
func (s *Service) behindOf(sub *authz.Subject, project string, tr api.Tracker, v syncView) orchapi.WorkBehind {
	b := orchapi.WorkBehind{Project: project, Source: tr.Prefix, State: v.state, Synced: v.synced}
	if v.err != nil {
		b.Error = s.adapterErr(sub, tracked{project: project, tr: tr}, "", v.err).Error()
	}
	return b
}

// staleNote is WorkItem.Stale for an item of tracker tr: "" while its
// mirror is current.
func (s *Service) staleNote(sub *authz.Subject, project string, tr api.Tracker) string {
	sy := s.m.lookup(project, tr.Prefix)
	if sy == nil {
		return ""
	}
	v := sy.view(time.Now())
	if v.state != orchapi.SyncStale {
		return ""
	}
	why := "the sync is overdue"
	if v.err != nil {
		why = s.adapterErr(sub, tracked{project: project, tr: tr}, "", v.err).Error()
	}
	return "tracker data as of " + v.synced.UTC().Format(time.RFC3339) + ": " + why
}

// overlay sets Owner from live leases (a local item with no lease has no
// owner; a tracker item keeps its tracker's) and redacts addresses for sub.
func (s *Service) overlay(ctx context.Context, sub *authz.Subject, project string, items []orchapi.WorkItem) error {
	ls, err := queryLeases(ctx, s.leaseReadDB(project), "project = ? AND expires > ?", project, store.Millis(s.now()))
	if err != nil {
		return err
	}
	// By (area, key), and by key alone for an item that moved since it was
	// claimed (a work item id is unique per project; see itemLease).
	held, byKey := map[[2]string]string{}, map[string]string{}
	for _, l := range ls {
		held[[2]string{l.Area, l.Key}] = l.Holder
		byKey[l.Key] = l.Holder
	}
	for i := range items {
		it := &items[i]
		if h, ok := held[[2]string{it.Area, it.ID}]; ok {
			it.Owner = h
		} else if h, ok := byKey[it.ID]; ok {
			it.Owner = h
		} else if it.Source == LocalSource {
			it.Owner = ""
		}
		it.Owner = s.d.Redact(ctx, sub, it.Owner)
		it.CreatedBy = s.d.Redact(ctx, sub, it.CreatedBy)
	}
	return nil
}

// sources is GET /v0/work/sources: each tracker's state comes from its
// mirror's sync (no tracker is asked), so a failing or stale tracker is
// reported with its last error and how far behind it is.
func (s *Service) sources(ctx context.Context, sub *authz.Subject, sc authz.Scope) []orchapi.WorkSource {
	out := []orchapi.WorkSource{}
	now := time.Now()
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		out = append(out, orchapi.WorkSource{Project: p.ID, Name: LocalSource, Prefix: LocalSource, Verbs: localVerbs, OK: true})
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			if !s.trackerVisible(sc, p.ID, tr, f) {
				continue
			}
			v := s.m.syncer(p.ID, tr).view(now)
			ws := orchapi.WorkSource{Project: p.ID, Name: tr.Adapter, Prefix: tr.Prefix, Verbs: nonNil(v.desc.Verbs), OK: v.state == orchapi.SyncOK,
				Sync: &orchapi.WorkSync{State: v.state, Synced: v.synced, FullSync: v.fullSync}}
			if !v.synced.IsZero() {
				ws.Sync.LagSeconds = now.Sub(v.synced).Seconds()
			}
			if v.err != nil {
				ws.Error = s.adapterErr(sub, tracked{project: p.ID, tr: tr}, "", v.err).Error()
				ws.Sync.Error = ws.Error
				if !v.failFrom.IsZero() {
					t := v.failFrom.UTC()
					ws.Sync.FailingSince, ws.Sync.Failures = &t, v.fails
				}
			} else if v.state == orchapi.SyncLoading {
				ws.Error = "first sync in progress"
			} else if v.state == orchapi.SyncStale {
				ws.Error = "sync overdue"
			}
			if sc.WholeProject(p.ID) {
				ws.Sync.Items, _ = mirrorCount(ctx, p.DB, tr.Prefix)
			}
			if localCaller(sub) {
				ws.Path, ws.Sync.Mode, ws.Sync.Serve = v.path, v.mode, v.desc.Serve
			}
			out = append(out, ws)
		}
	}
	return out
}

// leases is GET /v0/leases: live leases in sc, holders redacted.
func (s *Service) leases(ctx context.Context, sub *authz.Subject, sc authz.Scope, holder string) ([]orchapi.Lease, error) {
	ls, err := s.liveLeases(ctx, sc)
	if err != nil {
		return nil, err
	}
	if holder != "" {
		if a, err := api.ParseAddress(holder); err == nil {
			holder = a.String()
		}
	}
	out := []orchapi.Lease{}
	for _, l := range ls {
		v := s.leaseView(ctx, sub, l)
		if holder != "" && v.Holder != holder {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) liveLeases(ctx context.Context, sc authz.Scope) ([]lease, error) {
	where, args := sc.SQL("project", "area", authz.Read)
	ls, err := s.queryLeasesAll(ctx, where+" AND expires > ? ORDER BY claimed, key", append(args, store.Millis(s.now()))...)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ls, func(i, j int) bool {
		if !ls[i].Claimed.Equal(ls[j].Claimed) {
			return ls[i].Claimed.Before(ls[j].Claimed)
		}
		return ls[i].Key < ls[j].Key
	})
	// Only projects the store knows (a lease row can outlive nothing else, but be strict).
	var out []lease
	for _, l := range ls {
		if sc.Can(authz.Read, authz.Object{Project: l.Project, Area: l.Area}) {
			out = append(out, l)
		}
	}
	return out, nil
}

// inflight is GET /v0/work/inflight: live leases joined with their items and
// holding sessions (read-only). Hidden holders are "someone" and their
// sessions are left out.
func (s *Service) inflight(ctx context.Context, sub *authz.Subject, sc authz.Scope) ([]orchapi.Inflight, error) {
	ls, err := s.liveLeases(ctx, sc)
	if err != nil {
		return nil, err
	}
	out, leased := []orchapi.Inflight{}, map[string]bool{}
	item := func(project, key string, fallback orchapi.WorkStatus) *orchapi.WorkItem {
		il, isItem, err := s.keyItem(ctx, sc, project, key)
		if !isItem || err != nil {
			return nil
		}
		var it orchapi.WorkItem
		if il.local() {
			it, err = getLocal(ctx, il.db, il.project, il.ref)
		} else if it, err = s.trackerItem(ctx, sub, il); err != nil && fallback != "" { // a mirror read; the lease still shows if it fails
			it, err = orchapi.WorkItem{ID: il.id, Source: il.tr.Prefix, Status: fallback}, nil
		}
		if err != nil {
			return nil
		}
		it.Links = nil
		r, err := s.render(ctx, sub, il, it, false)
		if err != nil {
			return nil
		}
		r.Links = nil
		return &r
	}
	for _, l := range ls {
		leased[l.Project+"\x00"+l.Key] = true
		row := orchapi.Inflight{Lease: s.leaseView(ctx, sub, l), Item: item(l.Project, l.Key, orchapi.WorkClaimed)}
		if orchcore.Sessions(s.d) != nil && s.d.Visible(ctx, sub.Scope, l.Holder) {
			if a, err := api.ParseAddress(l.Holder); err == nil && a.Kind == api.KindSession {
				if sess, ok := orchcore.Sessions(s.d).Lookup(ctx, l.Holder); ok && orchcore.Sessions(s.d).Visible(ctx, sub.Scope, l.Holder) {
					row.Session = &sess
				}
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Lease.Claimed.Before(out[j].Lease.Claimed) })
	for _, p := range s.d.Store.Projects(sc, authz.Read) { // then the unheld
		if db, err := s.d.Store.Project(sc, authz.Read, p.ID); err == nil {
			ids, err := unheld.PRLinked(ctx, db, sc, p.ID)
			if err != nil {
				s.warn("inflight: unheld items", "project", p.ID, "err", err)
			}
			out = append(out, unheld.Rows(p.ID, ids, leased, func(k string) *orchapi.WorkItem { return item(p.ID, k, "") })...)
		}
	}
	return out, nil
}
