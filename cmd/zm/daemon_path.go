package main

// A daemon keeps the PATH it ran with. 2026-11-06: the Mac's
// launchd updater (PATH without ~/.local/bin) swapped the daemon; the new
// one inherited the updater's PATH, lost `claude`, and every task_start
// agent=claude failed. Whoever starts a daemon (a swap, a service manager,
// an autostart from a hook) passes its own PATH, so the daemon itself
// remembers: at start it adds the directories the previous daemon on this
// home had that its own PATH lacks, then records the result.
//
// The record only adds: deleting $ZM_HOME/daemon-path forgets every
// directory it kept (the next daemon records just its own PATH). It is
// trusted only when it is the user's and nobody else can write it, and it
// adds no directory another user could plant a `claude` in.

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// FilePath holds the PATH the last daemon on this home ran with.
const FilePath = "daemon-path"

// The record's caps: a PATH that grows past them keeps its first entries.
const (
	pathMaxEntries = 64
	pathMaxBytes   = 8 << 10
)

// keepPATH sets this process's PATH to its own followed by the recorded
// directories it lacks (those that still exist and are safe), and records
// it. Called once the home's lock is held: only the home's daemon writes
// the file. Its own entries come first, so a caller still chooses which
// agent wins.
func keepPATH(home string, log *slog.Logger) {
	file := filepath.Join(home, FilePath)
	own := os.Getenv("PATH")
	recorded, err := readPATH(file)
	if err != nil {
		log.Warn("daemon PATH: ignoring the record", "err", err)
	}
	path, added := mergePATH(own, recorded)
	if len(added) > 0 {
		os.Setenv("PATH", path)
		log.Info("daemon PATH: kept the previous daemon's directories", "added", strings.Join(added, string(os.PathListSeparator)))
	}
	if path == "" {
		return
	}
	if err := writePATH(file, capPATH(path)); err != nil {
		log.Warn("cannot record the daemon's PATH", "err", err)
	}
}

// writePATH replaces file with path through a fresh temp file beside it
// (O_EXCL: nothing planted at a fixed name is followed or reused).
func writePATH(file, path string) error {
	f, err := os.CreateTemp(filepath.Dir(file), FilePath+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(path + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readPATH is the record, "" when there is none. One that is not a
// regular file of this user's, or that group or others can write, is an
// error: its directories would decide which binary runs as `claude`.
func readPATH(file string) (string, error) {
	fi, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", &fs.PathError{Op: "read", Path: file, Err: syscall.EINVAL}
	}
	if fi.Mode().Perm()&0o022 != 0 || ownerOf(fi) != os.Getuid() {
		return "", &fs.PathError{Op: "read", Path: file, Err: fs.ErrPermission}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if len(b) > 4*pathMaxBytes {
		b = b[:4*pathMaxBytes]
	}
	return strings.TrimSpace(string(b)), nil
}

func ownerOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// safeDir: an existing directory owned by this user or root that group
// and others cannot write (sticky or not: anyone could plant a binary).
func safeDir(d string) bool {
	fi, err := os.Stat(d)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	o := ownerOf(fi)
	return o == 0 || o == os.Getuid()
}

// mergePATH is own followed by recorded's safe directories own lacks.
func mergePATH(own, recorded string) (string, []string) {
	dirs := filepath.SplitList(own)
	if own == "" {
		dirs = nil
	}
	var added []string
	for _, d := range filepath.SplitList(recorded) {
		if len(dirs) >= pathMaxEntries {
			break
		}
		if d == "" || !filepath.IsAbs(d) || slices.Contains(dirs, d) || !safeDir(d) {
			continue
		}
		dirs, added = append(dirs, d), append(added, d)
	}
	return strings.Join(dirs, string(os.PathListSeparator)), added
}

// capPATH is path's first pathMaxEntries entries, within pathMaxBytes.
func capPATH(path string) string {
	dirs := filepath.SplitList(path)
	if len(dirs) > pathMaxEntries {
		dirs = dirs[:pathMaxEntries]
	}
	n := 0
	for i, d := range dirs {
		if n += len(d) + 1; n > pathMaxBytes+1 {
			dirs = dirs[:i]
			break
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
