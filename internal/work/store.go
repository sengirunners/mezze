package work

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/fencing"
	"github.com/sengirunners/mezze/internal/work/rollupmirror"
	"github.com/sengirunners/mezze/internal/work/unheld"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// LocalSource is the built-in source: items in the project's own project.db,
// ids local:<6 random base32 chars>.
const LocalSource = "local"

// Migrations are this module's tables (core.Deps.AddMigrations); New adds them.
var Migrations = slices.Concat(storeMigrations, mirrorMigrations, pageMigrations, teamMigrations, rollupmirror.Migrations, unheld.Migrations)

var storeMigrations = []store.Migration{
	// project.db: local items, every item's history (comments) and links.
	// Rows carry the item's area so every query can filter with AreaSQL.
	{Kind: store.KindProject, Name: "work/0001_init", Script: `
CREATE TABLE work_items (
	ref        TEXT PRIMARY KEY,
	area       TEXT NOT NULL DEFAULT '',
	title      TEXT NOT NULL,
	body       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'open',
	created_by TEXT NOT NULL DEFAULT '',
	labels     TEXT NOT NULL DEFAULT '[]',
	created    INTEGER NOT NULL,
	updated    INTEGER NOT NULL
);
CREATE INDEX work_items_area ON work_items (area, status);
CREATE TABLE work_comments (
	id      TEXT PRIMARY KEY,
	item    TEXT NOT NULL,
	area    TEXT NOT NULL DEFAULT '',
	author  TEXT NOT NULL,
	body    TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE INDEX work_comments_item ON work_comments (item, id);
CREATE TABLE work_links (
	item           TEXT NOT NULL,
	rel            TEXT NOT NULL,
	target         TEXT NOT NULL,
	target_project TEXT NOT NULL DEFAULT '',
	area           TEXT NOT NULL DEFAULT '',
	created        INTEGER NOT NULL,
	PRIMARY KEY (item, rel, target, target_project)
);`},
	// project.db: which local item each source issue of a `zm work import`
	// became. That command was retired and nothing writes
	// this table now; the migration stays because migrations are
	// append-only (potluck/kit/store: applied once, by name).
	{Kind: store.KindProject, Name: "work/0101_imports", Script: `
CREATE TABLE work_imports (
	source    TEXT NOT NULL,
	source_id TEXT NOT NULL,
	ref       TEXT NOT NULL,
	job       TEXT NOT NULL,
	updated   INTEGER NOT NULL,
	PRIMARY KEY (source, source_id)
);`},
	// registry.db: leases share a transaction domain with sessions. Unique on
	// (project, area, key) so a hidden area's lock neither conflicts with nor
	// reveals itself to a caller in another area.
	{Kind: store.KindRegistry, Name: "work/0001_leases", Script: `
CREATE TABLE leases (
	project TEXT NOT NULL,
	area    TEXT NOT NULL DEFAULT '',
	key     TEXT NOT NULL,
	holder  TEXT NOT NULL,
	bound   INTEGER NOT NULL DEFAULT 0,
	note    TEXT NOT NULL DEFAULT '',
	claimed INTEGER NOT NULL,
	renewed INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	PRIMARY KEY (project, area, key)
);
CREATE INDEX leases_holder ON leases (holder);
CREATE INDEX leases_expires ON leases (expires);`},
	// ttl_set: the claim named its ttl_seconds (the ticker never binds it).
	{Kind: store.KindRegistry, Name: "work/0002_leases_ttl_set", Script: `ALTER TABLE leases ADD COLUMN ttl_set INTEGER NOT NULL DEFAULT 0;`},
	// fence: the lease's fencing token (SPEC "Leases"); lease_fences holds
	// each project's last one, so a token is never handed out twice.
	{Kind: store.KindRegistry, Name: "work/0003_leases_fence", Script: fencing.Script},
}

type scanner interface{ Scan(...any) error }

// withKey scans a row of itemCols followed by one more column, into key.
type withKey struct {
	scanner
	key *int64
}

func (w withKey) Scan(dest ...any) error { return w.scanner.Scan(append(dest, w.key)...) }

// likeEscaper escapes LIKE's wildcards for ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains is a LIKE pattern (ESCAPE '\') matching any text containing s.
func likeContains(s string) string { return "%" + likeEscaper.Replace(s) + "%" }

func likePrefix(s string) string { return likeEscaper.Replace(s) + "%" }

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const refAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// newRef is 6 random lowercase base32 characters (30 bits): never a counter,
// so a guest cannot learn how many hidden items exist.
func newRef() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = refAlphabet[b[i]&31]
	}
	return string(b[:])
}

const itemCols = `ref, area, title, body, status, created_by, labels, created, updated`

func scanItem(sc scanner, project string) (orchapi.WorkItem, error) {
	var (
		it               orchapi.WorkItem
		ref, st, labels  string
		created, updated int64
	)
	if err := sc.Scan(&ref, &it.Area, &it.Title, &it.Body, &st, &it.CreatedBy, &labels, &created, &updated); err != nil {
		return it, err
	}
	it.ID, it.Source, it.Project, it.Status = LocalSource+":"+ref, LocalSource, project, orchapi.WorkStatus(st)
	_ = json.Unmarshal([]byte(labels), &it.Labels)
	it.Created, it.Updated = store.FromMillis(created), store.FromMillis(updated)
	return it, nil
}

// getLocal loads one local item (no scope check: callers locate it first).
func getLocal(ctx context.Context, q queryer, project, ref string) (orchapi.WorkItem, error) {
	it, err := scanItem(q.QueryRowContext(ctx, `SELECT `+itemCols+` FROM work_items WHERE ref = ?`, ref), project)
	if errors.Is(err, sql.ErrNoRows) {
		return it, api.NotFound("work item", LocalSource+":"+ref)
	}
	return it, err
}

func insertLocal(ctx context.Context, db queryer, it *orchapi.WorkItem, now time.Time) error {
	labels, _ := json.Marshal(nonNil(it.Labels))
	for range 8 {
		ref := newRef()
		_, err := db.ExecContext(ctx, `INSERT INTO work_items (ref, area, title, body, status, created_by, labels, created, updated)
			VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?)`, ref, it.Area, it.Title, it.Body, it.CreatedBy, string(labels),
			store.Millis(now), store.Millis(now))
		if store.IsUnique(err) {
			continue
		}
		if err != nil {
			return err
		}
		it.ID, it.Source, it.Status = LocalSource+":"+ref, LocalSource, orchapi.WorkOpen
		it.Created, it.Updated = store.FromMillis(store.Millis(now)), store.FromMillis(store.Millis(now))
		return nil
	}
	return errors.New("work: could not pick a free local id")
}

func setLocalStatus(ctx context.Context, q queryer, ref string, st orchapi.WorkStatus, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE work_items SET status = ?, updated = ? WHERE ref = ?`, string(st), store.Millis(now), ref)
	return err
}

// setLocalStatusIf moves an item from one status to another (claim/release
// side effects never overwrite a status a human moved on). moved reports
// whether it did.
func setLocalStatusIf(ctx context.Context, q queryer, ref string, from, to orchapi.WorkStatus, now time.Time) (moved bool, err error) {
	res, err := q.ExecContext(ctx, `UPDATE work_items SET status = ?, updated = ? WHERE ref = ? AND status = ?`,
		string(to), store.Millis(now), ref, string(from))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func touchLocal(ctx context.Context, q queryer, ref string, now time.Time) {
	_, _ = q.ExecContext(ctx, `UPDATE work_items SET updated = ? WHERE ref = ?`, store.Millis(now), ref)
}

// localQuery is GET /v0/work's filter on one project's items.
type localQuery struct {
	area, text string
	status     orchapi.WorkStatus
	limit      int
}

func listLocal(ctx context.Context, db *sql.DB, sc authz.Scope, project string, q localQuery) ([]orchapi.WorkItem, error) {
	where, args := sc.AreaSQL(project, "area", authz.Read)
	conds := []string{where}
	if q.area != "" {
		conds, args = append(conds, "area = ?"), append(args, q.area)
	}
	if q.status != "" {
		conds, args = append(conds, "status = ?"), append(args, string(q.status))
	}
	if q.text != "" {
		like := likeContains(strings.ToLower(q.text))
		conds = append(conds, `(lower(title) LIKE ? ESCAPE '\' OR lower(body) LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	args = append(args, q.limit)
	rows, err := db.QueryContext(ctx, `SELECT `+itemCols+` FROM work_items WHERE `+strings.Join(conds, " AND ")+
		` ORDER BY created DESC, ref LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orchapi.WorkItem
	for rows.Next() {
		it, err := scanItem(rows, project)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func addComment(ctx context.Context, db queryer, c orchapi.Comment, area string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO work_comments (id, item, area, author, body, created) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.Item, area, c.Author, c.Body, store.Millis(c.Created))
	return err
}

func listComments(ctx context.Context, db *sql.DB, item string) ([]orchapi.Comment, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, author, body, created FROM work_comments WHERE item = ? ORDER BY id`, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.Comment{}
	for rows.Next() {
		c := orchapi.Comment{Item: item}
		var ms int64
		if err := rows.Scan(&c.ID, &c.Author, &c.Body, &ms); err != nil {
			return nil, err
		}
		c.Created = store.FromMillis(ms)
		out = append(out, c)
	}
	return out, rows.Err()
}

// storedLink is a link row; TargetProject is "" for the item's own project.
type storedLink struct {
	Rel, Target, TargetProject string
}

func addLink(ctx context.Context, db queryer, item, area string, l storedLink, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO work_links (item, rel, target, target_project, area, created) VALUES (?, ?, ?, ?, ?, ?)`,
		item, l.Rel, l.Target, l.TargetProject, area, store.Millis(now))
	return err
}

func listLinks(ctx context.Context, db *sql.DB, item string) ([]storedLink, error) {
	rows, err := db.QueryContext(ctx, `SELECT rel, target, target_project FROM work_links WHERE item = ? ORDER BY created, rel, target`, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedLink
	for rows.Next() {
		var l storedLink
		if err := rows.Scan(&l.Rel, &l.Target, &l.TargetProject); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
