// Package toolstest shares one download of the pinned tools between
// the opt-in real tests (ZM_TEST_DOWNLOAD=1): each is fetched once
// into a cache, then linked (or copied) into each test's own home, instead
// of a download per test. The cache is $ZM_TEST_TOOLS when set (kept, so
// several packages and runs share it), else one temp dir per test binary
// (call Cleanup from TestMain).
package toolstest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/tools"
)

// EnvCache names a directory the downloads are kept in across runs.
const EnvCache = "ZM_TEST_TOOLS"

var (
	mu  sync.Mutex
	tmp string // the per-binary cache when EnvCache is unset
)

func cacheDir() (string, error) {
	if d := os.Getenv(EnvCache); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	if tmp == "" {
		d, err := os.MkdirTemp("", "pctools")
		if err != nil {
			return "", err
		}
		tmp = d
	}
	return tmp, nil
}

// ProvisionTools puts the named pinned tools into home's tools directory,
// downloading them into the cache only the first time.
func ProvisionTools(ctx context.Context, home string, opt tools.Options, names ...string) error {
	mu.Lock()
	defer mu.Unlock()
	cache, err := cacheDir()
	if err != nil {
		return err
	}
	opt.Download = true
	// Another test binary (go test runs packages at once) may be fetching
	// into the same $ZM_TEST_TOOLS: wait for it rather than give up.
	tools.LockWait = 10 * time.Minute
	if _, err := tools.Ensure(ctx, cache, names, opt); err != nil {
		return err
	}
	if err := os.MkdirAll(tools.Dir(home), 0o700); err != nil {
		return err
	}
	for _, name := range append(append([]string(nil), names...), "manifest.json") {
		if err := linkOrCopy(filepath.Join(tools.Dir(cache), name), filepath.Join(tools.Dir(home), name)); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup removes the per-binary cache (not $ZM_TEST_TOOLS).
func Cleanup() {
	mu.Lock()
	defer mu.Unlock()
	if tmp != "" {
		os.RemoveAll(tmp)
		tmp = ""
	}
}

func linkOrCopy(from, to string) error {
	os.Remove(to)
	if err := os.Link(from, to); err == nil {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
