package team_test

import (
	"testing"

	"github.com/sengirunners/mezze/internal/team"
)

// A replica's zm login github runs at the project's authority (SPEC 22):
// both the start and the poll are forwarded, once each.
func TestGitHubLoginForwarded(t *testing.T) {
	for _, path := range []string{"/v0/login/github/start", "/v0/login/github/01JABC"} {
		if k := team.ForwardedKind("POST", path, "mini"); k != "write" {
			t.Errorf("POST %s forwarded as %q, want write", path, k)
		}
	}
	if k := team.ForwardedKind("GET", "/v0/login/github/start", "mini"); k != "" {
		t.Errorf("a GET forwarded as %q", k)
	}
}
