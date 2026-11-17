package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tarball(t *testing.T, member string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"./LICENSE", []byte("x")}, {"./" + member, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// serve pins tool to an archive served by an httptest server (counting hits).
func serve(t *testing.T, tool, member string, archive []byte, pinned string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	old, oldURLs := Pins, testURLs
	t.Cleanup(func() { Pins, testURLs = old, oldURLs })
	testURLs = true
	Pins = append([]Pin(nil), Pins...)
	for i := range Pins {
		if Pins[i].Tool == tool && Pins[i].OS == runtime.GOOS && Pins[i].Arch == runtime.GOARCH {
			Pins[i].URL, Pins[i].SHA256, Pins[i].Member = srv.URL+"/"+tool+".tar.gz", pinned, member
		}
	}
	return &hits
}

func TestEnsureDownloadsVerifiesAndReuses(t *testing.T) {
	home := t.TempDir()
	body := []byte("#!/bin/sh\necho rclone v1.75.2\n")
	arch := tarball(t, "rclone", body)
	hits := serve(t, Rclone, "rclone", arch, sum(arch))

	if _, err := Ensure(context.Background(), home, []string{Rclone}, Options{}); !errors.Is(err, ErrMissing) {
		t.Fatalf("no download allowed: want ErrMissing, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("downloaded although downloading was off")
	}
	var prog bytes.Buffer
	res, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true, Progress: &prog})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Downloaded || res[0].Path != Path(home, Rclone) || !strings.Contains(prog.String(), "sha256 ok") {
		t.Fatalf("result %+v progress %q", res, prog.String())
	}
	got, err := os.ReadFile(Path(home, Rclone))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("installed %q, %v", got, err)
	}
	if st, _ := os.Stat(Path(home, Rclone)); st.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}
	if p, err := Verify(home, Rclone); err != nil || p != Path(home, Rclone) {
		t.Fatalf("verify: %s %v", p, err)
	}
	// Provisioned: no second download.
	if res, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true}); err != nil || res[0].Downloaded || hits.Load() != 1 {
		t.Fatalf("second ensure downloaded again: %+v %v hits=%d", res, err, hits.Load())
	}
	// A binary swapped behind zm's back is refused, not used.
	if err := os.WriteFile(Path(home, Rclone), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	var mm *MismatchError
	if _, err := Verify(home, Rclone); !errors.As(err, &mm) {
		t.Fatalf("swapped binary: want MismatchError, got %v", err)
	}
	if _, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true}); !errors.As(err, &mm) {
		t.Fatalf("ensure over a swapped binary: want MismatchError, got %v", err)
	}
}

// TestChecksumMismatchRefuses: an archive whose sha256 is not the pinned
// one is refused and nothing is installed (SPEC 17.2, 17.7 "provisioning
// checksum refusal").
func TestChecksumMismatchRefuses(t *testing.T) {
	home := t.TempDir()
	arch := tarball(t, "rclone", []byte("tampered"))
	serve(t, Rclone, "rclone", arch, strings.Repeat("ab", 32))
	_, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true})
	var mm *MismatchError
	if !errors.As(err, &mm) || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, err := os.Stat(Path(home, Rclone)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused archive left a binary: %v", err)
	}
	ents, _ := os.ReadDir(Dir(home))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tar.gz") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("left %s behind", e.Name())
		}
	}
	if _, err := Verify(home, Rclone); !errors.Is(err, ErrMissing) {
		t.Fatalf("verify after refusal: %v", err)
	}
}

// A pin with no member is the executable itself (cloudflared on Linux).
func TestBareExecutablePin(t *testing.T) {
	home := t.TempDir()
	body := []byte("#!/bin/sh\necho cloudflared version 9\n")
	serve(t, Cloudflared, "", body, sum(body))
	res, err := Ensure(context.Background(), home, []string{Cloudflared}, Options{Download: true})
	if err != nil || len(res) != 1 || !res[0].Downloaded {
		t.Fatalf("ensure: %+v %v", res, err)
	}
	got, err := os.ReadFile(res[0].Path)
	if fi, _ := os.Stat(res[0].Path); err != nil || !bytes.Equal(got, body) || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed %q %v", got, err)
	}
	if _, err := Verify(home, Cloudflared); err != nil {
		t.Fatal(err)
	}
}

func TestMemberMissing(t *testing.T) {
	home := t.TempDir()
	arch := tarball(t, "other", []byte("x"))
	serve(t, Rclone, "rclone", arch, sum(arch))
	if _, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true}); err == nil || !strings.Contains(err.Error(), "not in the archive") {
		t.Fatalf("got %v", err)
	}
}

// TestPinTable: every pin is a github.com release asset with a sha256, one
// per tool and platform, for the pinned versions only.
func TestPinTable(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, p := range Pins {
		k := p.Tool + "/" + p.OS + "/" + p.Arch
		if seen[k] {
			t.Errorf("%s pinned twice", k)
		}
		seen[k] = true
		want := map[string]string{Rclone: RcloneVersion, Cloudflared: CloudflaredVersion}[p.Tool]
		tag := "/v" + want + "/"
		if p.Tool == Cloudflared {
			tag = "/" + want + "/" // cloudflare/cloudflared tags have no v
		}
		bare := p.Tool == Cloudflared && p.OS == "linux" // Linux releases are the executable itself
		if p.Version != want || !hex64.MatchString(p.SHA256) || (p.Member == "") != bare ||
			!strings.HasPrefix(p.URL, "https://github.com/") || !strings.Contains(p.URL, tag) {
			t.Errorf("bad pin %+v", p)
		}
	}
	for _, tool := range []string{Rclone, Cloudflared} {
		for _, plat := range []string{"darwin/arm64", "darwin/amd64", "linux/arm64", "linux/amd64"} {
			if !seen[tool+"/"+plat] {
				t.Errorf("no pin for %s on %s", tool, plat)
			}
		}
	}
}

// TestRealPins downloads the real pinned rclone and cloudflared for this
// platform into a temp dir, checking their checksums, and runs each to check
// its version. Opt in with ZM_TEST_DOWNLOAD=1 (network, ~60 MB). With
// ZM_TEST_TOOLS set it downloads into that cache instead (kept, and shared
// with the real off-site test: toolstest), so a pin bump fetches each once.
func TestRealPins(t *testing.T) {
	if os.Getenv("ZM_TEST_DOWNLOAD") != "1" {
		t.Skip("set ZM_TEST_DOWNLOAD=1 to download the pinned releases")
	}
	home := os.Getenv("ZM_TEST_TOOLS")
	if home == "" {
		d, err := os.MkdirTemp("", "pct")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		home = d
	}
	old := LockWait
	LockWait = 10 * time.Minute // the real off-site test may be fetching into the same cache
	t.Cleanup(func() { LockWait = old })
	res, err := Ensure(context.Background(), home, []string{Rclone, Cloudflared}, Options{Download: true, Progress: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("ensured %+v", res)
	}
	// A clean environment with a throwaway HOME that exists: a tool may
	// write its config there, and refuse to start without one.
	h := filepath.Join(home, "h")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		cmd := exec.Command(r.Path, "--version")
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + h}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), r.Version) {
			t.Errorf("%s --version: %q %v %s", r.Tool, out, err, strings.TrimSpace(stderr.String()))
		}
	}
}

// TestEnsureAllOrNothing: when one of two downloads fails (cloudflared's archive
// does not match its pin), the one that succeeded is not installed or
// recorded either: no half-provisioned home.
func TestEnsureAllOrNothing(t *testing.T) {
	home := t.TempDir()
	rcArch := tarball(t, "rclone", []byte("#!/bin/sh\necho rclone v1.75.2\n"))
	serve(t, Rclone, "rclone", rcArch, sum(rcArch))
	cfArch := tarball(t, "cloudflared", []byte("tampered"))
	serve(t, Cloudflared, "cloudflared", cfArch, strings.Repeat("ab", 32))
	_, err := Ensure(context.Background(), home, []string{Rclone, Cloudflared}, Options{Download: true})
	var mm *MismatchError
	if !errors.As(err, &mm) || mm.Tool != Cloudflared {
		t.Fatalf("want cloudflared's mismatch, got %v", err)
	}
	for _, tool := range []string{Rclone, Cloudflared} {
		if _, err := Verify(home, tool); !errors.Is(err, ErrMissing) {
			t.Errorf("%s after a failed Ensure: %v", tool, err)
		}
		if _, err := os.Stat(Path(home, tool)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s installed by a failed Ensure", tool)
		}
	}
	if _, ok := readManifest(home)[Rclone]; ok {
		t.Error("the manifest records rclone from a failed Ensure")
	}
	ents, _ := os.ReadDir(Dir(home))
	for _, e := range ents {
		if e.Name() != ".lock" {
			t.Errorf("left %s behind", e.Name())
		}
	}
	// Fixed, the next Ensure installs both.
	serve(t, Cloudflared, "cloudflared", cfArch, sum(cfArch))
	if _, err := Ensure(context.Background(), home, []string{Rclone, Cloudflared}, Options{Download: true}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{Rclone, Cloudflared} {
		if _, err := Verify(home, tool); err != nil {
			t.Errorf("%s: %v", tool, err)
		}
	}
}

// TestEnsureBusyLock: a second zm never blocks silently behind one that
// is fetching: it says so at once and gives up after LockWait.
func TestEnsureBusyLock(t *testing.T) {
	home := t.TempDir()
	arch := tarball(t, "rclone", []byte("x"))
	serve(t, Rclone, "rclone", arch, sum(arch))
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lock(context.Background(), filepath.Join(Dir(home), ".lock"), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := LockWait
	LockWait = 300 * time.Millisecond
	t.Cleanup(func() { LockWait = old })
	var prog bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := Ensure(context.Background(), home, []string{Rclone}, Options{Download: true, Progress: &prog})
		done <- err
	}()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Ensure blocked behind the held lock")
	}
	if !errors.Is(err, ErrBusy) || !strings.Contains(prog.String(), "another zm is fetching tools") {
		t.Fatalf("err %v, progress %q", err, prog.String())
	}
}
