package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// A completion mail the send limits refuse waits for retry_after without
// spending an attempt, goes once the window has passed, and goes only once.
func TestTaskNoticeWaitsOutRateLimit(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	var mu sync.Mutex
	refuse, sent := 5, 0 // refusals left, mails delivered
	refusals := 0
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		if kind != "mail" {
			return nil
		}
		if refuse > 0 {
			refuse--
			refusals++
			return api.Errorf(api.CodeRateLimited, "mail limit: try again in 30s").WithRetryAfter(30).WithData(map[string]any{"limit": "pair"})
		}
		sent++
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	db := h.d.Store.Registry()
	var calls = func() (int, int) { mu.Lock(); defer mu.Unlock(); return refusals, sent }
	// Many ticks inside the window make no further call: refused once, still pending, no attempt spent.
	for i := 0; i < 6; i++ {
		h.s.deliverTask(v.ID)
	}
	var state string
	var attempts int
	if err := db.QueryRowContext(context.Background(), `SELECT mail_done,mail_attempts FROM session_tasks WHERE id=?`, v.ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if r, s := calls(); r < 1 || s != 0 {
		t.Fatalf("refusals=%d sent=%d", r, s)
	}
	if r, _ := calls(); r != 1 {
		t.Fatalf("ticks inside the window called again: %d refusals", r)
	}
	if state != "pending" || attempts != 0 {
		t.Fatalf("state=%s attempts=%d", state, attempts)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "pending" || got.MailError == "" || got.Result == nil {
		t.Fatalf("status while waiting: %+v %v", got, err)
	}
	// Five windows pass, each refused again (more than the three attempts a failure may spend), then it goes.
	for i := 0; i < 10; i++ {
		if _, err := db.ExecContext(context.Background(), `UPDATE session_tasks SET mail_retry_at=0 WHERE id=?`, v.ID); err != nil {
			t.Fatal(err)
		}
		h.s.deliverTask(v.ID)
		h.s.deliverTask(v.ID)
	}
	got, err = orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "sent" || got.MailError != "" {
		t.Fatalf("after the window: %+v %v", got, err)
	}
	if r, s := calls(); r != 5 || s != 1 {
		t.Fatalf("refusals=%d sent=%d, want 5 and exactly 1", r, s)
	}
}

// The wait is bounded: a refusal still counted from the first one past the
// bound fails the mail, and the result stays readable from the task.
func TestTaskNoticeWaitIsBounded(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	var calls atomic.Int32
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind == "mail" {
			calls.Add(1)
			return api.Errorf(api.CodeRateLimited, "mail limit").WithRetryAfter(60)
		}
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	h.s.deliverTask(v.ID)
	if got, _ := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0); got.MailDelivery != "pending" {
		t.Fatalf("first refusal: %+v", got)
	}
	// The first refusal was over an hour ago.
	old := time.Now().Add(-taskNoticeWaitMax).Add(-time.Minute).UnixMilli()
	if _, err := h.d.Store.Registry().ExecContext(context.Background(), `UPDATE session_tasks SET mail_limited_since=?,mail_retry_at=0 WHERE id=?`, old, v.ID); err != nil {
		t.Fatal(err)
	}
	h.s.deliverTask(v.ID)
	h.s.deliverTask(v.ID)
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "failed" || calls.Load() != 2 || got.Result == nil || got.MailError == "" {
		t.Fatalf("bound: %+v calls=%d err=%v", got, calls.Load(), err)
	}
}

// A repeat refusal means the same mail is already there (an earlier try's
// answer was lost): it is delivered, never sent a second time.
func TestTaskNoticeRepeatRefusalIsDelivered(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'; sleep .2`)
	taskClient(h, parent)
	no := false
	var calls atomic.Int32
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, _ string) error {
		if kind == "mail" {
			calls.Add(1)
			return api.Errorf(api.CodeRateLimited, "same mail").WithRetryAfter(60).WithData(map[string]any{"limit": "repeat"})
		}
		return nil
	}
	v, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, v.ID)
	for i := 0; i < 3; i++ {
		h.s.deliverTask(v.ID)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, v.ID, 0)
	if err != nil || got.MailDelivery != "sent" || calls.Load() != 1 {
		t.Fatalf("repeat: %+v calls=%d err=%v", got, calls.Load(), err)
	}
}
