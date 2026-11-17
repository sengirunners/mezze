package team

import (
	"context"
	"encoding/json"
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

func snapshotBoundaryService() *Service {
	return &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

func snapshotBoundaryMail(t *testing.T, rp *replica, id string) (outEntry, api.WriteResult) {
	t.Helper()
	body := []byte(`{"to":"human:other","body":"` + id + `"}`)
	e, err := rp.enqueueID(context.Background(), id, "mail", http.MethodPost, "/v0/mail", body, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(orchapi.Mail{ID: id, From: "human:bea", To: "human:other", Body: id})
	return e, api.WriteResult{ID: id, Status: http.StatusOK, Body: m}
}

func TestSnapshotBoundaryRetiresPriorAckedKeepsFreshAndPending(t *testing.T) {
	ctx := context.Background()
	rp, s := testReplica(t), snapshotBoundaryService()
	it := orchapi.WorkItem{ID: "local:kept", Area: "solver", Status: orchapi.WorkInReview}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	// This pending row has a LOWER sequence than the old acknowledgments.
	// It will settle freshly during the snapshot and must remain visible.
	olderPending, pendingResult := snapshotBoundaryMail(t, rp, "older-pending")
	oldMail, oldResult := snapshotBoundaryMail(t, rp, "lost-old-mail")
	s.settle(ctx, rp, oldMail, oldResult, rp.copyFence())
	oldStatus, err := rp.enqueueID(ctx, "lost-old-status", "status", http.MethodPost, "/v0/work/"+it.ID+"/status", []byte(`{"status":"done"}`), "", it.ID, "03")
	if err != nil {
		t.Fatal(err)
	}
	done := it
	done.Status = orchapi.WorkDone
	raw, _ := json.Marshal(done)
	statusResult := api.WriteResult{ID: oldStatus.id, Status: 200, Body: raw, Seen: "09", StatusSeen: "09"}
	s.settle(ctx, rp, oldStatus, statusResult, rp.copyFence())
	attempt, err := rp.beginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Replaying a pending write after the request began is a fresh outcome,
	// even though its queue sequence precedes all captured acknowledgments.
	s.settle(ctx, rp, olderPending, pendingResult, rp.copyFence())
	fresh, freshResult := snapshotBoundaryMail(t, rp, "fresh-during-stream")
	s.settle(ctx, rp, fresh, freshResult, rp.copyFence())
	stillPending, _ := snapshotBoundaryMail(t, rp, "still-pending")
	if err := rp.applySnapshotAttempt(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 1, &attempt); err != nil {
		t.Fatal(err)
	}
	for _, old := range []outEntry{oldMail, oldStatus} {
		var state, result string
		var fenced bool
		if err := rp.db.QueryRow(`SELECT state,result,copy_fenced FROM outbox WHERE id=?`, old.id).Scan(&state, &result, &fenced); err != nil || state != api.OutboxAcked || !fenced || result == "" {
			t.Fatalf("old outcome lost or eligible to reapply: %s %s %v %v", old.id, state, fenced, err)
		}
	}
	statuses, err := rp.statusOverlay(ctx, it.ID)
	if err != nil || len(statuses) != 0 {
		t.Fatalf("old accepted status reappeared over restored row: %v %v", statuses, err)
	}
	mail, err := rp.mailWhere(ctx, api.Remote{Identity: "bea"}, "human:bea", true, "1=1", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{olderPending.id: true, fresh.id: true, stillPending.id: true}
	for _, m := range mail {
		if !want[m.ID] {
			t.Fatalf("old mail reconstructed after snapshot: %+v", m)
		}
		delete(want, m.ID)
	}
	if len(want) != 0 {
		t.Fatalf("snapshot lost fresh or pending read-your-write mail: %v", want)
	}
	var state string
	if err := rp.db.QueryRow(`SELECT state FROM outbox WHERE id=?`, stillPending.id).Scan(&state); err != nil || state != api.OutboxPending {
		t.Fatalf("snapshot changed pending outcome: %s %v", state, err)
	}
}

func TestSnapshotBoundaryFencesOldReplyBeforeInstallAndOnFailure(t *testing.T) {
	for _, install := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-request", true: "installed"}[install], func(t *testing.T) {
			ctx := context.Background()
			rp, s := testReplica(t), snapshotBoundaryService()
			rp.waiters = map[string][]chan api.WriteResult{}
			old, result := snapshotBoundaryMail(t, rp, "held-old-response")
			oldFence := rp.copyFence()
			waiter := make(chan api.WriteResult, 1)
			rp.waiters[old.id] = []chan api.WriteResult{waiter}
			attempt, err := rp.beginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// The old authority reply arrives BEFORE snapshot installation.
			s.settle(ctx, rp, old, result, oldFence)
			select {
			case got := <-waiter:
				if string(got.Body) != string(result.Body) || got.Status != result.Status {
					t.Fatal("fence changed the authority's durable outcome")
				}
			default:
				t.Fatal("fence lost the waiting caller's result")
			}
			fresh, freshResult := snapshotBoundaryMail(t, rp, "fresh-response")
			s.settle(ctx, rp, fresh, freshResult, rp.copyFence())
			if install {
				if err := rp.applySnapshotAttempt(ctx, nil, "03", time.Now(), 1, &attempt); err != nil {
					t.Fatal(err)
				}
			}
			es, err := rp.overlays(ctx, "")
			if err != nil || len(es) != 1 || es[0].id != fresh.id {
				t.Fatalf("old reply resurrected or fresh write suppressed: %+v %v", es, err)
			}
			var fenced bool
			if err := rp.db.QueryRow(`SELECT copy_fenced FROM outbox WHERE id=?`, old.id).Scan(&fenced); err != nil || !fenced {
				t.Fatalf("old pre-request response not durably fenced: %v %v", fenced, err)
			}
			if len(rp.waiters[old.id]) != 0 {
				t.Fatal("settled waiter retained")
			}
		})
	}
}

func TestSnapshotBoundaryFailedInstallDoesNotFencePriorAck(t *testing.T) {
	ctx := context.Background()
	rp, s := testReplica(t), snapshotBoundaryService()
	old, result := snapshotBoundaryMail(t, rp, "prior-ack")
	s.settle(ctx, rp, old, result, rp.copyFence())
	attempt, err := rp.beginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.db.Exec(`CREATE TRIGGER reject_snapshot BEFORE INSERT ON meta WHEN NEW.key = 'cursor' BEGIN SELECT RAISE(ABORT, 'snapshot install fault'); END`); err != nil {
		t.Fatal(err)
	}
	err = rp.applySnapshotAttempt(ctx, nil, "03", time.Now(), 1, &attempt)
	if err == nil || !strings.Contains(err.Error(), "snapshot install fault") {
		t.Fatalf("snapshot did not reach the injected commit failure: %v", err)
	}
	var fenced bool
	if err := rp.db.QueryRow(`SELECT copy_fenced FROM outbox WHERE id=?`, old.id).Scan(&fenced); err != nil || fenced {
		t.Fatalf("failed snapshot retired a current acknowledged overlay: %v %v", fenced, err)
	}
	if rp.copyFence().epoch != attempt.epoch {
		t.Fatal("failed snapshot advanced or rolled back the attempt epoch")
	}
}

func TestSnapshotBoundaryKeepsFreshStatusAcceptedDuringStream(t *testing.T) {
	for _, queuedBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-write", true: "older-pending-replayed-fresh"}[queuedBefore], func(t *testing.T) {
			ctx := context.Background()
			rp, s := testReplica(t), snapshotBoundaryService()
			it := orchapi.WorkItem{ID: "local:visible", Area: "solver", Status: orchapi.WorkInReview}
			if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 0); err != nil {
				t.Fatal(err)
			}
			enqueue := func() outEntry {
				e, err := rp.enqueueID(ctx, "fresh-status", "status", http.MethodPost, "/v0/work/"+it.ID+"/status", []byte(`{"status":"done"}`), "", it.ID, "03")
				if err != nil {
					t.Fatal(err)
				}
				return e
			}
			var fresh outEntry
			if queuedBefore {
				fresh = enqueue()
			}
			old, oldResult := snapshotBoundaryMail(t, rp, "prior-ack")
			s.settle(ctx, rp, old, oldResult, rp.copyFence())
			attempt, err := rp.beginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !queuedBefore {
				fresh = enqueue()
			}
			done := it
			done.Status = orchapi.WorkDone
			raw, _ := json.Marshal(done)
			s.settle(ctx, rp, fresh, api.WriteResult{ID: fresh.id, Status: 200, Body: raw, Seen: "11", StatusSeen: "11"}, rp.copyFence())
			if err := rp.applySnapshotAttempt(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 1, &attempt); err != nil {
				t.Fatal(err)
			}
			st, err := rp.statusOverlay(ctx, it.ID)
			if err != nil || st[it.ID] != orchapi.WorkDone {
				t.Fatalf("snapshot lost fresh status read-your-write: %v %v", st, err)
			}
			var fenced bool
			var state string
			if err := rp.db.QueryRow(`SELECT state,copy_fenced FROM outbox WHERE id=?`, fresh.id).Scan(&state, &fenced); err != nil || fenced || state != api.OutboxAcked {
				t.Fatalf("fresh status outcome changed across snapshot: %s %v %v", state, fenced, err)
			}
		})
	}
}
