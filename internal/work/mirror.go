package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/mirrorquery"
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// The tracker mirror (the Linear sync-engine pattern): every tracker item
// a project's trackers have, kept in the project's project.db by a
// background sync (sync.go), so reads never wait on a tracker. Rows carry
// the item's area, computed from the project's area rules (work globs,
// labels, the tracker's area) when the row is written and again whenever
// those rules change, so every read filters with AreaSQL like local items.
var mirrorMigrations = []store.Migration{
	// announce: a relist (restore) still owes the copies its items; durable,
	// so a restart before the listing ends still announces.
	{Kind: store.KindProject, Name: "work/0205_mirror_sync_announce", Script: `ALTER TABLE work_mirror_sync ADD COLUMN announce INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindProject, Name: "work/0203_mirror_status", Script: `CREATE INDEX work_mirror_status ON work_mirror (source, status, updated DESC, id);`},
	{Kind: store.KindProject, Name: "work/0002_mirror", Script: `
CREATE TABLE work_mirror (
	source  TEXT NOT NULL,
	ref     TEXT NOT NULL,
	id      TEXT NOT NULL,
	area    TEXT NOT NULL DEFAULT '',
	status  TEXT NOT NULL,
	owner   TEXT NOT NULL DEFAULT '',
	title   TEXT NOT NULL DEFAULT '',
	body    TEXT NOT NULL DEFAULT '',
	labels  TEXT NOT NULL DEFAULT '[]',
	updated INTEGER NOT NULL DEFAULT 0,
	gen     INTEGER NOT NULL DEFAULT 0,
	item    TEXT NOT NULL,
	UNIQUE (source, ref)
);
CREATE INDEX work_mirror_area ON work_mirror (source, area, status, updated DESC, id);
CREATE INDEX work_mirror_recent ON work_mirror (source, updated DESC, id);
CREATE INDEX work_mirror_owner ON work_mirror (source, owner);
CREATE TABLE work_mirror_sync (
	source    TEXT PRIMARY KEY,
	binding   TEXT NOT NULL,
	cursor    TEXT NOT NULL DEFAULT '',
	complete  INTEGER NOT NULL DEFAULT 0,
	gen       INTEGER NOT NULL DEFAULT 0,
	synced    INTEGER NOT NULL DEFAULT 0,
	full_sync INTEGER NOT NULL DEFAULT 0,
	error     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE work_mirror_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`},
}

// mirrorCols are what a read selects; scanMirror turns them into an item.
const mirrorCols = mirrorquery.Columns

func scanMirror(sc scanner, project, source string) (orchapi.WorkItem, error) {
	return mirrorstate.Scan(sc, project, source)
}

// mirrorGet is one mirrored item ("" area when missing; ok false).
func mirrorGet(ctx context.Context, db *sql.DB, project, source, ref string) (orchapi.WorkItem, bool, error) {
	return mirrorstate.Get(ctx, db, project, source, ref)
}

// mirrorQuery is GET /v0/work's filter on one tracker's mirror.
type mirrorQuery struct {
	source string
	area   string
	status orchapi.WorkStatus // "" = not done (like the trackers' own default lists)
	text   string
	// owner keeps items the tracker says owner holds, or whose key is in
	// leased (the live leases owner holds); the caller re-checks after the
	// lease overlay.
	owner  string
	leased []string
	limit  int
}

// mirrorListSQL builds the scoped mirror read, newest change first.
func mirrorListSQL(sc authz.Scope, project string, q mirrorQuery) (string, []any) {
	return mirrorquery.List(mirrorquery.Query{Scope: sc, Project: project, Source: q.source, Area: q.area,
		Status: q.status, Text: q.text, Owner: q.owner, Leased: q.leased, Limit: q.limit})
}

func mirrorList(ctx context.Context, db *sql.DB, sc authz.Scope, project string, q mirrorQuery) ([]orchapi.WorkItem, error) {
	query, args := mirrorListSQL(sc, project, q)
	return mirrorstate.List(ctx, db, query, args, project, q.source)
}

// mirrorUpsert writes fixed items (area set) of one tracker. A row is only
// replaced by an item at least as new (an optimistic write after a claim
// is not undone by an older page of a listing that was already running);
// gen always moves, so a full listing knows the row is still there.
func mirrorUpsert(ctx context.Context, db *sql.DB, source string, items []orchapi.WorkItem, gen int64) error {
	const chunk = 500 // short write transactions: readers and local writes go on meanwhile
	for len(items) > 0 {
		n := min(chunk, len(items))
		batch := items[:n]
		items = items[n:]
		err := store.Tx(ctx, db, func(tx *sql.Tx) error {
			st, err := tx.PrepareContext(ctx, mirrorUpsertSQL)
			if err != nil {
				return err
			}
			defer st.Close()
			for _, it := range batch {
				ref := strings.TrimPrefix(it.ID, source+":")
				labels, _ := json.Marshal(nonNil(it.Labels))
				title, body, area, status := it.Title, it.Body, it.Area, it.Status
				it.Title, it.Body, it.Area, it.Status, it.Project, it.Source, it.ID = "", "", "", "", "", "", ""
				it.Comments, it.Stale = nil, ""
				raw, err := json.Marshal(it)
				if err != nil {
					return err
				}
				var upd int64
				if !it.Updated.IsZero() {
					upd = store.Millis(it.Updated)
				}
				if _, err := st.ExecContext(ctx, source, ref, source+":"+ref, area, string(status), it.Owner, title, body,
					string(labels), upd, gen, string(raw)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

var mirrorUpsertSQL = func() string {
	cols := []string{"id", "area", "status", "owner", "title", "body", "labels", "updated", "item"}
	sets := []string{"gen = excluded.gen"}
	for _, c := range cols {
		sets = append(sets, c+" = CASE WHEN excluded.updated >= work_mirror.updated THEN excluded."+c+" ELSE work_mirror."+c+" END")
	}
	return `INSERT INTO work_mirror (source, ref, id, area, status, owner, title, body, labels, updated, gen, item)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, ref) DO UPDATE SET ` + strings.Join(sets, ", ")
}()

func mirrorDelete(ctx context.Context, db *sql.DB, source string, ids []string) (int, error) {
	gone := 0
	for _, id := range ids {
		ref := strings.TrimPrefix(id, source+":")
		r, err := db.ExecContext(ctx, `DELETE FROM work_mirror WHERE source = ? AND ref = ?`, source, ref)
		if err != nil {
			return gone, err
		}
		n, _ := r.RowsAffected()
		gone += int(n)
	}
	return gone, nil
}

// mirrorDropUnseen deletes the rows a full listing (gen) did not return.
func mirrorDropUnseen(ctx context.Context, db *sql.DB, source string, gen int64) (int64, error) {
	r, err := db.ExecContext(ctx, `DELETE FROM work_mirror WHERE source = ? AND gen <> ?`, source, gen)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func mirrorCount(ctx context.Context, db *sql.DB, source string) (int, error) {
	return mirrorstate.Count(ctx, db, source)
}

// mirrorReArea recomputes every row's area with area (the current rules)
// and records rules as what the area column now follows.
// keepArea, from mirrorReArea's area func, leaves a row's area as it is
// (a native issue's area is the issue store's, which re-areas it itself).
const keepArea = "\x00keep"

func mirrorReArea(ctx context.Context, db *sql.DB, rules string, area func(id string, labels []string) string) error {
	type row struct{ source, ref, id, area, next string }
	rows, err := db.QueryContext(ctx, `SELECT source, ref, id, area, labels FROM work_mirror`)
	if err != nil {
		return err
	}
	var changed []row
	for rows.Next() {
		var r row
		var labels string
		if err := rows.Scan(&r.source, &r.ref, &r.id, &r.area, &labels); err != nil {
			rows.Close()
			return err
		}
		var ls []string
		_ = json.Unmarshal([]byte(labels), &ls)
		if r.next = area(r.id, ls); r.next != keepArea && r.next != r.area {
			changed = append(changed, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		for _, r := range changed {
			if _, err := tx.ExecContext(ctx, `UPDATE work_mirror SET area = ? WHERE source = ? AND ref = ?`, r.next, r.source, r.ref); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO work_mirror_meta (key, value) VALUES ('areas', ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, rules)
		return err
	})
}

// mirrorRules is the area rules the area column follows ("" = none recorded).
func mirrorRules(ctx context.Context, db *sql.DB) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM work_mirror_meta WHERE key = 'areas'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// syncRow is a tracker's persisted sync state.
type syncRow struct {
	binding, cursor, err string
	complete, announce   bool
	gen                  int64
	synced, fullSync     time.Time
}

func loadSyncRow(ctx context.Context, db *sql.DB, source string) (syncRow, bool, error) {
	var r syncRow
	var synced, full int64
	err := db.QueryRowContext(ctx, `SELECT binding, cursor, complete, gen, synced, full_sync, error, announce FROM work_mirror_sync WHERE source = ?`, source).
		Scan(&r.binding, &r.cursor, &r.complete, &r.gen, &synced, &full, &r.err, &r.announce)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if synced > 0 {
		r.synced = store.FromMillis(synced)
	}
	if full > 0 {
		r.fullSync = store.FromMillis(full)
	}
	return r, err == nil, err
}

func saveSyncRow(ctx context.Context, db *sql.DB, source string, r syncRow) error {
	var synced, full int64
	if !r.synced.IsZero() {
		synced = store.Millis(r.synced)
	}
	if !r.fullSync.IsZero() {
		full = store.Millis(r.fullSync)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO work_mirror_sync (source, binding, cursor, complete, gen, synced, full_sync, error, announce)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET binding = excluded.binding, cursor = excluded.cursor, complete = excluded.complete,
			gen = excluded.gen, synced = excluded.synced, full_sync = excluded.full_sync, error = excluded.error, announce = excluded.announce`,
		source, r.binding, r.cursor, r.complete, r.gen, synced, full, r.err, r.announce)
	return err
}

// mirroredSources lists the trackers a project's mirror holds state for.
func mirroredSources(ctx context.Context, db *sql.DB) ([]string, error) {
	return mirrorstate.Sources(ctx, db)
}
