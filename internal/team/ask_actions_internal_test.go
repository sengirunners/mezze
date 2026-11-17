package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// authorize_key adds the frozen line once: a key already there (behind
// options, too) is left alone, and a file without a final newline keeps its
// last line whole.
func TestAppendAuthorizedKey(t *testing.T) {
	home := t.TempDir()
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB ivo@macbook"
	if already, err := appendAuthorizedKey(home, line, "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAIB"); err != nil || already {
		t.Fatalf("first: %v %v", already, err)
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	if err := os.WriteFile(path, []byte("from=\"10.0.0.1\" ssh-ed25519 AAAAOTHER other\nno-pty ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB old-comment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if already, err := appendAuthorizedKey(home, line, "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAIB"); err != nil || !already {
		t.Fatalf("behind options: %v %v", already, err)
	}
	if already, err := appendAuthorizedKey(home, "ssh-ed25519 AAAANEW new", "ssh-ed25519", "AAAANEW"); err != nil || already {
		t.Fatalf("new: %v %v", already, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(b), "old-comment\nssh-ed25519 AAAANEW new\n") {
		t.Fatalf("file:\n%s", b)
	}
}

// A run action is done only for an ok exit; anything else fails with how
// it ended in plain words.
func TestTaskActionStatus(t *testing.T) {
	zero, one := 0, 1
	now := time.Now()
	for _, c := range []struct {
		t     orchapi.Task
		state string
		why   string
	}{
		{orchapi.Task{State: orchapi.TaskPending}, orchapi.ActionQueued, ""},
		{orchapi.Task{State: "running"}, orchapi.ActionRunning, ""},
		{orchapi.Task{State: orchapi.TaskRefused, Error: "box did not take it within 10m0s"}, orchapi.ActionFailed, "did not take it"},
		{orchapi.Task{State: "ended", Result: &orchapi.TaskResult{Outcome: orchapi.Outcome{Kind: orchapi.OutcomeOK, ExitCode: &zero}}}, orchapi.ActionDone, ""},
		{orchapi.Task{State: "ended", Result: &orchapi.TaskResult{Text: "a\nb\nc\nno space left", Outcome: orchapi.Outcome{Kind: orchapi.OutcomeFailed, ExitCode: &one}}}, orchapi.ActionFailed, "it exited 1: b / c / no space left"},
		{orchapi.Task{State: "ended", Result: &orchapi.TaskResult{Outcome: orchapi.Outcome{Kind: orchapi.OutcomeKilled, Signal: "SIGKILL"}}}, orchapi.ActionFailed, "killed (SIGKILL)"},
	} {
		got := taskActionStatus(c.t, now)
		if got.State != c.state || !strings.Contains(got.Reason, c.why) {
			t.Errorf("%+v: %+v", c.t, got)
		}
	}
}
