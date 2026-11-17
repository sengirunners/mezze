package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

var _ istore.Restorer = (*Store)(nil)

// Copy implements istore.Restorer. The handle is its own connection: the
// reader pool is query_only (which refuses VACUUM INTO) and the writer
// connection must stay free.
func (s *Store) Copy(ctx context.Context, id string, write func(context.Context, *sql.DB) error) error {
	ps, err := s.Project(ctx, id, true)
	if err != nil {
		return err
	}
	db, err := zmstore.OpenFile(ctx, ps.(*project).path, 1, busyTimeout, false)
	if err != nil {
		return err
	}
	defer db.Close()
	return write(ctx, db)
}

// OpenCopy implements istore.Restorer.
func (s *Store) OpenCopy(ctx context.Context, id, path string) (istore.ProjectStore, func() error, error) {
	if err := checkCopy(ctx, path); err != nil {
		return nil, nil, err
	}
	p, err := openProject(ctx, id, path)
	if err != nil {
		return nil, nil, err
	}
	return p, func() error {
		err := p.close()
		// A log left beside the copy would stay behind at the rename, with
		// whatever the last writes (a redaction) put in it.
		if st, serr := os.Stat(path + "-wal"); err == nil && serr == nil && st.Size() > 0 {
			err = fmt.Errorf("the copy's log did not fold in")
		}
		return err
	}, nil
}

// checkCopy refuses what OpenCopy must not migrate: a file that is not an
// issue store (project.db has schema_migrations too), or a newer one.
func checkCopy(ctx context.Context, path string) error {
	db, err := zmstore.OpenFile(ctx, path, 1, busyTimeout, true)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('issues', 'schema_migrations')`).Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		return fmt.Errorf("%s is not an issue store", filepath.Base(path))
	}
	known := map[string]bool{}
	for _, m := range migrations {
		known[m.name] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !known[name] {
			return fmt.Errorf("%w: it has migration %s", istore.ErrSchemaNewer, name)
		}
	}
	return rows.Err()
}

// Replace implements istore.Restorer. A store with no handle open is fine:
// a machine that lost its file is what a restore is for.
func (s *Store) Replace(ctx context.Context, id, path string) error {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return fmt.Errorf("bad project id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.path(id)
	if p, ok := s.open[id]; ok {
		delete(s.open, id)
		_ = p.close() // a damaged store may not close cleanly; it is going
	}
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		return err
	}
	swapped := swapFile(live, path)
	// Open whichever file is there, so a failed rename leaves the store served.
	p, err := openProject(ctx, id, live)
	if err != nil {
		if swapped != nil {
			return fmt.Errorf("%v; and reopening the store failed: %w", swapped, err)
		}
		return err
	}
	s.open[id] = p
	return swapped
}

// walAside is the old store's log during a swap. openProject removes one a
// crash left: it may hold user text, and it cannot be told whether it belongs
// to the file now there.
const walAside = "-wal.aside"

// replaceStep lets a test fail the swap at a step.
var replaceStep = func(step string) error { return nil }

// swapFile renames path over live. A log left beside the new file would be
// replayed into it, so live's goes aside first, and comes back if the rename
// fails: the store it belongs to is the one that stays.
func swapFile(live, path string) error {
	if err := replaceStep("closed"); err != nil {
		return err
	}
	aside := live + walAside
	if err := os.Rename(live+"-wal", aside); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(live + "-shm"); err != nil && !os.IsNotExist(err) {
		return putBack(aside, live, err)
	}
	if err := replaceStep("aside"); err != nil {
		return putBack(aside, live, err)
	}
	if err := os.Rename(path, live); err != nil {
		return putBack(aside, live, err)
	}
	os.Remove(aside)
	d, err := os.Open(filepath.Dir(live))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func putBack(aside, live string, cause error) error {
	if err := os.Rename(aside, live+"-wal"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; and its log did not go back: %v", cause, err)
	}
	return cause
}
