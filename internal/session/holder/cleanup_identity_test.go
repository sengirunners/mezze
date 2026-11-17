package holder

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

type cleanupProcess struct {
	start  int64
	zombie bool
}

type cleanupPID struct {
	pid   int
	start int64
}

func recordCleanupPID(t *testing.T, pid int) cleanupPID {
	t.Helper()
	p := cleanupPID{pid: pid}
	if st, ok := cleanupProcessInfo(pid); ok && st.start > 0 {
		p.start = st.start
	} else if pid > 1 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Errorf("cannot record cleanup identity for pid %d; unsafe signals will be skipped", pid)
	}
	return p
}

func signalHolderCleanupPID(p cleanupPID, probe func(int) (cleanupProcess, bool), signal func(int, syscall.Signal) error) bool {
	if p.pid <= 1 || p.start <= 0 {
		return false
	}
	current, ok := probe(p.pid)
	if !ok || current.zombie || current.start != p.start {
		return false
	}
	return signal(p.pid, syscall.SIGKILL) == nil
}

func TestHolderCleanupRequiresMatchingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     cleanupPID
		now   cleanupProcess
		known bool
		want  bool
	}{
		{"matched", cleanupPID{42, 100}, cleanupProcess{start: 100}, true, true},
		{"reused", cleanupPID{42, 100}, cleanupProcess{start: 101}, true, false},
		{"unknown capture", cleanupPID{42, 0}, cleanupProcess{start: 100}, true, false},
		{"unknown current", cleanupPID{42, 100}, cleanupProcess{}, false, false},
		{"gone", cleanupPID{42, 100}, cleanupProcess{start: 100, zombie: true}, true, false},
		{"invalid pid", cleanupPID{1, 100}, cleanupProcess{start: 100}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := signalHolderCleanupPID(tc.p, func(int) (cleanupProcess, bool) { return tc.now, tc.known }, func(pid int, sig syscall.Signal) error {
				calls++
				if pid != tc.p.pid || sig != syscall.SIGKILL {
					t.Fatalf("signal %d %v", pid, sig)
				}
				return nil
			})
			if got != tc.want || (calls == 1) != tc.want {
				t.Fatalf("signalled=%v calls=%d want %v", got, calls, tc.want)
			}
		})
	}
}

func TestHolderCleanupReadsOwnKernelIdentity(t *testing.T) {
	first, ok := cleanupProcessInfo(os.Getpid())
	second, again := cleanupProcessInfo(os.Getpid())
	if !ok || !again || first.start <= 0 || first.start != second.start || first.zombie || second.zombie {
		t.Fatalf("unstable self identity: %+v %v, %+v %v", first, ok, second, again)
	}
}
