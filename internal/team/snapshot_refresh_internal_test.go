package team

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A cursor can go backwards on rescope; only a refresh started after the
// applied snapshot can add an item to that copy.
func TestSnapshotRefreshEpoch(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:visible", Title: "visible"}
	before := rp.snapshotEpoch
	if err := rp.applySnapshot(ctx, nil, "03", time.Now(), 0, snapshotAnswer{restore: ""}); err != nil {
		t.Fatal(err)
	}
	if err := rp.putWhole(ctx, it, "09", before); err == nil {
		t.Fatal("pre-snapshot refresh was accepted")
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("pre-snapshot refresh restored a row")
	}
	if err := rp.putWhole(ctx, it, "03", rp.snapshotEpoch); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); !ok {
		t.Fatal("fresh refresh did not add a visible row")
	}
}
