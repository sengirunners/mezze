package integrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Codex runs a hooks.json hook only after the user trusts it in Codex
// (/hooks). It records that in config.toml, keyed by the hook's place:
//
//	[hooks.state."/abs/real/path/hooks.json:session_start:0:0"]
//	trusted_hash = "sha256:..."
//	enabled = false            # only when turned off
//
// (key = the hooks.json real path, the event in snake case, the group index
// and the hook's index in it.) Source: Codex's own hooks/list, verified
// 2026-10-30 against codex-cli 0.166.0 by running `codex app-server` with a
// temp CODEX_HOME and calling the JSON-RPC method hooks/list: each hook comes
// back with "key": "<realpath>/hooks.json:session_start:0:0", "currentHash"
// and "trustStatus"; a [hooks.state."<key>"] table whose trusted_hash equals
// currentHash turns trustStatus "trusted" ("modified" for another hash),
// enabled = false turns "enabled" false, and trusted_content_hash alone does
// not count. (Codex's HookStateToml in codex-rs; not vendored here.) If a
// later Codex keys hooks differently, every hook reads untrusted: zm then
// hedges (HookTrust.Unverified) rather than claim the user has not trusted.
// zm never writes this state: trusting a hook is the user's decision.
// It only reads it to tell the user what is missing. Codex's hash is not
// recomputed here, so a trusted hook edited by hand later still reads as
// trusted; any change zm itself makes is reported as needing trust again.

// Codex hook trust states.
const (
	TrustTrusted   = "trusted"   // config.toml records a trusted hash for it
	TrustUntrusted = "untrusted" // never trusted: Codex skips it
	TrustDisabled  = "disabled"  // turned off in /hooks
	TrustChanged   = "changed"   // zm writes (or would write) it now: Codex asks again
	TrustMissing   = "missing"   // not in hooks.json (zm install codex adds it)
)

// HookTrust is one zm hook as Codex sees it.
type HookTrust struct {
	Event string `json:"event"` // SessionStart, UserPromptSubmit
	Key   string `json:"key,omitempty"`
	State string `json:"state"`
	// Unverified: untrusted, but config.toml records Codex hook state only
	// under other keys. Either the user trusted other hooks and not these, or
	// this Codex keys hooks in a way zm does not read.
	Unverified bool `json:"unverified,omitempty"`
}

// codexDir is $CODEX_HOME (o.ConfigDir), else <home>/.codex.
func codexDir(o Options) string {
	if o.ConfigDir != "" {
		return o.ConfigDir
	}
	return filepath.Join(o.Home, ".codex")
}

// CodexHookTrust reads dir's hooks.json and config.toml and reports, for each
// hook zm installs, whether Codex has trusted it. It never writes.
func CodexHookTrust(dir string) ([]HookTrust, error) {
	path := filepath.Join(dir, "hooks.json")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []hookEntry `json:"hooks"`
		} `json:"hooks"`
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	} else if real, err := filepath.EvalSymlinks(dir); err == nil {
		path = filepath.Join(real, "hooks.json")
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	state := codexHookState(string(cfg))
	var out []HookTrust
	ours := false
	for _, h := range codexHooks {
		t := HookTrust{Event: h.Event, State: TrustMissing}
	find:
		for gi, g := range doc.Hooks[h.Event] {
			for hi, e := range g.Hooks {
				if isZmHook(e.Command, "codex") {
					t.Key = fmt.Sprintf("%s:%s:%d:%d", path, snakeCase(h.Event), gi, hi)
					st, ok := state[t.Key]
					ours = ours || ok
					switch {
					case st.disabled:
						t.State = TrustDisabled
					case st.trusted:
						t.State = TrustTrusted
					default:
						t.State = TrustUntrusted
					}
					break find
				}
			}
		}
		out = append(out, t)
	}
	if !ours && len(state) > 0 {
		for i := range out {
			out[i].Unverified = out[i].State == TrustUntrusted
		}
	}
	return out, nil
}

// snakeCase is Codex's event key form: SessionStart -> session_start.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

type hookState struct{ trusted, disabled bool }

// codexHookState reads hooks.state.<key>.{trusted_hash,enabled} from
// config.toml text: table headers ([hooks.state."k"]), dotted keys and
// inline tables. Anything else is ignored (this is a reader of one table,
// not a TOML parser; an unreadable line never makes a hook look trusted).
func codexHookState(text string) map[string]hookState {
	out := map[string]hookState{}
	var table []string
	set := func(path []string, val string) {
		if len(path) != 4 || path[0] != "hooks" || path[1] != "state" {
			return
		}
		st := out[path[2]]
		val = strings.TrimSpace(val)
		switch path[3] {
		case "trusted_hash":
			if s, ok := tomlScalarString(val); ok && s != "" {
				st.trusted = true
			}
		case "enabled":
			st.disabled = val == "false"
		}
		out[path[2]] = st
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cutComment(line), "["), "]"))
			if strings.HasPrefix(inner, "[") { // [[array of tables]]
				table = nil
				continue
			}
			table, _ = splitDotted(inner)
			continue
		}
		k, v, ok := cutTopEquals(line)
		if !ok {
			continue
		}
		keys, ok := splitDotted(k)
		if !ok {
			continue
		}
		path := append(append([]string(nil), table...), keys...)
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "{") { // "key" = { trusted_hash = "...", enabled = false }
			body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cutComment(v), "{"), "}"))
			for _, kv := range strings.Split(body, ",") {
				if ik, iv, ok := cutTopEquals(kv); ok {
					if ikeys, ok := splitDotted(ik); ok {
						set(append(append([]string(nil), path...), ikeys...), iv)
					}
				}
			}
			continue
		}
		set(path, cutComment(v))
	}
	return out
}

// splitDotted splits a TOML key (bare and quoted parts, dot-separated).
func splitDotted(s string) ([]string, bool) {
	var parts []string
	s = strings.TrimSpace(s)
	for s != "" {
		var part string
		switch s[0] {
		case '"', '\'':
			q := s[0]
			end := -1
			for i := 1; i < len(s); i++ {
				if q == '"' && s[i] == '\\' {
					i++
					continue
				}
				if s[i] == q {
					end = i
					break
				}
			}
			if end < 0 {
				return nil, false
			}
			raw := s[:end+1]
			if q == '"' {
				if err := json.Unmarshal([]byte(raw), &part); err != nil {
					return nil, false
				}
			} else {
				part = raw[1 : len(raw)-1]
			}
			s = s[end+1:]
		default:
			i := strings.IndexByte(s, '.')
			if i < 0 {
				i = len(s)
			}
			part = strings.TrimSpace(s[:i])
			s = s[i:]
		}
		parts = append(parts, part)
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}
		if s[0] != '.' {
			return nil, false
		}
		s = strings.TrimSpace(s[1:])
	}
	return parts, len(parts) > 0
}

// cutTopEquals splits "key = value" at the first '=' outside quotes.
func cutTopEquals(s string) (string, string, bool) {
	var q byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case q != 0 && c == '\\' && q == '"':
			i++
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case q == 0 && c == '=':
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// cutComment drops a trailing # comment outside quotes.
func cutComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case q != 0 && c == '\\' && q == '"':
			i++
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case q == 0 && c == '#':
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

func tomlScalarString(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1], true
	}
	var s string
	if len(v) >= 2 && v[0] == '"' && json.Unmarshal([]byte(v), &s) == nil {
		return s, true
	}
	return "", false
}
