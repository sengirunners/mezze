package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Rclone is the rclone driver. Its remotes are defined only in the child's
// environment (RCLONE_CONFIG_<NAME>_<KEY>), which is all the child gets
// besides HOME, the cache directory and a config path that never exists:
// never the daemon's environment. Every error is masked for each secret it
// could quote.
type Rclone struct {
	bin   string
	root  string // "zmcrypt:" or "zms3:bucket/prefix" or "zmfile:/path"
	desc  string
	env   []string
	masks []string
}

// The remote names in the child's environment.
const (
	remoteS3    = "zms3"
	remoteFile  = "zmfile"
	remoteCrypt = "zmcrypt"
)

// callTimeout bounds one rclone call (a set's biggest file is a few hundred MB).
var callTimeout = 30 * time.Minute

func newRclone(ctx context.Context, o Opener, c Config, p Parsed) (*Rclone, error) {
	if o.Scratch == "" {
		return nil, errors.New("rclone needs a scratch directory")
	}
	if err := os.MkdirAll(filepath.Join(o.Scratch, "cache"), 0o700); err != nil {
		return nil, err
	}
	rd := o.Secrets
	if rd == nil {
		rd = secrets.Default()
	}
	r := &Rclone{bin: o.Bin, desc: c.String()}
	r.env = []string{
		"HOME=" + o.Scratch,
		"RCLONE_CONFIG=" + filepath.Join(o.Scratch, "none.conf"),
		"RCLONE_CACHE_DIR=" + filepath.Join(o.Scratch, "cache"),
		"PATH=/usr/bin:/bin",
	}
	get := func(what, ref string) (string, error) {
		rf, err := secrets.Parse(ref)
		if err != nil {
			return "", err
		}
		v, err := rd.Get(ctx, rf)
		if err != nil {
			return "", secrets.Prefix(err, what+": ")
		}
		if v.IsZero() {
			return "", api.Errorf(api.CodeBadRequest, "%s (%s) is empty", what, rf)
		}
		r.masks = append(r.masks, v.Reveal())
		return v.Reveal(), nil
	}
	set := func(remote, key, val string) {
		r.env = append(r.env, "RCLONE_CONFIG_"+strings.ToUpper(remote)+"_"+strings.ToUpper(key)+"="+val)
	}
	var base string
	switch p.Scheme {
	case "s3":
		ak, err := get("the access key", c.AccessKey)
		if err != nil {
			return nil, err
		}
		sk, err := get("the secret key", c.SecretKey)
		if err != nil {
			return nil, err
		}
		region := c.Region
		if region == "" {
			region = "auto"
		}
		provider := "Other"
		if e, _ := url.Parse(c.Endpoint); e != nil && strings.HasSuffix(e.Hostname(), ".r2.cloudflarestorage.com") {
			provider = "Cloudflare"
		}
		set(remoteS3, "type", "s3")
		set(remoteS3, "provider", provider)
		set(remoteS3, "env_auth", "false")
		set(remoteS3, "access_key_id", ak)
		set(remoteS3, "secret_access_key", sk)
		set(remoteS3, "endpoint", c.Endpoint)
		set(remoteS3, "region", region)
		set(remoteS3, "force_path_style", "true")
		// A key scoped to one bucket may not create or even look at buckets.
		set(remoteS3, "no_check_bucket", "true")
		base = remoteS3 + ":" + p.Bucket
		if p.Prefix != "" {
			base += "/" + p.Prefix
		}
	case "file":
		set(remoteFile, "type", "local")
		base = remoteFile + ":" + p.Path
	}
	r.root = base
	if c.Encrypt != "" {
		pass, err := get("the encryption key", c.Encrypt)
		if err != nil {
			return nil, err
		}
		salt, err := get("the encryption salt", c.EncryptSalt)
		if err != nil {
			return nil, err
		}
		ob, err := r.obscure(ctx, pass)
		if err != nil {
			return nil, err
		}
		ob2, err := r.obscure(ctx, salt)
		if err != nil {
			return nil, err
		}
		r.masks = append(r.masks, ob, ob2)
		set(remoteCrypt, "type", "crypt")
		set(remoteCrypt, "remote", base)
		set(remoteCrypt, "password", ob)
		set(remoteCrypt, "password2", ob2)
		set(remoteCrypt, "filename_encryption", "standard")
		set(remoteCrypt, "directory_name_encryption", "true")
		r.root = remoteCrypt + ":"
	}
	return r, nil
}

// obscure is rclone's reversible encoding of a crypt password, which its
// config wants (`rclone obscure -` reads it from stdin, never argv).
func (r *Rclone) obscure(ctx context.Context, pass string) (string, error) {
	out, err := r.run(ctx, strings.NewReader(pass), "obscure", "-")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Rclone) String() string { return r.desc }

func (r *Rclone) at(key string) string {
	if strings.HasSuffix(r.root, ":") {
		return r.root + key
	}
	if key == "" {
		return r.root
	}
	return r.root + "/" + key
}

// exit codes (rclone docs, "Exit code"): 3 directory not found, 4 file not found.
const (
	exitDirNotFound  = 3
	exitFileNotFound = 4
)

type rcloneErr struct {
	code int
	msg  string
}

func (e *rcloneErr) Error() string { return e.msg }

func (r *Rclone) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	full := append([]string{"--retries", "3", "--low-level-retries", "5", "-q"}, args...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	cmd.Env = r.env
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = "..." + msg[len(msg)-600:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, &rcloneErr{code: code, msg: "rclone " + args[0] + ": " + r.mask(msg)}
	}
	return out.Bytes(), nil
}

func (r *Rclone) mask(s string) string {
	for _, m := range r.masks {
		if m != "" {
			s = strings.ReplaceAll(s, m, "[secret]")
		}
	}
	return s
}

func notFound(err error) bool {
	var re *rcloneErr
	return errors.As(err, &re) && (re.code == exitDirNotFound || re.code == exitFileNotFound ||
		strings.Contains(re.msg, "directory not found") || strings.Contains(re.msg, "object not found"))
}

// exists asks whether key is an object: rclone's copyto of a missing
// source is not an error on every backend.
func (r *Rclone) exists(ctx context.Context, key string) error {
	out, err := r.run(ctx, nil, "lsjson", "--stat", "--no-mimetype", "--no-modtime", r.at(key))
	if notFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// S3 has no directories: a missing key may stat as an empty one.
	var st struct {
		Path  string
		IsDir bool
	}
	if json.Unmarshal(out, &st) != nil || st.Path == "" || st.IsDir {
		return ErrNotFound
	}
	return nil
}

func (r *Rclone) Put(ctx context.Context, local, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "copyto", local, r.at(key))
	return err
}

func (r *Rclone) Get(ctx context.Context, key, local string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return err
	}
	if err := r.exists(ctx, key); err != nil {
		return err
	}
	tmp := local + ".tmp"
	_, err := r.run(ctx, nil, "copyto", r.at(key), tmp)
	if notFound(err) {
		return ErrNotFound
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, local)
}

func (r *Rclone) Copy(ctx context.Context, from, to string) error {
	if err := CheckKey(from); err != nil {
		return err
	}
	if err := CheckKey(to); err != nil {
		return err
	}
	if err := r.exists(ctx, from); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "copyto", r.at(from), r.at(to))
	if notFound(err) {
		return ErrNotFound
	}
	return err
}

func (r *Rclone) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "lsjson", "-R", "--files-only", "--no-mimetype", "--no-modtime", r.at(strings.TrimSuffix(prefix, "/")))
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []struct {
		Path string
		Size int64
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("rclone lsjson: %w", err)
	}
	objs := make([]Object, 0, len(items))
	for _, it := range items {
		objs = append(objs, Object{Key: prefix + it.Path, Size: it.Size})
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

func (r *Rclone) DeletePrefix(ctx context.Context, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	if prefix == "" {
		return errors.New("refusing to delete every object")
	}
	_, err := r.run(ctx, nil, "purge", r.at(strings.TrimSuffix(prefix, "/")))
	if notFound(err) {
		return nil
	}
	return err
}
