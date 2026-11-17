package team

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// The real Mezze app runtime, with a fake node and vp: the first
// ensure checks out the pinned main commit, installs, starts `serve --lan`
// in its own process group and mints with the server's own CLI; the next
// one reuses the server. A pin that is not on origin/main is refused and
// leaves the running server up; a moved pin is prepared before the old
// server stops; a changed port or home replaces zm's server rather than
// orphaning it; a port another server answers is refused; a failed mint's
// stdout (the credential) stays out of the error.
func TestT3HostEnsure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old, oldFree := t3StartWait, t3DiskFree
	t3StartWait = 10 * time.Second
	t3DiskFree = func(string) (int64, error) { return 1 << 20, nil } // whatever the test volume has
	t.Cleanup(func() { t3StartWait, t3DiskFree = old, oldFree })
	dir := t.TempDir()
	gitIn := func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// origin: two commits on main; the checkout starts at the first.
	origin := filepath.Join(dir, "origin")
	os.MkdirAll(filepath.Join(origin, "apps", "server", "src"), 0o755)
	gitIn(dir, "init", "-q", "-b", "main", origin)
	os.WriteFile(filepath.Join(origin, "apps", "server", "src", "bin.ts"), []byte("// v1\n"), 0o644)
	gitIn(origin, "add", ".")
	gitIn(origin, "commit", "-q", "-m", "one")
	first := gitIn(origin, "rev-parse", "HEAD")
	checkout := filepath.Join(dir, "t3code")
	gitIn(dir, "clone", "-q", origin, checkout)
	os.WriteFile(filepath.Join(origin, "apps", "server", "src", "bin.ts"), []byte("// v2\n"), 0o644)
	gitIn(origin, "commit", "-q", "-am", "two")
	pin := gitIn(origin, "rev-parse", "HEAD")

	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	port, port2 := freePort(), freePort()

	// The fake node: serve records itself (served-<port>) and stays; auth
	// pairing create prints a credential (or, with mint-fails, prints it
	// and fails). vp notes whether a server is up while it installs.
	logs := filepath.Join(dir, "calls")
	node := filepath.Join(dir, "node")
	os.WriteFile(node, []byte(`#!/bin/sh
echo "$*" >> `+logs+`
case "$2" in
serve) echo $$ > `+dir+`/served-$5; sleep 60;;
auth) if [ -f `+dir+`/mint-fails ]; then printf '{"credential":"secret-1"}\n'; echo boom >&2; exit 1; fi
  echo 'starting...'; printf '{"id":"p1","credential":"secret-1","scopes":["terminal:read"],"expiresAt":"2026-11-06T00:05:00.000Z"}\n';;
esac
`), 0o700)
	vp := filepath.Join(dir, "vp")
	os.WriteFile(vp, []byte(`#!/bin/sh
up=""
for f in `+dir+`/served-*; do [ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null && up=" while serving"; done
echo "vp $*$up" >> `+logs+`
mkdir -p node_modules
`), 0o700)
	home := filepath.Join(dir, "z3home")
	zmHome := filepath.Join(dir, "zm")
	state := filepath.Join(zmHome, t3State)
	os.MkdirAll(zmHome, 0o700)
	c := config.T3{Checkout: checkout, Commit: pin, Home: home, Port: port, Node: node, VP: vp}
	t.Cleanup(func() {
		if st := readT3State(state); st.PID > 0 {
			t3Stop(st.PID)
		}
	})

	// The descriptor answers on a port once serve ran there (as the real
	// server would).
	serveAt := func(port int) {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/t3/environment" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"environmentId": "env-1", "serverVersion": "0.1.7", "label": "box"})
		})}
		t.Cleanup(func() { srv.Close() })
		go func() {
			for i := 0; i < 1200; i++ {
				if _, err := os.Stat(filepath.Join(dir, "served-"+strconv.Itoa(port))); err == nil {
					if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
						srv.Serve(ln)
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	serveAt(port)
	serveAt(port2)
	// calls is what the fakes logged since the last call. The log is never
	// removed: a fake serve that appends between a read and a remove (it
	// runs on after ensure returns) would lose its line. Only whole lines
	// count, so one half written is read next time.
	read := 0
	calls := func() string {
		b, _ := os.ReadFile(logs)
		b = b[min(read, len(b)):]
		if i := strings.LastIndexByte(string(b), '\n'); i >= 0 {
			b = b[:i+1]
		} else {
			b = nil
		}
		read += len(b)
		return strings.TrimSpace(string(b))
	}
	// awaitCalls waits (up to 5s) for n lines from calls(). Where the port
	// still answers from an earlier step, ensure can mint (and return) before
	// the new fake serve has logged its own line, so callers compare those two
	// lines order-free.
	awaitCalls := func(n int) []string {
		var got []string
		for dl := time.Now().Add(5 * time.Second); len(got) < n && time.Now().Before(dl); {
			if c := calls(); c != "" {
				got = append(got, strings.Split(c, "\n")...)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
		return got
	}
	sameSet := func(got, want []string) bool {
		g, w := append([]string(nil), got...), append([]string(nil), want...)
		sort.Strings(g)
		sort.Strings(w)
		return strings.Join(g, "\n") == strings.Join(w, "\n")
	}
	bin := filepath.Join(checkout, "apps/server/src/bin.ts")
	serve := func(port int, home string) string {
		return bin + " serve --lan --port " + strconv.Itoa(port) + " --base-dir " + home
	}
	mint := func(home string) string {
		return bin + " auth pairing create --base-dir " + home + " --ttl 5m --label zm: mac --json"
	}

	ctx := context.Background()
	var h t3Host
	env, err := h.ensure(ctx, zmHome, c, orchapi.T3EnsureOp{Label: "zm: mac", Scopes: []string{"terminal:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if env.EnvironmentID != "env-1" || env.Version != "0.1.7" || env.Commit != pin || env.Port != port || !env.Started ||
		env.Credential != "secret-1" || env.ExpiresAt.IsZero() {
		t.Fatalf("first ensure: %+v", env)
	}
	if head := gitIn(checkout, "rev-parse", "HEAD"); head != pin {
		t.Fatalf("checkout at %s, want %s", head, pin)
	}
	if got, want := calls(), strings.Join([]string{"vp i", serve(port, home), mint(home) + " --scope terminal:read"}, "\n"); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	st := readT3State(state)
	if st.Commit != pin || st.Port != port || st.Home != home || !t3Alive(st) {
		t.Fatalf("state: %+v", st)
	}
	if pg, err := syscall.Getpgid(st.PID); err != nil || pg != st.PID {
		t.Fatalf("serve runs in its own process group: pgid %d %v (pid %d)", pg, err, st.PID)
	}

	// Again: reused, only a mint.
	if env, err := h.ensure(ctx, zmHome, c, orchapi.T3EnsureOp{Label: "zm: mac"}); err != nil || env.Started || env.Credential != "secret-1" {
		t.Fatalf("second ensure: %+v %v", env, err)
	}
	if got := calls(); got != mint(home) {
		t.Fatalf("second ensure ran %q", got)
	}

	// A re-pin that is not on origin/main: refused, the server still up.
	bad := c
	bad.Commit = strings.Repeat("c", 40)
	if _, err := h.ensure(ctx, zmHome, bad, orchapi.T3EnsureOp{Label: "zm: mac"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a pin off main: %v", err)
	}
	if now := readT3State(state); now != st || !t3Alive(st) {
		t.Fatalf("a refused re-pin took the server down: %+v (was %+v), alive %v", now, st, t3Alive(st))
	}
	calls()

	// A re-pin on main: checked out and installed while the old server
	// runs, which stops only then.
	repin := c
	repin.Commit = first
	if env, err := h.ensure(ctx, zmHome, repin, orchapi.T3EnsureOp{Label: "zm: mac"}); err != nil || !env.Started || env.Commit != first {
		t.Fatalf("re-pin: %+v %v", env, err)
	}
	// The install comes first (before the old server stops); then serve and
	// mint, order-free: the old server's port still answers, so ensure can
	// mint before the new fake serve has logged.
	if got, want := awaitCalls(3), []string{"vp i while serving", serve(port, home), mint(home)}; len(got) != 3 || got[0] != want[0] || !sameSet(got[1:], want[1:]) {
		t.Fatalf("re-pin calls:\n%s\nwant (last two in any order):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if t3Alive(st) {
		t.Fatal("the server at the old pin still runs")
	}
	if head := gitIn(checkout, "rev-parse", "HEAD"); head != first {
		t.Fatalf("checkout at %s, want %s", head, first)
	}
	st = readT3State(state)

	// Another port: zm's server moves (no install: one finished at this
	// commit), the old one stopped.
	moved := repin
	moved.Port = port2
	if env, err := h.ensure(ctx, zmHome, moved, orchapi.T3EnsureOp{Label: "zm: mac"}); err != nil || !env.Started || env.Port != port2 {
		t.Fatalf("new port: %+v %v", env, err)
	}
	if got, want := calls(), strings.Join([]string{serve(port2, home), mint(home)}, "\n"); got != want {
		t.Fatalf("new port calls:\n%s\nwant:\n%s", got, want)
	}
	if t3Alive(st) {
		t.Fatal("a port change orphaned the old server")
	}
	st = readT3State(state)

	// Another home on that port: replaced too.
	home2 := filepath.Join(dir, "z3home2")
	moved.Home = home2
	if env, err := h.ensure(ctx, zmHome, moved, orchapi.T3EnsureOp{Label: "zm: mac"}); err != nil || !env.Started {
		t.Fatalf("new home: %+v %v", env, err)
	}
	// Order-free, and waiting for both lines: port2's descriptor still
	// answers from the step above.
	if got, want := awaitCalls(2), []string{serve(port2, home2), mint(home2)}; !sameSet(got, want) {
		t.Fatalf("new home calls:\n%s\nwant (any order):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if now := readT3State(state); t3Alive(st) || now.Home != home2 || !t3Alive(now) {
		t.Fatalf("a home change: old alive %v, state %+v", t3Alive(st), now)
	}

	// Another zm (its own record) on that port: that server is not its own.
	var other t3Host
	if _, err := other.ensure(ctx, filepath.Join(dir, "other-zm"), moved, orchapi.T3EnsureOp{Label: "zm: mac"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a port another server answers: %v", err)
	}
	// A recorded pid that is some other process now (a reboot) is not zm's server.
	if t3Alive(t3StateFile{PID: os.Getpid(), Port: port}) {
		t.Fatal("this test process taken for the server")
	}
	// A mint that fails: its stdout, the credential, is not in the error.
	os.WriteFile(filepath.Join(dir, "mint-fails"), nil, 0o600)
	if _, err := h.ensure(ctx, zmHome, moved, orchapi.T3EnsureOp{Label: "zm: mac"}); err == nil || !strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a failed mint: %v", err)
	}
	// A block naming a directory that is not a checkout (and not empty).
	if rc, err := resolveT3(config.T3{Checkout: dir, Commit: pin}, zmHome); err != nil {
		t.Fatal(err)
	} else if err := rc.provision(ctx); err == nil || !strings.Contains(err.Error(), "not a t3code checkout") {
		t.Fatalf("a checkout without apps/server/src/bin.ts: %v", err)
	}
}

// "t3": {} is every default: the checkout in zm's home, ~/.z3, port 3773,
// node and vp on PATH, origin/main's tip with no pin.
func TestT3Defaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rc, err := resolveT3(config.T3{}, "/zm")
	if err != nil {
		t.Fatal(err)
	}
	if want := (t3Conf{checkout: "/zm/t3code", home: filepath.Join(home, ".z3"), node: "node", vp: "vp", port: 3773}); rc != want {
		t.Fatalf("defaults: %+v, want %+v", rc, want)
	}
	if _, err := resolveT3(config.T3{Commit: "main"}, "/zm"); err == nil {
		t.Fatal("a commit that is not 40 hex")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		doc string
		on  bool
	}{{`{"t3": {}}`, true}, {`{"t3": {"port": 4000}}`, true}, {`{"machine": "box"}`, false}, {`{"t3": null}`, false}, {`not json`, false}} {
		p := filepath.Join(dir, "config.json")
		os.WriteFile(p, []byte(tc.doc), 0o600)
		if got := t3KeySet(p); got != tc.on {
			t.Errorf("%s: t3 key set %v, want %v", tc.doc, got, tc.on)
		}
	}
	if t3KeySet(filepath.Join(dir, "none.json")) {
		t.Error("no config.json")
	}
}

// With no checkout, ensure clones the repository (the machine's own git
// credentials) into the default place, runs origin/main's tip, installs and
// starts; a missing node or vp, and too little free disk, are refused
// before anything is cloned.
func TestT3HostClonesWhenMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	oldWait, oldRepo, oldFree := t3StartWait, t3Repo, t3DiskFree
	t.Cleanup(func() { t3StartWait, t3Repo, t3DiskFree = oldWait, oldRepo, oldFree })
	t3StartWait = 10 * time.Second
	free := int64(1 << 20)
	t3DiskFree = func(string) (int64, error) { return free, nil }
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	gitIn := func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(dir, "origin")
	os.MkdirAll(filepath.Join(origin, "apps", "server", "src"), 0o755)
	gitIn(dir, "init", "-q", "-b", "main", origin)
	os.WriteFile(filepath.Join(origin, "apps", "server", "src", "bin.ts"), []byte("// v1\n"), 0o644)
	gitIn(origin, "add", ".")
	gitIn(origin, "commit", "-q", "-m", "one")
	tip := gitIn(origin, "rev-parse", "HEAD")
	t3Repo = origin

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	served := filepath.Join(dir, "served")
	node := filepath.Join(dir, "node")
	os.WriteFile(node, []byte(`#!/bin/sh
case "$2" in
serve) echo $$ > `+served+`; sleep 60;;
auth) printf '{"credential":"secret-1","scopes":[],"expiresAt":"2026-11-06T00:05:00.000Z"}\n';;
esac
`), 0o700)
	vp := filepath.Join(dir, "vp")
	os.WriteFile(vp, []byte("#!/bin/sh\nmkdir -p node_modules\n"), 0o700)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"environmentId": "env-1", "serverVersion": "0.1.7"})
	})}
	t.Cleanup(func() { srv.Close() })
	go func() {
		for i := 0; i < 1200; i++ {
			if _, err := os.Stat(served); err == nil {
				if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
					srv.Serve(ln)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	zmHome := filepath.Join(dir, "zm")
	os.MkdirAll(zmHome, 0o700)
	t.Cleanup(func() {
		if st := readT3State(filepath.Join(zmHome, t3State)); st.PID > 0 {
			t3Stop(st.PID)
		}
	})
	checkout := filepath.Join(zmHome, t3Checkout)
	ctx := context.Background()
	var h t3Host
	op := orchapi.T3EnsureOp{Label: "zm: mac"}

	// No node: refused, naming what to install.
	if _, err := h.ensure(ctx, zmHome, config.T3{Port: port, Node: filepath.Join(dir, "no-node"), VP: vp}, op); err == nil ||
		!strings.Contains(err.Error(), "Node.js (node)") || strings.Contains(err.Error(), "Vite+") {
		t.Fatalf("no node: %v", err)
	}
	// Too little disk: refused, saying how much is free.
	free = 100
	if _, err := h.ensure(ctx, zmHome, config.T3{Port: port, Node: node, VP: vp}, op); !api.IsCode(err, api.CodeUnavailable) ||
		!strings.Contains(err.Error(), "100 MB free") || !strings.Contains(err.Error(), "8192 MB floor") {
		t.Fatalf("low disk: %v", err)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("a refused ensure left a checkout: %v", err)
	}
	free = 1 << 20

	env, err := h.ensure(ctx, zmHome, config.T3{Port: port, Node: node, VP: vp}, op)
	if err != nil {
		t.Fatal(err)
	}
	if env.Commit != tip || !env.Started || env.Credential != "secret-1" {
		t.Fatalf("first ensure: %+v", env)
	}
	if head := gitIn(checkout, "rev-parse", "HEAD"); head != tip {
		t.Fatalf("checkout at %s, want %s", head, tip)
	}
	if b, _ := os.ReadFile(filepath.Join(checkout, "node_modules", t3Installed)); strings.TrimSpace(string(b)) != tip {
		t.Fatalf("install marker %q", b)
	}
	ents, _ := os.ReadDir(zmHome)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".clone-") {
			t.Fatalf("a clone left %s", e.Name())
		}
	}
	// main moves on: the server zm started is reused (no pin to follow).
	os.WriteFile(filepath.Join(origin, "apps", "server", "src", "bin.ts"), []byte("// v2\n"), 0o644)
	gitIn(origin, "commit", "-q", "-am", "two")
	if env, err := h.ensure(ctx, zmHome, config.T3{Port: port, Node: node, VP: vp}, op); err != nil || env.Started || env.Commit != tip {
		t.Fatalf("second ensure: %+v %v", env, err)
	}
}
