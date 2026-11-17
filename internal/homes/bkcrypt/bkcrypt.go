// Package bkcrypt encrypts zm's backups (SPEC 17.9 "Backup encryption"): the
// sqlite copies of a native tracker (project.db can hold secret names: the
// grants table and the secret ledger) and every file of an off-site home set.
// It is on for both, always, with one key per home: $ZM_HOME/secrets/backup.key
// (0600, beside the home's other secrets), made on first use and never sent
// with a backup. Without that file no backup of the home can be read, so it
// must be kept somewhere off the machine too.
//
// The format (Cipher): a 40-byte header (magic, the key's id, a random salt),
// then the content in 64 KiB chunks, each sealed with AES-256-GCM under a key
// HKDF-SHA256 derives from the home key and the salt (one key per file). A
// chunk's nonce is its index and whether it is the last (the STREAM
// construction), and the header is every chunk's additional data, so a
// reordered, dropped, truncated or extended file fails to open, as does one
// sealed under another key. Only the standard library is used.
package bkcrypt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Cipher names the format in a manifest; a later format gets a new name.
const Cipher = "zm-aes256gcm-stream-v1"

// KeyRel is the key file, relative to $ZM_HOME.
const KeyRel = "secrets/backup.key"

const (
	keyLen    = 32
	idLen     = 8
	saltLen   = 24
	headerLen = len(magic) + idLen + saltLen
	chunk     = 64 << 10
	tagLen    = 16
	hkdfInfo  = "zm backup file v1"
)

// magic starts every encrypted file.
const magic = "zmbk\x00v1\n"

// Key is a home's backup key.
type Key struct {
	k  [keyLen]byte
	id [idLen]byte
}

// ID names the key without revealing it (in a manifest, an error).
func (k *Key) ID() string { return hex.EncodeToString(k.id[:]) }

func newKey(raw []byte) *Key {
	k := &Key{}
	copy(k.k[:], raw)
	sum := sha256.Sum256(append([]byte("zm backup key id\x00"), raw...))
	copy(k.id[:], sum[:idLen])
	return k
}

// Parse reads a key's text form (base64 of 32 bytes, surrounding space
// ignored). Errors never quote it.
func Parse(text string) (*Key, error) {
	t := strings.TrimSpace(text)
	raw, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(t)
	}
	if err != nil || len(raw) != keyLen {
		return nil, api.Errorf(api.CodeBadRequest, "not a zm backup key (want the base64 of %d bytes, as in %s)", keyLen, KeyRel)
	}
	return newKey(raw), nil
}

// KeyPath is home's key file.
func KeyPath(home string) string { return filepath.Join(home, filepath.FromSlash(KeyRel)) }

// Load reads the key a secret reference names (keychain:, file:, env:).
func Load(ctx context.Context, ref string) (*Key, error) {
	r, err := secrets.Parse(ref)
	if err != nil {
		return nil, err
	}
	v, err := secrets.Get(ctx, r)
	if err != nil {
		return nil, secrets.Prefix(err, "the backup key: ")
	}
	return Parse(v.Reveal())
}

// Open reads home's key; a home without one is ErrNoKey.
func Open(home string) (*Key, error) {
	if _, err := os.Lstat(KeyPath(home)); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey(home)
	}
	return Load(context.Background(), "file:"+KeyPath(home))
}

// ErrNoKey is a home with no backup key to read an encrypted backup with.
func ErrNoKey(home string) error {
	return api.Errorf(api.CodeBadRequest, "this backup is encrypted and %s has no backup key", KeyPath(home)).
		WithHint("put the key file of the home that wrote it there (0600), or name it with --key")
}

// Ensure is home's key, made (random, 0600, its directory 0700) when there
// is none. An existing key is never replaced: two callers racing to make one
// both end with the same key.
func Ensure(home string) (*Key, error) {
	p := KeyPath(home)
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		return Open(home)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".backup.key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	// A link, not a rename: it fails when the key appeared meanwhile.
	if err := os.Link(tmp.Name(), p); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return Open(home)
}

// Write writes k's text form to path (0600), refusing to replace a file
// there (a restore putting the key a set was sealed with into a new home).
func Write(path string, k *Key) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(k.k[:]) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ErrOpen is every failure to open an encrypted file but a wrong key: one
// message, whatever was changed.
var ErrOpen = errors.New("the encrypted backup is damaged or was changed (it does not open)")

// WrongKeyError is a file sealed under another key.
type WrongKeyError struct{ File, Have string }

func (e *WrongKeyError) Error() string {
	return fmt.Sprintf("this backup was encrypted with backup key %s, not this home's key %s", e.File, e.Have)
}

func fileAEAD(k *Key, salt []byte) (cipher.AEAD, error) {
	fk, err := hkdf.Key(sha256.New, k.k[:], salt, hkdfInfo, keyLen)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(fk)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func nonce(n []byte, i uint64, last bool) {
	clear(n)
	binary.BigEndian.PutUint64(n[3:11], i)
	if last {
		n[11] = 1
	}
}

type writer struct {
	w      io.Writer
	aead   cipher.AEAD
	header []byte
	buf    []byte
	out    []byte
	nonce  []byte
	i      uint64
	closed bool
	err    error
}

// NewWriter encrypts what is written to it into w. Close seals the last
// chunk (it does not close w); without it the file does not open.
func NewWriter(w io.Writer, k *Key) (io.WriteCloser, error) {
	h := make([]byte, headerLen)
	copy(h, magic)
	copy(h[len(magic):], k.id[:])
	if _, err := rand.Read(h[len(magic)+idLen:]); err != nil {
		return nil, err
	}
	a, err := fileAEAD(k, h[len(magic)+idLen:])
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(h); err != nil {
		return nil, err
	}
	return &writer{w: w, aead: a, header: h, buf: make([]byte, 0, chunk), out: make([]byte, 0, chunk+tagLen), nonce: make([]byte, a.NonceSize())}, nil
}

func (w *writer) seal(p []byte, last bool) error {
	nonce(w.nonce, w.i, last)
	w.i++
	w.out = w.aead.Seal(w.out[:0], w.nonce, p, w.header)
	_, err := w.w.Write(w.out)
	return err
}

func (w *writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("bkcrypt: write after close")
	}
	n := 0
	for len(p) > 0 {
		// A full chunk is sealed only once more follows: the last chunk is
		// the one Close seals, so it is never one already written.
		if len(w.buf) == chunk {
			if w.err = w.seal(w.buf, false); w.err != nil {
				return n, w.err
			}
			w.buf = w.buf[:0]
		}
		m := min(chunk-len(w.buf), len(p))
		w.buf = append(w.buf, p[:m]...)
		p, n = p[m:], n+m
	}
	return n, nil
}

func (w *writer) Close() error {
	if w.err != nil || w.closed {
		return w.err
	}
	w.closed = true
	w.err = w.seal(w.buf, true)
	return w.err
}

type reader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	header []byte
	in     []byte
	plain  []byte
	nonce  []byte
	i      uint64
	done   bool
}

// NewReader decrypts r, which NewWriter wrote under k. It returns io.EOF
// only after the last chunk opened: a cut file is ErrOpen, never a short
// read that looks whole.
func NewReader(r io.Reader, k *Key) (io.Reader, error) {
	h := make([]byte, headerLen)
	if _, err := io.ReadFull(r, h); err != nil || string(h[:len(magic)]) != magic {
		return nil, errors.New("not an encrypted zm backup")
	}
	if !bytes.Equal(h[len(magic):len(magic)+idLen], k.id[:]) {
		return nil, &WrongKeyError{File: hex.EncodeToString(h[len(magic) : len(magic)+idLen]), Have: k.ID()}
	}
	a, err := fileAEAD(k, h[len(magic)+idLen:])
	if err != nil {
		return nil, err
	}
	return &reader{r: bufio.NewReaderSize(r, chunk+tagLen+1), aead: a, header: h, in: make([]byte, chunk+tagLen), nonce: make([]byte, a.NonceSize())}, nil
}

func (r *reader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

func (r *reader) next() error {
	n, err := io.ReadFull(r.r, r.in)
	switch {
	case err == io.ErrUnexpectedEOF || err == io.EOF:
		r.done = true // a short chunk is the last, or the file was cut
	case err != nil:
		return err
	default:
		_, perr := r.r.Peek(1)
		if perr == io.EOF {
			r.done = true
		} else if perr != nil {
			return perr
		}
	}
	if n < tagLen {
		return ErrOpen
	}
	nonce(r.nonce, r.i, r.done)
	r.i++
	plain, oerr := r.aead.Open(r.in[:0], r.nonce, r.in[:n], r.header)
	if oerr != nil {
		return ErrOpen
	}
	r.plain = plain
	return nil
}

// Sealed says whether the file at path is an encrypted backup (by its magic).
func Sealed(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b := make([]byte, len(magic))
	if _, err := io.ReadFull(f, b); err != nil {
		return false, nil // shorter than the magic: not one
	}
	return string(b) == magic, nil
}

// EncryptFile writes src encrypted under k to dst (0600 from its first
// byte; through dst.tmp, renamed only when whole and synced) and returns
// src's size and sha256.
func EncryptFile(ctx context.Context, src, dst string, k *Key) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	bw := bufio.NewWriterSize(out, 256<<10)
	n, err := func() (int64, error) {
		ew, err := NewWriter(bw, k)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(io.MultiWriter(ew, h), ctxReader{ctx, in})
		if err != nil {
			return n, err
		}
		if err := ew.Close(); err != nil {
			return n, err
		}
		if err := bw.Flush(); err != nil {
			return n, err
		}
		return n, out.Sync()
	}()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// DecryptFile writes src, encrypted under k, to dst (0600) and returns the
// content's size and sha256. dst is empty: "" checks src without writing.
func DecryptFile(ctx context.Context, src, dst string, k *Key) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	r, err := NewReader(bufio.NewReaderSize(in, 256<<10), k)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	var w io.Writer = h
	var out *os.File
	if dst != "" {
		if out, err = os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err != nil {
			return 0, "", err
		}
		w = io.MultiWriter(out, h)
	}
	n, err := io.Copy(w, ctxReader{ctx, r})
	if out != nil {
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
