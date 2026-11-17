package team

import (
	"context"
	"encoding/json"
	"path/filepath"

	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func snapLine(t *testing.T, it orchapi.WorkItem) api.SnapshotLine {
	b, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	return api.SnapshotLine{Kind: orchapi.SnapItem, Data: b}
}

// Refresh at the current cursor, before the move event: row may land, the move then removes it.
func TestAttackOrderRefreshThenMove(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:harbor-1", Title: "old"}
	if _, err := rp.applyChanges(ctx, nil, "02", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := rp.putWhole(ctx, it, "02", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "03", Type: orchapi.EventWorkMoved, Subject: it.ID}}, "03", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("row kept")
	}
}

// Two stale refreshes in either order after the move.
func TestAttackOrderTwoStale(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:harbor-1", Title: "old"}
	if err := rp.putWhole(ctx, it, "01", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "05", Type: orchapi.EventWorkMoved, Subject: it.ID}}, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"03", "02", "04", ""} {
		if err := rp.putWhole(ctx, it, b, 0); err == nil {
			t.Errorf("stale refresh at %q accepted", b)
		}
		if _, _, ok := rp.findItem(ctx, it.ID); ok {
			t.Fatalf("stale refresh at %q restored the row", b)
		}
	}
}

// A rejected refresh keeps a pending redaction refetch row, so a restart still retries.
func TestAttackRejectedKeepsRefetch(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	if _, err := rp.applyChanges(ctx, nil, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.db.ExecContext(ctx, `INSERT INTO refetch (id) VALUES ('bd:x')`); err != nil {
		t.Fatal(err)
	}
	if err := rp.putWhole(ctx, orchapi.WorkItem{ID: "bd:x"}, "01", 0); err == nil {
		t.Fatal("accepted")
	}
	var n int
	_ = rp.db.QueryRowContext(ctx, `SELECT count(*) FROM refetch`).Scan(&n)
	if n != 1 {
		t.Fatalf("refetch row lost on rejection: %d", n)
	}
}

// A snapshot taken before the move, applied, then the changelog: the row goes.
func TestAttackEarlierSnapshotThenMove(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:harbor-1", Title: "old"}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "02", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "03", Type: orchapi.EventWorkMoved, Subject: it.ID}}, "03", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("row kept after an earlier snapshot")
	}
}

// A late work.changed/status event for the old id with a higher id than the move
// (any publisher running behind the move) revives the row: nothing tombstones it.
func TestAttackEventAfterMoveRevives(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:harbor-1", Title: "old"}
	fetch, err := rp.applyChanges(ctx, []api.Event{{ID: "03", Type: orchapi.EventWorkMoved, Subject: it.ID}, {ID: "04", Type: orchapi.EventWorkChanged, Subject: it.ID, Data: mustJSON(t, it)}}, "04", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fetch) != 1 || fetch[0] != it.ID {
		t.Fatalf("later event must be verified: %v", fetch)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Errorf("a changed event after the move revived the row")
	}
	if err := rp.dropItem(ctx, it.ID); err != nil {
		t.Fatal(err)
	} // authority answers not found
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("missing item was restored")
	}
}

// The marker survives a replica restart and is cleared by a real move back.
func TestMovedEventSurvivesRestartAndMoveBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "copy.db")
	db, err := copies(t).OpenReplica(ctx, path, 4)
	if err != nil {
		t.Fatal(err)
	}
	rp := &replica{db: db}
	it := orchapi.WorkItem{ID: "bd:harbor-1", Title: "old"}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "03", Type: orchapi.EventWorkMoved, Subject: it.ID}}, "03", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = copies(t).OpenReplica(ctx, path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rp = &replica{db: db}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "04", Type: orchapi.EventWorkChanged, Subject: it.ID, Data: mustJSON(t, it)}}, "04", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("late event revived moved item after restart")
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "05", Type: orchapi.EventWorkCreated, Subject: it.ID, Data: mustJSON(t, it)}}, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); !ok {
		t.Fatal("move back did not restore item")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
