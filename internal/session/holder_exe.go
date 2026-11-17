package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sengirunners/potluck/kit/api"
)

// holderBinDir keeps the daemon's private copy of its own binary, which new
// holders run. On macOS a binary overwritten in place (cp over
// ~/go/bin/zm) keeps running processes alive but kills every new exec of
// that path (code signing, exit 137): a daemon spawning holders from the
// installed path would lose every new session until a proper install.
// The copy is an APFS clone made once at start, so it costs no space and
// no later write to the installed file reaches it.
const holderBinDir = "holder-bin"

// pinHolderExe clones exe into home and returns the clone's path, or ""
// when it cannot (another volume, not macOS): holders then run exe. Older
// clones are removed: a holder running one keeps its unlinked file.
func pinHolderExe(home, exe string) (string, error) {
	st, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", nil
	}
	key := fmt.Sprintf("%s-%x-%x-%x", safeName(api.Version), sys.Ino, st.Size(), st.ModTime().UnixNano())
	root := filepath.Join(home, holderBinDir)
	dir := filepath.Join(root, key)
	out := filepath.Join(dir, filepath.Base(exe)) // same name: holders are found by it (holder.selfComm)
	if _, err := os.Stat(out); err != nil {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp(root, ".new-")
		if err != nil {
			return "", err
		}
		if err := cloneFile(exe, filepath.Join(tmp, filepath.Base(exe))); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
		if err := os.Rename(tmp, dir); err != nil {
			_ = os.RemoveAll(tmp)
			if _, serr := os.Stat(out); serr != nil {
				return "", err
			}
		}
	}
	if ents, err := os.ReadDir(root); err == nil {
		for _, e := range ents {
			if e.Name() != key {
				_ = os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
	}
	return out, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == os.PathSeparator || r == ' ' {
			return '_'
		}
		return r
	}, s)
}

// pinHolder sets pinnedExe for a daemon that keeps sessions. A failure
// only costs the protection: holders run the installed binary as before.
func (s *Service) pinHolder() {
	if s.HolderExe != "" || !s.keep.Load() {
		return
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil {
		s.pinnedExe, err = pinHolderExe(s.d.Config.Home, exe)
	}
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		s.d.Log.Warn("session holders run the installed zm (no private copy)", "err", err)
	}
}
