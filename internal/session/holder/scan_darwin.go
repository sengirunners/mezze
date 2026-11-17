package holder

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// scan lists this user's holder processes started from a binary with this
// one's name (one sysctl, then argv of those only).
func scan() ([]Running, error) {
	ps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	comm := selfComm()
	var out []Running
	for _, p := range ps {
		name := p.Proc.P_comm[:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		pid := int(p.Proc.P_pid)
		if !strings.HasPrefix(comm, string(name)) || len(name) == 0 || pid == os.Getpid() {
			continue
		}
		if r, ok := fromArgv(pid, procArgs(pid)); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// procArgs is a process's argv (kern.procargs2: argc, the exec path, then
// the arguments, NUL separated).
func procArgs(pid int) []string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // the exec path
	if i < 0 {
		return nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	argv := make([]string, 0, argc)
	for len(argv) < argc && len(b) > 0 {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			j = len(b)
		}
		argv = append(argv, string(b[:j]))
		b = b[min(j+1, len(b)):]
	}
	return argv
}
