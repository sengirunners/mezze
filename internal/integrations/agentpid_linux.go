package integrations

import (
	"os"
	"strconv"
	"strings"
)

// parentOf is pid's parent and command name (/proc/<pid>/stat).
func parentOf(pid int) (int, string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	s := string(b)
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return 0, "", false
	}
	f := strings.Fields(s[end+1:]) // state ppid ...
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, s[open+1 : end], err == nil
}

// procArgs is pid's executable path and argv (/proc/<pid>/exe and
// cmdline). Empty when unreadable (another user's process).
func procArgs(pid int) (string, []string) {
	dir := "/proc/" + strconv.Itoa(pid)
	exe, _ := os.Readlink(dir + "/exe")
	b, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return exe, nil
	}
	return exe, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}
