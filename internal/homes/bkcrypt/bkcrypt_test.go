package bkcrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	raw := make([]byte, keyLen)
	rand.Read(raw)
	return newKey(raw)
}

func seal(t *testing.T, k *Key, p []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NewWriter(&b, k)
	if err != nil {
		t.Fatal(err)
	}
	// Uneven writes: chunk boundaries never line up with them.
	for len(p) > 0 {
		n := min(len(p), 7777)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func open(k *Key, c []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(c), k)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestRoundTrip(t *testing.T) {
	k := testKey(t)
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 5} {
		p := make([]byte, n)
		rand.Read(p)
		c := seal(t, k, p)
		if bytes.Contains(c, p[:min(n, 64)]) && n >= 16 {
			t.Fatalf("%d bytes: the plaintext shows in the file", n)
		}
		got, err := open(k, c)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("%d bytes: round trip: err %v, equal %v", n, err, bytes.Equal(got, p))
		}
	}
}

// A file that was changed, cut, extended or sealed under another key never
// reads as whole.
func TestRefusesChangedFiles(t *testing.T) {
	k := testKey(t)
	p := make([]byte, 2*chunk+100)
	rand.Read(p)
	c := seal(t, k, p)
	sealedChunk := chunk + tagLen
	cases := map[string][]byte{
		"a flipped bit":             flip(c, headerLen+10),
		"a flipped header salt":     flip(c, len(magic)+idLen+1),
		"cut at a chunk boundary":   c[:headerLen+2*sealedChunk],
		"cut mid chunk":             c[:headerLen+sealedChunk+100],
		"only the header":           c[:headerLen],
		"a byte added":              append(append([]byte{}, c...), 0),
		"a middle chunk dropped":    append(append([]byte{}, c[:headerLen+sealedChunk]...), c[headerLen+2*sealedChunk:]...),
		"chunks swapped":            swap(c, headerLen, headerLen+sealedChunk, sealedChunk),
		"a chunk sealed twice over": append(append([]byte{}, c[:headerLen+sealedChunk]...), c[headerLen:]...),
	}
	for name, bad := range cases {
		got, err := open(k, bad)
		if err == nil {
			t.Errorf("%s: opened (%d bytes)", name, len(got))
		}
	}
	other := testKey(t)
	if _, err := open(other, c); !errors.As(err, new(*WrongKeyError)) {
		t.Errorf("another key: %v, want a WrongKeyError", err)
	}
	if _, err := open(k, []byte("SQLite format 3\x00 and so on, a plain copy")); err == nil {
		t.Error("a plain file opened")
	}
}

func flip(c []byte, i int) []byte {
	b := append([]byte{}, c...)
	b[i] ^= 1
	return b
}

func swap(c []byte, i, j, n int) []byte {
	b := append([]byte{}, c...)
	copy(b[i:i+n], c[j:j+n])
	copy(b[j:j+n], c[i:i+n])
	return b
}

func TestEnsureMakesOneKey(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if _, err := Open(home); err == nil || !strings.Contains(err.Error(), "no backup key") {
		t.Fatalf("open before any key: %v", err)
	}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Go(func() {
			k, err := Ensure(home)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = k.ID()
		})
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("racing Ensure made different keys: %v", ids)
		}
	}
	st, err := os.Stat(KeyPath(home))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Dir(KeyPath(home))); st.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode %v", st.Mode().Perm())
	}
	des, _ := os.ReadDir(filepath.Dir(KeyPath(home)))
	if len(des) != 1 {
		t.Fatalf("leftovers beside the key: %v", des)
	}
	k, err := Open(home)
	if err != nil || k.ID() != ids[0] {
		t.Fatalf("reopen: %v %v", k, err)
	}
	// The key read through a reference, as restore --key does, is the same.
	k2, err := Load(context.Background(), "file:"+KeyPath(home))
	if err != nil || k2.ID() != k.ID() {
		t.Fatalf("load: %v", err)
	}
	// Write refuses to replace a key.
	if err := Write(KeyPath(home), testKey(t)); err == nil {
		t.Fatal("Write replaced a key")
	}
}

func TestFiles(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	k := testKey(t)
	src := filepath.Join(dir, "a.db")
	p := bytes.Repeat([]byte("secret-name GITHUB_TOKEN "), 10000)
	os.WriteFile(src, p, 0o600)
	n, sum, err := EncryptFile(ctx, src, filepath.Join(dir, "a.db.enc"), k)
	if err != nil || n != int64(len(p)) {
		t.Fatal(n, err)
	}
	c, _ := os.ReadFile(filepath.Join(dir, "a.db.enc"))
	if bytes.Contains(c, []byte("GITHUB_TOKEN")) {
		t.Fatal("the encrypted file shows its content")
	}
	if st, _ := os.Stat(filepath.Join(dir, "a.db.enc")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if ok, _ := Sealed(filepath.Join(dir, "a.db.enc")); !ok {
		t.Fatal("Sealed says no")
	}
	if ok, _ := Sealed(src); ok {
		t.Fatal("Sealed says a plain file is")
	}
	n2, sum2, err := DecryptFile(ctx, filepath.Join(dir, "a.db.enc"), filepath.Join(dir, "b.db"), k)
	got, _ := os.ReadFile(filepath.Join(dir, "b.db"))
	if err != nil || n2 != n || sum2 != sum || !bytes.Equal(got, p) {
		t.Fatalf("decrypt: %v", err)
	}
	if _, sum3, err := DecryptFile(ctx, filepath.Join(dir, "a.db.enc"), "", k); err != nil || sum3 != sum {
		t.Fatalf("check only: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "c.enc"), c[:len(c)-1], 0o600)
	if _, _, err := DecryptFile(ctx, filepath.Join(dir, "c.enc"), filepath.Join(dir, "c.db"), k); err == nil {
		t.Fatal("a cut file decrypted")
	}
	if _, err := os.Stat(filepath.Join(dir, "c.db")); err == nil {
		t.Fatal("a failed decrypt left its output")
	}
}

func TestParse(t *testing.T) {
	k := testKey(t)
	var b bytes.Buffer
	dir, _ := os.MkdirTemp("", "zm")
	defer os.RemoveAll(dir)
	if err := Write(filepath.Join(dir, "k"), k); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(filepath.Join(dir, "k"))
	b.Write(text)
	k2, err := Parse("  " + b.String() + "\n")
	if err != nil || k2.ID() != k.ID() {
		t.Fatalf("parse: %v", err)
	}
	for _, bad := range []string{"", "short", "!!!!", strings.Repeat("A", 100)} {
		if _, err := Parse(bad); err == nil || strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("Parse(%q): %v", bad, err)
		}
	}
}
