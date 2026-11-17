package integrations

import "testing"

// Claude Code exports CLAUDE_CODE_SESSION_ID to its shell's
// children, so Codex run from Claude sees both variables; the nearer agent
// process above decides which runtime this is.
func TestEnvSessionBothSetNearestAgentDecides(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	both := env(map[string]string{EnvClaudeSession: "cl-1", EnvCodexThread: "cx-1"})
	for _, tt := range []struct {
		name      string
		getenv    func(string) string
		nearest   string
		wantAgent string
		wantID    string
	}{
		{"codex under claude", both, "codex", "codex", "cx-1"},
		{"claude under codex", both, "claude", "claude", "cl-1"},
		{"both, no agent process seen", both, "", "claude", "cl-1"},
		{"claude only", env(map[string]string{EnvClaudeSession: "cl-1"}), "codex", "claude", "cl-1"},
		{"codex only", env(map[string]string{EnvCodexThread: "cx-1"}), "claude", "codex", "cx-1"},
		{"neither", env(nil), "codex", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agent, id := envSessionWalk(tt.getenv, func(...string) (int, string) {
				if tt.nearest == "" {
					return 0, ""
				}
				return 4242, tt.nearest
			})
			if agent != tt.wantAgent || id != tt.wantID {
				t.Fatalf("got %s %s, want %s %s", agent, id, tt.wantAgent, tt.wantID)
			}
		})
	}
}
