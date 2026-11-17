package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plugin folder and the snippet file ship in the repo; these tests keep
// them identical to what the code generates.

func TestPluginHooksMatchInstall(t *testing.T) {
	want, err := os.ReadFile("../../integrations/claude-code/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := ClaudeHooksJSON("zm"); string(got) != string(want) {
		t.Errorf("integrations/claude-code/hooks/hooks.json is stale; want:\n%s", got)
	}
}

func TestPluginMCPMatchesInstall(t *testing.T) {
	b, err := os.ReadFile("../../integrations/claude-code/.mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCP map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if !isZmMCP(f.MCP["zm"]) {
		t.Errorf("plugin .mcp.json does not run zm mcp: %s", b)
	}
	// The compute plugin's sandbox skill: Claude Code and Codex need name and description frontmatter.
	sk, err := os.ReadFile("../../integrations/claude-code-compute/skills/sandboxes/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(sk), "---\nname: sandboxes\ndescription: ") || len(sk) > 2048 {
		t.Errorf("skills/sandboxes/SKILL.md: want name/description frontmatter and at most 2 KiB, got %d bytes", len(sk))
	}
	for _, p := range []string{"../../integrations/claude-code/.claude-plugin/plugin.json", "../../integrations/.claude-plugin/marketplace.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil || v["name"] != "zm" {
			t.Errorf("%s: name=%v err=%v", p, v["name"], err)
		}
	}
}

func TestSnippetFileAndBudget(t *testing.T) {
	b, err := os.ReadFile("../../integrations/AGENTS.md.snippet")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != AgentsSnippet() {
		t.Errorf("integrations/AGENTS.md.snippet differs from AgentsSnippet(); want:\n%s", AgentsSnippet())
	}
	if n := len(AgentsSnippet()); n > SnippetMaxBytes {
		t.Errorf("snippet is %d bytes, max %d", n, SnippetMaxBytes)
	}
	t.Logf("snippet: %d of %d bytes", len(AgentsSnippet()), SnippetMaxBytes)
	for _, must := range []string{"never authority", "zm inbox", "zm work claim", "zm work comment"} {
		if !strings.Contains(AgentsSnippet(), must) {
			t.Errorf("snippet lacks %q", must)
		}
	}
}

func TestDryRunShowsExactDiff(t *testing.T) {
	o := opts(t, "claude")
	settings := filepath.Join(o.Home, ".claude", "settings.json")
	write(t, settings, "{\n  \"model\": \"opus\"\n}\n")
	r := mustInstall(t, o)
	d := r.Files[0].Diff
	if !strings.HasPrefix(d, "--- a"+settings+"\n+++ b"+settings+"\n@@ -1,3 +1,") {
		t.Errorf("diff header:\n%s", d)
	}
	if !strings.Contains(d, "\n   \"model\": \"opus\",\n") && !strings.Contains(d, "\n-  \"model\": \"opus\"\n") {
		t.Errorf("diff body:\n%s", d)
	}
	if !strings.Contains(d, "+            \"command\": \"/opt/zm/bin/zm hook claude stop\",") {
		t.Errorf("diff lacks the stop hook:\n%s", d)
	}
	if !strings.HasPrefix(r.Files[1].Diff, "--- /dev/null\n") {
		t.Errorf("new file diff:\n%s", r.Files[1].Diff)
	}
	if read(t, settings) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Error("dry run changed the file")
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n")
	b := []byte("1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11\n12\ntwelve-and-a-half\n13\n")
	want := "--- a/f\n+++ b/f\n" +
		"@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n" +
		"@@ -10,4 +10,5 @@\n 10\n 11\n 12\n+twelve-and-a-half\n 13\n"
	if got := unifiedDiff("/f", a, b); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if unifiedDiff("/f", a, a) != "" {
		t.Error("equal files must give no diff")
	}
}

func TestHomeRequired(t *testing.T) {
	if _, err := Install(Options{Agent: "claude"}); err == nil {
		t.Error("install with no home must fail, never guess")
	}
}
