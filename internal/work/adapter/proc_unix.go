//go:build unix

package adapter

import (
	"os/exec"
	"syscall"
)

// OwnGroup makes cmd the leader of a new process group and makes the
// context's cancel (timeout) kill that whole group, so a tracker CLI the
// child started (bd, gh, a shell script's subprocess) dies with it instead
// of being orphaned.
func OwnGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return KillGroup(cmd) }
}

// KillGroup SIGKILLs cmd's process group (cmd must have been started with OwnGroup).
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
