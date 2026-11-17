package integrations

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// readFile returns the file's bytes, or nil if it does not exist.
func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// apply fills f.Action, f.Diff and f.Backup and, when o.Apply is set, backs up
// the old file and writes the new one atomically (same dir, then rename).
func apply(f *FileChange, o Options) error {
	// A config file that is a symlink (say, into a dotfiles repo) is edited in
	// place at its target: the rename below would otherwise replace the link
	// with a regular file and leave the target stale.
	target, err := resolveLink(f.Path)
	if err != nil {
		return wrapPath(f.Path, err)
	}
	f.Path = target
	switch {
	case f.before != nil && bytes.Equal(f.before, f.after):
		f.Action, f.Changes = ActionUnchanged, nil
		return nil
	case f.before == nil && f.after == nil:
		f.Action, f.Changes = ActionUnchanged, nil
		return nil
	case f.before == nil:
		f.Action = ActionCreate
	default:
		f.Action = ActionUpdate
		f.Backup = backupName(f.Path, o)
	}
	f.Diff = unifiedDiff(f.Path, f.before, f.after)
	if !o.Apply {
		return nil
	}
	mode := fs.FileMode(0o600) // new config files: user-only (they can hold account details)
	if st, err := os.Stat(f.Path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	if f.Backup != "" {
		if err := os.WriteFile(f.Backup, f.before, 0o600); err != nil {
			return fmt.Errorf("backup %s: %w", f.Path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".zm-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(f.after); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return err
	}
	f.Written = true
	return nil
}

// resolveLink follows symlinks on the file itself (not its parent dirs) and
// returns the path of the regular file, or of the missing target of a dangling
// link, that a write should land on.
func resolveLink(path string) (string, error) {
	for range 40 {
		st, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && st.Mode()&fs.ModeSymlink == 0) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = filepath.Clean(dest)
	}
	return "", errors.New("too many levels of symbolic links")
}

// backupName is <path>.zm-backup-<stamp>, numbered if that name is taken.
func backupName(path string, o Options) string {
	base := path + ".zm-backup-" + o.Now().Format("20060102-150405")
	name := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name
		}
		name = base + "-" + strconv.Itoa(i)
	}
}

func wrapPath(path string, err error) error { return fmt.Errorf("%s: %w", path, err) }
