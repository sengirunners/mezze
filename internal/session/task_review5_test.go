package session

import (
	"context"
	"fmt"
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
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

func zqKill(t *testing.T, hp int) {
	t.Helper()
	if err := syscall.Kill(hp, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if st, ok := procStat(hp); !ok || st.zombie {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("holder %d did not die", hp)
}

func zqState(h *harness, id string) orchapi.Session {
	ss, _ := orchclient.Session(context.Background(), h.c, id)
	return ss
}

func zqSetup(t *testing.T) (*harness, orchapi.Session, int, string) {
	home := keptHome(t)
	h1 := startHarness(t, home, nil, true)
	t.Cleanup(h1.close)
	ss := h1.spawn("shell", "exec sleep 60", nil)
	hp := h1.holderPID(ss.ID)
	ps := &pids{}
	ps.add(t, ss.PID, hp)
	return h1, ss, hp, home
}

func zqReport(t *testing.T, name string, h *harness, ss orchapi.Session) {
	t.Helper()
	got := zqState(h, ss.ID)
	t.Logf("%s: state=%s outcome=%+v unreachable=%q leaseReleased=%v managedLive=%d", name, got.State, got.Outcome, got.Unreachable, h.work.endedHas(ss.Address), h.s.ManagedLive(context.Background()))
}

// Q1a dead pid with a recorded holder.
func TestZQDeadRecorded(t *testing.T) {
	h1, ss, hp, home := zqSetup(t)
	h1.close()
	zqKill(t, hp)
	t0 := time.Now()
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	t.Logf("start took %v", time.Since(t0))
	zqReport(t, "dead recorded", h2, ss)
	if zqState(h2, ss.ID).State != orchapi.StateEnded || !h2.work.endedHas(ss.Address) {
		t.Errorf("GHOST: dead recorded holder not ended")
	}
}

// Q1b dead pid, no holder row (a session started by a pre-PR daemon).
func TestZQDeadNoRecord(t *testing.T) {
	h1, ss, hp, home := zqSetup(t)
	if _, err := h1.d.Store.Registry().Exec(`DELETE FROM session_holders`); err != nil {
		t.Fatal(err)
	}
	h1.close()
	zqKill(t, hp)
	_ = syscall.Kill(ss.PID, syscall.SIGKILL)
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	h2.s.recheck(context.Background())
	zqReport(t, "dead no record", h2, ss)
	time.Sleep(16 * time.Second) // the real recheck loop runs
	zqReport(t, "dead no record +16s", h2, ss)
	got := zqState(h2, ss.ID)
	if _, err := orchclient.Stop(context.Background(), h2.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Logf("stop: %v", err)
	}
	if got.State != orchapi.StateEnded || !h2.work.endedHas(ss.Address) {
		t.Errorf("GHOST: old-schema row with dead holder and dead child stays %s, leases held", got.State)
	}
}

// Q1c the same with the exit file the holder left: the session's real exit is on disk.
func TestZQExitFileNoRecord(t *testing.T) {
	h1, ss, _, home := zqSetup(t)
	if _, err := h1.d.Store.Registry().Exec(`DELETE FROM session_holders`); err != nil {
		t.Fatal(err)
	}
	h1.close()
	_ = syscall.Kill(ss.PID, syscall.SIGTERM)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(holder.SocketPath(home, ss.ID)); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	x, ok := holder.ReadExit(home, ss.ID)
	t.Logf("exit file ok=%v %+v", ok, x)
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	h2.s.recheck(context.Background())
	zqReport(t, "exit file, no record", h2, ss)
	if zqState(h2, ss.ID).State != orchapi.StateEnded {
		t.Errorf("GHOST: exit file says it ended, the session stays live")
	}
}

// Q1d reused pid with a differing start.
func TestZQReusedPid(t *testing.T) {
	h1, ss, hp, home := zqSetup(t)
	v := exec.Command("/bin/sleep", "30")
	if err := v.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Process.Kill(); _ = v.Wait() })
	st, ok := procStat(v.Process.Pid)
	if !ok || st.start <= 1 {
		t.Fatal("cannot identify the replacement process")
	}
	// Linux start times have clock-tick resolution: two processes started in
	// the same tick can legitimately have the same start. Model a reused PID
	// with a definitely different recorded start rather than relying on timing.
	if _, err := h1.d.Store.Registry().Exec(`UPDATE session_holders SET holder_pid=?, holder_start=? WHERE id=?`, v.Process.Pid, st.start-1, ss.ID); err != nil {
		t.Fatal(err)
	}
	h1.close()
	zqKill(t, hp)
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	zqReport(t, "reused pid", h2, ss)
	t.Logf("innocent alive=%v", syscall.Kill(v.Process.Pid, 0) == nil)
	if zqState(h2, ss.ID).State != orchapi.StateEnded {
		t.Errorf("GHOST: reused pid not ended")
	}
	if syscall.Kill(v.Process.Pid, 0) != nil {
		t.Errorf("WRONG KILL of reused pid")
	}
}

// Q1e home renamed (restore, homes move) with a dead holder: nothing lives, nothing to kill.
func TestZQHomeMovedDeadHolder(t *testing.T) {
	h1, ss, hp, home := zqSetup(t)
	h1.close()
	zqKill(t, hp)
	_ = syscall.Kill(ss.PID, syscall.SIGKILL)
	moved := home + "m"
	if err := os.Rename(home, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	h2 := startHarness(t, moved, nil, false)
	t.Cleanup(h2.close)
	h2.s.recheck(context.Background())
	zqReport(t, "home moved", h2, ss)
	if zqState(h2, ss.ID).State != orchapi.StateEnded {
		t.Errorf("GHOST: moved home keeps a dead session live forever")
	}
}

// Q1f home device number changes (reboot, container restart), simulated by editing the row.
func TestZQHomeDevChanged(t *testing.T) {
	h1, ss, hp, home := zqSetup(t)
	if _, err := h1.d.Store.Registry().Exec(`UPDATE session_holders SET home_dev='999'`); err != nil {
		t.Fatal(err)
	}
	h1.close()
	zqKill(t, hp)
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	h2.s.recheck(context.Background())
	zqReport(t, "dev changed", h2, ss)
	if zqState(h2, ss.ID).State != orchapi.StateEnded {
		t.Errorf("GHOST: changed st_dev keeps a dead session live forever")
	}
}

// Q1h live holder and id lost: does the adoption of a healthy session survive?
func TestZQHomeIDLostLiveHolder(t *testing.T) {
	h1, ss, _, home := zqSetup(t)
	h1.close()
	if err := os.Remove(filepath.Join(home, "session-home-id")); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	zqReport(t, "id lost live holder", h2, ss)
	if zqState(h2, ss.ID).Unreachable != "" {
		t.Errorf("healthy holder with a good socket refused: %q", zqState(h2, ss.ID).Unreachable)
	}
	if got, err := orchclient.Stop(context.Background(), h2.c, ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil || got.State != orchapi.StateEnded {
		t.Fatalf("authenticated holder did not stop: %+v %v", got, err)
	}
}

// Q2 live holder, socket gone.
func TestZQUnreachableWayOut(t *testing.T) {
	home := keptHome(t)
	h1 := startHarness(t, home, func(c *config.Config) {}, true)
	t.Cleanup(h1.close)
	ss := h1.spawn("shell", "exec sleep 60", nil)
	hp := h1.holderPID(ss.ID)
	var ps pids
	ps.add(t, ss.PID, hp)
	h1.close()
	if err := os.Remove(holder.SocketPath(home, ss.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	ctx := context.Background()
	zqReport(t, "unreachable", h2, ss)
	_, err := orchclient.Stop(ctx, h2.c, ss.ID, orchapi.StopRequest{Force: true, Escalate: true})
	t.Logf("stop --force: %v", err)
	t.Logf("child alive after stop=%v", syscall.Kill(ss.PID, 0) == nil)
	_, oerr := orchclient.Output(ctx, h2.c, ss.ID, 0)
	t.Logf("output err=%v", oerr)
	t.Logf("blockers=%d holderReport=%+v", len(h2.s.RestartBlockers(ctx)), h2.s.HolderReport())
	t0 := time.Now()
	if st, ok := procStat(hp); ok && !st.zombie {
		zqKill(t, hp)
	}
	for time.Since(t0) < 20*time.Second {
		if zqState(h2, ss.ID).State == orchapi.StateEnded {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("after holder death, took %v", time.Since(t0))
	zqReport(t, "holder died", h2, ss)
	if zqState(h2, ss.ID).State != orchapi.StateEnded || !h2.work.endedHas(ss.Address) {
		t.Errorf("GHOST after holder death")
	}
}

// Q2 admission: ghosts count?
func TestZQGhostsCountAgainstAdmission(t *testing.T) {
	home := keptHome(t)
	cfgf := func(c *config.Config) { c.File.Admission.MaxManagedSessions = 2 }
	h1 := startHarness(t, home, cfgf, true)
	t.Cleanup(h1.close)
	a := h1.spawn("shell", "exec sleep 60", nil)
	b := h1.spawn("shell", "exec sleep 60", nil)
	var ps pids
	ha, hb := h1.holderPID(a.ID), h1.holderPID(b.ID)
	ps.add(t, a.PID, b.PID, ha, hb)
	if _, err := h1.d.Store.Registry().Exec(`DELETE FROM session_holders`); err != nil {
		t.Fatal(err)
	}
	h1.close()
	zqKill(t, ha)
	zqKill(t, hb)
	h2 := startHarness(t, home, cfgf, true)
	t.Cleanup(h2.close)
	h2.s.recheck(context.Background())
	_, err := orchclient.Spawn(context.Background(), h2.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 5"}, Cwd: h2.home})
	t.Logf("spawn with 2 dead ghosts and a cap of 2: err=%v live=%d", err, h2.s.ManagedLive(context.Background()))
	if err != nil {
		t.Errorf("GHOSTS block admission: %v", err)
	}
}

// Q3 interactive spawn with an incompatible holder.
func TestZQIncompatibleSpawn(t *testing.T) {
	other := buildHolder(t, "-X "+holderPkg+".ProtoMajor=2")
	h := newHarness(t, nil)
	h.s.HolderExe = other
	ctx := context.Background()
	ss, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "stub", Command: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: h.home})
	t.Logf("spawn err=%v state=%s pid=%d", err, ss.State, ss.PID)
	if err != nil {
		return
	}
	var hp int
	_ = h.d.Store.Registry().QueryRow(`SELECT holder_pid FROM session_holders WHERE id=?`, ss.ID).Scan(&hp)
	var ps pids
	ps.add(t, ss.PID, hp)
	got := zqState(h, ss.ID)
	t.Logf("state=%s unreachable=%q", got.State, got.Unreachable)
	_, e1 := orchclient.Output(ctx, h.c, ss.ID, 0)
	e2 := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "x", Enter: true})
	_, e3 := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{Force: true, Escalate: true})
	t.Logf("output=%v\ninput=%v\nstop=%v", e1, e2, e3)
	t.Errorf("FALSE SUCCESS: spawn returned ok for a session it can never read, type to or stop")
}

// Q3 a task with an incompatible holder: deadline, cancel, parent's slots.
func TestZQIncompatibleTask(t *testing.T) {
	other := buildHolder(t, "-X "+holderPkg+".ProtoMajor=2")
	h, parent := taskHarness(t, `cat >/dev/null; exec sleep 25`)
	taskClient(h, parent)
	h.s.HolderExe = other
	ctx := context.Background()
	no := false
	task, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no, TimeoutSeconds: 2})
	t.Logf("task err=%v id=%s state=%s", err, task.ID, task.State)
	if err != nil {
		return
	}
	var hp int
	_ = h.d.Store.Registry().QueryRow(`SELECT holder_pid FROM session_holders WHERE id=?`, task.ID).Scan(&hp)
	cp := zqState(h, task.ID).PID
	var ps pids
	ps.add(t, cp, hp)
	_, cerr := orchclient.TaskCancel(ctx, h.c, task.ID)
	t.Logf("cancel err=%v", cerr)
	time.Sleep(5 * time.Second)
	v, _ := orchclient.TaskStatus(ctx, h.c, task.ID, 0)
	t.Logf("5s later (deadline 2s): state=%s child alive=%v unreachable=%q", v.State, syscall.Kill(cp, 0) == nil, zqState(h, task.ID).Unreachable)
	if v.State != "ended" {
		t.Errorf("TASK STUCK: deadline and cancel cannot stop it")
	}
}

// A live task whose socket was lost: the deadline.
func TestZQUnreachableTaskDeadline(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 40\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 40", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no, TimeoutSeconds: 3})
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
	h2 := startHarness(t, home, cfg, true)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	time.Sleep(7 * time.Second)
	v, _ := orchclient.TaskStatus(context.Background(), h2.c, task.ID, 0)
	_, cerr := orchclient.TaskCancel(context.Background(), h2.c, task.ID)
	t.Logf("state=%s child alive=%v cancel=%v", v.State, syscall.Kill(child.PID, 0) == nil, cerr)
	if v.State != "ended" {
		t.Errorf("UNSTOPPABLE: past deadline, task %s, child alive", v.State)
	}
	if st, ok := procStat(hp); ok && !st.zombie {
		zqKill(t, hp)
	}
	v2, d, ok := zzWait(t, h2, task.ID, 20*time.Second)
	t.Logf("after holder death ended=%v in %v result=%+v child alive=%v", ok, d, v2.Result, syscall.Kill(child.PID, 0) == nil)
}

// Quick exits: how long until ended?
func TestZQQuickExitLatency(t *testing.T) {
	h := newHarness(t, nil)
	var worst time.Duration
	var stuck int
	for i := 0; i < 12; i++ {
		t0 := time.Now()
		ss := h.spawn("shell", "exit 7", nil)
		for time.Since(t0) < 30*time.Second {
			if zqState(h, ss.ID).State == orchapi.StateEnded {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		d := time.Since(t0)
		if d > worst {
			worst = d
		}
		if d > 2*time.Second {
			stuck++
			t.Logf("slow %v outcome=%+v", d, zqState(h, ss.ID).Outcome)
		}
	}
	t.Logf("quick exits: worst=%v over2s=%d", worst, stuck)
	if stuck > 0 {
		t.Errorf("quick exit sat live %v", worst)
	}
}

// Exit while the daemon is down, next daemon starts right after: prompt? exit code kept?
func TestZQExitWhileDaemonDown(t *testing.T) {
	home := keptHome(t)
	h1 := startHarness(t, home, nil, true)
	t.Cleanup(h1.close)
	ss := h1.spawn("shell", "exec sleep 60", nil)
	hp := h1.holderPID(ss.ID)
	var ps pids
	ps.add(t, ss.PID, hp)
	h1.close()
	_ = syscall.Kill(ss.PID, syscall.SIGTERM)
	time.Sleep(150 * time.Millisecond) // holder still around (ack wait)
	st, ok := procStat(hp)
	t.Logf("holder alive at new start=%v zombie=%v", ok, st.zombie)
	t0 := time.Now()
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	for time.Since(t0) < 30*time.Second {
		if zqState(h2, ss.ID).State == orchapi.StateEnded {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := zqState(h2, ss.ID)
	took := time.Since(t0)
	t.Logf("ended after %v state=%s outcome=%+v unreachable=%q", took, got.State, got.Outcome, got.Unreachable)
	if took > 3*time.Second {
		t.Errorf("exit recorded late: %v", took)
	}
}

func TestUnreachableTaskCancelWithoutHomeIdentity(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 40\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 40", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no, OnLimit: "next_agent"})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := orchclient.Session(context.Background(), h1.c, task.ID)
	hp := h1.holderPID(task.ID)
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), hp)
	if _, err = h1.d.Store.Registry().Exec(`UPDATE session_tasks SET home_dev='mismatch' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	h1.close()
	if err = os.Remove(holder.SocketPath(home, task.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, cfg, true)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	mail := make(chan string, 1)
	h2.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, _, body string) error {
		if kind == "mail" {
			mail <- body
		}
		return nil
	}
	_, err = orchclient.TaskCancel(context.Background(), h2.c, task.ID)
	if !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unverified child: %v", err)
	}
	v, err := orchclient.TaskStatus(context.Background(), h2.c, task.ID, 0)
	if err != nil || v.State != "running" || v.Result != nil {
		t.Fatalf("unverified child recorded ended: %+v %v", v, err)
	}
	var failure string
	if err = h2.d.Store.Registry().QueryRow(`SELECT failure FROM session_tasks WHERE id=?`, task.ID).Scan(&failure); err != nil || !strings.Contains(failure, "home identity") || !strings.Contains(failure, "may still be running") {
		t.Fatalf("warning %q: %v", failure, err)
	}
	select {
	case <-mail:
		t.Fatal("completion mail before proof of exit")
	default:
	}

	var continuations int
	if err := h2.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_handoffs WHERE source=?`, task.ID).Scan(&continuations); err != nil || continuations != 0 {
		t.Fatalf("unsafe handoff: %d %v", continuations, err)
	}
	if _, err := orchclient.TaskRead(context.Background(), h2.c, task.ID, task.ID+":0", 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unsafe read: %v", err)
	}
	st, ok := procStat(child.PID)
	if !ok || st.zombie {
		t.Fatal("unverified child was signalled")
	}
	// This is a test-owned PID. Verify its recorded start before cleanup.
	row, ok := h2.s.load(context.Background(), task.ID)
	if !ok || row.pidStart != st.start {
		t.Fatal("test child identity changed")
	}
	if err := syscall.Kill(child.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := holder.ReadExit(home, task.ID); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := holder.ReadExit(home, task.ID); !ok {
		t.Fatal("holder did not record exit")
	}
	row, ok = h2.s.load(context.Background(), task.ID)
	if !ok {
		t.Fatal("task missing")
	}
	h2.s.endFromFile(context.Background(), row)
	v = waitTask(t, h2, task.ID)
	if v.Result == nil || v.Result.Finished != "not_done" || !strings.Contains(v.Result.Outcome.Evidence, "home identity") {
		t.Fatalf("result: %+v", v.Result)
	}
	if _, err := orchclient.TaskRead(context.Background(), h2.c, task.ID, task.ID+":0", 0); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unsafe read: %v", err)
	}

	if why := h2.s.unreachableWhy(task.ID); why != "" {
		t.Fatalf("ended task still unreachable: %s", why)
	}
	h2.s.deliverTask(task.ID)
	select {
	case body := <-mail:
		if !strings.Contains(body, fmt.Sprintf("pid %d", child.PID)) || !strings.Contains(body, "may still be running") {
			t.Fatalf("mail: %s", body)
		}
	default:
		t.Fatal("completion mail was not delivered")
	}
}

func TestAdoptedTaskCancelWithAuthenticatedHolder(t *testing.T) {
	home := keptHome(t)
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 40\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 40", nil)
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
	if err = os.Remove(filepath.Join(home, "session-home-id")); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, cfg, true)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	v, err := orchclient.TaskCancel(context.Background(), h2.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "ended" || v.Result == nil || v.Result.Finished != "not_done" || v.Result.Outcome.Kind != orchapi.OutcomeFailed || !strings.Contains(v.Result.Outcome.Evidence, "home identity") || !strings.Contains(v.Result.Outcome.Evidence, "may still be running") {
		t.Fatalf("result: %+v", v.Result)
	}
	if st, ok := procStat(child.PID); ok && !st.zombie {
		t.Fatal("authenticated holder did not stop the child group")
	}

}

func TestLegacyAdoptedHolderCanStopViaAuthenticatedSocket(t *testing.T) {
	h1, ss, _, home := zqSetup(t)
	if _, err := h1.d.Store.Registry().Exec(`DELETE FROM session_holders WHERE id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	h1.close()
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	if got := zqState(h2, ss.ID); got.Unreachable != "" {
		t.Fatalf("adoption failed: %+v", got)
	}
	if got, err := orchclient.Stop(context.Background(), h2.c, ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil || got.State != orchapi.StateEnded {
		t.Fatalf("legacy holder did not stop: %+v %v", got, err)
	}
}

func TestDaemonStopReportsEndedAndUnreachableLegacySessions(t *testing.T) {
	home := keptHome(t)
	h1 := startHarness(t, home, nil, true)
	t.Cleanup(h1.close)
	legacy := h1.spawn("shell", "exec sleep 40", nil)
	missing := h1.spawn("shell", "exec sleep 40", nil)
	var ps pids
	ps.add(t, legacy.PID, missing.PID, h1.holderPID(legacy.ID), h1.holderPID(missing.ID))
	if _, err := h1.d.Store.Registry().Exec(`DELETE FROM session_holders WHERE id IN (?,?)`, legacy.ID, missing.ID); err != nil {
		t.Fatal(err)
	}
	h1.close()
	if err := os.Remove(holder.SocketPath(home, missing.ID)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	ended, notEnded, err := h2.s.EndManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ended != 1 || len(notEnded) != 1 || notEnded[0].Session != missing.Address || !strings.Contains(notEnded[0].Reason, "manually") {
		t.Fatalf("daemon stop: ended=%d not ended=%+v", ended, notEnded)
	}
	if zqState(h2, legacy.ID).State != orchapi.StateEnded {
		t.Fatal("authenticated legacy session did not end")
	}
	if syscall.Kill(missing.PID, 0) != nil {
		t.Fatal("unverified session was signalled")
	}
}

func TestCancelTaskWhenAdoptedHolderSocketDisappears(t *testing.T) {
	h, parent, task := zzStart(t, `cat >/dev/null; exec sleep 30`, "", "p")
	taskClient(h, parent)
	child, err := orchclient.Session(context.Background(), h.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(holder.SocketPath(h.home, task.ID)); err != nil {
		t.Fatal(err)
	}
	got, err := orchclient.TaskCancel(context.Background(), h.c, task.ID)
	if err != nil || got.State != "ended" || got.Result == nil || got.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	waitGone(t, child.PID)
	if why := h.s.unreachableWhy(task.ID); why != "" {
		t.Fatalf("ended task still unreachable: %s", why)
	}
}
