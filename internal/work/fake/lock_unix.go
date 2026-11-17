//go:build unix

package fake

import (
	"os"
	"syscall"
)

// lock serialises fake calls on one state file (zm runs several at
// once: a sync's listing, a claim meanwhile).
func lock(state string) func() {
	f, err := os.OpenFile(state+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}
