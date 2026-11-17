package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/store"
)

// surviveRig runs real `zm daemon` processes (this test binary as zm) on a
// temp home. Every pid it starts or learns of is killed at the end.
type surviveRig struct {
	t    *testing.T
	home string
	work string // a project directory
	mu   sync.Mutex
	pids []int
}

func TestDaemonEndSessionsReportsLegacyAndUnreachable(t *testing.T) {
	r := newSurviveRig(t)
	r.startDaemon()
	legacyID := r.zm("run", "-d", "--agent", "shell", "/bin/sh", "-c", "exec sleep 40")
	missingID := r.zm("run", "-d", "--agent", "shell", "/bin/sh", "-c", "exec sleep 40")
	legacy, missing := r.session(legacyID), r.session(missingID)
	r.track(legacy.PID, missing.PID)
	r.zm("daemon", "stop")
	db, err := store.OpenFile(context.Background(), filepath.Join(r.home, "registry.db"), 1, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{legacy.ID, missing.ID} {
		var hp int
		if err = db.QueryRow(`SELECT holder_pid FROM session_holders WHERE id=?`, id).Scan(&hp); err != nil {
			t.Fatal(err)
		}
		r.track(hp)
	}
	if _, err = db.Exec(`DELETE FROM session_holders WHERE id IN (?,?)`, legacy.ID, missing.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(holder.SocketPath(r.home, missing.ID)); err != nil {
		t.Fatal(err)
	}
	r.startDaemon()
	out := r.zm("daemon", "stop", "--end-sessions")
	if !strings.Contains(out, "1 managed sessions ended") || !strings.Contains(out, missing.Address) || !strings.Contains(out, "manually") {
		t.Fatalf("unexpected stop report: %s", out)
	}
	if syscall.Kill(missing.PID, 0) != nil {
		t.Fatal("unverified session was signalled")
	}
	r.startDaemon()
	if got := r.session(legacyID); got.State != orchapi.StateEnded {
		t.Fatalf("legacy session: %+v", got)
	}
}

// holderTrouble reports whether zm status flags this rig's session holders:
// an unreachable session, or an orphan from this rig's home or of session id.
// Status lists every orphan of this user on the machine (SPEC "Orphans"), so
// a holder left by another test's deleted temp home is not this test's.
func (r *surviveRig) holderTrouble(status, id string) bool {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.Contains(line, "unreachable") {
			return true
		}
		if strings.Contains(line, "orphan") && (strings.Contains(line, r.home) || strings.Contains(line, "session "+short)) {
			return true
		}
	}
	return false
}

func newSurviveRig(t *testing.T) *surviveRig {
	t.Helper()
	root, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	r := &surviveRig{t: t, home: filepath.Join(root, "h"), work: filepath.Join(root, "acme")}
	for _, d := range []string{r.home, r.work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1},
		"lease_tick_seconds": 1, "bound_lease_ttl_seconds": 6}`
	if err := os.WriteFile(filepath.Join(r.home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.zmErr("daemon", "stop", "--force")
		r.mu.Lock()
		for _, pid := range r.pids {
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		r.mu.Unlock()
		_ = os.RemoveAll(root)
	})
	return r
}

func (r *surviveRig) track(pids ...int) {
	r.mu.Lock()
	r.pids = append(r.pids, pids...)
	r.mu.Unlock()
}

func (r *surviveRig) env() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZM_") || k == "HOME" || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+filepath.Dir(r.home), "ZM_HOME="+r.home, "ZM_MACHINE=mini", "ZM_HUMAN=ana",
		"ZM_TEST_MAIN=1", "ZM_NO_AUTOSTART=1")
}

func (r *surviveRig) cmd(args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], args...)
	c.Env, c.Dir = r.env(), r.work
	return c
}

// zmErr runs zm and returns its output and error.
func (r *surviveRig) zmErr(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Args[0], args...)
	c.Env, c.Dir = r.env(), r.work
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

func (r *surviveRig) zm(args ...string) string {
	r.t.Helper()
	out, err := r.zmErr(args...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.t.Fatalf("zm %v: %v: %s %s", args, err, out, ee.Stderr)
		}
		r.t.Fatalf("zm %v: %v", args, err)
	}
	return out
}

// startDaemon starts a detached daemon and returns its pid.
func (r *surviveRig) startDaemon() int {
	r.t.Helper()
	r.zm("daemon", "--detach")
	b, err := os.ReadFile(filepath.Join(r.home, "daemon.lock"))
	if err != nil {
		r.t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	r.track(pid)
	return pid
}

func (r *surviveRig) session(id string) orchapi.Session {
	r.t.Helper()
	var ss orchapi.Session
	if err := json.Unmarshal([]byte(r.zm("session", "show", id, "--json")), &ss); err != nil {
		r.t.Fatal(err)
	}
	return ss
}

func (r *surviveRig) leaseHolder(key string) string {
	r.t.Helper()
	var ls []orchapi.Lease
	if err := json.Unmarshal([]byte(r.zm("leases", "--json")), &ls); err != nil {
		r.t.Fatal(err)
	}
	for _, l := range ls {
		if l.Key == key {
			return l.Holder
		}
	}
	return ""
}

// embed is a running `zm attach --embed`.
type embed struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func (r *surviveRig) embedAttach(id string) *embed {
	r.t.Helper()
	e := &embed{cmd: r.cmd("attach", "--embed", id), done: make(chan struct{})}
	e.in, _ = e.cmd.StdinPipe()
	pr, pw := io.Pipe()
	e.cmd.Stdout = pw
	if err := e.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.track(e.cmd.Process.Pid)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			e.mu.Lock()
			e.out.Write(buf[:n])
			e.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = e.cmd.Wait(); _ = pw.Close(); close(e.done) }()
	return e
}

func (e *embed) expect(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		got := e.out.String()
		e.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("embed attach never showed %q: %q", want, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *embed) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case <-e.done:
		return e.cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatal("embed attach did not exit")
		return -1
	}
}

// TestDaemonKillKeepsSessions: SIGKILL of a real `zm daemon` leaves its
// session running with its bound lease; the next daemon adopts it, an embed
// attach (an embedding client) that lost its stream exits 3 and a new one replays it all.
func TestDaemonKillKeepsSessions(t *testing.T) {
	r := newSurviveRig(t)
	pid := r.startDaemon()
	r.zm("project", "new", "acme", "--path", r.work)
	var item orchapi.WorkItem
	if err := json.Unmarshal([]byte(r.zm("work", "new", "survive the restart", "--json")), &item); err != nil {
		t.Fatal(err)
	}
	id := r.zm("run", "-d", "--work", item.ID, "--agent", "shell", "/bin/sh", "-c", "echo e2e-ready; exec cat")
	ss := r.session(id)
	r.track(ss.PID)
	if ss.State != orchapi.StateRunning || r.leaseHolder(item.ID) != ss.Address {
		t.Fatalf("before: %+v, lease holder %q", ss, r.leaseHolder(item.ID))
	}
	e := r.embedAttach(id)
	e.expect(t, "e2e-ready")
	if _, err := e.in.Write([]byte("before-kill\n")); err != nil {
		t.Fatal(err)
	}
	e.expect(t, "before-kill")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := e.exitCode(t); code != 3 {
		t.Fatalf("embed attach exit after the daemon died: %d, want 3 (attach again)", code)
	}
	if syscall.Kill(ss.PID, 0) != nil {
		t.Fatal("the session died with the daemon")
	}
	time.Sleep(2 * time.Second) // shorter than the bound TTL (6 s)

	r.startDaemon()
	got := r.session(id)
	if got.State != orchapi.StateRunning || got.Unreachable != "" {
		t.Fatalf("after the kill: %+v", got)
	}
	time.Sleep(7 * time.Second) // past the old deadline: only the new daemon's renewals keep it
	if h := r.leaseHolder(item.ID); h != ss.Address {
		t.Fatalf("lease holder after the restart: %q, want %s", h, ss.Address)
	}
	e2 := r.embedAttach(id)
	e2.expect(t, "before-kill") // the whole scrollback, from before the kill
	if _, err := e2.in.Write([]byte("after-kill\n")); err != nil {
		t.Fatal(err)
	}
	e2.expect(t, "after-kill")
	if st := r.zm("status"); r.holderTrouble(st, id) {
		t.Fatalf("status: %s", st)
	}

	// A graceful stop keeps it too.
	if out := r.zm("daemon", "stop"); !strings.Contains(out, "keep running") {
		t.Fatalf("daemon stop: %s", out)
	}
	if code := e2.exitCode(t); code != 3 {
		t.Fatalf("embed attach exit after daemon stop: %d", code)
	}
	r.startDaemon()
	if got := r.session(id); got.State != orchapi.StateRunning {
		t.Fatalf("after daemon stop: %+v", got)
	}
	out, _ := r.zmErr("kill", id)
	if got := r.session(id); got.State != orchapi.StateEnded || got.Outcome == nil || got.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("kill after restarts: %s %+v", out, got.Outcome)
	}
	if h := r.leaseHolder(item.ID); h != "" {
		t.Fatalf("lease still held after the session ended: %s", h)
	}
}

// TestUpgradeKeepsHolderSessions: `zm daemon upgrade` with a live session
// passes the restart guard (its holder outlives the daemon), and the session
// survives the swap: same process, its scrollback, input after.
func TestUpgradeKeepsHolderSessions(t *testing.T) {
	z := newZeroEnv(t)
	if err := os.MkdirAll(z.zmHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1}}`
	if err := os.WriteFile(filepath.Join(z.zmHome(), "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	inst, stage := filepath.Join(z.home, "inst"), filepath.Join(z.home, "stage")
	zm := filepath.Join(inst, "zm")
	copyExe(t, os.Args[0], zm)
	if _, e, code := z.runBin(zm, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	pid0 := z.daemonPID()
	out, e, code := z.runBin(zm, "run", "-d", "--agent", "shell", "/bin/sh", "-c", "echo before-upgrade; exec cat")
	if code != 0 {
		t.Fatalf("run: %s %s", out, e)
	}
	id := strings.TrimSpace(out)
	show := func() orchapi.Session {
		t.Helper()
		o, e, code := z.runBin(zm, "session", "show", id, "--json")
		var ss orchapi.Session
		if code != 0 || json.Unmarshal([]byte(o), &ss) != nil {
			t.Fatalf("session show: %d %s %s", code, o, e)
		}
		return ss
	}
	before := show()
	t.Cleanup(func() { _ = syscall.Kill(before.PID, syscall.SIGKILL) })
	waitOut := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			o, _, _ := z.runBin(zm, "session", "output", id)
			if strings.Contains(o, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("output never had %q: %q", want, o)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitOut("before-upgrade")

	copyExe(t, os.Args[0], filepath.Join(stage, "zm"))
	out, e, code = z.runBin(zm, "daemon", "upgrade", filepath.Join(stage, "zm"))
	if code != 0 || !strings.Contains(out, "zm daemon upgraded") {
		t.Fatalf("upgrade with a live holder session: %d\n%s\n%s", code, out, e)
	}
	if p := z.daemonPID(); p == 0 || p == pid0 {
		t.Fatalf("daemon not swapped: %d -> %d", pid0, p)
	}
	after := show()
	if after.State == orchapi.StateEnded || after.PID != before.PID || after.Unreachable != "" || syscall.Kill(before.PID, 0) != nil {
		t.Fatalf("the session did not survive the upgrade: %+v", after)
	}
	waitOut("before-upgrade")
	if _, e, code := z.runBin(zm, "session", "input", id, "after-upgrade"); code != 0 {
		t.Fatalf("input: %s", e)
	}
	waitOut("after-upgrade")
	if o, _, _ := z.runBin(zm, "status"); strings.Contains(o, "unreachable") {
		t.Fatalf("status: %s", o)
	}
	if _, _, code := z.runBin(zm, "kill", id); code != 0 {
		t.Fatal("kill after the upgrade")
	}
	if got := show(); got.State != orchapi.StateEnded {
		t.Fatalf("after kill: %+v", got)
	}
}

// TestUpgradeVerifyFailsOnUnreachableHolders: a new daemon that reports
// sessions it cannot reach is not kept (the swap rolls back).
func TestUpgradeVerifyFailsOnUnreachableHolders(t *testing.T) {
	if why := holdersProblem(orchapi.Health{}); why != "" {
		t.Fatalf("no holders: %q", why)
	}
	if why := holdersProblem(orchapi.Health{Holders: &orchapi.HolderReport{Adopted: 3}}); why != "" {
		t.Fatalf("all adopted: %q", why)
	}
	h := orchapi.Health{Holders: &orchapi.HolderReport{Unreachable: []orchapi.HolderProblem{{Session: "abcd1234", Why: "its holder speaks zm-holder/2"}}}}
	if why := holdersProblem(h); !strings.Contains(why, "1 sessions are alive") || !strings.Contains(why, "abcd1234") {
		t.Fatalf("unreachable: %q", why)
	}
}

// TestVerifyAsksForHoldersAfterTheProbe: the Health a swap first gets can
// predate the session module's start (GET /v0/health skips the start gate),
// with no holders in it; only one read after the probe's gated calls can
// name the sessions the new build cannot reach.
func TestVerifyAsksForHoldersAfterTheProbe(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	var started atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		h := orchapi.Health{Health: api.Health{OK: true, Protocol: api.Protocol, Version: api.Version, Commit: api.Commit}}
		if started.Load() {
			h.Holders = &orchapi.HolderReport{Unreachable: []orchapi.HolderProblem{{Session: "abcd1234", Why: "its holder speaks zm-holder/2"}}}
		}
		_ = json.NewEncoder(w).Encode(h)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	probe = func(context.Context, *client.Client) api.ServeCheck {
		started.Store(true) // its gated calls waited for the modules
		return api.ServeCheck{}
	}
	t.Cleanup(func() { probe = swapProbe })
	s := &swapper{ctx: context.Background(), c: client.NewUnix(filepath.Join(dir, "s"))}
	early := api.Health{OK: true, Protocol: api.Protocol, Version: api.Version, Commit: api.Commit} // no Holders yet
	if why := s.verify(early, api.ServeCheck{}); !strings.Contains(why, "cannot reach their holders") || !strings.Contains(why, "abcd1234") {
		t.Fatalf("verify judged the early health: %q", why)
	}
	_ = srv.Close()
	started.Store(false)
	if why := s.verify(early, api.ServeCheck{}); !strings.Contains(why, "stopped answering") {
		t.Fatalf("verify with the daemon gone: %q", why)
	}
}

// TestUpgradeToOtherHolderMajorRollsBack: a real upgrade to a build that
// cannot speak to the live session's holder is rolled back, directly and
// under a supervisor, and the previous build adopts the session again.
func TestUpgradeToOtherHolderMajorRollsBack(t *testing.T) {
	// This test binary with another holder protocol major, standing in for a
	// zm whose holders this one cannot speak to. Built before the rig changes
	// HOME, where the go build cache lives.
	other := filepath.Join(t.TempDir(), "zm")
	build := exec.Command("go", "test", "-c", "-o", other,
		"-ldflags", "-X github.com/sengirunners/mezze/internal/session/holder.ProtoMajor=2", ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the other major: %v\n%s", err, out)
	}
	for _, under := range []string{"direct", "supervisor"} {
		t.Run(under, func(t *testing.T) {
			r := newSwapRig(t)
			if under == "supervisor" {
				detectSupervisor = func(context.Context, int) (*supervisor, error) {
					return &supervisor{Kind: "systemd", Unit: "zm.service", User: true}, nil
				}
				supervisorRestart = func(ctx context.Context, _ *supervisor) error {
					if pid := r.daemonPID(); pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGTERM)
						for i := 0; i < 500 && alive(pid); i++ {
							time.Sleep(10 * time.Millisecond)
						}
					}
					c := exec.Command(r.zm, "daemon")
					c.Env = r.env
					c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
					if err := c.Start(); err != nil {
						return err
					}
					go func() { _ = c.Wait() }() // a stopped daemon must not linger as a zombie
					return nil
				}
				supervisorProgram = func(context.Context, *supervisor) (string, error) { return r.zm, nil }
				supervisorMain = func(context.Context, *supervisor) (int, error) { return r.daemonPID(), nil }
				t.Cleanup(func() {
					detectSupervisor, supervisorRestart, supervisorProgram = detectSupervisorDefault, supervisorRestartDefault, supervisorProgramDefault
					supervisorMain = supervisorMainDefault
				})
			}
			cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1}}`
			if err := os.WriteFile(filepath.Join(r.zmHome(), "config.json"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, e, code := r.runBin(r.zm, "daemon", "stop"); code != 0 { // read the admission config
				t.Fatalf("stop: %s", e)
			}
			if _, e, code := r.runBin(r.zm, "daemon", "--detach"); code != 0 {
				t.Fatalf("start: %s", e)
			}
			out, e, code := r.runBin(r.zm, "run", "-d", "--agent", "shell", "/bin/sh", "-c", "echo before-swap; exec cat")
			if code != 0 {
				t.Fatalf("run: %s %s", out, e)
			}
			id := strings.TrimSpace(out)
			show := func() orchapi.Session {
				t.Helper()
				o, e, code := r.runBin(r.zm, "session", "show", id, "--json")
				var ss orchapi.Session
				if code != 0 || json.Unmarshal([]byte(o), &ss) != nil {
					t.Fatalf("session show: %d %s %s", code, o, e)
				}
				return ss
			}
			before := show()
			t.Cleanup(func() { _ = syscall.Kill(before.PID, syscall.SIGKILL) })

			dir, err := os.MkdirTemp(r.home, "stage")
			if err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(dir, "zm")
			copyExe(t, other, staged)
			err = runSwap(context.Background(), swapOpts{what: "upgrade", newBin: staged})
			if err == nil || !strings.Contains(err.Error(), "rolled back") {
				t.Fatalf("upgrade to the other major: %v", err)
			}
			if fileSum(t, r.zm) != r.sum {
				t.Fatal("zm is not the previous binary")
			}
			h := r.health(t)
			if h.LastSwap == nil || h.LastSwap.Result != "rolled_back" || !strings.Contains(h.LastSwap.Reason, "cannot reach their holders") {
				t.Fatalf("after the rollback: %+v", h.LastSwap)
			}
			got := show()
			if got.State == orchapi.StateEnded || got.PID != before.PID || got.Unreachable != "" || !alive(before.PID) {
				t.Fatalf("the session after the rollback: %+v", got)
			}
			if _, e, code := r.runBin(r.zm, "session", "input", id, "after-rollback"); code != 0 {
				t.Fatalf("input: %s", e)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				o, _, _ := r.runBin(r.zm, "session", "output", id)
				if strings.Contains(o, "before-swap") && strings.Contains(o, "after-rollback") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("output after the rollback: %q", o)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
