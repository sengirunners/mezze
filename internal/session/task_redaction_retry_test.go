package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/core"
)

// Attack TestAtkF1NewTaskUnderLongHold: a redaction holds the
// tasks running when it happens; its retry, as each held task ends, must
// not hold (and disconnect) an unrelated task started after it.
func TestRedactionRetryLeavesTasksStartedAfterIt(t *testing.T) {
	dir := t.TempDir()
	ma, mb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	script := `p=$(cat); echo ready; case "$p" in *first*) m=` + ma + `;; *) m=` + mb + `;; esac; ` +
		`while [ ! -e "$m" ]; do sleep .05; done; ` + zzReport
	h, parent, first := zzStart(t, script, "", "first")
	h.waitOutput(first.ID, "ready")
	secret := "text redacted at the source"
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}); err == nil {
		t.Fatal("redaction completed while a task that may hold the text runs")
	}
	holds := func(id string) int {
		var n int
		if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if holds(first.ID) == 0 {
		t.Fatal("the running task was not held")
	}
	time.Sleep(10 * time.Millisecond) // started strictly after the first hold
	no := false
	second, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "second", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(second.ID, "ready")

	// The held task ends: its hold's retry runs over the project.
	if err := os.WriteFile(ma, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, first.ID)
	deadline := time.Now().Add(5 * time.Second)
	for holds(first.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if holds(first.ID) != 0 {
		t.Fatal("the original hold did not resolve")
	}
	if n := holds(second.ID); n != 0 {
		t.Fatalf("a task started after the redaction was held by its retry (%d holds)", n)
	}
	st, err := orchclient.TaskStatus(context.Background(), h.c, second.ID, 0)
	if err != nil || st.State != "running" || st.RedactionPending {
		t.Fatalf("second task: %+v %v", st, err)
	}
	if err := os.WriteFile(mb, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if done := waitTask(t, h, second.ID); done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Result.Text != "real report" {
		t.Fatalf("second task result: %+v", done.Result)
	}
}

// The other side of that attack: a retry still holds a newer task whose own
// output carries the text, and a fresh redaction of the same text (same
// hold key, e.g. after an import brought it back) still holds every task
// running then, newer or not.
func TestRedactionRetryStillHoldsWhatMatters(t *testing.T) {
	dir := t.TempDir()
	secret := "text redacted at the source"
	ma, mb, mc := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	script := `p=$(cat); case "$p" in *first*) m=` + ma + `;; *leak*) echo '` + secret + `'; m=` + mc + `;; *) m=` + mb + `;; esac; echo ready; ` +
		`while [ ! -e "$m" ]; do sleep .05; done; ` + zzReport
	h, parent, first := zzStart(t, script, "", "first")
	h.waitOutput(first.ID, "ready")
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("redaction completed while a task runs")
	}
	holds := func(id string) int {
		var n int
		if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	time.Sleep(10 * time.Millisecond)
	no := false
	start := func(prompt string) orchapi.Task {
		t.Helper()
		task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: prompt, Worktree: &no})
		if err != nil {
			t.Fatal(err)
		}
		h.waitOutput(task.ID, "ready")
		return task
	}
	leak := start("leak")
	plain := start("plain")

	// A fresh redaction of the same text holds every running task.
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("fresh redaction completed while tasks run")
	}
	if holds(plain.ID) == 0 || holds(leak.ID) == 0 {
		t.Fatalf("fresh redaction: plain %d, leak %d holds", holds(plain.ID), holds(leak.ID))
	}
	// Retries keep what is held; a newer task carrying the text is held by
	// its own copies even if its hold were gone.
	if _, err := h.d.Store.Registry().Exec(`DELETE FROM session_task_holds WHERE task_id=?`, leak.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ma, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, first.ID)
	deadline := time.Now().Add(5 * time.Second)
	for holds(first.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if holds(leak.ID) == 0 {
		t.Fatal("the retry left a newer task whose output carries the text unheld")
	}
	if holds(plain.ID) == 0 {
		t.Fatal("the retry dropped a hold the fresh redaction placed")
	}
	for _, m := range []string{mb, mc} {
		if err := os.WriteFile(m, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_ = waitTask(t, h, leak.ID)
	_ = waitTask(t, h, plain.ID)
}

// A scrub that carries its redaction's time (a pending-scrub
// retry from the issues module) holds the tasks running then, not a task
// started after it whose copies do not show the text.
func TestRedactionAtLeavesTasksStartedAfterIt(t *testing.T) {
	dir := t.TempDir()
	ma, mb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	script := `p=$(cat); echo ready; case "$p" in *first*) m=` + ma + `;; *) m=` + mb + `;; esac; ` +
		`while [ ! -e "$m" ]; do sleep .05; done; ` + zzReport
	h, parent, first := zzStart(t, script, "", "first")
	h.waitOutput(first.ID, "ready")
	at := time.Now()
	time.Sleep(10 * time.Millisecond)
	no := false
	second, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "second", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(second.ID, "ready")
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{"text redacted at the source"}, At: at}
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("redaction completed while a task running at its time runs")
	}
	holds := func(id string) int {
		var n int
		if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if holds(first.ID) == 0 {
		t.Fatal("the task running at the redaction's time was not held")
	}
	if holds(second.ID) != 0 {
		t.Fatal("a task started after the redaction's time was held")
	}
	for _, m := range []string{ma, mb} {
		if err := os.WriteFile(m, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_ = waitTask(t, h, first.ID)
	_ = waitTask(t, h, second.ID)
}
