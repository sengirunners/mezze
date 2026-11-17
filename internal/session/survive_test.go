package session

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
)

// pids are processes a test started (holders and their children): each is
// killed at the end only while the same kernel identity still runs. Unknown
// or reused identities never authorize a cleanup signal.
type pids struct {
	mu     sync.Mutex
	l      []int
	starts map[int]int64
}

func (p *pids) add(t *testing.T, ps ...int) {
	// A task's group leader is a separate holder child. Record it for the
	// failure-path cleanup too, so a killed holder cannot orphan the anchor.
	if table, err := procTable(); err == nil {
		owned := map[int]bool{}
		for _, pid := range ps {
			owned[pid] = true
		}
		for _, pid := range ps {
			if child, ok := table[pid]; ok && child.pgid > 1 && child.pgid != pid {
				if anchor, ok := table[child.pgid]; ok && anchor.ppid == child.ppid && owned[child.ppid] {
					ps = append(ps, child.pgid)
				}
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.l) == 0 {
		t.Cleanup(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, pid := range p.l {
				signalSessionCleanupPID(pid, p.starts[pid], procStat, syscall.Kill)
			}
		})
	}
	if p.starts == nil {
		p.starts = make(map[int]int64)
	}
	for _, pid := range ps {
		st, ok := procStat(pid)
		if ok && st.start > 0 {
			p.starts[pid] = st.start
		} else if pid > 1 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			t.Errorf("cannot record cleanup identity for pid %d; unsafe signals will be skipped", pid)
		}
	}
	p.l = append(p.l, ps...)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// holderPID is the pid of a live session's holder.
func (h *harness) holderPID(id string) int {
	h.t.Helper()
	p := h.s.proc(id)
	if p == nil {
		h.t.Fatalf("session %s has no live holder here", id)
	}
	return p.hc.Hello.HolderPID
}

// keptHome is a temp home whose sessions outlive each daemon on it; the
// test's last daemon is closed and the home removed at the end.
func keptHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// restart closes h (its sessions keep running) and starts a new daemon on
// the same home.
func (h *harness) restart(keep bool) *harness {
	h.t.Helper()
	h.close()
	n := startHarness(h.t, h.home, nil, keep)
	h.t.Cleanup(n.close)
	return n
}

func readUntilTimeout(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if strings.Contains(b.String(), want) || err != nil {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, want) {
			t.Fatalf("stream ended without %q: %q", want, s)
		}
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q within 10s", want)
		return ""
	}
}

// TestSessionsSurviveRestart: a daemon restart keeps a shell and a stub
// agent running, with their scrollback, input, resize, exit status and
// bound lease.
func TestSessionsSurviveRestart(t *testing.T) {
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, true)
	t.Cleanup(h1.close)
	ctx := context.Background()
	shell := h1.spawn("shell", `echo shell-ready; exec cat`, func(r *orchapi.SpawnRequest) { r.Work = "local:abc123" })
	agent := h1.spawn("stub", `echo agent-up; read x; echo "got-$x"; exit 3`, nil)
	ps.add(t, shell.PID, agent.PID, h1.holderPID(shell.ID), h1.holderPID(agent.ID))
	h1.waitOutput(shell.ID, "shell-ready")
	h1.waitOutput(agent.ID, "agent-up")

	conn, err := orchclient.Attach(ctx, h1.c, shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	readUntilTimeout(t, br, "shell-ready")
	if _, err := conn.Write([]byte("before-restart\n")); err != nil {
		t.Fatal(err)
	}
	readUntilTimeout(t, br, "before-restart")

	h1.close() // like zm daemon stop: the sessions keep running
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("the attach should end when its daemon stops: %v", err)
	}
	for _, pid := range []int{shell.PID, agent.PID} {
		if !alive(pid) {
			t.Fatalf("pid %d died with the daemon", pid)
		}
	}

	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	for _, ss := range []orchapi.Session{shell, agent} {
		got, err := orchclient.Session(ctx, h2.c, ss.ID)
		if err != nil || got.State != orchapi.StateRunning || got.Unreachable != "" {
			t.Fatalf("after the restart %s is %+v %v", ss.ID, got, err)
		}
	}
	if h2.work.endedHas(shell.Address) {
		t.Fatal("the restart released the shell's bound lease")
	}
	if n := h2.s.ManagedLive(ctx); n != 2 {
		t.Fatalf("managed live %d", n)
	}
	// Output and a full replay continue from before the restart.
	o, err := orchclient.Output(ctx, h2.c, shell.ID, 0)
	if err != nil || !strings.Contains(o.Data, "shell-ready") || !strings.Contains(o.Data, "before-restart") {
		t.Fatalf("output after the restart: %+v %v", o, err)
	}
	conn2, err := orchclient.AttachAll(ctx, h2.c, shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	br2 := bufio.NewReader(conn2)
	replay := readUntilTimeout(t, br2, "before-restart")
	if !strings.Contains(replay, "shell-ready") {
		t.Fatalf("replay after the restart: %q", replay)
	}
	if _, err := conn2.Write([]byte("after-restart\n")); err != nil {
		t.Fatal(err)
	}
	readUntilTimeout(t, br2, "after-restart")
	if err := orchclient.Resize(ctx, h2.c, shell.ID, orchapi.ResizeRequest{Rows: 30, Cols: 90}); err != nil {
		t.Fatal(err)
	}
	if ws, err := pty.GetsizeFull(h2.s.proc(shell.ID).pty); err != nil || ws.Rows != 30 || ws.Cols != 90 {
		t.Fatalf("size after resize: %+v %v", ws, err)
	}

	// The stub agent's exit status and outcome come through the new daemon.
	if err := orchclient.Input(ctx, h2.c, agent.ID, orchapi.InputRequest{Data: "go", Enter: true}); err != nil {
		t.Fatal(err)
	}
	got := h2.wait(agent.ID, "ended", ended)
	if got.Outcome.Kind != orchapi.OutcomeFailed || got.Outcome.ExitCode == nil || *got.Outcome.ExitCode != 3 {
		t.Fatalf("stub agent outcome: %+v", got.Outcome)
	}
	if o, _ := orchclient.Output(ctx, h2.c, agent.ID, 0); !strings.Contains(o.Data, "agent-up") || !strings.Contains(o.Data, "got-go") {
		t.Fatalf("ended agent's output: %q", o.Data)
	}
	h2.waitReleased(agent.Address)

	// Stopping goes through the holder.
	stopped, err := orchclient.Stop(ctx, h2.c, shell.ID, orchapi.StopRequest{})
	if err != nil || !ended(stopped) || stopped.Outcome.Kind != orchapi.OutcomeKilled || stopped.Outcome.Signal != "TERM" {
		t.Fatalf("stop after the restart: %+v %v", stopped.Outcome, err)
	}
	h2.waitReleased(shell.Address)
	waitGone(t, ps.l...)
	if ents, _ := os.ReadDir(holder.Dir(home)); len(ents) != 0 {
		t.Fatalf("holder files left: %v", ents)
	}
}

// waitGone waits for pids to exit.
func waitGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for alive(pid) && !zombie(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("pid %d still runs", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// zombie: exited, not yet reaped (a holder this test process started is its
// child until reaped).
func zombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// TestExitWhileNoDaemon: a session that ends while no daemon runs gets its
// real outcome and output from the next one, not infra.
func TestExitWhileNoDaemon(t *testing.T) {
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, true)
	ss := h1.spawn("stub", `echo last-words; exec sleep 30`, nil)
	hp := h1.holderPID(ss.ID)
	ps.add(t, ss.PID, hp)
	h1.waitOutput(ss.ID, "last-words")
	h1.close()
	if err := syscall.Kill(ss.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitGone(t, ss.PID, hp)
	if _, ok := holder.ReadExit(home, ss.ID); !ok {
		t.Fatal("the holder left no exit file")
	}
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	got, err := orchclient.Session(context.Background(), h2.c, ss.ID)
	if err != nil || !ended(got) || got.Outcome.Kind != orchapi.OutcomeKilled || got.Outcome.Signal != "TERM" ||
		!strings.Contains(got.Outcome.Note, "outside zm") || !strings.Contains(got.Outcome.Note, "no zm daemon ran") {
		t.Fatalf("ended while no daemon ran: %+v %v", got.Outcome, err)
	}
	if o, _ := orchclient.Output(context.Background(), h2.c, ss.ID, 0); !strings.Contains(o.Data, "last-words") {
		t.Fatalf("output: %q", o.Data)
	}
	if !h2.work.endedHas(ss.Address) {
		t.Fatal("its bound leases were not released")
	}
}

// TestHolderKilledIsInfra: a holder killed outright (no exit file) ends its
// session as infra, as before holders.
func TestHolderKilledIsInfra(t *testing.T) {
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, true)
	ss := h1.spawn("stub", `exec sleep 30`, nil)
	hp := h1.holderPID(ss.ID)
	ps.add(t, ss.PID, hp)
	h1.close()
	_ = syscall.Kill(hp, syscall.SIGKILL)
	waitGone(t, hp, ss.PID) // the child gets SIGHUP when the master closes
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	got, _ := orchclient.Session(context.Background(), h2.c, ss.ID)
	if !ended(got) || got.Outcome.Kind != orchapi.OutcomeInfra || got.Outcome.Note != "daemon restarted" {
		t.Fatalf("killed holder: %+v", got.Outcome)
	}
}

// TestTiedHoldersEndWithTheirDaemon: a daemon that does not keep sessions
// (tests, in-process daemons) ends them when it stops.
func TestTiedHoldersEndWithTheirDaemon(t *testing.T) {
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, false)
	ss := h1.spawn("stub", `exec sleep 30`, nil)
	ps.add(t, ss.PID, h1.holderPID(ss.ID))
	h1.close()
	waitGone(t, ss.PID)
	h2 := startHarness(t, home, nil, false)
	t.Cleanup(h2.close)
	got, _ := orchclient.Session(context.Background(), h2.c, ss.ID)
	if !ended(got) || got.Outcome.Kind != orchapi.OutcomeInfra || got.Outcome.Note != "daemon stopped" {
		t.Fatalf("tied session: %+v", got.Outcome)
	}
}

// buildHolder builds a holder binary standing in for another zm build.
func buildHolder(t *testing.T, ldflags string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "holder")
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", ldflags, "./holder/testdata/holdermain")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build holder: %v\n%s", err, b)
	}
	return out
}

const holderPkg = "github.com/sengirunners/mezze/internal/session/holder"

// TestUpgradeAcrossBuilds: a daemon adopts a holder another build started
// with a newer minor version; a holder of another major keeps running,
// unreachable and said so, until a daemon of its version adopts it.
func TestUpgradeAcrossBuilds(t *testing.T) {
	newer := buildHolder(t, "-X "+holderPkg+".ProtoMinor=9 -X github.com/sengirunners/potluck/kit/api.Version=v9.9.9-old")
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, true)
	h1.s.HolderExe = newer
	ss := h1.spawn("shell", `echo from-old-build; exec cat`, nil)
	ps.add(t, ss.PID, h1.holderPID(ss.ID))
	if hl := h1.s.proc(ss.ID).hc.Hello; hl.Minor != 9 || hl.Version != "v9.9.9-old" {
		t.Fatalf("hello from the other build: %+v", hl)
	}
	h1.waitOutput(ss.ID, "from-old-build")
	h2 := h1.restart(true)
	h2.waitOutput(ss.ID, "from-old-build")
	if err := orchclient.Input(context.Background(), h2.c, ss.ID, orchapi.InputRequest{Data: "typed-after-upgrade", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h2.waitOutput(ss.ID, "typed-after-upgrade")
	if _, err := orchclient.Stop(context.Background(), h2.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	h2.close()

	// Another major: started by a daemon of that version, then a daemon of
	// this one cannot adopt it.
	other := buildHolder(t, "-X "+holderPkg+".ProtoMajor=2")
	defer func() { holder.ProtoMajor = "1" }()
	holder.ProtoMajor = "2"
	h3 := startHarness(t, home, nil, true)
	h3.s.HolderExe = other
	s2 := h3.spawn("shell", `echo major-two; exec cat`, nil)
	hp := h3.holderPID(s2.ID)
	ps.add(t, s2.PID, hp)
	h3.waitOutput(s2.ID, "major-two")
	h3.close()
	holder.ProtoMajor = "1"
	h4 := startHarness(t, home, nil, true)
	t.Cleanup(h4.close)
	ctx := context.Background()
	got, err := orchclient.Session(ctx, h4.c, s2.ID)
	if err != nil || got.State == orchapi.StateEnded || !strings.Contains(got.Unreachable, "zm-holder/2") {
		t.Fatalf("incompatible holder: %+v %v", got, err)
	}
	if !alive(s2.PID) || !alive(hp) {
		t.Fatal("an incompatible holder was stopped")
	}
	if _, err := orchclient.Attach(ctx, h4.c, s2.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("attach to an unreachable session: %v", err)
	}
	if rep := h4.s.HolderReport(); rep == nil || len(rep.Unreachable) != 1 || rep.Unreachable[0].Session != s2.ID {
		t.Fatalf("holder report: %+v", rep)
	}
	if n := h4.s.ManagedLive(ctx); n != 1 {
		t.Fatalf("an unreachable session still counts for admission: %d", n)
	}
	// A daemon of its version comes back (a downgrade): the recheck adopts it.
	holder.ProtoMajor = "2"
	h4.s.recheck(ctx)
	got, _ = orchclient.Session(ctx, h4.c, s2.ID)
	if got.Unreachable != "" || h4.s.proc(s2.ID) == nil {
		t.Fatalf("recheck did not adopt it: %+v", got)
	}
	h4.waitOutput(s2.ID, "major-two")
	if _, err := orchclient.Stop(ctx, h4.c, s2.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s2.PID)
}

// TestOrphanReportedNotKilled: a holder whose home is gone is reported and
// left running.
func TestOrphanReportedNotKilled(t *testing.T) {
	gone := keptHome(t)
	var ps pids
	exe, _ := os.Executable()
	st, err := holder.Start(exe, holder.Spec{Session: "0rphan00", Home: gone, Path: "/bin/sleep",
		Args: []string{"sleep", "30"}, Dir: "/", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = st.Cmd.Wait() }()
	ps.add(t, st.ChildPID, st.Cmd.Process.Pid)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil)
	rep := h.s.HolderReport()
	found := false
	for _, o := range rep.Orphans {
		found = found || (o.PID == st.Cmd.Process.Pid && o.Session == "0rphan00" && o.Home == gone)
	}
	if !found {
		t.Fatalf("orphan not reported: %+v", rep)
	}
	h.s.recheck(context.Background())
	if !alive(st.ChildPID) || !alive(st.Cmd.Process.Pid) {
		t.Fatal("an orphan was stopped")
	}
}

// TestEmbedAttachSeesRestart: an attach (`zm attach --embed`) loses its
// stream when the daemon stops, and a new one replays everything, the
// session still live.
func TestEmbedAttachSeesRestart(t *testing.T) {
	home := keptHome(t)
	var ps pids
	h1 := startHarness(t, home, nil, true)
	ss := h1.spawn("shell", `echo embed-ready; exec cat`, nil)
	ps.add(t, ss.PID, h1.holderPID(ss.ID))
	conn, err := orchclient.AttachAll(context.Background(), h1.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	readUntilTimeout(t, bufio.NewReader(conn), "embed-ready")
	h2 := h1.restart(true)
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatal(err)
	}
	got, err := orchclient.Session(context.Background(), h2.c, ss.ID)
	if err != nil || got.State == orchapi.StateEnded {
		t.Fatalf("the embed attach would find the session ended: %+v %v", got, err)
	}
	conn2, err := orchclient.AttachAll(context.Background(), h2.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	readUntilTimeout(t, bufio.NewReader(conn2), "embed-ready")
	if _, err := orchclient.Stop(context.Background(), h2.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
}

// TestQuickExitsNeverFailSpawn: a child that exits while its holder is
// being adopted (its PTY already closed) ends with its real outcome; the
// spawn never fails (CI caught "start /bin/sh: EOF" once).
func TestQuickExitsNeverFailSpawn(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < 40; i++ {
		ss := h.spawn("stub", `exit 4`, nil)
		got := h.wait(ss.ID, "ended", ended)
		if got.Outcome.ExitCode == nil || *got.Outcome.ExitCode != 4 {
			t.Fatalf("run %d: %+v", i, got.Outcome)
		}
	}
}

// TestStreamDropKeepsLiveSession: when the holder's stream drops while the
// holder lives (it hangs up on a daemon that stalled past its write
// deadline, or the socket breaks), the session is not ended and its socket
// is kept: the daemon adopts it again, and input and output go on.
func TestStreamDropKeepsLiveSession(t *testing.T) {
	h := newHarness(t, nil)
	var ps pids
	ctx := context.Background()
	ss := h.spawn("shell", `echo before-drop; exec cat`, nil)
	hp := h.holderPID(ss.ID)
	ps.add(t, ss.PID, hp)
	h.waitOutput(ss.ID, "before-drop")
	old := h.s.proc(ss.ID)
	_ = old.hc.Close() // the stream breaks; the holder lives

	deadline := time.Now().Add(10 * time.Second)
	for {
		if p := h.s.proc(ss.ID); p != nil && p != old {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never adopted again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := orchclient.Session(ctx, h.c, ss.ID)
	if err != nil || got.State == orchapi.StateEnded || got.Unreachable != "" {
		t.Fatalf("after the drop: %+v %v", got, err)
	}
	if h.work.endedHas(ss.Address) {
		t.Fatal("the drop released its leases")
	}
	if _, err := os.Stat(holder.SocketPath(h.home, ss.ID)); err != nil {
		t.Fatalf("its socket: %v", err)
	}
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "after-drop", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "after-drop")
	if o, _ := orchclient.Output(ctx, h.c, ss.ID, 0); !strings.Contains(o.Data, "before-drop") {
		t.Fatalf("scrollback after the drop: %q", o.Data)
	}
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	waitGone(t, ss.PID)
}

// An incompatible but live holder cannot safely be killed on a dial failure.
func TestFailedSpawnLeavesNothing(t *testing.T) {
	other := buildHolder(t, "-X "+holderPkg+".ProtoMajor=2")
	h := newHarness(t, nil)
	h.s.HolderExe = other
	_, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "stub", Command: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: h.home})
	if err == nil || !strings.Contains(err.Error(), "restart it") {
		t.Fatalf("spawn with an incompatible holder: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second) // removed once the holder has gone
	for {
		ents, _ := os.ReadDir(holder.Dir(h.home))
		if len(ents) == 0 {
			break
		}
		if time.Now().After(deadline) {
			names := []string{}
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Fatalf("holder files left: %v", names)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReadoptRace: a lost stream's readopt and the recheck ticker's race;
// the session ends up adopted once, with no stale unreachable entry (a cockpit
// would show it, admission would count it twice).
func TestReadoptRace(t *testing.T) {
	h := newHarness(t, nil)
	var ps pids
	ctx := context.Background()
	ss := h.spawn("shell", `echo racing; exec cat`, nil)
	ps.add(t, ss.PID, h.holderPID(ss.ID))
	h.waitOutput(ss.ID, "racing")
	for round := 0; round < 5; round++ {
		old := h.s.proc(ss.ID)
		_ = old.hc.Close()
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); h.s.recheck(ctx) }()
		}
		wg.Wait()
		deadline := time.Now().Add(10 * time.Second)
		for {
			p := h.s.proc(ss.ID)
			if p != nil && p != old && h.s.unreachableWhy(ss.ID) == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: proc %v, unreachable %q", round, p != nil, h.s.unreachableWhy(ss.ID))
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // let any loser settle
		if why := h.s.unreachableWhy(ss.ID); why != "" {
			t.Fatalf("round %d: stale unreachable: %s", round, why)
		}
		if n := h.s.ManagedLive(ctx); n != 1 {
			t.Fatalf("round %d: managed live %d", round, n)
		}
	}
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "still-here", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "still-here")
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	waitGone(t, ss.PID)
}

// TestSpawnStripsAgentMarkers: a session started through a holder gets its
// environment from envMap: the parent agent's markers do not reach it.
func TestSpawnStripsAgentMarkers(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent-session")
	t.Setenv("CODEX_THREAD_ID", "parent-thread")
	h := newHarness(t, nil)
	ss := h.spawn("stub", `echo "markers=[$CLAUDE_CODE_SESSION_ID$CODEX_THREAD_ID]"; exec cat`, nil)
	h.waitOutput(ss.ID, "markers=[]")
	if _, err := orchclient.Stop(context.Background(), h.c, ss.ID, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
}
