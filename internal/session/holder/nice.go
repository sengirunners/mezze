package holder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// PriorityArg is a private re-exec mode, before any payload or task probe runs.
const PriorityArg = "__zm-priority"

type priorityError struct {
	Message string
	Errno   syscall.Errno
}

// startPriority preserves the launcher's PID, process attributes and existing
// ExtraFiles (notably the recorded task grant at fd3). The added pipe closes on
// payload exec, so exec failures still fail the holder's Ready handshake.
func startPriority(cmd *exec.Cmd, nice int, start func() error) error {
	if nice <= 0 {
		return start()
	}
	if nice > 19 {
		return syscall.EINVAL
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	fd := 3 + len(cmd.ExtraFiles)
	path, args := cmd.Path, cmd.Args
	cmd.Path = exe
	cmd.Args = append([]string{exe, PriorityArg, strconv.Itoa(nice), strconv.Itoa(fd), path}, args...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	if err := start(); err != nil {
		return err
	}
	_ = w.Close()
	data, err := io.ReadAll(io.LimitReader(r, 4096))
	if err == nil && len(data) == 0 {
		return nil
	}
	// A failed trampoline has no payload or descendants to preserve.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err != nil {
		return err
	}
	var failure priorityError
	if err := json.Unmarshal(data, &failure); err != nil {
		return fmt.Errorf("priority launcher: %w", err)
	}
	if failure.Errno != 0 {
		return fmt.Errorf("%s: %w", failure.Message, failure.Errno)
	}
	return errors.New(failure.Message)
}

func priorityMain(args []string) int {
	return priorityMainWith(args, unix.Getpriority, unix.Setpriority)
}

// Syscall arguments keep denied-priority behavior testable in a real exec child.
func priorityMainWith(args []string, get func(int, int) (int, error), set func(int, int, int) error) int {
	if len(args) < 4 {
		return 1
	}
	nice, e1 := strconv.Atoi(args[0])
	fd, e2 := strconv.Atoi(args[1])
	if e1 != nil || e2 != nil || nice < 1 || nice > 19 || fd < 3 {
		return 1
	}
	report := os.NewFile(uintptr(fd), "priority-exec-error")
	defer report.Close()
	syscall.CloseOnExec(fd)
	fail := func(err error) int {
		failure := priorityError{Message: err.Error()}
		_ = errors.As(err, &failure.Errno)
		_ = json.NewEncoder(report).Encode(failure)
		return 1
	}
	// Linux niceness is per-thread. Exec must occur on the very thread whose
	// priority we changed; all new payload threads and forks then inherit it.
	runtime.LockOSThread()
	// Scheduling is best effort, never launch authorization. If inherited
	// priority cannot be read, leave it alone rather than risk raising it.
	if inherited, err := get(unix.PRIO_PROCESS, 0); err == nil {
		if runtime.GOOS == "linux" {
			inherited = 20 - inherited
		}
		if nice > inherited {
			_ = set(unix.PRIO_PROCESS, 0, nice)
		}
	}
	return fail(syscall.Exec(args[2], args[3:], os.Environ()))
}
