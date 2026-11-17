package offsite

// A home's backup sets on a Target (SPEC 17.9 "Off-site home backups"):
//
//	<home>/<stamp>/files/<path>.gz   each file, gzipped, then encrypted with the
//	                                 home's backup key (bkcrypt; the manifest
//	                                 names the cipher and key id), and again by
//	                                 the target when it has a key of its own
//	<home>/<stamp>/manifest.json     written last: a set without one is not
//	                                 a set, only a leftover the next run removes
//
// The manifest names every file with its size and the sha256 of its content,
// so a restore proves each file before it puts any in place.

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

// StampLayout names a set; stamps sort by time.
const StampLayout = "20060102T150405Z"

const manifestName = "manifest.json"

// ManifestVersion is the layout this zm writes and reads (2: files may be
// encrypted, Cipher; a zm that reads only 1 refuses such a set as newer
// instead of failing to gunzip it).
const ManifestVersion = 2

var homeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// CheckHome refuses a home id that is not a short plain name.
func CheckHome(home string) error {
	if !homeRE.MatchString(home) {
		return api.Errorf(api.CodeBadRequest, "home id %q: want lowercase letters, digits, '.', '_' or '-', at most 64", home)
	}
	return nil
}

// File is one file of a set.
type File struct {
	Path   string `json:"path"`   // relative to $ZM_HOME, slash-separated
	Bytes  int64  `json:"bytes"`  // its size
	SHA256 string `json:"sha256"` // of its content
	Stored int64  `json:"stored"` // the gzipped object's size
}

// Manifest describes a whole set.
type Manifest struct {
	V       int       `json:"v"`
	Home    string    `json:"home"`
	Stamp   string    `json:"stamp"`
	Created time.Time `json:"created"`
	Version string    `json:"version,omitempty"` // the zm that wrote it
	Commit  string    `json:"commit,omitempty"`
	Files   []File    `json:"files"`
	Stored  int64     `json:"stored"` // every object of the set
	// Cipher is bkcrypt.Cipher when every file object is encrypted with the
	// home's backup key KeyID ("" = plain gzip, a set from before).
	Cipher string `json:"cipher,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// Staged is one local file for a set, at its path in the home.
type Staged struct{ Path, Local string }

func setPrefix(home, stamp string) string { return home + "/" + stamp + "/" }
func fileKey(home, stamp, path string) string {
	return setPrefix(home, stamp) + "files/" + path + ".gz"
}

// Upload writes files as set stamp of home and returns its manifest, each
// file encrypted with key (nil: plain, tests and nothing else). A file
// whose content equals its namesake in prev (the last good set, still on the
// target) is copied there server side instead of uploaded, if prev was
// encrypted the same way. The gzipped files are written next to each Local
// and removed after.
func Upload(ctx context.Context, t Target, m Manifest, files []Staged, prev *Manifest, key *bkcrypt.Key) (Manifest, error) {
	if err := CheckHome(m.Home); err != nil {
		return m, err
	}
	if len(files) == 0 {
		return m, errors.New("a set needs at least one file")
	}
	m.V, m.Files, m.Stored, m.Cipher, m.KeyID = ManifestVersion, nil, 0, "", ""
	if key != nil {
		m.Cipher, m.KeyID = bkcrypt.Cipher, key.ID()
	}
	old := map[string]File{}
	// A plain object, or one under another key, is never carried into this set.
	if prev != nil && prev.Cipher == m.Cipher && prev.KeyID == m.KeyID {
		for _, f := range prev.Files {
			old[f.Path] = f
		}
	}
	for _, s := range files {
		if err := CheckKey(s.Path); err != nil {
			return m, err
		}
		if !HomeFile(s.Path) {
			return m, fmt.Errorf("%s is not a file a set holds", s.Path)
		}
		gz := s.Local + ".gz"
		f, err := gzipFile(s.Local, gz, key)
		if err != nil {
			return m, fmt.Errorf("%s: %w", s.Path, err)
		}
		f.Path = s.Path
		obj := fileKey(m.Home, m.Stamp, s.Path)
		copied := false
		if o, ok := old[s.Path]; ok && o.SHA256 == f.SHA256 && o.Bytes == f.Bytes {
			if err := t.Copy(ctx, fileKey(prev.Home, prev.Stamp, s.Path), obj); err == nil {
				copied, f.Stored = true, o.Stored
			}
		}
		if !copied {
			err = t.Put(ctx, gz, obj)
		}
		os.Remove(gz)
		if err != nil {
			return m, fmt.Errorf("%s: %w", s.Path, err)
		}
		m.Files = append(m.Files, f)
		m.Stored += f.Stored
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	mf := filepath.Join(filepath.Dir(files[0].Local), "."+manifestName)
	if err := os.WriteFile(mf, b, 0o600); err != nil {
		return m, err
	}
	defer os.Remove(mf)
	m.Stored += int64(len(b))
	if err := t.Put(ctx, mf, setPrefix(m.Home, m.Stamp)+manifestName); err != nil {
		return m, fmt.Errorf("the manifest: %w", err)
	}
	return m, nil
}

// gzipFile writes src gzipped (then encrypted under key, when not nil) to
// dst (0600) and returns src's size and sha256 and dst's size.
func gzipFile(src, dst string, key *bkcrypt.Key) (File, error) {
	in, err := os.Open(src)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	var w io.Writer = out
	var ew io.WriteCloser
	if key != nil {
		if ew, err = bkcrypt.NewWriter(out, key); err != nil {
			out.Close()
			os.Remove(dst)
			return File{}, err
		}
		w = ew
	}
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	n, err := io.Copy(io.MultiWriter(zw, h), in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if ew != nil {
		if cerr := ew.Close(); err == nil {
			err = cerr
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return File{}, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return File{}, err
	}
	return File{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), Stored: st.Size()}, nil
}

// SetInfo is a set found on a target.
type SetInfo struct {
	Stamp    string `json:"stamp"`
	Complete bool   `json:"complete"` // it has its manifest
	Stored   int64  `json:"stored"`   // its objects' bytes
	Objects  int    `json:"objects"`
}

// Sets lists home's sets on t, oldest first.
func Sets(ctx context.Context, t Target, home string) ([]SetInfo, error) {
	if err := CheckHome(home); err != nil {
		return nil, err
	}
	objs, err := t.List(ctx, home+"/")
	if err != nil {
		return nil, err
	}
	by := map[string]*SetInfo{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, home+"/")
		stamp, name, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		if _, err := time.Parse(StampLayout, stamp); err != nil {
			continue
		}
		s := by[stamp]
		if s == nil {
			s = &SetInfo{Stamp: stamp}
			by[stamp] = s
		}
		s.Objects++
		s.Stored += o.Size
		s.Complete = s.Complete || name == manifestName
	}
	out := make([]SetInfo, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp < out[j].Stamp })
	return out, nil
}

// Policy is how many sets retention keeps: the newest Keep, plus the newest
// of each of the last Hourly hours and of each of the last Daily days (UTC).
type Policy struct {
	Keep   int `json:"keep"`
	Hourly int `json:"keep_hourly"`
	Daily  int `json:"keep_daily"`
}

// DefaultPolicy keeps the last 8 sets, a day of hourly ones and a month of
// daily ones.
var DefaultPolicy = Policy{Keep: 8, Hourly: 24, Daily: 30}

// Retain is the stamps to delete from sets so that p holds. current (the set
// just written) is always kept; an
// incomplete set other than current is a leftover and goes. A complete set
// newer than current is kept too (current is then not the newest).
func Retain(sets []SetInfo, current string, now time.Time, p Policy) []string {
	var complete []SetInfo
	var drop []string
	for _, s := range sets {
		switch {
		case s.Stamp == current && !s.Complete:
		case !s.Complete:
			drop = append(drop, s.Stamp)
		default:
			complete = append(complete, s)
		}
	}
	keep := map[string]bool{current: true}
	if n := len(complete); n > 0 && complete[n-1].Stamp > current {
		keep[complete[n-1].Stamp] = true // a newer set than current: never the last one to go
	}
	for i := len(complete) - 1; i >= 0 && len(complete)-i <= p.Keep; i-- {
		keep[complete[i].Stamp] = true
	}
	bucket := func(layout string, n int, step time.Duration) {
		if n <= 0 {
			return
		}
		seen := map[string]bool{}
		oldest := now.Add(-time.Duration(n) * step)
		for i := len(complete) - 1; i >= 0; i-- {
			t, _ := time.Parse(StampLayout, complete[i].Stamp)
			if t.Before(oldest) {
				break
			}
			b := t.Format(layout)
			if !seen[b] {
				seen[b] = true
				keep[complete[i].Stamp] = true
			}
		}
	}
	bucket("2006010215", p.Hourly, time.Hour)
	bucket("20060102", p.Daily, 24*time.Hour)
	for _, s := range complete {
		if !keep[s.Stamp] {
			drop = append(drop, s.Stamp)
		}
	}
	sort.Strings(drop)
	return drop
}

// DeleteSet removes set stamp of home.
func DeleteSet(ctx context.Context, t Target, home, stamp string) error {
	return t.DeletePrefix(ctx, setPrefix(home, stamp))
}

// ReadManifest fetches set stamp's manifest through scratch.
func ReadManifest(ctx context.Context, t Target, home, stamp, scratch string) (Manifest, error) {
	var m Manifest
	p := filepath.Join(scratch, ".manifest-"+stamp+".json")
	defer os.Remove(p)
	if err := t.Get(ctx, setPrefix(home, stamp)+manifestName, p); err != nil {
		return m, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("set %s: manifest: %w", stamp, err)
	}
	if m.V > ManifestVersion {
		return m, api.Errorf(api.CodeConflict, "set %s was written by a newer zm (layout %d); restore it with that zm", stamp, m.V)
	}
	if m.Home != home || m.Stamp != stamp {
		return m, fmt.Errorf("set %s: its manifest names %s/%s", stamp, m.Home, m.Stamp)
	}
	return m, nil
}

// Pick is the set to restore: the newest complete set whose stamp starts
// with at ("" = the newest).
func Pick(sets []SetInfo, at string) (string, error) {
	for i := len(sets) - 1; i >= 0; i-- {
		if sets[i].Complete && strings.HasPrefix(sets[i].Stamp, at) {
			return sets[i].Stamp, nil
		}
	}
	if at != "" {
		return "", api.Errorf(api.CodeNotFound, "no complete set whose stamp starts with %q", at)
	}
	return "", api.Errorf(api.CodeNotFound, "no complete set")
}

// Restore fetches set stamp of home into the directory dst, which must be
// empty but for tools/ (where the restoring zm keeps rclone) and secrets/
// (where the backup key may wait). An encrypted
// set needs key, the backup key it names (nil: only a plain set restores).
// Every file is
// fetched into dst/.restore first and checked against the manifest (size,
// sha256; a database also with sqlbackup.Check); only when all pass are
// they moved into place, after a new restore generation is written
// (config.FileRestored). dry: check, then remove everything fetched.
// It holds dst's daemon lock throughout: no daemon (a CLI's
// autostart, an agent hook) can start on dst and write a registry the
// restore would then rename over while it is open.
func Restore(ctx context.Context, t Target, home, stamp, dst string, dry bool, key *bkcrypt.Key) (Manifest, error) {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return Manifest{}, err
	}
	release, err := config.LockHome(dst)
	if err != nil {
		return Manifest{}, api.Errorf(api.CodeConflict, "%s is in use by a daemon or another restore: restore only into a home no daemon runs on (%v)", dst, err)
	}
	defer release()
	des, err := os.ReadDir(dst)
	if err != nil {
		return Manifest{}, err
	}
	for _, de := range des {
		// secrets/ may hold the backup key, put there to restore with; a set
		// never writes into it.
		if n := de.Name(); n != "tools" && n != "secrets" && n != ".restore" && n != config.FileHomeLock && n != "autostart.lock" {
			return Manifest{}, api.Errorf(api.CodeConflict, "%s is not empty (%s): restore only into a fresh home", dst, n)
		}
	}
	stage := filepath.Join(dst, ".restore")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)
	m, err := ReadManifest(ctx, t, home, stamp, stage)
	if err != nil {
		return m, err
	}
	fk, err := SetKey(m, key)
	if err != nil {
		return m, err
	}
	for _, f := range m.Files {
		if err := CheckKey(f.Path); err != nil {
			return m, err
		}
		if !HomeFile(f.Path) {
			// A set holds only what HomeFile allows: a manifest naming anything
			// else (tools/, a secret, backup.json) could plant an executable or a
			// setting where zm trusts what it finds.
			return m, fmt.Errorf("set %s names %s, which a backup never holds; refusing it", stamp, f.Path)
		}
		local := filepath.Join(stage, filepath.FromSlash(f.Path))
		if err := t.Get(ctx, fileKey(home, stamp, f.Path), local+".gz"); err != nil {
			return m, fmt.Errorf("%s: %w", f.Path, err)
		}
		got, err := gunzipFile(local+".gz", local, fk)
		os.Remove(local + ".gz")
		if err != nil {
			return m, fmt.Errorf("%s: %w", f.Path, err)
		}
		if got.Bytes != f.Bytes || got.SHA256 != f.SHA256 {
			return m, fmt.Errorf("%s does not match its manifest (%d bytes, sha256 %.12s; want %d, %.12s)", f.Path, got.Bytes, got.SHA256, f.Bytes, f.SHA256)
		}
		if strings.HasSuffix(f.Path, ".db") {
			if _, err := sqlbackup.Check(ctx, local); err != nil {
				return m, err
			}
		}
	}
	if dry {
		return m, nil
	}
	// The restoring marker first, durable: from here until every file and
	// directory is synced, a crash leaves a home a daemon refuses to start
	// on (config.FileRestoring). Any error below leaves it in place.
	if err := config.MarkRestoring(dst, stamp); err != nil {
		return m, err
	}
	// A new restore generation before anything lands: a daemon that starts
	// on this home, even a partly moved one, never answers members with
	// the generation their copies were taken under (config.FileRestored).
	gen, err := config.NewRestoreGeneration()
	if err != nil {
		return m, err
	}
	if err := config.WriteRestoreGeneration(dst, gen); err != nil {
		return m, err
	}
	if err := install(m.Files, stage, dst, installOps{os.Rename, sqlbackup.SyncDir}); err != nil {
		return m, err
	}
	return m, config.ClearRestoring(dst)
}

// installOps are the two filesystem steps install takes, so a test can fail one.
type installOps struct {
	rename  func(from, to string) error
	syncDir func(dir string) error
}

// install moves files from stage into dst, then syncs every directory that
// received an entry, deepest first, dst last.
func install(files []File, stage, dst string, ops installOps) error {
	dst = filepath.Clean(dst)
	dirs := map[string]bool{dst: true}
	for _, f := range files {
		from := filepath.Join(stage, filepath.FromSlash(f.Path))
		to := filepath.Join(dst, filepath.FromSlash(f.Path))
		for d := filepath.Dir(to); d != dst && len(d) > len(dst); d = filepath.Dir(d) {
			dirs[d] = true
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if err := ops.rename(from, to); err != nil {
			return err
		}
	}
	// Each directory's parent is a prefix of it, so longest first is deepest first.
	order := make([]string, 0, len(dirs))
	for d := range dirs {
		order = append(order, d)
	}
	sort.Slice(order, func(i, j int) bool { return len(order[i]) > len(order[j]) })
	for _, d := range order {
		if err := ops.syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// homeFileRE is every path a set may hold, relative to $ZM_HOME: exactly what
// the homes module stages (offsite_home.go stageHome), plus top.json, which
// older sets hold (it is no longer staged). Upload refuses anything
// else, and so does Restore, so a tampered manifest can name nothing more.
var homeFileRE = regexp.MustCompile(`^(registry\.db|config\.json|top\.json` +
	`|projects/[0-9A-Z]{26}/(project\.db|issues\.db|project\.toml)` +
	`|trackers/[0-9A-Z]{26}/[A-Za-z0-9][A-Za-z0-9._-]{0,63}/home\.json)$`)

// HomeFile says whether path is one a set may hold.
func HomeFile(path string) bool { return homeFileRE.MatchString(path) }

// SetKey is the key set m's files open with: nil for a plain set, key for
// an encrypted one, which needs it (ErrNeedKey) and refuses another.
func SetKey(m Manifest, key *bkcrypt.Key) (*bkcrypt.Key, error) {
	switch {
	case m.Cipher == "":
		return nil, nil
	case m.Cipher != bkcrypt.Cipher:
		return nil, api.Errorf(api.CodeConflict, "set %s is encrypted with %q, which this zm does not know; restore it with a newer zm", m.Stamp, m.Cipher)
	case key == nil:
		return nil, ErrNeedKey(m)
	case key.ID() != m.KeyID:
		return nil, api.Errorf(api.CodeBadRequest, "set %s was encrypted with backup key %s, not %s", m.Stamp, m.KeyID, key.ID()).
			WithHint("--key names the key file of the home that wrote the set")
	}
	return key, nil
}

// ErrNeedKey is an encrypted set read with no key.
func ErrNeedKey(m Manifest) error {
	return api.Errorf(api.CodeBadRequest, "set %s is encrypted with backup key %s, and no key was given", m.Stamp, m.KeyID).
		WithHint("--key file:/path/to/backup.key (the secrets/backup.key of the home that wrote it)")
}

func gunzipFile(src, dst string, key *bkcrypt.Key) (File, error) {
	in, err := os.Open(src)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	var r io.Reader = in
	if key != nil {
		if r, err = bkcrypt.NewReader(bufio.NewReader(in), key); err != nil {
			return File{}, err
		}
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return File{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return File{}, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), zr)
	if err == nil && key != nil {
		// The last chunk must open too: gzip may stop reading before it.
		_, err = io.Copy(io.Discard, r)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return File{}, err
	}
	return File{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
