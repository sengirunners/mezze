package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestClaudeTaskContextVersionGuard(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{{"", false}, {"wrapper", false}, {"2.1.317 (Claude Code)", false}, {"2.1.318 (Claude Code)", true}, {"2.2.0", true}, {"2.1.318-preview", false}} {
		if claudeTaskSkillVersion(tc.v) != tc.ok {
			t.Fatalf("version %q", tc.v)
		}
	}
}
func TestClaudeTaskContextResolvedCwdAndAuthUnchanged(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "resolved-worktree")
	scratch := filepath.Join(dir, "scratch")
	if e := os.MkdirAll(filepath.Join(repo, ".claude/skills/probe"), 0700); e != nil {
		t.Fatal(e)
	}
	os.Mkdir(scratch, 0700)
	os.WriteFile(filepath.Join(repo, ".claude/skills/probe/SKILL.md"), []byte("---\nname: probe\ndescription: synthetic\n---\nbody\n"), 0600)
	bin := filepath.Join(dir, "claude")
	marker := filepath.Join(dir, "pre-holder-marker")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf touched > '"+marker+"'\nprintf '2.1.318 (Claude Code)\\n'\n"), 0700)
	s := &Service{HolderExe: "/tmp/zm-test", versions: map[string]string{}, d: &core.Deps{Config: &config.Config{}}}
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "FAKE_EXISTING", "PATH": "/usr/bin:/bin"}
	argv, e := s.taskCommand("claude", env)
	if e != nil {
		t.Fatal(e)
	}
	got, guarded, e := s.prepareClaudeTaskContext(context.Background(), bin, argv, env, repo, scratch, "fixture/12345678")
	if e != nil {
		t.Fatal(e)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("snapshot preparation executed agent before holder")
	}
	if !guarded {
		t.Fatal("skills missing recorded launch guard")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--add-dir "+repo) || !strings.Contains(joined, "--plugin-dir "+filepath.Join(scratch, "task-context/plugin")) || !strings.Contains(joined, "disableSkillShellExecution") || !strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("task context flags: %v", got)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "FAKE_EXISTING" || env["CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"] != "1" {
		t.Fatal("auth changed or native instructions missing")
	}
}

func TestClaudeTaskNoSkillsKeepsOriginalSettings(t *testing.T) {
	repo, scratch := t.TempDir(), t.TempDir()
	s := &Service{HolderExe: "/tmp/zm-test", d: &core.Deps{Config: &config.Config{}}}
	env := map[string]string{"ANTHROPIC_API_KEY": "FAKE_EXISTING"}
	argv, e := s.taskCommand("claude", env)
	if e != nil {
		t.Fatal(e)
	}
	got, guarded, e := s.prepareClaudeTaskContext(context.Background(), "/unversioned-wrapper", argv, env, repo, scratch, "fixture/12345678")
	if e != nil {
		t.Fatal(e)
	}
	if guarded {
		t.Fatal("no-skills launch unexpectedly guarded")
	}
	if strings.Contains(strings.Join(got, " "), "disableSkillShellExecution") || strings.Contains(strings.Join(got, " "), "--plugin-dir") {
		t.Fatal("unguarded setting added without skills")
	}
	if env["ANTHROPIC_API_KEY"] != "FAKE_EXISTING" {
		t.Fatal("auth changed")
	}
}
