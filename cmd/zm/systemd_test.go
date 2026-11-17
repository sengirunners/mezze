package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// systemdRig is a throwaway systemd --user unit running this test binary as
// `zm daemon` on a temp home. The unit file lives in $XDG_RUNTIME_DIR, so
// neither $HOME nor anything else of the user's is touched.
type systemdRig struct {
	t         *testing.T
	root      string
	home      string
	zm        string
	unit      string
	unitPath  string
	killMode  string
	sessionPs []int
}

func systemctlUser(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// needSystemdUser skips without a systemd --user manager; CI sets
// ZM_REQUIRE_SYSTEMD=1, where a skip would be a false green.
func needSystemdUser(t *testing.T) string {
	t.Helper()
	why := ""
	dir := os.Getenv("XDG_RUNTIME_DIR")
	switch {
	case runtime.GOOS != "linux":
		why = "not Linux"
	case dir == "":
		why = "no XDG_RUNTIME_DIR"
	default:
		if out, err := systemctlUser("show-environment"); err != nil {
			why = fmt.Sprintf("no systemd --user manager: %v: %s", err, out)
		}
	}
	if why != "" {
		if os.Getenv("ZM_REQUIRE_SYSTEMD") == "1" {
			t.Fatalf("ZM_REQUIRE_SYSTEMD=1, but: %s", why)
		}
		t.Skip(why)
	}
	return dir
}

func newSystemdRig(t *testing.T, runtimeDir, killMode string, extra ...string) *systemdRig {
	t.Helper()
	root, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	r := &systemdRig{t: t, root: root, home: filepath.Join(root, "h"), zm: filepath.Join(root, "inst", "zm"),
		unit: "zmtest-" + hex.EncodeToString(b) + ".service", killMode: killMode}
	r.unitPath = filepath.Join(runtimeDir, "systemd", "user", r.unit)
	if err := os.MkdirAll(r.home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1}}`
	if err := os.WriteFile(filepath.Join(r.home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	copyExe(t, os.Args[0], r.zm)
	body := "[Service]\nExecStart=" + r.zm + " daemon\nRestart=always\nRestartSec=100ms\n" +
		"Environment=ZM_TEST_MAIN=1 ZM_HOME=" + r.home + " ZM_MACHINE=mini ZM_HUMAN=ana HOME=" + root + "\n"
	if killMode != "" {
		body += "KillMode=" + killMode + "\n"
	}
	body += strings.Join(extra, "")
	if err := os.MkdirAll(filepath.Dir(r.unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.unitPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = systemctlUser("stop", r.unit)
		for _, pid := range r.sessionPs {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = os.Remove(r.unitPath)
		_, _ = systemctlUser("daemon-reload")
		_, _ = systemctlUser("reset-failed", r.unit)
		_ = os.RemoveAll(root)
	})
	if out, err := systemctlUser("daemon-reload"); err != nil {
		t.Fatalf("daemon-reload: %v: %s", err, out)
	}
	if out, err := systemctlUser("start", r.unit); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	r.waitDaemon(0)
	return r
}

func (r *systemdRig) env() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZM_") || k == "HOME" || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+r.root, "ZM_HOME="+r.home, "ZM_MACHINE=mini", "ZM_HUMAN=ana", "ZM_TEST_MAIN=1", "ZM_NO_AUTOSTART=1")
}

func (r *systemdRig) run(args ...string) (string, int) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, r.zm, args...)
	c.Env, c.Dir = r.env(), r.root
	out, err := c.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(out)), ee.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("zm %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), 0
}

func (r *systemdRig) health() (api.Health, bool) {
	out, code := r.run("status", "--json")
	var h api.Health
	if code != 0 && code != 1 || json.Unmarshal([]byte(out), &h) != nil || h.PID == 0 {
		return api.Health{}, false
	}
	return h, true
}

// waitDaemon waits for a daemon other than not to answer.
func (r *systemdRig) waitDaemon(not int) int {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h, ok := r.health(); ok && h.PID != not {
			return h.PID
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, _ := systemctlUser("status", "--no-pager", r.unit)
	r.t.Fatalf("no daemon (other than %d) within 20 s:\n%s", not, status)
	return 0
}

func (r *systemdRig) spawn() (string, orchapi.Session) {
	r.t.Helper()
	out, code := r.run("run", "-d", "--agent", "shell", "/bin/sh", "-c", "echo before-restart; exec cat")
	if code != 0 {
		r.t.Fatalf("run: %d %s", code, out)
	}
	ss := r.session(out)
	r.sessionPs = append(r.sessionPs, ss.PID)
	return out, ss
}

func (r *systemdRig) session(id string) orchapi.Session {
	r.t.Helper()
	out, code := r.run("session", "show", id, "--json")
	var ss orchapi.Session
	if code != 0 || json.Unmarshal([]byte(out), &ss) != nil {
		r.t.Fatalf("session show %s: %d %s", id, code, out)
	}
	return ss
}

// liveProcess: running, not a zombie.
func liveProcess(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func (r *systemdRig) waitOutput(id, want string) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := r.run("session", "output", id)
		if strings.Contains(out, want) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("output never had %q: %q", want, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *systemdRig) stage() string {
	dir, err := os.MkdirTemp(r.root, "stage")
	if err != nil {
		r.t.Fatal(err)
	}
	p := filepath.Join(dir, "zm")
	copyExe(r.t, os.Args[0], p)
	return p
}

// TestSessionsUnderSystemd: what a real `systemctl restart` and `zm daemon
// upgrade` do to a live session, by the unit's KillMode.
func TestSessionsUnderSystemd(t *testing.T) {
	dir := needSystemdUser(t)
	t.Run("default KillMode", func(t *testing.T) {
		r := newSystemdRig(t, dir, "")
		id, ss := r.spawn()
		r.waitOutput(id, "before-restart")
		pid0 := r.waitDaemon(0)
		// The guard: the swap is refused, before anything moves.
		out, code := r.run("daemon", "upgrade", r.stage())
		t.Logf("upgrade under the default KillMode: exit %d: %s", code, out)
		if code == 0 || !strings.Contains(out, "KillMode=process") {
			t.Fatalf("upgrade with a live session under KillMode=control-group: exit %d: %s", code, out)
		}
		if p := r.waitDaemon(0); p != pid0 || !liveProcess(ss.PID) {
			t.Fatalf("a refused upgrade moved something: daemon %d -> %d, session alive=%v", pid0, p, liveProcess(ss.PID))
		}
		// Why: a restart zm cannot guard kills the holder with the daemon.
		if out, err := systemctlUser("restart", r.unit); err != nil {
			t.Fatalf("restart: %v: %s", err, out)
		}
		r.waitDaemon(pid0)
		deadline := time.Now().Add(5 * time.Second)
		for liveProcess(ss.PID) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("after systemctl restart under the default KillMode: session pid %d alive=%v", ss.PID, liveProcess(ss.PID))
		if liveProcess(ss.PID) {
			t.Fatal("KillMode=control-group left the session alive: the guard is no longer needed")
		}
		if got := r.session(id); got.State != orchapi.StateEnded {
			t.Fatalf("the killed session: %+v", got)
		}
		// --end-sessions is the way through it.
		_, ss2 := r.spawn()
		out, code = r.run("daemon", "upgrade", "--end-sessions", r.stage())
		if code != 0 || !strings.Contains(out, "zm daemon upgraded") || liveProcess(ss2.PID) {
			t.Fatalf("upgrade --end-sessions: %d alive=%v %s", code, liveProcess(ss2.PID), out)
		}
	})
	t.Run("KillMode unreadable", func(t *testing.T) {
		// The daemon cannot run systemctl: it fails closed, though this unit is fine.
		r := newSystemdRig(t, dir, "process", "Environment=PATH=/nonexistent\n")
		id, ss := r.spawn()
		r.waitOutput(id, "before-restart")
		pid0 := r.waitDaemon(0)
		out, code := r.run("daemon", "stop")
		t.Logf("stop with KillMode unreadable: exit %d: %s", code, out)
		if code == 0 || !strings.Contains(out, "cannot read its KillMode") {
			t.Fatalf("stop: exit %d: %s", code, out)
		}
		if p := r.waitDaemon(0); p != pid0 || !liveProcess(ss.PID) {
			t.Fatalf("a refused stop moved something: daemon %d -> %d, session alive=%v", pid0, p, liveProcess(ss.PID))
		}
	})
	t.Run("KillMode=process", func(t *testing.T) {
		r := newSystemdRig(t, dir, "process")
		id, ss := r.spawn()
		r.waitOutput(id, "before-restart")
		pid0 := r.waitDaemon(0)
		if out, code := r.run("daemon", "upgrade", r.stage()); code != 0 || !strings.Contains(out, "zm daemon upgraded") {
			t.Fatalf("upgrade: %d %s", code, out)
		}
		pid1 := r.waitDaemon(pid0)
		got := r.session(id)
		if !liveProcess(ss.PID) || got.PID != ss.PID || got.State == orchapi.StateEnded || got.Unreachable != "" {
			t.Fatalf("after the upgrade: alive=%v %+v", liveProcess(ss.PID), got)
		}
		if out, err := systemctlUser("restart", r.unit); err != nil {
			t.Fatalf("restart: %v: %s", err, out)
		}
		pid2 := r.waitDaemon(pid1)
		if got := r.session(id); !liveProcess(ss.PID) || got.State == orchapi.StateEnded {
			t.Fatalf("after systemctl restart: alive=%v %+v", liveProcess(ss.PID), got)
		}
		if err := syscall.Kill(pid2, syscall.SIGKILL); err != nil { // a crash: Restart=always brings the next one
			t.Fatal(err)
		}
		r.waitDaemon(pid2)
		if got := r.session(id); !liveProcess(ss.PID) || got.State == orchapi.StateEnded {
			t.Fatalf("after a daemon crash: alive=%v %+v", liveProcess(ss.PID), got)
		}
		if out, code := r.run("session", "input", id, "after-all-that"); code != 0 {
			t.Fatalf("input: %s", out)
		}
		r.waitOutput(id, "after-all-that")
		r.waitOutput(id, "before-restart")
		t.Logf("KillMode=process: session pid %d survived an upgrade, a systemctl restart and a daemon crash", ss.PID)
	})
}
