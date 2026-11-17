package orchapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/potluck/kit/api"
)

// Chores: a step an agent's report asks its reader to do (SPEC 9
// "Reports"). A person judges or clicks a permission, nothing more; a step
// they must do goes on an ask as its action. Whether a report asks for one
// is a question of meaning, so the mail-chore judge answers it (an
// owner rule: never a regex or a word list). A card is
// read by the card judge (internal/approvals/cardjudge.go).

//go:embed mailchore.md
var mailChorePrompt string

const mailChoreSchema = `{"type":"object","properties":{
"pass":{"type":"boolean"},"fixes":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},
"required":["pass","fixes","reason"],"additionalProperties":false}`

// MailChoreWait bounds one mail-chore judging: the sender waits for it.
var MailChoreWait = 40 * time.Second

// MailChoreInput is what the mail-chore judge reads.
type MailChoreInput struct {
	Reader string `json:"reader"` // the person's name, or "the reader"
	Body   string `json:"body"`
}

// MailChoreVerdict is the mail-chore judge's verdict on body
// (mailchore.md, with its eval cases in testdata/mailchore).
func MailChoreVerdict(ctx context.Context, c judge.Caller, in MailChoreInput) judge.Verdict {
	j := judge.Judge{Name: "mail-chore judge", Prompt: mailChorePrompt, Schema: json.RawMessage(mailChoreSchema), Timeout: MailChoreWait}
	return j.Run(ctx, c, in)
}

// RefuseChoreMail refuses an agent's report to a person (SPEC 9 "Reports")
// whose body asks them to do a step: a report is read later, if at all, so
// the step is lost. The step goes on an ask as its action, which
// the person approves with one press. A judge that cannot answer (no
// provider, a timeout, a model error) refuses it as unavailable: a report is
// never filed unchecked. A fresh note to a person is already refused
// (RefusePersonMail); a nudge and an ask's own mail pass, as does a reply
// (the caller skips it).
func RefuseChoreMail(ctx context.Context, c judge.Caller, from, kind, recipient string, needs Needs, body string) error {
	if strings.ToLower(strings.TrimSpace(kind)) != MailKindReport || !AgentSender(from) || !ToPerson(recipient, needs) {
		return nil
	}
	v := MailChoreVerdict(ctx, c, MailChoreInput{Reader: recipientName(recipient), Body: body})
	who := askTo(recipient)
	switch v.Status {
	case judge.Pass:
		return nil
	case judge.Fail:
		why := strings.TrimSuffix(v.Reason, ".")
		if why == "" {
			why = "the judge read a step in it"
		}
		return api.Errorf(api.CodeBadRequest, "not sent: this report asks %s to do a step (%s), and a report is read later, if at all, so the step is lost. Put it on an ask as its action: they approve with one press and zm does it on their machine, as them", recipientName(recipient), why).
			WithHint(strings.TrimSuffix(strings.Join(v.Fixes, "; "), ".") + ". To carry the step: zm ask machines " + who + " (their machines that take tasks), then zm ask new --to " + who + " --run-on <machine> --agent claude --brief <file> (or --command or --script); or do the step yourself and report that it is done")
	default:
		return api.Errorf(api.CodeUnavailable, "not sent: the judge that reads a report for steps put to %s could not answer (%s), and a report is never filed unchecked", recipientName(recipient), v.Reason).
			WithHint("send it again in a minute; if it keeps failing, the daemon has no provider CLI signed in (ZM_JUDGE)")
	}
}

func recipientName(recipient string) string {
	if strings.HasPrefix(recipient, "human:") {
		return strings.TrimPrefix(recipient, "human:")
	}
	return "the reader"
}
