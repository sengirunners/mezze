package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

const zzReport = `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"real report\"}"}}' '{"type":"turn.completed"}'`

func zzStart(t *testing.T, script string, work string, prompt string) (*harness, orchapi.Session, orchapi.Task) {
	t.Helper()
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	if prompt == "" {
		prompt = "probe"
	}
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: prompt, Work: work, Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	return h, parent, task
}
func zzWait(t *testing.T, h *harness, id string, d time.Duration) (orchapi.Task, time.Duration, bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		v, err := orchclient.TaskStatus(context.Background(), h.c, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == "ended" {
			return v, time.Since(start), true
		}
		time.Sleep(50 * time.Millisecond)
	}
	v, _ := orchclient.TaskStatus(context.Background(), h.c, id, 0)
	return v, time.Since(start), false
}
func zzKill(t *testing.T, path string) {
	t.Cleanup(func() {
		if b, err := os.ReadFile(path); err == nil {
			if n, e := strconv.Atoi(strings.TrimSpace(string(b))); e == nil && n > 1 {
				_ = syscall.Kill(n, syscall.SIGKILL)
			}
		}
	})
}

// Secret on stderr only, unlinked running task: redaction says done; does a copy survive?
func TestZZStderrOnlySecretSurvivesRedaction(t *testing.T) {
	secret := "private stderr secret marker"
	h, parent, task := zzStart(t, `printf '%s\n' '`+secret+`' >&2; sleep 2; `+zzReport, "", "")
	h.waitOutput(task.ID, secret)
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	t.Logf("ScrubAll err=%v", err)
	v, el, ok := zzWait(t, h, task.ID, 15*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	out, oerr := orchclient.Output(context.Background(), h.c, task.ID, 0)
	t.Logf("output err=%v contains=%v", oerr, strings.Contains(out.Data, secret))
	b, e := os.ReadFile(h.s.scrollbackPath(task.ID))
	t.Logf("scrollback err=%v contains=%v", e, strings.Contains(string(b), secret))
	if strings.Contains(string(b), secret) || strings.Contains(out.Data, secret) {
		t.Errorf("LEAK: secret from stderr survives a redaction that returned %v", err)
	}
}

// Non-canonical escape in stdout of a running unlinked task: spool rewritten under a live writer?
func TestZZNonCanonicalEscapeRunningTask(t *testing.T) {
	secret := "private escape marker"
	line := `{"type":"item.started","note":"private \u0065scape marker"}`
	pad := strings.Repeat("x", 2000)
	h, parent, task := zzStart(t, `printf '%s\n' '`+line+`' '`+pad+`'; sleep 2; `+zzReport, "", "")
	h.waitOutput(task.ID, "scape marker")
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	t.Logf("ScrubAll err=%v", err)
	sp := holder.TaskOutputPath(h.home, task.ID)
	v, el, ok := zzWait(t, h, task.ID, 15*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	sb, _ := os.ReadFile(sp)
	t.Logf("spool len=%d nul=%v escaped=%v", len(sb), strings.Contains(string(sb), "\x00"), strings.Contains(string(sb), `\u0065scape`))
	b, _ := os.ReadFile(h.s.scrollbackPath(task.ID))
	t.Logf("scrollback escaped=%v", strings.Contains(string(b), `\u0065scape`))
	if v.Result == nil || v.Result.Finished != "done" || v.Result.Text != "real report" {
		t.Errorf("FALSE RESULT after live spool rewrite: %+v", v.Result)
	}
	if strings.Contains(string(b), `\u0065scape`) {
		t.Errorf("LEAK: escaped secret in scrollback; ScrubAll returned %v", err)
	}
}

func TestZZScrubForms(t *testing.T) {
	s := "private secret marker"
	ml := "private first line\nprivate second line"
	cases := map[string][2]string{
		"parity 12":      {"stray \" quote\n12 {\"t\":\"private s\\u0065cret marker\"}\n", s},
		"parity prefix":  {"stray \" quote\n[1] \"private s\\u0065cret marker\"\n", s},
		"parity obj":     {"stray \" quote\n{\"a\":1} {\"t\":\"private s\\u0065cret marker\"}\n", s},
		"nested canon":   {`{"t":"{\"inner\":\"private secret marker\"}"}` + "\n", s},
		"nested unicode": {`{"t":"{\"inner\":\"private s\\u0065cret marker\"}"}` + "\n", s},
		"nested ml":      {`{"t":"{\"inner\":\"private first line\\nprivate second line\"}"}` + "\n", ml},
		"ml in json":     {`{"t":"private first line\nprivate second line"}` + "\n", ml},
		"ml crlf":        {"before private first line\r\nprivate second line after\n", ml},
		"ml prefix json": {`12 {"t":"private first line\nprivate second line"}` + "\n", ml},
		"key":            {`{"private secret marker":1}` + "\n", s},
		"key escaped":    {`{"private s\u0065cret marker":1}` + "\n", s},
	}
	for name, c := range cases {
		got := scrubTaskTranscript(c[0], []string{c[1]}, false)
		leak := strings.Contains(got, "s\\u0065cret") || strings.Contains(got, "s\\\\u0065cret") || strings.Contains(got, "private secret") || strings.Contains(got, "private first line")
		t.Logf("%-16s leak=%v out=%q", name, leak, got)
		if leak {
			t.Errorf("%s: redaction retained secret", name)
		}
	}
}

func TestZZVerificationFailureDropsOutput(t *testing.T) {
	secret := "private secret marker"
	deep := strings.Repeat(`\`, 1<<16) + `u0041`
	h, parent, task := zzStart(t, `printf '%s\n' 'bad `+deep+` private secret marker'; `+zzReport, "target", "")
	_ = waitTask(t, h, task.ID)
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	v, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || v.Result == nil || !v.Result.TextWithheld || v.Result.Text != "" || v.Result.Finished != "done" {
		t.Fatalf("fail-closed result: %+v %v", v.Result, err)
	}
	for _, p := range []string{holder.TaskOutputPath(h.home, task.ID), holder.TaskErrorPath(h.home, task.ID), h.s.scrollbackPath(task.ID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("output retained: %s %v", p, err)
		}
	}
}

func TestZZStderrOnlyAppearsInGC(t *testing.T) {
	h, _, task := zzStart(t, `printf '%s\n' 'diagnostic only' >&2`, "", "")
	_ = waitTask(t, h, task.ID)
	gc, err := orchclient.SessionGC(context.Background(), h.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range gc {
		if c.Session == task.Session && c.Transcript {
			return
		}
	}
	t.Fatal("stderr-only task not offered for cleanup")
}

// Grandchild in its own session inherits the pipe and outlives the agent.
func TestZZGrandchildHoldsPipe(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "gc.pid")
	zzKill(t, pidf)
	h, _, task := zzStart(t, `perl -e 'use POSIX; POSIX::setsid(); open(F,">`+pidf+`"); print F $$; close F; exec "sleep","25"' & sleep .3; `+zzReport, "", "")
	v, el, ok := zzWait(t, h, task.ID, 20*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	if !ok {
		t.Fatal("HANG: grandchild holds stdout and task never ends")
	}
	if v.Result.Finished != "done" || v.Result.Text != "real report" {
		t.Errorf("report lost: %+v", v.Result)
	}
	if v.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Errorf("clean run with background helper marked %s: %s", v.Result.Outcome.Kind, v.Result.Outcome.Evidence)
	}
}

// Same-group background grandchild (a tool's `server &`).
func TestZZSameGroupGrandchildHoldsPipe(t *testing.T) {
	h, _, task := zzStart(t, `sleep 12 & sleep .3; `+zzReport, "", "")
	v, el, ok := zzWait(t, h, task.ID, 20*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	if ok && v.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Errorf("clean run with background helper marked %s: %s", v.Result.Outcome.Kind, v.Result.Outcome.Evidence)
	}
}

func TestZZEndedTaskKillsRemainingGroup(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "group-child.pid")
	zzKill(t, pidf)
	h, _, task := zzStart(t, `sh -c 'trap "" HUP TERM; echo $$ > `+pidf+`; exec sleep 25' & sleep .3; `+zzReport, "", "")
	v, _, ok := zzWait(t, h, task.ID, 20*time.Second)
	if !ok || v.Result == nil {
		t.Fatalf("task did not end: %+v", v)
	}
	b, err := os.ReadFile(pidf)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := procStat(pid); !ok || st.zombie {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ended task left its recorded process group running")
}

// Child closes stdout and keeps running.
func TestZZClosesStdout(t *testing.T) {
	h, _, task := zzStart(t, `exec 1>&-; exec 2>&-; sleep 25`, "", "")
	h.s.TaskNoOutput = 2 * time.Second
	child, _ := orchclient.Session(context.Background(), h.c, task.ID)
	v, el, ok := zzWait(t, h, task.ID, 12*time.Second)
	t.Logf("ended=%v in %v result=%+v alive=%v", ok, el, v.Result, syscall.Kill(child.PID, 0) == nil)
	if !ok {
		t.Error("HANG: child closed stdout and task did not end at the no-output limit")
	}
}

// Holder SIGKILLed: is the unsupervised child stopped?
func TestZZHolderCrash(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "c.pid")
	zzKill(t, pidf)
	h, _, task := zzStart(t, `echo $$ > `+pidf+`; exec sleep 25`, "", "")
	for i := 0; i < 40; i++ {
		if _, e := os.Stat(pidf); e == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	hp := h.holderPID(task.ID)
	child, _ := orchclient.Session(context.Background(), h.c, task.ID)
	if err := syscall.Kill(hp, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	v, el, ok := zzWait(t, h, task.ID, 10*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	time.Sleep(2 * time.Second)
	alive := syscall.Kill(child.PID, 0) == nil
	t.Logf("child %d alive after task recorded ended: %v", child.PID, alive)
	if alive {
		t.Errorf("RUNAWAY: child still running after task ended (limits no longer count it)")
	}
}

func TestZZGroupCleanupRejectsReusedIdentity(t *testing.T) {
	h, _, task := zzStart(t, `exec sleep 25`, "", "")
	child, err := orchclient.Session(context.Background(), h.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var start int64
	if err = h.d.Store.Registry().QueryRow(`SELECT group_start FROM session_tasks WHERE id=?`, task.ID).Scan(&start); err != nil || start == 0 {
		t.Fatalf("group identity %d: %v", start, err)
	}
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET group_start=? WHERE id=?`, start+1, task.ID); err != nil {
		t.Fatal(err)
	}
	h.s.killRecordedTaskGroup(context.Background(), task.ID)
	if err = syscall.Kill(child.PID, 0); err != nil {
		t.Fatalf("killed mismatched process: %v", err)
	}
	_, _ = h.d.Store.Registry().Exec(`UPDATE session_tasks SET group_start=? WHERE id=?`, start, task.ID)
}

func TestZZReconcileKillsRecordedEndedGroup(t *testing.T) {
	h, _, task := zzStart(t, `exec sleep 25`, "", "")
	child, err := orchclient.Session(context.Background(), h.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET state='ended' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	h.s.reconcileTaskGroups(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := procStat(child.PID); !ok || st.zombie {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("recorded ended group remained alive")
}

func TestZZRestartReconcilesEndedGroup(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h1.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(task.ID))
	if _, err = h1.d.Store.Registry().Exec(`UPDATE session_tasks SET state='ended' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := procStat(child.PID); !ok || st.zombie {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("startup did not stop the recorded ended task group")
}

// Large prompt, child exits without reading, setsid grandchild keeps stdin.
func TestZZLargePromptUnreadStdin(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "gc.pid")
	zzKill(t, pidf)
	h, _, task := zzStart(t, `perl -e 'use POSIX; POSIX::setsid(); open(F,">`+pidf+`"); print F $$; close F; close(STDOUT); close(STDERR); exec "sleep","25"' <&0 & sleep .3; `+zzReport, "", strings.Repeat("p", 200<<10))
	h.s.TaskNoOutput = 3 * time.Second
	v, el, ok := zzWait(t, h, task.ID, 15*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	if !ok {
		t.Errorf("HANG: agent exited, grandchild holds stdin, large prompt; task stuck running past no-output limit")
	}
}

// A parent that succeeds but whose last output mentions a detector phrase must leave its running task alone (SPEC 8).
func TestZZSuccessfulParentPhraseCancelsChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "go")
	script := `case "$(cat)" in *childprompt*) sleep 15;; *) while [ ! -e ` + marker + ` ]; do sleep .1; done; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"the test log said out of memory; fixed\"}"}}' '{"type":"turn.completed"}';; esac`
	h, _, task := zzStart(t, script, "", "")
	h.c.As = task.Session
	no := false
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "childprompt", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	taskClient(h, orchapi.Session{Address: ""})
	v, _, ok := zzWait(t, h, task.ID, 10*time.Second)
	ss, _ := orchclient.Session(context.Background(), h.c, task.ID)
	t.Logf("parent ended=%v finished=%s taskOutcome=%s sessionOutcome=%+v", ok, v.Result.Finished, v.Result.Outcome.Kind, ss.Outcome)
	if !ok || v.Result == nil || ss.Outcome == nil || ss.Outcome.Kind != v.Result.Outcome.Kind {
		t.Errorf("task and session outcomes disagree: task=%+v session=%+v", v.Result, ss.Outcome)
	}
	time.Sleep(6 * time.Second)
	c, _ := orchclient.TaskStatus(context.Background(), h.c, child.ID, 0)
	t.Logf("child state=%s result=%+v", c.State, c.Result)
	if c.State == "ended" {
		t.Errorf("FALSE CANCEL: successful parent's phrase cancelled running child: %+v", c.Result)
	}
}

// Task session surfaces that assume a PTY.
func TestZZPTYAssumptions(t *testing.T) {
	h, _, task := zzStart(t, `sleep 6; `+zzReport, "", "")
	ctx := context.Background()
	err := orchclient.Resize(ctx, h.c, task.ID, orchapi.ResizeRequest{Rows: 30, Cols: 100})
	t.Logf("resize err=%v", err)
	if !api.IsCode(err, api.CodeConflict) {
		t.Errorf("resize should refuse task cleanly: %v", err)
	}
	err = orchclient.Input(ctx, h.c, task.ID, orchapi.InputRequest{Data: "hi\n"})
	t.Logf("input err=%v", err)
	for i := 0; i < 5; i++ {
		ss, e := orchclient.Session(ctx, h.c, task.ID)
		t.Logf("t=%ds state=%s fg=%v busy=%v jobs=%v err=%v", i, ss.State, ss.Fg, ss.Busy, ss.Jobs, e)
		time.Sleep(time.Second)
	}
	v, _, ok := zzWait(t, h, task.ID, 10*time.Second)
	ss, _ := orchclient.Session(ctx, h.c, task.ID)
	t.Logf("end ok=%v state=%s sessionState=%s outcome=%+v", ok, v.State, ss.State, ss.Outcome)
}

// Daemon down during a burst: holder must keep draining.
func TestZZDaemonDownBurst(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\nsleep 1\nhead -c 8388608 /dev/zero | tr '\\000' y\nprintf '\\n'\n" + zzReport + "\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := orchclient.Session(context.Background(), h1.c, task.ID)
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(task.ID))
	h1.close()
	time.Sleep(5 * time.Second)
	t.Logf("child alive while daemon down: %v", syscall.Kill(child.PID, 0) == nil)
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	v, el, ok := zzWait(t, h2, task.ID, 15*time.Second)
	t.Logf("ended=%v in %v result=%+v", ok, el, v.Result)
	if !ok || v.Result == nil || v.Result.Text != "real report" {
		t.Errorf("report lost across daemon outage: %+v", v.Result)
	}
}
