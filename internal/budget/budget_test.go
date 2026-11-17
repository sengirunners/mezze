package budget

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// TestAgentSurface: AGENTS.md snippet + MCP tool names and descriptions +
// the longest notice line must fit in Bytes. Lesson: session-start size
// (41.9 KB in one retrospective) is paid by every agent on every start.
func TestAgentSurface(t *testing.T) {
	n := len(integrations.AgentsSnippet())
	for _, tool := range mcp.Tools() {
		n += len(tool.Name) + len(tool.Description)
	}
	n += orchapi.MaxNoticeLen
	if n > Bytes {
		t.Fatalf("agent-facing surface is %d bytes, budget %d: shorten the snippet or tool descriptions", n, Bytes)
	}
	t.Logf("agent-facing surface: %d of %d bytes", n, Bytes)
}

// TestContributorAgentsMD: the repo's AGENTS.md stays under its own budget.
func TestContributorAgentsMD(t *testing.T) {
	b, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > ContributorBytes {
		t.Fatalf("AGENTS.md is %d bytes, budget %d: compress it", len(b), ContributorBytes)
	}
	t.Logf("AGENTS.md: %d of %d bytes", len(b), ContributorBytes)
}

// TestScopedAgentsMD: every other AGENTS.md in the repo stays under its own
// budget, is listed in the root AGENTS.md (an unlisted one is never read),
// and CLAUDE.md is the one-line import of the root file.
func TestScopedAgentsMD(t *testing.T) {
	root, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	// The seams paragraph: from its first words to the next blank line.
	seams := string(root)
	if i := strings.Index(seams, "Seams have their own AGENTS.md"); i >= 0 {
		seams = seams[i:]
		if j := strings.Index(seams, "\n\n"); j >= 0 {
			seams = seams[:j]
		}
	} else {
		t.Fatal(`the root AGENTS.md has no "Seams have their own AGENTS.md" paragraph listing the scoped files`)
	}
	var found int
	err = filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "Vendor", ".build", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "AGENTS.md" || p == filepath.Join("../..", "AGENTS.md") {
			return nil
		}
		found++
		dir := filepath.ToSlash(strings.TrimPrefix(filepath.Dir(p), "../../"))
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n := strings.Count(string(b), "\n")
		if len(b) > ScopedBytes || n > ScopedLines {
			t.Errorf("%s/AGENTS.md is %d bytes and %d lines, budget %d and %d: compress it", dir, len(b), n, ScopedBytes, ScopedLines)
		}
		if !strings.Contains(seams, "`"+dir+"`") {
			t.Errorf("%s/AGENTS.md is not listed in the root AGENTS.md (add `%s` to its seams line)", dir, dir)
		}
		t.Logf("%s/AGENTS.md: %d bytes, %d lines", dir, len(b), n)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no scoped AGENTS.md found: is the walk rooted at the repo?")
	}
	c, err := os.ReadFile("../../CLAUDE.md")
	if err != nil || strings.TrimSpace(string(c)) != "@AGENTS.md" {
		t.Errorf("CLAUDE.md must be the single line @AGENTS.md (got %q, %v)", c, err)
	}
}
