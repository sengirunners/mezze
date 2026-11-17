package team

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sengirunners/mezze/internal/rollup/rolluptest"
)

func TestRollupConformance(t *testing.T) {
	rolluptest.Run(t, rolluptest.Driver{Table: copyTable,
		// As team/0001_copy and team/0005_rollup make it.
		Create: `CREATE TABLE items (id TEXT PRIMARY KEY, area TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
			created INTEGER NOT NULL DEFAULT 0, base TEXT NOT NULL DEFAULT '', json TEXT NOT NULL, updated INTEGER NOT NULL DEFAULT 0)`,
		Insert: func(tx *sql.Tx, id, _, area, status string, updated int64, _ string, _ []string, item map[string]any) error {
			js, _ := json.Marshal(item)
			_, err := tx.Exec(`INSERT INTO items (id, area, status, source, json, updated) VALUES (?,?,?,?,?,?)`, id, area, status, "bd", string(js), updated)
			return err
		}})
}
