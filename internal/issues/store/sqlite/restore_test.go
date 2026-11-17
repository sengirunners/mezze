package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

func openStore(t *testing.T) (*Store, istore.ProjectStore, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "zmis")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	s := Open(home)
	t.Cleanup(func() { s.Close() })
	p, err := s.Project(context.Background(), "01PROJ", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPrefix(context.Background(), "oc"); err != nil {
		t.Fatal(err)
	}
	return s, p, home
}

func copyOf(t *testing.T, s *Store, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, "copy.db")
	if err := s.Copy(context.Background(), "01PROJ", func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return dst
}

// The swap: the copy's rows are served, a log the old store left is not
// replayed into it, and the store works afterwards.
func TestReplaceSwapsTheStore(t *testing.T) {
	ctx := context.Background()
	s, p, home := openStore(t)
	kept := create(t, p, orchapi.NewIssueRequest{Title: "in the copy"})
	cp := copyOf(t, s, t.TempDir())
	gone := create(t, p, orchapi.NewIssueRequest{Title: "after the copy"})
	if err := s.Replace(ctx, "01PROJ", cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy stayed: %v", err)
	}
	q, err := s.Project(ctx, "01PROJ", false)
	if err != nil {
		t.Fatal(err)
	}
	all := istore.Areas{All: true}
	if _, err := q.Get(ctx, kept.ID, all); err != nil {
		t.Fatalf("the copy's issue: %v", err)
	}
	if _, err := q.Get(ctx, gone.ID, all); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("the later issue: %v", err)
	}
	create(t, q, orchapi.NewIssueRequest{Title: "a write after the swap"})
	live := filepath.Join(home, "projects", "01PROJ", FileName)
	if st, err := os.Stat(live); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store file: %+v %v", st, err)
	}
}

// A machine that lost its store restores into nothing.
func TestReplaceIntoNoStore(t *testing.T) {
	ctx := context.Background()
	s, p, home := openStore(t)
	it := create(t, p, orchapi.NewIssueRequest{Title: "kept"})
	cp := copyOf(t, s, t.TempDir())
	s.Close()
	if err := os.RemoveAll(filepath.Join(home, "projects")); err != nil {
		t.Fatal(err)
	}
	s2 := Open(home)
	defer s2.Close()
	if err := s2.Replace(ctx, "01PROJ", cp); err != nil {
		t.Fatal(err)
	}
	q, err := s2.Project(ctx, "01PROJ", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(ctx, it.ID, istore.Areas{All: true}); err != nil {
		t.Fatal(err)
	}
}

// A failed rename leaves the old store served.
func TestReplaceFailureKeepsTheStore(t *testing.T) {
	ctx := context.Background()
	s, p, _ := openStore(t)
	it := create(t, p, orchapi.NewIssueRequest{Title: "kept"})
	if err := s.Replace(ctx, "01PROJ", filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("no error")
	}
	q, err := s.Project(ctx, "01PROJ", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(ctx, it.ID, istore.Areas{All: true}); err != nil {
		t.Fatal(err)
	}
}

// A failure at any step before the rename leaves the old store served, with
// its log: the rows here live only in the log (a close that could not fold
// it in), so they survive only if it comes back. The copy stays for a retry.
func TestSwapFailureKeepsTheStoreAndItsLog(t *testing.T) {
	for _, step := range []string{"closed", "aside", "rename"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			oldEvery := checkpointEvery
			checkpointEvery = time.Hour // the log stays unfolded
			t.Cleanup(func() { checkpointEvery = oldEvery })
			s, p, home := openStore(t)
			cp := copyOf(t, s, t.TempDir())
			it := create(t, p, orchapi.NewIssueRequest{Title: "only in the log"})
			live := filepath.Join(home, "projects", "01PROJ", FileName)
			mainFile, err := os.ReadFile(live)
			if err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(live + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			boom := errors.New("boom")
			old := replaceStep
			t.Cleanup(func() { replaceStep = old })
			aside := false
			replaceStep = func(at string) error {
				if at == "closed" {
					if err := os.WriteFile(live, mainFile, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(live+"-wal", log, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if at == "aside" {
					_, err := os.Stat(live + "-wal")
					aside = errors.Is(err, os.ErrNotExist)
				}
				if at == step {
					return boom
				}
				return nil
			}
			from := cp
			if step == "rename" {
				from = cp + ".missing" // the rename itself fails
			}
			err = s.Replace(ctx, "01PROJ", from)
			if step == "rename" {
				if err == nil {
					t.Fatal("no error")
				}
			} else if !errors.Is(err, boom) {
				t.Fatalf("err %v", err)
			}
			if step != "closed" && !aside {
				t.Fatal("the log was still beside the store at the swap")
			}
			if _, err := os.Stat(live + walAside); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the log stayed aside: %v", err)
			}
			q, err := s.Project(ctx, "01PROJ", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.Get(ctx, it.ID, istore.Areas{All: true}); err != nil {
				t.Fatalf("the store after a failed swap: %v", err)
			}
			if _, err := os.Stat(cp); err != nil {
				t.Fatalf("the copy: %v", err)
			}
		})
	}
}

// A log a crash left aside goes at the next open: it may hold text, and it
// cannot be told which store it belongs to.
func TestLeftoverLogIsRemoved(t *testing.T) {
	ctx := context.Background()
	s, _, home := openStore(t)
	s.Close()
	aside := filepath.Join(home, "projects", "01PROJ", FileName+walAside)
	if err := os.WriteFile(aside, []byte("old frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := Open(home)
	defer s2.Close()
	if _, err := s2.Project(ctx, "01PROJ", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("it stayed: %v", err)
	}
}

// OpenCopy migrates an older copy up, and refuses a newer one or a file that
// is not a store, leaving the live store alone either way.
func TestOpenCopy(t *testing.T) {
	ctx := context.Background()
	s, p, _ := openStore(t)
	create(t, p, orchapi.NewIssueRequest{Title: "x"})
	dir := t.TempDir()
	cp := copyOf(t, s, dir)
	db, err := sql.Open("sqlite", "file:"+cp)
	if err != nil {
		t.Fatal(err)
	}
	last := migrations[len(migrations)-1].name
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = ?`, last); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('9999_future', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, _, err := s.OpenCopy(ctx, "01PROJ", cp); !errors.Is(err, istore.ErrSchemaNewer) {
		t.Fatalf("newer: %v", err)
	}
	plain := filepath.Join(dir, "plain.db")
	db, err = sql.Open("sqlite", "file:"+plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, _, err := s.OpenCopy(ctx, "01PROJ", plain); err == nil || !strings.Contains(err.Error(), "not an issue store") {
		t.Fatalf("not a store: %v", err)
	}
}
