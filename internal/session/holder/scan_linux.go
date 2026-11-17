package holder

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// scan lists this user's holder processes (from /proc).
func scan() ([]Running, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Running
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		if r, ok := fromArgv(pid, strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")); ok {
			out = append(out, r)
		}
	}
	return out, nil
}
