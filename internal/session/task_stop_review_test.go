package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/core"
)

func TestTaskSignalHelper(t *testing.T) {
	dir := os.Getenv("ZM_REVIEW_TOOL_DIR")
	if dir == "" {
		return
	}
	if os.Getenv("ZM_REVIEW_SAME_GROUP") == "" {
		if _, err := syscall.Setsid(); err != nil {
			t.Fatal(err)
		}
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGPIPE)
	if os.Getenv("ZM_REVIEW_WRITE_AFTER_EXIT") != "" {
		go func() {
			for {
				if _, err := os.Stat(filepath.Join(dir, "write")); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			_, outErr := os.Stdout.Write([]byte("private post-exit output marker\n"))
			_, errErr := os.Stderr.Write([]byte("private post-exit output marker\n"))
			_ = os.WriteFile(filepath.Join(dir, "write-result"), []byte(fmt.Sprintf("%t %t", errors.Is(outErr, syscall.EPIPE), errors.Is(errErr, syscall.EPIPE))), 0600)
		}()
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	<-c
	if err := os.WriteFile(filepath.Join(dir, "term"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func reviewTaskWithTool(t *testing.T) (*harness, orchapi.Session, orchapi.Task, string, int, pstat) {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`ZM_REVIEW_TOOL_DIR=%q %q -test.run=^TestTaskSignalHelper$ & wait`, dir, exe)
	h, parent, task := zzStart(t, script, "", "")
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(filepath.Join(dir, "pid"))
		if e == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("tool did not start")
	}
	ident, ok := procStat(pid)
	if !ok {
		t.Fatal("missing tool identity")
	}
	t.Cleanup(func() {
		if now, ok := procStat(pid); ok && now.start == ident.start && !now.zombie {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return h, parent, task, dir, pid, ident
}
func reviewAlive(pid int, ident pstat) bool {
	v, ok := procStat(pid)
	return ok && v.start == ident.start && !v.zombie
}

func TestReviewTaskTermAllowsToolCleanup(t *testing.T) {
	h, _, task, dir, _, _ := reviewTaskWithTool(t)
	if _, err := orchclient.TaskCancel(context.Background(), h.c, task.ID); err != nil {
		t.Fatal(err)
	}
	result := waitTask(t, h, task.ID)
	if _, err := os.Stat(filepath.Join(dir, "term")); err != nil {
		t.Fatalf("TERM cleanup did not run: %v, result=%+v", err, result.Result)
	}
}
func TestReviewTaskSoftStopDoesNotKillTool(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	if _, err := h.s.Stop(context.Background(), h.d.System(), task.ID, orchapi.StopRequest{Signal: "INT", WaitMS: 300}); err != nil {
		t.Fatal(err)
	}
	if !reviewAlive(pid, ident) {
		t.Fatal("INT killed the active tool")
	}
}
func TestReviewUnreachableStopsSeparateTool(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	if err := os.Remove(holder.SocketPath(h.home, task.ID)); err != nil {
		t.Fatal(err)
	}
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && reviewAlive(pid, ident) {
		time.Sleep(20 * time.Millisecond)
	}
	if reviewAlive(pid, ident) {
		t.Fatal("unreachable task left a tool running")
	}
}

func TestReviewSweepFailureStillStopsTask(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	previous := taskStopProcTable
	taskStopProcTable = func() (map[int]procEntry, error) { return nil, fmt.Errorf("injected process listing failure") }
	t.Cleanup(func() { taskStopProcTable = previous })
	cv, ce := orchclient.TaskCancel(context.Background(), h.c, task.ID)
	r0, _ := h.s.load(context.Background(), task.ID)
	st0, ok0 := procStat(r0.PID)
	p0 := h.s.proc(task.ID)
	raw := false
	if p0 != nil {
		raw = taskExitReported(p0)
	}
	t.Logf("cancel=%+v err=%v rootpid=%d stat=%+v ok=%v proc=%v raw=%v", cv, ce, r0.PID, st0, ok0, p0 != nil, raw)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		v, e := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		if e == nil && v.State == "ended" {
			if v.Result == nil || !strings.Contains(v.Result.Outcome.Evidence, "may still be running") {
				t.Fatalf("missing warning: %+v", v.Result)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reviewAlive(pid, ident) {
		t.Log("tool remains live after failed sweep")
	}
	t.Fatal("listing failure prevented group stop")
}
func TestReviewSweepPrunesDaemonAndHolder(t *testing.T) {
	h, _, task, _, _, _ := reviewTaskWithTool(t)
	row, ok := h.s.load(context.Background(), task.ID)
	if !ok {
		t.Fatal("missing task")
	}
	hpid := h.holderPID(task.ID)
	self := os.Getpid()
	beforeTable, beforeSignal := taskStopProcTable, taskStopSignal
	taskStopProcTable = func() (map[int]procEntry, error) {
		all, err := procTable()
		if err != nil {
			return nil, err
		}
		root := all[row.PID]
		root.ppid = 1
		daemon := all[self]
		daemon.ppid = row.PID
		held := all[hpid]
		held.ppid = self
		return map[int]procEntry{row.PID: root, self: daemon, hpid: held}, nil
	}

	var targeted []int
	taskStopSignal = func(pid int, _ syscall.Signal) error { targeted = append(targeted, pid); return nil }
	defer func() { taskStopProcTable = beforeTable; taskStopSignal = beforeSignal }()
	sweep := h.s.newTaskSweep(context.Background(), row)
	sweep.refresh(context.Background())
	sweep.signal(syscall.SIGTERM)
	if sweep.err != nil {
		t.Fatal(sweep.err)
	}
	for _, pid := range targeted {
		if pid == self || pid == hpid {
			t.Fatalf("targeted protected process %d", pid)
		}
	}
}

// A scan can consume the stop budget after SIGSTOP but before KILL. The
// compensating resume must not inherit the expired signal-work deadline.
func TestReviewFreezeBoundExpiryResumesTool(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	ctx := context.Background()
	r, ok := h.s.load(ctx, task.ID)
	if !ok {
		t.Fatal("missing task")
	}
	sweep := h.s.newTaskSweep(ctx, r)
	sweep.bound = time.Now().Add(5 * time.Second)
	previous := taskStopSignal
	taskStopSignal = func(target int, sig syscall.Signal) error {
		err := previous(target, sig)
		if err == nil && target == pid && sig == syscall.SIGSTOP {
			// Deterministically exhaust the budget after a real suspension.
			sweep.bound = time.Now().Add(-time.Second)
		}
		return err
	}
	defer func() {
		taskStopSignal = previous
		sweep.resumeFrozen()
	}()
	sweep.freezeForKill(ctx, time.Now().Add(2*time.Second))
	if sweep.frozen[pid] != ident.start || sweep.err == nil {
		t.Fatalf("did not suspend tool before exhausting the bound: frozen=%v err=%v", sweep.frozen, sweep.err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if st, ok := procStat(pid); ok && st.start == ident.start && st.stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if st, ok := procStat(pid); !ok || st.start != ident.start || !st.stopped {
		t.Fatalf("tool was not suspended: %+v, found=%t", st, ok)
	}
	// The failed inspection prevents KILL, leaving resume as the only cleanup.
	sweep.signal(syscall.SIGKILL)
	previousTable := taskStopProcTable
	taskStopProcTable = func() (map[int]procEntry, error) {
		return nil, errors.New("injected cleanup inspection failure")
	}
	sweep.resumeFrozen()
	taskStopProcTable = previousTable
	if sweep.frozen[pid] != ident.start {
		t.Fatal("failed cleanup discarded the suspended tool identity")
	}
	sweep.resumeFrozen()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if st, ok := procStat(pid); ok && st.start == ident.start && !st.zombie && !st.stopped {
			if sweep.err == nil {
				t.Fatal("resume cleared the incomplete-inspection warning")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tool remained suspended after the stop bound expired")
}

func TestReviewTaskSurvivorCannotReportKilledHigh(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	previous := taskStopSignal
	taskStopSignal = func(target int, sig syscall.Signal) error {
		if target == pid {
			return nil
		}
		return previous(target, sig)
	}
	t.Cleanup(func() { taskStopSignal = previous })
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	done := waitTask(t, h, task.ID)
	if reviewAlive(pid, ident) && (done.Result == nil || !strings.Contains(done.Result.Outcome.Evidence, "may still be running") || done.Result.Outcome.Kind == orchapi.OutcomeKilled && done.Result.Outcome.Confidence == "high") {
		t.Fatalf("live tool %d reported as safely stopped: %+v", pid, done.Result)
	}
}

func TestReviewRespawnHelper(t *testing.T) {
	dir := os.Getenv("ZM_REVIEW_RESPAWN_DIR")
	if dir == "" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
	record := func(p *os.Process) {
		st, ok := procStat(p.Pid)
		if !ok {
			t.Fatal("cannot identify worker")
		}
		f, e := os.OpenFile(filepath.Join(dir, "pids"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = fmt.Fprintf(f, "%d %d\n", p.Pid, st.start)
		_ = f.Close()
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record(self)
	for i := 0; i < 25; i++ {
		c := exec.Command("/bin/sleep", "60")
		if e := c.Start(); e != nil {
			t.Fatal(e)
		}
		record(c.Process)
		go func() { _ = c.Wait() }()
	}
	for {
		c := exec.Command("/bin/sleep", "60")
		if e := c.Start(); e != nil {
			t.Fatal(e)
		}
		record(c.Process)
		_ = c.Wait()
	}
}
func TestReviewRespawningSupervisorCannotHideSurvivors(t *testing.T) {
	dir := t.TempDir()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	script := fmt.Sprintf(`ZM_REVIEW_RESPAWN_DIR=%q %q -test.run=^TestReviewRespawnHelper$ & wait`, dir, exe)
	h, _, task := zzStart(t, script, "", "")
	file := filepath.Join(dir, "pids")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(file)
		if len(strings.Fields(string(b))) >= 54 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	type savedPID struct {
		pid   int
		start int64
	}
	livePIDs := func() []savedPID {
		b, _ := os.ReadFile(file)
		fields := strings.Fields(string(b))
		var live []savedPID
		for i := 0; i+1 < len(fields); i += 2 {
			pid, _ := strconv.Atoi(fields[i])
			start, _ := strconv.ParseInt(fields[i+1], 10, 64)
			st, ok := procStat(pid)
			if ok && st.start == start && !st.zombie {
				live = append(live, savedPID{pid, start})
			}
		}
		return live
	}
	t.Cleanup(func() {
		b, _ := os.ReadFile(file)
		fields := strings.Fields(string(b))
		if len(fields) >= 2 {
			pid, _ := strconv.Atoi(fields[0])
			start, _ := strconv.ParseInt(fields[1], 10, 64)
			if st, ok := procStat(pid); ok && st.start == start && !st.zombie {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				st, ok := procStat(pid)
				if !ok || st.zombie || st.start != start {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		for _, p := range livePIDs() {
			if st, ok := procStat(p.pid); ok && st.start == p.start && !st.zombie {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
	})

	if len(livePIDs()) < 27 {
		t.Fatal("supervisor workers did not start")
	}
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	done := waitTask(t, h, task.ID)
	live := livePIDs()
	if len(live) > 0 && (done.Result == nil || !strings.Contains(done.Result.Outcome.Evidence, "may still be running") || done.Result.Outcome.Kind == orchapi.OutcomeKilled && done.Result.Outcome.Confidence == "high") {
		t.Fatalf("live workers %v reported as stopped: %+v", live, done.Result)
	}
}

func TestReviewPermissionErrorStillStopsGroup(t *testing.T) {
	h, _, task, _, pid, ident := reviewTaskWithTool(t)
	previous := taskStopSignal
	taskStopSignal = func(target int, sig syscall.Signal) error {
		if target == pid {
			return syscall.EPERM
		}
		return previous(target, sig)
	}
	t.Cleanup(func() { taskStopSignal = previous })
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	v := waitTask(t, h, task.ID)
	if v.Result == nil || !strings.Contains(v.Result.Outcome.Evidence, "may still be running") {
		t.Fatalf("missing warning after EPERM: %+v", v.Result)
	}
	if !reviewAlive(pid, ident) {
		t.Fatal("fault injection did not leave the tool alive")
	}
}

func TestReviewSweepDoesNotWalkBelowHolder(t *testing.T) {
	h, _, task, _, tool, _ := reviewTaskWithTool(t)
	r, ok := h.s.load(context.Background(), task.ID)
	if !ok {
		t.Fatal("missing task")
	}
	hp := h.holderPID(task.ID)
	beforeTable, beforeSignal := taskStopProcTable, taskStopSignal
	taskStopProcTable = func() (map[int]procEntry, error) {
		all, e := procTable()
		if e != nil {
			return nil, e
		}
		root := all[r.PID]
		root.ppid = 1
		held := all[hp]
		held.ppid = r.PID
		child := all[tool]
		child.ppid = hp
		return map[int]procEntry{r.PID: root, hp: held, tool: child}, nil
	}
	var hit []int
	taskStopSignal = func(pid int, _ syscall.Signal) error { hit = append(hit, pid); return nil }
	defer func() { taskStopProcTable = beforeTable; taskStopSignal = beforeSignal }()
	sweep := h.s.newTaskSweep(context.Background(), r)
	sweep.refresh(context.Background())
	sweep.signal(syscall.SIGTERM)
	if sweep.err != nil {
		t.Fatal(sweep.err)
	}
	if _, found := sweep.known[tool]; found {
		t.Fatal("walked below holder")
	}
	for _, pid := range hit {
		if pid == hp || pid == tool {
			t.Fatalf("signalled holder subtree pid %d", pid)
		}
	}
}

func TestReviewSoftStopDoesNotKillSameGroupAfterAgentExit(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`trap 'exit 0' INT; ZM_REVIEW_SAME_GROUP=1 ZM_REVIEW_TOOL_DIR=%q %q -test.run=^TestTaskSignalHelper$ & wait`, dir, exe)
	h, _, task := zzStart(t, script, "", "")
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(filepath.Join(dir, "pid"))
		if e == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("tool did not start")
	}
	ident, ok := procStat(pid)
	if !ok {
		t.Fatal("tool identity unavailable")
	}
	t.Cleanup(func() {
		if reviewAlive(pid, ident) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if _, err := h.s.Stop(context.Background(), h.d.System(), task.ID, orchapi.StopRequest{Signal: "INT", WaitMS: 300}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	if !reviewAlive(pid, ident) {
		x, ok := holder.ReadExit(h.home, task.ID)
		v, _ := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
		t.Fatalf("INT-only stop caused same-group tool to be killed: exit=%+v ok=%v status=%+v", x, ok, v.Result)
	}
}

// A surviving separate-session tool cannot rewrite the closed captured pipes.
// Redaction may finish once the holder is gone; worktree and fresh API writes
// are outside its contract and are not asserted here.
func TestReviewSurvivingToolCannotReintroduceRedactedOutput(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	secret := "private post-exit output marker"
	script := fmt.Sprintf(`printf '%%s\n' %q; ZM_REVIEW_WRITE_AFTER_EXIT=1 ZM_REVIEW_TOOL_DIR=%q %q -test.run=^TestTaskSignalHelper$ & wait`, secret, dir, exe)
	h, parent, task := zzStart(t, script, "", "")
	h.waitOutput(task.ID, secret)
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(filepath.Join(dir, "pid"))
		if e == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 1 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("tool did not start")
	}
	ident, ok := procStat(pid)
	if !ok {
		t.Fatal("tool identity unavailable")
	}
	t.Cleanup(func() {
		if reviewAlive(pid, ident) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	previous := taskStopSignal
	taskStopSignal = func(target int, sig syscall.Signal) error {
		if target == pid {
			return nil
		}
		return previous(target, sig)
	}
	t.Cleanup(func() { taskStopSignal = previous })
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	done := waitTask(t, h, task.ID)
	if !reviewAlive(pid, ident) || done.Result == nil || !strings.Contains(done.Result.Outcome.Evidence, "may still be running") {
		t.Fatalf("expected reported live tool: %+v", done.Result)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.s.proc(task.ID) == nil && h.s.taskWriterGone(context.Background(), task.ID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h.s.proc(task.ID) != nil || !h.s.taskWriterGone(context.Background(), task.ID) {
		t.Fatal("holder did not finish closing captured output")
	}
	if !reviewAlive(pid, ident) {
		t.Fatal("tool exited before redaction was verified")
	}
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}); err != nil {
		t.Fatalf("redact retained output: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "write"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var report []byte
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		report, _ = os.ReadFile(filepath.Join(dir, "write-result"))
		if len(report) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(report) != "true true" {
		t.Fatalf("captured pipe writes did not both fail with EPIPE: %q", report)
	}
	for _, p := range []string{holder.TaskOutputPath(h.home, task.ID), holder.TaskErrorPath(h.home, task.ID), h.s.scrollbackPath(task.ID)} {
		if b, e := os.ReadFile(p); e == nil && strings.Contains(string(b), secret) {
			t.Fatalf("redacted text remains in %s", p)
		}
	}
	out, err := orchclient.Output(context.Background(), h.c, task.ID, 0)
	if err == nil && strings.Contains(out.Data, secret) {
		t.Fatal("redacted text returned by output route")
	}
	status, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || status.RedactionPending {
		t.Fatalf("redaction still pending: %+v %v", status, err)
	}
}
