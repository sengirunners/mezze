package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// A hook session whose agent exits without a session-end hook (Codex never
// sends one) ends at the reaper's next tick: its pid is gone.
func TestHookSessionEndsWhenItsProcessIsGone(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	agent := exec.Command("sleep", "30")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _, _ = agent.Process.Wait() })
	r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: "gone-1",
		State: orchapi.StateIdle, PID: agent.Process.Pid}})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := loadRow(ctx, h.s.d.Store.Registry(), r.Session.ID); row.PID != agent.Process.Pid || row.pidStart == 0 {
		t.Fatalf("pid %d start %d, want %d and its start", row.PID, row.pidStart, agent.Process.Pid)
	}
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, r.Session.ID); got.State == orchapi.StateEnded {
		t.Fatalf("ended while its process runs: %+v", got)
	}
	_ = agent.Process.Kill()
	_, _ = agent.Process.Wait()
	h.s.reapUnmanaged()
	got, _ := orchclient.Session(ctx, h.c, r.Session.ID)
	if got.State != orchapi.StateEnded || got.Outcome == nil || got.Outcome.Evidence != "process gone" || got.Outcome.Kind == orchapi.OutcomeOK {
		t.Fatalf("after its process exited: %+v %+v", got, got.Outcome)
	}
}

// A pid the kernel gave to another process does not keep a gone agent's
// session alive: the recorded start time no longer matches.
func TestHookSessionEndsWhenItsPidIsReused(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, ok := procStat(os.Getpid()); !ok {
		t.Skip("no process start times here")
	}
	mine := os.Getpid() // alive throughout
	same, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "same-1", PID: mine})
	reused, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "reused-1", PID: mine})
	// As if reused-1's agent had this pid before this process took it.
	if _, err := h.s.d.Store.Registry().ExecContext(ctx, `UPDATE sessions SET pid_start = pid_start - 1 WHERE id = ?`, reused.ID); err != nil {
		t.Fatal(err)
	}
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, same.ID); got.State == orchapi.StateEnded {
		t.Fatalf("a session of a live process ended: %+v", got)
	}
	got, _ := orchclient.Session(ctx, h.c, reused.ID)
	if got.State != orchapi.StateEnded || got.Outcome == nil || !strings.Contains(got.Outcome.Evidence, "another process") {
		t.Fatalf("reused pid: %+v %+v", got, got.Outcome)
	}
}

// A hook session with no pid (registered before hooks sent one: the ghosts
// on the farm boxes) ends once no hook has reached it for twice
// bound_lease_ttl, counted from the store, not from the daemon's start; its
// next hook revives it under the same address. One heard from recently,
// and one with a pid to watch, stay.
func TestPidlessHookSessionExpires(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ghost, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "ghost-1", State: orchapi.StateIdle})
	heard, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "heard-1", State: orchapi.StateIdle})
	watched, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "watched-1", State: orchapi.StateIdle, PID: os.Getpid()})
	ttl := 2 * h.s.d.Config.BoundLeaseTTL()
	later := time.Now().Add(ttl + time.Minute)
	h.s.hooked.Store(heard.ID, later.Add(-time.Minute))
	h.s.startedAt = later // a daemon restarted just now still ends the ghost

	h.s.expireQuiet(time.Now().Add(ttl - time.Minute))
	if got, _ := orchclient.Session(ctx, h.c, ghost.ID); got.State == orchapi.StateEnded {
		t.Fatalf("ended before 2x bound_lease_ttl: %+v", got)
	}
	h.s.expireQuiet(later)
	got, _ := orchclient.Session(ctx, h.c, ghost.ID)
	if got.State != orchapi.StateEnded || got.Outcome == nil || !strings.Contains(got.Outcome.Evidence, "no pid to watch") {
		t.Fatalf("ghost: %+v %+v", got, got.Outcome)
	}
	for _, s := range []orchapi.Session{heard, watched} {
		if got, _ := orchclient.Session(ctx, h.c, s.ID); got.State == orchapi.StateEnded {
			t.Fatalf("%s ended: %+v", s.RuntimeID, got)
		}
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{}); len(l) != 2 {
		t.Fatalf("live list still has the ghost: %+v", l)
	}
	back, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "ghost-1", State: orchapi.StateRunning})
	if err != nil || back.Address != ghost.Address || back.State != orchapi.StateRunning {
		t.Fatalf("revived: %+v %v", back, err)
	}
}

// A running pid-less session that quietHooks set idle ("no hooks", which
// writes the row a bound TTL after the last hook) ends at 2x bound TTL
// after its last hook, not 3x.
func TestQuietRunningSessionExpiresAtTwiceTheTTL(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "run-1", State: orchapi.StateRunning})
	bound := h.s.d.Config.BoundLeaseTTL()
	idled := time.Now() // as quietHooks writes it: a bound TTL after the last hook
	if _, err := h.s.d.Store.Registry().ExecContext(ctx, `UPDATE sessions SET state = 'idle', note = 'no hooks', updated = ? WHERE id = ?`,
		idled.UnixMilli(), ss.ID); err != nil {
		t.Fatal(err)
	}
	h.s.hooked.Delete(ss.ID)
	h.s.expireQuiet(idled.Add(bound - time.Minute))
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State == orchapi.StateEnded {
		t.Fatalf("ended before 2x bound TTL from its last hook: %+v", got)
	}
	h.s.expireQuiet(idled.Add(bound + time.Minute))
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateEnded {
		t.Fatalf("not ended at 2x bound TTL from its last hook: %+v", got)
	}
}

// startChild starts a process that runs until the test ends; reap: wait
// for it when it is killed (false leaves a zombie until cleanup).
func startChild(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	return c
}

func pidOf(t *testing.T, h *harness, id string) (int, int64) {
	t.Helper()
	r, err := loadRow(context.Background(), h.s.d.Store.Registry(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r.PID, r.pidStart
}

// The pid a hook sends is taken only when it can be the agent: never pid
// 1 (an orphaned hook), never over a recorded pid whose process runs,
// never a process that started after the session; and a hook whose agent
// has exited (a late Codex notify) does not revive an ended session.
func TestHookPIDIsTakenOnlyFromTheAgent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, ok := procStat(os.Getpid()); !ok {
		t.Skip("no process start times here")
	}
	hook := func(rid string, pid int) orchapi.Session {
		t.Helper()
		r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: rid, State: orchapi.StateIdle, PID: pid}})
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}
	a, b := startChild(t), startChild(t)

	orphan := hook("orphan-1", 1)
	if pid, _ := pidOf(t, h, orphan.ID); pid != 0 {
		t.Fatalf("pid 1 recorded: %d", pid)
	}
	ss := hook("agent-1", a.Process.Pid)
	hook("agent-1", b.Process.Pid) // another live process: the recorded one still runs
	hook("agent-1", 1)
	if pid, st := pidOf(t, h, ss.ID); pid != a.Process.Pid || st == 0 {
		t.Fatalf("recorded pid %d (start %d), want the first agent %d", pid, st, a.Process.Pid)
	}

	// A session that started before the process a hook names: not its agent.
	late := hook("late-1", 0)
	if _, err := h.s.d.Store.Registry().ExecContext(ctx, `UPDATE sessions SET started = started - 600000 WHERE id = ?`, late.ID); err != nil {
		t.Fatal(err)
	}
	hook("late-1", b.Process.Pid)
	if pid, _ := pidOf(t, h, late.ID); pid != 0 {
		t.Fatalf("a process started after the session was taken: %d", pid)
	}

	// A late notify from an exited agent does not revive its ended session.
	gone := startChild(t)
	g := hook("gone-1", gone.Process.Pid)
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, g.ID); got.State != orchapi.StateEnded {
		t.Fatalf("not ended: %+v", got)
	}
	if got := hook("gone-1", gone.Process.Pid); got.State != orchapi.StateEnded {
		t.Fatalf("a late hook of an exited agent revived it: %+v", got)
	}
	if got := hook("gone-1", 1); got.State != orchapi.StateEnded {
		t.Fatalf("an orphaned hook (old client: pid 1) revived it: %+v", got)
	}
	if got := hook("gone-1", 0); got.State != orchapi.StateEnded {
		t.Fatalf("an orphaned hook (no pid) revived a session whose agent zm watched: %+v", got)
	}
	// A hook naming no agent but reporting work (an agent AgentPID cannot
	// tell) revives it, without the dead pid: the quiet expiry watches it.
	other := startChild(t)
	o := hook("other-1", other.Process.Pid)
	_ = other.Process.Kill()
	_, _ = other.Process.Wait()
	h.s.reapUnmanaged()
	r2, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: "other-1", State: orchapi.StateRunning}})
	if err != nil || r2.Session.State != orchapi.StateRunning {
		t.Fatalf("a working hook did not revive it: %+v %v", r2.Session, err)
	}
	if pid, st := pidOf(t, h, o.ID); pid != 0 || st != 0 {
		t.Fatalf("revived keeping the dead pid %d (start %d)", pid, st)
	}
	// A new run of the conversation (a live agent) does revive it.
	if got := hook("gone-1", b.Process.Pid); got.State == orchapi.StateEnded {
		t.Fatalf("a live agent's hook did not revive it: %+v", got)
	} else if pid, _ := pidOf(t, h, g.ID); pid != b.Process.Pid {
		t.Fatalf("revived onto pid %d, want %d", pid, b.Process.Pid)
	}
}

// A zombie (exited, not yet reaped by its parent) is gone.
func TestHookSessionOfAZombieEnds(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	z := startChild(t)
	r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex", RuntimeID: "zombie-1", PID: z.Process.Pid}})
	if err != nil {
		t.Fatal(err)
	}
	_ = z.Process.Signal(os.Kill) // not waited for: a zombie until cleanup
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if p, ok := procStat(z.Process.Pid); !ok || p.zombie {
			break
		}
	}
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, r.Session.ID); got.State != orchapi.StateEnded {
		t.Fatalf("a zombie's session: %+v", got)
	}
}

// After an upgrade, a live session recorded with a pid but no start (an
// older daemon) gets its start filled in rather than being ended; one whose
// pid now names a process that started after the session is ended.
func TestUpgradeBackfillsPidStart(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, ok := procStat(os.Getpid()); !ok {
		t.Skip("no process start times here")
	}
	live, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "old-live", PID: os.Getpid()})
	reused, _ := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "old-reused", PID: os.Getpid()})
	db := h.s.d.Store.Registry()
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET pid_start = 0 WHERE id IN (?, ?)`, live.ID, reused.ID); err != nil {
		t.Fatal(err)
	}
	// reused-1's session began before this process: its pid was reused.
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET started = 1000 WHERE id = ?`, reused.ID); err != nil {
		t.Fatal(err)
	}
	h.s.expireQuiet(time.Now().Add(24 * time.Hour)) // never for a session with a pid
	h.s.reapUnmanaged()
	if got, _ := orchclient.Session(ctx, h.c, live.ID); got.State == orchapi.StateEnded {
		t.Fatalf("a live old session ended: %+v", got)
	}
	if _, st := pidOf(t, h, live.ID); st == 0 {
		t.Fatal("start not filled in")
	}
	if got, _ := orchclient.Session(ctx, h.c, reused.ID); got.State != orchapi.StateEnded {
		t.Fatalf("an old session on a reused pid: %+v", got)
	}
}
