package holder

import "golang.org/x/sys/unix"

func cleanupProcessInfo(pid int) (cleanupProcess, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return cleanupProcess{}, false
	}
	start := int64(p.Proc.P_starttime.Sec)*1e6 + int64(p.Proc.P_starttime.Usec)
	return cleanupProcess{start: start, zombie: p.Proc.P_stat == 5}, start > 0
}
