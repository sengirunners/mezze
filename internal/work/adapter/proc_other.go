//go:build !unix

package adapter

import "os/exec"

// OwnGroup is a no-op off unix: the context's cancel kills the child only.
func OwnGroup(cmd *exec.Cmd) {}

// KillGroup kills cmd's process.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
