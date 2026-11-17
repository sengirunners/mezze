package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/store"
)

func TestRecordedProbeHelper(t *testing.T) {
	if os.Getenv("FIXTURE_RECORDED_PROBE") != "1" {
		return
	}
	marker := os.Getenv("FIXTURE_RECORD_MARKER")
	_ = os.WriteFile(marker, []byte("attempted"), 0600)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(os.Getenv("ZM_HOME"), store.FileRegistry)+"?mode=ro")
	if err != nil {
		os.Exit(2)
	}
	defer db.Close()
	_, id, _ := strings.Cut(os.Getenv("ZM_SESSION"), "/")
	var group, holder int
	var groupStart, holderStart int64
	err = db.QueryRow(`SELECT t.pgid,t.group_start,h.holder_pid,h.holder_start FROM session_tasks t JOIN session_holders h ON h.id=t.id WHERE t.id=? AND t.state='running'`, id).Scan(&group, &groupStart, &holder, &holderStart)
	if err != nil || group != syscall.Getpgrp() || groupStart <= 0 || holder <= 1 || holderStart <= 0 {
		os.Exit(3)
	}
	_ = os.WriteFile(marker, []byte("recorded"), 0600)
	fmt.Println("2.1.318 (Claude Code)")
	os.Exit(0)
}

func TestClaudeSkillsProbeRequiresDurableTaskGroup(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprint(refuse), func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "probe-marker")
			bin := filepath.Join(t.TempDir(), "claude")
			helper := shellJoin([]string{"env", "FIXTURE_RECORDED_PROBE=1", "FIXTURE_RECORD_MARKER=" + marker, exe, "-test.run=^TestRecordedProbeHelper$"})
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then exec " + helper + "; fi\ncat > \"$ZM_SCRATCH/actual-prompt\"\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"finished\":\"done\",\"text\":\"fixture\"}}'\n"
			if err = os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, func(c *config.Config) { c.File.Agents = map[string][]string{"claude": {bin}} })
			dir := filepath.Join(h.home, ".claude/skills/probe")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: probe\ndescription: synthetic fixture\n---\nbody"), 0600); err != nil {
				t.Fatal(err)
			}
			parent, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "fixture-parent", Cwd: h.home, PID: os.Getpid()})
			if err != nil {
				t.Fatal(err)
			}
			taskClient(h, parent)
			if refuse {
				if _, err = h.d.Store.Registry().Exec(`CREATE TRIGGER refuse_group BEFORE UPDATE OF pgid ON session_tasks BEGIN SELECT RAISE(ABORT,'synthetic durable group failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			no := false
			task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "synthetic original prompt", Worktree: &no})
			if refuse {
				if err == nil {
					t.Fatal("launch with missing durable group succeeded")
				}
				if _, e := os.Stat(marker); !os.IsNotExist(e) {
					t.Fatal("probe executed before durable group recording")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result := waitTask(t, h, task.ID)
			if result.Result == nil || result.Result.Outcome.Kind != orchapi.OutcomeOK {
				t.Fatalf("task result: %+v", result)
			}
			b, err := os.ReadFile(marker)
			if err != nil || string(b) != "recorded" {
				t.Fatalf("probe lacked durable authority: %q %v", b, err)
			}
			ss, err := orchclient.Session(context.Background(), h.c, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			b, err = os.ReadFile(filepath.Join(ss.Scratch, "actual-prompt"))
			if err != nil || !strings.HasPrefix(string(b), "synthetic original prompt\n") {
				t.Fatalf("probe consumed prompt: %q %v", b, err)
			}
		})
	}
}
