package session

// Foreground process and listening ports of managed sessions (SPEC 8).
// procTable and listenPorts are per OS (procinfo_<os>.go); fgPgrp asks the
// PTY master for its foreground process group.

import (
	"os"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// pstat is what the daemon checks of a hook session's agent process
// (procStat, per OS).
type pstat struct {
	start   int64     // the process's start in the OS's own unit: its identity with the pid
	started time.Time // the same on the wall clock (zero when unknown)
	zombie  bool      // exited, not yet reaped by its parent
	stopped bool      // suspended by a stop signal
	ppid    int       // its parent
}

// procEntry is one process of the machine.
type procEntry struct {
	pid, ppid, pgid int
	name            string
}

// fgPgrp is the foreground process group of the terminal behind the PTY
// master f (tcgetpgrp). It does not call f.Fd(), which would put f in
// blocking mode under the read loop.
func fgPgrp(f *os.File) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgrp int
	var ierr error
	if err := rc.Control(func(fd uintptr) { pgrp, ierr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP) }); err != nil {
		return 0, err
	}
	return pgrp, ierr
}

// descendants are root and every process under it in table.
func descendants(table map[int]procEntry, children map[int][]int, root int) []int {
	out := []int{root}
	for i := 0; i < len(out); i++ {
		out = append(out, children[out[i]]...)
	}
	sort.Ints(out)
	return out
}

// childrenOf indexes table by parent.
func childrenOf(table map[int]procEntry) map[int][]int {
	m := map[int][]int{}
	for pid, e := range table {
		if e.ppid != pid {
			m[e.ppid] = append(m[e.ppid], pid)
		}
	}
	return m
}
