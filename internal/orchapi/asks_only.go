package orchapi

import (
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// RefusePersonMail is the asks-only rule (SPEC 9 "Reports") for a
// fresh note an agent sends: one that would reach a person is refused, so a
// question never goes out as mail and lands unseen in their reports. A reply,
// an explicit --kind report and a nudge are not fresh notes. Not generated.
func RefusePersonMail(from, kind, recipient string, needs Needs) error {
	k := strings.ToLower(strings.TrimSpace(kind))
	if (k != "" && k != MailKindNote) || !AgentSender(from) || !ToPerson(recipient, needs) {
		return nil
	}
	who := recipient
	if !strings.HasPrefix(who, "human:") && who != "owners" {
		who = "a person"
	}
	return api.Errorf(api.CodeBadRequest, "not sent: people take decisions only, so mail from an agent to %s is refused. A choice, approval or should-I question is a zm ask new (it reaches their Decisions); an FYI with nothing to decide is --kind report", who).
		WithHint("zm ask new --to " + askTo(recipient) + " --help")
}

func askTo(recipient string) string {
	if strings.HasPrefix(recipient, "human:") {
		return recipient
	}
	return "human:<name>"
}
