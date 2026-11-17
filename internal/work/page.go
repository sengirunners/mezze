package work

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/query"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// GET /v0/work/page: a keyset-paginated read of every item the caller may
// read (done items included), totally ordered by (created|updated, project,
// id), so a cursor never skips or repeats an item that did not move. It
// reads the same tables with the same AreaSQL scope as GET /v0/work.

const (
	pageDefault = 50
	pageMax     = 500
	pageMaxList = 500 // values of one repeated parameter
	// pageMaxLeased bounds the owner pushdown's IN list; past it the SQL
	// keeps every row and the owner check after the overlay decides.
	pageMaxLeased = 2000
)

var pageMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "work/0201_page", Script: `
CREATE INDEX IF NOT EXISTS work_items_created ON work_items (created, ref);
CREATE INDEX IF NOT EXISTS work_items_updated ON work_items (updated, ref);
CREATE INDEX IF NOT EXISTS work_items_area_created ON work_items (area, created, ref);`},
	// The mirror's default order (created, from the item JSON) walks this
	// expression index instead of sorting a whole tracker per page: the
	// expression must stay byte-identical to mirrorCreated to be used.
	{Kind: store.KindProject, Name: "work/0202_mirror_created", Script: `CREATE INDEX IF NOT EXISTS work_mirror_created ON work_mirror (source, ` + mirrorCreated + `, id);`},
}

func (s *Service) handlePage(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
	q, err := parsePageQuery(r.URL.Query())
	if err != nil {
		return nil, err
	}
	return s.page(r.Context(), sub, sc, q)
}

func parsePageQuery(v url.Values) (orchapi.WorkPageQuery, error) {
	return query.Page(v, pageMaxList)
}

// pageCursor is a position in the total order.
type pageCursor struct {
	key     int64
	project string
	id      string
}

func (c pageCursor) encode() string {
	return query.EncodeCursor(c.key, c.project, c.id)
}

func decodeCursor(s string) (pageCursor, error) {
	key, project, id, err := query.DecodeCursor(s)
	return pageCursor{key: key, project: project, id: id}, err
}

// pageRow is a candidate item with its sort key.
type pageRow struct {
	key int64
	it  orchapi.WorkItem
}

func (s *Service) page(ctx context.Context, sub *authz.Subject, sc authz.Scope, q orchapi.WorkPageQuery) (orchapi.WorkPage, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = pageDefault
	case limit > pageMax:
		limit = pageMax
	}
	var cur *pageCursor
	if q.After != "" {
		c, err := decodeCursor(q.After)
		if err != nil {
			return orchapi.WorkPage{}, err
		}
		cur = &c
	}
	// in: per project, nil = the whole project, else the named areas.
	var in map[string]map[string]bool
	if len(q.In) > 0 {
		in = map[string]map[string]bool{}
		for _, e := range q.In {
			ref, area, hasArea := strings.Cut(strings.TrimSpace(e), "/")
			p, err := s.d.Projects.Get(sc, ref)
			if err != nil {
				continue // hidden or missing: matches nothing, like an empty project
			}
			if !hasArea {
				in[p.ID] = nil
				continue
			}
			if areas, ok := in[p.ID]; ok && areas == nil {
				continue // already the whole project
			}
			if in[p.ID] == nil {
				in[p.ID] = map[string]bool{}
			}
			in[p.ID][area] = true
		}
	}
	// ids: per project ("" = any visible project), by source, the refs.
	var byID map[string]map[string][]string
	if len(q.IDs) > 0 {
		byID = map[string]map[string][]string{}
		for _, raw := range q.IDs {
			proj, id := "", raw
			if p, rest, ok := strings.Cut(raw, "/"); ok && ids.Valid(p) {
				proj, id = p, rest
			}
			a, ok := parseWorkID(id)
			if !ok {
				continue
			}
			cid := canonID(a)
			if byID[proj] == nil {
				byID[proj] = map[string][]string{}
			}
			byID[proj][a.Source] = append(byID[proj][a.Source], strings.TrimPrefix(cid, a.Source+":"))
		}
	}
	var epicProject string // epic=: the open epic whose items are kept ("" project: any)
	var epic api.Address
	if q.Epic != "" {
		var err error
		if epicProject, epic, err = query.Epic(q.Epic); err != nil {
			return orchapi.WorkPage{}, err
		}
	}
	var rows []pageRow
	out := orchapi.WorkPage{Epic: q.Epic} // the echo: a caller refuses an epic= page without it
	total := 0
	dbs := map[string]*sql.DB{}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		var areas map[string]bool
		if in != nil {
			a, ok := in[p.ID]
			if !ok {
				continue
			}
			areas = a
		}
		var refs map[string][]string
		if byID != nil {
			refs = query.MergeRefs(byID[""], byID[p.ID])
			if len(refs) == 0 {
				continue
			}
		}
		if q.Epic != "" {
			owned, err := s.epicOwns(ctx, sc, p.ID, p.DB, epicProject, areas, epic)
			if err != nil {
				return orchapi.WorkPage{}, err
			}
			if refs = query.IntersectRefs(refs, map[string][]string{epic.Source: owned}); len(refs[epic.Source]) == 0 {
				continue
			}
		}
		dbs[p.ID] = p.DB
		pq := pageSQL{q: q, project: p.ID, areas: areas, cur: cur, limit: limit + 1}
		if q.Owner != "" {
			ls, err := queryLeases(ctx, s.leaseReadDB(p.ID), "project = ? AND holder = ? AND expires > ?", p.ID, q.Owner, store.Millis(s.now()))
			if err != nil {
				return orchapi.WorkPage{}, err
			}
			pq.leased = []string{}
			for _, l := range ls {
				pq.leased = append(pq.leased, l.Key)
			}
		}
		if q.Source == "" || q.Source == LocalSource {
			if refs == nil || len(refs[LocalSource]) > 0 {
				pq.refs = refs[LocalSource]
				if q.Count {
					n, err := pageCount(ctx, p.DB, sc, pq, "work_items", "created", "ref", LocalSource+":", "")
					if err != nil {
						return orchapi.WorkPage{}, err
					}
					total += n
					goto trackers
				}
				r, err := pageLocal(ctx, p.DB, sc, pq)
				if err != nil {
					return orchapi.WorkPage{}, err
				}
				rows = append(rows, r...)
			}
		}
	trackers:
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		var trs []api.Tracker
		for _, tr := range orchcore.ProjectTrackers(f) {
			if (q.Source == "" || q.Source == tr.Prefix || q.Source == tr.Adapter) && s.trackerVisible(sc, p.ID, tr, f) &&
				(refs == nil || len(refs[tr.Prefix]) > 0) {
				trs = append(trs, tr)
			}
		}
		if len(trs) == 0 {
			continue
		}
		if err := s.m.fresh(ctx, p.ID, p.DB); err != nil {
			return orchapi.WorkPage{}, err
		}
		now := time.Now()
		for _, tr := range trs {
			v := s.m.syncer(p.ID, tr).view(now)
			if v.failing {
				e := s.adapterErr(sub, tracked{project: p.ID, tr: tr}, "", v.err)
				out.Skipped = append(out.Skipped, api.WorkSkipped{Project: p.ID, Source: tr.Prefix, Error: e.Error()})
				continue
			}
			pq.refs = nil
			if refs != nil {
				pq.refs = refs[tr.Prefix]
			}
			if q.Count {
				n, err := pageCount(ctx, p.DB, sc, pq, "work_mirror", mirrorCreated, "id", "", tr.Prefix)
				if err != nil {
					return orchapi.WorkPage{}, err
				}
				total += n
				continue
			}
			r, err := pageMirror(ctx, p.DB, sc, tr.Prefix, pq)
			if err != nil {
				return orchapi.WorkPage{}, err
			}
			rows = append(rows, r...)
			if v.state != orchapi.SyncOK {
				out.Behind = append(out.Behind, s.behindOf(sub, p.ID, tr, v))
			}
		}
	}
	if q.Count {
		out.Items, out.Cursors, out.Total = []orchapi.WorkItem{}, []string{}, &total
		return out, nil
	}
	sort.Slice(rows, func(i, j int) bool { return pageLess(rows[i], rows[j], q.Asc) })
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	items := make([]orchapi.WorkItem, len(rows))
	drop := make([]bool, len(rows))
	byProject := map[string][]int{}
	for i, r := range rows {
		items[i] = r.it
		byProject[r.it.Project] = append(byProject[r.it.Project], i)
	}
	for pid, idx := range byProject {
		group := make([]orchapi.WorkItem, len(idx))
		for k, i := range idx {
			group[k] = items[i]
		}
		if err := s.overlay(ctx, sub, pid, group); err != nil {
			return orchapi.WorkPage{}, err
		}
		for k, i := range idx {
			it := group[k]
			if q.Owner != "" && it.Owner != q.Owner {
				// The SQL pushdown is a superset; the redacted, lease-overlaid
				// owner decides (a hidden holder is "someone" and never matches,
				// like GET /v0/work?owner=).
				drop[i] = true
				continue
			}
			l := loc{project: pid, area: it.Area, id: it.ID, db: dbs[pid]}
			fromTracker := it.Links
			it.Links = nil
			if q.Links {
				ls, err := s.links(ctx, sub, l, fromTracker)
				if err != nil {
					return orchapi.WorkPage{}, err
				}
				it.Links = ls
			}
			if q.Comments {
				cs, err := s.comments(ctx, sub, l)
				if err != nil {
					return orchapi.WorkPage{}, err
				}
				it.Comments = cs
			}
			items[i] = it
		}
	}
	out.Items, out.Cursors = []orchapi.WorkItem{}, []string{}
	for i, r := range rows {
		if drop[i] {
			continue
		}
		out.Items = append(out.Items, items[i])
		out.Cursors = append(out.Cursors, pageCursor{key: r.key, project: r.it.Project, id: r.it.ID}.encode())
	}
	if more && len(rows) > 0 {
		// The last row read, kept or not: a page filtered by owner may hold
		// fewer than limit items; follow next.
		last := rows[len(rows)-1]
		out.Next = pageCursor{key: last.key, project: last.it.Project, id: last.it.ID}.encode()
	}
	return out, nil
}

func pageLess(a, b pageRow, asc bool) bool {
	return query.Less(a.key, b.key, a.it, b.it, asc)
}

// pageSQL is one project's share of a page query.
type pageSQL struct {
	q       orchapi.WorkPageQuery
	project string
	areas   map[string]bool // nil = every area sc reads
	refs    []string        // nil = any
	cur     *pageCursor
	limit   int
	created string   // the table's created expression (Unix ms)
	leased  []string // with q.Owner: the keys owner holds a live lease on
}

// where builds the shared conditions. key is the sort key's SQL expression;
// idCol holds the item id less prefix ("ref" and "local:" for local items,
// "id" and "" for the mirror), so ORDER BY key, idCol can walk an index.
func (p pageSQL) where(sc authz.Scope, key, idCol, prefix string) ([]string, []any) {
	w, args := sc.AreaSQL(p.project, "area", authz.Read)
	conds := []string{w}
	if p.areas != nil {
		as := make([]string, 0, len(p.areas))
		for a := range p.areas {
			as = append(as, a)
		}
		sort.Strings(as)
		conds = append(conds, "area IN ("+placeholders(len(as))+")")
		for _, a := range as {
			args = append(args, a)
		}
	}
	if p.refs != nil {
		conds = append(conds, "ref IN ("+placeholders(len(p.refs))+")")
		for _, r := range p.refs {
			args = append(args, r)
		}
	}
	if len(p.q.Status) > 0 {
		conds = append(conds, "status IN ("+placeholders(len(p.q.Status))+")")
		for _, st := range p.q.Status {
			args = append(args, string(st))
		}
	}
	if p.leased != nil && len(p.leased) <= pageMaxLeased {
		// Local items: owned only through a live lease. Mirrored items: a
		// lease, or the tracker's own owner (the overlay re-checks).
		var refs []any
		for _, k := range p.leased {
			if r, ok := strings.CutPrefix(k, prefix); ok || prefix == "" {
				if prefix == "" {
					r = k
				}
				refs = append(refs, r)
			}
		}
		c := "0"
		if len(refs) > 0 {
			c = idCol + " IN (" + placeholders(len(refs)) + ")"
			args = append(args, refs...)
		}
		if prefix == "" {
			c = "(owner = ? OR " + c + ")"
			args = append(args[:len(args)-len(refs)], append([]any{p.q.Owner}, refs...)...)
		}
		conds = append(conds, c)
	}
	if len(p.q.Labels) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM json_each(labels) WHERE value IN ("+placeholders(len(p.q.Labels))+"))")
		for _, l := range p.q.Labels {
			args = append(args, l)
		}
	}
	if f := p.q.Waiting; f != nil {
		or := []string{"value IN (" + placeholders(len(f.Labels)) + ")"}
		for _, l := range f.Labels {
			args = append(args, l)
		}
		for _, pre := range f.Prefixes {
			or = append(or, `value LIKE ? ESCAPE '\'`)
			args = append(args, likePrefix(pre))
		}
		c := "EXISTS (SELECT 1 FROM json_each(labels) WHERE " + strings.Join(or, " OR ") + ")"
		if idCol == "id" && len(f.Types) > 0 { // only a tracker's item has a type
			c = "(" + c + " OR json_extract(item, '$.extra.issue_type') IN (" + placeholders(len(f.Types)) + "))"
			for _, t := range f.Types {
				args = append(args, t)
			}
		}
		conds = append(conds, c)
	}
	if p.q.Q != "" {
		// LIKE is case-insensitive for ASCII.
		like := likeContains(p.q.Q)
		conds = append(conds, `(title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	rng := func(col string, after, before time.Time) {
		if !after.IsZero() {
			conds, args = append(conds, col+" >= ?"), append(args, store.Millis(after))
		}
		if !before.IsZero() {
			conds, args = append(conds, col+" <= ?"), append(args, store.Millis(before))
		}
	}
	rng(p.created, p.q.CreatedAfter, p.q.CreatedBefore)
	rng("updated", p.q.UpdatedAfter, p.q.UpdatedBefore)
	if c := p.cur; c != nil {
		lt, le := "<", "<="
		if p.q.Asc {
			lt, le = ">", ">="
		}
		switch {
		case p.project == c.project:
			if ref, ok := strings.CutPrefix(c.id, prefix); ok {
				conds = append(conds, "("+key+" "+lt+" ? OR ("+key+" = ? AND "+idCol+" "+lt+" ?))")
				args = append(args, c.key, c.key, ref)
				break
			}
			// The cursor item is in another table: every id here compares the
			// same way with it (they all start with prefix and it does not).
			before := prefix < c.id
			if p.q.Asc {
				before = !before
			}
			if before {
				conds, args = append(conds, key+" "+le+" ?"), append(args, c.key)
			} else {
				conds, args = append(conds, key+" "+lt+" ?"), append(args, c.key)
			}
		case (p.project < c.project) != p.q.Asc:
			// This project sorts after the cursor's at an equal key.
			conds, args = append(conds, key+" "+le+" ?"), append(args, c.key)
		default:
			conds, args = append(conds, key+" "+lt+" ?"), append(args, c.key)
		}
	}
	return conds, args
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

func (p pageSQL) orderBy(key, id string) string {
	dir := " DESC"
	if p.q.Asc {
		dir = " ASC"
	}
	return " ORDER BY " + key + dir + ", " + id + dir + " LIMIT ?"
}

func pageLocal(ctx context.Context, db *sql.DB, sc authz.Scope, p pageSQL) ([]pageRow, error) {
	key := "created"
	if p.q.Order == "updated" {
		key = "updated"
	}
	p.created = "created"
	conds, args := p.where(sc, key, "ref", LocalSource+":")
	args = append(args, p.limit)
	rows, err := db.QueryContext(ctx, `SELECT `+itemCols+`, `+key+` FROM work_items WHERE `+strings.Join(conds, " AND ")+p.orderBy(key, "ref"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pageRow
	for rows.Next() {
		var r pageRow
		it, err := scanItem(withKey{rows, &r.key}, p.project)
		if err != nil {
			return nil, err
		}
		r.it = it
		out = append(out, r)
	}
	return out, rows.Err()
}

// mirrorCreated is a mirrored item's created time in Unix ms (0 when the
// tracker gave none); the mirror keeps it only inside the item JSON. It is
// also the key of index work_mirror_created, so it must not change without a
// migration. SQLite refuses a row whose created is the
// string "now" in an index expression; created is always a Go-marshalled
// time, so that cannot happen.
const mirrorCreated = `COALESCE(CAST(unixepoch(json_extract(item, '$.created'), 'subsec') * 1000 AS INTEGER), 0)`

// mirrorFrom is the mirror table for a page query. A caller who sees a few
// areas (a guest, or an area filter) seeks by area and sorts that subset:
// walking work_mirror_created and filtering every row is 15-50x slower there
// (without statistics, as a fresh daemon runs until its first PRAGMA
// optimize). Everyone else walks work_mirror_created in order.
func mirrorFrom(areaScoped bool) string {
	if areaScoped {
		return "work_mirror INDEXED BY work_mirror_area"
	}
	return "work_mirror"
}

func pageMirror(ctx context.Context, db *sql.DB, sc authz.Scope, source string, p pageSQL) ([]pageRow, error) {
	key := mirrorCreated
	if p.q.Order == "updated" {
		key = "updated"
	}
	p.created = mirrorCreated
	conds, args := p.where(sc, key, "id", "")
	from := mirrorFrom(conds[0] != "1=1" || p.areas != nil)
	conds = append([]string{"source = ?"}, conds...)
	args = append([]any{source}, args...)
	args = append(args, p.limit)
	rows, err := db.QueryContext(ctx, `SELECT `+mirrorCols+`, `+key+` FROM `+from+` WHERE `+strings.Join(conds, " AND ")+p.orderBy(key, "id"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pageRow
	for rows.Next() {
		var r pageRow
		var id, area, st, title, body, raw string
		if err := rows.Scan(&id, &area, &st, &title, &body, &raw, &r.key); err != nil {
			return nil, err
		}
		it, err := scanMirror(staticRow{id, area, st, title, body, raw}, p.project, source)
		if err != nil {
			return nil, err
		}
		r.it = it
		out = append(out, r)
	}
	return out, rows.Err()
}

// staticRow replays scanned columns into scanMirror.
type staticRow []string

func (r staticRow) Scan(dst ...any) error {
	for i := range dst {
		*(dst[i].(*string)) = r[i]
	}
	return nil
}

func pageCount(ctx context.Context, db *sql.DB, sc authz.Scope, p pageSQL, table, created, idCol, prefix, source string) (int, error) {
	p.created, p.cur = created, nil
	key := created
	if p.q.Order == "updated" {
		key = "updated"
	}
	conds, args := p.where(sc, key, idCol, prefix)
	if source != "" {
		conds = append([]string{"source = ?"}, conds...)
		args = append([]any{source}, args...)
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+strings.Join(conds, " AND "), args...).Scan(&n)
	return n, err
}

// labels is GET /v0/work/labels.
func (s *Service) labels(ctx context.Context, sub *authz.Subject, sc authz.Scope, in []string) (orchapi.WorkLabels, error) {
	seen := map[string]bool{}
	var out orchapi.WorkLabels
	scan := func(db *sql.DB, sql string, args ...any) error {
		rows, err := db.QueryContext(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				return err
			}
			seen[l] = true
		}
		return rows.Err()
	}
	areasIn := map[string]map[string]bool{}
	for _, e := range in {
		ref, area, hasArea := strings.Cut(strings.TrimSpace(e), "/")
		p, err := s.d.Projects.Get(sc, ref)
		if err != nil {
			continue
		}
		if !hasArea {
			areasIn[p.ID] = nil
		} else if a, ok := areasIn[p.ID]; !ok || a != nil {
			if areasIn[p.ID] == nil {
				areasIn[p.ID] = map[string]bool{}
			}
			areasIn[p.ID][area] = true
		}
	}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		var areas map[string]bool
		if len(in) > 0 {
			a, ok := areasIn[p.ID]
			if !ok {
				continue
			}
			areas = a
		}
		pq := pageSQL{project: p.ID, areas: areas, created: "created"}
		conds, args := pq.where(sc, "created", "ref", "")
		if err := scan(p.DB, `SELECT DISTINCT j.value FROM work_items, json_each(work_items.labels) j WHERE `+strings.Join(conds, " AND "), args...); err != nil {
			return out, err
		}
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		visible := false
		for _, tr := range orchcore.ProjectTrackers(f) {
			if s.trackerVisible(sc, p.ID, tr, f) {
				visible = true
			}
		}
		if !visible {
			continue
		}
		if err := s.m.fresh(ctx, p.ID, p.DB); err != nil {
			return out, err
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			if !s.trackerVisible(sc, p.ID, tr, f) {
				continue
			}
			if v := s.m.syncer(p.ID, tr).view(time.Now()); v.failing {
				e := s.adapterErr(sub, tracked{project: p.ID, tr: tr}, "", v.err)
				out.Skipped = append(out.Skipped, api.WorkSkipped{Project: p.ID, Source: tr.Prefix, Error: e.Error()})
				continue
			}
			pq.created = mirrorCreated
			conds, args := pq.where(sc, mirrorCreated, "id", "")
			if err := scan(p.DB, `SELECT DISTINCT j.value FROM work_mirror, json_each(work_mirror.labels) j WHERE source = ? AND `+strings.Join(conds, " AND "),
				append([]any{tr.Prefix}, args...)...); err != nil {
				return out, err
			}
		}
	}
	out.Labels = make([]string, 0, len(seen))
	for l := range seen {
		out.Labels = append(out.Labels, l)
	}
	sort.Strings(out.Labels)
	return out, nil
}
