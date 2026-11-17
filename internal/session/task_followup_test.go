package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

func TestHolderWithoutStartKeepsLivePIDDespiteOlderSession(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.spawn("shell", "exec sleep 30", nil)
	p := exec.Command("/bin/sleep", "30")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	st, ok := procStat(p.Process.Pid)
	t.Cleanup(func() {
		if current, found := procStat(p.Process.Pid); ok && found && current.start == st.start {
			_ = p.Process.Kill()
		}
		_ = p.Wait()
	})
	if !ok || st.started.IsZero() {
		t.Skip("process wall-clock start unavailable")
	}
	// A delayed holder may start well after its session row. Reproduce that
	// gap through stored timestamps, without a scheduling-dependent sleep.
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET started=? WHERE id=?`, st.started.Add(-time.Hour).UnixMilli(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET holder_pid=?, holder_start=0 WHERE id=?`, p.Process.Pid, ss.ID); err != nil {
		t.Fatal(err)
	}
	gone, why := h.s.recordedHolderGone(context.Background(), ss.ID)
	if gone {
		t.Fatalf("live PID with unknown kernel start reported gone: %s", why)
	}
}

func TestRedactionHoldsRunningTaskBeforeLaterOutput(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "continue")
	secret := "secret written after redaction"
	script := `cat >/dev/null; echo ready; while [ ! -e ` + marker + ` ]; do sleep .05; done; echo '` + secret + `'; ` + zzReport
	h, parent, task := zzStart(t, script, "", "")
	h.waitOutput(task.ID, "ready")
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	if err == nil {
		t.Error("redaction completed while a running task could still write the secret")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, task.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, e := orchclient.Output(context.Background(), h.c, task.ID, 0)
		if e == nil {
			if strings.Contains(out.Data, secret) {
				t.Fatal("later output retained secret")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("hold did not resolve")
}

func TestRedactionDoesNotLeaveSpoolForUnverifiedLiveChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "continue")
	secret := "secret rewritten after scrub"
	script := `cat >/dev/null; echo ready; while [ ! -e ` + marker + ` ]; do sleep .05; done; echo '` + secret + `'; sleep .5; ` + zzReport
	h, parent, task := zzStart(t, script, "", "")
	h.waitOutput(task.ID, "ready")
	child, err := orchclient.Session(context.Background(), h.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	childStart, ok := procStat(child.PID)
	if !ok {
		t.Fatal("cannot identify test child")
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_tasks SET home_dev='mismatch' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(holder.SocketPath(h.home, task.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.TaskCancel(context.Background(), h.c, task.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unverified child stop: %v", err)
	}
	if current, ok := procStat(child.PID); !ok || current.start != childStart.start {
		t.Fatal("unverified child did not remain running")
	}
	err = h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	if err == nil {
		t.Error("redaction completed while an unverified child could write")
	}
	status, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || !status.RedactionPending || status.State != "running" || status.Result != nil {
		t.Fatalf("pending redaction not visible in status: %+v %v", status, err)
	}
	if out, e := orchclient.Output(context.Background(), h.c, task.ID, 0); e == nil && strings.Contains(out.Data, secret) {
		t.Fatal("unverified task exposed redacted output")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := holder.ReadExit(h.home, task.ID); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := holder.ReadExit(h.home, task.ID); !ok {
		t.Fatal("test holder did not finish")
	}
	row, ok := h.s.load(context.Background(), task.ID)
	if !ok {
		t.Fatal("task missing after holder exit")
	}
	h.s.endFromFile(context.Background(), row)

	for _, path := range []string{holder.TaskOutputPath(h.home, task.ID), holder.TaskErrorPath(h.home, task.ID), h.s.scrollbackPath(task.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("output file remains after holder exit: %s: %v", path, err)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		if err == nil && !status.RedactionPending {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("hold did not clear after the writer exited: %+v %v", status, err)
}

func TestRedactionHoldsAlreadyStartedContinuation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "continue")
	secret := "private handoff transcript marker"
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"` + secret + `"}}' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; echo ready; while [ ! -e ` + marker + ` ]; do sleep .05; done; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"` + secret + `"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.ContinuationError != "" {
			t.Fatal(source.ContinuationError)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatal("continuation did not start")
	}
	h.waitOutput(source.Continuation, "ready")
	err = h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	if err == nil {
		t.Error("redaction completed while continuation could repeat handoff text")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Result != nil && strings.Contains(child.Result.Text, secret) {
		t.Fatal("continuation result retained secret")
	}
}

func snapshotRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "initial"}} {
		if _, err := git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir, "main"
}

func TestSnapshotDisablesRepositoryPrograms(t *testing.T) {
	dir, branch := snapshotRepo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	script := "echo invoked >> " + shellQuote(marker) + "; cat"
	for _, args := range [][]string{{"config", "filter.trap.clean", script}, {"config", "filter.trap.process", "echo invoked >> " + shellQuote(marker) + "; exit 1"}, {"config", "filter.trap.required", "true"}, {"config", "core.fsmonitor", "echo invoked >> " + shellQuote(marker)}, {"config", "core.hooksPath", filepath.Join(dir, "hooks")}} {
		if _, err := git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks", "post-index-change"), []byte("#!/bin/sh\necho invoked >> "+shellQuote(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=trap\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("raw content"), 0600); err != nil {
		t.Fatal(err)
	}
	ok, err := snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: dir, Branch: branch}})
	if err == nil || ok {
		t.Fatalf("filtered snapshot should be refused: %v %v", ok, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository program ran: %v", err)
	}

}

func TestSnapshotRefusesMergeState(t *testing.T) {
	dir, branch := snapshotRepo(t)
	head, err := git(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte(head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "conflict"), []byte("<<<<<<< ours\n=======\n>>>>>>> theirs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: dir, Branch: branch}}); err == nil {
		t.Fatal("snapshot accepted merge")
	}
	after, err := git(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil || after != head {
		t.Fatalf("HEAD changed: %s %v", after, err)
	}
}

func TestGitTimeoutKillsDescendantsHoldingOutput(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 10 &\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := git(ctx, dir, "status")
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("git failed to stop promptly: %v after %v", err, time.Since(start))
	}
}

func TestFailedExecCanRetryHandoff(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	ctx := context.Background()
	no := false
	source, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	bad := filepath.Join(t.TempDir(), "bad-agent")
	if err = os.WriteFile(bad, []byte("#!/nonexistent/zm-interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	original := h.d.Config.File.Agents["claude"]
	h.d.Config.File.Agents["claude"] = []string{bad}
	req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID, ClientRequestID: "retry"}
	if _, err = orchclient.TaskStart(ctx, h.c, req); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("failed exec: %v", err)
	}
	h.d.Config.File.Agents["claude"] = original
	child, err := orchclient.TaskStart(ctx, h.c, req)
	if err != nil || child.ID == "" {
		t.Fatalf("retry: %+v %v", child, err)
	}
	child = waitTask(t, h, child.ID)
	if child.Result == nil || child.Result.Finished != "done" {
		t.Fatalf("child: %+v", child)
	}
}

func TestStoppingMarkerClearsForSurvivingParent(t *testing.T) {
	h, _ := taskHarness(t, `printf '%s\n' '{"type":"turn.completed"}'`)
	parent := h.spawn("shell", `trap '' TERM; echo ready; while :; do sleep 1; done`, nil)
	h.waitOutput(parent.ID, "ready")
	taskClient(h, parent)
	if _, err := h.s.Stop(context.Background(), h.d.System(), parent.ID, orchapi.StopRequest{WaitMS: 100}); err != nil {
		t.Fatal(err)
	}
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil || task.ID == "" {
		t.Fatalf("surviving parent could not start task: %+v %v", task, err)
	}
}
