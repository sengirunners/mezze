package offsite_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/homes/offsite/offsitetest"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/mezze/internal/tools/toolstest"
	"github.com/sengirunners/potluck/kit/config"

	_ "modernc.org/sqlite"
)

func TestMain(m *testing.M) {
	code := m.Run()
	toolstest.Cleanup()
	os.Exit(code)
}

func TestDirConformance(t *testing.T) {
	offsitetest.Run(t, offsite.Dir{Root: t.TempDir()})
}

func TestConfigCheck(t *testing.T) {
	bad := []offsite.Config{
		{URL: "ftp://x/y"},
		{URL: "file://relative/path"},
		{URL: "s3://Bad_Bucket/x", Endpoint: "https://e", AccessKey: "env:A", SecretKey: "env:B"},
		{URL: "s3://bucket/x"}, // no endpoint
		{URL: "s3://bucket/x", Endpoint: "http://example.com", AccessKey: "env:A", SecretKey: "env:B"}, // plain http off loopback
		{URL: "s3://bucket/x", Endpoint: "https://e"},                                                  // no keys
		{URL: "s3://key:secret@bucket/x", Endpoint: "https://e", AccessKey: "env:A", SecretKey: "env:B"},
		{URL: "s3://bucket/x", Endpoint: "https://e", AccessKey: "plaintext", SecretKey: "env:B"},
		{URL: "file:///tmp", Encrypt: "env:E"}, // a passphrase without its salt
	}
	for _, c := range bad {
		if _, err := c.Check(); err == nil {
			t.Errorf("Check(%+v) passed", c)
		}
	}
	good := offsite.Config{URL: "s3://zm-backups/homes", Endpoint: "https://acct.r2.cloudflarestorage.com", AccessKey: "keychain:a", SecretKey: "file:/k", Encrypt: "env:E", EncryptSalt: "env:S"}
	p, err := good.Check()
	if err != nil || p.Bucket != "zm-backups" || p.Prefix != "homes" {
		t.Fatalf("Check(good) = %+v, %v", p, err)
	}
	if s := good.String(); strings.Contains(s, "keychain") || !strings.Contains(s, "encrypted") {
		t.Fatalf("String() = %q", s)
	}
}

func TestRetain(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var sets []offsite.SetInfo
	// Every 15 minutes for three days.
	for at := now.Add(-72 * time.Hour); !at.After(now); at = at.Add(15 * time.Minute) {
		sets = append(sets, offsite.SetInfo{Stamp: at.Format(offsite.StampLayout), Complete: true})
	}
	leftover := now.Add(-5 * time.Minute).Format(offsite.StampLayout)
	sets = append(sets, offsite.SetInfo{Stamp: leftover})
	cur := now.Format(offsite.StampLayout)
	drop := offsite.Retain(sets, cur, now, offsite.Policy{Keep: 4, Hourly: 6, Daily: 2})
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	kept := 0
	for _, s := range sets {
		if !dropped[s.Stamp] {
			kept++
		}
	}
	// current + 3 more newest, 6 hourly (the first overlaps), 2 daily: 4 + 5 + 2 at most, the
	// leftover always goes.
	if !dropped[leftover] || dropped[cur] || kept < 9 || kept > 12 {
		t.Fatalf("kept %d of %d (leftover dropped %v, current dropped %v)", kept, len(sets), dropped[leftover], dropped[cur])
	}
	// Keep 0 everything: still the current and the newest complete one.
	drop = offsite.Retain(sets, cur, now, offsite.Policy{})
	if len(sets)-len(drop) != 1 {
		t.Fatalf("an empty policy kept %d sets, want only the current one", len(sets)-len(drop))
	}
}

// home writes a small home: a registry, one project with two databases and
// a toml file.
func home(t *testing.T) (string, []offsite.Staged) {
	dir := t.TempDir()
	var staged []offsite.Staged
	for _, p := range []string{"registry.db", "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.db", "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/issues.db"} {
		path := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(path), 0o700)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE t(x TEXT); INSERT INTO t VALUES (?)`, strings.Repeat(p, 1000)); err != nil {
			t.Fatal(err)
		}
		db.Close()
		staged = append(staged, offsite.Staged{Path: p, Local: path})
	}
	toml := filepath.Join(dir, "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml")
	os.WriteFile(toml, []byte("slug = \"x\"\n"), 0o600)
	return dir, append(staged, offsite.Staged{Path: "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml", Local: toml})
}

func backupKey(t *testing.T) *bkcrypt.Key {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	k, err := bkcrypt.Parse(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// roundTrip uploads two encrypted sets (the second copies unchanged files),
// restores the newest into a fresh home, and checks retention and refusals.
func roundTrip(t *testing.T, tg offsite.Target) {
	ctx := context.Background()
	key := backupKey(t)
	src, staged := home(t)
	m1, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T010000Z", Created: time.Now()}, staged, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml"), []byte("slug = \"y\"\n"), 0o600)
	m2, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T011500Z", Created: time.Now()}, staged, &m1, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Files) != 4 || m2.Stored <= 0 || m2.Cipher != bkcrypt.Cipher || m2.KeyID != key.ID() {
		t.Fatalf("manifest %+v", m2)
	}
	sets, err := offsite.Sets(ctx, tg, "h1")
	if err != nil || len(sets) != 2 || !sets[1].Complete {
		t.Fatalf("Sets = %+v, %v", sets, err)
	}
	stamp, _ := offsite.Pick(sets, "")
	dst := t.TempDir()
	// The key is needed, and only the one the set names opens it.
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst, true, nil); err == nil || !strings.Contains(err.Error(), "no key was given") {
		t.Fatalf("restore with no key: %v", err)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst, true, backupKey(t)); err == nil || !strings.Contains(err.Error(), key.ID()) {
		t.Fatalf("restore with another key: %v", err)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst, true, key); err != nil {
		t.Fatalf("dry restore: %v", err)
	}
	if des := entries(dst); len(des) != 0 {
		t.Fatalf("a dry restore left %v", des)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst, false, key); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// A finished restore leaves no restoring marker.
	if err := config.CheckNotRestoring(dst); err != nil {
		t.Fatalf("restored home refused: %v", err)
	}
	// Every restore mints a new restore generation.
	gen, err := config.RestoreGeneration(dst)
	if err != nil || gen == "" {
		t.Fatalf("restore wrote no restore generation: %q %v", gen, err)
	}
	for _, f := range staged {
		want, _ := os.ReadFile(f.Local)
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(f.Path)))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s restored wrong (%v)", f.Path, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml")); string(b) != "slug = \"y\"\n" {
		t.Fatalf("restored the older set: %q", b)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst, false, key); err == nil {
		t.Fatal("restored into a home that is not empty")
	}
	// An older set by stamp.
	at, err := offsite.Pick(sets, "20261102T0100")
	if err != nil || at != "20261102T010000Z" {
		t.Fatalf("Pick = %s, %v", at, err)
	}
	if err := offsite.DeleteSet(ctx, tg, "h1", at); err != nil {
		t.Fatal(err)
	}
	if sets, _ := offsite.Sets(ctx, tg, "h1"); len(sets) != 1 {
		t.Fatalf("after DeleteSet: %+v", sets)
	}
	// The newest set's files were copied from the deleted one: still whole.
	dst2 := t.TempDir()
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst2, true, key); err != nil {
		t.Fatalf("the newest set after its source set went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst2, config.FileRestored)); !os.IsNotExist(err) {
		t.Fatalf("a dry restore wrote a restore generation: %v", err)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", stamp, dst2, false, key); err != nil {
		t.Fatal(err)
	}
	if gen2, err := config.RestoreGeneration(dst2); err != nil || gen2 == "" || gen2 == gen {
		t.Fatalf("a second restore of the same set kept the generation: %q then %q (%v)", gen, gen2, err)
	}
}

func TestRoundTripDir(t *testing.T) {
	roundTrip(t, offsite.Dir{Root: t.TempDir()})
}

// The secret names project.db holds never reach the target in the clear,
// and a set an older zm wrote plain is never carried into an encrypted one;
// plain sets still restore without a key.
func TestSetsAreEncrypted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tg := offsite.Dir{Root: root}
	key := backupKey(t)
	_, staged := home(t)
	plain, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T010000Z"}, staged, nil, nil)
	if err != nil || plain.Cipher != "" {
		t.Fatalf("plain set: %+v %v", plain, err)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", plain.Stamp, t.TempDir(), true, nil); err != nil {
		t.Fatalf("a plain set without a key: %v", err)
	}
	enc, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T011500Z"}, staged, &plain, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := offsite.DeleteSet(ctx, tg, "h1", plain.Stamp); err != nil {
		t.Fatal(err)
	}
	// What home() wrote into project.db, as it would appear gzipped or not.
	needle := []byte("projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.db")
	filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || filepath.Base(p) == "manifest.json" {
			return err
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, needle) || bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			t.Errorf("%s is not encrypted", p)
		}
		return nil
	})
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, "secrets"), 0o700) // where a restore's key may wait
	if _, err := offsite.Restore(ctx, tg, "h1", enc.Stamp, dst, false, key); err != nil {
		t.Fatalf("the encrypted set after the plain one went: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(staged[1].Path)))
	want, _ := os.ReadFile(staged[1].Local)
	if !bytes.Equal(got, want) {
		t.Fatal("project.db restored wrong")
	}
}

// TestRestoreRefusesTampering: a file that does not match its manifest
// stops the restore before anything is in place.
func TestRestoreRefusesTampering(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tg := offsite.Dir{Root: root}
	_, staged := home(t)
	if _, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T010000Z"}, staged, nil, nil); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "x")
	os.WriteFile(other, []byte("not the file"), 0o600)
	if err := tg.Put(ctx, staged[0].Local, "h1/20261102T010000Z/files/projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/issues.db.gz"); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := offsite.Restore(ctx, tg, "h1", "20261102T010000Z", dst, false, nil); err == nil {
		t.Fatal("restored a set with a swapped file")
	}
	if des := entries(dst); len(des) != 0 {
		t.Fatalf("a refused restore left %v", des)
	}
}

// entries is dir's entries but the (empty) daemon lock a restore holds while
// it runs and leaves behind.
func entries(dir string) []string {
	des, _ := os.ReadDir(dir)
	var out []string
	for _, de := range des {
		if de.Name() != config.FileHomeLock {
			out = append(out, de.Name())
		}
	}
	return out
}

// gated holds the first Get until open is closed.
type gated struct {
	offsite.Target
	entered chan struct{}
	open    chan struct{}
}

func (g *gated) Get(ctx context.Context, key, local string) error {
	select {
	case <-g.entered:
	default:
		close(g.entered)
		<-g.open
	}
	return g.Target.Get(ctx, key, local)
}

// TestRestoreHoldsTheHome: a restore refuses a home a daemon
// holds, and no daemon can take the home while a restore fetches into it
// (a CLI autostart there would write a registry the restore renames over).
func TestRestoreHoldsTheHome(t *testing.T) {
	ctx := context.Background()
	tg := offsite.Dir{Root: t.TempDir()}
	_, staged := home(t)
	if _, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: "h1", Stamp: "20261102T010000Z"}, staged, nil, nil); err != nil {
		t.Fatal(err)
	}
	busy := t.TempDir()
	release, err := config.LockHome(busy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offsite.Restore(ctx, tg, "h1", "20261102T010000Z", busy, false, nil); err == nil || !strings.Contains(err.Error(), "no daemon runs") {
		t.Fatalf("restore into a home a daemon holds: %v", err)
	}
	release()

	dst := t.TempDir()
	g := &gated{Target: tg, entered: make(chan struct{}), open: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := offsite.Restore(ctx, g, "h1", "20261102T010000Z", dst, false, nil); done <- err }()
	<-g.entered // fetching now
	if rel, err := config.LockHome(dst); err == nil {
		rel()
		t.Fatal("a daemon took the home while a restore fetched into it")
	}
	close(g.open)
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}
	rel, err := config.LockHome(dst) // a daemon starts once it is done
	if err != nil {
		t.Fatalf("after the restore: %v", err)
	}
	rel()
}

func rclone(t *testing.T) string {
	return offsitetest.Rclone(t, func() (string, error) {
		h := t.TempDir()
		if err := toolstest.ProvisionTools(context.Background(), h, tools.Options{}, tools.Rclone); err != nil {
			return "", err
		}
		return tools.Verify(h, tools.Rclone)
	})
}

func fakeS3(t *testing.T, bin, ak, sk string) (string, string) {
	return offsitetest.FakeS3(t, bin, ak, sk)
}

func secretFile(t *testing.T, val string) string { return offsitetest.SecretFile(t, val) }

func open(t *testing.T, bin string, c offsite.Config) offsite.Target {
	t.Helper()
	tg, err := offsite.Opener{Bin: bin, Scratch: t.TempDir()}.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// TestRealRcloneS3 is the driver against a local S3 (rclone serve s3), plain
// and encrypted: conformance, then a full round trip of sets.
func TestRealRcloneS3(t *testing.T) {
	bin := rclone(t)
	ak, sk := "zmtestaccess", "zmtestsecret-0123456789"
	endpoint, dir := fakeS3(t, bin, ak, sk)
	base := offsite.Config{Endpoint: endpoint, AccessKey: secretFile(t, ak), SecretKey: secretFile(t, sk)}

	plain := base
	plain.URL = "s3://zm-backups/plain"
	t.Run("conformance", func(t *testing.T) { offsitetest.Run(t, open(t, bin, plain)) })
	t.Run("roundtrip", func(t *testing.T) { roundTrip(t, open(t, bin, plain)) })

	enc := base
	enc.URL = "s3://zm-backups/enc"
	enc.Encrypt = secretFile(t, "a long passphrase for the test")
	enc.EncryptSalt = secretFile(t, "a salt for the test")
	t.Run("encrypted", func(t *testing.T) {
		tg := open(t, bin, enc)
		offsitetest.Run(t, tg)
		roundTrip(t, open(t, bin, enc))
		// Nothing readable on the server: no plain name, no sqlite header.
		filepath.WalkDir(filepath.Join(dir, "zm-backups", "enc"), func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return err
			}
			if strings.Contains(p, "manifest") || strings.Contains(p, "registry") {
				t.Errorf("a plain name on the server: %s", p)
			}
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "SQLite format") || strings.Contains(string(b), "\"home\"") {
				t.Errorf("plain content on the server: %s", p)
			}
			return nil
		})
	})

	t.Run("wrong key", func(t *testing.T) {
		bad := plain
		bad.SecretKey = secretFile(t, "not-the-secret-key")
		tg := open(t, bin, bad)
		_, err := tg.List(context.Background(), "")
		if err == nil {
			t.Fatal("listed with a wrong secret key")
		}
		if strings.Contains(err.Error(), "not-the-secret-key") || strings.Contains(err.Error(), sk) {
			t.Fatalf("the error quotes a secret: %v", err)
		}
		t.Log("wrong key error:", err)
	})
}

// TestRealRcloneEncryptedFile: an encrypted file:// target goes through rclone's
// crypt over its local backend.
func TestRealRcloneEncryptedFile(t *testing.T) {
	bin := rclone(t)
	root := t.TempDir()
	c := offsite.Config{URL: "file://" + root, Encrypt: secretFile(t, "another passphrase"), EncryptSalt: secretFile(t, "another salt")}
	offsitetest.Run(t, open(t, bin, c))
	roundTrip(t, open(t, bin, c))
	// The same passphrase with another salt reads nothing.
	other := c
	other.EncryptSalt = secretFile(t, "not the salt")
	sets, _ := offsite.Sets(context.Background(), open(t, bin, other), "h1")
	if _, err := offsite.Pick(sets, ""); err == nil {
		t.Fatalf("another salt found sets: %+v", sets)
	}
}

// A manifest naming anything a set does not hold (tools/, a secret's place,
// backup.json) is refused before anything is fetched into place. Upload
// refuses such a path too, so the manifest is written by hand here, as a
// tampered one would be.
func TestRestoreRefusesPathsOutsideTheAllowlist(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tg := offsite.Dir{Root: root}
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, []byte("#!/bin/sh\necho planted\n"), 0o700)
	for i, p := range []string{"tools/rclone", "secrets/x/y.password", "receipt.key", "backup.json", "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/other.db"} {
		home, stamp := fmt.Sprintf("h%d", i), "20261102T010000Z"
		if _, err := offsite.Upload(ctx, tg, offsite.Manifest{Home: home, Stamp: stamp}, []offsite.Staged{{Path: p, Local: src}}, nil, nil); err == nil {
			t.Fatalf("Upload took %s", p)
		}
		gz := filepath.Join(root, home, stamp, "files", filepath.FromSlash(p)+".gz")
		os.MkdirAll(filepath.Dir(gz), 0o700)
		os.WriteFile(gz, []byte("x"), 0o600)
		mf, _ := json.Marshal(offsite.Manifest{V: 1, Home: home, Stamp: stamp, Files: []offsite.File{{Path: p, Bytes: 1, SHA256: "00"}}})
		os.WriteFile(filepath.Join(root, home, stamp, "manifest.json"), mf, 0o600)
		dst := t.TempDir()
		if _, err := offsite.Restore(ctx, tg, home, stamp, dst, false, nil); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("%s: restore said %v", p, err)
		}
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(p))); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
	for _, p := range []string{"registry.db", "config.json", "top.json", "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/issues.db",
		"projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml", "trackers/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/bd/home.json"} {
		if !offsite.HomeFile(p) {
			t.Errorf("HomeFile(%s) = false", p)
		}
	}
}
