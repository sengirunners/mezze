// Package offsite is the off-site target brick of the homes module (SPEC 17.9
// "Off-site home backups"): somewhere outside this machine that keeps a home's
// backup sets as objects under keys. A Target puts, gets, copies, lists and
// deletes objects; set.go lays a home's sets out on any Target and restores
// one into a fresh home.
//
// Drivers:
//
//	file:///abs/path   Dir: a directory (another disk, a mounted share); also the
//	                   driver every other one is checked against
//	s3://bucket[/pre]  Rclone: rclone's s3 backend (Cloudflare R2, AWS, MinIO, ...)
//
// Rclone serves any target that names an encryption key, a file:// one too:
// rclone's crypt backend encrypts names and contents on this machine before
// anything leaves it. A native S3 client is the second driver we expect (no
// process per call), if rclone's start-up cost ever matters at a set every few
// minutes.
//
// Why rclone, not our own S3 code or a linked SDK: it is the maintained,
// widely used tool for exactly this, with client-side encryption we would
// otherwise write; it runs as a pinned process (internal/tools, sha256-checked)
// with an environment we build, so the daemon links no cloud SDK and a
// credential reaches rclone only through its environment (never argv, a
// config file or a log). Credentials are secrets.Refs read when a Target
// opens.
//
// Conformance: offsitetest.Run; every driver passes it.
package offsite

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Object is one stored object: its key under the target's root, and size.
type Object struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// Target keeps objects by key. Keys are slash-separated relative paths
// (CheckKey). A missing object or prefix is ErrNotFound for Get and Copy; an
// empty List; DeletePrefix of nothing succeeds.
type Target interface {
	// Put stores the local file at key, replacing any object there.
	Put(ctx context.Context, local, key string) error
	// Get writes the object at key to the local path (mode 0600).
	Get(ctx context.Context, key, local string) error
	// Copy copies an object to another key, server side where the driver can.
	Copy(ctx context.Context, from, to string) error
	// List is every object under prefix ("" = all), sorted by key.
	List(ctx context.Context, prefix string) ([]Object, error)
	// DeletePrefix removes every object under prefix, which must end in "/".
	DeletePrefix(ctx context.Context, prefix string) error
	// String names the target without any secret.
	String() string
}

// ErrNotFound is a key with no object.
var ErrNotFound = errors.New("no such object")

var keyRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// CheckKey refuses a key that is not a clean relative path of plain names.
func CheckKey(key string) error {
	if !keyRE.MatchString(key) {
		return fmt.Errorf("bad object key %q", key)
	}
	for _, p := range strings.Split(key, "/") {
		if p == "." || p == ".." {
			return fmt.Errorf("bad object key %q", key)
		}
	}
	return nil
}

func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("prefix %q does not end in /", prefix)
	}
	return CheckKey(strings.TrimSuffix(prefix, "/"))
}

// Config names a target and the secrets it needs (references only: SPEC 2's
// secrets brick reads them when the target opens).
type Config struct {
	// URL is file:///abs/path or s3://bucket[/prefix].
	URL string `json:"url"`
	// Endpoint is the S3 API's URL (R2: https://<account>.r2.cloudflarestorage.com).
	Endpoint string `json:"endpoint,omitempty"`
	// Region: "auto" when empty (R2's).
	Region string `json:"region,omitempty"`
	// AccessKey and SecretKey are secret references (keychain:, file:, env:).
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	// Encrypt is the secret reference of the encryption passphrase ("" = none);
	// EncryptSalt the reference of a second secret, rclone crypt's salt
	// (password2), required with Encrypt so no set uses rclone's built-in one.
	Encrypt     string `json:"encrypt,omitempty"`
	EncryptSalt string `json:"encrypt_salt,omitempty"`
}

// Kind is the target's scheme: "file" or "s3".
func (c Config) Kind() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return u.Scheme
}

// Parsed is a checked Config's parts.
type Parsed struct {
	Scheme string // file or s3
	Path   string // file: the directory
	Bucket string // s3
	Prefix string // s3: the folder in the bucket, no slashes at either end
}

var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Check parses c and refuses what cannot work: an unknown scheme, a secret
// inline (a URL with a user or password), an S3 target without an endpoint
// or keys, and a reference the secrets brick does not read.
func (c Config) Check() (Parsed, error) {
	bad := func(f string, a ...any) (Parsed, error) {
		return Parsed{}, api.Errorf(api.CodeBadRequest, f, a...)
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return bad("target %q is not a URL", c.URL)
	}
	if u.User != nil {
		return bad("a target never carries a login: name the keys with --access-key and --secret-key")
	}
	var p Parsed
	switch u.Scheme {
	case "file":
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return bad("target %q: want file:///absolute/path", c.URL)
		}
		p = Parsed{Scheme: "file", Path: filepath.Clean(u.Path)}
	case "s3":
		if !bucketRE.MatchString(u.Host) {
			return bad("target %q: %q is not a bucket name", c.URL, u.Host)
		}
		p = Parsed{Scheme: "s3", Bucket: u.Host, Prefix: strings.Trim(u.Path, "/")}
		if p.Prefix != "" {
			if err := CheckKey(p.Prefix); err != nil {
				return bad("target %q: %v", c.URL, err)
			}
		}
		e, err := url.Parse(c.Endpoint)
		if c.Endpoint == "" || err != nil || (e.Scheme != "https" && !(e.Scheme == "http" && loopback(e.Hostname()))) || e.User != nil {
			return bad("an s3 target needs --endpoint https://... (R2: https://<account id>.r2.cloudflarestorage.com)")
		}
		if c.AccessKey == "" || c.SecretKey == "" {
			return bad("an s3 target needs --access-key and --secret-key (secret references: keychain:, file: or env:)")
		}
	default:
		return Parsed{}, api.Errorf(api.CodeBadRequest, "unknown off-site target %q", c.URL).
			WithHint("s3://bucket/folder (with --endpoint) or file:///absolute/path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return bad("target %q: no query or fragment", c.URL)
	}
	if (c.Encrypt == "") != (c.EncryptSalt == "") {
		return bad("--encrypt and --encrypt-salt go together (two secrets, both needed to read the sets)")
	}
	for _, r := range []string{c.AccessKey, c.SecretKey, c.Encrypt, c.EncryptSalt} {
		if r == "" {
			continue
		}
		if _, err := secrets.Parse(r); err != nil {
			return Parsed{}, err
		}
	}
	return p, nil
}

func loopback(h string) bool { return h == "127.0.0.1" || h == "localhost" || h == "::1" }

// String is the target with no secret: its URL and endpoint host, and
// whether it is encrypted.
func (c Config) String() string {
	s := c.URL
	if e, err := url.Parse(c.Endpoint); err == nil && e.Host != "" {
		s += " at " + e.Host
	}
	if c.Encrypt != "" {
		s += " (encrypted)"
	}
	return s
}

// Opener opens a Config's target. Bin is the pinned rclone (needed by s3
// targets and encrypted ones); Scratch is a private directory rclone may use
// (its cache and a config file that never exists).
type Opener struct {
	Secrets *secrets.Reader
	Bin     string
	Scratch string
}

// Open checks c, reads its secrets and returns its Target.
func (o Opener) Open(ctx context.Context, c Config) (Target, error) {
	p, err := c.Check()
	if err != nil {
		return nil, err
	}
	if p.Scheme == "file" && c.Encrypt == "" {
		return Dir{Root: p.Path}, nil
	}
	if o.Bin == "" {
		return nil, api.Errorf(api.CodeUnavailable, "this target needs rclone, which is not installed").
			WithHint("zm backup set fetches the pinned rclone")
	}
	return newRclone(ctx, o, c, p)
}
