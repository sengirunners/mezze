package conformance

import (
	"bytes"
	"encoding/json"
	"regexp"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

var email = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// dropKeys are runtime fields a committed recording leaves out: the
// machine's setup (tools, skills, plugins, MCP servers, sockets), accounts
// and install paths. No driver reads them.
var dropKeys = map[string]bool{
	"tools": true, "slash_commands": true, "terminal_slash_commands": true, "skills": true, "plugins": true,
	"plugin_errors": true, "agents": true, "mcp_servers": true, "memory_paths": true, "messaging_socket_path": true,
	"additional_directories": true, "account": true, "commands": true, "models": true, "codexHome": true,
	"userAgent": true, "path": true, "gitInfo": true, "instructionSources": true, "installationId": true, "serverName": true,
}

// dropMethods are runtime notifications a committed recording leaves out
// whole: they name the machine's MCP servers, host and account, and no
// driver reads them.
var dropMethods = map[string]bool{
	"mcpServer/startupStatus/updated": true, "remoteControl/status/changed": true, "account/updated": true,
	"account/rateLimits/updated": true, "skills/changed": true, "app/list/updated": true,
}

// Scrub returns entries with each from path replaced by its to (longest
// first: list cwd before home), emails replaced and dropKeys removed.
func Scrub(entries []agentpipe.RecEntry, repl [][2]string) []agentpipe.RecEntry {
	fix := func(b json.RawMessage) json.RawMessage {
		if len(b) == 0 {
			return b
		}
		for _, r := range repl {
			from, _ := json.Marshal(r[0])
			to, _ := json.Marshal(r[1])
			b = bytes.ReplaceAll(b, from[1:len(from)-1], to[1:len(to)-1])
		}
		b = email.ReplaceAll(b, []byte("user@example.com"))
		var v any
		if json.Unmarshal(b, &v) != nil {
			return b
		}
		v = drop(v)
		out, _ := json.Marshal(v)
		return out
	}
	out := make([]agentpipe.RecEntry, 0, len(entries))
	for _, e := range entries {
		var m struct {
			Method string `json:"method"`
			Type   string `json:"type"`
		}
		if e.T == agentpipe.RecOut && json.Unmarshal(e.Frame, &m) == nil && (dropMethods[m.Method] || m.Type == "rate_limit_event" && !bytes.Contains(e.Frame, []byte(`"rejected"`))) {
			continue
		}
		e.Op, e.Frame = fix(e.Op), fix(e.Frame)
		if e.Raw != "" {
			e.Raw = string(fix(json.RawMessage(e.Raw)))
		}
		out = append(out, e)
	}
	return out
}

func drop(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if dropKeys[k] {
				delete(x, k)
				continue
			}
			x[k] = drop(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = drop(e)
		}
		return x
	}
	return v
}

// WriteRecording is a recording file's bytes.
func WriteRecording(head agentpipe.RecEntry, entries []agentpipe.RecEntry) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(head)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
	return buf.Bytes()
}
