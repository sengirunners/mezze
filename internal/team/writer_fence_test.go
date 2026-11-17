package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// HoldWriterReply exposes a per-copy barrier after remote I/O but before
// applying create, refresh404 or settle. The returned callback runs afterward.
func (s *Service) HoldWriterReply(slug string, hook func(string, string) func()) func() {
	rp := rep(s.ByName(slug))
	rp.mu.Lock()
	rp.testCopyHook = hook
	rp.mu.Unlock()
	return func() { rp.mu.Lock(); rp.testCopyHook = nil; rp.mu.Unlock() }
}

func (s *Service) WriterCopyEpoch(slug string) uint64 { return rep(s.ByName(slug)).copyFence().epoch }

func TestWriterFence404AndCreate(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	it := orchapi.WorkItem{ID: "local:visible", Title: "fresh", Area: "solver"}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "09", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	before := rp.copyFence()
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, it)}, "03", time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if err := rp.dropItemAt(ctx, it.ID, before); !errors.Is(err, errStaleCopy) {
		t.Fatalf("old404 accepted across backwards snapshot cursor: %v", err)
	}
	if _, _, ok := rp.findItem(ctx, it.ID); !ok {
		t.Fatal("old404 deleted the freshly visible row")
	}
	secret := orchapi.WorkItem{ID: "local:hidden", Title: "old-scope secret", Area: "billing"}
	if err := rp.putCreated(ctx, secret, before); !errors.Is(err, errStaleCopy) {
		t.Fatalf("old create accepted across snapshot: %v", err)
	}
	if _, _, ok := rp.findItem(ctx, secret.ID); ok {
		t.Fatal("old create resurrected hidden work")
	}
	current := rp.copyFence()
	newer := it
	newer.Title = "newer sync row"
	if err := rp.writeApply(ctx, func(tx *sql.Tx) error { return putItem(ctx, tx, newer, "11", true) }); err != nil {
		t.Fatal(err)
	}
	if err := rp.dropItemAt(ctx, it.ID, current); !errors.Is(err, errStaleCopy) {
		t.Fatalf("old404 deleted a newer same-epoch upsert: %v", err)
	}
	if err := rp.putCreated(ctx, orchapi.WorkItem{ID: "local:new", Area: "solver"}, current); err != nil {
		t.Fatalf("fresh create: %v", err)
	}
	if err := rp.dropItemAt(ctx, "local:new", current); err != nil {
		t.Fatalf("fresh404: %v", err)
	}
	if _, _, ok := rp.findItem(ctx, "local:new"); ok {
		t.Fatal("fresh404 failed to remove missing work")
	}
}

func TestWriterFenceSettlementKeepsOutcomeWithoutCopyOrDisclosure(t *testing.T) {
	for _, kind := range []string{"status", "link", "comment", "claim", "mail", "reply"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			rp := testReplica(t)
			rp.waiters = map[string][]chan api.WriteResult{}
			const actor, item, secret = "bea-mac/1234abcd", "local:old-scope", "invented-old-scope-secret"
			old := orchapi.WorkItem{ID: item, Title: secret, Area: "billing", Status: orchapi.WorkInReview}
			if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, old)}, "09", time.Now(), 0); err != nil {
				t.Fatal(err)
			}
			path := "/v0/work/" + item + "/" + kind
			body := json.RawMessage(`{"body":"` + secret + `","status":"in_review"}`)
			var result any = old
			switch kind {
			case "comment":
				result = orchapi.Comment{ID: "w", Item: item, Body: secret, Author: actor}
			case "claim":
				result = orchapi.ClaimResponse{Lease: orchapi.Lease{Key: item, Holder: actor, Project: "p", Area: "billing"}}
			case "mail", "reply":
				path = "/v0/mail"
				body = json.RawMessage(`{"to":"human:other","body":"` + secret + `","audience":"participants"}`)
				result = orchapi.Mail{ID: "w", Project: "p", Area: "billing", From: actor, To: "human:other", Body: secret,
					Audience: orchapi.MailAudienceParticipants, Participants: []string{actor, "human:other"}}
			}
			e, err := rp.enqueueID(ctx, "w", kind, http.MethodPost, path, body, actor, item, "09",
				outVisibility{Area: "billing", Readers: []string{actor}})
			if err != nil {
				t.Fatal(err)
			}
			waiter := rp.waitFor(e.id)
			fence := rp.copyFence()
			if err := rp.applySnapshot(ctx, nil, "03", time.Now(), 1); err != nil {
				t.Fatal(err)
			}
			later, err := rp.enqueueID(ctx, "later", "status", http.MethodPost, "/v0/work/local:safe/status",
				[]byte(`{"status":"done"}`), actor, "local:safe", "03", outVisibility{Area: "solver"})
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{d: &core.Deps{Config: &config.Config{Machine: "bea-mac"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			raw, _ := json.Marshal(result)
			res := api.WriteResult{ID: e.id, Status: http.StatusOK, Body: raw, Seen: "10", StatusSeen: "10"}
			s.settle(ctx, rp, e, res, fence)
			// Assert immediately, with no sync/refresh goroutine available to
			// repair a transient resurrection before the assertions observe it.
			for _, table := range []string{"items", "comments", "leases", "mail"} {
				var count int
				if err := rp.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("late %s contaminated %s: count%d, %v", kind, table, count, err)
				}
			}
			var state, stored string
			var fenced int
			if err := rp.db.QueryRow(`SELECT state,result,copy_fenced FROM outbox WHERE id=?`, e.id).Scan(&state, &stored, &fenced); err != nil ||
				state != api.OutboxAcked || stored != string(raw) || fenced != 1 {
				t.Fatalf("durable outcome was lost: %s %s %d %v", state, stored, fenced, err)
			}
			select {
			case got := <-waiter:
				if got.ID != res.ID || got.Status != res.Status || string(got.Body) != string(res.Body) {
					t.Fatal("waiter did not receive the actual authority outcome")
				}
			default:
				t.Fatal("old-scope result left its waiter pending")
			}
			if rp.answer(res, nil) {
				t.Fatal("waiter remained registered after its answer")
			}
			overlay, err := rp.overlays(ctx, "")
			if err != nil || len(overlay) != 1 || overlay[0].id != later.id {
				t.Fatalf("old answer overlaid or unrelated pending write vanished: %v %v", overlay, err)
			}
			for _, all := range []bool{false, true} {
				entries, err := rp.entries(ctx, api.ProjectRef{ID: "p"}, all, authz.All(), actor, true)
				js, _ := json.Marshal(entries)
				if err != nil || len(entries) != 1 || entries[0].ID != later.id || strings.Contains(string(js), secret) || strings.Contains(string(js), item) {
					t.Fatalf("current Outbox disclosed old-scope outcome/metadata: %s %v", js, err)
				}
			}
			mail, err := rp.mailWhere(ctx, api.Remote{Project: api.ProjectRef{ID: "p"}}, actor, true, "1=1", nil, 50)
			if err != nil || len(mail) != 0 {
				t.Fatalf("current sent-mail surface disclosed fenced participants/body: %+v %v", mail, err)
			}
			if s.cache.holds(actor, item) {
				t.Fatal("late claim restored an old-scope held key")
			}
			var base string
			if err := rp.db.QueryRow(`SELECT base FROM outbox WHERE id=?`, later.id).Scan(&base); err != nil || base != "03" {
				t.Fatalf("unrelated pending base changed: %q %v", base, err)
			}
		})
	}
}

// A later write can target the same item under the new snapshot. An old
// reply's StatusSeen must not become evidence that this new request saw it.
func TestWriterFenceLateStatusSeenKeepsSameTargetPendingBase(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	const item = "local:target"
	old := orchapi.WorkItem{ID: item, Area: "billing", Status: orchapi.WorkOpen}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, old)}, "09", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	e, err := rp.enqueueID(ctx, "old", "status", http.MethodPost, "/v0/work/"+item+"/status", []byte(`{"status":"done"}`), "", item, "09")
	if err != nil {
		t.Fatal(err)
	}
	fence := rp.copyFence()
	if err := rp.applySnapshot(ctx, nil, "03", time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	later, err := rp.enqueueID(ctx, "later-same-target", "status", http.MethodPost, e.path, []byte(`{"status":"in_review"}`), "", item, "03")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	raw, _ := json.Marshal(old)
	s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: 200, Body: raw, Seen: "10", StatusSeen: "10"}, fence)
	var base, state string
	if err := rp.db.QueryRow(`SELECT base,state FROM outbox WHERE id=?`, later.id).Scan(&base, &state); err != nil || base != "03" || state != api.OutboxPending {
		t.Fatalf("old StatusSeen changed the new same-target write: base%q state%q %v", base, state, err)
	}
}

func TestWriterFenceFreshSettlementAndMove(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	item := orchapi.WorkItem{ID: "local:kept", Title: "kept", Status: orchapi.WorkOpen}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, item)}, "03", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	e, err := rp.enqueueID(ctx, "current", "status", http.MethodPost, "/v0/work/"+item.ID+"/status", []byte(`{"status":"done"}`), "", item.ID, "03")
	if err != nil {
		t.Fatal(err)
	}
	fence := rp.copyFence()
	item.Status = orchapi.WorkDone
	body, _ := json.Marshal(item)
	s := &Service{d: &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: 200, Body: body, Seen: "04", StatusSeen: "04"}, fence)
	if got, _, ok := rp.findItem(ctx, item.ID); !ok || got.Status != orchapi.WorkDone {
		t.Fatal("fresh result no longer updates the copy")
	}
	if _, err := rp.applyChanges(ctx, []api.Event{{Type: orchapi.EventWorkMoved, Subject: item.ID, ID: "05"}}, "05", time.Now()); err != nil {
		t.Fatal(err)
	}
	late, err := rp.enqueueID(ctx, "moved", "link", http.MethodPost, "/v0/work/"+item.ID+"/link", []byte(`{"rel":"pr","target":"x"}`), "", item.ID, "03")
	if err != nil {
		t.Fatal(err)
	}
	s.settle(ctx, rp, late, api.WriteResult{ID: late.id, Status: 200, Body: body, Seen: "04"}, fence)
	if _, _, ok := rp.findItem(ctx, item.ID); ok {
		t.Fatal("same-epoch move was undone by an old result")
	}
}
