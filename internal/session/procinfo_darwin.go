package session

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procTable is every process of the machine (one sysctl).
func procTable() (map[int]procEntry, error) {
	ps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	m := make(map[int]procEntry, len(ps))
	for _, p := range ps {
		name := p.Proc.P_comm[:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		m[int(p.Proc.P_pid)] = procEntry{pid: int(p.Proc.P_pid), ppid: int(p.Eproc.Ppid), pgid: int(p.Eproc.Pgid), name: string(name)}
	}
	return m, nil
}

// lsofTimeout bounds one lsof run (macOS has no syscall wrapper for socket
// listing in the dependencies zm already has).
var lsofTimeout = 1500 * time.Millisecond

// lsofWaitDelay bounds the wait for lsof's pipes after it was killed.
var lsofWaitDelay = time.Second

// lsofPath is the lsof run (tests point it at a stand-in).
var lsofPath = "lsof"

// listenPorts maps each pid in pids that owns a TCP socket in LISTEN state
// to its ports (lsof, one run for every pid).
func listenPorts(pids []int) (map[int][]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-nP", "-a", "-iTCP", "-sTCP:LISTEN", "-p", strings.Join(list, ","), "-Fpn")
	// A child that keeps the output pipe open must not hold the sampler:
	// after the kill, Wait gives up on the pipe within WaitDelay.
	cmd.WaitDelay = lsofWaitDelay
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("lsof: no answer within %s: %w", lsofTimeout, ctx.Err())
	}
	if err != nil && len(out) == 0 {
		return nil, nil // lsof exits 1 when nothing matched (or is missing): no ports
	}
	m := map[int][]int{}
	pid := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0:
			addr := line[1:]
			if i := strings.LastIndexByte(addr, ':'); i >= 0 {
				if port, err := strconv.Atoi(addr[i+1:]); err == nil && port > 0 {
					m[pid] = append(m[pid], port)
				}
			}
		}
	}
	return m, nil
}

// procStat reads pid's start and state (one sysctl): start is the start
// time in microseconds since the epoch, the identity of the process (a pid
// the kernel gives to a new process gets a new start), and started the
// same on the wall clock.
func procStat(pid int) (pstat, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return pstat{}, false
	}
	tv := kp.Proc.P_starttime
	t := int64(tv.Sec)*1e6 + int64(tv.Usec)
	if t <= 0 {
		return pstat{}, false
	}
	const szomb = 5 // sys/proc.h
	return pstat{start: t, started: time.UnixMicro(t), zombie: kp.Proc.P_stat == szomb, stopped: kp.Proc.P_stat == 4, ppid: int(kp.Eproc.Ppid)}, true
}
