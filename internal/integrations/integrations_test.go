package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

func opts(t *testing.T, agent string) Options {
	t.Helper()
	return Options{Agent: agent, Home: t.TempDir(), PeoplesCommune: "/opt/zm/bin/zm",
		Now: func() time.Time { return time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC) }}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustInstall(t *testing.T, o Options) Result {
	t.Helper()
	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClaudeDryRunWritesNothing(t *testing.T) {
	o := opts(t, "claude")
	r := mustInstall(t, o)
	if !r.DryRun || !r.Changed() {
		t.Fatalf("want a dry run with changes: %+v", r)
	}
	for _, f := range r.Files {
		if f.Written || f.Action != ActionCreate {
			t.Errorf("%s: %+v", f.Path, f)
		}
		if _, err := os.Stat(f.Path); err == nil {
			t.Errorf("dry run wrote %s", f.Path)
		}
	}
	entries, _ := os.ReadDir(o.Home)
	if len(entries) != 0 {
		t.Errorf("dry run created %v", entries)
	}
}

func TestClaudeInstallMergesBacksUpAndIsIdempotent(t *testing.T) {
	o := opts(t, "claude")
	settings := filepath.Join(o.Home, ".claude", "settings.json")
	state := filepath.Join(o.Home, ".claude.json")
	write(t, settings, `{
  "model": "opus",
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "say done"}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/old/zm hook claude prompt"}]}]
  }
}
`)
	write(t, state, `{"numStartups": 3, "mcpServers": {"other": {"command": "x"}}, "zeta": true}`)
	o.Apply = true
	r := mustInstall(t, o)
	for _, f := range r.Files {
		if f.Action != ActionUpdate || !f.Written || f.Backup == "" {
			t.Fatalf("%s: %+v", f.Path, f)
		}
		if _, err := os.Stat(f.Backup); err != nil {
			t.Errorf("no backup: %v", err)
		}
	}

	var s struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(read(t, settings)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Model != "opus" {
		t.Errorf("lost model: %q", s.Model)
	}
	cmds := hookCommands(t, read(t, settings))
	if got := cmds["Stop"]; len(got) != 2 || got[0] != "say done" || got[1] != "/opt/zm/bin/zm hook claude stop" {
		t.Errorf("Stop = %v", got)
	}
	if got := cmds["UserPromptSubmit"]; len(got) != 1 || got[0] != "/opt/zm/bin/zm hook claude prompt" {
		t.Errorf("UserPromptSubmit = %v (old zm hook must be replaced)", got)
	}
	for _, ev := range []string{"SessionStart", "Notification"} {
		if len(cmds[ev]) != 1 {
			t.Errorf("%s = %v", ev, cmds[ev])
		}
	}
	if got := cmds["PreToolUse"]; len(got) != 1 || got[0] != "/opt/zm/bin/zm hook claude tool" {
		t.Errorf("PreToolUse = %v", got)
	}
	// PreToolUse runs before every tool call: a short timeout.
	var hs struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(read(t, settings)), &hs); err != nil {
		t.Fatal(err)
	}
	if g := hs.Hooks["PreToolUse"]; len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Timeout != 5 {
		t.Errorf("PreToolUse timeout: %+v", g)
	}
	if g := hs.Hooks["SessionStart"]; len(g) != 1 || g[0].Hooks[0].Timeout != 10 {
		t.Errorf("SessionStart timeout: %+v", g)
	}

	st := read(t, state)
	if strings.Index(st, "numStartups") > strings.Index(st, "zeta") {
		t.Errorf("key order not kept:\n%s", st)
	}
	var cj struct {
		MCP map[string]struct {
			Type, Command string
			Args          []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(st), &cj); err != nil {
		t.Fatal(err)
	}
	if b := cj.MCP["zm"]; b.Command != "/opt/zm/bin/zm" || b.Type != "stdio" || len(b.Args) != 1 || b.Args[0] != "mcp" {
		t.Errorf("mcp entry = %+v", b)
	}
	if cj.MCP["other"].Command != "x" {
		t.Errorf("lost other server")
	}

	// Second run: nothing to do, nothing written, no new backups.
	before := countBackups(t, o.Home)
	r2 := mustInstall(t, o)
	if r2.Changed() {
		t.Fatalf("second install changed: %+v", r2.Files)
	}
	if countBackups(t, o.Home) != before {
		t.Errorf("second install made backups")
	}

	// Uninstall restores everything zm did not own.
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	cmds = hookCommands(t, read(t, settings))
	if len(cmds["Stop"]) != 1 || cmds["Stop"][0] != "say done" || len(cmds["UserPromptSubmit"]) != 0 || len(cmds["SessionStart"]) != 0 {
		t.Errorf("after uninstall: %v", cmds)
	}
	if strings.Contains(read(t, state), "zm") {
		t.Errorf("uninstall left zm in %s", state)
	}
}

func hookCommands(t *testing.T, settings string) map[string][]string {
	t.Helper()
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[ev] = append(out[ev], h.Command)
			}
		}
	}
	return out
}

func countBackups(t *testing.T, root string) int {
	n := 0
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		if strings.Contains(filepath.Base(p), ".zm-backup-") {
			n++
		}
		return nil
	})
	return n
}

func TestClaudeConfigDir(t *testing.T) {
	o := opts(t, "claude")
	o.ConfigDir = filepath.Join(o.Home, "alt")
	r := mustInstall(t, o)
	if r.Files[0].Path != filepath.Join(o.ConfigDir, "settings.json") || r.Files[1].Path != filepath.Join(o.ConfigDir, ".claude.json") {
		t.Errorf("config dir paths: %s, %s", r.Files[0].Path, r.Files[1].Path)
	}
}

func TestForeignMCPEntryLeftAlone(t *testing.T) {
	o := opts(t, "claude")
	state := filepath.Join(o.Home, ".claude.json")
	orig := `{"mcpServers": {"zm": {"command": "npx", "args": ["something-else"]}}}`
	write(t, state, orig)
	r := mustInstall(t, o)
	if r.Files[1].Action != ActionUnchanged || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "left it alone") {
		t.Errorf("%+v %v", r.Files[1], r.Notes)
	}
}

func TestCodexInstall(t *testing.T) {
	o := opts(t, "codex")
	cfg := filepath.Join(o.Home, ".codex", "config.toml")
	orig := "model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"
	write(t, cfg, orig)
	o.Apply = true
	r := mustInstall(t, o)
	got := read(t, cfg)
	if !strings.HasPrefix(got, "# >>> zm notify") {
		t.Errorf("notify block must come first (top-level key):\n%s", got)
	}
	for _, s := range []string{`notify = ["/opt/zm/bin/zm", "hook", "codex", "notify"]`, "[mcp_servers.zm]", `args = ["mcp"]`, `env_vars = ["ZM_HOME", "ZM_AS", "ZM_SESSION"`, "[mcp_servers.other]", `model = "gpt-5"`} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if r.Files[0].Backup == "" {
		t.Errorf("no backup: %+v", r.Files[0])
	}
	cmds := hookCommands(t, read(t, filepath.Join(o.Home, ".codex", "hooks.json")))
	if len(cmds["SessionStart"]) != 1 || len(cmds["UserPromptSubmit"]) != 1 {
		t.Errorf("codex hooks: %v", cmds)
	}
	if r2 := mustInstall(t, o); r2.Changed() {
		t.Errorf("second install changed: %+v", r2.Files)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, cfg); got != orig {
		t.Errorf("uninstall did not restore config.toml:\n%q\nwant\n%q", got, orig)
	}
}

func TestCodexKeepsUserNotify(t *testing.T) {
	o := opts(t, "codex")
	cfg := filepath.Join(o.Home, ".codex", "config.toml")
	write(t, cfg, "notify = [\"my-notifier\"]\n")
	o.Apply = true
	r := mustInstall(t, o)
	got := read(t, cfg)
	if strings.Contains(got, "# >>> zm notify") || !strings.Contains(got, "my-notifier") {
		t.Errorf("user notify must win:\n%s", got)
	}
	if len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "already sets notify") {
		t.Errorf("notes: %v", r.Notes)
	}
	if !strings.Contains(got, "[mcp_servers.zm]") {
		t.Errorf("mcp block still expected:\n%s", got)
	}
}

func TestUnknownAgent(t *testing.T) {
	if _, err := Install(Options{Agent: "nope", Home: t.TempDir()}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("err = %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/zm": "/usr/local/bin/zm",
		"/Users/a b/zm":     "'/Users/a b/zm'",
		"/x/it's/zm":        `'/x/it'\''s/zm'`,
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if !isZmHook(HookCommand("/Users/a b/zm", "claude", "stop"), "claude") {
		t.Error("quoted path not recognised")
	}
	if isZmHook("zm hook codex stop", "claude") || isZmHook("zmfett hook claude stop", "claude") {
		t.Error("false positive")
	}
}

// Config files linked from a dotfiles repo must be edited at their target and
// the link kept (a rename onto the link path would replace the link).
func TestInstallThroughSymlinkKeepsLink(t *testing.T) {
	o := opts(t, "claude")
	dot := filepath.Join(o.Home, "dotfiles", "claude", "settings.json")
	write(t, dot, "{\n  \"model\": \"opus\"\n}\n")
	link := filepath.Join(o.Home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dotfiles/claude/settings.json", link); err != nil { // relative, as dotfile managers do
		t.Fatal(err)
	}
	// A dangling link (target not created yet) also gets its target written.
	codexOpts := o
	codexOpts.Agent = "codex"
	cdot := filepath.Join(o.Home, "dotfiles", "codex", "config.toml")
	clink := filepath.Join(o.Home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(clink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cdot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cdot, clink); err != nil {
		t.Fatal(err)
	}

	o.Apply, codexOpts.Apply = true, true
	r := mustInstall(t, o)
	var got *FileChange
	for i := range r.Files {
		if strings.HasSuffix(r.Files[i].Path, "settings.json") {
			got = &r.Files[i]
		}
	}
	if got == nil || got.Path != dot || !got.Written {
		t.Fatalf("settings change = %+v, want written at %s", got, dot)
	}
	if filepath.Dir(got.Backup) != filepath.Dir(dot) {
		t.Errorf("backup %s not beside target %s", got.Backup, dot)
	}
	if b := read(t, got.Backup); b != "{\n  \"model\": \"opus\"\n}\n" {
		t.Errorf("backup = %q", b)
	}
	cr := mustInstall(t, codexOpts)

	for _, l := range []string{link, clink} {
		st, err := os.Lstat(l)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer a symlink (err=%v)", l, err)
		}
	}
	if cmds := hookCommands(t, read(t, dot)); len(cmds["Stop"]) != 1 {
		t.Errorf("target not updated:\n%s", read(t, dot))
	}
	if !strings.Contains(read(t, cdot), "zm") {
		t.Errorf("dangling target not written:\n%s", read(t, cdot))
	}
	for _, f := range cr.Files {
		if strings.HasSuffix(f.Path, "config.toml") && (f.Path != cdot || f.Action != ActionCreate) {
			t.Errorf("codex change = %+v, want create at %s", f, cdot)
		}
	}
	// Running again is a no-op through the link.
	for _, f := range mustInstall(t, o).Files {
		if f.Action != ActionUnchanged {
			t.Errorf("second install: %s %s", f.Path, f.Action)
		}
	}
}

func TestResolveLinkLoop(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLink(a); err == nil {
		t.Error("symlink loop not reported")
	}
}
