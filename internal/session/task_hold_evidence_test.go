package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

func TestHeldOutcomeEvidenceStatusAndCompletion(t *testing.T) {
	h, parent := taskHarness(t, `echo ready; cat >/dev/null; exec sleep 600`)
	taskClient(h, parent)
	ctx := context.Background()
	var mu sync.Mutex
	bodies := map[string]string{}
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, body string) error {
		mu.Lock()
		defer mu.Unlock()
		bodies[kind] = body
		return nil
	}
	no := false
	task, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(task.ID, "ready")
	secret := "private marker present only in outcome evidence"
	exit := 17
	outcome := orchapi.Outcome{Kind: orchapi.OutcomeFailed, Confidence: "low", Signal: "TERM", ExitCode: &exit, Evidence: secret}
	result := orchapi.TaskResult{Finished: "not_done", Text: "public result", Outcome: outcome}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	// Model completion while the holder remains connected and can still drain.
	// Skip initial delivery so the canary cannot escape before the hold exists.
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET state='ended',result=?,mail_done='skipped',comment_done='skipped' WHERE id=?`, string(raw), task.ID); err != nil {
		t.Fatal(err)
	}
	baseline, err := orchclient.TaskStatus(ctx, h.c, task.ID, 0)
	if err != nil || baseline.Result == nil || !reflect.DeepEqual(baseline.Result.Outcome, outcome) {
		t.Fatalf("unheld outcome changed: %+v %v", baseline, err)
	}
	if err = h.d.ScrubAll(ctx, core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{secret}}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("live writer scrub: %v", err)
	}
	if h.s.proc(task.ID) == nil {
		t.Fatal("fixture lost live holder before evidence was withheld")
	}
	// Young holds still defer completion notices, without sleeping 30 seconds.
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET mail_done='pending',comment_done='pending' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	h.s.deliverTask(task.ID)
	mu.Lock()
	youngCount := len(bodies)
	mu.Unlock()
	if youngCount != 0 {
		t.Fatalf("young hold sent %d notices", youngCount)
	}
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_task_holds SET created=? WHERE task_id=?`, time.Now().Add(-time.Minute).UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	status, err := orchclient.TaskStatus(ctx, h.c, task.ID, 0)
	if err != nil || !status.RedactionPending || status.Result == nil || !status.Result.TextWithheld {
		t.Fatalf("held status: %+v %v", status, err)
	}
	publicOutcome := outcome
	publicOutcome.Evidence = "withheld pending redaction"
	if !reflect.DeepEqual(status.Result.Outcome, publicOutcome) {
		t.Errorf("held outcome leaked evidence or changed process metadata: %+v", status.Result.Outcome)
	}
	h.s.deliverTask(task.ID)
	mu.Lock()
	defer mu.Unlock()
	for _, kind := range []string{"mail", "comment"} {
		body, ok := bodies[kind]
		if !ok || !strings.Contains(body, "withheld") || strings.Contains(body, secret) {
			t.Errorf("held %s notice: %q", kind, body)
		}
	}
	// The public projection must neither erase historical evidence nor release
	// safety holds just to permit bounded completion delivery.
	var persisted string
	var holds int
	if err := h.d.Store.Registry().QueryRow(`SELECT result FROM session_tasks WHERE id=?`, task.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persisted, secret) {
		t.Fatal("historical evidence changed before the live writer was gone")
	}
	if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, task.ID).Scan(&holds); err != nil || holds == 0 {
		t.Fatalf("completion released hold: count=%d err=%v", holds, err)
	}
}
