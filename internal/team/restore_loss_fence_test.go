package team

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// A restore can remember an item lost while a failed comment/link reply is
// in flight. The refusal settles visibly and tells its session (or its waiting
// caller) whether it was sent before or after the restore snapshot: the copy
// already records the loss, so this is current news. It never
// resurrects the item.
func TestWriterFenceRestoreLossSessionMetadata(t *testing.T) {
	for _, kind := range []string{"comment", "link"} {
		for _, stale := range []bool{false, true} {
			for _, waiting := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stale=%v/waiting=%v", kind, stale, waiting), func(t *testing.T) {
					ctx := context.Background()
					rp := testReplica(t)
					rp.waiters = map[string][]chan api.WriteResult{}
					const actor, item = "bea-mac/1234abcd", "local:lost"
					if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, orchapi.WorkItem{ID: item, Area: "secret"})}, "03", time.Now(), 0,
						snapshotAnswer{restore: "before"}); err != nil {
						t.Fatal(err)
					}
					restore := func() {
						t.Helper()
						if err := rp.applySnapshot(ctx, nil, "01", time.Now(), 1, snapshotAnswer{restore: "restored"}); err != nil {
							t.Fatal(err)
						}
						if !rp.lostToRestore(item) {
							t.Fatal("fixture did not record the item lost by restore")
						}
					}
					if !stale {
						restore() // a genuinely current refusal must still notify its session
					}
					e, err := rp.enqueueID(ctx, "restore-loss", kind, http.MethodPost, "/v0/work/"+item+"/"+kind,
						[]byte(`{}`), actor, item, "03", outVisibility{Area: "secret"})
					if err != nil {
						t.Fatal(err)
					}
					fence := rp.copyFence()
					var answer chan api.WriteResult
					if waiting {
						answer = rp.waitFor(e.id)
					}
					if stale {
						restore() // commits the replacement copy before the old refusal arrives
					}
					calls := 0
					s := &Service{d: withSessions(&core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, writerFenceSessions{
						state: func(_ context.Context, addr string, req orchapi.StateRequest) error {
							calls++
							if addr != actor || req.State != orchapi.StateNeedsHuman || !strings.Contains(req.Note, "restored and lost "+item) {
								t.Errorf("unexpected restore-loss note: %s %+v", addr, req)
							}
							return nil
						},
					})}
					result := api.WriteResult{ID: e.id, Status: http.StatusNotFound,
						Body: []byte(`{"error":{"code":"not_found","message":"item gone"}}`), Seen: "01", Replayed: true}
					s.settle(ctx, rp, e, result, fence)
					wantCalls := 1
					if waiting {
						wantCalls = 0 // a waiting caller receives the refusal directly, never twice
					}
					if calls != wantCalls {
						t.Fatalf("session side effects: calls%d want%d", calls, wantCalls)
					}
					if waiting {
						select {
						case got := <-answer:
							if got.ID != result.ID || got.Status != result.Status || string(got.Body) != string(result.Body) ||
								got.Seen != result.Seen || got.Replayed != result.Replayed || got.StatusSeen != result.StatusSeen {
								t.Fatalf("original caller outcome changed: got%+v want%+v", got, result)
							}
						default:
							t.Fatal("waiting caller did not receive its original refusal")
						}
						var n int
						if err := rp.db.QueryRow(`SELECT count(*) FROM outbox WHERE id=?`, e.id).Scan(&n); err != nil || n != 0 {
							t.Fatalf("answered refusal cleanup changed: rows%d err%v", n, err)
						}
					} else {
						var state, body, message string
						var fenced bool
						if err := rp.db.QueryRow(`SELECT state,result,error,copy_fenced FROM outbox WHERE id=?`, e.id).
							Scan(&state, &body, &message, &fenced); err != nil {
							t.Fatal(err)
						}
						if state != api.OutboxFailed || body != string(result.Body) || fenced || !strings.Contains(message, "restored from a backup") {
							t.Fatalf("durable refusal changed: state%q body%q fenced%v message%q", state, body, fenced, message)
						}
					}
					if _, _, ok := rp.findItem(ctx, item); ok {
						t.Fatal("failed reply resurrected the restored-away item")
					}
				})
			}
		}
	}
}
