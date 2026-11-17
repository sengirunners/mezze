package holder

import (
	"errors"
	"os"
	"path/filepath"
)

// Running is a holder process seen in the process table.
type Running struct {
	PID     int
	Home    string
	Session string
}

// Orphans are this user's holders whose home is gone: no daemon can reach
// them. They are reported, never stopped (SPEC 8).
func Orphans() ([]Running, error) {
	all, err := scan()
	if err != nil {
		return nil, err
	}
	var out []Running
	for _, r := range all {
		if _, err := os.Stat(r.Home); errors.Is(err, os.ErrNotExist) {
			out = append(out, r)
		}
	}
	return out, nil
}

// fromArgv reads a holder's argv: <exe> __zm-holder <home> <session8>.
func fromArgv(pid int, argv []string) (Running, bool) {
	if len(argv) != 4 || argv[1] != Arg || !filepath.IsAbs(argv[2]) {
		return Running{}, false
	}
	return Running{PID: pid, Home: argv[2], Session: argv[3]}, true
}

// selfComm is the process name holders started by this binary carry.
func selfComm() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	name := filepath.Base(exe)
	if len(name) > 16 { // MAXCOMLEN on macOS, TASK_COMM_LEN-1 on Linux is 15
		name = name[:16]
	}
	return name
}
