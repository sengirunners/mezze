package sqlite

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Force the generator to draw an old redirect, then a free handle.
func TestMintSkipsMovedID(t *testing.T) {
	ctx := context.Background()
	src, dst := twoProjects(t)
	it := create(t, src, orchapi.NewIssueRequest{ID: "oc-abc", Title: "move"})
	moveOne(t, src, dst, it.ID, "sg-abc", nil)
	p := src.(*project)
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	w := &writer{ctx: ctx, tx: tx, p: p}
	draws := 0
	got, err := w.hashIDWith("oc", func(int) string {
		draws++
		if draws == 1 {
			return "abc"
		}
		return "def"
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "oc-def" || draws != 2 {
		t.Fatalf("minted %q after %d draws; moved id was reused", got, draws)
	}
}
