package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

type writerFenceSessions struct {
	orchcore.SessionService
	state func(context.Context, string, orchapi.StateRequest) error
}

func (s writerFenceSessions) SetState(ctx context.Context, addr string, req orchapi.StateRequest) error {
	return s.state(ctx, addr, req)
}

// Session metadata has the same fence as the SQL copy. TryLock makes the
// old check/use race deterministic: a snapshot must still be excluded when
// the callback publishes a note, while the replica writer must be available
// to callbacks. No scheduler delay or eventual repair can hide the gap.
func TestWriterFenceSettlementSessionMetadata(t *testing.T) {
	for _, kind := range []string{"mail", "reply", "claim", "lease", "status"} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stale=%v", kind, stale), func(t *testing.T) {
				ctx := context.Background()
				rp := testReplica(t)
				const actor, item = "bea-mac/1234abcd", "local:target"
				if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, orchapi.WorkItem{ID: item, Area: "solver"})}, "03", time.Now(), 0); err != nil {
					t.Fatal(err)
				}
				path, status := "/v0/work/"+item+"/"+kind, http.StatusConflict
				result := json.RawMessage(`{"error":{"code":"conflict","message":"another holder won"}}`)
				if kind == "mail" || kind == "reply" {
					path, status = "/v0/mail", http.StatusOK
					result, _ = json.Marshal(orchapi.Mail{ID: "w", Project: "p", Area: "solver", From: actor, To: "human:other", Needs: orchapi.NeedsHuman,
						Audience: orchapi.MailAudienceParticipants, Participants: []string{actor, "human:other"}})
				}
				e, err := rp.enqueueID(ctx, "w", kind, http.MethodPost, path, []byte(`{}`), actor, item, "03", outVisibility{Area: "solver"})
				if err != nil {
					t.Fatal(err)
				}
				fence := rp.copyFence()
				if stale {
					if err := rp.applySnapshot(ctx, nil, "01", time.Now(), 1); err != nil {
						t.Fatal(err)
					}
				}
				calls, callbackErr := 0, error(nil)
				s := &Service{d: withSessions(&core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, writerFenceSessions{
					state: func(ctx context.Context, addr string, req orchapi.StateRequest) error {
						calls++
						if rp.snapshotMu.TryLock() {
							rp.snapshotMu.Unlock()
							callbackErr = fmt.Errorf("snapshot can commit before old metadata effect")
						}
						if !rp.writing.TryLock() {
							callbackErr = fmt.Errorf("session callback retains the replica writer")
							return callbackErr
						}
						rp.writing.Unlock()
						if addr != actor || req.State != orchapi.StateNeedsHuman {
							callbackErr = fmt.Errorf("unexpected session metadata: %s %+v", addr, req)
						}
						// A session callback may read the copy or commit unrelated
						// local bookkeeping: the writer transaction is already over.
						if err := rp.exec(ctx, `INSERT INTO meta(key,value) VALUES ('session_callback','done')`); err != nil {
							callbackErr = err
						}
						return callbackErr
					},
				})}
				s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: status, Body: result}, fence)
				want := 1
				if stale {
					want = 0
				}
				if calls != want || callbackErr != nil {
					t.Fatalf("session side effects: calls%d want%d err%v", calls, want, callbackErr)
				}
				if !rp.snapshotMu.TryLock() {
					t.Fatal("settlement retained its snapshot fence after returning")
				}
				defer rp.snapshotMu.Unlock()
				var state string
				var fenced bool
				if err := rp.db.QueryRow(`SELECT state,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &fenced); err != nil ||
					fenced != stale || state == api.OutboxPending {
					t.Fatalf("session fencing lost durable settlement: state%q fenced%v %v", state, fenced, err)
				}
			})
		}
	}
}

// A sync event after the request can supersede a lease answer without a
// snapshot. SQL already keeps the newer row; the presence cache must agree.
func TestWriterFenceSettlementKeepsNewerLeaseCache(t *testing.T) {
	for _, kind := range []string{"claim", "lease", "release", "status"} {
		for _, seen := range []string{"04", ""} {
			t.Run(fmt.Sprintf("%s/seen=%q", kind, seen), func(t *testing.T) {
				ctx := context.Background()
				rp := testReplica(t)
				rp.waiters = map[string][]chan api.WriteResult{}
				const actor, other, item = "bea-mac/1234abcd", "bea-mac/abcdef12", "local:target"
				it := orchapi.WorkItem{ID: item, Area: "solver", Status: orchapi.WorkInReview}
				if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 0); err != nil {
					t.Fatal(err)
				}
				path := "/v0/work/" + item + "/" + kind
				if kind == "lease" {
					path = "/v0/leases"
				}
				e, err := rp.enqueueID(ctx, "w", kind, http.MethodPost, path, []byte(`{}`), actor, item, "03")
				if err != nil {
					t.Fatal(err)
				}
				waiter := rp.waitFor(e.id)
				fence := rp.copyFence()
				s := &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
				currentHolder := actor
				var result any = orchapi.ReleaseResponse{Key: item, Released: true}
				switch kind {
				case "claim", "lease":
					currentHolder = other
					result = orchapi.ClaimResponse{Lease: orchapi.Lease{Key: item, Holder: actor, Project: "p", Area: "solver", Bound: true}}
				case "status":
					old := it
					old.Status = orchapi.WorkDone
					result = old
				}
				lease, _ := json.Marshal(orchapi.Lease{Key: item, Holder: currentHolder, Project: "p", Area: "solver", Bound: true})
				if _, err := rp.applyChanges(ctx, []api.Event{{ID: "05", Type: orchapi.EventWorkClaimed, Data: lease}}, "05", time.Now()); err != nil {
					t.Fatal(err)
				}
				s.cache.claimedIn("p", currentHolder, item)
				raw, _ := json.Marshal(result)
				res := api.WriteResult{ID: e.id, Status: http.StatusOK, Body: raw, Seen: seen, Replayed: true}
				s.settle(ctx, rp, e, res, fence)
				if s.cache.holds(actor, item) != (currentHolder == actor) || !s.cache.holds(currentHolder, item) {
					t.Fatal("old answer changed the held-key cache after a newer lease event")
				}
				leases, err := rp.liveLeases(ctx)
				if err != nil || leases[item].Holder != currentHolder {
					t.Fatalf("old answer replaced the current SQL lease: %+v %v", leases, err)
				}
				if got, _, ok := rp.findItem(ctx, item); !ok || got.Status != it.Status {
					t.Fatal("old status reply overwrote the current item")
				}
				var state, stored, storedSeen string
				var fenced bool
				if err := rp.db.QueryRow(`SELECT state,result,seen,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &stored, &storedSeen, &fenced); err != nil ||
					state != api.OutboxAcked || stored != string(raw) || storedSeen != seen || fenced != (seen == "") {
					t.Fatalf("ordering fence changed the durable exact outcome: %q %q seen%q fenced%v %v", state, stored, storedSeen, fenced, err)
				}
				select {
				case got := <-waiter:
					if got.ID != res.ID || got.Status != res.Status || got.Seen != res.Seen || string(got.Body) != string(res.Body) || got.Replayed != res.Replayed {
						t.Fatal("waiter lost the exact authority outcome")
					}
				default:
					t.Fatal("fenced accepted result left its waiter pending")
				}
				if rp.answer(res, nil) {
					t.Fatal("waiter was still registered after delivery")
				}
				if seen == "" {
					if overlays, err := rp.overlays(ctx, ""); err != nil || len(overlays) != 0 {
						t.Fatalf("unordered reply still overlaid the current copy: %+v %v", overlays, err)
					}
				}
			})
		}
	}
}

func TestWriterFenceGenericLeaseKeepsSameEpochMove(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const actor, item = "bea-mac/1234abcd", "local:target"
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, orchapi.WorkItem{ID: item})}, "03", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	e, err := rp.enqueueID(ctx, "w", "lease", http.MethodPost, "/v0/leases", []byte(`{}`), actor, item, "03")
	if err != nil {
		t.Fatal(err)
	}
	fence := rp.copyFence()
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "05", Type: orchapi.EventWorkMoved, Subject: item}}, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	raw, _ := json.Marshal(orchapi.ClaimResponse{Lease: orchapi.Lease{Key: item, Holder: actor, Project: "p", Bound: true}})
	s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: http.StatusOK, Body: raw, Seen: "04"}, fence)
	leases, err := rp.liveLeases(ctx)
	if err != nil || len(leases) != 0 || s.cache.holds(actor, item) {
		t.Fatalf("generic lease result undid a work move: %+v %v", leases, err)
	}
	var state string
	var fenced bool
	if err := rp.db.QueryRow(`SELECT state,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &fenced); err != nil || state != api.OutboxAcked || !fenced {
		t.Fatalf("generic lease fence lost its outcome: state%q fenced%v %v", state, fenced, err)
	}
}

// An absent Seen is not by itself stale. Preserve immediate application
// when the target has not advanced, including arbitrary lock keys that
// never have an items row and unrelated sync progress while they run.
func TestWriterFenceFreshSettlementWithoutSeen(t *testing.T) {
	for _, tc := range []struct{ kind, key string }{
		{"claim", "local:target"}, {"lease", "local:target"},
		{"release", "local:target"}, {"status", "local:target"},
		{"lease", "lock:build"}, {"release", "lock:build"},
	} {
		t.Run(tc.kind+"/"+tc.key, func(t *testing.T) {
			ctx := context.Background()
			rp := testReplica(t)
			const actor = "bea-mac/1234abcd"
			it := orchapi.WorkItem{ID: "local:target", Area: "solver", Status: orchapi.WorkInReview}
			if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 0); err != nil {
				t.Fatal(err)
			}
			path := "/v0/work/" + tc.key + "/" + tc.kind
			method := http.MethodPost
			if tc.kind == "lease" {
				path = "/v0/leases"
			} else if tc.key == "lock:build" {
				path, method = "/v0/leases/"+tc.key, http.MethodDelete
			}
			e, err := rp.enqueueID(ctx, "w", tc.kind, method, path, []byte(`{}`), actor, tc.key, "03")
			if err != nil {
				t.Fatal(err)
			}
			fence := rp.copyFence()
			if tc.key == "lock:build" {
				// Global cursor progress is not evidence about an arbitrary key.
				if _, err := rp.applyChanges(ctx, nil, "05", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			s := &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			lease := orchapi.Lease{Key: tc.key, Holder: actor, Project: "p", Bound: true}
			var result any = orchapi.ClaimResponse{Lease: lease}
			wantHeld := true
			switch tc.kind {
			case "release":
				if err := rp.writeApply(ctx, func(tx *sql.Tx) error { return putLease(ctx, tx, lease) }); err != nil {
					t.Fatal(err)
				}
				s.cache.claimedIn("p", actor, tc.key)
				result, wantHeld = orchapi.ReleaseResponse{Key: tc.key, Released: true}, false
			case "status":
				s.cache.claimedIn("p", actor, tc.key)
				it.Status = orchapi.WorkDone
				result, wantHeld = it, false
			}
			raw, _ := json.Marshal(result)
			s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: http.StatusOK, Body: raw}, fence)
			if s.cache.holds(actor, tc.key) != wantHeld {
				t.Fatal("fresh reply without Seen did not update held-key state")
			}
			if tc.kind == "status" {
				if got, _, ok := rp.findItem(ctx, tc.key); !ok || got.Status != orchapi.WorkDone {
					t.Fatal("fresh status reply without Seen did not update its item")
				}
			} else if leases, err := rp.liveLeases(ctx); err != nil || (leases[tc.key].Holder == actor) != wantHeld {
				t.Fatalf("fresh reply without Seen did not update its SQL lease: %+v %v", leases, err)
			}
			var state string
			var fenced bool
			if err := rp.db.QueryRow(`SELECT state,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &fenced); err != nil || state != api.OutboxAcked || fenced {
				t.Fatalf("fresh unordered reply was fenced: state%q fenced%v %v", state, fenced, err)
			}
		})
	}
}

// A reconnect can sync the other holder's claim after drain captures its
// fence, before that drain settles the refused queued claim. Refusals have
// no Seen by design; this valid conflict must remain visible to its maker.
func TestWriterFenceCurrentConflictSurvivesNewerItemWithoutSeen(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const actor, other, item = "bea-mac/1234abcd", "mini/abcdef12", "local:contested"
	it := orchapi.WorkItem{ID: item, Area: "solver", Status: orchapi.WorkOpen}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	e, err := rp.enqueueID(ctx, "contested", "claim", http.MethodPost, "/v0/work/"+item+"/claim", []byte(`{}`), actor, item, "03", outVisibility{Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	fence := rp.copyFence()
	lease, _ := json.Marshal(orchapi.Lease{Key: item, Holder: other, Project: "p", Area: "solver", Bound: true})
	if _, err := rp.applyChanges(ctx, []api.Event{{ID: "05", Type: orchapi.EventWorkClaimed, Data: lease}}, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	states := []orchapi.StateRequest{}
	var stateActor string
	s := &Service{d: withSessions(&core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, writerFenceSessions{
		state: func(_ context.Context, addr string, req orchapi.StateRequest) error {
			stateActor = addr
			states = append(states, req)
			return nil
		},
	})}
	raw := json.RawMessage(`{"error":{"code":"conflict","message":"another holder won"}}`)
	s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: http.StatusConflict, Body: raw}, fence)
	var state, stored string
	var fenced bool
	if err := rp.db.QueryRow(`SELECT state,result,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &stored, &fenced); err != nil ||
		state != api.OutboxConflict || stored != string(raw) || fenced {
		t.Fatalf("current conflict was lost or hidden: state%q result%q fenced%v %v", state, stored, fenced, err)
	}
	for _, all := range []bool{false, true} {
		entries, err := rp.entries(ctx, api.ProjectRef{ID: "p"}, all, authz.All(), actor, true)
		if err != nil || len(entries) != 1 || entries[0].ID != e.id || entries[0].State != api.OutboxConflict || entries[0].Item != item {
			t.Fatalf("current conflict disappeared from public Outbox(all=%v): %+v %v", all, entries, err)
		}
	}
	if stateActor != actor || len(states) != 1 || states[0].State != orchapi.StateNeedsHuman || !strings.Contains(states[0].Note, "claim conflict: "+item) {
		t.Fatalf("current conflict did not update its maker's session: %q %+v", stateActor, states)
	}
	leases, err := rp.liveLeases(ctx)
	if err != nil || leases[item].Holder != other || s.cache.holds(actor, item) {
		t.Fatalf("refused claim changed the real holder: %+v %v", leases, err)
	}
}
