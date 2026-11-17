package approvals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
)

// Asks that carry an action (SPEC 16): a card names one typed, bounded
// action, frozen when the ask is made. When an approver picks its choice, a
// decision effect runs exactly that action as the approver. Nothing is
// evaluated later, so the approval covers exactly what runs.

// mailActions is the mail module's side, or nil.
func (s *Service) mailActions() orchcore.MailActions {
	m, _ := orchcore.Mail(s.d).(orchcore.MailActions)
	return m
}

// cleanAction validates the asker's action against the card's choice keys
// and freezes it: full mail ids the asker can see in project, the count and
// a few first lines the daemon read. Asker-sent count and samples are dropped.
// With the asker's risk (risk.go) and, then, its confirm word.
func (s *Service) cleanAction(ctx context.Context, sub *authz.Subject, project, area string, in orchapi.AskAction, keys []string) (*orchapi.AskAction, error) {
	act, err := s.cleanActionKind(ctx, sub, project, area, in, keys)
	if err != nil {
		return nil, err
	}
	risk, err := cleanRisk(in.Risk, act.Kind)
	if err != nil {
		return nil, err
	}
	if risk != "" {
		act.Risk, act.ConfirmWord = risk, first(act.Machine, s.d.Projects.Slug(project), project)
	}
	return act, nil
}

func (s *Service) cleanActionKind(ctx context.Context, sub *authz.Subject, project, area string, in orchapi.AskAction, keys []string) (*orchapi.AskAction, error) {
	if in.Kind != orchapi.ActionAckMail && in.Kind != orchapi.ActionHand && !runsSomething(in.Kind) {
		return nil, api.Errorf(api.CodeBadRequest, "action kind %q: %s, %s, %s, %s, %s or %s", in.Kind,
			orchapi.ActionAckMail, orchapi.ActionRun, orchapi.ActionCommand, orchapi.ActionBrowser, orchapi.ActionAuthorizeKey, orchapi.ActionHand)
	}
	on := strings.ToLower(strings.TrimSpace(in.On))
	if on == "" {
		on = keys[0]
	}
	found := false
	for _, k := range keys {
		found = found || k == on
	}
	if !found {
		return nil, api.Errorf(api.CodeBadRequest, "action on %q: one of the card's choices (%s)", on, strings.Join(keys, ", "))
	}
	if runsSomething(in.Kind) {
		return s.cleanRunning(ctx, sub, project, area, in, on)
	}
	if in.Kind == orchapi.ActionHand {
		return cleanHand(sub, project, area, in, on)
	}
	if len(in.IDs) == 0 || len(in.IDs) > orchapi.MaxActionIDs {
		return nil, api.Errorf(api.CodeBadRequest, "an %s action names 1 to %d mails, got %d", orchapi.ActionAckMail, orchapi.MaxActionIDs, len(in.IDs))
	}
	mc := s.mailActions()
	if mc == nil {
		return nil, api.Errorf(api.CodeUnavailable, "the mail module cannot run actions")
	}
	full, lines, err := mc.AckPreview(ctx, sub, project, in.IDs)
	if err != nil {
		return nil, err
	}
	var samples []string
	for _, l := range lines {
		l = strings.TrimSpace(textclean.Clean(l))
		if l == "" || unsafePresentation(l) {
			l = "(not shown)"
		}
		samples = append(samples, l)
	}
	return &orchapi.AskAction{Kind: in.Kind, IDs: full, On: on, Count: len(full), Samples: samples}, nil
}

// actionLine names a card's action in its receipt, or "".
func actionLine(c *orchapi.AskCard) string {
	if c == nil || c.Action == nil {
		return ""
	}
	if c.Action.Kind != orchapi.ActionAckMail {
		return fmt.Sprintf("%s sha256=%s on=%s", c.Action.Kind, textSHA(marshal(*c.Action)), c.Action.On)
	}
	return fmt.Sprintf("%s count=%d ids_sha256=%s on=%s", c.Action.Kind, len(c.Action.IDs), idsSHA(c.Action.IDs), c.Action.On)
}

func idsSHA(ids []string) string { return textSHA(strings.Join(ids, "\n")) }

// ackBody is an ack effect's payload: the frozen ids and who takes them.
type ackBody struct {
	Taker string   `json:"taker"`
	IDs   []string `json:"ids"`
}

// ackTaker is who an approved ack records as the taker: the approver as a
// human, else this machine's human.
func (s *Service) ackTaker(a orchapi.Ask, d orchapi.Decision) string {
	if from := decisionSender(a, &d); from != "" {
		return from
	}
	if ad, err := api.ParseAddress("human:" + d.Approver); err == nil && ad.Kind == api.KindHuman {
		return ad.String()
	}
	return s.d.Config.HumanAddress()
}

// frozenAction is a's action if a's stored card still hashes to the
// card_sha256 the approver saw and the receipt signs; a changed card is a
// conflict, never run.
func frozenAction(a orchapi.Ask) (*orchapi.AskAction, error) {
	if a.Card == nil || a.Card.Action == nil {
		return nil, nil
	}
	if cardSHA(marshal(*a.Card)) != a.CardSHA {
		return nil, api.Errorf(api.CodeConflict, "ask %s: its card changed after it was asked; the action does not run", a.ID).
			WithData(orchapi.Refusal{Reason: orchapi.ReasonCardChanged})
	}
	return a.Card.Action, nil
}

// actionEffect is the decision's ack or action effect, when the decision
// picks the action's choice.
func (s *Service) actionEffect(ctx context.Context, a orchapi.Ask, d orchapi.Decision) (*effect, error) {
	act, err := frozenAction(a)
	if err != nil || act == nil || d.Decision != act.On {
		return nil, err
	}
	if runsSomething(act.Kind) {
		return s.runEffect(ctx, a, d, act)
	}
	if act.Kind == orchapi.ActionHand {
		return nil, nil // the approver's own agent works from it (the Mezze app); its verify check tracks it
	}
	return &effect{Kind: "ack", Target: a.Project, Body: marshal(ackBody{Taker: s.ackTaker(a, d), IDs: act.IDs})}, nil
}

// runAck delivers an ack effect: only the ids frozen on a's card, in a's
// project. A body that names others is a conflict.
func (s *Service) runAck(ctx context.Context, a orchapi.Ask, e effect) error {
	var b ackBody
	if err := json.Unmarshal([]byte(e.Body), &b); err != nil {
		return api.Errorf(api.CodeConflict, "ack payload: %v", err)
	}
	act, err := frozenAction(a)
	if err != nil {
		return err
	}
	if act == nil || e.Target != a.Project || idsSHA(b.IDs) != idsSHA(act.IDs) || len(b.IDs) != len(act.IDs) {
		return api.Errorf(api.CodeConflict, "ack payload is not the action frozen on ask %s", a.ID)
	}
	mc := s.mailActions()
	if mc == nil {
		return api.Errorf(api.CodeUnavailable, "the mail module cannot run actions")
	}
	_, err = mc.AckApproved(ctx, a.Project, b.Taker, b.IDs)
	return err
}

// actionText is the card's action as a person reads it.
func actionText(act *orchapi.AskAction) string {
	if act == nil {
		return ""
	}
	if act.Kind != orchapi.ActionAckMail {
		return runningText(act) + riskText(act)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "If you choose %s, zm acks %d mails as you", strings.ToUpper(act.On), act.Count)
	if len(act.Samples) > 0 {
		b.WriteString(", like ")
		for i, x := range act.Samples {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoted(x))
		}
	}
	b.WriteString(". Any other answer does nothing.")
	return b.String()
}
