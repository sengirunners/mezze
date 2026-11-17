package tasklaunch

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLauncherHelper(t *testing.T) {
	if os.Getenv("ZM_LAUNCH_FIXTURE") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			fmt.Fprintln(os.Stderr, "launcher-ready")
			os.Exit(Main(os.Args[i+1:]))
		}
	}
	os.Exit(2)
}
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}
func TestGrantRequiredBeforeAnyAgentCode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probe")
	bin := fakeClaude(t, "printf touched > \"$FIXTURE_MARKER\"\nprintf '2.1.318 (Claude Code)\\n'\n")
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherHelper$", "--", "2", bin, "--version", bin)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZM_LAUNCH_FIXTURE=1", "FIXTURE_MARKER=" + marker}
	cmd.ExtraFiles = []*os.File{read}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stderr)
	if !scan.Scan() || scan.Text() != "launcher-ready" {
		t.Fatal("helper did not reach launch")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("agent ran before release")
	}
	_ = write.Close()
	if err = cmd.Wait(); err == nil {
		t.Fatal("EOF grant succeeded")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("agent ran after refused grant")
	}
}
func TestGrantedColdLaunchPreservesPromptPIDAndArgv(t *testing.T) {
	bin := fakeClaude(t, `if [ "$1" = --version ]; then
 read ignored && exit 9
 printf '2.1.318 (Claude Code)\n'
 exit 0
fi
printf '%s\n' "$$" "$1" "$2"
cat
`)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherHelper$", "--", "2", bin, "--version", bin, "--plugin-dir", "fixture plugin")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZM_LAUNCH_FIXTURE=1"}
	cmd.ExtraFiles = []*os.File{read}
	prompt := "invented prompt\nsecond line\x00tail"
	cmd.Stdin = strings.NewReader(prompt)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err = write.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatalf("launch: %v %s", err, stderr.String())
	}
	expected := fmt.Sprintf("%d\n--plugin-dir\nfixture plugin\n%s", cmd.Process.Pid, prompt)
	if out.String() != expected {
		t.Fatalf("PID/argv/prompt changed: %q", out.String())
	}
}
func TestUnsupportedOverflowAndTimeoutRefuse(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"old", "printf '2.1.317 (Claude Code)\\n'"},
		{"unknown", "printf 'unrecognized wrapper\\n'"},
		{"overflow", "printf '2.1.318 '; head -c 5000 /dev/zero"},
		{"timeout", "exec sleep 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := probe([]string{fakeClaude(t, tc.body), "--version"}, []string{"PATH=/usr/bin:/bin"}, 100*time.Millisecond)
			if err == nil {
				t.Fatal("unsafe probe accepted")
			}
		})
	}
}
func TestCapabilityVersion(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{{"", false}, {"2.1.317", false}, {"2.1.318 (Claude Code)", true}, {"2.2.0", true}, {"2.1.+318", false}, {"2.1.318-preview", false}} {
		if SupportedVersion(tc.v) != tc.ok {
			t.Fatalf("version %q", tc.v)
		}
	}
}

func TestProbeCancellationUsesRecordedTaskGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probe-pid")
	bin := fakeClaude(t, `printf '%s' "$$" > "$FIXTURE_MARKER"
exec sleep 10
`)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherHelper$", "--", "2", bin, "--version", bin)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZM_LAUNCH_FIXTURE=1", "FIXTURE_MARKER=" + marker}
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	if _, err = write.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		pid, _ = strconv.Atoi(string(b))
		if pid > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("probe did not start")
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group != cmd.Process.Pid {
		t.Fatalf("probe escaped task group: %d %v", group, err)
	}
	if err = syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("cancelled launcher succeeded")
	}
}

func TestUnsupportedVersionCannotExecCLIOrForgeTerminalEvents(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cli-marker")
	bin := fakeClaude(t, `if [ "$1" = --version ]; then printf '2.1.317 (Claude Code)\n'; exit 0; fi
printf executed > "$FIXTURE_MARKER"
`)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherHelper$", "--", "2", bin, "--version", bin, "--plugin-dir", "fixture")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZM_LAUNCH_FIXTURE=1", "FIXTURE_MARKER=" + marker}
	cmd.ExtraFiles = []*os.File{read}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_, _ = write.Write([]byte{1})
	_ = write.Close()
	if err = cmd.Wait(); err == nil {
		t.Fatal("unsupported CLI succeeded")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsupported CLI was executed")
	}
	if out.Len() != 0 || !strings.Contains(stderr.String(), "2.1.318") {
		t.Fatalf("capability failure was not honest stderr: stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestWrappedProbeAndExecRemainBehindGrant(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "wrapper-marker")
	bin := fakeClaude(t, `if [ "$1" = --version ]; then printf '2.1.318 (Claude Code)\n'; exit 0; fi
cat
`)
	wrapper := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf called\\n >> \"$FIXTURE_MARKER\"\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	args := []string{"-test.run=^TestLauncherHelper$", "--"}
	args = append(args, Arguments([]string{wrapper, bin, "--version"}, []string{wrapper, bin, "-p"})[1:]...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZM_LAUNCH_FIXTURE=1", "FIXTURE_MARKER=" + marker}
	cmd.ExtraFiles = []*os.File{read}
	cmd.Stdin = strings.NewReader("synthetic unchanged prompt")
	var out bytes.Buffer
	cmd.Stdout = &out
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stderr)
	if !scan.Scan() || scan.Text() != "launcher-ready" {
		t.Fatal("helper not ready")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("wrapper ran before grant")
	}
	_, _ = write.Write([]byte{1})
	_ = write.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(b), "called") != 2 {
		t.Fatalf("probe/final wrapper: %q %v", b, err)
	}
	if out.String() != "synthetic unchanged prompt" {
		t.Fatal("wrapper changed prompt")
	}
}
