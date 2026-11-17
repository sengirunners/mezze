// Package setup is the daemon's generic side of SPEC 11's setup verb, apart
// from the work service: which setup options an adapter gets (exactly what
// its describe declares, with what a --config key may not override), and
// the undo of what a setup call made, contained in the tracker's own directory. It knows no
// tracker and links no adapter; internal/work calls it.
package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// flagNames are the tracker add flags behind the setup options, for errors.
var flagNames = map[string]string{
	orchapi.SetupOptMode: "--new/--dir/--server (mode)", orchapi.SetupOptDir: "--dir", orchapi.SetupOptServer: "--server",
	orchapi.SetupOptDatabase: "--database", orchapi.SetupOptUser: "--user", orchapi.SetupOptAllowMigrate: "--allow-migrate",
}

// Flags are tracker add's flags as setup options, as given.
func Flags(req orchapi.AddTrackerRequest) map[string]string {
	o := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			o[k] = v
		}
	}
	set(orchapi.SetupOptMode, req.Mode)
	set(orchapi.SetupOptDir, req.Dir)
	set(orchapi.SetupOptServer, req.Server)
	set(orchapi.SetupOptDatabase, req.Database)
	set(orchapi.SetupOptUser, req.User)
	if req.AllowMigrate {
		o[orchapi.SetupOptAllowMigrate] = "true"
	}
	return o
}

// Options is the options map a tracker add request gives an adapter whose
// describe.setup is info (nil: no setup verb): the flags given, each one the
// adapter must declare (an undeclared flag is refused), and no --config key
// that a declared option owns (the setup plan sets it; a config that
// disagrees would point setup outside the tracker's directory).
// --password-file needs info.Password.
func Options(info *orchapi.AdapterSetupInfo, adapter string, req orchapi.AddTrackerRequest) (map[string]string, error) {
	var decl []string
	password := false
	if info != nil {
		decl, password = info.Options, info.Password
	}
	flags := Flags(req)
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(decl, k) {
			name := flagNames[k]
			if name == "" {
				name = k
			}
			return nil, api.Errorf(api.CodeBadRequest, "the %s adapter takes no %s", adapter, name)
		}
	}
	if req.PasswordFile != "" && !password {
		return nil, api.Errorf(api.CodeBadRequest, "the %s adapter takes no --password-file", adapter)
	}
	cfgKeys := make([]string, 0, len(req.Config))
	for k := range req.Config {
		cfgKeys = append(cfgKeys, k)
	}
	sort.Strings(cfgKeys)
	for _, k := range cfgKeys {
		if slices.Contains(decl, k) {
			flag := flagNames[k]
			if flag == "" {
				flag = "its flag"
			}
			return nil, api.Errorf(api.CodeBadRequest, "--config %s is the %s adapter's setup option: the setup plan sets it", k, adapter).
				WithHint("use " + flag + " instead")
		}
	}
	if len(flags) == 0 {
		return nil, nil
	}
	return flags, nil
}

// With is args for one call: dry run or not.
func With(args orchapi.AdapterSetupArgs, dry bool) orchapi.AdapterSetupArgs {
	args.DryRun = dry
	return args
}

// MergeConfig sets the keys of add that tr does not set.
func MergeConfig(tr *api.Tracker, add map[string]string) {
	for k, v := range add {
		if tr.Config == nil {
			tr.Config = map[string]string{}
		}
		if _, set := tr.Config[k]; !set {
			tr.Config[k] = v
		}
	}
}

// SnapshotDepth is how deep Snapshot records a tracker directory: an undo
// path deeper than this whose ancestors all existed is refused (zm cannot
// tell that it is new).
const SnapshotDepth = 3

// Snapshot is the set of paths under root (symlinks resolved for root,
// relative to it, at most SnapshotDepth deep) before a setup call.
func Snapshot(root string) map[string]bool {
	seen := map[string]bool{}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return seen // no root yet: everything under it is new
	}
	_ = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == r {
			return nil
		}
		rel, rerr := filepath.Rel(r, p)
		if rerr != nil {
			return nil
		}
		seen[rel] = true
		if d.IsDir() && strings.Count(rel, string(filepath.Separator))+1 >= SnapshotDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return seen
}

// undoTarget checks one undo step against root and the snapshot taken
// before the call: the op is remove_dir, the path (symlinks resolved) is a
// directory strictly inside root, and it did not exist before the call.
// "" with a nil error: nothing there any more.
func undoTarget(root string, before map[string]bool, u orchapi.AdapterUndo) (string, error) {
	if u.Op != orchapi.UndoRemoveDir {
		return "", fmt.Errorf("unknown undo op %q", u.Op)
	}
	if !filepath.IsAbs(u.Path) {
		return "", fmt.Errorf("undo path %q is not absolute", u.Path)
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("tracker directory %s: %v", root, err)
	}
	p, err := filepath.EvalSymlinks(u.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("undo path %s (%s) is not inside the tracker's directory %s", u.Path, p, r)
	}
	if st, err := os.Lstat(p); err != nil || !st.IsDir() {
		return "", fmt.Errorf("undo path %s is not a directory", p)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i := 1; i <= len(parts) && i <= SnapshotDepth; i++ { // the snapshot is exact this deep
		if !before[filepath.Join(parts[:i]...)] {
			return p, nil // this call made it (or an ancestor)
		}
	}
	if len(parts) <= SnapshotDepth {
		return "", fmt.Errorf("undo path %s existed before setup", p)
	}
	return "", fmt.Errorf("undo path %s: zm cannot tell whether it existed before setup", p)
}

// Undo takes back what a setup call made: each step is checked
// (undoTarget) and refused, and logged, unless it is contained and new.
// It never touches a server database: the only op removes a directory.
// The containment guards against a buggy adapter, not a hostile one: an
// adapter runs as the user and owns the tracker's directory, so it could
// delete inside it anyway; what zm refuses is removing anything else on
// an adapter's say-so.
func Undo(root string, before map[string]bool, steps []orchapi.AdapterUndo, warn func(msg string, args ...any)) {
	for i := len(steps) - 1; i >= 0; i-- {
		p, err := undoTarget(root, before, steps[i])
		if err != nil {
			warn("tracker: setup undo refused", "dir", root, "err", err)
			continue
		}
		if p != "" {
			if err := os.RemoveAll(p); err != nil {
				warn("tracker: setup undo", "path", p, "err", err)
			}
		}
	}
}
