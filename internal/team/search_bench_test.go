package team

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/store"
)

// benchCopy is a replica copy shaped like scripts/bench's: 11.5k items of
// about 1 KiB, a fifth not done.
func benchCopy(b testing.TB) *replica {
	b.Helper()
	ctx := context.Background()
	db, err := copies(b).OpenReplica(ctx, filepath.Join(b.TempDir(), "copy.db"), 4)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	rng := rand.New(rand.NewPCG(1, 2))
	words := []string{"race", "timeout", "drift", "refusal", "copy", "parser", "socket", "ledger"}
	now := time.Now()
	err = store.Tx(ctx, db, func(tx *sql.Tx) error {
		for i := range 11500 {
			var body strings.Builder
			for body.Len() < 900 {
				fmt.Fprintf(&body, "Steps to reproduce the %s failure, the logs from the %s run and the plan for %s. ", words[rng.IntN(8)], words[rng.IntN(8)], words[rng.IntN(8)])
			}
			st := orchapi.WorkDone
			if rng.IntN(100) < 20 {
				st = orchapi.WorkOpen
			}
			it := orchapi.WorkItem{ID: fmt.Sprintf("issue:oc-%05d", i), Source: "issue", Area: "solver", Status: st,
				Title: fmt.Sprintf("Fix the %s %s in module %d", words[rng.IntN(8)], words[rng.IntN(8)], i%40), Body: body.String(),
				Labels: []string{"bench", "v0"}, Created: now.Add(-time.Duration(11500-i) * time.Minute), Updated: now}
			if err := putItem(ctx, tx, it, "", true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return &replica{db: db}
}

func BenchmarkCopySearch(b *testing.B) {
	rp := benchCopy(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wl, err := rp.listItems(ctx, "P", orchapi.WorkQuery{Q: fmt.Sprintf("module %d", i%40)})
		if err != nil || len(wl.Items) == 0 {
			b.Fatalf("%v %d", err, len(wl.Items))
		}
	}
}
