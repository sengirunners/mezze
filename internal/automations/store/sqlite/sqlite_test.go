package sqlite_test

import (
	"context"
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/automations/store"
	"github.com/sengirunners/mezze/internal/automations/store/sqlite"
	"github.com/sengirunners/mezze/internal/automations/store/storetest"
	kstore "github.com/sengirunners/potluck/kit/store"
)

// The SQLite driver passes the conformance suite on a project.db with its
// migration, through one pool (a solo project's reads and writes).
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		dir, err := os.MkdirTemp("", "zm")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		ctx := context.Background()
		st, err := kstore.OpenForWiring(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.AddMigrations(ctx, sqlite.Migrations...); err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		db, err := st.CreateProject(ctx, storetest.Project)
		if err != nil {
			t.Fatal(err)
		}
		return sqlite.New(db, st.Writer(storetest.Project))
	})
}
