package integrations

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// codex installs into Codex ($CODEX_HOME, default ~/.codex):
//   - config.toml: notify = [zm, "hook", "codex", "notify"] (turn finished -> idle)
//     and [mcp_servers.zm], each inside a marked block zm owns
//   - hooks.json: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse,
//     SessionEnd, SubagentStop -> `zm hook codex <event>` (tool use keeps a long turn
//     running, so its bound leases renew: SPEC 10)
type codex struct{}

func (codex) Name() string { return "codex" }

var codexHooks = []struct{ Event, Arg string }{
	{"SessionStart", HookSessionStart},
	{"UserPromptSubmit", HookPrompt},
	{"PreToolUse", HookTool},
	{"PostToolUse", HookTool},
	{"SessionEnd", HookSessionEnd},
	// SubagentStop ends a subagent thread's turn, not the thread (codex-rs:
	// root turns run Stop, a thread's turns SubagentStop; the thread takes
	// more): idle, like a turn's end, for that thread's session (agent_id).
	// Codex sends no SessionEnd for a subagent thread: its session goes idle
	// by the no-hooks rule and its claims lapse one bound TTL later.
	{"SubagentStop", HookStop},
}

// CodexHookArg is the `zm hook codex <arg>` that zm install codex runs for
// Codex hook event ("" when it installs none).
func CodexHookArg(event string) string {
	for _, h := range codexHooks {
		if h.Event == event {
			return h.Arg
		}
	}
	return ""
}

func (codex) Plan(o Options, uninstall bool) ([]FileChange, []string, []string, error) {
	dir := codexDir(o)

	var events []string
	want := map[string]string{}
	for _, h := range codexHooks {
		events = append(events, h.Event)
		if !uninstall {
			want[h.Event] = HookCommand(o.PeoplesCommune, "codex", h.Arg)
		}
	}
	hf, err := planHookFile(filepath.Join(dir, "hooks.json"), "codex", events, want)
	if err != nil {
		return nil, nil, nil, err
	}

	path := filepath.Join(dir, "config.toml")
	before, err := readFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	after, changes, notes := editCodexTOML(string(before), o.PeoplesCommune, uninstall)
	cf := FileChange{Path: path, Changes: changes, before: before}
	if before != nil || after != "" {
		cf.after = []byte(after)
	}
	var next []string
	if !uninstall {
		next = append(next, "open Codex and run /hooks to review and trust zm's hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd, SubagentStop). "+
			"Codex skips them until you do (zm never trusts hooks for you). Then start a new Codex session.")
	}
	return []FileChange{cf, hf}, notes, next, nil
}

const (
	tomlBegin = "# >>> zm %s: managed by `zm install codex`, removed by `zm uninstall codex` >>>"
	tomlEnd   = "# <<< zm %s <<<"
)

var (
	tomlTable   = regexp.MustCompile(`^\s*\[`)
	tomlNotify  = regexp.MustCompile(`^\s*notify\s*=`)
	tomlZmTable = regexp.MustCompile(`(?m)^\s*\[\s*mcp_servers\s*\.\s*("zm"|zm)\s*\]`)
)

// editCodexTOML rewrites config.toml text: zm's two blocks are removed and,
// unless uninstalling, re-added (notify at the top, where top-level keys must
// be; the MCP table at the end). Everything outside the blocks is kept.
func editCodexTOML(text, zm string, uninstall bool) (string, []string, []string) {
	base, hadNotify := stripTOMLBlock(text, "notify")
	base, hadMCP := stripTOMLBlock(base, "mcp")
	base = strings.Trim(base, "\n")

	var notes, changes []string
	var notify, mcp string
	if !uninstall {
		argv := []string{zm, "hook", "codex", HookNotify}
		notify = tomlBlock("notify", "notify = "+tomlArray(argv))
		if hasTopLevelNotify(base) {
			notes = append(notes, fmt.Sprintf("config.toml already sets notify; zm did not replace it. Codex runs one notify program: call %s from yours to report idle sessions.",
				strings.Join(append(quoteAll(argv), `"$1"`), " ")))
			notify = ""
		}
		mcp = tomlBlock("mcp", "[mcp_servers.zm]\ncommand = "+strconv.Quote(zm)+"\nargs = [\"mcp\"]\nenv_vars = [\"ZM_HOME\", \"ZM_AS\", \"ZM_SESSION\", \"ZM_MACHINE\", \"ZM_HUMAN\", \"ZM_TASK_DEPTH\", \"ZM_SCRATCH\", \"CODEX_THREAD_ID\"]")
		if tomlZmTable.MatchString(base) {
			notes = append(notes, "config.toml already has [mcp_servers.zm]; left it alone.")
			mcp = ""
		}
	}

	if !hadNotify && !hadMCP && notify == "" && mcp == "" {
		return text, nil, notes
	}
	var parts []string
	if notify != "" {
		parts = append(parts, notify)
	}
	if base != "" {
		parts = append(parts, base)
	}
	if mcp != "" {
		parts = append(parts, mcp)
	}
	out := ""
	if len(parts) > 0 {
		out = strings.Join(parts, "\n\n") + "\n"
	}
	if out == text {
		return out, nil, notes
	}
	switch {
	case notify != "" && !hadNotify:
		changes = append(changes, "+ notify = "+tomlArray([]string{zm, "hook", "codex", HookNotify}))
	case notify == "" && hadNotify:
		changes = append(changes, "- notify (zm block)")
	}
	switch {
	case mcp != "" && !hadMCP:
		changes = append(changes, "+ [mcp_servers.zm] command = "+strconv.Quote(zm)+`, args = ["mcp"]`)
	case mcp == "" && hadMCP:
		changes = append(changes, "- [mcp_servers.zm] (zm block)")
	}
	if len(changes) == 0 {
		changes = append(changes, "~ refresh zm blocks")
	}
	return out, changes, notes
}

func tomlBlock(name, body string) string {
	return fmt.Sprintf(tomlBegin, name) + "\n" + body + "\n" + fmt.Sprintf(tomlEnd, name)
}

// stripTOMLBlock removes the named zm block's lines.
func stripTOMLBlock(text, name string) (string, bool) {
	begin, end := fmt.Sprintf(tomlBegin, name), fmt.Sprintf(tomlEnd, name)
	lines := strings.SplitAfter(text, "\n")
	var out strings.Builder
	in, found := false, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case !in && t == begin:
			in, found = true, true
		case in && t == end:
			in = false
		case !in:
			out.WriteString(l)
		}
	}
	return out.String(), found
}

// hasTopLevelNotify reports a notify key before the first table header.
func hasTopLevelNotify(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		if tomlTable.MatchString(l) {
			return false
		}
		if tomlNotify.MatchString(l) {
			return true
		}
	}
	return false
}

func tomlArray(xs []string) string { return "[" + strings.Join(quoteAll(xs), ", ") + "]" }

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Quote(x)
	}
	return out
}
