// Package offsitetest is the conformance suite every offsite.Target driver
// passes (docs/ARCHITECTURE.md, rule 1).
package offsitetest

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/offsite"
)

// Run checks t against the Target contract. The target must start empty.
func Run(t *testing.T, tg offsite.Target) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("hello, off-site"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tg.List(ctx, ""); err != nil || len(got) != 0 {
		t.Fatalf("a new target lists %v, %v; want nothing", got, err)
	}
	if got, err := tg.List(ctx, "nothing/"); err != nil || len(got) != 0 {
		t.Fatalf("a missing prefix lists %v, %v; want nothing", got, err)
	}
	if err := tg.Get(ctx, "a/missing", filepath.Join(dir, "x")); !errors.Is(err, offsite.ErrNotFound) {
		t.Fatalf("Get of a missing key: %v, want ErrNotFound", err)
	}
	if err := tg.Copy(ctx, "a/missing", "a/other"); !errors.Is(err, offsite.ErrNotFound) {
		t.Fatalf("Copy of a missing key: %v, want ErrNotFound", err)
	}
	for _, k := range []string{"h/1/files/registry.db.gz", "h/1/manifest.json", "h/2/files/projects/p/issues.db.gz"} {
		if err := tg.Put(ctx, src, k); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	if err := tg.Put(ctx, src, "../escape"); err == nil {
		t.Fatal("Put accepted a key with ..")
	}
	if err := tg.Copy(ctx, "h/1/manifest.json", "h/3/manifest.json"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := tg.List(ctx, "h/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"h/1/files/registry.db.gz", "h/1/manifest.json", "h/2/files/projects/p/issues.db.gz", "h/3/manifest.json"}
	if len(got) != len(want) {
		t.Fatalf("List h/ = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Key != want[i] || got[i].Size != int64(len("hello, off-site")) {
			t.Fatalf("List h/ = %v, want %v of %d bytes", got, want, len("hello, off-site"))
		}
	}
	out := filepath.Join(dir, "deep", "out")
	if err := tg.Get(ctx, "h/3/manifest.json", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "hello, off-site" {
		t.Fatalf("Get read %q", b)
	}
	if st, _ := os.Stat(out); st == nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("Get wrote %v; want an owner-only file", st)
	}
	if err := tg.DeletePrefix(ctx, "h/1/"); err != nil {
		t.Fatal(err)
	}
	if err := tg.DeletePrefix(ctx, "h/9/"); err != nil {
		t.Fatalf("DeletePrefix of nothing: %v", err)
	}
	if err := tg.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("DeletePrefix of everything was accepted")
	}
	got, _ = tg.List(ctx, "h/")
	if len(got) != 2 || got[0].Key != "h/2/files/projects/p/issues.db.gz" {
		t.Fatalf("after deleting h/1/: %v", got)
	}
}

// Rclone is the pinned rclone for a test: $ZM_TEST_RCLONE, or fetched into
// home's tools with ZM_TEST_DOWNLOAD=1 by provision; else the test skips.
func Rclone(t *testing.T, provision func() (string, error)) string {
	t.Helper()
	if p := os.Getenv("ZM_TEST_RCLONE"); p != "" {
		return p
	}
	if os.Getenv("ZM_TEST_DOWNLOAD") != "1" {
		t.Skip("set ZM_TEST_DOWNLOAD=1 (or ZM_TEST_RCLONE=<path>) to fetch the pinned rclone")
	}
	p, err := provision()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// FakeS3 runs `rclone serve s3` (a real S3 API over a directory) on a
// loopback port, with the bucket zm-backups and the keys ak and sk, and stops
// it by its saved process when the test ends. It returns the endpoint and
// the directory behind it.
func FakeS3(t *testing.T, bin, ak, sk string) (endpoint, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "zm-backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, "serve", "s3", "--auth-key", ak+","+sk, "--addr", addr, dir)
	scratch := t.TempDir()
	cmd.Env = []string{"HOME=" + scratch, "RCLONE_CONFIG=" + filepath.Join(scratch, "none.conf"), "RCLONE_CACHE_DIR=" + scratch}
	if testing.Verbose() {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := cmd.Process // stopped by this saved process, never by name
	t.Cleanup(func() {
		proc.Kill()
		cmd.Wait()
	})
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if i > 200 {
			t.Fatalf("the fake S3 never listened on %s", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "http://" + addr, dir
}

// SecretFile writes val to a 0600 file and returns its secret reference.
func SecretFile(t *testing.T, val string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(val+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file:" + p
}
