package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type taskHomeIdentity struct{ id, path, dev, ino string }

func taskHomeIdentityAt(home string, create bool) (taskHomeIdentity, error) {
	path, err := filepath.Abs(home)
	if err != nil {
		return taskHomeIdentity{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return taskHomeIdentity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return taskHomeIdentity{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return taskHomeIdentity{}, fmt.Errorf("cannot identify home directory")
	}
	file := filepath.Join(path, "session-home-id")
	if create {
		f, e := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			var b [32]byte
			if _, err = rand.Read(b[:]); err == nil {
				_, err = f.WriteString(hex.EncodeToString(b[:]))
			}
			if e = f.Close(); err == nil {
				err = e
			}
			if err != nil {
				_ = os.Remove(file)
				return taskHomeIdentity{}, err
			}
		} else if !os.IsExist(e) {
			return taskHomeIdentity{}, e
		}
	}
	fi, err := os.Lstat(file)
	if err != nil {
		return taskHomeIdentity{}, err
	}
	if !fi.Mode().IsRegular() {
		return taskHomeIdentity{}, fmt.Errorf("home identity is not a regular file")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return taskHomeIdentity{}, err
	}
	id := strings.TrimSpace(string(b))
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 32 {
		return taskHomeIdentity{}, fmt.Errorf("invalid home identity")
	}
	return taskHomeIdentity{id: id, path: path, dev: strconv.FormatUint(uint64(st.Dev), 10), ino: strconv.FormatUint(uint64(st.Ino), 10)}, nil
}

func (s *Service) currentTaskHome() (taskHomeIdentity, error) {
	h, err := taskHomeIdentityAt(s.d.Config.Home, false)
	if err != nil {
		return h, err
	}
	if h != s.homeIdentity {
		return h, fmt.Errorf("home identity changed")
	}
	return h, nil
}
