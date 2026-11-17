package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
)

// The parent leaves the task group only after forking. It deliberately does not
// reap its child until cleanup, so the test controls a real zombie's lifetime.
// Unlike the original attack fixture, cleanup asks the owning parent to reap,
// acknowledges that waitpid completed, and waits for the parent to disappear.
const zombieRedactionParent = `use POSIX qw(setsid);
my $dir = shift;
my $child = fork(); die "fork: $!" unless defined $child;
if (!$child) {
  for (1..3000) {
    exit 0 if -e "$dir/child-exit";
    select(undef, undef, undef, 0.02);
  }
  exit 0;
}
die "setsid: $!" if setsid() < 0;
for my $entry (["parent", $$], ["child", $child]) {
  my ($name, $pid) = @$entry;
  open(my $f, ">", "$dir/$name.tmp") or die $!;
  print $f $pid; close($f) or die $!;
  rename("$dir/$name.tmp", "$dir/$name") or die $!;
}
for (1..3000) {
  last if -e "$dir/cleanup";
  select(undef, undef, undef, 0.02);
}
kill 9, $child;
die "waitpid: $!" unless waitpid($child, 0) == $child;
open(my $ack, ">", "$dir/reaped") or die $!;
print $ack $child; close($ack) or die $!;
exit 0;`

func zombieRedactionWait(t *testing.T, description string, ready func() bool) {
	t.Helper()
	if !zombieRedactionUntil(ready) {
		t.Fatalf("timed out waiting for %s", description)
	}
}

func zombieRedactionUntil(ready func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func zombieRedactionPID(t *testing.T, path string) (int, int64) {
	t.Helper()
	var pid int
	zombieRedactionWait(t, filepath.Base(path)+" identity", func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 1
	})
	st, ok := procStat(pid)
	if !ok || st.start <= 0 || st.zombie {
		t.Fatalf("cannot capture live owned process %d: %+v, inspected=%v", pid, st, ok)
	}
	return pid, st.start
}

func TestTaskRedactionStoppedAndZombieDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin retains zombie process groups with a non-ESRCH group probe")
	}
	for _, mode := range []string{"stopped", "zombie"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			gate := filepath.Join(dir, "task-exit")
			script := fmt.Sprintf(`cat >/dev/null; perl -e '%s' %q >/dev/null 2>&1 & echo ready; while [ ! -e %q ]; do sleep .02; done; exit 0`, zombieRedactionParent, dir, gate)
			// Register cooperative cleanup before identity capture so a failed
			// assertion still releases the fixture. No unknown PID is signalled.
			var parent, child int
			var parentStart, childStart int64
			t.Cleanup(func() {
				gone := func(pid int, start int64) bool {
					if pid <= 1 || start <= 0 {
						return false
					}
					if st, ok := procStat(pid); ok {
						return st.start != start
					}
					return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
				}
				stopOwned := func(pid int, start int64) {
					if pid <= 1 || start <= 0 {
						return
					}
					if st, ok := procStat(pid); ok && st.start == start && !st.zombie {
						if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
							t.Errorf("stop owned fixture %d: %v", pid, err)
						}
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "cleanup"), nil, 0600); err != nil {
					t.Error(err)
				}
				if !zombieRedactionUntil(func() bool {
					_, err := os.Stat(filepath.Join(dir, "reaped"))
					return err == nil
				}) {
					t.Error("fixture parent did not acknowledge reaping its child")
					// A cleanup failure must not leave captured live fixture
					// processes until the script's own timeout. Never signal an
					// unknown or reused PID, including during failure recovery.
					stopOwned(child, childStart)
					stopOwned(parent, parentStart)
				}
				if parent > 1 && parentStart > 0 {
					if !zombieRedactionUntil(func() bool { return gone(parent, parentStart) }) {
						t.Error("owned fixture parent did not exit after cleanup")
						stopOwned(child, childStart)
						stopOwned(parent, parentStart)
						if !zombieRedactionUntil(func() bool { return gone(parent, parentStart) }) {
							t.Error("owned fixture parent remains after identity-verified cleanup")
						}
					}
				}
				if child > 1 && childStart > 0 && !zombieRedactionUntil(func() bool { return gone(child, childStart) }) {
					t.Error("owned fixture child was not reaped after cleanup")
					stopOwned(child, childStart)
					if !zombieRedactionUntil(func() bool { return gone(child, childStart) }) {
						t.Error("owned fixture child remains after identity-verified cleanup")
					}
				}
			})
			h, _, task, _ := atkUncertain(t, script, "zombie-redaction-fixture-canary")
			parent, parentStart = zombieRedactionPID(t, filepath.Join(dir, "parent"))
			child, childStart = zombieRedactionPID(t, filepath.Join(dir, "child"))
			var pgid int
			if err := h.d.Store.Registry().QueryRow(`SELECT pgid FROM session_tasks WHERE id=?`, task.ID).Scan(&pgid); err != nil {
				t.Fatal(err)
			}
			table, err := procTable()
			if err != nil || table[parent].pgid != parent || table[child].pgid != pgid {
				t.Fatalf("fixture group identities: parent=%+v child=%+v want group=%d err=%v", table[parent], table[child], pgid, err)
			}
			if mode == "stopped" {
				st, ok := procStat(child)
				if !ok || st.start != childStart || st.zombie {
					t.Fatal("owned child identity changed before STOP")
				}
				if err := syscall.Kill(child, syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				zombieRedactionWait(t, "owned child to stop", func() bool {
					st, ok := procStat(child)
					return ok && st.start == childStart && st.stopped && !st.zombie
				})
				if _, ok := holder.ReadExit(h.home, task.ID); ok {
					t.Fatal("fixture completed before stopped-member check")
				}
				status, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
				if err != nil || !status.RedactionPending || h.s.taskWriterGone(context.Background(), task.ID) {
					t.Fatalf("stopped member lost its hold without completion: %+v err=%v", status, err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(dir, "child-exit"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				zombieRedactionWait(t, "owned child to become a zombie", func() bool {
					st, ok := procStat(child)
					return ok && st.start == childStart && st.zombie
				})
			}
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			// Recovery may consume and remove the holder receipt immediately.
			// Its persisted completion bit is the same authoritative proof.
			zombieRedactionWait(t, "durable holder group completion", func() bool {
				var stopped bool
				return h.d.Store.Registry().QueryRow(`SELECT group_stopped FROM session_tasks WHERE id=?`, task.ID).Scan(&stopped) == nil && stopped
			})
			if status, cleared := atkWaitClear(t, h, task.ID, 10*time.Second); !cleared {
				t.Fatalf("completion proof did not release hold: %+v", status)
			}
			if mode != "zombie" {
				return
			}
			zombieRedactionWait(t, "zombie-only group residue", func() bool {
				table, err := procTable()
				if err != nil {
					return false
				}
				members := 0
				for pid, entry := range table {
					if entry.pgid == pgid {
						st, ok := procStat(pid)
						if !ok || !st.zombie {
							return false
						}
						members++
					}
				}
				return members > 0 && table[child].pgid == pgid
			})
			st, ok := procStat(parent)
			if !ok || st.start != parentStart || st.zombie {
				t.Fatal("zombie's parent exited before proof checks")
			}
			if childState, ok := procStat(child); !ok || childState.start != childStart || !childState.zombie {
				t.Fatal("owned zombie identity changed before group probe")
			}
			if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.EPERM) {
				t.Fatalf("expected Darwin zombie-group EPERM, got %v", err)
			}
			if !h.s.taskWriterGone(context.Background(), task.ID) {
				t.Fatal("real completion proof failed with zombie residue")
			}
			// A disconnected proc is already nil before completion. Wait for
			// the actual ended rows, then share recovery's stop lock so receipt
			// consumption and group_stopped persistence cannot race this probe.
			zombieRedactionWait(t, "task completion to settle", func() bool {
				row, ok := h.s.load(context.Background(), task.ID)
				var state string
				return ok && row.State == orchapi.StateEnded && h.d.Store.Registry().QueryRow(`SELECT state FROM session_tasks WHERE id=?`, task.ID).Scan(&state) == nil && state == "ended"
			})
			unlock := h.s.lockTaskStop(task.ID)
			defer unlock()
			db := h.d.Store.Registry()
			var stopped bool
			if err := db.QueryRow(`SELECT group_stopped FROM session_tasks WHERE id=?`, task.ID).Scan(&stopped); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE session_tasks SET group_stopped=0 WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := db.Exec(`UPDATE session_tasks SET group_stopped=? WHERE id=?`, stopped, task.ID); err != nil {
					t.Error(err)
				}
			}()
			path := holder.ExitPath(h.home, task.ID)
			if err := os.Rename(path, path+".zombie-test"); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			} else if err == nil {
				defer func() {
					if err := os.Rename(path+".zombie-test", path); err != nil {
						t.Error(err)
					}
				}()
			}
			if h.s.taskWriterGone(context.Background(), task.ID) {
				t.Fatal("EPERM zombie group was accepted without completion proof")
			}
		})
	}
}
