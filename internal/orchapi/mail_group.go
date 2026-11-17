package orchapi

import (
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Mail to several people (SPEC 9): a comma list of 2 to 8 people besides
// the sender, sent by a person.
const (
	MinGroupRecipients = 2
	MaxGroupRecipients = 8
)

// IsMailGroup reports whether to names several recipients.
func IsMailGroup(to string) bool { return strings.Contains(to, ",") }

// MailGroup parses to, a comma list of human:<name> addresses, for a send
// from from. It answers the recipients (deduped, without the sender) and
// the members (the sender and every recipient), both sorted.
func MailGroup(from, to string) (recipients, members []string, err error) {
	sender, perr := api.ParseAddress(from)
	if perr != nil || sender.Kind != api.KindHuman {
		return nil, nil, api.Errorf(api.CodeBadRequest, "only a person sends mail to several people").
			WithHint("a report for the team goes to owners: zm send owners --kind report <body>")
	}
	seen := map[string]bool{sender.String(): true}
	for _, part := range strings.Split(to, ",") {
		a, perr := api.ParseAddress(strings.TrimSpace(part))
		if perr != nil || a.Kind != api.KindHuman {
			return nil, nil, api.Errorf(api.CodeBadRequest, "%q: mail to several people names only people (human:<name>,human:<name>)", strings.TrimSpace(part))
		}
		if !seen[a.String()] {
			seen[a.String()] = true
			recipients = append(recipients, a.String())
		}
	}
	if len(recipients) < MinGroupRecipients || len(recipients) > MaxGroupRecipients {
		return nil, nil, api.Errorf(api.CodeBadRequest, "mail to several people names %d to %d people besides you, not %d", MinGroupRecipients, MaxGroupRecipients, len(recipients)).
			WithHint("one person: zm send human:<name> <body>")
	}
	slices.Sort(recipients)
	members = append([]string{sender.String()}, recipients...)
	slices.Sort(members)
	return recipients, members, nil
}
