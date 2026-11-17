package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/store"
)

func TestSSHExitCode(t *testing.T) {
	cases := []struct {
		name string
		code int
		said string
		want int
	}{
		{"the session ended", 0, "", 0},
		{"the far zm's own codes pass", 2, "", 2},
		{"no zm on the box", 127, "", exitNoZM},
		{"not executable", 126, "", exitNoZM},
		{"keys", 255, "user@mini: Permission denied (publickey).\n", exitRefused},
		{"a new host key", 255, "Host key verification failed.\n", exitRefused},
		{"host key changed", 255, "@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @\n", exitRefused},
		{"bad name", 255, "ssh: Could not resolve hostname mini: nodename nor servname provided, or not known\n", exitUnreachable},
		{"timed out", 255, "ssh: connect to host 10.0.0.1 port 22: Operation timed out\n", exitUnreachable},
		{"linux timed out", 255, "ssh: connect to host mini port 22: Connection timed out\n", exitUnreachable},
		{"no sshd", 255, "ssh: connect to host mini port 22: Connection refused\n", exitUnreachable},
		{"banner", 255, "Connection timed out during banner exchange\n", exitUnreachable},
		{"no ssh here", 255, `exec: "ssh": executable file not found in $PATH`, exitUnreachable},
		{"the link dropped", 255, "client_loop: send disconnect: Broken pipe\n", 3},
		{"keepalive gave up", 255, "Timeout, server mini not responding.\n", 3},
		{"closed by the far side", 255, "Connection to mini closed by remote host.\n", 3},
		{"killed", -1, "", 3},
		{"anything else", 42, "", 1},
	}
	for _, c := range cases {
		if got := sshExitCode(c.code, c.said); got != c.want {
			t.Errorf("%s: code %d %q = %d, want %d", c.name, c.code, c.said, got, c.want)
		}
	}
}

func TestSSHTargetRefusesWhatCouldBeAnOption(t *testing.T) {
	for _, bad := range []string{"", "-oProxyCommand=sh", "--", "a b", "a;b", "a%h", "a/b", "$(x)", ".hidden", "a\nb", "user@host"} {
		if checkSSHTarget(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"mini", "ivo-mac", "box.tail1234.ts.net", "zm_01"} {
		if err := checkSSHTarget(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

func TestAttachSSHArgs(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "zm") // macOS's own temp dir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg := &config.Config{Home: home}
	args := attachSSHArgs(cfg, "mini", "abcd1234", false, true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-tt", "-e none", "BatchMode=yes", "ControlMaster=auto", "ForwardAgent=no", "ForwardX11=no", "ConnectTimeout=" + embedConnect} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	i := slices.Index(args, "--")
	if i < 0 || args[i+1] != "mini" || i+3 != len(args) {
		t.Fatalf("the machine must follow -- and be followed only by the command: %q", args)
	}
	if far := args[i+2]; !strings.Contains(far, "zm attach --embed") || !strings.Contains(far, envReplyQuiet) {
		t.Errorf("far command: %s", far)
	}
	// A person can answer prompts: no BatchMode, and a plain attach (ctrl-] detaches).
	args = attachSSHArgs(cfg, "mini", "abcd1234", true, false)
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "BatchMode") || strings.Contains(joined, "--embed") || !strings.Contains(joined, "--read-only") {
		t.Errorf("interactive: %s", joined)
	}
	// A home too long for a socket path: no sharing, still an attach.
	long := &config.Config{Home: filepath.Join(t.TempDir(), strings.Repeat("x", 90))}
	if strings.Contains(strings.Join(sshArgs(long, true), " "), "ControlPath") {
		t.Error("control path beyond sun_path")
	}
}

func TestReplyWindowQuietFromEnv(t *testing.T) {
	t.Setenv(envReplyQuiet, "1000")
	now := time.Unix(100, 0)
	w := newReplyWindow(func() time.Time { return now })
	w.output()
	now = now.Add(900 * time.Millisecond)
	if !w.open() {
		t.Error("closed inside the widened quiet")
	}
	now = now.Add(200 * time.Millisecond)
	if w.open() {
		t.Error("open after the widened quiet")
	}
}

// remoteRig is two throwaway daemons, "near" and "far", and an ssh on PATH
// that runs the command locally with the target's ZM_HOME, so a remote attach
// runs whole with no network. It never touches a real ssh, ~/.ssh or ~/.zm.
type remoteRig struct {
	t    *testing.T
	root string
	bin  string
}

const sshShim = `#!/bin/sh
# Fake ssh (remote_test.go): logs its argv, takes the target after --, and runs
# the command as that machine would: its ZM_HOME, its ZM_MACHINE.
case " $* " in
*" -G "*)
  for a; do name=$a; done
  printf 'hostname %s\nport 22\nuserknownhostsfile none\nglobalknownhostsfile none\n' "$name"
  echo "debug1: /etc/ssh/ssh_config line 1: Applying options for *" >&2
  if grep -qx "$name" "$RIG/hosts" 2>/dev/null; then
    echo "debug1: /home/ana/.ssh/config line 3: Applying options for $name" >&2
  fi
  exit 0 ;;
esac
printf '%s\n' "$*" >> "$RIG/ssh.log"
while [ $# -gt 0 ]; do
  a=$1; shift
  [ "$a" = "--" ] && break
done
target=$1; shift
mode=$(cat "$RIG/$target.mode" 2>/dev/null)
case $mode in
refused) echo "$USER@$target: Permission denied (publickey)." >&2; exit 255 ;;
unreachable) echo "ssh: connect to host $target port 22: Operation timed out" >&2; exit 255 ;;
dropped) echo "client_loop: send disconnect: Broken pipe" >&2; exit 255 ;;
nozm) export PATH=/usr/bin:/bin HOME=/nonexistent ;;
esac
export ZM_HOME="$RIG/$target" ZM_MACHINE="$target"
exec sh -c "$1"
`

func newRemoteRig(t *testing.T) *remoteRig {
	t.Helper()
	root, err := os.MkdirTemp("", "zm") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	r := &remoteRig{t: t, root: root, bin: filepath.Join(root, "bin")}
	t.Cleanup(func() {
		for _, m := range []string{"near", "far"} {
			r.stopDaemon(m)
		}
		os.RemoveAll(root)
	})
	if err := os.MkdirAll(r.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh", sshShim)
	if err := os.WriteFile(filepath.Join(root, "hosts"), []byte("near\nfar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("zm", "#!/bin/sh\nZM_TEST_MAIN=1 exec "+os.Args[0]+` "$@"`+"\n")
	for _, m := range []string{"near", "far"} {
		if err := os.MkdirAll(filepath.Join(root, m), 0o700); err != nil {
			t.Fatal(err)
		}
		// Whatever this machine's swap and memory look like now.
		cfg := `{"admission": {"max_managed_sessions": 100, "max_swap_used_mb": 1000000, "min_memory_free_percent": 0, "min_disk_free_mb": 1}}`
		if err := os.WriteFile(filepath.Join(root, m, "config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, code := r.zm(m, "daemon", "--detach"); code != 0 {
			t.Fatalf("daemon on %s: %d %s", m, code, out)
		}
		for i := 0; ; i++ {
			if _, code := r.zm(m, "status"); code == 0 {
				break
			}
			if i == 100 {
				t.Fatalf("daemon on %s never answered", m)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return r
}

func (r *remoteRig) env(machine string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "ZM_") && k != "PATH" && k != "HOME" {
			env = append(env, kv)
		}
	}
	return append(env, "RIG="+r.root, "HOME="+r.root, "PATH="+r.bin+":/usr/bin:/bin", "ZM_HOME="+filepath.Join(r.root, machine),
		"ZM_MACHINE="+machine, "ZM_HUMAN=ana", "ZM_TEST_MAIN=1", "ZM_NO_AUTOSTART=1")
}

// zm runs zm as `machine` and returns stdout and stderr together, and the exit code.
func (r *remoteRig) zm(machine string, args ...string) (string, int) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Args[0], args...)
	c.Env = r.env(machine)
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("zm %v: %v", args, err)
	}
	return string(out), 0
}

func (r *remoteRig) stopDaemon(machine string) {
	c := exec.Command(os.Args[0], "daemon", "stop", "--force")
	c.Env = r.env(machine)
	_ = c.Run()
	b, err := os.ReadFile(filepath.Join(r.root, machine, "daemon.lock"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err == nil && pid > 0 {
		for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// presence tells `on`'s daemon about a session elsewhere, as a team authority
// knows it from a replica's heartbeat.
func (r *remoteRig) presence(on, address string, managed bool, state orchapi.SessionState) {
	r.t.Helper()
	ctx := context.Background()
	st, err := storetest.Open(ctx, filepath.Join(r.root, on))
	if err != nil {
		r.t.Fatal(err)
	}
	defer st.Close()
	a, err := api.ParseAddress(address)
	if err != nil {
		r.t.Fatal(err)
	}
	now := store.Millis(time.Now())
	if _, err := st.Registry().ExecContext(ctx, `INSERT INTO presence (address, machine, project, area, identity, agent, title, state,
		note, managed, work, started, seen, ended, activity, activity_at, term_title, reach, shared, in_use)
		VALUES (?,?,?,'','ana','cat','',?,'',?,'',?,?,NULL,'',NULL,'','project',0,'')`,
		address, a.Machine, "none", string(state), boolToInt(managed), now, now); err != nil {
		r.t.Fatal(err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// spawnCat starts `cat` on machine: it echoes what is typed.
func (r *remoteRig) spawnCat(machine string) (address string) {
	r.t.Helper()
	out, code := r.zm(machine, "run", "-d", "--", "/bin/cat")
	if code != 0 {
		r.t.Fatalf("spawn on %s: %d %s", machine, code, out)
	}
	address = strings.TrimSpace(out)
	r.t.Cleanup(func() { r.zm(machine, "kill", "--force", address) })
	return address
}

func (r *remoteRig) sshLog() string {
	b, _ := os.ReadFile(filepath.Join(r.root, "ssh.log"))
	return string(b)
}

func (r *remoteRig) mode(machine, mode string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, machine+".mode"), []byte(mode), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// attached is a `zm attach` in a pty, as a terminal surface runs it.
type attached struct {
	t   *testing.T
	cmd *exec.Cmd
	f   *os.File
	out chan string
}

func (r *remoteRig) attach(machine string, args ...string) *attached {
	r.t.Helper()
	c := exec.Command(os.Args[0], append([]string{"attach"}, args...)...)
	c.Env = r.env(machine)
	f, err := pty.StartWithSize(c, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		r.t.Fatal(err)
	}
	a := &attached{t: r.t, cmd: c, f: f, out: make(chan string, 256)}
	go func() {
		br := bufio.NewReader(f)
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				a.out <- string(buf[:n])
			}
			if err != nil {
				close(a.out)
				return
			}
		}
	}()
	r.t.Cleanup(func() { f.Close(); _ = c.Process.Kill(); _ = c.Wait() })
	return a
}

// expect waits until the output has shown want.
func (a *attached) expect(want string) {
	a.t.Helper()
	seen := ""
	deadline := time.After(15 * time.Second)
	for {
		select {
		case s, ok := <-a.out:
			seen += s
			if strings.Contains(seen, want) {
				return
			}
			if !ok {
				a.t.Fatalf("attach ended before %q: %q", want, seen)
			}
		case <-deadline:
			a.t.Fatalf("no %q in %q", want, seen)
		}
	}
}

// exit waits for the attach to end and returns its code.
func (a *attached) exit() int {
	a.t.Helper()
	go func() {
		for range a.out {
		}
	}()
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	case <-time.After(15 * time.Second):
		a.t.Fatal("attach did not exit")
		return -1
	}
}

// Both directions, whole: a session on one daemon, attached and typed into
// from the other, ending as the host app expects.
func TestRemoteAttachBothDirections(t *testing.T) {
	r := newRemoteRig(t)
	for _, dir := range [][2]string{{"near", "far"}, {"far", "near"}} {
		from, to := dir[0], dir[1]
		address := r.spawnCat(to)
		r.presence(from, address, true, orchapi.StateRunning)
		a := r.attach(from, "--embed", address)
		// The reply window is still open: a person's keys pass it.
		if _, err := a.f.WriteString("hello from " + from + "\n"); err != nil {
			t.Fatal(err)
		}
		a.expect("hello from " + from)
		if out, code := r.zm(to, "kill", "--force", address); code != 0 {
			t.Fatalf("kill: %d %s", code, out)
		}
		if code := a.exit(); code != 0 {
			t.Errorf("%s to %s: exit %d after the session ended, want 0", from, to, code)
		}
	}
	log := r.sshLog()
	for _, want := range []string{"-tt", "-e none", "BatchMode=yes", "-- far", "-- near", "ForwardAgent=no"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in ssh's argv:\n%s", want, log)
		}
	}
}

func TestRemoteAttachOnlyWhatTheDaemonShows(t *testing.T) {
	r := newRemoteRig(t)
	if out, code := r.zm("near", "attach", "--embed", "far/abcd1234"); code != 2 {
		t.Errorf("a session the daemon does not list: %d %s", code, out)
	}
	if log := r.sshLog(); log != "" {
		t.Errorf("ssh ran for a session nobody can see:\n%s", log)
	}
	r.presence("near", "far/aaaa1111", false, orchapi.StateRunning)
	if out, code := r.zm("near", "attach", "--embed", "far/aaaa1111"); code != 2 || !strings.Contains(out, "its own terminal") {
		t.Errorf("unmanaged: %d %s", code, out)
	}
	r.presence("near", "far/bbbb2222", true, orchapi.StateEnded)
	if out, code := r.zm("near", "attach", "--embed", "far/bbbb2222"); code != 0 {
		t.Errorf("ended: %d %s", code, out)
	}
	if log := r.sshLog(); log != "" {
		t.Errorf("ssh ran with nothing to attach:\n%s", log)
	}
}

func TestRemoteAttachSaysWhy(t *testing.T) {
	r := newRemoteRig(t)
	address := r.spawnCat("far")
	r.presence("near", address, true, orchapi.StateRunning)
	cases := []struct {
		mode string
		want int
		line string
	}{
		{"refused", exitRefused, "zm ssh far"},
		{"unreachable", exitUnreachable, "no answer from far"},
		{"nozm", exitNoZM, "zm is not on its PATH"},
		{"dropped", 3, ""},
	}
	why := filepath.Join(r.root, "why")
	for _, c := range cases {
		r.mode("far", c.mode)
		out, code := r.zm("near", "attach", "--embed", "--why-file", why, address)
		if code != c.want || !strings.Contains(out, c.line) {
			t.Errorf("%s: exit %d, want %d: %s", c.mode, code, c.want, out)
		}
	}
	// The last one failed with no reason of zm's: ssh's own line is for the card.
	if b, _ := os.ReadFile(why); !strings.Contains(string(b), "Broken pipe") {
		t.Errorf("why file: %q", b)
	}
	// A person gets the reason too, with the plain exit code.
	r.mode("far", "refused")
	if out, code := r.zm("near", "attach", address); code != 1 || !strings.Contains(out, "zm ssh far") {
		t.Errorf("no --embed: %d %s", code, out)
	}
}

// A machine's name comes from a teammate. One this Mac does not know gets no
// ssh at all: not a connection, and not the words to go and answer its prompts.
func TestUnknownNameNeverReachesSSH(t *testing.T) {
	r := newRemoteRig(t)
	r.presence("near", "evil.example.com/abcd1234", true, orchapi.StateRunning)
	why := filepath.Join(r.root, "why")
	out, code := r.zm("near", "attach", "--embed", "--why-file", why, "evil.example.com/abcd1234")
	if code != exitUnknownHost || !strings.Contains(out, "comes from a teammate") {
		t.Errorf("--embed: %d %s", code, out)
	}
	if !strings.Contains(out, "Match block") || !strings.Contains(out, "by hand") || strings.Contains(out, "answer") {
		t.Errorf("the way past it is by hand, and no invitation to answer prompts: %s", out)
	}
	if b, _ := os.ReadFile(why); !strings.Contains(string(b), "comes from a teammate") {
		t.Errorf("why file: %q", b)
	}
	if out, code := r.zm("near", "attach", "evil.example.com/abcd1234"); code != exitUnknownHost || !strings.Contains(out, "comes from a teammate") {
		t.Errorf("a person's attach by address is still a name from presence: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"attach", "--embed", "--on", "evil.example.com", "abcd1234"},
		{"run", "--on", "evil.example.com", "-d", "--", "/bin/cat"},
		{"ssh", "evil.example.com"}, // no terminal: a script, or a host app
		{"ssh", "--known-only", "evil.example.com"},
	} {
		if out, code := r.zm("near", args...); code != exitUnknownHost {
			t.Errorf("%v: %d %s", args, code, out)
		}
	}
	if log := r.sshLog(); log != "" {
		t.Errorf("ssh ran for a name this Mac does not know:\n%s", log)
	}
}

// A person at a terminal chose the host: a first connection goes ahead. With
// --known-only (an embedding client, with a name from presence) it does not.
func TestSSHAtATerminal(t *testing.T) {
	r := newRemoteRig(t)
	run := func(args ...string) int {
		c := exec.Command(os.Args[0], append([]string{"ssh"}, args...)...)
		c.Env = r.env("near")
		f, err := pty.Start(c)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		go func() { _, _ = io.Copy(io.Discard, f) }()
		var ee *exec.ExitError
		if err := c.Wait(); errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	}
	if code := run("--known-only", "newbox.example.com", "true"); code != exitUnknownHost {
		t.Errorf("--known-only: exit %d", code)
	}
	if log := r.sshLog(); log != "" {
		t.Errorf("ssh ran: %s", log)
	}
	run("newbox.example.com", "true")
	log := r.sshLog()
	if !strings.Contains(log, "-- newbox.example.com") {
		t.Errorf("a person's first connection did not go ahead: %q", log)
	}
	// Whatever the user's config says, nothing is forwarded to the box.
	for _, want := range []string{"ForwardAgent=no", "ForwardX11=no"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %s in %s", want, log)
		}
	}
}

// The real tools on a private config: `ssh -G` and `ssh-keygen -F` connect to
// nothing, and the shim here refuses anything but -G.
func TestSSHKnownWithRealTools(t *testing.T) {
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh here")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here")
	}
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := write("config", "Include "+write("included", "Host viainclude\n  User z\n", 0o600)+`
Host *
  ServerAliveInterval 30
Host mini
  HostName 10.9.8.7
Host *.local dev-* node?
  User me
Match host matched.example.com
  User me
Match host keyedbymatch.example.com
  User me
Host *
  UserKnownHostsFile `+filepath.Join(dir, "known")+`
`, 0o600)
	out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, "k")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "k.pub"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Join(strings.Fields(string(pub))[:2], " ")
	write("known", "connected.example.com "+key+"\nkeyed.local "+key+"\nkeyedbymatch.example.com "+key+"\n", 0o600)
	write("ssh", "#!/bin/sh\ncase \" $* \" in *\" -G \"*) exec "+realSSH+" -F "+cfg+" \"$@\" ;; esac\nexit 99\n", 0o755)
	t.Setenv("PATH", dir+":"+filepath.Dir(keygen)+":/usr/bin:/bin")
	cases := map[string]bool{
		"mini":                     true,  // a Host entry
		"viainclude":               true,  // through an Include
		"connected.example.com":    true,  // no entry, a key in known_hosts
		"keyed.local":              true,  // a wildcard, and a key: known by the key
		"keyedbymatch.example.com": true,  // a Match block, and a key
		"evil.example.com":         false, // only `Host *`
		"anything.local":           false, // a wildcard fits any name a teammate picks
		"dev-box":                  false,
		"node1":                    false,
		"matched.example.com":      false, // a Match block alone
	}
	for name, want := range cases {
		if got := sshKnown(name); got != want {
			t.Errorf("sshKnown(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestKnownHostsQuery(t *testing.T) {
	t.Setenv("HOME", "/home/ana")
	key, files := knownHostsQuery("hostname 10.9.8.7\nport 2222\nglobalknownhostsfile /etc/ssh/ssh_known_hosts\n")
	if key != "[10.9.8.7]:2222" || !slices.Equal(files, []string{"/home/ana/.ssh/known_hosts", "/home/ana/.ssh/known_hosts2", "/etc/ssh/ssh_known_hosts"}) {
		t.Errorf("%s %v", key, files)
	}
	key, files = knownHostsQuery("hostname mini\nport 22\nhostkeyalias shared\nuserknownhostsfile none\n")
	if key != "shared" || len(files) != 0 {
		t.Errorf("%s %v", key, files)
	}
}

func TestLastLine(t *testing.T) {
	if got := sshLastLine("first\nchannel 0: open failed\r\n\n", 255, "mini"); got != "channel 0: open failed" {
		t.Errorf("%q", got)
	}
	if got := sshLastLine("\x1b[31mred\x1b[0m\n", 255, "mini"); strings.ContainsRune(got, 0x1b) {
		t.Errorf("control characters: %q", got)
	}
	// Bidi overrides and isolates, marks, zero width, C1, a line separator.
	if got := sshLastLine("a\u202eb\u2066c\u200fd\u200be\u0085f\u2028g\ufeffh\n", 255, "mini"); got != "abcdefgh" {
		t.Errorf("format characters: %q", got)
	}
	if got := sshLastLine("\u202e\u200f\n", 255, "mini"); !strings.Contains(got, "without saying why") {
		t.Errorf("a line of nothing but format characters: %q", got)
	}
	if got := sshLastLine(strings.Repeat("x", 500), 255, "mini"); len(got) != 300 {
		t.Errorf("not clipped: %d", len(got))
	}
	if got := sshLastLine("", 3, "mini"); !strings.Contains(got, "daemon") {
		t.Errorf("%q", got)
	}
	if got := sshLastLine("", 255, "mini"); !strings.Contains(got, "without saying why") {
		t.Errorf("%q", got)
	}
}

func TestRunOn(t *testing.T) {
	r := newRemoteRig(t)
	out, code := r.zm("near", "run", "--on", "far", "-d", "--json", "--title", "far shell", "--", "/bin/cat")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	var ss orchapi.Session
	if err := json.Unmarshal([]byte(out), &ss); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { r.zm("far", "kill", "--force", ss.Address) })
	if ss.Machine != "far" || ss.Title != "far shell" || !ss.Managed {
		t.Errorf("%+v", ss)
	}
	if ls, _ := r.zm("far", "ls"); !strings.Contains(ls, ss.ID) {
		t.Errorf("not on the far daemon: %s", ls)
	}
	if ls, _ := r.zm("near", "ls"); strings.Contains(ls, ss.ID) {
		t.Errorf("on the near daemon: %s", ls)
	}
	// Started this moment: the far machine's heartbeat has not listed it here yet.
	if out, code := r.zm("near", "attach", "--embed", ss.Address); code != 2 {
		t.Errorf("not listed yet: %d %s", code, out)
	}
	a := r.attach("near", "--embed", "--on", "far", ss.ID)
	if _, err := a.f.WriteString("typed\n"); err != nil {
		t.Fatal(err)
	}
	a.expect("typed")
	if out, code := r.zm("near", "attach", "--embed", "--on", "far", "abcd0000"); code != 2 {
		t.Errorf("--on asks the far daemon, which has no such session: %d %s", code, out)
	}
	before := r.sshLog()
	if out, code := r.zm("near", "attach", "--embed", "--on", "far", "other/abcd1234"); code != 2 || !strings.Contains(out, "other") {
		t.Errorf("--on with another machine's address: %d %s", code, out)
	}
	if r.sshLog() != before {
		t.Error("ssh ran for a mismatched --on")
	}
	if out, code := r.zm("near", "attach", "--embed", "--on", "far", "far/abcd0000"); code != 2 || strings.Contains(out, "is a session on") {
		t.Errorf("--on with its own address: %d %s", code, out)
	}
	// The far zm's own refusal reaches this end, in its words.
	if out, code := r.zm("near", "run", "--on", "far", "-d", "--project", "nope", "--", "/bin/cat"); code != 1 || !strings.Contains(out, "nope") {
		t.Errorf("far refusal: %d %s", code, out)
	}
	r.mode("far", "refused")
	if out, code := r.zm("near", "run", "--on", "far", "-d", "--", "/bin/cat"); code != exitRefused || !strings.Contains(out, "zm ssh far") {
		t.Errorf("refused: %d %s", code, out)
	}
}

// upperEcho is the session program of TestTypingLatency: no terminal echo,
// each key read and written back upper-cased, so what comes back has been to
// the program (a shell's readline, an agent's prompt) and back.
const upperEcho = `import os, sys, termios, tty
tty.setraw(0)
while True:
    b = os.read(0, 64)
    if not b:
        break
    os.write(1, b.upper())
`

// TestTypingLatency prints keystroke-to-echo times, attached locally and
// through the fake ssh (ZM_LATENCY=1; it measures, it does not judge). It
// needs python3 on the session's PATH.
func TestTypingLatency(t *testing.T) {
	if os.Getenv("ZM_LATENCY") == "" {
		t.Skip("set ZM_LATENCY=1")
	}
	r := newRemoteRig(t)
	out, code := r.zm("far", "run", "-d", "--", "/usr/bin/python3", "-c", upperEcho)
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	address := strings.TrimSpace(out)
	t.Cleanup(func() { r.zm("far", "kill", "--force", address) })
	r.presence("near", address, true, orchapi.StateRunning)
	run := func(name, machine string) {
		a := r.attach(machine, "--embed", address)
		time.Sleep(2 * time.Second) // python starts; the reply window closes
		for len(a.out) > 0 {
			<-a.out
		}
		var ms []float64
		for i := 0; i < 300; i++ {
			key := string(rune('a' + i%26))
			t0 := time.Now()
			if _, err := io.WriteString(a.f, key); err != nil {
				t.Fatal(err)
			}
			for s := ""; !strings.Contains(s, strings.ToUpper(key)); {
				s += <-a.out
			}
			ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
			time.Sleep(5 * time.Millisecond)
		}
		slices.Sort(ms)
		t.Logf("%s: p50 %.2f ms, p95 %.2f ms, max %.2f ms", name, ms[len(ms)/2], ms[len(ms)*95/100], ms[len(ms)-1])
	}
	run("local attach", "far")
	run("through the fake ssh", "near")
}
