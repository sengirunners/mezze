package integrations

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The compute plugin's installer (integrations/claude-code-compute/scripts/zm-install)
// against a fake release server: it installs the pinned release only when zm
// is missing, and only what matches the pinned SHA256SUMS.

const computeDir = "../../integrations/claude-code-compute"

// fakeZm is the zm inside fake release tarballs (and the "present" zm): it
// logs its arguments and answers like zm where the plugin cares.
const fakeZm = `#!/bin/sh
[ -z "$ZM_FAKE_LOG" ] || echo "$*" >> "$ZM_FAKE_LOG"
case "$1" in
--version) echo "zm fake" ;;
status) exit 3 ;;
mcp) echo "MCP $*" ;;
esac
`

// sysPath is a PATH with the tools the installer needs and no zm.
func sysPath(t *testing.T) string {
	t.Helper()
	dirs := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "zm")); err == nil {
			t.Skipf("%s/zm exists: cannot test a machine without zm", d)
		}
	}
	p := strings.Join(dirs, ":")
	for _, tool := range []string{"sh", "curl", "tar", "awk", "sed"} {
		if _, err := lookIn(p, tool); err != nil {
			t.Skipf("installer test needs %s", tool)
		}
	}
	if _, err := lookIn(p, "shasum"); err != nil {
		if _, err := lookIn(p, "sha256sum"); err != nil {
			t.Skip("installer test needs shasum or sha256sum")
		}
	}
	return p
}

func lookIn(path, name string) (string, error) {
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

type fakeRelease struct {
	tag, version string
	files        map[string][]byte // served at /<tag>/<name>
	sumsSHA      string            // what the pin says
	tarball      string
	mu           sync.Mutex
	hits         []string
}

// newFakeRelease builds a release like scripts/release does: one tarball for
// this OS and CPU with zm under zm_<version>_<os>_<arch>/, and SHA256SUMS.
func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()
	r := &fakeRelease{tag: "zm-main-0123456789ab", version: "main-0123456789ab", files: map[string][]byte{}}
	name := fmt.Sprintf("zm_%s_%s_%s", r.version, runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	put(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755}, "")
	put(&tar.Header{Typeflag: tar.TypeReg, Name: name + "/zm", Mode: 0o755}, fakeZm)
	put(&tar.Header{Typeflag: tar.TypeReg, Name: name + "/README.md", Mode: 0o644}, "readme\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	r.tarball = name + ".tar.gz"
	r.files[r.tarball] = buf.Bytes()
	other := fmt.Sprintf("zm_%s_plan9_mips.tar.gz", r.version)
	sums := fmt.Sprintf("%s  %s\n%s  %s\n", hexSHA([]byte("other")), other, hexSHA(buf.Bytes()), r.tarball)
	r.files["SHA256SUMS"] = []byte(sums)
	r.sumsSHA = hexSHA([]byte(sums))
	return r
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (r *fakeRelease) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, q.URL.Path)
		r.mu.Unlock()
		b, ok := r.files[strings.TrimPrefix(q.URL.Path, "/"+r.tag+"/")]
		if !ok || !strings.HasPrefix(q.URL.Path, "/"+r.tag+"/") {
			http.NotFound(w, q)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *fakeRelease) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

// pluginCopy copies the plugin's scripts into a temp dir with a pin that
// points at base. It returns the scripts dir.
func pluginCopy(t *testing.T, base, tag, version, sumsSHA string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(computeDir, "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(computeDir, "scripts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scripts", e.Name()), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pin := fmt.Sprintf("# test pin\nrepo=example-org/zm-private\ntag=%s\nversion=%s\nsums_sha256=%s\nbase_url=%s\n", tag, version, sumsSHA, base)
	if err := os.WriteFile(filepath.Join(dir, "zm-release.pin"), []byte(pin), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "scripts")
}

type scriptRun struct {
	stdout, stderr string
	err            error
}

// runScript runs a plugin script with a clean environment: HOME is a temp
// dir, PATH has no zm unless the caller adds one. stdin is never a terminal.
func runScript(t *testing.T, home, path string, env []string, script string, args ...string) scriptRun {
	t.Helper()
	return runScriptCtx(t, context.Background(), "", home, path, env, script, args...)
}

// runScriptCtx is runScript in dir (if set), killed when ctx ends.
func runScriptCtx(t *testing.T, ctx context.Context, dir, home, path string, env []string, script string, args ...string) scriptRun {
	t.Helper()
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + path, "LC_ALL=C"}, env...)
	cmd.Stdin = strings.NewReader("y\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return scriptRun{out.String(), errb.String(), err}
}

func TestComputeInstallMissingZm(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	home := t.TempDir()
	res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err != nil {
		t.Fatalf("install failed: %v\n%s", res.err, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("installer wrote to stdout (an MCP server's protocol channel): %q", res.stdout)
	}
	zm := filepath.Join(home, ".local", "bin", "zm")
	target, err := os.Readlink(zm)
	if want := filepath.Join(home, ".local", "lib", "zm", r.tag, "zm"); err != nil || target != want {
		t.Fatalf("%s -> %q (%v), want a link to %s", zm, target, err, want)
	}
	if out, err := exec.Command(zm, "--version").Output(); err != nil || string(out) != "zm fake\n" {
		t.Errorf("installed zm: %q %v", out, err)
	}
	if !strings.Contains(res.stderr, "installed zm "+r.version) {
		t.Errorf("stderr does not say what it installed:\n%s", res.stderr)
	}
	want := []string{"/" + r.tag + "/SHA256SUMS", "/" + r.tag + "/" + r.tarball}
	if got := r.requests(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("requests %v, want %v", got, want)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".local", "lib", "zm", ".install*")); len(left) != 0 {
		t.Errorf("installer left %v behind", left)
	}
	// --path now names it, and a second run changes nothing.
	if p := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--path"); p.err != nil || p.stdout != zm+"\n" {
		t.Errorf("--path: %q %v", p.stdout, p.err)
	}
	if again := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes"); again.err != nil || !strings.Contains(again.stderr, "leaving it alone") {
		t.Errorf("second run: %v\n%s", again.err, again.stderr)
	}
	if n := len(r.requests()); n != 2 {
		t.Errorf("second run downloaded again (%d requests)", n)
	}
}

func TestComputeInstallRefusesWrongChecksum(t *testing.T) {
	path := sysPath(t)
	for _, tt := range []struct {
		name, want string
		tamper     func(r *fakeRelease)
		downloads  int // requests the installer may make before refusing
	}{
		{"tarball differs from SHA256SUMS", "checksum mismatch: zm_", func(r *fakeRelease) {
			r.files[r.tarball] = append(append([]byte(nil), r.files[r.tarball]...), 0)
		}, 2},
		{"SHA256SUMS differs from the pin", "checksum mismatch: SHA256SUMS", func(r *fakeRelease) {
			r.files["SHA256SUMS"] = append(append([]byte(nil), r.files["SHA256SUMS"]...), '\n')
		}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRelease(t)
			tt.tamper(r)
			srv := r.serve(t)
			scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
			home := t.TempDir()
			res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
			if res.err == nil || !strings.Contains(res.stderr, tt.want) || !strings.Contains(res.stderr, "not installing") {
				t.Fatalf("err=%v, want a refusal with %q:\n%s", res.err, tt.want, res.stderr)
			}
			if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "zm")); err == nil {
				t.Error("a refused download was linked as zm")
			}
			if left, _ := filepath.Glob(filepath.Join(home, ".local", "lib", "zm", "*")); len(left) != 0 {
				t.Errorf("a refused download left %v", left)
			}
			if n := len(r.requests()); n != tt.downloads {
				t.Errorf("%d requests, want %d", n, tt.downloads)
			}
		})
	}
}

func TestComputeInstallLeavesPresentZmAlone(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	t.Run("on PATH", func(t *testing.T) {
		home, bin := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "zm"), []byte(fakeZm), 0o755); err != nil {
			t.Fatal(err)
		}
		res := runScript(t, home, bin+":"+path, nil, filepath.Join(scripts, "zm-install"), "--yes")
		if res.err != nil || !strings.Contains(res.stderr, "already installed ("+filepath.Join(bin, "zm")+")") {
			t.Fatalf("%v\n%s", res.err, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
			t.Error("installer wrote under ~/.local although zm is on PATH")
		}
		if p := runScript(t, home, bin+":"+path, nil, filepath.Join(scripts, "zm-install"), "--path"); p.stdout != filepath.Join(bin, "zm")+"\n" {
			t.Errorf("--path = %q", p.stdout)
		}
	})
	t.Run("in prefix, not on PATH", func(t *testing.T) {
		home := t.TempDir()
		zm := filepath.Join(home, ".local", "bin", "zm")
		if err := os.MkdirAll(filepath.Dir(zm), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(zm, []byte(fakeZm), 0o755); err != nil {
			t.Fatal(err)
		}
		res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
		if res.err != nil || !strings.Contains(res.stderr, "leaving it alone") {
			t.Fatalf("%v\n%s", res.err, res.stderr)
		}
		if b, _ := os.ReadFile(zm); string(b) != fakeZm {
			t.Error("the present zm was changed")
		}
	})
	if n := len(r.requests()); n != 0 {
		t.Errorf("installer downloaded %v although zm was present", r.requests())
	}
}

// While the repository is private, GitHub answers 404 to an anonymous
// download: the installer says so instead of a bare HTTP error.
func TestComputeInstallPrivateRepo(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	r.files = map[string][]byte{}
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	home := t.TempDir()
	res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err == nil || !strings.Contains(res.stderr, "answered 404") || !strings.Contains(res.stderr, "private") ||
		!strings.Contains(res.stderr, "make install") {
		t.Fatalf("err=%v, want the private-repository message:\n%s", res.err, res.stderr)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "zm")); err == nil {
		t.Error("zm linked after a 404")
	}
}

// A pin with a blank tag (the first public copy pins no release) installs nothing and says where to install
// from, without asking any server.
func TestComputeInstallBlankPin(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, "", "", "")
	home := t.TempDir()
	res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err == nil || !strings.Contains(res.stderr, "pins no zm release") || !strings.Contains(res.stderr, "make install") {
		t.Fatalf("err=%v, want the no-release message:\n%s", res.err, res.stderr)
	}
	if n := len(r.requests()); n != 0 {
		t.Errorf("asked a server with no release pinned: %v", r.requests())
	}
}

// Without --yes and without a terminal to ask on, nothing is downloaded.
func TestComputeInstallAsksFirst(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	home := t.TempDir()
	res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"))
	if res.err == nil || !strings.Contains(res.stderr, "--yes") {
		t.Fatalf("err=%v, want a refusal naming --yes:\n%s", res.err, res.stderr)
	}
	if n := len(r.requests()); n != 0 {
		t.Errorf("downloaded without confirmation: %v", r.requests())
	}
}

// A pin may name plain http only on this machine.
func TestComputeInstallHTTPSOnly(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	scripts := pluginCopy(t, "http://example.com/releases", r.tag, r.version, r.sumsSHA)
	res := runScript(t, t.TempDir(), path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err == nil || !strings.Contains(res.stderr, "must be https://") {
		t.Fatalf("err=%v:\n%s", res.err, res.stderr)
	}
}

// The shipped pin names a release the way zm-release.yml publishes it, or none (a blank tag).
func TestComputePinShape(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(computeDir, "zm-release.pin"))
	if err != nil {
		t.Fatal(err)
	}
	pin := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") {
			pin[k] = v
		}
	}
	if pin["tag"] == "" {
		// A blank tag pins no release: only the public copy's pin (TestUpdateSourceOneSetting) is blank, never the
		// private tree's, which still has the override directory beside it.
		if _, err := os.Stat("../../internal/deps/publiccopy"); err == nil {
			t.Error("the private tree's pin names no release: only the public copy's override may")
		}
		if pin["version"] != "" || pin["sums_sha256"] != "" {
			t.Errorf("tag is blank but version %q or sums_sha256 %q is set", pin["version"], pin["sums_sha256"])
		}
		return
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin["sums_sha256"]) {
		t.Errorf("sums_sha256 %q is not a SHA-256", pin["sums_sha256"])
	}
	// The repos and their base URLs: TestUpdateSourceOneSetting.
	if pin["tag"] == "" || pin["tag"] != "zm-"+pin["version"] {
		t.Errorf("tag %q and version %q: the release workflow tags version V as zm-V", pin["tag"], pin["version"])
	}
}

// A relative --prefix is made absolute: the zm link must not dangle.
func TestComputeInstallRelativePrefix(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	home, cwd := t.TempDir(), t.TempDir()
	res := runScriptCtx(t, context.Background(), cwd, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes", "--prefix", "rel")
	if res.err != nil {
		t.Fatalf("%v\n%s", res.err, res.stderr)
	}
	zm := filepath.Join(cwd, "rel", "bin", "zm")
	if out, err := exec.Command(zm, "--version").Output(); err != nil || string(out) != "zm fake\n" {
		t.Errorf("%s does not run from elsewhere: %q %v", zm, out, err)
	}
	if target, _ := os.Readlink(zm); !filepath.IsAbs(target) {
		t.Errorf("link target %q is relative", target)
	}
}

// A lock left by a killed installer is taken over; a live one is waited for.
func TestComputeInstallStaleLock(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	lockWith := func(t *testing.T, home string, pid int) string {
		lock := filepath.Join(home, ".local", "lib", "zm", ".install-lock")
		if err := os.MkdirAll(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		return lock
	}
	t.Run("holder gone", func(t *testing.T) {
		dead := exec.Command("/bin/sh", "-c", "exit 0")
		if err := dead.Run(); err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		lock := lockWith(t, home, dead.Process.Pid)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res := runScriptCtx(t, ctx, "", home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
		if res.err != nil || !strings.Contains(res.stderr, "took over the install lock") {
			t.Fatalf("err=%v\n%s", res.err, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", "zm")); err != nil {
			t.Error("not installed after taking over the lock")
		}
		if _, err := os.Stat(lock); err == nil {
			t.Error("the lock is still there after the install")
		}
	})
	t.Run("holder alive", func(t *testing.T) {
		live := exec.Command("/bin/sleep", "30")
		if err := live.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
		home := t.TempDir()
		lock := lockWith(t, home, live.Process.Pid)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res := runScriptCtx(t, ctx, "", home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
		if res.err == nil || ctx.Err() == nil {
			t.Fatalf("installer did not wait for a live lock holder: %v\n%s", res.err, res.stderr)
		}
		if b, _ := os.ReadFile(filepath.Join(lock, "pid")); strings.TrimSpace(string(b)) != fmt.Sprint(live.Process.Pid) {
			t.Errorf("a live holder's lock was taken: pid file %q", b)
		}
	})
}

// The daemon runs the drivers beside zm only when no directory from theirs
// up is writable by group or others (potluck DRIVERS.md):
// the installer makes lib/zm and the release's directory so, whatever was
// there, and names an ancestor it does not own the fix for.
func TestComputeInstallDriverDirsNotGroupWritable(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	home := t.TempDir()
	local, libzm := filepath.Join(home, ".local"), filepath.Join(home, ".local", "lib", "zm")
	if err := os.MkdirAll(libzm, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{local, libzm} { // as a umask of 002 leaves them
		if err := os.Chmod(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err != nil {
		t.Fatalf("install failed: %v\n%s", res.err, res.stderr)
	}
	for _, d := range []string{libzm, filepath.Join(libzm, r.tag)} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s: %v %v, want no group or other write", d, fi.Mode(), err)
		}
	}
	if !strings.Contains(res.stderr, "warning: "+local+" is writable by group or others") || !strings.Contains(res.stderr, "chmod go-w "+local) ||
		strings.Contains(res.stderr, "warning: "+libzm+" ") {
		t.Errorf("stderr does not name the group-writable ancestor alone:\n%s", res.stderr)
	}
}

// The pin lists sources, new repo first: one that is not public yet (404), or
// whose SHA256SUMS is not the pinned one, passes to the next; the install
// still verifies against the pin, and a pin with no good source fails. The
// what-to-do-about-a-404 advice comes once, only when no source had it, and
// the consent prompt names every source the install may come from.
func TestComputeInstallFallsToNextSource(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	good := r.serve(t)
	missing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	other := newFakeRelease(t) // same tag, a different SHA256SUMS than the pin
	other.files["SHA256SUMS"] = []byte("0000  elsewhere\n")
	wrong := other.serve(t)
	for name, c := range map[string]struct {
		first   string
		install bool
	}{
		"first is not public yet":   {missing.URL, true},
		"first has other checksums": {wrong.URL, true},
	} {
		scripts := pluginCopy(t, good.URL, r.tag, r.version, r.sumsSHA)
		pin := filepath.Join(scripts, "..", "zm-release.pin")
		b, err := os.ReadFile(pin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pin, append([]byte("repo=sengirunners/mezze\nbase_url="+c.first+"\n"), b...), 0o644); err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		res := runScript(t, home, path, nil, filepath.Join(scripts, "zm-install"), "--yes")
		if (res.err == nil) != c.install {
			t.Fatalf("%s: err %v\n%s", name, res.err, res.stderr)
		}
		if out, err := exec.Command(filepath.Join(home, ".local", "bin", "zm"), "--version").Output(); err != nil || string(out) != "zm fake\n" {
			t.Errorf("%s: installed zm %q %v", name, out, err)
		}
		if !strings.Contains(res.stderr, "trying the next source") || strings.Contains(res.stderr, "make install") {
			t.Errorf("%s: a source that failed before one that worked should be noted, without the no-source advice:\n%s", name, res.stderr)
		}
		if name == "first is not public yet" {
			ask := runScript(t, t.TempDir(), path, nil, filepath.Join(scripts, "zm-install"))
			if ask.err == nil || !strings.Contains(ask.stderr, "from github.com/sengirunners/mezze or github.com/example-org/zm-private ") {
				t.Errorf("the prompt does not name both sources: %v\n%s", ask.err, ask.stderr)
			}
		}
	}
	// No source has it: the install fails and says why.
	scripts := pluginCopy(t, missing.URL, r.tag, r.version, r.sumsSHA)
	res := runScript(t, t.TempDir(), path, nil, filepath.Join(scripts, "zm-install"), "--yes")
	if res.err == nil || !strings.Contains(res.stderr, "answered 404") || !strings.Contains(res.stderr, "make install") {
		t.Errorf("err %v\n%s", res.err, res.stderr)
	}
}
