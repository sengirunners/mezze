package taskcontext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func put(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func skill(t *testing.T, root, name string) {
	put(t, root, ".claude/skills/"+name+"/SKILL.md", "---\nname: "+name+"\ndescription: synthetic fixture\n---\nNative skill $ARGUMENTS\n")
}
func TestSnapshotResolvedAncestorsAndFrozenNativeFiles(t *testing.T) {
	repo := t.TempDir()
	scratch := t.TempDir()
	put(t, repo, ".git", "worktree fixture")
	skill(t, repo, "root-skill")
	nested := filepath.Join(repo, "app")
	skill(t, nested, "app-skill")
	put(t, repo, ".claude/skills/root-skill/ref.md", "original")
	// No source config is opened or copied; unreadable invalid contents are inert.
	put(t, repo, ".claude/settings.json", "not JSON")
	put(t, repo, ".claude/commands/legacy.md", "excluded legacy")
	got, err := Build(context.Background(), nested, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Directories) != 2 || got.Directories[0] != repo || got.Directories[1] != nested || len(got.Skills) != 2 {
		t.Fatalf("wrong ancestry %+v", got)
	}
	put(t, repo, ".claude/skills/root-skill/ref.md", "mutated")
	b, err := os.ReadFile(filepath.Join(got.Plugin, "skills/root-skill/ref.md"))
	if err != nil || string(b) != "original" {
		t.Fatal("snapshot changed")
	}
	if _, err = os.Stat(filepath.Join(got.Plugin, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source settings copied")
	}
	b, _ = os.ReadFile(filepath.Join(got.Plugin, ".claude-plugin/plugin.json"))
	if string(b) != `{"name":"zm-task-repo","version":"0.0.0"}` {
		t.Fatalf("unexpected plugin manifest %s", b)
	}
}
func TestSnapshotRejectsUnsafeAndAmbiguousSources(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"escaping link": func(t *testing.T, r string) {
			outside := filepath.Join(t.TempDir(), "outside")
			put(t, filepath.Dir(outside), "outside", "do not read")
			if e := os.Symlink(outside, filepath.Join(r, ".claude/skills/probe/ref")); e != nil {
				t.Fatal(e)
			}
		},
		"contained link": func(t *testing.T, r string) {
			if e := os.Symlink("SKILL.md", filepath.Join(r, ".claude/skills/probe/ref")); e != nil {
				t.Fatal(e)
			}
		},
		"fifo": func(t *testing.T, r string) {
			if e := syscall.Mkfifo(filepath.Join(r, ".claude/skills/probe/pipe"), 0600); e != nil {
				t.Fatal(e)
			}
		},
		"large": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/large", strings.Repeat("x", MaxFileBytes+1))
		},
		"missing skill": func(t *testing.T, r string) { os.Remove(filepath.Join(r, ".claude/skills/probe/SKILL.md")) },
		"renamed": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/SKILL.md", "---\nname: different\n---\nbody")
		},
		"duplicate name": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/SKILL.md", "---\nname: probe\nname: probe\n---\nbody")
		},
		"merge key": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/SKILL.md", "---\nname: probe\n<<: *name\n---\nbody")
		},
		"plugin": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/.claude-plugin/plugin.json", "never parse")
		},
		"MCP": func(t *testing.T, r string) { put(t, r, ".claude/skills/probe/.mcp.json", "never parse") },
		"depth": func(t *testing.T, r string) {
			put(t, r, ".claude/skills/probe/"+strings.Repeat("deep/", MaxDepth+1)+"ref", "deep")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			repo, scratch := t.TempDir(), t.TempDir()
			skill(t, repo, "probe")
			mutate(t, repo)
			if _, err := Build(context.Background(), repo, scratch); err == nil {
				t.Fatal("unsafe source accepted")
			}
			if _, err := os.Stat(filepath.Join(scratch, "task-context")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial snapshot leaked")
			}
		})
	}
}
func TestSnapshotDuplicateAncestorAndCancellation(t *testing.T) {
	repo, scratch := t.TempDir(), t.TempDir()
	put(t, repo, ".git", "")
	skill(t, repo, "probe")
	nested := filepath.Join(repo, "child")
	skill(t, nested, "probe")
	if _, err := Build(context.Background(), nested, scratch); err == nil {
		t.Fatal("shadowed skill silently chosen")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, repo, scratch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
func TestSnapshotAllResourceBudgets(t *testing.T) {
	t.Run("empty directories", func(t *testing.T) {
		repo, scratch := t.TempDir(), t.TempDir()
		skill(t, repo, "probe")
		for i := 0; i < MaxFiles; i++ {
			if err := os.Mkdir(filepath.Join(repo, ".claude/skills/probe", fmtName(i)), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Build(context.Background(), repo, scratch); err == nil {
			t.Fatal("unbounded empty directories")
		}
	})
	t.Run("total bytes", func(t *testing.T) {
		repo, scratch := t.TempDir(), t.TempDir()
		skill(t, repo, "probe")
		for i := 0; i < 8; i++ {
			put(t, repo, ".claude/skills/probe/"+fmtName(i), strings.Repeat("x", MaxFileBytes))
		}
		if _, err := Build(context.Background(), repo, scratch); err == nil {
			t.Fatal("total resource bound ignored")
		}
	})
}
func fmtName(i int) string { return fmt.Sprintf("resource-%04d", i) }

func TestSnapshotFrontmatterSubsetPreservesBytes(t *testing.T) {
	for _, header := range []string{
		"name: probe\ndescription: [broken",
		"name: probe\ndescription: ''",
		"name: probe\ndescription: |\n    first\n  second",
		"name: probe\ndescription: true",
		"name: probe\ndescription: |",
		"name: probe\ndescription: synthetic\nhooks:\n  Stop: []",
		"name: probe\ndescription: synthetic\nmetadata:\n  name: hidden",
		"name: probe\ndescription: synthetic\nuser-invocable: \"false\"",
		"name: probe\ndescription: synthetic\nagent: custom-agent",
	} {
		if validateName([]byte("---\n"+header+"\n---\nbody"), "probe") == nil {
			t.Fatalf("unsupported header accepted %q", header)
		}
	}
	repo, scratch := t.TempDir(), t.TempDir()
	original := "---\nname: 'probe'\ndescription: >-\n  A bounded native fixture.\nallowed-tools: Bash(git *) Read\ndisable-model-invocation: false\n---\nPreserve $ARGUMENTS and !\x60printf synthetic\x60 verbatim.\n"
	put(t, repo, ".claude/skills/probe/SKILL.md", original)
	got, err := Build(context.Background(), repo, scratch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(got.Plugin, "skills/probe/SKILL.md"))
	if err != nil || string(b) != original {
		t.Fatal("skill source rewritten")
	}
}
