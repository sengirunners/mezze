package holder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/session/tasklaunch"
	"golang.org/x/sys/unix"
)

type niceSnapshot struct{ Nice, PID, Group, Descendant, Grant int }

func currentNice() int {
	v, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		panic(err)
	}
	if runtime.GOOS == "linux" {
		return 20 - v
	}
	return v
}

// Report priority before the payload starts any descendant. The descendant
// then reports its own initial priority; neither waits for parent-side renice.
func init() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__nice-denied-read":
			os.Exit(priorityMainWith(os.Args[2:], func(int, int) (int, error) { fmt.Fprintln(os.Stderr, "denied-read"); return 0, syscall.EPERM }, func(int, int, int) error { panic("must not adjust unknown inherited priority") }))
		case "__nice-denied-set":
			os.Exit(priorityMainWith(os.Args[2:], func(int, int) (int, error) {
				// Ensure the denied-set seam is reached even on an already-niced host.
				if runtime.GOOS == "linux" {
					return 20, nil
				}
				return 0, nil
			}, func(int, int, int) error { fmt.Fprintln(os.Stderr, "denied-set"); return syscall.EPERM }))
		}
	}

	if len(os.Args) > 1 && os.Args[1] == "__nice-version" {
		file, err := os.Create(os.Getenv("NICE_PROBE_MARKER"))
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(file).Encode(niceSnapshot{Nice: currentNice(), Group: syscall.Getpgrp()})
		_ = file.Close()
		fmt.Fprintln(os.Stdout, "2.1.318 (Claude Code)")
		os.Exit(0)
	}
	if len(os.Args) < 2 || os.Args[1] != "__nice-probe" {
		return
	}
	snapshot := niceSnapshot{Nice: currentNice(), PID: os.Getpid(), Group: syscall.Getpgrp(), Grant: -1}
	if len(os.Args) > 2 && os.Args[2] == "descendant" {
		_ = json.NewEncoder(os.Stdout).Encode(snapshot)
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[2] == "grant" {
		grant := os.NewFile(3, "test-grant")
		var b [1]byte
		if _, err := grant.Read(b[:]); err == nil {
			snapshot.Grant = int(b[0])
		}
	}
	child := exec.Command(os.Args[0], "__nice-probe", "descendant")
	out, err := child.Output()
	if err != nil {
		os.Exit(2)
	}
	var descendant niceSnapshot
	if json.Unmarshal(out, &descendant) != nil {
		os.Exit(3)
	}
	snapshot.Descendant = descendant.Nice
	// Exit only once the test has adopted the session: a child that ends
	// first lets the holder close its socket before the test dials.
	if gate := os.Getenv("NICE_PROBE_GATE"); gate != "" {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(gate); err == nil {
				break
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(snapshot)
	os.Exit(0)
}

func TestPriorityBeforePayload(t *testing.T) {
	own := currentNice()
	for _, configured := range []int{-1, 0, 10, 15} {
		t.Run(strconv.Itoa(configured), func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "__nice-probe", "grant")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			grant, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer grant.Close()
			_, _ = write.Write([]byte{73})
			_ = write.Close()
			cmd.ExtraFiles = []*os.File{grant}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if err := output.(*os.File).SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var first niceSnapshot
			// Hold Start's caller until payload and descendant have both run. This
			// deterministically exposes the old post-Start renice window without a
			// scheduler race, retries, or a longer readiness wait.
			err = startPriority(cmd, configured, func() error {
				if err := cmd.Start(); err != nil {
					return err
				}
				return json.NewDecoder(output).Decode(&first)
			})
			if err != nil {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			want := own
			if configured > 0 {
				want = max(want, configured)
			}
			if first.Nice != want || first.Descendant != want {
				t.Fatalf("first payload=%d descendant=%d, want %d", first.Nice, first.Descendant, want)
			}
			if first.PID != cmd.Process.Pid || first.Group != first.PID || first.Grant != 73 {
				t.Fatalf("PID/group/fd3 changed: %+v, launched PID=%d", first, cmd.Process.Pid)
			}
			if currentNice() != own {
				t.Fatal("launcher changed holder priority")
			}
		})
	}
}

func TestPriorityExecError(t *testing.T) {
	for _, path := range []string{"/nonexistent-zm-priority-payload", t.TempDir()} {
		cmd := exec.Command(path)
		err := startPriority(cmd, 10, cmd.Start)
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.EACCES) {
			t.Fatalf("exec %s: %v", path, err)
		}
		if cmd.ProcessState == nil {
			t.Fatal("failed exec child not reaped")
		}
	}
}

func TestPriorityAlreadyNiced(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/nice", "-n", "15", exe, "-test.run=^TestPriorityBeforePayload$", "-test.timeout=10s", "-test.v")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("already-niced launcher: %v\n%s", err, out)
	}
}

// Exercise the real holder call sites, including PTY/session attributes and
// the task anchor group, rather than only the pre-exec helper.
func TestHolderPriorityModes(t *testing.T) {
	for _, mode := range []string{"pty", "pipe", "task", "recorded-task"} {
		t.Run(mode, func(t *testing.T) {
			home := tempHome(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(home, "probe-priority")
			gate := filepath.Join(home, "probe-gate")
			args := []string{exe, "__nice-probe"}
			if mode == "recorded-task" {
				args = append([]string{exe}, tasklaunch.Arguments([]string{exe, "__nice-version"}, args)...)
			}
			st, err := Start(exe, Spec{Session: "abcd1234", Home: home, Path: exe, Args: args, Dir: home, Env: []string{"PATH=/usr/bin:/bin", "NICE_PROBE_MARKER=" + marker, "NICE_PROBE_GATE=" + gate}, Nice: 10, Rows: 24, Cols: 80, Pipe: mode == "pipe", Task: mode == "task" || mode == "recorded-task", RecordedLaunch: mode == "recorded-task"})
			if err != nil {
				t.Fatal(err)
			}
			owned := []cleanupPID{recordCleanupPID(t, st.Cmd.Process.Pid)}
			if st.GroupPID > 1 {
				owned = append(owned, recordCleanupPID(t, st.GroupPID))
			}
			done := make(chan struct{})
			go func() { _ = st.Cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				for _, p := range owned {
					signalHolderCleanupPID(p, cleanupProcessInfo, syscall.Kill)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("holder not reaped")
				}
			})
			if mode == "recorded-task" {
				defer st.LaunchGrant.Close()
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("probe ran before recorded launch grant")
				}
				if _, err := st.LaunchGrant.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
				_ = st.LaunchGrant.Close()
			}
			holderNice, err := unix.Getpriority(unix.PRIO_PROCESS, st.Cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "linux" {
				holderNice = 20 - holderNice
			}
			if holderNice != currentNice() {
				t.Fatalf("holder nice=%d, want %d", holderNice, currentNice())
			}
			c, err := Dial(home, "abcd1234")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			fd, err := c.Adopt(0)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			output, exited := until(t, c, "")
			if exited == nil || exited.Code != 0 {
				t.Fatalf("exit: %+v output=%q", exited, output)
			}
			_ = c.Ack()
			var first niceSnapshot
			if err := json.Unmarshal([]byte(output), &first); err != nil {
				t.Fatalf("snapshot %q: %v", output, err)
			}
			group := st.ChildPID
			if mode == "task" || mode == "recorded-task" {
				group = st.GroupPID
			}
			if mode == "recorded-task" {
				data, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				var probe niceSnapshot
				if err := json.Unmarshal(data, &probe); err != nil {
					t.Fatal(err)
				}
				if probe.Nice != max(currentNice(), 10) || probe.Group != group {
					t.Fatalf("probe priority/group changed: %+v", probe)
				}
			}
			if first.Nice != max(currentNice(), 10) || first.Descendant != first.Nice || first.PID != st.ChildPID || first.Group != group {
				t.Fatalf("mode %s snapshot %+v, child %d group %d", mode, first, st.ChildPID, group)
			}
		})
	}
}

// Run the real exec/error-pipe path with priority syscalls denied in the test
// child. Scheduling preferences must not become payload launch authorization.
func TestPriorityDeniedStillExecutes(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"read", "set"} {
		for _, payload := range []string{"permitted", "missing", "directory"} {
			t.Run(denied+"/"+payload, func(t *testing.T) {
				path, args := exe, []string{"__nice-probe"}
				var wantErr syscall.Errno
				switch payload {
				case "missing":
					path, args, wantErr = "/nonexistent-zm-denied-priority-payload", nil, syscall.ENOENT
				case "directory":
					path, args, wantErr = t.TempDir(), nil, syscall.EACCES
				}
				cmd := exec.Command(path, args...)
				var output, deniedCall bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &deniedCall
				defer func() {
					if deniedCall.String() != "denied-"+denied+"\n" {
						t.Errorf("denial was not exercised exactly once: %q", deniedCall.String())
					}
				}()
				err := startPriority(cmd, 10, func() error {
					if cmd.Args[1] != PriorityArg {
						t.Fatal("missing priority launcher")
					}
					cmd.Args[1] = "__nice-denied-" + denied
					return cmd.Start()
				})
				if wantErr != 0 {
					if !errors.Is(err, wantErr) {
						t.Fatalf("payload failure=%v, want %v", err, wantErr)
					}
					if cmd.ProcessState == nil {
						t.Fatal("failed payload exec not reaped")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				var first niceSnapshot
				if err := json.Unmarshal(output.Bytes(), &first); err != nil {
					t.Fatalf("payload did not record execution: %q: %v", output.String(), err)
				}
				if first.PID != cmd.Process.Pid || first.Nice != currentNice() || first.Descendant != first.Nice {
					t.Fatalf("denied %s: payload/descendant did not inherit priority/PID: %+v", denied, first)
				}
			})
		}
	}
}
