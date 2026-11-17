package holder

import "syscall"

func taskProcessAttr(group int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pgid: group, Pdeathsig: syscall.SIGKILL}
}
