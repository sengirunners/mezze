package holder

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func IsHolder(pid int) (bool, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false, err
	}
	if len(b) == 0 {
		return false, os.ErrNotExist
	}
	_, ok := fromArgv(pid, strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"))
	return ok, nil
}
