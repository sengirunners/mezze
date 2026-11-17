// Package sqlite is the automation record store's SQLite driver: tables in
// the project's project.db (SPEC 18.6), written through the project's
// writer (a team project's single writer connection) and read through its
// pool.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/automations/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	kstore "github.com/sengirunners/potluck/kit/store"
)

// Migrations are the driver's project.db tables (the module adds them).
var Migrations = []kstore.Migration{
	{Kind: kstore.KindProject, Name: "automations/0001_records", Script: ddl},
}

// automations is one row per record (tombstones until keep_until);
// automation_revisions its history, each with the write id that made it;
// automation_health the home's last post per record; automation_homes one
// claim per home, with its one wanted install.
const ddl = `
CREATE TABLE automations (
  id         TEXT PRIMARY KEY,
  area       TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,
  home       TEXT NOT NULL,
  task_id    TEXT NOT NULL,
  name       TEXT NOT NULL,
  enabled    INTEGER NOT NULL,
  resume_rev INTEGER,
  owner      TEXT NOT NULL DEFAULT '',
  fields     TEXT NOT NULL,
  body       TEXT NOT NULL,
  settings   TEXT NOT NULL DEFAULT '',
  rev        INTEGER NOT NULL,
  created    INTEGER NOT NULL,
  updated    INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  deleted    INTEGER,
  keep_until INTEGER,
  UNIQUE (home, task_id)
);
CREATE INDEX automations_gone ON automations (keep_until) WHERE keep_until IS NOT NULL;
CREATE TABLE automation_revisions (
  id       TEXT NOT NULL,
  rev      INTEGER NOT NULL,
  op       TEXT NOT NULL,
  at       INTEGER NOT NULL,
  actor    TEXT NOT NULL DEFAULT '',
  write_id TEXT NOT NULL DEFAULT '',
  def      TEXT NOT NULL,
  bytes    INTEGER NOT NULL,
  PRIMARY KEY (id, rev)
);
CREATE UNIQUE INDEX automation_revisions_write ON automation_revisions (write_id) WHERE write_id != '';
CREATE TABLE automation_health (
  id       TEXT PRIMARY KEY,
  home     TEXT NOT NULL,
  state    TEXT NOT NULL,
  received INTEGER NOT NULL,
  health   TEXT NOT NULL
);
CREATE INDEX automation_health_home ON automation_health (home, received);
CREATE TABLE automation_homes (
  home           TEXT PRIMARY KEY,
  install        TEXT NOT NULL,
  store          TEXT NOT NULL,
  path           TEXT NOT NULL DEFAULT '',
  claimed        INTEGER NOT NULL,
  updated        INTEGER NOT NULL,
  write_id       TEXT NOT NULL DEFAULT '',
  wanted_install TEXT NOT NULL DEFAULT '',
  wanted_store   TEXT NOT NULL DEFAULT '',
  wanted_path    TEXT NOT NULL DEFAULT '',
  wanted_first   INTEGER NOT NULL DEFAULT 0
);
`

// DB is one project's store.
type DB struct {
	r, w *sql.DB
}

var _ store.Store = (*DB)(nil)

// New is the store over read (the project's pool) and write (its writer:
// kit/store's Writer; the same pool for a solo project).
func New(read, write *sql.DB) *DB { return &DB{r: read, w: write} }

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const recCols = `a.id, a.area, a.kind, a.home, a.task_id, a.name, a.enabled, a.resume_rev, a.owner, a.fields, a.body,
  a.settings, a.rev, a.created, a.updated, a.updated_by, a.deleted, h.health`

const recFrom = ` FROM automations a LEFT JOIN automation_health h ON h.id = a.id`

type scanner interface{ Scan(...any) error }

func scanRecord(sc scanner) (store.Record, error) {
	var r store.Record
	var enabled int
	var resume, deleted sql.NullInt64
	var fields, settings string
	var created, updated int64
	var health sql.NullString
	if err := sc.Scan(&r.ID, &r.Area, &r.Kind, &r.Home, &r.TaskID, &r.Name, &enabled, &resume, &r.Owner, &fields, &r.Body,
		&settings, &r.Rev, &created, &updated, &r.UpdatedBy, &deleted, &health); err != nil {
		return r, err
	}
	r.Enabled = enabled != 0
	if resume.Valid {
		v := resume.Int64
		r.ResumeRev = &v
	}
	r.Fields = json.RawMessage(fields)
	if settings != "" {
		if err := json.Unmarshal([]byte(settings), &r.Settings); err != nil {
			return r, err
		}
	}
	r.Created, r.Updated, r.Deleted = kstore.FromMillis(created), kstore.FromMillis(updated), kstore.NullMillis(deleted)
	if health.Valid {
		var h orchapi.AutomationHealth
		if err := json.Unmarshal([]byte(health.String), &h); err != nil {
			return r, err
		}
		r.Health = &h
	}
	return r, nil
}

func get(ctx context.Context, q querier, id string) (store.Record, error) {
	r, err := scanRecord(q.QueryRowContext(ctx, `SELECT `+recCols+recFrom+` WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, store.ErrNotFound
	}
	return r, err
}

// List implements store.Store.
func (d *DB) List(ctx context.Context, sc authz.Scope, project string, q store.Query) ([]store.Record, bool, error) {
	where, args := sc.AreaSQL(project, "a.area", authz.Read)
	if q.Home != "" {
		where += ` AND a.home = ?`
		args = append(args, q.Home)
	}
	if q.Kind != "" {
		where += ` AND a.kind = ?`
		args = append(args, q.Kind)
	}
	if !q.Deleted {
		where += ` AND a.deleted IS NULL`
	}
	limit := ""
	if q.Limit > 0 {
		limit = ` LIMIT ` + strconv.Itoa(q.Limit+1)
	}
	rows, err := d.r.QueryContext(ctx, `SELECT `+recCols+recFrom+` WHERE `+where+` ORDER BY a.id`+limit, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []store.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if q.Limit > 0 && len(out) > q.Limit {
		return out[:q.Limit], true, nil
	}
	return out, false, nil
}

// Get implements store.Store.
func (d *DB) Get(ctx context.Context, id string) (store.Record, error) { return get(ctx, d.r, id) }

// defJSON is a definition as a revision keeps it (fields compacted).
func defJSON(def store.Definition) (string, error) {
	var buf bytes.Buffer
	if len(def.Fields) > 0 {
		if err := json.Compact(&buf, def.Fields); err != nil {
			return "", err
		}
		def.Fields = buf.Bytes()
	} else {
		def.Fields = json.RawMessage(`{}`)
	}
	b, err := json.Marshal(def)
	return string(b), err
}

func settingsJSON(s map[string]orchapi.AutomationSetting) (string, error) {
	if len(s) == 0 {
		return "", nil
	}
	b, err := json.Marshal(s)
	return string(b), err
}

func compactFields(f json.RawMessage) (string, error) {
	if len(f) == 0 {
		return `{}`, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, f); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// byWriteID is the record and revision a write id made (ok false: none).
func byWriteID(ctx context.Context, q querier, writeID string) (id string, rev int64, ok bool, err error) {
	if writeID == "" {
		return "", 0, false, nil
	}
	err = q.QueryRowContext(ctx, `SELECT id, rev FROM automation_revisions WHERE write_id = ?`, writeID).Scan(&id, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return id, rev, err == nil, err
}

// asOf is record id as revision rev left it (its definition, rev, time,
// actor and, for a delete, its tombstone); the record's identity and health
// are as now.
func asOf(ctx context.Context, q querier, id string, rev int64) (store.Record, error) {
	r, err := get(ctx, q, id)
	if err != nil {
		return r, err
	}
	var op, def string
	var at int64
	if err := q.QueryRowContext(ctx, `SELECT op, at, actor, def FROM automation_revisions WHERE id = ? AND rev = ?`, id, rev).Scan(&op, &at, &r.UpdatedBy, &def); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, nil // dropped from history: the record as it is
		}
		return r, err
	}
	var d store.Definition
	if err := json.Unmarshal([]byte(def), &d); err != nil {
		return r, err
	}
	r.Definition, r.Rev, r.Updated = d, rev, kstore.FromMillis(at)
	r.Deleted = nil
	if op == store.OpDelete {
		t := kstore.FromMillis(at)
		r.Deleted = &t
	}
	return r, nil
}

func (d *DB) addRevision(ctx context.Context, tx *sql.Tx, id string, rev int64, op string, w store.Write, def store.Definition) error {
	js, err := defJSON(def)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO automation_revisions (id, rev, op, at, actor, write_id, def, bytes) VALUES (?,?,?,?,?,?,?,?)`,
		id, rev, op, kstore.Millis(w.At), w.By, w.ID, js, len(js)); err != nil {
		return err
	}
	return prune(ctx, tx, id)
}

// prune keeps the newest store.HistoryRevisions revisions, within
// store.HistoryBytes, the newest always.
func prune(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, `SELECT rev, bytes FROM automation_revisions WHERE id = ? ORDER BY rev DESC`, id)
	if err != nil {
		return err
	}
	var cut int64 = -1
	n, total := 0, 0
	for rows.Next() {
		var rev int64
		var b int
		if err := rows.Scan(&rev, &b); err != nil {
			rows.Close()
			return err
		}
		n, total = n+1, total+b
		if n > 1 && (n > store.HistoryRevisions || total > store.HistoryBytes) {
			cut = rev
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || cut < 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM automation_revisions WHERE id = ? AND rev <= ?`, id, cut)
	return err
}

// Create implements store.Store.
func (d *DB) Create(ctx context.Context, r store.Record, w store.Write) (store.Record, error) {
	var out store.Record
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		if id, rev, ok, err := byWriteID(ctx, tx, w.ID); err != nil {
			return err
		} else if ok {
			if rev != 1 {
				return store.ErrWriteIDUsed
			}
			out, err = asOf(ctx, tx, id, rev)
			return err
		}
		var other string
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM automations WHERE home = ? AND task_id = ?`, r.Home, r.TaskID).Scan(&other); {
		case err == nil:
			return &store.ExistsError{ID: other}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		fields, err := compactFields(r.Fields)
		if err != nil {
			return err
		}
		settings, err := settingsJSON(r.Settings)
		if err != nil {
			return err
		}
		if w.Resume {
			one := int64(1)
			r.ResumeRev = &one
		}
		at := kstore.Millis(w.At)
		if _, err := tx.ExecContext(ctx, `INSERT INTO automations (id, area, kind, home, task_id, name, enabled, resume_rev, owner, fields, body,
  settings, rev, created, updated, updated_by) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)`,
			r.ID, r.Area, r.Kind, r.Home, r.TaskID, r.Name, r.Enabled, nullInt(r.ResumeRev), r.Owner, fields, r.Body, settings, at, at, w.By); err != nil {
			if kstore.IsUnique(err) {
				return &store.ExistsError{ID: r.ID}
			}
			return err
		}
		if err := d.addRevision(ctx, tx, r.ID, 1, store.OpCreate, w, r.Definition); err != nil {
			return err
		}
		out, err = get(ctx, tx, r.ID)
		return err
	})
	return out, err
}

// sameDef reports whether two definitions store alike.
func sameDef(a, b store.Definition) bool {
	x, err1 := defJSON(a)
	y, err2 := defJSON(b)
	return err1 == nil && err2 == nil && x == y
}

// Update implements store.Store.
func (d *DB) Update(ctx context.Context, id string, w store.Write, change func(store.Record) (store.Definition, error)) (store.Record, error) {
	var out store.Record
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		if wid, rev, ok, err := byWriteID(ctx, tx, w.ID); err != nil {
			return err
		} else if ok {
			if wid != id {
				return store.ErrWriteIDUsed
			}
			out, err = asOf(ctx, tx, id, rev)
			return err
		}
		cur, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Deleted != nil {
			return store.ErrDeleted
		}
		def, err := change(cur)
		if err != nil {
			return err
		}
		next := cur.Rev + 1
		if w.Resume {
			def.ResumeRev = &next
		}
		if sameDef(def, cur.Definition) {
			out = cur
			return nil
		}
		fields, err := compactFields(def.Fields)
		if err != nil {
			return err
		}
		settings, err := settingsJSON(def.Settings)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE automations SET name = ?, enabled = ?, resume_rev = ?, owner = ?, fields = ?, body = ?, settings = ?,
  rev = ?, updated = ?, updated_by = ? WHERE id = ? AND rev = ? AND deleted IS NULL`,
			def.Name, def.Enabled, nullInt(def.ResumeRev), def.Owner, fields, def.Body, settings, next, kstore.Millis(w.At), w.By, id, cur.Rev)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return store.ErrChanged
		}
		if err := d.addRevision(ctx, tx, id, next, store.OpUpdate, w, def); err != nil {
			return err
		}
		out, err = get(ctx, tx, id)
		return err
	})
	return out, err
}

// Delete implements store.Store.
func (d *DB) Delete(ctx context.Context, id string, rev int64, w store.Write) (store.Record, error) {
	var out store.Record
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		if wid, wrev, ok, err := byWriteID(ctx, tx, w.ID); err != nil {
			return err
		} else if ok {
			if wid != id {
				return store.ErrWriteIDUsed
			}
			out, err = asOf(ctx, tx, id, wrev)
			return err
		}
		cur, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Deleted != nil {
			out = cur
			return nil
		}
		if rev != 0 && rev != cur.Rev {
			return store.ErrChanged
		}
		next, at := cur.Rev+1, kstore.Millis(w.At)
		res, err := tx.ExecContext(ctx, `UPDATE automations SET rev = ?, updated = ?, updated_by = ?, deleted = ?, keep_until = ? WHERE id = ? AND rev = ?`,
			next, at, w.By, at, kstore.Millis(w.At.Add(store.TombstoneFor)), id, cur.Rev)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return store.ErrChanged
		}
		if err := d.addRevision(ctx, tx, id, next, store.OpDelete, w, cur.Definition); err != nil {
			return err
		}
		out, err = get(ctx, tx, id)
		return err
	})
	return out, err
}

// History implements store.Store.
func (d *DB) History(ctx context.Context, id string) ([]store.Revision, error) {
	if _, err := get(ctx, d.r, id); err != nil {
		return nil, err
	}
	rows, err := d.r.QueryContext(ctx, `SELECT rev, op, at, actor, write_id, def, bytes FROM automation_revisions WHERE id = ? ORDER BY rev DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Revision
	for rows.Next() {
		var v store.Revision
		var at int64
		var def string
		if err := rows.Scan(&v.Rev, &v.Op, &at, &v.By, &v.WriteID, &def, &v.Bytes); err != nil {
			return nil, err
		}
		v.At = kstore.FromMillis(at)
		if err := json.Unmarshal([]byte(def), &v.Def); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PutHealth implements store.Store.
func (d *DB) PutHealth(ctx context.Context, id string, h orchapi.AutomationHealth, received time.Time) (*orchapi.AutomationHealth, error) {
	var prev *orchapi.AutomationHealth
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		cur, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		prev = cur.Health
		b, err := json.Marshal(h)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO automation_health (id, home, state, received, health) VALUES (?,?,?,?,?)
  ON CONFLICT (id) DO UPDATE SET home = excluded.home, state = excluded.state, received = excluded.received, health = excluded.health`,
			id, cur.Home, h.State, kstore.Millis(received), string(b)); err != nil {
			return err
		}
		if cur.Deleted != nil && h.AppliedRev >= cur.Rev {
			_, err = tx.ExecContext(ctx, `UPDATE automations SET keep_until = max(keep_until, ?) WHERE id = ?`, kstore.Millis(received.Add(store.TombstoneFor)), id)
		}
		return err
	})
	return prev, err
}

const homeCols = `h.home, h.install, h.store, h.path, h.claimed, h.updated, h.write_id, h.wanted_install, h.wanted_store, h.wanted_path,
  h.wanted_first, COALESCE((SELECT MAX(x.received) FROM automation_health x WHERE x.home = h.home), 0)`

func scanHome(sc scanner) (store.Home, error) {
	var h store.Home
	var claimed, updated, wfirst, last int64
	var w store.Wanted
	if err := sc.Scan(&h.Home, &h.Install, &h.Store, &h.Path, &claimed, &updated, &h.WriteID, &w.Install, &w.Store, &w.Path, &wfirst, &last); err != nil {
		return h, err
	}
	h.Claimed, h.Updated, h.LastHealth = kstore.FromMillis(claimed), kstore.FromMillis(updated), kstore.FromMillis(last)
	if w.Install != "" {
		w.First = kstore.FromMillis(wfirst)
		h.Wanted = &w
	}
	return h, nil
}

func home(ctx context.Context, q querier, name string) (store.Home, error) {
	h, err := scanHome(q.QueryRowContext(ctx, `SELECT `+homeCols+` FROM automation_homes h WHERE h.home = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return h, store.ErrNotFound
	}
	return h, err
}

// Homes implements store.Store.
func (d *DB) Homes(ctx context.Context) ([]store.Home, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+homeCols+` FROM automation_homes h ORDER BY h.home`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Home
	for rows.Next() {
		h, err := scanHome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Home implements store.Store.
func (d *DB) Home(ctx context.Context, name string) (store.Home, error) { return home(ctx, d.r, name) }

// Claim implements store.Store.
func (d *DB) Claim(ctx context.Context, name string, c store.Claim, at time.Time) (store.Home, bool, error) {
	var out store.Home
	held := false
	ms := kstore.Millis(at)
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		cur, err := home(ctx, tx, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			held = true
			_, err = tx.ExecContext(ctx, `INSERT INTO automation_homes (home, install, store, path, claimed, updated) VALUES (?,?,?,?,?,?)`,
				name, c.Install, c.Store, c.Path, ms, ms)
		case err != nil:
			return err
		case cur.Install == c.Install:
			held = true
			_, err = tx.ExecContext(ctx, `UPDATE automation_homes SET store = ?, path = ?, updated = ? WHERE home = ?`, c.Store, c.Path, ms, name)
		case cur.Wanted != nil && cur.Wanted.Install == c.Install:
			_, err = tx.ExecContext(ctx, `UPDATE automation_homes SET wanted_store = ?, wanted_path = ? WHERE home = ?`, c.Store, c.Path, name)
		default:
			_, err = tx.ExecContext(ctx, `UPDATE automation_homes SET wanted_install = ?, wanted_store = ?, wanted_path = ?, wanted_first = ? WHERE home = ?`,
				c.Install, c.Store, c.Path, ms, name)
		}
		if err != nil {
			return err
		}
		out, err = home(ctx, tx, name)
		return err
	})
	return out, held, err
}

// SetHolder implements store.Store.
func (d *DB) SetHolder(ctx context.Context, name, from, to, writeID string, at time.Time) (store.Home, error) {
	var out store.Home
	err := kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		cur, err := home(ctx, tx, name)
		if err != nil {
			return err
		}
		if writeID != "" && cur.Install == to && cur.WriteID == writeID {
			out = cur
			return nil
		}
		switch {
		case cur.Install != from:
			return store.ErrChanged
		case cur.Wanted == nil || cur.Wanted.Install != to:
			return store.ErrNotWanted
		case cur.Wanted.Store == orchapi.AutomationStoreZ3:
			return store.ErrWantedZ3
		}
		ms := kstore.Millis(at)
		res, err := tx.ExecContext(ctx, `UPDATE automation_homes SET install = wanted_install, store = wanted_store, path = wanted_path, claimed = ?,
  updated = ?, write_id = ?, wanted_install = '', wanted_store = '', wanted_path = '', wanted_first = 0 WHERE home = ? AND install = ?`, ms, ms, writeID, name, from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return store.ErrChanged
		}
		out, err = home(ctx, tx, name)
		return err
	})
	return out, err
}

// Purge implements store.Store. A read first, so a project with nothing
// past its keep takes no write lock.
func (d *DB) Purge(ctx context.Context, now time.Time) (int, error) {
	var one int
	err := d.r.QueryRowContext(ctx, `SELECT 1 FROM automations WHERE keep_until IS NOT NULL AND keep_until <= ? AND deleted IS NOT NULL LIMIT 1`, kstore.Millis(now)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	err = kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM automations WHERE keep_until IS NOT NULL AND keep_until <= ? AND deleted IS NOT NULL`, kstore.Millis(now))
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			gone = append(gone, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range gone {
			for _, q := range []string{`DELETE FROM automation_revisions WHERE id = ?`, `DELETE FROM automation_health WHERE id = ?`, `DELETE FROM automations WHERE id = ?`} {
				if _, err := tx.ExecContext(ctx, q, id); err != nil {
					return err
				}
			}
		}
		n = len(gone)
		return nil
	})
	return n, err
}

// Scrub implements store.Store.
func (d *DB) Scrub(ctx context.Context, ids, texts []string, minLen int) error {
	var long []string
	for _, t := range texts {
		if t != "" && len(t) >= minLen {
			long = append(long, t)
		}
	}
	return kstore.Tx(ctx, d.w, func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := scrubWhere(ctx, tx, `id = ?`, []any{id}, texts); err != nil {
				return err
			}
		}
		if len(long) == 0 {
			return nil
		}
		return scrubWhere(ctx, tx, `1=1`, nil, long)
	})
}

// scrubWhere scrubs texts in the records, revisions and health rows that
// match where (over an id column) and hold one of texts, raw or as JSON.
func scrubWhere(ctx context.Context, tx *sql.Tx, where string, args []any, texts []string) error {
	var hold []string
	var hargs []any
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, form := range forms(t) {
			hold = append(hold, `instr(%s, ?) > 0`)
			hargs = append(hargs, form)
		}
	}
	if len(hold) == 0 {
		return nil
	}
	match := func(col string) (string, []any) {
		parts := make([]string, len(hold))
		for i, h := range hold {
			parts[i] = strings.ReplaceAll(h, "%s", col)
		}
		return "(" + strings.Join(parts, " OR ") + ")", hargs
	}
	// Records: name, owner, body, fields, settings.
	m1, a1 := match(`name || owner || body || fields || settings`)
	rows, err := tx.QueryContext(ctx, `SELECT id, name, owner, body, fields, settings FROM automations WHERE `+where+` AND `+m1, append(append([]any{}, args...), a1...)...)
	if err != nil {
		return err
	}
	type rec struct{ id, name, owner, body, fields, settings string }
	var recs []rec
	for rows.Next() {
		var x rec
		if err := rows.Scan(&x.id, &x.name, &x.owner, &x.body, &x.fields, &x.settings); err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range recs {
		if _, err := tx.ExecContext(ctx, `UPDATE automations SET name = ?, owner = ?, body = ?, fields = ?, settings = ? WHERE id = ?`,
			core.ScrubText(x.name, texts, 0), core.ScrubText(x.owner, texts, 0), core.ScrubText(x.body, texts, 0),
			scrubJSON(x.fields, texts), scrubJSON(x.settings, texts), x.id); err != nil {
			return err
		}
	}
	// Revisions: the definition each kept.
	m2, a2 := match(`def`)
	rows, err = tx.QueryContext(ctx, `SELECT id, rev, def FROM automation_revisions WHERE `+where+` AND `+m2, append(append([]any{}, args...), a2...)...)
	if err != nil {
		return err
	}
	type rv struct {
		id  string
		rev int64
		def string
	}
	var revs []rv
	for rows.Next() {
		var x rv
		if err := rows.Scan(&x.id, &x.rev, &x.def); err != nil {
			rows.Close()
			return err
		}
		revs = append(revs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range revs {
		js := scrubJSON(x.def, texts)
		if _, err := tx.ExecContext(ctx, `UPDATE automation_revisions SET def = ?, bytes = ? WHERE id = ? AND rev = ?`, js, len(js), x.id, x.rev); err != nil {
			return err
		}
	}
	// Health: its apply error.
	m3, a3 := match(`health`)
	rows, err = tx.QueryContext(ctx, `SELECT id, health FROM automation_health WHERE `+where+` AND `+m3, append(append([]any{}, args...), a3...)...)
	if err != nil {
		return err
	}
	type hr struct{ id, health string }
	var hs []hr
	for rows.Next() {
		var x hr
		if err := rows.Scan(&x.id, &x.health); err != nil {
			rows.Close()
			return err
		}
		hs = append(hs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range hs {
		if _, err := tx.ExecContext(ctx, `UPDATE automation_health SET health = ? WHERE id = ?`, scrubJSON(x.health, texts), x.id); err != nil {
			return err
		}
	}
	return nil
}

// forms is t raw and as it appears inside a JSON string.
func forms(t string) []string {
	out := []string{t}
	if b, err := json.Marshal(t); err == nil {
		if enc := string(b[1 : len(b)-1]); enc != t {
			out = append(out, enc)
		}
	}
	return out
}

// scrubJSON replaces texts inside every string of a JSON document (keys
// kept), so the document stays JSON; text that is not JSON is scrubbed as
// text.
func scrubJSON(js string, texts []string) string {
	if js == "" {
		return js
	}
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return core.ScrubText(js, texts, 0)
	}
	var walk func(x any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return core.ScrubText(t, texts, 0)
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return x
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return core.ScrubText(js, texts, 0)
	}
	return string(b)
}
