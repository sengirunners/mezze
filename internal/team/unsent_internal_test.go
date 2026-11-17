package team

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

func unsentReplica(t *testing.T) *replica {
	t.Helper()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "P1.db")
	db, err := copies(t).OpenReplica(context.Background(), path, 2)
	if err != nil {
		t.Fatal(err)
	}
	return &replica{db: db, path: path, refresh: map[string]bool{}}
}

func addOut(t *testing.T, rp *replica, id, body, state string, created int64) {
	t.Helper()
	if _, err := rp.db.Exec(`INSERT INTO outbox (id, kind, method, path, body, as_addr, item, state, created) VALUES (?, 'comment', 'POST', '/v0/work/x/comments', ?, 'bea-mac/aa', 'issue:X', ?, ?)`,
		id, body, state, created); err != nil {
		t.Fatal(err)
	}
}

func readUnsent(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(unsentPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestUnsentExportOnDrop(t *testing.T) {
	rp := unsentReplica(t)
	addOut(t, rp, "w1", `{"body":"hi"}`, api.OutboxPending, 1700000000000)
	addOut(t, rp, "w2", `not json`, api.OutboxPending, 1700000001000)
	addOut(t, rp, "w3", `{"body":"done"}`, api.OutboxAcked, 1700000002000)
	path := rp.path
	n, err := rp.close(true)
	if err != nil || n != 2 {
		t.Fatalf("close = %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("db still there: %v", err)
	}
	st, err := os.Stat(unsentPath(path))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("export file: %v %v", st, err)
	}
	rows := readUnsent(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	b, _ := json.Marshal(rows[0]["body"])
	if rows[0]["id"] != "w1" || string(b) != `{"body":"hi"}` || rows[0]["kind"] != "comment" || rows[0]["method"] != "POST" ||
		rows[0]["path"] != "/v0/work/x/comments" || rows[0]["as"] != "bea-mac/aa" || rows[0]["item"] != "issue:X" ||
		rows[0]["created"] != "2023-11-14T22:13:20Z" {
		t.Fatalf("row 0 = %v", rows[0])
	}
	if rows[1]["id"] != "w2" || rows[1]["body"] != "not json" {
		t.Fatalf("row 1 = %v", rows[1])
	}
	if p, c := unsentSaved(path); p != unsentPath(path) || c != 2 {
		t.Fatalf("unsentSaved = %q %d", p, c)
	}
}

func TestUnsentExportNothingPending(t *testing.T) {
	rp := unsentReplica(t)
	addOut(t, rp, "w1", `{}`, api.OutboxAcked, 1)
	addOut(t, rp, "w2", `{}`, api.OutboxFailed, 1)
	path := rp.path
	if n, err := rp.close(true); err != nil || n != 0 {
		t.Fatalf("close = %d, %v", n, err)
	}
	if _, err := os.Stat(unsentPath(path)); !os.IsNotExist(err) {
		t.Fatalf("export file exists: %v", err)
	}
	if p, c := unsentSaved(path); p != "" || c != 0 {
		t.Fatalf("unsentSaved = %q %d", p, c)
	}
}

func TestUnsentExportAppends(t *testing.T) {
	rp := unsentReplica(t)
	path := rp.path
	addOut(t, rp, "w1", `{"a":1}`, api.OutboxPending, 1)
	if _, err := rp.close(true); err != nil {
		t.Fatal(err)
	}
	// The project is joined again, queues more, and is dropped again.
	db, err := copies(t).OpenReplica(context.Background(), path, 2)
	if err != nil {
		t.Fatal(err)
	}
	rp2 := &replica{db: db, path: path, refresh: map[string]bool{}}
	addOut(t, rp2, "w9", `{"a":2}`, api.OutboxPending, 2)
	if _, err := rp2.close(true); err != nil {
		t.Fatal(err)
	}
	rows := readUnsent(t, path)
	if len(rows) != 2 || rows[0]["id"] != "w1" || rows[1]["id"] != "w9" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestUnsentExportFileWithoutReplica(t *testing.T) {
	rp := unsentReplica(t)
	addOut(t, rp, "w1", `{}`, api.OutboxPending, 1)
	path := rp.path
	rp.db.Close()
	n, err := exportUnsentFile(context.Background(), copies(t), path)
	if err != nil || n != 1 {
		t.Fatalf("exportUnsentFile = %d, %v", n, err)
	}
	if n, err := exportUnsentFile(context.Background(), copies(t), path+".missing"); n != 0 || err != nil {
		t.Fatalf("missing: %d %v", n, err)
	}
}

// copies is a store whose replica copies get team's copy migrations, as the
// daemon's do (Migrations, through core.Deps.AddMigrations).
func copies(tb testing.TB) *store.Store {
	tb.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		tb.Fatal(err)
	}
	st, err := store.Open(ctx, dir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close(); os.RemoveAll(dir) })
	var ms []store.Migration
	for _, m := range Migrations {
		if m.Kind == store.KindReplica {
			ms = append(ms, m)
		}
	}
	if err := st.AddMigrations(ctx, ms...); err != nil {
		tb.Fatal(err)
	}
	return st
}
