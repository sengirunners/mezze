package taskcontext

import (
	"context"
	"testing"
)

func TestIndependentRejectsOverindentedLeadingBlankBlock(t *testing.T) {
	repo, scratch := t.TempDir(), t.TempDir()
	// YAML1.2.2 8.1.1.1: a leading empty line cannot be more indented
	// than the first nonempty block content line. This was incorrectly
	// added as a positive fixture in 9f0c7ed0.
	put(t, repo, ".claude/skills/probe/SKILL.md", "---\nname: probe\ndescription: |\n    \n  # Literal block text\n  More text\n---\nsynthetic body\n")
	if got, err := Build(context.Background(), repo, scratch); err == nil {
		t.Fatalf("invalid YAML leading blank indentation published %q", got.Plugin)
	}
}
