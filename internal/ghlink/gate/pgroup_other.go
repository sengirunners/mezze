//go:build !unix

package gate

import "os/exec"

func ownGroup(*exec.Cmd) {}
