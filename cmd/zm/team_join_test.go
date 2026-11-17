package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A sole authority is still shown with its tailnet owner and confirmed on a
// terminal; elsewhere, and with several, --from must name it. Skipped peers
// are listed with why.
func TestPickAuthority(t *testing.T) {
	one := api.JoinResponse{Authorities: []api.JoinAuthority{{Machine: "evil", Owner: "mallory@example.com"}},
		Skipped: []api.JoinSkipped{{Machine: "plain", Reason: "runs no GitHub logins (github:false)"}}}
	var msg bytes.Buffer
	if m, err := pickAuthority("harbor", one, true, strings.NewReader("y\n"), &msg); err != nil || m != "evil" {
		t.Fatalf("yes: %q %v", m, err)
	}
	if !strings.Contains(msg.String(), "join harbor at evil (tailnet owner mallory@example.com)? [y/N]") ||
		!strings.Contains(msg.String(), "skipped plain: runs no GitHub logins") {
		t.Fatalf("prompt %q", msg.String())
	}
	for what, tc := range map[string]struct {
		r        api.JoinResponse
		terminal bool
		in       string
	}{"no": {one, true, "n\n"}, "enter": {one, true, "\n"}, "no terminal": {one, false, "y\n"},
		"several": {api.JoinResponse{Authorities: []api.JoinAuthority{{Machine: "a"}, {Machine: "b"}}}, true, "y\n"}} {
		if m, err := pickAuthority("harbor", tc.r, tc.terminal, strings.NewReader(tc.in), &bytes.Buffer{}); err == nil {
			t.Errorf("%s: picked %q", what, m)
		}
	}
}
