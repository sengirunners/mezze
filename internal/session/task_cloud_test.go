package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

// fakeProviders are claude and codex CLIs that answer the cloud calls the
// watcher makes and log each argv (one per line) to log.
func fakeProviders(t *testing.T) (claude, codex, log string) {
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	count := filepath.Join(dir, "statuses")
	claude, codex = filepath.Join(dir, "claude"), filepath.Join(dir, "codex")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+log+"\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(claude, `printf 'Created cloud session: fix\nView: https://claude.ai/code/session_01XY?from=cli&m=0\nResume with: claude --teleport session_01XY\n'`)
	write(codex, `echo "codex cloud log" >> error.log
case "$1 $2" in
"cloud exec") echo "https://chatgpt.com/codex/tasks/task_e_77" ;;
"cloud status") n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+count+`
  if [ $n -lt 2 ]; then echo "[PENDING] fix"; exit 1; fi; echo "[READY] fix" ;;
"cloud list") echo '{"tasks":[],"cursor":null}' ;;
"cloud diff") printf 'diff --git a/x b/x\n+fixed\n' ;;
*) exit 2 ;;
esac`)
	return claude, codex, log
}

func cloudHarness(t *testing.T) (*harness, orchapi.Session, string) {
	t.Helper()
	claude, codex, log := fakeProviders(t)
	h := newHarness(t, func(c *config.Config) { c.File.Agents = map[string][]string{"claude": {claude}, "codex": {codex}} })
	h.s.CloudPoll = 20 * time.Millisecond
	h.project("acme", h.home)
	parent := h.spawn("shell", "exec sleep 30", nil)
	return h, parent, log
}

func setCloud(t *testing.T, h *harness, u orchapi.TaskCloudUpdate) (orchapi.TaskCloudSettings, error) {
	t.Helper()
	c := *h.c
	c.As, c.Project, c.ProjectFrom = "", "acme", "flag"
	return orchclient.SetTaskCloud(context.Background(), &c, u)
}

func waitCloud(t *testing.T, h *harness, id string) orchapi.TaskView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		v, err := orchclient.TaskStatusView(context.Background(), h.c, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == "ended" {
			return v
		}
	}
	t.Fatalf("cloud task did not end: %s", taskCaptureDiagnostic(h, id))
	return orchapi.TaskView{}
}

// A project that has not named a runtime's environment sends that provider
// nothing: the start is refused before any CLI runs.
func TestCloudTaskRefusedUntilTheProjectOptsIn(t *testing.T) {
	h, parent, log := cloudHarness(t)
	taskClient(h, parent)
	_, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "codex", Prompt: "fix it"})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "not opted in") {
		t.Fatalf("start without opt-in: %v", err)
	}
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Claude: strPtr("env_1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "codex", Prompt: "fix it"}); err == nil {
		t.Fatal("claude's opt-in let a codex cloud task start")
	}
	for _, bad := range []orchapi.TaskStartRequest{
		{Agent: "codex", Prompt: "x", Machine: "auto"},
		{Agent: "claude", Prompt: "x", Model: "opus"},
		{Agent: "claude", Prompt: "x", OnLimit: "next_agent"},
		{Agent: "claude", Prompt: strings.Repeat("x", cloudPromptLimit+1)},
	} {
		if _, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if _, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "moon", orchapi.TaskStartRequest{Agent: "claude", Prompt: "x"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("where moon: %v", err)
	}
	// Naming the opted-in project does not make another repo its checkout:
	// the provider would clone the cwd's repo.
	other := *h.c
	other.Project, other.ProjectFrom, other.Cwd = "acme", "flag", t.TempDir()
	if _, err := orchclient.TaskStartWhere(context.Background(), &other, "", "cloud", orchapi.TaskStartRequest{Agent: "claude", Prompt: "x"}); !api.IsCode(err, api.CodeBadRequest) ||
		!strings.Contains(err.Error(), "checkout") {
		t.Fatalf("cloud task from a repo outside the project: %v", err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("a refused cloud task ran the provider CLI:\n%s", b)
	}
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Codex: strPtr("--env=x")}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a flag as environment: %v", err)
	}
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Codex: strPtr("default")}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("codex default: %v", err)
	}
	// An agent cannot opt its own project in.
	if _, err := orchclient.SetTaskCloud(context.Background(), h.c, orchapi.TaskCloudUpdate{Codex: strPtr("env-1")}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a session opted the project in: %v", err)
	}
}

// A Codex cloud task: submitted with the project's environment and the bare
// prompt, followed until ready, ended with its diff, and its result mailed
// to the parent like a local task's.
func TestCodexCloudTaskEndsWithTheDiff(t *testing.T) {
	h, parent, log := cloudHarness(t)
	var mu sync.Mutex
	var mails []string
	h.s.taskDeliver = func(_ context.Context, _ *authz.Subject, _ orchapi.Session, kind, to, body string) error {
		mu.Lock()
		defer mu.Unlock()
		if kind == "mail" && to == parent.Address {
			mails = append(mails, body)
		}
		return nil
	}
	set, err := setCloud(t, h, orchapi.TaskCloudUpdate{Codex: strPtr("env-123")})
	if err != nil || set.Codex != "env-123" || set.Project != "acme" {
		t.Fatalf("set: %+v %v", set, err)
	}
	taskClient(h, parent)
	v, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "codex", Prompt: "fix the bug", ClientRequestID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Agent != "codex-cloud" || v.Cloud == nil || v.Cloud.Agent != "codex" {
		t.Fatalf("started: %+v cloud %+v", v.Task, v.Cloud)
	}
	// The same client_request_id for a local task is a different request.
	if _, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "fix the bug", ClientRequestID: "c1"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("client_request_id reused for a local start: %v", err)
	}
	done := waitCloud(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Result.Finished != "unreported" ||
		!strings.Contains(done.Result.Text, "+fixed") || !strings.Contains(done.Result.Text, "codex cloud apply task_e_77") {
		t.Fatalf("result: %+v", done.Result)
	}
	if c := done.Cloud; c == nil || c.ID != "task_e_77" || c.URL != "https://chatgpt.com/codex/tasks/task_e_77" || c.Status != "ready" || c.Environment != "env-123" {
		t.Fatalf("cloud view: %+v", done.Cloud)
	}
	calls, _ := os.ReadFile(log)
	if first := strings.SplitN(string(calls), "\n", 2)[0]; !strings.HasPrefix(first, "cloud exec --env env-123 ") || !strings.HasSuffix(first, " -- fix the bug") {
		t.Fatalf("submitted %q: want the project's environment and the prompt as given", first)
	}
	if _, err := os.Stat(filepath.Join(h.home, "error.log")); err == nil {
		t.Fatal("codex cloud's error.log landed in the task's checkout")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(mails)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no result mail to the parent")
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(mails[0], "codex cloud apply task_e_77") {
		t.Fatalf("mail: %s", mails[0])
	}
}

// A Claude cloud task ends once the session exists: Claude Code has no
// status command, so the result says where to follow it.
func TestClaudeCloudTaskEndsWithTheSessionLink(t *testing.T) {
	h, parent, log := cloudHarness(t)
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Claude: strPtr("default")}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(h.home, "projects", "*", "project.toml"))
	var b []byte
	var err error
	if len(files) == 1 {
		b, err = os.ReadFile(files[0])
	}
	if len(files) != 1 || err != nil || !strings.Contains(string(b), "[cloud]\nclaude = \"default\"") {
		t.Fatalf("project.toml: %v\n%s", err, b)
	}
	taskClient(h, parent)
	v, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "claude", Prompt: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitCloud(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || !strings.Contains(done.Result.Text, "claude --teleport session_01XY") {
		t.Fatalf("result: %+v", done.Result)
	}
	if done.Cloud == nil || done.Cloud.ID != "session_01XY" || done.Cloud.URL != "https://claude.ai/code/session_01XY?from=cli&m=0" {
		t.Fatalf("cloud view: %+v", done.Cloud)
	}
	if calls, _ := os.ReadFile(log); strings.TrimSpace(string(calls)) != "--cloud=fix the bug --permission-mode auto" {
		t.Fatalf("claude ran with %q: default is the account's own environment, no flag", calls)
	}
}

// A named Anthropic-hosted environment is selected through --settings.
func TestClaudeCloudTaskSelectsTheNamedEnvironment(t *testing.T) {
	h, parent, log := cloudHarness(t)
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Claude: strPtr("env_01AbC")}); err != nil {
		t.Fatal(err)
	}
	taskClient(h, parent)
	v, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "claude", Prompt: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitCloud(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeOK || done.Cloud == nil || done.Cloud.ID != "session_01XY" || done.Cloud.Environment != "env_01AbC" {
		t.Fatalf("result %+v, cloud %+v", done.Result, done.Cloud)
	}
	want := `--settings {"remote":{"defaultEnvironmentId":"env_01AbC"}} --cloud=fix the bug --permission-mode auto`
	if calls, _ := os.ReadFile(log); strings.TrimSpace(string(calls)) != want {
		t.Fatalf("claude ran with %q, want %q", calls, want)
	}
}

func strPtr(s string) *string { return &s }

// Cancelling a cloud task stops zm's watch: it ends killed and the watcher
// removes its scratch (where codex cloud's error.log went) before it dies.
func TestCancelledCloudTaskCleansUp(t *testing.T) {
	h, parent, _ := cloudHarness(t)
	h.s.CloudPoll = time.Hour // never polls: still pending when cancelled
	if _, err := setCloud(t, h, orchapi.TaskCloudUpdate{Codex: strPtr("env-123")}); err != nil {
		t.Fatal(err)
	}
	taskClient(h, parent)
	v, err := orchclient.TaskStartWhere(context.Background(), h.c, "", "cloud", orchapi.TaskStartRequest{Agent: "codex", Prompt: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := orchclient.TaskStatusView(context.Background(), h.c, v.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if st.Cloud != nil && st.Cloud.Status == "submitted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never submitted: %+v", st.Cloud)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := orchclient.TaskCancel(context.Background(), h.c, v.ID); err != nil {
		t.Fatal(err)
	}
	done := waitCloud(t, h, v.ID)
	if done.Result == nil || done.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("cancelled: %+v", done.Result)
	}
	left, _ := filepath.Glob(filepath.Join(h.home, "*", "*", "cloud-*"))
	if len(left) != 0 {
		t.Fatalf("watcher scratch left behind: %v", left)
	}
}
