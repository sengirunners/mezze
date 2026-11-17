package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func TestEnvMissingInterpreterCanRetryHandoff(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	ctx := context.Background()
	no := false
	source, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	bad := filepath.Join(t.TempDir(), "bad-agent")
	if err = os.WriteFile(bad, []byte("#!/usr/bin/env zm-test-missing-interpreter-8tp5-24\n"), 0700); err != nil {
		t.Fatal(err)
	}
	original := h.d.Config.File.Agents["claude"]
	h.d.Config.File.Agents["claude"] = []string{bad}
	req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID, ClientRequestID: "env-retry"}
	if _, err = orchclient.TaskStart(ctx, h.c, req); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("missing env interpreter was not refused before start: %v", err)
	}
	h.d.Config.File.Agents["claude"] = original
	child, err := orchclient.TaskStart(ctx, h.c, req)
	if err != nil || child.ID == "" {
		t.Fatalf("retry after repairing agent: %+v %v", child, err)
	}
	child = waitTask(t, h, child.ID)
	if child.Result == nil || child.Result.Finished != "done" {
		t.Fatalf("continued child: %+v", child)
	}
}

func TestStartedExit127KeepsContinuationReservation(t *testing.T) {
	for _, message := range []string{"", "env: missing-interpreter: No such file or directory"} {
		t.Run(message, func(t *testing.T) {
			h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`)
			taskClient(h, parent)
			ctx := context.Background()
			no := false
			source, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
			if err != nil {
				t.Fatal(err)
			}
			source = waitTask(t, h, source.ID)
			dir := t.TempDir()
			marker := filepath.Join(dir, "work-performed")
			// A known basename also exercises the metadata version path: it
			// must not execute this agent separately before the task starts.
			agent := filepath.Join(dir, "claude")
			// Perform observable work before returning the env-like failure.
			body := "#!/bin/sh\nif [ \"$1\" = auth ] && [ \"$2\" = status ]; then exit 0; fi\nprintf 'work\\n' >> " + shellQuote(marker) + "\nprintf '%s\\n' " + shellQuote(message) + " >&2\nexit 127\n"
			if err = os.WriteFile(agent, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			h.d.Config.File.Agents["claude"] = []string{agent}
			req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID, ClientRequestID: "started-retry"}
			child, err := orchclient.TaskStart(ctx, h.c, req)
			if err != nil {
				t.Fatal(err)
			}
			_ = waitTask(t, h, child.ID)
			again, err := orchclient.TaskStart(ctx, h.c, req)
			if err != nil || again.ID != child.ID {
				t.Fatalf("started continuation must remain idempotent: first=%s again=%+v err=%v", child.ID, again, err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "work\n" {
				t.Fatalf("started child work must run exactly once: %q %v", data, err)
			}
		})
	}
}

func TestTaskVersionMetadataNeverExecutesAgent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "version-executed")
	bin := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nprintf 'probe\\n' >> " + shellQuote(marker) + "\nprintf 'fixture-v1\\n'\n"
	if err := os.WriteFile(bin, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Service{versions: make(map[string]string)}
	if got := s.version("claude", bin, nil, true); got != "" {
		t.Fatalf("uncached task version: %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("uncached task version executed the agent: %v", err)
	}
	if got := s.version("claude", bin, nil, false); got != "fixture-v1" {
		t.Fatalf("ordinary session version probe: %q", got)
	}
	if got := s.version("claude", bin, nil, true); got != "fixture-v1" {
		t.Fatalf("task should reuse known metadata: %q", got)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "probe\n" {
		t.Fatalf("only the ordinary version lookup may execute: %q %v", data, err)
	}
}
