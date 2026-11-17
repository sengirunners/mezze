package main

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/integrations"
)

// zm install codex tells the user plainly to trust the hooks in Codex,
// and says so again on a re-run while they are still untrusted.
func TestInstallSaysTrustCodexHooks(t *testing.T) {
	untrusted := integrations.Result{Agent: "codex", Trust: []integrations.HookTrust{
		{Event: "SessionStart", State: integrations.TrustUntrusted}, {Event: "UserPromptSubmit", State: integrations.TrustTrusted}}}
	out := formatInstall("install", untrusted)
	for _, want := range []string{"SessionStart untrusted", "Open Codex, run /hooks", "zm never trusts hooks for you", "No config changes needed"} {
		if !strings.Contains(out, want) {
			t.Errorf("untrusted output lacks %q:\n%s", want, out)
		}
	}
	unsure := integrations.Result{Agent: "codex", Trust: []integrations.HookTrust{
		{Event: "SessionStart", State: integrations.TrustUntrusted, Unverified: true}}}
	if out := formatInstall("install", unsure); !strings.Contains(out, "If you already trusted them with /hooks") || strings.Contains(out, "Codex will not run them yet") {
		t.Errorf("unverified output does not hedge:\n%s", out)
	}
	trusted := integrations.Result{Agent: "codex", Trust: []integrations.HookTrust{{Event: "SessionStart", State: integrations.TrustTrusted}}}
	if out := formatInstall("install", trusted); strings.Contains(out, "/hooks") || !strings.Contains(out, "Nothing to do") {
		t.Errorf("trusted output:\n%s", out)
	}
	if out := formatInstall("install", integrations.Result{Agent: "claude"}); strings.Contains(out, "codex hooks") {
		t.Errorf("claude output mentions codex:\n%s", out)
	}
}
