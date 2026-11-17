package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/mezze/internal/initgc"
	"github.com/sengirunners/mezze/internal/repoguard"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/protocol"
)

// TestMain lets a test run this binary as zm itself (ZM_TEST_MAIN=1).
func TestMain(m *testing.M) {
	initgc.Restore() // as main does: the helpers below are long-lived processes
	// Tests never read this machine's swap: the authority gives box secrets only to a host whose
	// swap is safe, and a plain-disk swap here would turn every secrets test into a refusal.
	hostswap.Local = func(context.Context) hostswap.Reading { return hostswap.Reading{State: hostswap.Off} }
	newUpdateSchedule = func() (*updateSchedule, error) { return nil, errors.New("tests never install a real update schedule") }
	lendHostNow = func(int, int) (protocol.HostStatus, error) { // tests never read the host-wide switch (lend_switch_test.go sets its own)
		return protocol.HostStatus{Mode: "auto", State: "accepting"}, nil
	}
	lendHostSet = func(string, string, string, string) error { return errors.New("tests never set the host-wide switch") }
	if code, ok := refdriver.Main(); ok { // the reference run-target driver runs as this binary (pool_test.go)
		os.Exit(code)
	}
	if sock := os.Getenv("ZM_TEST_WEDGE"); sock != "" {
		wedge(sock)
	}
	if os.Getenv("ZM_TEST_MAIN") == "1" {
		testBuildHooks()
		os.Args = append([]string{"zm"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	// A test run inside a pool box still tests the socket client.
	os.Unsetenv("POTLUCK_BOX_CREDENTIAL")
	os.Exit(repoguard.Main(m)) // fails the run if a test rewrote the git config of the repository around cmd/zm (zm-jmep)
}

// TestNoTerminalProbeAtStartup: every zm call on a terminal used to ask it
// for its background colour at package init (bubbletea), and waited 5 s on a
// pty nobody answers (an agent's, a CI runner's). Nothing may write to the
// terminal or wait for it before the command runs.
func TestNoTerminalProbeAtStartup(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	for _, args := range [][]string{{"--version"}, {"status"}} {
		c := exec.Command(os.Args[0], args...)
		c.Env = append(os.Environ(), "ZM_TEST_MAIN=1", "ZM_HOME="+home, "TERM=xterm-256color", "CI=")
		start := time.Now()
		f, err := pty.Start(c)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		copied := make(chan struct{})
		go func() { _, _ = io.Copy(&out, f); close(copied) }()
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.Process.Kill()
			<-done
			t.Fatalf("zm %v on an unanswered pty did not exit within 3s", args)
		}
		f.Close()
		<-copied
		if d := time.Since(start); d > time.Second {
			t.Errorf("zm %v on an unanswered pty took %s", args, d)
		}
		if bytes.Contains(out.Bytes(), []byte("\x1b]11;")) || bytes.Contains(out.Bytes(), []byte("\x1b[6n")) {
			t.Errorf("zm %v queried the terminal: %q", args, out.String())
		}
	}
}
