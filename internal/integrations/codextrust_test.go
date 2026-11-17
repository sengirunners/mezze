package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Dogfood: Codex runs hooks.json hooks only after the user trusts them with
// /hooks, so a fresh `zm install codex` silently gave no notice. zm
// never trusts them itself; it reads Codex's state and says what is missing.
func TestCodexHookTrust(t *testing.T) {
	o := opts(t, "codex")
	o.Apply = true
	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	states := func(tr []HookTrust) []string {
		var out []string
		for _, x := range tr {
			out = append(out, x.Event+"="+x.State)
		}
		return out
	}
	if got := states(r.Trust); !reflect.DeepEqual(got, []string{"SessionStart=changed", "UserPromptSubmit=changed", "PreToolUse=changed", "PostToolUse=changed", "SessionEnd=changed", "SubagentStop=changed"}) {
		t.Fatalf("fresh install: %v", got)
	}
	o.Apply = false
	if r, err = Install(o); err != nil || r.Changed() {
		t.Fatal(r, err)
	}
	if got := states(r.Trust); !reflect.DeepEqual(got, []string{"SessionStart=untrusted", "UserPromptSubmit=untrusted", "PreToolUse=untrusted", "PostToolUse=untrusted", "SessionEnd=untrusted", "SubagentStop=untrusted"}) ||
		!reflect.DeepEqual(r.Untrusted(), []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd", "SubagentStop"}) {
		t.Fatalf("installed, not trusted yet: %v", got)
	}
	dir := filepath.Join(o.Home, ".codex")
	real, _ := filepath.EvalSymlinks(filepath.Join(dir, "hooks.json"))
	if want := real + ":session_start:0:0"; r.Trust[0].Key != want {
		t.Fatalf("key %q, want %q (Codex keys hooks by the file's real path)", r.Trust[0].Key, want)
	}
	// What Codex writes once the user trusts (header form) or turns one off
	// (inline form); nothing zm wrote changes.
	cfg := filepath.Join(dir, "config.toml")
	before, _ := os.ReadFile(cfg)
	extra := fmt.Sprintf("\n[hooks.state.%q]\ntrusted_hash = \"sha256:abc\" # trusted\n\n[hooks.state]\n%q = { trusted_hash = \"sha256:def\", enabled = false }\n",
		real+":session_start:0:0", real+":user_prompt_submit:0:0")
	if err := os.WriteFile(cfg, append(before, extra...), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := CodexHookTrust(dir)
	if err != nil || !reflect.DeepEqual(states(tr), []string{"SessionStart=trusted", "UserPromptSubmit=disabled", "PreToolUse=untrusted", "PostToolUse=untrusted", "SessionEnd=untrusted", "SubagentStop=untrusted"}) {
		t.Fatalf("after /hooks: %v %v", states(tr), err)
	}
	// Dotted keys work too; a trust entry for another place does not count.
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf("hooks.state.%q.trusted_hash = 'sha256:x'\n[hooks.state.\"/elsewhere/hooks.json:session_start:0:0\"]\ntrusted_hash = \"sha256:y\"\n",
		real+":user_prompt_submit:0:0")), 0o600)
	if tr, _ := CodexHookTrust(dir); !reflect.DeepEqual(states(tr), []string{"SessionStart=untrusted", "UserPromptSubmit=trusted", "PreToolUse=untrusted", "PostToolUse=untrusted", "SessionEnd=untrusted", "SubagentStop=untrusted"}) {
		t.Fatalf("dotted: %v", states(tr))
	}
	// Review: Codex state that names none of zm's keys (other hooks, or a
	// Codex that keys them differently) marks the untrusted ones unverified,
	// so the install output hedges instead of nagging.
	_ = os.WriteFile(cfg, []byte("[hooks.state.\"/elsewhere/hooks.json:session_start:0:0\"]\ntrusted_hash = \"sha256:y\"\n"), 0o600)
	if tr, _ := CodexHookTrust(dir); !tr[0].Unverified || !tr[1].Unverified || tr[0].State != TrustUntrusted {
		t.Fatalf("other keys only: %+v", tr)
	}
	_ = os.WriteFile(cfg, nil, 0o600)
	if tr, _ := CodexHookTrust(dir); tr[0].Unverified {
		t.Fatalf("no state at all is plain untrusted: %+v", tr)
	}
	// No hooks.json: missing, never trusted.
	if tr, _ := CodexHookTrust(t.TempDir()); !reflect.DeepEqual(states(tr), []string{"SessionStart=missing", "UserPromptSubmit=missing", "PreToolUse=missing", "PostToolUse=missing", "SessionEnd=missing", "SubagentStop=missing"}) {
		t.Fatalf("empty dir: %v", states(tr))
	}
	if got := snakeCase("UserPromptSubmit"); got != "user_prompt_submit" {
		t.Fatal(got)
	}
}
