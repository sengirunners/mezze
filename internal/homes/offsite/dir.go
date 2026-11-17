package offsite

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir is a directory target (file:///path): an object is a file under Root.
// Root must exist (an unmounted volume is an error, not a new folder on the
// boot disk). Files are written to a temporary name and renamed, so a key
// that exists holds a whole object.
type Dir struct{ Root string }

func (d Dir) String() string { return "file://" + d.Root }

func (d Dir) path(key string) (string, error) {
	if err := CheckKey(key); err != nil {
		return "", err
	}
	return filepath.Join(d.Root, filepath.FromSlash(key)), nil
}

func (d Dir) root() error {
	st, err := os.Stat(d.Root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New(d.Root + " is not a directory")
	}
	return nil
}

func (d Dir) Put(ctx context.Context, local, key string) error {
	if err := d.root(); err != nil {
		return err
	}
	dst, err := d.path(key)
	if err != nil {
		return err
	}
	return copyFile(ctx, local, dst)
}

func (d Dir) Get(ctx context.Context, key, local string) error {
	src, err := d.path(key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return copyFile(ctx, src, local)
}

func (d Dir) Copy(ctx context.Context, from, to string) error {
	src, err := d.path(from)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	dst, err := d.path(to)
	if err != nil {
		return err
	}
	return copyFile(ctx, src, dst)
}

func (d Dir) List(_ context.Context, prefix string) ([]Object, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	if err := d.root(); err != nil {
		return nil, err
	}
	start := filepath.Join(d.Root, filepath.FromSlash(prefix))
	var out []Object
	err := filepath.WalkDir(start, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if de.IsDir() || strings.HasSuffix(de.Name(), ".tmp") {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil {
			return err
		}
		out = append(out, Object{Key: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

func (d Dir) DeletePrefix(_ context.Context, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	if prefix == "" {
		return errors.New("refusing to delete every object")
	}
	return os.RemoveAll(filepath.Join(d.Root, filepath.FromSlash(prefix)))
}

// copyFile copies src to dst (0600, parent directories 0700) through a
// temporary name.
func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, ctxReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
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
