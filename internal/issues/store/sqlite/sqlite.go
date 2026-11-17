// Package sqlite is the native issue store's SQLite driver: one file per
// project ($ZM_HOME/projects/<ulid>/issues.db), WAL, one writer connection
// and four readers, prepared statements cached per connection
// (potluck/kit/store.OpenFile), and an FTS5 index over title, description and
// notes. See internal/issues/store for the contract.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/potluck/kit/api"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// FileName is each project's issue file, next to its project.db.
const FileName = "issues.db"

const (
	readers     = 4
	busyTimeout = 5 * time.Second
)

// Store opens projects lazily under home/projects/<ulid>/.
type Store struct {
	home string

	mu   sync.Mutex
	open map[string]*project
	atts map[string]*attachments // attachments.go
}

var _ istore.Store = (*Store)(nil)

// Open returns a store rooted at home ($ZM_HOME). Nothing is opened until a
// project is asked for.
func Open(home string) *Store {
	return &Store{home: home, open: map[string]*project{}, atts: map[string]*attachments{}}
}

func (s *Store) path(project string) string {
	return filepath.Join(s.home, zmstore.DirProjects, project, FileName)
}

// Project implements istore.Store.
func (s *Store) Project(ctx context.Context, project string, create bool) (istore.ProjectStore, error) {
	if project == "" || filepath.Base(project) != project || project == "." || project == ".." {
		return nil, fmt.Errorf("bad project id %q", project)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.open[project]; ok {
		return p, nil
	}
	path := s.path(project)
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if !create {
			return nil, istore.ErrNotFound
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	p, err := openProject(ctx, project, path)
	if err != nil {
		return nil, err
	}
	s.open[project] = p
	return p, nil
}

// Existing implements istore.Store.
func (s *Store) Existing(ctx context.Context) ([]istore.ProjectMeta, error) {
	dirs, err := os.ReadDir(filepath.Join(s.home, zmstore.DirProjects))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []istore.ProjectMeta
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, err := os.Stat(s.path(d.Name())); err != nil {
			continue
		}
		p, err := s.Project(ctx, d.Name(), false)
		if err != nil {
			return nil, err
		}
		pre, err := p.Prefix(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, istore.ProjectMeta{Project: d.Name(), Prefix: pre})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// Close implements istore.Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for id, p := range s.open {
		if err := p.close(); err != nil && first == nil {
			first = err
		}
		delete(s.open, id)
	}
	for id, a := range s.atts {
		if err := a.db.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.atts, id)
	}
	return first
}

// project is one project's file: w is the one writer connection, r the
// readers, ck the checkpointer.
type project struct {
	id   string
	path string
	w    *sql.DB
	r    *sql.DB
	ck   *sql.DB
	stop chan struct{}
	done chan struct{}
}

// checkpointEvery is how often the background checkpointer folds the WAL
// into the file. Commits never checkpoint themselves (wal_autocheckpoint=0
// on the writer), so a write never pays for one (they cost up to hundreds
// of ms on a busy machine, which put writes' p95 far over budget).
var checkpointEvery = 500 * time.Millisecond

func openProject(ctx context.Context, id, path string) (*project, error) {
	os.Remove(path + walAside) // a crash in a swap (restore.go)
	// Every writer connection (a replaced one too) leaves checkpoints to the checkpointer.
	w, err := zmstore.OpenFile(ctx, path, 1, busyTimeout, false, "wal_autocheckpoint(0)")
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, w); err != nil {
		w.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r, err := zmstore.OpenFile(ctx, path, readers, busyTimeout, true)
	if err != nil {
		w.Close()
		return nil, err
	}
	ck, err := zmstore.OpenFile(ctx, path, 1, busyTimeout, false)
	if err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	_, _ = w.ExecContext(ctx, `PRAGMA optimize=0x10002`)
	p := &project{id: id, path: path, w: w, r: r, ck: ck, stop: make(chan struct{}), done: make(chan struct{})}
	go p.checkpointer()
	return p, nil
}

// checkpointer runs PASSIVE checkpoints beside the writer (WAL lets a
// checkpoint run concurrently with readers and the writer).
func (p *project) checkpointer() {
	defer close(p.done)
	t := time.NewTicker(checkpointEvery)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			_, _ = p.ck.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
		}
	}
}

func (p *project) close() error {
	close(p.stop)
	<-p.done
	_, _ = p.ck.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	err := p.r.Close()
	if e := p.w.Close(); err == nil {
		err = e
	}
	if e := p.ck.Close(); err == nil {
		err = e
	}
	return err
}

// Migrations lists the migration names this build knows, in order (the
// swap preflight: a store with another one is newer than the build).
func Migrations() []string {
	out := make([]string, len(migrations))
	for i, m := range migrations {
		out[i] = m.name
	}
	return out
}

// migrations run once each, in order. Never edit one that shipped.
var migrations = []struct{ name, sql string }{
	{"0001_init", `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE issues (
	-- Short columns first, long text last: a scan that tests a short column
	-- never walks a long description's overflow pages.
	id                TEXT PRIMARY KEY,
	area              TEXT NOT NULL DEFAULT '',
	home_area         TEXT NOT NULL DEFAULT '',
	status            TEXT NOT NULL DEFAULT 'open',
	priority          INTEGER NOT NULL DEFAULT 2,
	issue_type        TEXT NOT NULL DEFAULT 'task',
	is_blocked        INTEGER NOT NULL DEFAULT 0,
	pinned            INTEGER NOT NULL DEFAULT 0,
	is_template       INTEGER NOT NULL DEFAULT 0,
	ephemeral         INTEGER NOT NULL DEFAULT 0,
	parent            TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	started_at        INTEGER,
	closed_at         INTEGER,
	due_at            INTEGER,
	defer_until       INTEGER,
	estimated_minutes INTEGER,
	assignee          TEXT NOT NULL DEFAULT '',
	claimed_by        TEXT NOT NULL DEFAULT '',
	owner             TEXT NOT NULL DEFAULT '',
	created_by        TEXT NOT NULL DEFAULT '',
	created_by_addr   TEXT NOT NULL DEFAULT '',
	external_ref      TEXT NOT NULL DEFAULT '',
	spec_id           TEXT NOT NULL DEFAULT '',
	source_system     TEXT NOT NULL DEFAULT '',
	closed_by_session TEXT NOT NULL DEFAULT '',
	version           TEXT NOT NULL DEFAULT '',
	title             TEXT NOT NULL,
	close_reason      TEXT NOT NULL DEFAULT '',
	metadata          TEXT NOT NULL DEFAULT '',
	extra             TEXT NOT NULL DEFAULT '',
	versions          TEXT NOT NULL DEFAULT '',
	notes             TEXT NOT NULL DEFAULT '',
	acceptance        TEXT NOT NULL DEFAULT '',
	design            TEXT NOT NULL DEFAULT '',
	description       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX issues_order ON issues (priority, created_at DESC, id, is_template, status, pinned, issue_type, area);
-- A status filter, and bd search (title, id, external ref substrings) as an
-- index-only scan.
CREATE INDEX issues_status ON issues (status, priority, created_at DESC, id, is_template, area, title, external_ref);
-- The default list (not closed or pinned) and ready work scan these, covering
-- every filter column, so a page or a count never reads the wide rows.
CREATE INDEX issues_open ON issues (priority, created_at DESC, id, issue_type, is_template, pinned, area)
	WHERE status NOT IN ('closed', 'pinned');
CREATE INDEX issues_ready ON issues (priority, created_at DESC, id, issue_type, is_template, pinned, ephemeral, area, defer_until, parent)
	WHERE status = 'open' AND is_blocked = 0;
CREATE INDEX issues_area ON issues (area, status);
CREATE INDEX issues_assignee ON issues (assignee, status);
CREATE INDEX issues_parent ON issues (parent) WHERE parent != '';
CREATE INDEX issues_updated ON issues (updated_at);
CREATE TABLE labels (
	issue_id TEXT NOT NULL,
	label    TEXT NOT NULL,
	PRIMARY KEY (issue_id, label)
) WITHOUT ROWID;
CREATE INDEX labels_label ON labels (label, issue_id);
CREATE TABLE deps (
	issue_id   TEXT NOT NULL,
	depends_on TEXT NOT NULL,
	type       TEXT NOT NULL DEFAULT 'blocks',
	external   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	metadata   TEXT NOT NULL DEFAULT '',
	thread_id  TEXT NOT NULL DEFAULT '',
	source_id  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (issue_id, depends_on)
) WITHOUT ROWID;
CREATE INDEX deps_target ON deps (depends_on, type);
CREATE TABLE comments (
	id          TEXT PRIMARY KEY,
	issue_id    TEXT NOT NULL,
	author      TEXT NOT NULL DEFAULT '',
	author_addr TEXT NOT NULL DEFAULT '',
	text        TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX comments_issue ON comments (issue_id, created_at, id);
CREATE TABLE history (
	id          TEXT PRIMARY KEY,
	issue_id    TEXT NOT NULL,
	kind        TEXT NOT NULL,
	actor       TEXT NOT NULL DEFAULT '',
	actor_addr  TEXT NOT NULL DEFAULT '',
	fields      TEXT NOT NULL DEFAULT '',
	comment     TEXT NOT NULL DEFAULT '',
	area_before TEXT NOT NULL DEFAULT '',
	area_after  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	source_id   TEXT
);
CREATE INDEX history_issue ON history (issue_id, created_at, id);
CREATE UNIQUE INDEX history_source ON history (source_id) WHERE source_id IS NOT NULL;
CREATE TABLE child_counters (parent TEXT PRIMARY KEY, last_child INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE VIRTUAL TABLE issues_fts USING fts5(
	title, description, notes,
	content='issues', content_rowid='rowid',
	tokenize='unicode61 remove_diacritics 2', prefix='2 3'
);
CREATE TRIGGER issues_fts_ai AFTER INSERT ON issues BEGIN
	INSERT INTO issues_fts (rowid, title, description, notes) VALUES (new.rowid, new.title, new.description, new.notes);
END;
CREATE TRIGGER issues_fts_ad AFTER DELETE ON issues BEGIN
	INSERT INTO issues_fts (issues_fts, rowid, title, description, notes) VALUES ('delete', old.rowid, old.title, old.description, old.notes);
END;
CREATE TRIGGER issues_fts_au AFTER UPDATE OF title, description, notes ON issues BEGIN
	INSERT INTO issues_fts (issues_fts, rowid, title, description, notes) VALUES ('delete', old.rowid, old.title, old.description, old.notes);
	INSERT INTO issues_fts (rowid, title, description, notes) VALUES (new.rowid, new.title, new.description, new.notes);
END;`},
	// A per-project sequential number (Linear's Issue.number): the order
	// issues were created in, never reused. Existing rows are numbered by
	// creation time.
	{"0002_number", `
ALTER TABLE issues ADD COLUMN number INTEGER;
UPDATE issues SET number = n.rn FROM (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn FROM issues) AS n WHERE n.id = issues.id;
CREATE UNIQUE INDEX issues_number ON issues (number);
-- Keyset pages by creation or update time (the GraphQL door's order).
CREATE INDEX issues_created ON issues (created_at DESC, id DESC, is_template, area, status);
DROP INDEX issues_updated;
CREATE INDEX issues_updated ON issues (updated_at DESC, id DESC, is_template, area, status);`},
	// Comments can be edited by their author and deleted (a tombstone) by the
	// author or a moderator (identifiers design, stage 2). identity is the
	// author's identity ULID ("" for comments made before).
	{"0003_comment_edit", `
ALTER TABLE comments ADD COLUMN identity TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN edited_at INTEGER;
ALTER TABLE comments ADD COLUMN deleted_at INTEGER;
ALTER TABLE comments ADD COLUMN deleted_by TEXT NOT NULL DEFAULT '';`},
	// Issues moved to or from another project (istore.Move): dir 'out' is
	// the redirect from an old id, 'in' where a moved-in issue came from
	// (pending until the move finished; mark is its "moved" history row).
	{"0004_moves", `
CREATE TABLE moves (
	dir      TEXT NOT NULL,
	old_id   TEXT NOT NULL,
	new_id   TEXT NOT NULL,
	project  TEXT NOT NULL,
	moved_at INTEGER NOT NULL,
	pending  INTEGER NOT NULL DEFAULT 0,
	mark     TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (dir, old_id)
) WITHOUT ROWID;
CREATE INDEX moves_new ON moves (dir, new_id);`},
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, m := range migrations {
		err := zmstore.Tx(ctx, db, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE name = ?`, m.name).Scan(&n); err != nil || n > 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, m.name, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

func (p *project) Prefix(ctx context.Context) (string, error) {
	var v string
	err := p.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'prefix'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p *project) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := p.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p *project) SetMeta(ctx context.Context, key, value string) error {
	_, err := p.w.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Reset empties the store (not its prefix): an import that failed part way
// starts again from nothing.
func (p *project) Reset(ctx context.Context) error {
	return zmstore.Tx(ctx, p.w, func(tx *sql.Tx) error {
		for _, t := range []string{"issues", "labels", "deps", "comments", "history", "child_counters", "kv", "moves"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO issues_fts (issues_fts) VALUES ('rebuild')`)
		return err
	})
}

func (p *project) SetPrefix(ctx context.Context, prefix string) error {
	_, err := p.w.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('prefix', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, prefix)
	return err
}

// bad and conflict are the write errors callers see.
func bad(format string, a ...any) error { return api.Errorf(api.CodeBadRequest, format, a...) }

func conflict(format string, a ...any) error { return api.Errorf(api.CodeConflict, format, a...) }
