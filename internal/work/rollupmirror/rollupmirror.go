// Package rollupmirror is the rollup driver for an authority: the tracker
// mirror (work_mirror) plus local items (work_items), the work module's tables.
package rollupmirror

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/rollup"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// localSource is a local item's source.
const localSource = "local"

// Table is the tracker mirror, as rollup reads it.
var Table = rollup.Table{Name: "work_mirror", JSON: "item", Title: "title", Labels: "labels", Key: []string{"source", "ref"}, Ref: "ref",
	Refs: func(source, refs string) (string, []any) {
		return "source = ? AND ref IN (SELECT value FROM json_each(?))", []any{source, refs}
	}}

// Migrations are this package's indexes; work.New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{
	{Kind: store.KindProject, Name: "work/0203_rollup", Script: Table.IndexSQL()},
	// GET /v0/work/page?epic= walks down from an epic (rollup.SQL.Owned).
	{Kind: store.KindProject, Name: "work/0204_rollup_children", Script: Table.ChildIndexSQL()},
}

// Source reads a project's mirror and local items within one reader's sight.
type Source struct{ rollup.SQL }

// New is a Source: where and args are the reader's area scope (authz AreaSQL
// on column area), area a narrower one asked for ("": none), sources the
// trackers the reader may see.
func New(db *sql.DB, where string, args []any, area string, sources []string) Source {
	if area != "" {
		where, args = where+" AND area = ?", append(args[:len(args):len(args)], area)
	}
	return Source{rollup.SQL{DB: db, Table: Table, Where: where, Args: args, Sources: sources}}
}

// Groups implements rollup.Source.
func (r Source) Groups(ctx context.Context) ([]rollup.Group, error) {
	out, err := r.SQL.Groups(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT status, count(*), max(updated) FROM work_items WHERE `+r.Where+` GROUP BY status`, r.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		g := rollup.Group{Source: localSource, Priority: rollup.NoPriority}
		var changed sql.NullInt64
		if err := rows.Scan(&g.Status, &g.Count, &changed); err != nil {
			return nil, err
		}
		if changed.Valid {
			g.Changed = store.FromMillis(changed.Int64)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Items implements rollup.Source.
func (r Source) Items(ctx context.Context, ids []string) ([]rollup.Item, error) {
	var tracker []string
	var local []any
	for _, id := range ids {
		if ref, ok := strings.CutPrefix(id, localSource+":"); ok {
			local = append(local, ref)
		} else {
			tracker = append(tracker, id)
		}
	}
	var out []rollup.Item
	if len(tracker) > 0 && len(r.Sources) > 0 {
		var err error
		if out, err = r.SQL.Items(ctx, tracker); err != nil {
			return nil, err
		}
	}
	if len(local) == 0 {
		return out, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(local)), ",")
	rows, err := r.DB.QueryContext(ctx, `SELECT ref, title, status FROM work_items WHERE `+r.Where+` AND ref IN (`+in+`)`, append(append([]any(nil), r.Args...), local...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		it := rollup.Item{Source: localSource, Priority: rollup.NoPriority}
		if err := rows.Scan(&ref, &it.Title, &it.Status); err != nil {
			return nil, err
		}
		it.ID, it.Ref = localSource+":"+ref, ref
		out = append(out, it)
	}
	return out, rows.Err()
}

// InAreas is New's where and args for a reader of project pid with scope
// sc, narrowed to areas (nil: every area sc reads).
func InAreas(sc authz.Scope, pid string, areas map[string]bool) (string, []any) {
	where, args := sc.AreaSQL(pid, "area", authz.Read)
	if areas == nil {
		return where, args
	}
	in := slices.Sorted(maps.Keys(areas))
	out := append(args[:len(args):len(args)], make([]any, 0, len(in))...)
	for _, a := range in {
		out = append(out, a)
	}
	return where + " AND area IN (" + strings.TrimSuffix(strings.Repeat("?,", len(in)), ",") + ")", out
}

// HolderState is rollup.Claim.Holder for a reader: the holding session's
// state when sub sees that session (as GET /v0/work/inflight shows it),
// else "". rollup.HolderState looks up session addresses only, and the
// session module's Visible is the whole check for those.
func HolderState(ctx context.Context, d *core.Deps, sub *authz.Subject, holder string) orchapi.SessionState {
	if orchcore.Sessions(d) == nil || sub == nil {
		return ""
	}
	return rollup.HolderState(holder, func(addr string) (orchapi.Session, bool) {
		if !orchcore.Sessions(d).Visible(ctx, sub.Scope, addr) {
			return orchapi.Session{}, false
		}
		return orchcore.Sessions(d).Lookup(ctx, addr)
	})
}

// Mine reports whether a lease's holder is the caller: its own session or
// address, or, for a person (no session), a session that person started.
func Mine(ctx context.Context, sessions orchcore.SessionService, sub *authz.Subject, holder string) bool {
	if holder == "" || sub == nil {
		return false
	}
	if holder == sub.Session || holder == sub.Address {
		return true
	}
	if sub.Session != "" || sessions == nil {
		return false
	}
	ss, ok := sessions.Lookup(ctx, holder)
	return ok && ss.Launcher != "" && (ss.Launcher == sub.Identity.ID || ss.Launcher == sub.Identity.Name)
}
