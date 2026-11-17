package taskcontext

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentRejectsWhitespaceOnlyDescriptionBlock(t *testing.T) {
	for _, description := range []string{"|\n  ", ">-\n    "} {
		t.Run(description, func(t *testing.T) {
			repo, scratch := t.TempDir(), t.TempDir()
			put(t, repo, ".claude/skills/probe/SKILL.md", "---\nname: probe\ndescription: "+description+"\n---\nsynthetic body\n")
			if got, err := Build(context.Background(), repo, scratch); err == nil {
				t.Fatalf("empty whitespace-only description block published %q", got.Plugin)
			}
		})
	}
}

func TestSnapshotPreservesTextBlockWithLeadingBlankLines(t *testing.T) {
	repo, scratch := t.TempDir(), t.TempDir()
	body := "---\nname: probe\ndescription: |\n  \n  # Literal block text\n  More text\n---\nNative fixture body\n"
	put(t, repo, ".claude/skills/probe/SKILL.md", body)
	got, err := Build(context.Background(), repo, scratch)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(got.Plugin, "skills/probe/SKILL.md"))
	if err != nil || string(copied) != body {
		t.Fatalf("accepted native block changed: %q %v", copied, err)
	}
}
