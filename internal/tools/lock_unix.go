//go:build unix

package tools

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lock serialises provisioning in one home (two CLIs adding trackers at
// once). It never blocks silently: when another zm holds the lock it
// calls busy once, then keeps trying until wait has passed (ErrBusy) or
// ctx is done.
func lock(ctx context.Context, path string, wait time.Duration, busy func()) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	told := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if !told && busy != nil {
			busy()
			told = true
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
