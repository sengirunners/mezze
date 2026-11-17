package session

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// No reconciliation loop or second binary build: the child holder starts with
// its compiled protocol, while this caller temporarily expects another one.
func failedAdoptionForRedaction(t *testing.T, denyPIDWrite bool) (*harness, string, core.Scrub) {
	t.Helper()
	h, parent := failedStartHarness(t, `cat >/dev/null; exec sleep 30`)
	ctx := context.Background()
	if denyPIDWrite {
		if _, err := h.d.Store.Registry().Exec(`CREATE TRIGGER deny_started_child BEFORE UPDATE OF pid ON sessions
 WHEN NEW.pid>1 BEGIN SELECT RAISE(ABORT, 'fixture child identity write refused'); END`); err != nil {
			t.Fatal(err)
		}
	}
	oldMajor := holder.ProtoMajor
	holder.ProtoMajor = "999"
	t.Cleanup(func() { holder.ProtoMajor = oldMajor })
	no := false
	h.s.taskMu.Lock()
	_, err := startFailureTaskLocked(t, h, parent, orchapi.TaskStartRequest{
		Agent: "codex", Prompt: "startup redaction canary", Work: "target", Worktree: &no, ClientRequestID: "adoption-redaction",
	})
	h.s.taskMu.Unlock()
	holder.ProtoMajor = oldMajor
	if err == nil {
		t.Fatal("failed adoption unexpectedly succeeded")
	}
	if denyPIDWrite && !strings.Contains(err.Error(), "fixture child identity write refused") {
		t.Fatalf("child identity write fault was not exercised: %v", err)
	}
	var id string
	var pgid, holderPID int
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT t.id,t.pgid,h.holder_pid,t.child_never_started
 FROM session_tasks t JOIN session_holders h ON h.id=t.id WHERE t.request_id='adoption-redaction'`).
		Scan(&id, &pgid, &holderPID, &neverStarted); err != nil || neverStarted || pgid <= 1 || holderPID <= 1 {
		t.Fatalf("started process proof: id=%s group=%d holder=%d never=%v err=%v", id, pgid, holderPID, neverStarted, err)
	}
	// Observe production giveUp cleanup; never signal an invented/reused PID.
	deadline := time.Now().Add(5 * time.Second)
	for {
		gone, _ := h.s.recordedHolderGone(ctx, id)
		if gone && errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("giveUp did not establish holder and group death")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Model the documented post-giveUp state even if its asynchronous Forget
	// has not run yet: there is no exit file to substitute for durable identity.
	holder.Forget(h.home, id)
	h.s.completeTask(id)
	return h, id, core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"startup redaction canary"}}
}

func TestFailedAdoptionRetainsChildIdentityForRedaction(t *testing.T) {
	h, id, sc := failedAdoptionForRedaction(t, false)
	ctx := context.Background()
	r, ok := h.s.load(ctx, id)
	if !ok || r.PID <= 1 || r.pidStart <= 0 {
		t.Fatalf("started child identity was lost after giveUp: %+v", r)
	}
	// Missing child identity still holds redaction despite observed death.
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET pid_start=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := h.s.scrubTasks(ctx, sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("missing child identity bypassed the hold: %v", err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET pid_start=? WHERE id=?`, r.pidStart, id); err != nil {
		t.Fatal(err)
	}
	if err := h.s.scrubTasks(ctx, sc); err != nil {
		t.Fatalf("redaction remained blocked after failed adoption writers exited: %v", err)
	}
	var holds int
	var request string
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT (SELECT count(*) FROM session_task_holds WHERE task_id=t.id),request_json,child_never_started
 FROM session_tasks t WHERE id=?`, id).Scan(&holds, &request, &neverStarted); err != nil || holds != 0 || neverStarted || strings.Contains(request, sc.Texts[0]) {
		t.Fatalf("recovered redaction: holds=%d never=%v request=%q err=%v", holds, neverStarted, request, err)
	}
}

func TestFailedChildIdentityWriteKeepsRedactionHeld(t *testing.T) {
	h, id, sc := failedAdoptionForRedaction(t, true)
	if err := h.s.scrubTasks(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("failed durable identity write bypassed redaction hold: %v", err)
	}
	var holds int
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT (SELECT count(*) FROM session_task_holds WHERE task_id=t.id),child_never_started
 FROM session_tasks t WHERE id=?`, id).Scan(&holds, &neverStarted); err != nil || holds == 0 || neverStarted {
		t.Fatalf("write failure lost conservative proof: holds=%d never=%v err=%v", holds, neverStarted, err)
	}
}
