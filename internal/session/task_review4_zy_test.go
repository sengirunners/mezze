package session

import (
	"bytes"
	"context"
	"io"
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

// Verifier/scrubber vs a literal secret right after a backslash.
func TestZYBackslashBeforeSecret(t *testing.T) {
	for _, c := range [][2]string{
		{`C:\token abc marker`, "token abc marker"},
		{`{"p":"C:\\token abc marker"}`, "token abc marker"},
		{`{"p":"C:\\name abc marker"}`, "name abc marker"},
		{`{"p":"C:\\first abc marker"}`, "first abc marker"},
		{`{"p":"C:\\bob abc marker"}`, "bob abc marker"},
		{`{"p":"C:\\run abc marker"}`, "run abc marker"},
		{`{"p":"C:\\secret abc marker"}`, "secret abc marker"},
	} {
		safe, err := taskRedactionSafe(c[0], []string{c[1]}, 0)
		out, e2 := scrubTaskBytes(c[0], []string{c[1]}, 0)
		t.Logf("input=%s safe=%v err=%v scrubbed=%s err=%v rawStillThere=%v", c[0], safe, err, out, e2, strings.Contains(out, c[1]))
		if strings.Contains(out, c[1]) {
			t.Errorf("LEAK %q", c[0])
		}
	}
}

// End to end: the literal secret stays on disk and in status after a nil ScrubAll.
func TestZYBackslashEndToEnd(t *testing.T) {
	secret := "token abc marker"
	h, parent, task := zzStart(t, `cat >/dev/null; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"see C:\\\\\\\\token abc marker\"}"}}' '{"type":"turn.completed"}'`, "", "p")
	v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
	t.Logf("before=%+v", v.Result)
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	t.Logf("ScrubAll err=%v", err)
	v, _ = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	t.Logf("after=%+v", v.Result)
	sb, _ := os.ReadFile(holder.TaskOutputPath(h.home, task.ID))
	t.Logf("spool=%s", sb)
	if bytes.Contains(sb, []byte(secret)) || strings.Contains(v.Result.Text, secret) {
		t.Errorf("LEAK after ScrubAll err=%v", err)
	}
}

// Everything: prompt, stdout, stderr, detector line, signal. Grep the whole home afterwards.
// Same with exit 0 + success report + phrase: must be ok.
func TestZYPhraseCleanExit(t *testing.T) {
	h, _, task := zzStart(t, `cat >/dev/null; printf '%s\n' 'rate limit exceeded out of memory'; `+zzReport, "", "p")
	v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
	ss, _ := orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("task=%+v session=%+v", v.Result, ss.Outcome)
	if v.Result.Outcome.Kind != orchapi.OutcomeOK || ss.Outcome.Kind != orchapi.OutcomeOK {
		t.Errorf("phrase changed outcome")
	}
}

// exit 3 + phrase.
func TestZYPhraseBadExit(t *testing.T) {
	h, _, task := zzStart(t, `cat >/dev/null; printf '%s\n' 'rate limit exceeded out of memory'; exit 3`, "", "p")
	v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
	ss, _ := orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("task=%+v session=%+v", v.Result, ss.Outcome)
	if v.Result.Outcome.Kind != orchapi.OutcomeFailed || ss.Outcome.Kind != orchapi.OutcomeFailed {
		t.Errorf("phrase changed outcome")
	}
}

// Copied home: a second daemon on a copy must not kill the first daemon's live task.
func TestZYCopiedHomeDaemonKillsOriginalTask(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 40\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, false)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 40", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := orchclient.Session(context.Background(), h1.c, task.ID)
	time.Sleep(1200 * time.Millisecond)
	copyHome := keptHome(t)
	filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		dst := filepath.Join(copyHome, rel)
		if fi.IsDir() {
			os.MkdirAll(dst, 0700)
		} else if fi.Mode().IsRegular() {
			in, e := os.Open(p)
			if e == nil {
				o, _ := os.Create(dst)
				io.Copy(o, in)
				o.Close()
				in.Close()
			}
		}
		return nil
	})
	t.Logf("before copy-daemon: child alive=%v", syscall.Kill(child.PID, 0) == nil)
	h2 := startHarness(t, copyHome, cfg, false)
	t.Cleanup(h2.close)
	time.Sleep(1500 * time.Millisecond)
	st, _ := procStat(child.PID)
	alive := syscall.Kill(child.PID, 0) == nil && !st.zombie
	v, _ := orchclient.TaskStatus(context.Background(), h1.c, task.ID, 0)
	t.Logf("after copy-daemon: child alive=%v original task state=%s result=%+v", alive, v.State, v.Result)
	if !alive {
		t.Errorf("WRONG KILL: daemon on copied home killed the original daemon's live task")
	}
}

// Two genuinely separate homes: reconciling one leaves the other's task alone.
func TestZYSeparateHomesIndependent(t *testing.T) {
	h1, _, t1 := zzStart(t, `cat >/dev/null; exec sleep 15`, "", "p")
	h2, _, t2 := zzStart(t, `cat >/dev/null; exec sleep 15`, "", "p")
	c1, _ := orchclient.Session(context.Background(), h1.c, t1.ID)
	c2, _ := orchclient.Session(context.Background(), h2.c, t2.ID)
	if _, err := h2.s.d.Store.Registry().Exec(`UPDATE session_tasks SET state='ended'`); err != nil {
		t.Fatal(err)
	}
	h2.s.reconcileTaskGroups(context.Background())
	time.Sleep(300 * time.Millisecond)
	t.Logf("h1 child alive=%v, h2 child alive=%v (h2 was reconciled as ended)", syscall.Kill(c1.PID, 0) == nil, syscall.Kill(c2.PID, 0) == nil)
	if syscall.Kill(c1.PID, 0) != nil {
		t.Errorf("other home touched")
	}
}

// Garbage rows: nothing signalled. Victims are sleeps this test started. pgid 1 and
// -1 are not exercised for real: they stop at the guard (checked by source text).
func TestZYGarbageRows(t *testing.T) {
	h, _, task := zzStart(t, `cat >/dev/null; `+zzReport, "", "p")
	zzWait(t, h, task.ID, 15*time.Second)
	v := exec.Command("/bin/sleep", "20")
	v.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := v.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Process.Kill(); _ = v.Wait() })
	nl := exec.Command("/bin/sleep", "20") // not a group leader: in the test's group
	if err := nl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nl.Process.Kill(); _ = nl.Wait() })
	st, _ := procStat(v.Process.Pid)
	nst, _ := procStat(nl.Process.Pid)
	db := h.s.d.Store.Registry()
	set := func(p any, s any) {
		if _, err := db.Exec(`UPDATE session_tasks SET pgid=?,group_start=? WHERE id=?`, p, s, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	alive := func(c *exec.Cmd) bool { return syscall.Kill(c.Process.Pid, 0) == nil }
	for _, c := range []struct {
		name string
		p, s any
	}{
		{"wrong start -1us", v.Process.Pid, st.start - 1},
		{"wrong start +1us", v.Process.Pid, st.start + 1},
		{"zero start", v.Process.Pid, 0},
		{"neg start", v.Process.Pid, -1},
		{"text pgid", "abc", st.start},
		{"text start", v.Process.Pid, "abc"},
		{"float pgid", 1.5, st.start},
		{"overflow pgid +2^32", int64(v.Process.Pid) + 1<<32, st.start},
		{"neg pgid", -v.Process.Pid, st.start},
		{"zero pgid", 0, st.start},
		{"non-leader", nl.Process.Pid, nst.start},
	} {
		set(c.p, c.s)
		h.s.killRecordedTaskGroup(context.Background(), task.ID)
		h.s.reconcileTaskGroups(context.Background())
		time.Sleep(60 * time.Millisecond)
		t.Logf("%-22s victim=%v nonleader=%v", c.name, alive(v), alive(nl))
		if !alive(v) || !alive(nl) {
			t.Fatalf("WRONG KILL on %s", c.name)
		}
	}
	for _, p := range []int64{1 << 32, 1<<32 + 1, -1, 0, int64(os.Getpid()) + 1<<32} {
		_, ok := procStat(int(p))
		t.Logf("procStat(%d) ok=%v", p, ok)
		if ok && p != 0 && p != -1 {
			t.Errorf("procStat accepted wrapped pid %d", p)
		}
	}
	src, _ := os.ReadFile("task_group.go")
	if !strings.Contains(string(src), "pgid <= 1 || started <= 0") {
		t.Error("guard changed")
	}
	set(v.Process.Pid, st.start)
	h.s.killRecordedTaskGroup(context.Background(), task.ID)
	_ = v.Wait()
	t.Logf("control: exact row kills victim state=%v", v.ProcessState)
}

// Running task with live holder across restart.
func TestZYRestartKeepsLiveTask(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\ncat >/dev/null\nsleep 6\n" + zzReport + "\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
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
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(task.ID))
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	time.Sleep(500 * time.Millisecond)
	t.Logf("after restart alive=%v", syscall.Kill(child.PID, 0) == nil)
	v, _, ok := zzWait(t, h2, task.ID, 15*time.Second)
	t.Logf("ended=%v %+v", ok, v.Result)
	if !ok || v.Result.Text != "real report" || v.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Errorf("live task hurt by reconcile: %+v", v.Result)
	}
}

// Cap: spool ends mid-secret. Check passes? Partial remains?
// Fail closed on an unreadable and on an unwritable spool.
func TestZYUnreadableAndUnwritable(t *testing.T) {
	secret := "private failclosed marker"
	run := func(name string, mode os.FileMode) {
		t.Run(name, func(t *testing.T) {
			h, parent, task := zzStart(t, `cat >/dev/null; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"has private failclosed marker\"}"}}' '{"type":"turn.completed"}'`, "", "p")
			v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
			t.Logf("before=%+v", v.Result)
			sp := holder.TaskOutputPath(h.home, task.ID)
			os.Chmod(sp, mode)
			t.Cleanup(func() { os.Chmod(sp, 0600) })
			err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
			t.Logf("ScrubAll err=%v", err)
			v, _ = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
			t.Logf("after=%+v", v.Result)
			if err == nil && strings.Contains(v.Result.Text, secret) {
				t.Errorf("LEAK with nil error")
			}
			if strings.Contains(v.Result.Text, secret) {
				t.Errorf("task_status still returns secret after failed scrub (err=%v)", err)
			}
			var n int
			h.s.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, task.ID).Scan(&n)
			_, e := os.Stat(sp)
			t.Logf("holds=%d spool stat=%v", n, e)
		})
	}
	run("unreadable", 0)
	run("unwritable", 0400)
}

// Invalid escape in an unrelated task: does an unrelated redaction wipe its result?
func TestZYUnrelatedInvalidEscape(t *testing.T) {
	h, parent, task := zzStart(t, `cat >/dev/null; printf '%s\n' 'see C:\users\me for logs'; `+zzReport, "", "p")
	v, _, _ := zzWait(t, h, task.ID, 15*time.Second)
	t.Logf("before=%+v", v.Result)
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{"totally unrelated secret text"}})
	t.Logf("ScrubAll err=%v", err)
	v, _ = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	t.Logf("after=%+v", v.Result)
	_, e := os.Stat(holder.TaskOutputPath(h.home, task.ID))
	t.Logf("spool stat err=%v", e)
	if v.Result.TextWithheld || v.Result.Text == "" {
		t.Errorf("unrelated redaction destroyed result text of a task that never had the secret")
	}
}

// Hold never clears: is the result delivered, is anything stuck?
// Cancel of a parent that traps TERM and exits 0: does explicit cancel cascade?
func TestZYCancelTrapExit0Cascades(t *testing.T) {
	script := `case "$(cat)" in *childprompt*) sleep 15;; *) trap 'exit 0' TERM; sleep 15 & wait;; esac`
	h, _, task := zzStart(t, script, "", "parentprompt")
	h.c.As = task.Session
	no := false
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "childprompt", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	time.Sleep(500 * time.Millisecond)
	v, err := orchclient.TaskCancel(context.Background(), h.c, task.ID)
	t.Logf("cancel err=%v result=%+v", err, v.Result)
	v, _, _ = zzWait(t, h, task.ID, 10*time.Second)
	ss, _ := orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("task=%+v session=%+v", v.Result, ss.Outcome)
	c, _, ok := zzWait(t, h, child.ID, 8*time.Second)
	t.Logf("child ended=%v %+v", ok, c.Result)
	if !ok {
		t.Errorf("explicit cancel did not cascade when parent exits 0 on TERM")
	}
	if v.Result != nil && ss.Outcome != nil && v.Result.Outcome.Kind != ss.Outcome.Kind {
		t.Errorf("disagree")
	}
}
