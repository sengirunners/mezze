package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// AttachmentsFile is each project's attachment file, next to its issues.db.
// It is its own file so that nothing that copies issues.db (backups, the
// offsite home, a restore, a move) carries an image, and a removal is gone
// for good once the file's freed pages are overwritten.
const AttachmentsFile = "attachments.db"

var _ istore.Attacher = (*Store)(nil)

// attachments is one project's attachment file. One connection: reads and
// writes are rare and small, and a TRUNCATE checkpoint after a delete is
// never held up by a reader of our own.
type attachments struct {
	db *sql.DB
	// pending: a Delete's log truncation was held up (another process
	// reading the file); Settle runs it again.
	mu      sync.Mutex
	pending bool
}

func (s *Store) attachmentsPath(project string) string {
	return filepath.Join(s.home, zmstore.DirProjects, project, AttachmentsFile)
}

// Attachments implements istore.Attacher.
func (s *Store) Attachments(ctx context.Context, project string, create bool) (istore.AttachmentStore, error) {
	if project == "" || filepath.Base(project) != project || project == "." || project == ".." {
		return nil, fmt.Errorf("bad project id %q", project)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.atts[project]; ok {
		return a, nil
	}
	path := s.attachmentsPath(project)
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
	// secure_delete overwrites a removed row's pages, and auto_vacuum (set
	// before the first table) gives freed pages back to the file system.
	db, err := zmstore.OpenFile(ctx, path, 1, busyTimeout, false, "secure_delete(1)", "auto_vacuum(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, attachmentsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	a := &attachments{db: db}
	s.atts[project] = a
	return a, nil
}

// AttachmentProjects implements istore.Attacher.
func (s *Store) AttachmentProjects(ctx context.Context) ([]string, error) {
	dirs, err := os.ReadDir(filepath.Join(s.home, zmstore.DirProjects))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if _, err := os.Stat(s.attachmentsPath(d.Name())); d.IsDir() && err == nil {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// attachmentsSchema: one table, the bytes last so a metadata read never
// walks a blob's overflow pages. Only CREATE IF NOT EXISTS: a later change
// adds a migration list like issues.db's.
const attachmentsSchema = `
CREATE TABLE IF NOT EXISTS attachments (
	id         TEXT PRIMARY KEY,
	issue_id   TEXT NOT NULL,
	type       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	sha256     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	identity   TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL,
	data       BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS attachments_issue ON attachments (issue_id, expires_at);
CREATE INDEX IF NOT EXISTS attachments_expires ON attachments (expires_at);`

const attachmentCols = `id, issue_id, type, size, sha256, created_at, created_by, identity, expires_at`

func scanAttachment(sc interface{ Scan(...any) error }, extra ...any) (istore.Attachment, error) {
	var a istore.Attachment
	var created, expires int64
	err := sc.Scan(append([]any{&a.ID, &a.IssueID, &a.Type, &a.Size, &a.SHA256, &created, &a.CreatedBy, &a.Identity, &expires}, extra...)...)
	a.Created, a.Expires = fromMillis(created).UTC(), fromMillis(expires).UTC()
	return a, err
}

func (a *attachments) Add(ctx context.Context, at istore.Attachment, data []byte, max int, now time.Time) error {
	return zmstore.Tx(ctx, a.db, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM attachments WHERE issue_id = ? AND expires_at > ?`, at.IssueID, millis(now)).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return istore.ErrTooMany
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO attachments (`+attachmentCols+`, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			at.ID, at.IssueID, at.Type, at.Size, at.SHA256, millis(at.Created), at.CreatedBy, at.Identity, millis(at.Expires), data)
		return err
	})
}

func (a *attachments) query(ctx context.Context, where string, args ...any) ([]istore.Attachment, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT `+attachmentCols+` FROM attachments WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []istore.Attachment
	for rows.Next() {
		at, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, rows.Err()
}

func (a *attachments) List(ctx context.Context, issueIDs []string, now time.Time) (map[string][]istore.Attachment, error) {
	out := map[string][]istore.Attachment{}
	for len(issueIDs) > 0 {
		batch := issueIDs[:min(len(issueIDs), 500)]
		issueIDs = issueIDs[len(batch):]
		args := []any{millis(now)}
		for _, id := range batch {
			args = append(args, id)
		}
		ats, err := a.query(ctx, `expires_at > ? AND issue_id IN (?`+strings.Repeat(", ?", len(batch)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, at := range ats {
			out[at.IssueID] = append(out[at.IssueID], at)
		}
	}
	return out, nil
}

func (a *attachments) Get(ctx context.Context, issueID, id string, now time.Time) (istore.Attachment, []byte, error) {
	var data []byte
	at, err := scanAttachment(a.db.QueryRowContext(ctx, `SELECT `+attachmentCols+`, data FROM attachments WHERE id = ? AND issue_id = ? AND expires_at > ?`,
		id, issueID, millis(now)), &data)
	if errors.Is(err, sql.ErrNoRows) {
		return at, nil, istore.ErrNotFound
	}
	return at, data, err
}

func (a *attachments) Expired(ctx context.Context, now time.Time) ([]istore.Attachment, error) {
	return a.query(ctx, `expires_at <= ?`, millis(now))
}

func (a *attachments) Of(ctx context.Context, issueID string) ([]istore.Attachment, error) {
	return a.query(ctx, `issue_id = ?`, issueID)
}

func (a *attachments) Delete(ctx context.Context, ids []string) ([]string, error) {
	var gone []string
	err := zmstore.Tx(ctx, a.db, func(tx *sql.Tx) error {
		gone = nil
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				gone = append(gone, id)
			}
		}
		return nil
	})
	if err != nil || len(gone) == 0 {
		return nil, err
	}
	a.mu.Lock()
	a.pending = true
	a.mu.Unlock()
	return gone, a.Settle(ctx)
}

// Settle implements istore.AttachmentStore: the overwritten pages reach the
// file and the log is emptied, so no frame still holds the bytes. A
// checkpoint another reader holds up stays pending (tried a few times here,
// then on the next sweep).
func (a *attachments) Settle(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.pending {
		return nil
	}
	for try := 0; try < 5; try++ {
		var busy, logN, ckN int
		if err := a.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logN, &ckN); err != nil {
			return err
		}
		if busy == 0 {
			a.pending = false
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil // still pending: the next sweep settles it
}

// more runs the ops write.go's switch does not list.
func (w *writer) more() error {
	if w.m.Op == istore.OpAttachmentNote {
		return w.attachmentNote()
	}
	return bad("unknown write %q", w.m.Op)
}

// attachmentNote records an attachment's arrival or removal: a history row
// whose "attachment" field holds its metadata (no bytes), new on an attach,
// old on a removal or expiry.
func (w *writer) attachmentNote() error {
	n := w.m.Note
	if n == nil {
		return bad("an attachment note names its attachment")
	}
	switch n.Kind {
	case istore.KindAttached, istore.KindAttachmentRemoved, istore.KindAttachmentExpired:
	default:
		return bad("unknown attachment note %q", n.Kind)
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	a := n.Attachment
	meta := map[string]any{"id": a.ID, "type": a.Type, "size": a.Size, "sha256": a.SHA256,
		"expires": a.Expires.UTC().Format(time.RFC3339Nano)}
	d := fieldDiff{"attachment": {nil, meta}}
	if n.Kind != istore.KindAttached {
		d["attachment"] = [2]any{meta, nil}
	}
	return w.record(cur.ID, n.Kind, d, "", cur.Area, cur.Area)
}
