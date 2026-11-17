package team

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// A refresh started before a move must not restore the old row after the
// move event commits. No timing or background goroutines are involved.
func TestMovedRefreshCannotRestoreOldRow(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "bd:harbor-123", Title: "old"}
	if err := rp.putWhole(ctx, it, "01", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "02", Type: orchapi.EventWorkMoved, Subject: it.ID}}, "02", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := rp.putWhole(ctx, it, "01", 0); err == nil { // earlier answer arrives late
		t.Fatal("late refresh should be retried")
	}
	if _, _, ok := rp.findItem(ctx, it.ID); ok {
		t.Fatal("late refresh restored moved work")
	}
	// A fresh fetch at the advanced cursor may introduce a visible item.
	it.ID = "bd:harbor-visible"
	if err := rp.putWhole(ctx, it, "02", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); !ok {
		t.Fatal("fresh refresh did not restore visible work")
	}
}
