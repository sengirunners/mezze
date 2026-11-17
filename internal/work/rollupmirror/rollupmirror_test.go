package rollupmirror

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sengirunners/mezze/internal/rollup/rolluptest"
)

func TestConformance(t *testing.T) {
	rolluptest.Run(t, rolluptest.Driver{Table: Table, Children: true,
		// As work/0002_mirror makes it.
		Create: `CREATE TABLE work_mirror (source TEXT NOT NULL, ref TEXT NOT NULL, id TEXT NOT NULL, area TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '[]',
			updated INTEGER NOT NULL DEFAULT 0, gen INTEGER NOT NULL DEFAULT 0, item TEXT NOT NULL, UNIQUE (source, ref))`,
		// The mirror keeps title, body, labels and area as columns, the rest as JSON.
		Insert: func(tx *sql.Tx, id, ref, area, status string, updated int64, title string, labels []string, item map[string]any) error {
			body, _ := item["body"].(string)
			delete(item, "title")
			delete(item, "body")
			delete(item, "labels")
			js, _ := json.Marshal(item)
			lb, _ := json.Marshal(labels)
			_, err := tx.Exec(`INSERT INTO work_mirror (source, ref, id, area, status, title, body, labels, updated, item) VALUES (?,?,?,?,?,?,?,?,?,?)`,
				"bd", ref, id, area, status, title, body, string(lb), updated, string(js))
			return err
		}})
}
