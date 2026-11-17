package integrations

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// ActivityThinking is the activity the prompt hook reports: never the
// prompt itself.
const ActivityThinking = "Thinking"

// ToolActivity is what a session is doing, from the tool its agent is about
// to run (Claude Code's PreToolUse tool_name and tool_input). Privacy first:
// the text goes to every reader of the session, teammates included, so it
// never holds a command, a prompt, a search pattern or file contents. Bash
// gives only the description Claude writes for it; file tools only the
// file's base name; web tools only the host.
func ToolActivity(tool string, input json.RawMessage) string {
	var in struct {
		Description  string `json:"description"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		URL          string `json:"url"`
		Todos        []struct {
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	_ = json.Unmarshal(input, &in)
	file := func(verb, p string) string {
		if b := baseName(p); b != "" {
			return verb + " " + b
		}
		return verb + " a file"
	}
	switch tool {
	case "Bash":
		if d := cleanActivity(in.Description); d != "" {
			return d
		}
		return "Running a command"
	case "Edit", "MultiEdit":
		return file("Editing", in.FilePath)
	case "NotebookEdit":
		return file("Editing", in.NotebookPath)
	case "Write":
		return file("Writing", in.FilePath)
	case "Read":
		return file("Reading", in.FilePath)
	case "Grep", "Glob":
		return "Searching"
	case "Task", "Agent":
		if d := cleanActivity(in.Description); d != "" {
			return d
		}
	case "WebFetch":
		if u, err := url.Parse(strings.TrimSpace(in.URL)); err == nil && u.Hostname() != "" {
			return "Web: " + u.Hostname()
		}
		return "Searching the web"
	case "WebSearch":
		return "Searching the web"
	case "TodoWrite":
		for _, t := range in.Todos {
			if t.Status == "in_progress" {
				if a := cleanActivity(t.ActiveForm); a != "" {
					return a
				}
			}
		}
	}
	return cleanActivity(tool)
}

// baseName is a path's last element, "" for none.
func baseName(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"), "/")
	if p == "" {
		return ""
	}
	return cleanActivity(path.Base(p))
}

// cleanActivity keeps one line of s without control characters, at most
// api.MaxActivityLen characters (the daemon clips again).
func cleanActivity(s string) string {
	s, _, _ = strings.Cut(strings.ReplaceAll(s, "\r", "\n"), "\n")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s))
	if rs := []rune(s); len(rs) > orchapi.MaxActivityLen {
		s = strings.TrimSpace(string(rs[:orchapi.MaxActivityLen-1])) + "…"
	}
	return s
}
