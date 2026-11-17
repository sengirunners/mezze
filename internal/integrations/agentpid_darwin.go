package integrations

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// parentOf is pid's parent and command name (one sysctl).
func parentOf(pid int) (int, string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return 0, "", false
	}
	name := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return int(kp.Eproc.Ppid), string(name), true
}

// procArgs is pid's executable path and argv (kern.procargs2: argc, the
// exec path, padding, then argv). Empty when unreadable (another user's
// process).
func procArgs(pid int) (string, []string) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return "", nil
	}
	argc := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", nil
	}
	exe := string(b[:i])
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	var argv []string
	for len(argv) < argc && len(b) > 0 {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			j = len(b)
		}
		argv = append(argv, string(b[:j]))
		b = b[min(j+1, len(b)):]
	}
	return exe, argv
}
