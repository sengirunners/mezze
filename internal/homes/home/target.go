// Package home holds the bricks behind `zm tracker backup` and `zm issues
// restore` (SPEC 17.9): the backup targets a tracker's copies go to, the
// service units zm keeps a long-running process alive with (zm tunnel's
// cloudflared), and an atomic JSON write. It knows no adapter, project or
// route: the homes module drives it.
package home

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Backup target kinds.
const (
	KindFile   = "file"
	KindICloud = "icloud"
)

// ICloudRel is where `icloud` backups go, under the user's home.
const ICloudRel = "Library/Mobile Documents/com~apple~CloudDocs/zm-backups"

// Target is a parsed backup target. Credentials are never part of one: a
// target naming a password is refused.
type Target struct {
	Kind string
	Path string // absolute
}

// Parse reads a target: file:///abs/path (or an absolute path, or ~/...),
// icloud[:subdir]. userHome resolves ~ and icloud.
func Parse(s, userHome string) (Target, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Target{}, fmt.Errorf("empty target")
	case s == KindICloud || strings.HasPrefix(s, KindICloud+":"):
		sub := strings.TrimPrefix(strings.TrimPrefix(s, KindICloud), ":")
		if strings.Contains(sub, "..") {
			return Target{}, fmt.Errorf("icloud subdirectory %q may not contain ..", sub)
		}
		if userHome == "" {
			return Target{}, fmt.Errorf("icloud: no home directory")
		}
		p := filepath.Join(userHome, ICloudRel)
		if sub != "" {
			p = filepath.Join(p, sub)
		}
		return Target{Kind: KindICloud, Path: p}, nil
	case strings.HasPrefix(s, "/"), s == "~", strings.HasPrefix(s, "~/"):
		return fileTarget(expand(s, userHome))
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return Target{}, fmt.Errorf("target %q: want file:///path or icloud", s)
	}
	if _, has := u.User.Password(); has {
		return Target{}, fmt.Errorf("target %q carries a password: never put one in a target", redactURL(u))
	}
	if u.Scheme == "file" {
		if u.Host != "" && u.Host != "localhost" {
			return Target{}, fmt.Errorf("file target %q: want file:///absolute/path", s)
		}
		return fileTarget(u.Path)
	}
	return Target{}, fmt.Errorf("target %q: unknown scheme %q (want file:///path or icloud)", s, u.Scheme)
}

func fileTarget(p string) (Target, error) {
	if !filepath.IsAbs(p) {
		return Target{}, fmt.Errorf("file target %q is not absolute", p)
	}
	return Target{Kind: KindFile, Path: filepath.Clean(p)}, nil
}

func expand(p, userHome string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(userHome, strings.TrimPrefix(p, "~"))
	}
	return p
}

func redactURL(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.User(c.User.Username())
	}
	return c.String()
}

// String is the canonical spelling (what zm stores and prints).
func (t Target) String() string {
	switch t.Kind {
	case KindFile:
		return "file://" + t.Path
	case KindICloud:
		return KindICloud + ":" + t.Path
	}
	return t.Kind
}

// Spec is what the user typed back, short: icloud stays "icloud".
func (t Target) Spec(userHome string) string {
	if t.Kind == KindICloud {
		base := filepath.Join(userHome, ICloudRel)
		if t.Path == base {
			return KindICloud
		}
		if rel, err := filepath.Rel(base, t.Path); err == nil && !strings.HasPrefix(rel, "..") {
			return KindICloud + ":" + rel
		}
	}
	return t.String()
}

// BuiltAsBackup is nil when this zm can back up to t, else why not.
func (t Target) BuiltAsBackup() error {
	switch t.Kind {
	case KindFile:
		return nil
	case KindICloud:
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("icloud is a macOS folder; use file:///path on %s", runtime.GOOS)
		}
		return nil
	}
	return fmt.Errorf("%s is not a backup target", t.Kind)
}

// WriteJSON writes v to path as indented JSON (mode 0600), through a
// temporary file and a rename so a reader never sees half a file.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
