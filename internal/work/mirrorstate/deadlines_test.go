package mirrorstate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/rollup"
	_ "modernc.org/sqlite"
)

func TestStoredDeadlinesUseMarkedIndex(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE work_mirror (source TEXT, ref TEXT, id TEXT, item TEXT, title TEXT, labels TEXT, area TEXT, status TEXT, updated INTEGER, PRIMARY KEY(source,ref))`)
	if err != nil {
		t.Fatal(err)
	}
	table := rollup.Table{Name: "work_mirror", JSON: "item", Title: "title", Labels: "labels", Key: []string{"source", "ref"}}
	if _, err = db.Exec(table.IndexSQL()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN "+deadlineIDsQuery, "bd")
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "work_mirror_rollup_marked") {
		t.Fatalf("stored deadlines scan item rows: %s", plan)
	}
	// The same ref in a different tracker must not become a candidate.
	for _, row := range [][3]string{{"bd", "bd:old", `{"extra":{"due_at":"tomorrow"}}`}, {"gh", "gh:old", `{"extra":{"due_at":"tomorrow"}}`}, {"bd", "bd:ep", `{"extra":{"issue_type":"epic"}}`}, {"bd", "bd:empty", `{"extra":{"defer_until":""}}`}} {
		if _, err := db.Exec(`INSERT INTO work_mirror(source,ref,id,item) VALUES(?,?,?,?)`, row[0], row[1], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := deadlineIDs(context.Background(), db, "bd")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || !ids["bd:old"] {
		t.Fatalf("deadline candidates: %v", ids)
	}
}
