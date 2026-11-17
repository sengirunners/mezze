package taskcontext

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentRejectsInvalidOrNullDescription(t *testing.T) {
	for _, description := range []string{"# only a YAML comment", "@reserved-indicator", "`reserved-indicator`"} {
		t.Run(description, func(t *testing.T) {
			repo, scratch := t.TempDir(), t.TempDir()
			put(t, repo, ".claude/skills/probe/SKILL.md", "---\nname: probe\ndescription: "+description+"\n---\nsynthetic body\n")
			if got, err := Build(context.Background(), repo, scratch); err == nil {
				t.Fatalf("unsupported/null YAML description silently accepted and published plugin %q", got.Plugin)
			}
		})
	}
}

func TestSnapshotRejectsAmbiguousScalarText(t *testing.T) {
	values := []string{"%directive", ",flow", "?mapping", ":mapping", "text:", "Null", "NULL", "TRUE", "False", "YES", "Off", "n", "text\x00hidden", "text\x01hidden", "text\thidden", "text\rhidden", "text\x7fhidden", "text\u0085hidden", "text\u2028hidden", "text\u2029hidden", "text\ufeffhidden", "text\ufffehidden", "text\uffffhidden", "|\n    # literal content\n  second line", "\"text\\u0000hidden\"", "'text\x02hidden'"}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			repo, scratch := t.TempDir(), t.TempDir()
			put(t, repo, ".claude/skills/probe/SKILL.md", "---\nname: probe\ndescription: "+value+"\n---\nbody\n")
			if _, err := Build(context.Background(), repo, scratch); err == nil {
				t.Fatalf("ambiguous scalar accepted: %q", value)
			}
		})
	}
}

func TestSnapshotScalarBytesPreserved(t *testing.T) {
	for _, value := range []string{`"# literal text"`, "'@literal text'", "'`literal text`'", `"TRUE"`, "'Null'", "'yes'", "https://example.invalid/context", "Read Bash(git *)", "|-\n  # literal block text\n  next line"} {
		t.Run(value, func(t *testing.T) {
			repo, scratch := t.TempDir(), t.TempDir()
			original := "---\nname: probe\ndescription: " + value + "\n---\nbody remains byte-identical\n"
			put(t, repo, ".claude/skills/probe/SKILL.md", original)
			result, err := Build(context.Background(), repo, scratch)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(filepath.Join(result.Plugin, "skills/probe/SKILL.md"))
			if err != nil || string(actual) != original {
				t.Fatal("accepted source changed")
			}
		})
	}
	if validateName([]byte("---\nname: probe\ndescription:missing-space\n---\nbody"), "probe") == nil {
		t.Fatal("missing YAML mapping separator accepted")
	}
}

func TestSnapshotRejectsHeaderControls(t *testing.T) {
	for r := rune(0); r <= 0x9f; r++ {
		if r > 0x1f && r < 0x7f {
			continue
		}
		for _, value := range []string{"plain" + string(r) + "text", "'quoted" + string(r) + "text'", "|\n  block" + string(r) + "text"} {
			if validateName([]byte("---\nname: probe\ndescription: "+value+"\n---\nbody"), "probe") == nil {
				t.Fatalf("header control U+%04X accepted in %q", r, value)
			}
		}
	}
}
