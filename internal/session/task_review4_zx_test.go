package session

import (
	"context"
	"os"
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

func codeOf(o *orchapi.Outcome) int {
	if o == nil || o.ExitCode == nil {
		return -999
	}
	return *o.ExitCode
}

// Signal plus a detector phrase: kind must come from the signal; the evidence line must not survive scrub.
func TestZXSignalPhrase(t *testing.T) {
	secret := "private phrase marker text"
	h, parent, task := zzStart(t, `cat >/dev/null; printf '%s\n' "rate limit reached private phrase marker text"; sleep 0.2; kill -9 $$`, "", "p")
	v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
	ss, _ := orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("task kind=%s ev=%q conf=%s sig=%s", v.Result.Outcome.Kind, v.Result.Outcome.Evidence, v.Result.Outcome.Confidence, v.Result.Outcome.Signal)
	t.Logf("session kind=%s ev=%q", ss.Outcome.Kind, ss.Outcome.Evidence)
	if v.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Errorf("FALSE OUTCOME: signal-killed task has kind %s from an output phrase", v.Result.Outcome.Kind)
	}
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	t.Logf("ScrubAll err=%v", err)
	v, _ = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	ss, _ = orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("after task ev=%q session ev=%q", v.Result.Outcome.Evidence, ss.Outcome.Evidence)
	if strings.Contains(ss.Outcome.Evidence, secret) {
		t.Errorf("LEAK: session outcome evidence keeps secret after nil ScrubAll")
	}
}

func TestZXSessionRowScrubsNewTextColumns(t *testing.T) {
	secret := "private future marker text"
	h, parent, task := zzStart(t, `cat >/dev/null; `+zzReport, "", "")
	_, _, _ = zzWait(t, h, task.ID, 15*time.Second)
	db := h.d.Store.Registry()
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN future_review_text TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET title=?,note=?,term_title=?,future_review_text=? WHERE id=?`, secret, secret, secret, secret, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	fields, err := h.s.taskSessionTexts(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if strings.Contains(f.value, secret) {
			t.Errorf("session text %s retained secret", f.name)
		}
	}
}

// Live holder whose socket file vanished, daemon restart.
func TestZXSocketGoneLiveTask(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := orchclient.Session(context.Background(), h1.c, task.ID)
	hp := h1.holderPID(task.ID)
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), hp)
	h1.close()
	if err := os.Remove(holder.SocketPath(home, task.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	time.Sleep(800 * time.Millisecond)
	st, _ := procStat(child.PID)
	alive := syscall.Kill(child.PID, 0) == nil && !st.zombie
	t.Logf("holder alive=%v child alive=%v", syscall.Kill(hp, 0) == nil, alive)
	if !alive {
		t.Errorf("task with a live holder killed because its socket file is gone")
	}
}

func TestZXSocketGoneLiveInteractiveSession(t *testing.T) {
	home := keptHome(t)
	h1 := startHarness(t, home, nil, true)
	t.Cleanup(h1.close)
	ss := h1.spawn("shell", "exec sleep 30", nil)
	hp := h1.holderPID(ss.ID)
	var ps pids
	ps.add(t, ss.PID, hp)
	h1.close()
	if err := os.Remove(holder.SocketPath(home, ss.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	got, err := orchclient.Session(context.Background(), h2.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == orchapi.StateEnded || got.Unreachable == "" || syscall.Kill(ss.PID, 0) != nil {
		t.Fatalf("interactive session incorrectly ended: %+v", got)
	}
}
