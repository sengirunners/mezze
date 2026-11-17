//go:build unix

package gate

import (
	"os/exec"
	"syscall"
)

// ownGroup runs the gate in its own process group and kills the whole group
// on timeout (a gate's children, gh or git, go with it).
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
