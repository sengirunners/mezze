package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DirName is the tools directory inside $ZM_HOME; adapters get it first
// on PATH.
const DirName = "tools"

// manifestName records what Ensure installed (tool -> version and hashes).
const manifestName = "manifest.json"

// maxArchive bounds one download (the pinned archives are tens of MB).
const maxArchive = 512 << 20

// Dir is $ZM_HOME/tools.
func Dir(home string) string { return filepath.Join(home, DirName) }

// Path is where tool lives once provisioned.
func Path(home, tool string) string { return filepath.Join(Dir(home), tool) }

// Installed is one manifest entry.
type Installed struct {
	Version       string `json:"version"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
	Installed     string `json:"installed"`
}

// ErrMissing means the tool is not provisioned (and downloading was not allowed).
var ErrMissing = errors.New("not installed")

// ErrBusy means another zm held the tools lock (fetching) for longer
// than LockWait.
var ErrBusy = errors.New("another zm is fetching tools")

// LockWait bounds how long Ensure waits for another zm that is fetching
// tools into the same home (it says so at once, then waits this long).
var LockWait = 30 * time.Second

// MismatchError is a downloaded archive (or an installed binary) whose
// sha256 is not the pinned one. Nothing is installed or used.
type MismatchError struct {
	Tool, What, Want, Got string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s: %s sha256 %s does not match the pinned %s; refusing it", e.Tool, e.What, short(e.Got), short(e.Want))
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "..."
	}
	return h
}

func readManifest(home string) map[string]Installed {
	m := map[string]Installed{}
	if b, err := os.ReadFile(filepath.Join(Dir(home), manifestName)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writeManifest(home string, m map[string]Installed) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(Dir(home), manifestName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Verify checks that tool is provisioned at the pinned version for this
// platform: the manifest names the pinned archive, and the binary on disk
// still hashes to what was extracted from it. It returns the binary's path.
// A file changed behind zm's back is a MismatchError (re-provision with
// Ensure after removing it); no manifest entry is ErrMissing.
func Verify(home, tool string) (string, error) {
	pin, ok := PinFor(tool)
	if !ok {
		return "", fmt.Errorf("%s: no pinned release for %s/%s", tool, runtime.GOOS, runtime.GOARCH)
	}
	p := Path(home, tool)
	in, ok := readManifest(home)[tool]
	if !ok {
		return p, fmt.Errorf("%s %s: %w", tool, pin.Version, ErrMissing)
	}
	if in.Version != pin.Version || in.ArchiveSHA256 != pin.SHA256 {
		// Another pin (zm was upgraded): the old binary is not this pin.
		return p, fmt.Errorf("%s %s (have %s): %w", tool, pin.Version, in.Version, ErrMissing)
	}
	got, err := fileSHA256(p)
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("%s %s: %w", tool, pin.Version, ErrMissing)
	}
	if err != nil {
		return p, err
	}
	if got != in.BinarySHA256 {
		return p, &MismatchError{Tool: tool, What: "installed binary " + p, Want: in.BinarySHA256, Got: got}
	}
	return p, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Options for Ensure.
type Options struct {
	// Download allows fetching a missing tool. Without it a missing tool
	// is ErrMissing (--no-download).
	Download bool
	// Progress gets one line per download (may be nil).
	Progress io.Writer
	// Client fetches the archives (default: http.DefaultClient with a 10
	// minute bound per archive).
	Client *http.Client
}

// Result says what Ensure did for one tool.
type Result struct {
	Tool       string        `json:"tool"`
	Version    string        `json:"version"`
	Path       string        `json:"path"`
	Downloaded bool          `json:"downloaded"`
	Bytes      int64         `json:"bytes,omitempty"`
	Took       time.Duration `json:"took,omitempty"`
}

// Ensure provisions each tool (verified, or downloaded and verified when
// allowed), downloading missing ones in parallel. Every archive is checked
// against its pin before extraction; a mismatch is a MismatchError. It is
// all or nothing: downloads extract to staging files that are installed
// (and recorded in the manifest) only when every requested tool succeeded,
// so a failure leaves nothing behind. A second Ensure on the same home
// while one is fetching says so on Progress and waits up to LockWait
// (then ErrBusy).
func Ensure(ctx context.Context, home string, names []string, opt Options) ([]Result, error) {
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lock(ctx, filepath.Join(Dir(home), ".lock"), LockWait, func() {
		if opt.Progress != nil {
			fmt.Fprintf(opt.Progress, "zm: another zm is fetching tools into %s; waiting up to %s\n", Dir(home), LockWait)
		}
	})
	if errors.Is(err, ErrBusy) {
		return nil, fmt.Errorf("%w into %s (waited %s); run again when it is done", ErrBusy, Dir(home), LockWait)
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	out := make([]Result, len(names))
	var need []int
	for i, n := range names {
		pin, ok := PinFor(n)
		if !ok {
			return nil, fmt.Errorf("%s: no pinned release for %s/%s (install it yourself and pass its path)", n, runtime.GOOS, runtime.GOARCH)
		}
		out[i] = Result{Tool: n, Version: pin.Version, Path: Path(home, n)}
		_, err := Verify(home, n)
		switch {
		case err == nil:
			continue
		case errors.Is(err, ErrMissing):
			if !opt.Download {
				return nil, fmt.Errorf("%s %s is not in %s and downloading is off: %w", n, pin.Version, Dir(home), ErrMissing)
			}
			need = append(need, i)
		default:
			return nil, err
		}
	}
	if len(need) == 0 {
		return out, nil
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		got  = map[string]Installed{}
	)
	for _, i := range need {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pin, _ := PinFor(names[i])
			start := time.Now()
			n, in, err := fetch(ctx, home, pin, opt)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got[pin.Tool] = in
			out[i].Downloaded, out[i].Bytes, out[i].Took = true, n, time.Since(start)
			if opt.Progress != nil {
				fmt.Fprintf(opt.Progress, "zm: fetched %s %s (%.0f MB, sha256 ok) into %s in %.1fs\n",
					pin.Tool, pin.Version, float64(n)/(1<<20), Dir(home), time.Since(start).Seconds())
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		for k := range got {
			os.Remove(staging(home, k))
		}
		return nil, errors.Join(errs...)
	}
	m := readManifest(home)
	for k, v := range got {
		if err := os.Rename(staging(home, k), Path(home, k)); err != nil {
			for k := range got {
				os.Remove(staging(home, k))
			}
			return nil, err
		}
		m[k] = v
	}
	if err := writeManifest(home, m); err != nil {
		return nil, err
	}
	return out, nil
}

// staging is where a downloaded tool waits until every download of one
// Ensure has succeeded.
func staging(home, tool string) string { return Path(home, tool) + ".new" }

// fetch downloads pin's archive into the tools dir, checks its sha256 and
// extracts the member to its staging path (Ensure installs it).
func fetch(ctx context.Context, home string, pin Pin, opt Options) (int64, Installed, error) {
	if !strings.HasPrefix(pin.URL, "https://") && !testURLs {
		return 0, Installed{}, fmt.Errorf("%s: pinned URL %q is not https", pin.Tool, pin.URL)
	}
	c := opt.Client
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.URL, nil)
	if err != nil {
		return 0, Installed{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, Installed{}, fmt.Errorf("%s %s: download: %w", pin.Tool, pin.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, Installed{}, fmt.Errorf("%s %s: download %s: %s", pin.Tool, pin.Version, pin.URL, resp.Status)
	}
	arch, err := os.CreateTemp(Dir(home), "."+pin.Tool+"-*.tar.gz")
	if err != nil {
		return 0, Installed{}, err
	}
	defer os.Remove(arch.Name())
	defer arch.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(arch, h), io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return n, Installed{}, fmt.Errorf("%s %s: download: %w", pin.Tool, pin.Version, err)
	}
	if n > maxArchive {
		return n, Installed{}, fmt.Errorf("%s %s: archive larger than %d MB", pin.Tool, pin.Version, maxArchive>>20)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != pin.SHA256 {
		return n, Installed{}, &MismatchError{Tool: pin.Tool, What: "downloaded archive " + pin.URL, Want: pin.SHA256, Got: sum}
	}
	if _, err := arch.Seek(0, io.SeekStart); err != nil {
		return n, Installed{}, err
	}
	var binSum string
	if strings.HasSuffix(pin.URL, ".zip") {
		binSum, err = extractZip(arch, n, pin, staging(home, pin.Tool))
	} else {
		binSum, err = extract(arch, pin, staging(home, pin.Tool))
	}
	if err != nil {
		return n, Installed{}, err
	}
	return n, Installed{Version: pin.Version, ArchiveSHA256: pin.SHA256, BinarySHA256: binSum,
		Installed: time.Now().UTC().Format(time.RFC3339)}, nil
}

// extractZip is extract for a .zip archive of size n (rclone ships zips).
func extractZip(r io.ReaderAt, n int64, pin Pin, dst string) (string, error) {
	zr, err := zip.NewReader(r, n)
	if err != nil {
		return "", fmt.Errorf("%s: archive: %w", pin.Tool, err)
	}
	for _, f := range zr.File {
		if strings.TrimPrefix(f.Name, "./") != pin.Member || !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("%s: archive: %w", pin.Tool, err)
		}
		defer rc.Close()
		return writeMember(rc, dst)
	}
	return "", fmt.Errorf("%s: %s not in the archive", pin.Tool, pin.Member)
}

// writeMember writes one archive member to dst (0755, atomically) and
// returns its sha256.
func writeMember(r io.Reader, dst string) (string, error) {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxArchive))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extract writes the archive's member to dst (0755, atomically) and returns its sha256.
// A pin with no Member is a release that ships the executable itself.
func extract(r io.Reader, pin Pin, dst string) (string, error) {
	if pin.Member == "" {
		return writeMember(r, dst)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("%s: archive: %w", pin.Tool, err)
	}
	tr := tar.NewReader(gz)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("%s: %s not in the archive", pin.Tool, pin.Member)
		}
		if err != nil {
			return "", fmt.Errorf("%s: archive: %w", pin.Tool, err)
		}
		if strings.TrimPrefix(hd.Name, "./") != pin.Member || hd.Typeflag != tar.TypeReg {
			continue
		}
		return writeMember(tr, dst)
	}
}

// testURLs lets tests serve archives over plain http (httptest).
var testURLs = false
