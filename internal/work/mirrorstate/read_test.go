package mirrorstate

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/mirrorquery"
	_ "modernc.org/sqlite"
)

func readDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE work_mirror (source TEXT, ref TEXT, id TEXT, area TEXT, status TEXT, title TEXT, body TEXT, item TEXT);
		INSERT INTO work_mirror VALUES
		('bd', 'a', 'bd:a', '', 'claimed', 'row title', '', '{"id":"old","project":"old","source":"old","area":"old","status":"done","title":"old","body":"old","owner":"alice","labels":["keep"],"extra":{"due_at":"tomorrow"}}'),
		('bd', 'b', 'bd:b', 'area', 'open', 'second', 'body', '{}'),
		('gh', 'a', 'gh:a', '', 'done', 'other source', '', '{}'),
		('bd', 'bad', 'bd:bad', '', 'open', 'bad', '', '{')`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestGetOverlaysStoredFields(t *testing.T) {
	db := readDB(t)
	it, ok, err := Get(context.Background(), db, "project", "bd", "a")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	want := orchapi.WorkItem{ID: "bd:a", Project: "project", Source: "bd", Status: orchapi.WorkClaimed,
		Title: "row title", Owner: "alice", Labels: []string{"keep"}, Extra: map[string]any{"due_at": "tomorrow"}}
	if !reflect.DeepEqual(it, want) {
		t.Fatalf("item = %#v, want %#v", it, want)
	}
	for _, ref := range []string{"missing", "bad"} {
		it, ok, err := Get(context.Background(), db, "project", "bd", ref)
		if ok || (err != nil) != (ref == "bad") {
			t.Fatalf("Get(%s): ok=%v err=%v", ref, ok, err)
		}
		if ref == "missing" && !reflect.DeepEqual(it, orchapi.WorkItem{}) {
			t.Fatalf("missing item = %#v", it)
		}
	}
}

func TestScanReadsTransactionRow(t *testing.T) {
	db := readDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE work_mirror SET title = 'uncommitted' WHERE source = 'bd' AND ref = 'a'`); err != nil {
		t.Fatal(err)
	}
	it, err := Scan(tx.QueryRowContext(ctx, `SELECT `+mirrorquery.Columns+` FROM work_mirror WHERE source = 'bd' AND ref = 'a'`), "project", "bd")
	if err != nil || it.Title != "uncommitted" {
		t.Fatalf("transaction item: %#v, err=%v", it, err)
	}
}

func TestListPreservesQueryOrderAndErrors(t *testing.T) {
	db := readDB(t)
	ctx := context.Background()
	query := `SELECT ` + mirrorquery.Columns + ` FROM work_mirror WHERE source = ? AND ref IN ('a', 'b') ORDER BY ref DESC`
	items, err := List(ctx, db, query, []any{"bd"}, "project", "bd")
	if err != nil || len(items) != 2 {
		t.Fatalf("List: %#v, err=%v", items, err)
	}
	if items[0].ID != "bd:b" || items[1].ID != "bd:a" || items[0].Project != "project" || items[1].Source != "bd" {
		t.Fatalf("ordered items: %#v", items)
	}
	items, err = List(ctx, db, query, []any{"missing"}, "project", "bd")
	if err != nil || items != nil {
		t.Fatalf("empty list: %#v, err=%v", items, err)
	}
	for _, query := range []string{`SELECT missing FROM work_mirror`, `SELECT ` + mirrorquery.Columns + ` FROM work_mirror WHERE ref = 'bad'`} {
		if items, err := List(ctx, db, query, nil, "project", "bd"); err == nil || items != nil {
			t.Fatalf("invalid list: %#v, err=%v", items, err)
		}
	}
}

func TestCountAndSourcesIncludeOnlyStoredState(t *testing.T) {
	db := readDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE work_mirror_sync (source TEXT); INSERT INTO work_mirror_sync VALUES ('bd'), ('sync-only')`); err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]int{"bd": 3, "gh": 1, "sync-only": 0} {
		if n, err := Count(ctx, db, source); err != nil || n != want {
			t.Fatalf("Count(%s) = %d, %v; want %d", source, n, err, want)
		}
	}
	sources, err := Sources(ctx, db)
	if err != nil || !reflect.DeepEqual(sources, []string{"bd", "gh", "sync-only"}) {
		t.Fatalf("Sources = %v, %v", sources, err)
	}
	if _, err := db.Exec(`DELETE FROM work_mirror; DELETE FROM work_mirror_sync`); err != nil {
		t.Fatal(err)
	}
	if sources, err := Sources(ctx, db); err != nil || sources != nil {
		t.Fatalf("empty Sources = %v, %v", sources, err)
	}
	if _, err := db.Exec(`DROP TABLE work_mirror`); err != nil {
		t.Fatal(err)
	}
	if _, err := Count(ctx, db, "bd"); err == nil {
		t.Fatal("Count ignored query error")
	}
	if _, err := Sources(ctx, db); err == nil {
		t.Fatal("Sources ignored query error")
	}
}
