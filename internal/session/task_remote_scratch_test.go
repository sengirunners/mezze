package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// An approved ask's agent action on a machine with no known
// checkout of the project ran nowhere and failed after the owner's yes. It
// now runs in a scratch directory of its own, removed when it ends; a plain
// remote start without a cwd is still refused.
func TestAskAgentActionWithoutCheckoutRunsInScratch(t *testing.T) {
	where := filepath.Join(t.TempDir(), "where")
	script := `pwd > '` + where + `'; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"finished\":\"done\",\"text\":\"ok\"}"}'`
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *config.Config) {
		c.File.Agents = map[string][]string{"claude": {bin}}
		c.File.RemoteTasks = true
	})
	proj := h.project("acme", filepath.Join(h.home, "nowhere"))
	ctx := context.Background()
	req := orchapi.TaskStartRequest{Agent: "claude", Prompt: "add the rulings"}

	_, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+"plain", orchapi.TaskStartOp{Parent: "elsewhere/0a1b2c3d", Depth: 1, Request: req})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "no known checkout") {
		t.Fatalf("a plain start without a checkout: %v", err)
	}

	task, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+"ask", orchapi.TaskStartOp{Parent: "elsewhere/0a1b2c3d", Depth: 1, Request: req, Ask: true})
	if err != nil {
		t.Fatal(err)
	}
	var cur orchapi.Task
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cur, err = h.s.RemoteTaskStatus(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		if cur.State == "ended" {
			break
		}
	}
	if cur.State != "ended" || cur.Result == nil || cur.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("ask action: %+v (%s)", cur, taskCaptureDiagnostic(h, task.ID))
	}
	b, err := os.ReadFile(where)
	if err != nil {
		t.Fatal(err)
	}
	ran := strings.TrimSpace(string(b))
	scratch, _ := filepath.EvalSymlinks(h.s.askScratchDir())
	if got, _ := filepath.EvalSymlinks(filepath.Dir(ran)); got != scratch && filepath.Dir(ran) != h.s.askScratchDir() {
		t.Fatalf("ran in %s, want a directory under %s", ran, h.s.askScratchDir())
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(h.s.askScratch(orchapi.RemoteTaskPrefix + "ask")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scratch directory outlived its task")
		}
	}
	// A repeat of the op answers the task it started and makes no directory.
	again, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+"ask", orchapi.TaskStartOp{Parent: "elsewhere/0a1b2c3d", Depth: 1, Request: req, Ask: true})
	if err != nil || again.ID != task.ID {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if _, err := os.Stat(h.s.askScratch(orchapi.RemoteTaskPrefix + "ask")); !os.IsNotExist(err) {
		t.Fatalf("a repeat made the scratch directory again: %v", err)
	}
	// A task of its own standing in another op's scratch directory never
	// removes it: only the task made for it does.
	other := h.s.askScratch(orchapi.RemoteTaskPrefix + "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	no := false
	child, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+"child", orchapi.TaskStartOp{Parent: "elsewhere/0a1b2c3d", Depth: 1,
		Request: orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Cwd: other, Worktree: &no}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cur, err = h.s.RemoteTaskStatus(ctx, child.ID); err != nil || cur.State == "ended" {
			break
		}
	}
	if cur.State != "ended" {
		t.Fatalf("child: %+v %v", cur, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a task removed a scratch directory not made for it: %v", err)
	}
	// The task's session there is never taken for the project's checkout.
	if got := h.s.projectCheckout(ctx, proj.ID); got != "" {
		t.Fatalf("project checkout %q", got)
	}
}

// An approved ask's action failed after the owner's yes when the
// project's most recent checkout here held a submodule (no worktree is made
// from one). It now takes the next checkout it can use, else its scratch
// directory; a plain remote start is still refused there.
func TestAskAgentActionSkipsUnusableCheckout(t *testing.T) {
	where := filepath.Join(t.TempDir(), "where")
	script := `pwd > '` + where + `'; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"finished\":\"done\",\"text\":\"ok\"}"}'`
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *config.Config) {
		c.File.Agents = map[string][]string{"claude": {bin}}
		c.File.RemoteTasks = true
	})
	ctx := context.Background()
	repos := t.TempDir()
	repo := func(name string, gitlink bool) string {
		dir := filepath.Join(repos, name)
		maintenanceGit(t, repos, "init", "--quiet", dir)
		maintenanceWrite(t, filepath.Join(dir, name+".txt"), name)
		maintenanceGit(t, dir, "add", ".")
		if gitlink {
			maintenanceGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",sub")
		}
		maintenanceGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", name)
		return dir
	}
	good, bad := repo("good", false), repo("bad", true)
	proj := h.project("acme", good, bad)
	h.spawn("shell", "exec sleep 30", func(req *orchapi.SpawnRequest) { req.Cwd = good })
	time.Sleep(1100 * time.Millisecond)
	h.spawn("shell", "exec sleep 30", func(req *orchapi.SpawnRequest) { req.Cwd = bad })
	if got := h.s.projectCheckouts(ctx, proj.ID, askCheckouts); len(got) != 2 || got[0] != bad {
		t.Fatalf("checkouts %q: want the submodule's first", got)
	}
	parent := orchapi.TaskStartOp{Parent: "elsewhere/0a1b2c3d", Depth: 1, Request: orchapi.TaskStartRequest{Agent: "claude", Prompt: "go"}}
	if _, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+"plain", parent); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("a plain start in the submodule checkout: %v", err)
	}
	ran := func(op string) string {
		t.Helper()
		_ = os.Remove(where)
		ask := parent
		ask.Ask = true
		task, err := h.s.StartRemoteTask(ctx, proj.ID, orchapi.RemoteTaskPrefix+op, ask)
		if err != nil {
			t.Fatalf("ask action: %v", err)
		}
		var cur orchapi.Task
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if cur, err = h.s.RemoteTaskStatus(ctx, task.ID); err != nil || cur.State == "ended" {
				break
			}
		}
		if cur.State != "ended" || cur.Result == nil || cur.Result.Outcome.Kind != orchapi.OutcomeOK {
			t.Fatalf("ask action: %+v (%s)", cur, taskCaptureDiagnostic(h, task.ID))
		}
		b, err := os.ReadFile(where)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if dir := ran("next"); func() error { _, err := os.Stat(filepath.Join(dir, "good.txt")); return err }() != nil {
		t.Fatalf("ran in %s, want a worktree of the usable checkout", dir)
	}
	// With only the submodule's checkout left, its scratch directory.
	if err := os.RemoveAll(good); err != nil {
		t.Fatal(err)
	}
	dir := ran("scratch")
	scratch, _ := filepath.EvalSymlinks(h.s.askScratchDir())
	if got, _ := filepath.EvalSymlinks(filepath.Dir(dir)); got != scratch && filepath.Dir(dir) != h.s.askScratchDir() {
		t.Fatalf("ran in %s, want a directory under %s", dir, h.s.askScratchDir())
	}
}
