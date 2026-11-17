package session

import (
	"os"
	"syscall"
	"testing"
)

// A saved PID alone never authorizes cleanup. Unknown, reused and exited
// processes are left alone; the caller retains only identities it recorded.
func signalSessionCleanupPID(pid int, start int64, probe func(int) (pstat, bool), signal func(int, syscall.Signal) error) bool {
	if pid <= 1 || start <= 0 {
		return false
	}
	current, ok := probe(pid)
	if !ok || current.zombie || current.start != start {
		return false
	}
	return signal(pid, syscall.SIGKILL) == nil
}

func TestSessionCleanupRequiresMatchingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pid   int
		start int64
		now   pstat
		known bool
		want  bool
	}{
		{"matched", 42, 100, pstat{start: 100}, true, true},
		{"reused", 42, 100, pstat{start: 101}, true, false},
		{"unknown capture", 42, 0, pstat{start: 100}, true, false},
		{"unknown current", 42, 100, pstat{}, false, false},
		{"gone", 42, 100, pstat{start: 100, zombie: true}, true, false},
		{"invalid pid", 1, 100, pstat{start: 100}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := signalSessionCleanupPID(tc.pid, tc.start, func(int) (pstat, bool) { return tc.now, tc.known }, func(pid int, sig syscall.Signal) error {
				calls++
				if pid != tc.pid || sig != syscall.SIGKILL {
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

func TestSessionCleanupReadsOwnKernelIdentity(t *testing.T) {
	first, ok := procStat(os.Getpid())
	second, again := procStat(os.Getpid())
	if !ok || !again || first.start <= 0 || first.start != second.start || first.zombie || second.zombie {
		t.Fatalf("unstable self identity: %+v %v, %+v %v", first, ok, second, again)
	}
}
