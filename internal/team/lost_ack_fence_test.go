package team

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// A restore snapshot tells each session whose acked write it lost. The
// first note's callback may change what the copy is (a newer snapshot, a
// rescope, a scope change waiting for a snapshot, a leave): later notes from
// the old snapshot are then not sent, while every durable failed outcome
// stays. A fresh control tells both sessions.
func TestWriterFenceLostAckNotes(t *testing.T) {
	const pid = "01PROJECT"
	for _, tc := range []struct {
		name       string
		invalidate func(t *testing.T, s *Service, x *remote, rp *replica)
	}{
		{"fresh", nil},
		{"newer-snapshot", func(t *testing.T, _ *Service, _ *remote, rp *replica) {
			if _, err := rp.beginSnapshot(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
		{"rescope", func(_ *testing.T, _ *Service, x *remote, _ *replica) {
			x.Mu.Lock()
			x.R.ScopeVersion++
			x.Mu.Unlock()
		}},
		{"scope-change-seen", func(_ *testing.T, _ *Service, _ *remote, rp *replica) {
			rp.mu.Lock()
			rp.resnap++
			rp.mu.Unlock()
		}},
		{"leave", func(_ *testing.T, s *Service, _ *remote, _ *replica) {
			s.Forget(pid)
		}},
		{"stopped-copy", func(_ *testing.T, _ *Service, x *remote, _ *replica) {
			x.Mu.Lock()
			x.Copy = nil
			x.Mu.Unlock()
		}},
		{"revoked", func(_ *testing.T, _ *Service, x *remote, _ *replica) {
			x.Mu.Lock()
			x.R.State = api.RemoteRevoked
			x.Mu.Unlock()
		}},
		{"left", func(_ *testing.T, _ *Service, x *remote, _ *replica) {
			x.Mu.Lock()
			x.Left = true
			x.Mu.Unlock()
		}},
		{"role-generation", func(_ *testing.T, _ *Service, x *remote, _ *replica) {
			x.Mu.Lock()
			x.Recovery.Generation++
			x.Mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rp := testReplica(t)
			item := orchapi.WorkItem{ID: "local:kept", Title: "kept", Status: orchapi.WorkOpen}
			if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, item)}, "01", time.Now(), 0, snapshotAnswer{restore: "before"}); err != nil {
				t.Fatal(err)
			}
			for _, e := range []struct{ id, as string }{{"01LOSTA", "bea-mac/aaaaaaaa"}, {"01LOSTB", "bea-mac/bbbbbbbb"}} {
				if _, err := rp.db.Exec(`INSERT INTO outbox (id, kind, method, path, body, as_addr, item, seen, state, created) VALUES (?, 'status', 'POST', '/x', '{}', ?, ?, '99', ?, 0)`,
					e.id, e.as, item.ID, api.OutboxAcked); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := rp.beginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := rp.applySnapshotAttempt(ctx, []api.SnapshotLine{snapLine(t, item)}, "02", time.Now(), 0, &attempt, snapshotAnswer{restore: "restored"}); err != nil {
				t.Fatal(err)
			}
			if attempt.committed == 0 || len(rp.lostAcks) != 2 {
				t.Fatalf("fixture: committed %d lost acks %+v", attempt.committed, rp.lostAcks)
			}
			snap := api.Remote{Project: api.ProjectRef{ID: pid}, ScopeVersion: 3, State: api.RemoteOK, Areas: []string{"solver"}}
			x := &remote{R: snap, Copy: rp}
			var told []string
			s := &Service{Service: linked(x)}
			s.d = withSessions(&core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, writerFenceSessions{
				state: func(_ context.Context, addr string, req orchapi.StateRequest) error {
					if req.State != orchapi.StateNeedsHuman || !strings.Contains(req.Note, "restored from an older backup") {
						t.Errorf("unexpected note: %s %+v", addr, req)
					}
					told = append(told, addr)
					if len(told) == 1 && tc.invalidate != nil {
						tc.invalidate(t, s, x, rp)
					}
					return nil
				},
			})
			s.tellLostAcks(ctx, rp, x.RoleReply(), attempt.committed, 0, rp.lostAcks, nil)
			want := 2
			if tc.invalidate != nil {
				want = 1
			}
			if len(told) != want {
				t.Fatalf("sessions told %v, want %d", told, want)
			}
			for _, id := range []string{"01LOSTA", "01LOSTB"} {
				var st, msg string
				if err := rp.db.QueryRow(`SELECT state, error FROM outbox WHERE id = ?`, id).Scan(&st, &msg); err != nil {
					t.Fatal(err)
				}
				if st != api.OutboxFailed || !strings.Contains(msg, "restored from a backup") {
					t.Fatalf("durable outcome of %s changed: %q %q", id, st, msg)
				}
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rp := testReplica(t)
		x := &remote{Copy: rp}
		s := &Service{Service: linked(x)}
		if s.lostAckCurrent(ctx, x, rp, x.Snapshot(), 1, 0) {
			t.Fatal("a cancelled snapshot admitted a note")
		}
	})
}
