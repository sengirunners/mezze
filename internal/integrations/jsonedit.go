package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// object is a JSON object that keeps its key order and leaves the values it
// does not touch byte-for-byte as they were (modulo indentation).
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// parseObject parses b; empty or blank input is an empty object.
func parseObject(b []byte) (*object, error) {
	o := newObject()
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) { v, ok := o.vals[k]; return v, ok }

func (o *object) set(k string, v any) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		raw = mustMarshal(v)
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = raw
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// child returns the object at key k (a new empty one if absent).
func (o *object) child(k string) (*object, error) {
	v, ok := o.get(k)
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return newObject(), nil
	}
	c, err := parseObject(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	return c, nil
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(mustMarshal(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// render formats o as a config file: 2-space indent, trailing newline, no HTML escaping.
func (o *object) render() []byte {
	var out bytes.Buffer
	_ = json.Indent(&out, mustMarshal(o), "", "  ")
	out.WriteByte('\n')
	return out.Bytes()
}

func mustMarshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// sameJSON reports whether a and b hold equal JSON values.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// ---- hooks (Claude Code settings.json and Codex hooks.json share this shape) ----
//
//	{"hooks": {"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "...", "timeout": 10}]}]}}

// hookTimeout is the timeout (seconds) on every hook zm installs, except
// those in hookTimeouts.
const hookTimeout = 10

// hookTimeouts are the hooks with a shorter timeout: PreToolUse runs before
// every tool call, and a hook that cannot answer must never hold one.
var hookTimeouts = map[string]int{"PreToolUse": 5}

func timeoutFor(event string) int {
	if t, ok := hookTimeouts[event]; ok {
		return t
	}
	return hookTimeout
}

// isZmHook reports whether a hook command is one zm install wrote for agent.
func isZmHook(command, agent string) bool {
	re := regexp.MustCompile(`(^|[\s/'"])zm['"]?\s+hook\s+` + regexp.QuoteMeta(agent) + `(\s|$)`)
	return re.MatchString(command)
}

type hookEntry struct {
	Command string `json:"command"`
}

// editHooks rewrites hooks[event]: zm hooks other than one exact match of
// want are dropped (with their group when it empties); want is appended in a
// new group if no exact match was kept. want == "" removes them all.
func editHooks(hooks *object, agent, event, want string) (string, error) {
	var groups []json.RawMessage
	if v, ok := hooks.get(event); ok {
		if err := json.Unmarshal(v, &groups); err != nil {
			return "", fmt.Errorf("hooks.%s: %w", event, err)
		}
	}
	var out []json.RawMessage
	var removed []string
	kept := false
	for _, g := range groups {
		gobj, err := parseObject(g)
		if err != nil {
			return "", fmt.Errorf("hooks.%s: %w", event, err)
		}
		var inner []json.RawMessage
		if v, ok := gobj.get("hooks"); ok {
			if err := json.Unmarshal(v, &inner); err != nil {
				return "", fmt.Errorf("hooks.%s: %w", event, err)
			}
		}
		var keep []json.RawMessage
		touched := false
		for _, h := range inner {
			var e hookEntry
			_ = json.Unmarshal(h, &e)
			if !isZmHook(e.Command, agent) {
				keep = append(keep, h)
				continue
			}
			if want != "" && !kept && e.Command == want {
				kept = true
				keep = append(keep, h)
				continue
			}
			touched = true
			removed = append(removed, e.Command)
		}
		switch {
		case !touched:
			out = append(out, g)
		case len(keep) > 0:
			gobj.set("hooks", keep)
			out = append(out, mustMarshal(gobj))
		}
	}
	var changes []string
	for _, r := range removed {
		changes = append(changes, fmt.Sprintf("- hooks.%s: %s", event, r))
	}
	if want != "" && !kept {
		g := newObject()
		h := newObject()
		h.set("type", "command")
		h.set("command", want)
		h.set("timeout", timeoutFor(event))
		g.set("hooks", []json.RawMessage{mustMarshal(h)})
		out = append(out, mustMarshal(g))
		changes = append(changes, fmt.Sprintf("+ hooks.%s: %s", event, want))
	}
	if len(changes) == 0 {
		return "", nil
	}
	if len(out) == 0 {
		hooks.del(event)
	} else {
		hooks.set(event, out)
	}
	return strings.Join(changes, "\n"), nil
}

// editHookFile applies want (event -> command; "" command removes) to a hooks
// file's content and returns the new content and change lines.
func editHookFile(before []byte, agent string, events []string, want map[string]string) ([]byte, []string, error) {
	root, err := parseObject(before)
	if err != nil {
		return nil, nil, err
	}
	hooks, err := root.child("hooks")
	if err != nil {
		return nil, nil, err
	}
	var changes []string
	for _, ev := range events {
		c, err := editHooks(hooks, agent, ev, want[ev])
		if err != nil {
			return nil, nil, err
		}
		if c != "" {
			changes = append(changes, strings.Split(c, "\n")...)
		}
	}
	if len(changes) == 0 {
		return before, nil, nil
	}
	if len(hooks.keys) == 0 {
		root.del("hooks")
	} else {
		root.set("hooks", hooks)
	}
	return root.render(), changes, nil
}

// ---- MCP server entries ({"mcpServers": {"zm": {...}}}) ----

// mcpEntry is the server entry zm registers.
func mcpEntry(zm string, withType bool) json.RawMessage {
	e := newObject()
	if withType {
		e.set("type", "stdio")
	}
	e.set("command", zm)
	e.set("args", []string{"mcp"})
	return mustMarshal(e)
}

// isZmMCP reports whether an existing entry runs zm's MCP server.
func isZmMCP(raw json.RawMessage) bool {
	var e struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	return isZmCommand(e.Command) && len(e.Args) > 0 && e.Args[0] == "mcp"
}

func isZmCommand(cmd string) bool {
	cmd = strings.TrimRight(cmd, "/")
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		cmd = cmd[i+1:]
	}
	return cmd == "zm"
}

// editMCPFile sets (or, entry == nil, removes) mcpServers.zm. An entry
// named zm that runs something else is left alone and reported in note.
func editMCPFile(before []byte, entry json.RawMessage) (after []byte, changes []string, note string, err error) {
	root, err := parseObject(before)
	if err != nil {
		return nil, nil, "", err
	}
	servers, err := root.child("mcpServers")
	if err != nil {
		return nil, nil, "", err
	}
	cur, has := servers.get("zm")
	switch {
	case has && !isZmMCP(cur):
		return before, nil, "mcpServers.zm exists and does not run zm mcp; left it alone", nil
	case entry == nil && !has:
		return before, nil, "", nil
	case entry == nil:
		servers.del("zm")
		changes = []string{"- mcpServers.zm: " + compact(cur)}
	case has && sameJSON(cur, entry):
		return before, nil, "", nil
	default:
		if has {
			changes = append(changes, "- mcpServers.zm: "+compact(cur))
		}
		servers.set("zm", entry)
		changes = append(changes, "+ mcpServers.zm: "+compact(entry))
	}
	if len(servers.keys) == 0 {
		root.del("mcpServers")
	} else {
		root.set("mcpServers", servers)
	}
	return root.render(), changes, "", nil
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}
