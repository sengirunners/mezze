package team

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// A refusal answered while the restore snapshot streams sees
// no loss yet and settles fenced (unlisted, its session not told). When that
// snapshot installs the restore, a refusal of an item it lost is current
// news, also when an earlier attempt of that snapshot failed: un-fenced,
// saying why, and told to its session once. A fenced
// refusal of a kept item, or one fenced before this snapshot began, stays
// fenced and untold; a lost ack is told once, as a lost ack.
func TestWriterFenceLostRefusalDuringRestoreSnapshot(t *testing.T) {
	ctx := context.Background()
	const pid = "01PROJECT"
	rp := testReplica(t)
	lost := orchapi.WorkItem{ID: "local:lost", Title: "lost", Status: orchapi.WorkOpen, Area: "solver"}
	kept := orchapi.WorkItem{ID: "local:kept", Title: "kept", Status: orchapi.WorkOpen, Area: "solver"}
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, lost), snapLine(t, kept)}, "03", time.Now(), 0, snapshotAnswer{restore: "before"}); err != nil {
		t.Fatal(err)
	}
	var told []orchapi.StateRequest
	var toldTo []string
	s := &Service{d: withSessions(&core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, writerFenceSessions{
		state: func(_ context.Context, addr string, req orchapi.StateRequest) error {
			told, toldTo = append(told, req), append(toldTo, addr)
			return nil
		},
	})}
	// Both requests go out (fence captured) before the restore snapshot starts.
	send := func(id, as, item string) func() {
		t.Helper()
		e, err := rp.enqueueID(ctx, id, "comment", http.MethodPost, "/v0/work/"+item+"/comment", []byte(`{}`), as, item, "03", outVisibility{Area: "solver"})
		if err != nil {
			t.Fatal(err)
		}
		fence := rp.copyFence()
		return func() {
			s.settle(ctx, rp, e, api.WriteResult{ID: e.id, Status: http.StatusNotFound,
				Body: []byte(`{"error":{"code":"not_found","message":"no work item"}}`)}, fence)
		}
	}
	// One refusal was fenced by an earlier, ordinary snapshot: not this
	// restore's window, so it stays as it was.
	answerOld := send("01REFOLD", "bea-mac/cccccccc", lost.ID)
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{snapLine(t, lost), snapLine(t, kept)}, "03", time.Now(), 0, snapshotAnswer{restore: "before"}); err != nil {
		t.Fatal(err)
	}
	answerOld()
	// An acked write on the lost item: told once, as a lost ack.
	if _, err := rp.db.Exec(`INSERT INTO outbox (id, kind, method, path, body, as_addr, item, seen, state, created) VALUES ('01ACKLOST', 'comment', 'POST', '/x', '{}', 'bea-mac/dddddddd', ?, '99', ?, 0)`,
		lost.ID, api.OutboxAcked); err != nil {
		t.Fatal(err)
	}
	answerLost := send("01REFLOST", "bea-mac/aaaaaaaa", lost.ID)
	answerKept := send("01REFKEPT", "bea-mac/bbbbbbbb", kept.ID)
	failed, err := rp.beginSnapshot(ctx) // the restore snapshot starts streaming, then fails
	if err != nil {
		t.Fatal(err)
	}
	answerLost() // answered mid-snapshot: no loss recorded yet
	answerKept()
	attempt, err := rp.beginSnapshot(ctx) // its retry installs the restore
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.applySnapshotAttempt(ctx, nil, "09", time.Now(), 0, &failed, snapshotAnswer{restore: "restored"}); !api.IsCode(err, api.CodeResync) {
		t.Fatalf("fixture: the failed attempt must be superseded: %v", err)
	}
	row := func(id string) (state, msg string, fenced bool) {
		t.Helper()
		if err := rp.db.QueryRow(`SELECT state, error, copy_fenced FROM outbox WHERE id = ?`, id).Scan(&state, &msg, &fenced); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, id := range []string{"01REFOLD", "01REFLOST", "01REFKEPT"} {
		if st, _, fenced := row(id); st != api.OutboxFailed || !fenced {
			t.Fatalf("fixture: %s answered mid-snapshot should settle fenced: %s fenced=%v", id, st, fenced)
		}
	}
	if len(told) != 0 {
		t.Fatalf("a fenced refusal told its session before the restore installed: %v", toldTo)
	}
	if err := rp.applySnapshotAttempt(ctx, []api.SnapshotLine{snapLine(t, kept)}, "02", time.Now(), 0, &attempt, snapshotAnswer{restore: "restored"}); err != nil {
		t.Fatal(err)
	}
	if !rp.lostToRestore(lost.ID) || rp.lostToRestore(kept.ID) {
		t.Fatalf("fixture: restore lost %v", rp.lost)
	}
	if st, msg, fenced := row("01REFLOST"); st != api.OutboxFailed || fenced || !strings.Contains(msg, "restored from a backup taken before "+lost.ID) || !strings.Contains(msg, "no work item") {
		t.Fatalf("lost-item refusal after the restore installed: %s fenced=%v %q", st, fenced, msg)
	}
	for _, id := range []string{"01REFKEPT", "01REFOLD"} {
		if _, msg, fenced := row(id); !fenced || strings.Contains(msg, "restored") {
			t.Fatalf("%s was un-fenced outside this restore's window: fenced=%v %q", id, fenced, msg)
		}
	}
	if st, msg, _ := row("01ACKLOST"); st != api.OutboxFailed || strings.Contains(msg, "not applied") || !strings.Contains(msg, "lost: the home took this") {
		t.Fatalf("lost ack rewritten as a refusal: %s %q", st, msg)
	}
	listed, err := rp.entries(ctx, api.ProjectRef{ID: pid}, true, authz.All(), "bea-mac/aaaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	for _, e := range listed {
		shown[e.ID] = true
	}
	if !shown["01REFLOST"] || shown["01REFKEPT"] || shown["01REFOLD"] {
		t.Fatalf("outbox listing after the restore: %+v", listed)
	}
	x := &remote{R: api.Remote{Project: api.ProjectRef{ID: pid}, State: api.RemoteOK}, Copy: rp}
	s.Service = linked(x)
	rp.mu.Lock()
	acks, refused := rp.lostAcks, rp.refused
	rp.mu.Unlock()
	s.tellLostAcks(ctx, rp, x.RoleReply(), attempt.committed, 0, acks, refused)
	notes := map[string]string{}
	for i, addr := range toldTo {
		if _, twice := notes[addr]; twice || told[i].State != orchapi.StateNeedsHuman {
			t.Fatalf("sessions told: %v %+v", toldTo, told)
		}
		notes[addr] = told[i].Note
	}
	if len(notes) != 2 || !strings.Contains(notes["bea-mac/aaaaaaaa"], "not applied: the home was restored and lost "+lost.ID) ||
		!strings.Contains(notes["bea-mac/dddddddd"], "lost: the home was restored from an older backup") {
		t.Fatalf("sessions told: %v %+v", toldTo, told)
	}
	// Settling again (a replay) never doubles the prefix.
	if got := lostRefusal(lost.ID, lostRefusal(lost.ID, "x")); strings.Count(got, "not applied") != 1 {
		t.Fatalf("restore message repeated: %q", got)
	}
}
