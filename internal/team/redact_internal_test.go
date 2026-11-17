package team

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

func testReplica(t *testing.T) *replica {
	t.Helper()
	db, err := copies(t).OpenReplica(context.Background(), filepath.Join(t.TempDir(), "r.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &replica{db: db, refresh: map[string]bool{}}
}

func outRow(t *testing.T, db *sql.DB, id, kind, item, state, body, result string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO outbox (id, kind, method, path, body, item, state, result, created) VALUES (?, ?, 'POST', '/x', ?, ?, ?, ?, 0)`,
		id, kind, body, item, state, result); err != nil {
		t.Fatal(err)
	}
}

func outBody(t *testing.T, db *sql.DB, id string) (body, result string) {
	t.Helper()
	if err := db.QueryRow(`SELECT body, result FROM outbox WHERE id = ?`, id).Scan(&body, &result); err != nil {
		t.Fatal(err)
	}
	return
}

// A short redacted comment ("done") is scrubbed from its own outbox rows
// only where it is a text field: a status write of "done" on the same item
// stays as it was.
func TestNoteRedactionShortBody(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const item = "issue:01ABC"
	outRow(t, rp.db, "w1", "comment", item, api.OutboxAcked, `{"body":"done"}`, `{"id":"c1","item":"issue:01ABC","body":"done"}`)
	outRow(t, rp.db, "w2", "status", item, api.OutboxPending, `{"status":"done"}`, "")
	err := store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		if err := putComment(ctx, tx, orchapi.Comment{ID: "c1", Item: item, Body: "done"}); err != nil {
			return err
		}
		return putComment(ctx, tx, orchapi.Comment{ID: "c1", Item: item, Body: core.Redacted})
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, r := outBody(t, rp.db, "w1"); b != `{"body":"[redacted]"}` || r != `{"body":"[redacted]","id":"c1","item":"issue:01ABC"}` {
		t.Errorf("comment row: %s / %s", b, r)
	}
	if b, _ := outBody(t, rp.db, "w2"); b != `{"status":"done"}` {
		t.Errorf("status row changed: %s", b)
	}
	if err := rp.compactIfMarked(ctx); err != nil {
		t.Fatal(err)
	}
	if rp.meta(ctx, metaCompact) != "" {
		t.Error("the compaction mark is still set after a compaction")
	}
}

// The refetch a redaction asks for is written down in the same transaction
// (a restart before the fetch would otherwise lose it) and cleared when the
// item lands or is dropped.
func TestRedactionRefetchIsDurable(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const id = "01ISSUE"
	err := store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		if err := putItem(ctx, tx, orchapi.WorkItem{ID: "issue:" + id, Area: "solver"}, "", true); err != nil {
			return err
		}
		ids, err := applyEvent(ctx, tx, api.Event{Type: orchapi.EventIssueCommentRedacted, Subject: id})
		if len(ids) != 1 || ids[0] != "issue:"+id {
			t.Errorf("refetch ids %v", ids)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = rp.db.QueryRow(`SELECT count(*) FROM refetch`).Scan(&n)
	if n != 1 {
		t.Fatalf("refetch rows %d", n)
	}
	if err := rp.putWhole(ctx, orchapi.WorkItem{ID: "issue:" + id, Area: "solver"}, "", 0); err != nil {
		t.Fatal(err)
	}
	_ = rp.db.QueryRow(`SELECT count(*) FROM refetch`).Scan(&n)
	if n != 0 {
		t.Errorf("refetch rows after the fetch %d", n)
	}
}

// An item dropped from the copy (404 on refetch, or gone from this
// replica's areas) takes its settled outbox rows with it; a pending write
// still goes out.
func TestDropItemForgetsSettledOutbox(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const item = "issue:01GONE"
	outRow(t, rp.db, "s1", "comment", item, api.OutboxAcked, `{"body":"AKIA-DROPPED-SECRET"}`, "")
	outRow(t, rp.db, "p1", "status", item, api.OutboxPending, `{"status":"done"}`, "")
	if _, err := rp.db.Exec(`INSERT INTO refetch (id) VALUES (?)`, item); err != nil {
		t.Fatal(err)
	}
	if err := rp.dropItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, _ := rp.db.Query(`SELECT id FROM outbox ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != "p1" {
		t.Errorf("outbox after drop: %v", ids)
	}
	var n int
	_ = rp.db.QueryRow(`SELECT count(*) FROM refetch`).Scan(&n)
	if n != 0 || rp.meta(ctx, metaCompact) != "" {
		t.Errorf("refetch %d, compact mark %q", n, rp.meta(ctx, metaCompact))
	}
}

// A redacted comment's old text reaches this machine's other modules:
// queued with the redaction, handed to the scrubbers with the
// copy's project once the apply is done, kept while they fail, then
// forgotten.
func TestRedactionScrubsThisMachine(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	rp.project = "01PROJECT"
	var got []core.Scrub
	fail := true
	rp.scrub = func(_ context.Context, sc core.Scrub) error {
		got = append(got, sc)
		if fail {
			return errors.New("busy")
		}
		return nil
	}
	const item, secret = "issue:01ABC", "the staging password is hunter2-long"
	err := store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		if err := putComment(ctx, tx, orchapi.Comment{ID: "c1", Item: item, Body: secret}); err != nil {
			return err
		}
		return putComment(ctx, tx, orchapi.Comment{ID: "c1", Item: item, Body: core.Redacted})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.compactIfMarked(ctx); err == nil {
		t.Fatal("a failed scrub was taken for done")
	}
	if rp.meta(ctx, metaCompact) == "" {
		t.Fatal("the mark went with the scrub undone")
	}
	fail = false
	if err := rp.compactIfMarked(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Project != "01PROJECT" || len(got[1].Texts) != 1 || got[1].Texts[0] != secret ||
		len(got[1].Subjects) != 2 || got[1].Subjects[0] != item || got[1].Subjects[1] != "01ABC" {
		t.Fatalf("scrubs: %+v", got)
	}
	var n int
	if err := rp.db.QueryRow(`SELECT count(*) FROM pending_scrubs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("queued after the scrub: %d %v", n, err)
	}
	if err := rp.compactIfMarked(ctx); err != nil || len(got) != 2 {
		t.Fatalf("scrubbed again: %d %v", len(got), err)
	}
	// A text queued after the drain (another apply) keeps the mark, so the
	// next round still scrubs it; an apply's caller is not told it failed.
	rp.drainMu.Lock() // another caller is draining: this round skips it
	if err := store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		if err := putComment(ctx, tx, orchapi.Comment{ID: "c2", Item: item, Body: secret + " again"}); err != nil {
			return err
		}
		return putComment(ctx, tx, orchapi.Comment{ID: "c2", Item: item, Body: core.Redacted})
	}); err != nil {
		t.Fatal(err)
	}
	if err := rp.compactIfMarked(ctx); err != nil || rp.meta(ctx, metaCompact) == "" {
		t.Fatalf("mark with a text queued: %q %v", rp.meta(ctx, metaCompact), err)
	}
	rp.drainMu.Unlock()
	rp.scrub = func(context.Context, core.Scrub) error { return errors.New("still recording") }
	if err := rp.compactAfterApply(ctx); err != nil {
		t.Fatalf("an apply failed for a pending scrub: %v", err)
	}
}
