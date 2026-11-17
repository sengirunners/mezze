package session

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procTable is every process of the machine (/proc/<pid>/stat).
func procTable() (map[int]procEntry, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	m := make(map[int]procEntry, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may hold spaces and parens.
		s := string(b)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < open {
			continue
		}
		f := strings.Fields(s[close+1:])
		if len(f) < 3 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		pgid, _ := strconv.Atoi(f[2])
		m[pid] = procEntry{pid: pid, ppid: ppid, pgid: pgid, name: s[open+1 : close]}
	}
	return m, nil
}

// listenPorts maps each pid in pids that owns a TCP socket in LISTEN state
// to its ports (/proc/net/tcp{,6} and each pid's fds).
func listenPorts(pids []int) (map[int][]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	listening := map[string]int{} // socket inode -> port
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // header
		for sc.Scan() {
			x := strings.Fields(sc.Text())
			if len(x) < 10 || x[3] != "0A" { // 0A = LISTEN
				continue
			}
			i := strings.LastIndexByte(x[1], ':')
			port, err := strconv.ParseInt(x[1][i+1:], 16, 32)
			if err == nil && port > 0 {
				listening[x[9]] = int(port)
			}
		}
		fh.Close()
	}
	if len(listening) == 0 {
		return nil, nil
	}
	m := map[int][]int{}
	for _, pid := range pids {
		dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			l, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasPrefix(l, "socket:[") {
				continue
			}
			if port, ok := listening[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")]; ok {
				m[pid] = append(m[pid], port)
			}
		}
	}
	return m, nil
}

// procStat reads pid's start and state (/proc/<pid>/stat): start is the
// start time in clock ticks since boot (field 22), the identity of the
// process (a pid the kernel gives to a new process gets a new start);
// started is that time on the wall clock, for ordering (btime from
// /proc/stat, 100 ticks a second: Linux's USER_HZ on every architecture).
func procStat(pid int) (pstat, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return pstat{}, false
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:]) // state ppid ... starttime is f[19]
	if len(f) < 20 {
		return pstat{}, false
	}
	t, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil || t <= 0 {
		return pstat{}, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return pstat{}, false
	}
	p := pstat{ppid: ppid, start: t, zombie: f[0] == "Z" || f[0] == "X", stopped: f[0] == "T" || f[0] == "t"}
	if bt := bootTime(); !bt.IsZero() {
		p.started = bt.Add(time.Duration(t) * 10 * time.Millisecond)
	}
	return p, true
}

// bootTime is btime from /proc/stat (zero when unreadable).
func bootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}
