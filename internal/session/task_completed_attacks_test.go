package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func atk2ResultText(t orchapi.Task) string {
	if t.Result == nil {
		return ""
	}
	return t.Result.Text
}
func atk2Unrelated(project string) core.Scrub {
	return core.Scrub{Project: project, Subjects: []string{"unrelated"}, Texts: []string{"text no task ever saw or printed"}}
}

// An ended, clean task; home copied to a new path; an unrelated redaction.
func TestAtk2CopiedHome(t *testing.T) {
	for _, mode := range []string{"new_path", "same_path"} {
		t.Run(mode, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "fake-agent")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null; echo keep-this-output; "+zzReport+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			base, _ := os.MkdirTemp("", "zm")
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			homeA, homeB := filepath.Join(base, "a"), filepath.Join(base, "b")
			_ = os.MkdirAll(homeA, 0700)
			mut := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}} }
			h := startHarness(t, homeA, mut, true)
			t.Cleanup(h.close)
			parent := h.spawn("shell", "exec sleep 2", nil)
			atkTrackSession(t, h, parent)
			taskClient(h, parent)
			no := false
			task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no})
			if err != nil {
				h.close()
				t.Fatal(err)
			}
			done := waitTask(t, h, task.ID)
			dl := time.Now().Add(3 * time.Second)
			for time.Now().Before(dl) && h.s.proc(task.ID) != nil {
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(2500 * time.Millisecond) // the parent shell exits too
			t.Logf("ended: finished=%s text=%q", done.Result.Finished, done.Result.Text)
			h.close()
			target := homeB
			if mode == "same_path" {
				target = homeA
				if err := os.Rename(homeA, homeB); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command("/bin/cp", "-Rp", homeB, homeA).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
				_ = os.RemoveAll(homeB)
			} else {
				if out, err := exec.Command("/bin/cp", "-Rp", homeA, homeB).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
				_ = os.RemoveAll(homeA)
			}
			h2 := startHarness(t, target, mut, false)
			t.Cleanup(h2.close)
			before := atkFiles(h2, task.ID)
			err = h2.d.ScrubAll(context.Background(), atk2Unrelated(parent.Project))
			after := atkFiles(h2, task.ID)
			st, cleared := atkWaitClear(t, h2, task.ID, 5*time.Second)
			var comp int
			_ = h2.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_compactions`).Scan(&comp)
			t.Logf("redaction answer=%v\n before=%q\n after=%q\n cleared=%v pending=%v text=%q withheld=%v\n compactions=%d", err, before, after, cleared, st.RedactionPending, atk2ResultText(st), st.Result != nil && st.Result.TextWithheld, comp)
			if err != nil || !cleared || after[filepath.Base(holder.TaskOutputPath(target, task.ID))] == "<absent>" {
				t.Fatalf("WRONG: an ended, clean, unrelated task was held or lost output after a home copy")
			}
			for name, content := range before {
				if after[name] != content {
					t.Fatalf("unrelated redaction changed %s", name)
				}
			}
		})
	}
}

// A task whose holder never started: nothing exists that can write.
func TestAtk2SpawnFailedTask(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/nonexistent/interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}} })
	parent := h.spawn("shell", "exec sleep 30", nil)
	taskClient(h, parent)
	no := false
	_, serr := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no})
	if serr == nil {
		t.Fatal("bad interpreter unexpectedly started")
	}
	t.Logf("start: %v", serr)
	var id string
	var pid int
	if err := h.d.Store.Registry().QueryRow(`SELECT t.id,s.pid FROM session_tasks t JOIN sessions s ON s.id=t.id`).Scan(&id, &pid); err != nil {
		t.Fatalf("no task row was kept: %v", err)
	}
	var holderRow int
	_ = h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_holders WHERE id=?`, id).Scan(&holderRow)
	dl := time.Now().Add(5 * time.Second)
	var state string
	for time.Now().Before(dl) {
		_ = h.d.Store.Registry().QueryRow(`SELECT state FROM session_tasks WHERE id=?`, id).Scan(&state)
		if state == "ended" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("row %s pid=%d task state=%s holder rows=%d", id, pid, state, holderRow)
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT child_never_started FROM session_tasks WHERE id=?`, id).Scan(&neverStarted); err != nil || !neverStarted || pid != 0 || holderRow != 0 {
		t.Fatalf("never-started evidence: %v pid=%d holders=%d err=%v", neverStarted, pid, holderRow, err)
	}
	err := h.d.ScrubAll(context.Background(), atk2Unrelated(parent.Project))
	st, cleared := atkWaitClear(t, h, id, 4*time.Second)
	t.Logf("redaction=%v cleared=%v pending=%v", err, cleared, st.RedactionPending)
	if err != nil || !cleared {
		t.Fatalf("WRONG: a task that never ran blocks every redaction in the project")
	}
	h.close()
	h2 := startHarness(t, h.home, nil, false)
	t.Cleanup(h2.close)
	if err := h2.d.ScrubAll(context.Background(), atk2Unrelated(parent.Project)); err != nil {
		t.Fatalf("never-started evidence lost on restart: %v", err)
	}
}

// Ended clean task, its pgid now reused by an unrelated live group.
func TestAtk2GroupReuseUnrelatedEnded(t *testing.T) {
	h, parent, task := zzStart(t, `cat >/dev/null; echo keep-this-output; `+zzReport, "", "")
	done := waitTask(t, h, task.ID)
	time.Sleep(300 * time.Millisecond)
	f := exec.Command("/bin/sleep", "25")
	f.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	started, ok := procStat(f.Process.Pid)
	if !ok {
		_ = f.Wait()
		t.Fatal("cannot identify test process")
	}
	t.Cleanup(func() {
		if now, ok := procStat(f.Process.Pid); ok && now.start == started.start {
			_ = f.Process.Kill()
		}
		_ = f.Wait()
	})
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_tasks SET pgid=? WHERE id=?`, f.Process.Pid, task.ID); err != nil {
		t.Fatal(err)
	}
	var stopped bool
	_ = h.d.Store.Registry().QueryRow(`SELECT group_stopped FROM session_tasks WHERE id=?`, task.ID).Scan(&stopped)
	before := atkFiles(h, task.ID)
	err := h.d.ScrubAll(context.Background(), atk2Unrelated(parent.Project))
	after := atkFiles(h, task.ID)
	st, cleared := atkWaitClear(t, h, task.ID, 4*time.Second)
	o, oerr := orchclient.Output(context.Background(), h.c, task.ID, 0)
	t.Logf("text before=%q group_stopped=%v\n before=%q\n after=%q\n redaction=%v cleared=%v text=%q output=%q %v", done.Result.Text, stopped, before, after, err, cleared, atk2ResultText(st), o.Data, oerr)
	if err != nil || !cleared || strings.Contains(after[filepath.Base(holder.TaskOutputPath(h.home, task.ID))], "<absent>") {
		t.Fatalf("WRONG: pid reuse made a finished task lose output and hold the redaction")
	}
}

// A redaction just as an ordinary task finishes: Forget removed the exit record, holder not yet gone.
func TestAtk2FinishRace(t *testing.T) {
	h, parent := taskHarness(t, `cat >/dev/null; echo keep-this-output; `+zzReport)
	taskClient(h, parent)
	no := false
	lost, held, runs := 0, 0, 0
	for i := 0; i < 25; i++ {
		task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no})
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		runs++
		// Spin until the proc is gone, then redact at once.
		dl := time.Now().Add(5 * time.Second)
		for time.Now().Before(dl) && h.s.proc(task.ID) != nil {
			time.Sleep(50 * time.Microsecond)
		}
		_ = h.s.scrubTasks(context.Background(), atk2Unrelated(parent.Project))
		_, statErr := os.Stat(holder.TaskOutputPath(h.home, task.ID))
		var n int
		_ = h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, task.ID).Scan(&n)
		held += n
		waitTask(t, h, task.ID)
		if statErr != nil {
			lost++
			st, cleared := atkWaitClear(t, h, task.ID, 4*time.Second)
			o, oerr := orchclient.Output(context.Background(), h.c, task.ID, 0)
			var gs bool
			_ = h.d.Store.Registry().QueryRow(`SELECT group_stopped FROM session_tasks WHERE id=?`, task.ID).Scan(&gs)
			t.Logf("task %s: hold cleared=%v text=%q group_stopped=%v output afterwards=%q %v files=%q", task.ID, cleared, atk2ResultText(st), gs, o.Data, oerr, atkFiles(h, task.ID))
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("runs=%d output spool lost=%d held=%d", runs, lost, held)
	if runs == 0 || lost > 0 {
		t.Errorf("DATA LOSS: unrelated redaction removed the spool of %d finished tasks", lost)
	}
}

func TestIncompatibleStartedHolderIsNotMarkedNeverStarted(t *testing.T) {
	other := buildHolder(t, "-X "+holderPkg+".ProtoMajor=2")
	h, parent := taskHarness(t, `cat >/dev/null; exec sleep 25`)
	taskClient(h, parent)
	h.s.HolderExe = other
	no := false
	if _, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no}); err == nil {
		t.Fatal("incompatible holder unexpectedly accepted")
	}
	var neverStarted bool
	var holders int
	err := h.d.Store.Registry().QueryRow(`SELECT child_never_started,(SELECT count(*) FROM session_holders WHERE id=session_tasks.id) FROM session_tasks`).Scan(&neverStarted, &holders)
	if err != nil || neverStarted || holders != 1 {
		t.Fatalf("started holder classified as never started: %v holders=%d err=%v", neverStarted, holders, err)
	}
}
