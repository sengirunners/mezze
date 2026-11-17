package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// The first snapshot under a new restore generation fails
// the acked writes it does not show (taken by the home, then lost with its
// restore) instead of overlaying them as ghosts, drops the ones it shows,
// leaves pending ones to be sent, and names the items the home lost.
func TestRestoreSnapshotSettlesLostWrites(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	kept := orchapi.WorkItem{ID: "local:kept", Title: "kept", Status: orchapi.WorkOpen}
	gone := orchapi.WorkItem{ID: "local:gone", Title: "gone", Status: orchapi.WorkOpen}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, kept), snapLine(t, gone)}, "01", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ id, kind, item, state string }{
		{"01LOSTACK", "status", kept.ID, api.OutboxAcked},    // acked, not shown after: lost
		{"01SHOWNACK", "claim", gone.ID, api.OutboxAcked},    // a lease write on an item gone: shown, dropped
		{"01PENDING", "comment", kept.ID, api.OutboxPending}, // still to send
	} {
		if _, err := rp.db.Exec(`INSERT INTO outbox (id, kind, method, path, body, as_addr, item, seen, state, created) VALUES (?, ?, 'POST', '/x', '{}', 'bea-mac/s1', ?, '99', ?, 0)`,
			e.id, e.kind, e.item, e.state); err != nil {
			t.Fatal(err)
		}
	}
	// Same generation: nothing settles.
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, kept)}, "02", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	if rp.lostToRestore(gone.ID) || rp.meta(ctx, "restore") != "" {
		t.Fatal("a snapshot under the same generation counted a restore")
	}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, gone)}, "03", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	// The restore: the home lost kept's status write and, now, gone.
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, kept)}, "02", time.Now(), 0, snapshotAnswer{restore: "abc"}); err != nil {
		t.Fatal(err)
	}
	if rp.meta(ctx, "restore") != "abc" || !rp.lostToRestore(gone.ID) || rp.lostToRestore(kept.ID) {
		t.Fatalf("restore note: gen %q lost %v", rp.meta(ctx, "restore"), rp.lost)
	}
	state := func(id string) (string, string) {
		var st, msg string
		_ = rp.db.QueryRow(`SELECT state, error FROM outbox WHERE id = ?`, id).Scan(&st, &msg)
		return st, msg
	}
	if st, msg := state("01LOSTACK"); st != api.OutboxFailed || !strings.Contains(msg, "restored from a backup") {
		t.Fatalf("a lost acked write: %q %q", st, msg)
	}
	if st, _ := state("01SHOWNACK"); st != "" {
		t.Fatalf("an acked write the snapshot shows was kept: %q", st)
	}
	if st, _ := state("01PENDING"); st != api.OutboxPending {
		t.Fatalf("a pending write was settled by the restore: %q", st)
	}
	if len(rp.lostAcks) != 1 || rp.lostAcks[0].id != "01LOSTACK" {
		t.Fatalf("writes to tell their sessions: %+v", rp.lostAcks)
	}
	// Reopened, the copy keeps its generation and what it lost.
	if decodeLost(rp.meta(ctx, "restore_lost"))[gone.ID] != true {
		t.Fatal("restore_lost not kept")
	}
}

// A copy from a zm before restore generations (ready, no restore_known)
// meeting a home restored earlier re-snapshots, but counts no restore: what
// it lacks may be ordinary deletions, and its acked writes may have landed.
func TestUpgradedCopyCountsNoRestore(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	kept := orchapi.WorkItem{ID: "local:kept", Title: "kept", Status: orchapi.WorkOpen}
	gone := orchapi.WorkItem{ID: "local:deleted", Title: "deleted", Status: orchapi.WorkOpen}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, kept), snapLine(t, gone)}, "01", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	// As an older zm left it.
	if _, err := rp.db.Exec(`DELETE FROM meta WHERE key IN ('restore', 'restore_known')`); err != nil {
		t.Fatal(err)
	}
	rp.restore, rp.known = "", false
	if _, err := rp.db.Exec(`INSERT INTO outbox (id, kind, method, path, body, as_addr, item, seen, state, created) VALUES ('01LANDED', 'status', 'POST', '/x', '{}', 'bea-mac/s1', ?, '99', 'acked', 0)`, kept.ID); err != nil {
		t.Fatal(err)
	}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, kept)}, "02", time.Now(), 0, snapshotAnswer{restore: "abc"}); err != nil {
		t.Fatal(err)
	}
	if rp.lostToRestore(gone.ID) || len(rp.lostAcks) != 0 {
		t.Fatalf("an upgraded copy counted a restore: lost %v acks %+v", rp.lost, rp.lostAcks)
	}
	var st string
	_ = rp.db.QueryRow(`SELECT state FROM outbox WHERE id = '01LANDED'`).Scan(&st)
	if st != api.OutboxAcked {
		t.Fatalf("an upgraded copy's acked write was settled: %q", st)
	}
	if rp.meta(ctx, "restore") != "abc" || rp.meta(ctx, "restore_known") != "1" {
		t.Fatal("the upgraded copy did not take the generation")
	}
}
