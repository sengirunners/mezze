package orchapi

import "testing"

func TestSharesTermTitle(t *testing.T) {
	for _, c := range []struct {
		agent string
		cmd   []string
		want  bool
	}{
		{"claude", nil, true},
		{"claude", []string{"/opt/bin/claude", "--model", "opus"}, true},
		{"codex", []string{"srt", "--settings", "x", "codex"}, true},
		{"claude", []string{"/bin/zsh"}, false},
		{"shell", []string{"/bin/zsh"}, false},
		{"shell", nil, false},
		{"mytool", []string{"mytool"}, false},
	} {
		if got := SharesTermTitle(c.agent, c.cmd); got != c.want {
			t.Errorf("SharesTermTitle(%q, %q) = %v", c.agent, c.cmd, got)
		}
	}
}
