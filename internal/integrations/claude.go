package integrations

import (
	"path/filepath"
	"regexp"
	"strings"
)

// claude installs into Claude Code ($CLAUDE_CONFIG_DIR, default ~/.claude):
//   - hooks in settings.json: SessionStart, UserPromptSubmit, PreToolUse,
//     Notification, Stop, SessionEnd, SubagentStart, SubagentStop ->
//     `zm hook claude <event>`
//   - the MCP server (user scope) in ~/.claude.json, or
//     $CLAUDE_CONFIG_DIR/.claude.json when the config dir is moved.
type claude struct{}

func (claude) Name() string { return "claude" }

// claudeHooks maps Claude Code hook events to `zm hook claude <arg>`.
var claudeHooks = []struct{ Event, Arg string }{
	{"SessionStart", HookSessionStart},
	{"UserPromptSubmit", HookPrompt},
	{"PreToolUse", HookTool},
	{"Notification", HookNotification},
	{"Stop", HookStop},
	{"SessionEnd", HookSessionEnd},
	{"SubagentStart", HookSubagentStart},
	{"SubagentStop", HookSubagentStop},
}

// ClaudeHooksJSON is the hooks file content for command zm (the plugin's
// hooks/hooks.json is this with zm = "zm"; a test keeps them in sync).
func ClaudeHooksJSON(zm string) []byte {
	b, _, _ := editHookFile(nil, "claude", claudeEvents(), claudeWant(zm))
	return b
}

func claudeEvents() []string {
	var evs []string
	for _, h := range claudeHooks {
		evs = append(evs, h.Event)
	}
	return evs
}

func claudeWant(zm string) map[string]string {
	want := map[string]string{}
	for _, h := range claudeHooks {
		want[h.Event] = HookCommand(zm, "claude", h.Arg)
	}
	return want
}

func (claude) Plan(o Options, uninstall bool) ([]FileChange, []string, []string, error) {
	var settings, mcpFile string
	if o.ConfigDir != "" {
		settings = filepath.Join(o.ConfigDir, "settings.json")
		mcpFile = filepath.Join(o.ConfigDir, ".claude.json")
	} else {
		settings = filepath.Join(o.Home, ".claude", "settings.json")
		mcpFile = filepath.Join(o.Home, ".claude.json")
	}

	want := claudeWant(o.PeoplesCommune)
	if uninstall {
		want = map[string]string{}
	}
	hf, err := planHookFile(settings, "claude", claudeEvents(), want)
	if err != nil {
		return nil, nil, nil, err
	}

	var entry []byte
	if !uninstall {
		entry = mcpEntry(o.PeoplesCommune, true)
	}
	mf, note, err := planMCPFile(mcpFile, entry)
	if err != nil {
		return nil, nil, nil, err
	}
	var notes, next []string
	if note != "" {
		notes = append(notes, note)
	}
	if !uninstall {
		next = append(next,
			"Start a new Claude Code session: hooks and MCP servers load at startup.",
			"Claude Code rewrites "+mcpFile+" while it runs; if mcpServers.zm goes missing, run this again with Claude Code closed.",
			"Do not also enable the zm plugin (integrations/claude-code): every hook would run twice.")
	}
	return []FileChange{hf, mf}, notes, next, nil
}

func planHookFile(path, agent string, events []string, want map[string]string) (FileChange, error) {
	before, err := readFile(path)
	if err != nil {
		return FileChange{}, err
	}
	after, changes, err := editHookFile(before, agent, events, want)
	if err != nil {
		return FileChange{}, wrapPath(path, err)
	}
	return FileChange{Path: path, Changes: changes, before: before, after: after}, nil
}

func planMCPFile(path string, entry []byte) (FileChange, string, error) {
	before, err := readFile(path)
	if err != nil {
		return FileChange{}, "", err
	}
	after, changes, note, err := editMCPFile(before, entry)
	if err != nil {
		return FileChange{}, "", wrapPath(path, err)
	}
	if note != "" {
		note = path + ": " + note
	}
	return FileChange{Path: path, Changes: changes, before: before, after: after}, note, nil
}

// HookCommand is the shell command a hook runs: `<zm> hook <agent> <event>`.
func HookCommand(zm, agent, event string) string {
	return ShellQuote(zm) + " hook " + agent + " " + event
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote quotes s for a POSIX shell when it needs it.
func ShellQuote(s string) string {
	if s != "" && shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
