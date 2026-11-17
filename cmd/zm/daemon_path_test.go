package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

// withPATH is env with PATH replaced.
func withPATH(env []string, path string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+path)
}

// claudeScript answers `claude auth status`: ok, unless the daemon's
// environment has CLAUDE_TEST_AUTH=fail (exit 1) or =hang (past the 3 s probe;
// exec, so the probe's kill reaps it).
const claudeScript = "#!/bin/sh\ncase \"$CLAUDE_TEST_AUTH\" in\nfail) exit 1 ;;\nhang) exec sleep 30 ;;\nesac\nexit 0\n"

// swapPATHRig: a daemon from inst/zm whose PATH has a claude (a script
// that answers `claude auth status`) that the minimal PATH lacks, as the
// Mac's daemon had ~/.local/bin and its launchd updater did not.
func swapPATHRig(t *testing.T) (z *zeroEnv, zm, minimal string) {
	z = newZeroEnv(t)
	agents := filepath.Join(z.home, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	zm = filepath.Join(z.home, "inst", "zm")
	copyExe(t, os.Args[0], zm)
	minimal = "/usr/bin:/bin"
	full := z.env
	z.env = withPATH(full, agents+":"+minimal)
	if _, e, code := z.runBin(zm, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	if !claudeAvailable(t, z) {
		t.Fatal("the old daemon does not find claude on its PATH")
	}
	z.env = withPATH(full, minimal) // the swapper's, from here
	return z, zm, minimal
}

func claudeAvailable(t *testing.T, z *zeroEnv) bool {
	t.Helper()
	ta, err := orchclient.TaskAgents(context.Background(), client.NewUnix(filepath.Join(z.zmHome(), "zm.sock")))
	if err != nil {
		t.Fatalf("task agents: %v", err)
	}
	for _, a := range ta.Agents {
		if a.Agent == "claude" {
			return a.Available
		}
	}
	return false
}

// TestSwapKeepsDaemonPATH: a swapper with a minimal PATH (the
// Mac's launchd updater) restarts a daemon that found claude on a fuller
// PATH; the new daemon still finds it.
func TestSwapKeepsDaemonPATH(t *testing.T) {
	z, zm, _ := swapPATHRig(t)
	pid0 := z.daemonPID()
	out, e, code := z.runBin(zm, "daemon", "restart")
	if code != 0 || !strings.Contains(out, "zm daemon restarted") {
		t.Fatalf("restart: %d\n%s\n%s", code, out, e)
	}
	if p := z.daemonPID(); p == 0 || p == pid0 {
		t.Fatalf("not restarted: %d -> %d", pid0, p)
	}
	if !claudeAvailable(t, z) {
		t.Fatal("the swapped-in daemon lost claude: it took the swapper's PATH")
	}
}

// TestSwapRollsBackLostAgent: when the new daemon cannot start
// an agent the old one could (here its PATH record is gone), the upgrade's
// verify calls it a regression and rolls back.
func TestSwapRollsBackLostAgent(t *testing.T) {
	z, zm, _ := swapPATHRig(t)
	if err := os.Remove(filepath.Join(z.zmHome(), FilePath)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	stage := filepath.Join(z.home, "stage", "zm")
	copyExe(t, os.Args[0], stage)
	out, e, code := z.runBin(zm, "daemon", "upgrade", stage)
	if code != 1 || !strings.Contains(e, "agent claude: served before, now unavailable") || !strings.Contains(e, "rolled back to") {
		t.Fatalf("upgrade that loses claude: %d\n%s\n%s", code, out, e)
	}
	if z.daemonPID() == 0 {
		t.Fatal("no daemon runs after the rollback")
	}
}

// TestSwapVerifiesWithAuthFlap: claude still resolves
// after the swap but its auth probe fails or hangs. That is login state,
// not the build: the restart verifies, no rollback.
func TestSwapVerifiesWithAuthFlap(t *testing.T) {
	for _, mode := range []string{"fail", "hang"} {
		t.Run(mode, func(t *testing.T) {
			z, zm, _ := swapPATHRig(t)
			z.env = append(z.env, "CLAUDE_TEST_AUTH="+mode) // the new daemon inherits it
			out, e, code := z.runBin(zm, "daemon", "restart")
			if code != 0 || !strings.Contains(out, "zm daemon restarted") {
				t.Fatalf("restart with claude's auth %s: %d\n%s\n%s", mode, code, out, e)
			}
			if claudeAvailable(t, z) {
				t.Fatal("claude's auth probe passed: the rig does not flap")
			}
		})
	}
}

// TestRestartReportsLostAgent: restart has no previous binary;
// a new daemon that cannot find claude is reported and restart exits 1.
func TestRestartReportsLostAgent(t *testing.T) {
	z, zm, _ := swapPATHRig(t)
	if err := os.Remove(filepath.Join(z.zmHome(), FilePath)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out, e, code := z.runBin(zm, "daemon", "restart")
	if code != 1 || !strings.Contains(e, "agent claude: served before, now unavailable") || !strings.Contains(e, "no previous binary to roll back to") {
		t.Fatalf("restart that loses claude: %d\n%s\n%s", code, out, e)
	}
	if z.daemonPID() == 0 {
		t.Fatal("no daemon runs after the failed restart")
	}
}

func mkdirMode(t *testing.T, d string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil { // past the umask
		t.Fatal(err)
	}
	return d
}

func TestMergePATH(t *testing.T) {
	d := t.TempDir()
	ok := mkdirMode(t, filepath.Join(d, "ok"), 0o755)
	world := mkdirMode(t, filepath.Join(d, "world"), 0o777)
	sticky := mkdirMode(t, filepath.Join(d, "sticky"), 0o1777)
	group := mkdirMode(t, filepath.Join(d, "group"), 0o775)
	got, added := mergePATH("/usr/bin:/bin", strings.Join([]string{world, ok, sticky, group, "/bin", "/no/such/dir", "relative"}, ":"))
	if got != "/usr/bin:/bin:"+ok || len(added) != 1 || added[0] != ok {
		t.Fatalf("merge: %q %v", got, added)
	}
	if got, added := mergePATH("/usr/bin", ""); got != "/usr/bin" || added != nil {
		t.Fatalf("no record: %q %v", got, added)
	}
}

func TestPATHCap(t *testing.T) {
	d := t.TempDir()
	var many []string
	for i := range 100 {
		many = append(many, mkdirMode(t, filepath.Join(d, fmt.Sprint(i)), 0o755))
	}
	got, added := mergePATH("/usr/bin:/bin", strings.Join(many, ":"))
	if n := len(filepath.SplitList(got)); n != pathMaxEntries || len(added) != pathMaxEntries-2 {
		t.Fatalf("merge kept %d entries (%d added), want %d", n, len(added), pathMaxEntries)
	}
	if n := len(filepath.SplitList(capPATH(strings.Join(many, ":")))); n != pathMaxEntries {
		t.Fatalf("cap kept %d entries", n)
	}
	long := "/" + strings.Repeat("x", 999) // 1000 bytes each
	var big []string
	for range 20 {
		big = append(big, long)
	}
	if c := capPATH(strings.Join(big, ":")); len(c) > pathMaxBytes || len(filepath.SplitList(c)) != 8 {
		t.Fatalf("byte cap: %d bytes, %d entries", len(c), len(filepath.SplitList(c)))
	}
}

// TestPATHRecordTrust: a record group or others can write, or that is not
// a regular file, adds nothing, and the daemon's own record replaces it
// (the user's, 0600, through a fresh temp file).
func TestPATHRecordTrust(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root: ownership checks differ")
	}
	home := t.TempDir()
	extra := mkdirMode(t, filepath.Join(home, "extra"), 0o755)
	file := filepath.Join(home, FilePath)
	t.Setenv("PATH", "/usr/bin:/bin")
	log := slog.New(slog.DiscardHandler)

	for _, mode := range []os.FileMode{0o666, 0o620, 0o602} {
		if err := os.WriteFile(file, []byte(extra+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readPATH(file); err == nil {
			t.Fatalf("mode %o: read", mode)
		}
		keepPATH(home, log)
		if p := os.Getenv("PATH"); p != "/usr/bin:/bin" {
			t.Fatalf("mode %o: PATH %q", mode, p)
		}
		fi, err := os.Lstat(file)
		if err != nil || fi.Mode() != 0o600 {
			t.Fatalf("mode %o: rewritten as %v %v", mode, fi.Mode(), err)
		}
	}

	// A symlink to a good file is not followed.
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte(extra+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(file)
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := readPATH(file); err == nil {
		t.Fatal("symlink: read")
	}

	// The good case, and no temp file left behind.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(extra+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keepPATH(home, log)
	if p := os.Getenv("PATH"); p != "/usr/bin:/bin:"+extra {
		t.Fatalf("good record: PATH %q", p)
	}
	if m, _ := filepath.Glob(filepath.Join(home, "*.tmp")); len(m) > 0 {
		t.Fatalf("temp files left: %v", m)
	}
}
