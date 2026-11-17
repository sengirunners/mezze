package mail

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// The mail side of an ask that carries an ack-mail action (SPEC 16 "Asks
// that carry an action"). The approvals module freezes the ids on the card
// with AckPreview and, once an approver picks the action's choice, runs
// AckApproved from its decision effects.

var _ orchcore.MailActions = (*Service)(nil)

// maxSamples is how many first lines a card shows.
const maxSamples = 3

// AckPreview implements orchcore.MailActions.
func (s *Service) AckPreview(ctx context.Context, sub *authz.Subject, project string, ids []string) ([]string, []string, error) {
	if sub == nil {
		return nil, nil, api.Errorf(api.CodeUnauthenticated, "no caller")
	}
	sc := sub.Scope.Only(project)
	full := make([]string, 0, len(ids))
	seen := map[string]bool{}
	var samples []string
	for _, id := range ids {
		m, _, err := s.find(ctx, sc, sub.Address, id)
		if err != nil {
			return nil, nil, err
		}
		if m.Project != project {
			return nil, nil, api.NotFound("mail", id)
		}
		if m.Recipient == "all" {
			return nil, nil, api.Errorf(api.CodeBadRequest, "mail %s is an announcement: nobody takes it", short(m.ID))
		}
		if err := s.refuseOpenAsk(ctx, m); err != nil {
			return nil, nil, err
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		full = append(full, m.ID)
		if len(samples) < maxSamples {
			samples = append(samples, firstLine(m.Body))
		}
	}
	return full, samples, nil
}

// AckApproved implements orchcore.MailActions.
func (s *Service) AckApproved(ctx context.Context, project, taker string, ids []string) (int, error) {
	addr, err := api.ParseAddress(taker)
	if err != nil || addr.Kind != api.KindHuman {
		return 0, api.Errorf(api.CodeBadRequest, "an approved ack is taken by a human, not %q", taker)
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return 0, err
	}
	c := caller{sub: &authz.Subject{Address: addr.String(), Scope: authz.All()}, addr: addr, machine: s.machine()}
	n := 0
	for _, id := range ids {
		m, err := getRow(ctx, db, project, id)
		if api.IsCode(err, api.CodeNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		if m.AckedAt != nil || m.Recipient == "all" {
			continue
		}
		// An action never decides another ask: only its own decision does.
		if err := s.refuseOpenAsk(ctx, m); err != nil {
			continue
		}
		r, err := s.ack(ctx, c, db, m)
		if err != nil {
			return n, err
		}
		if r.Acked {
			n++
		}
	}
	return n, nil
}

// firstLine is a mail's first non-empty line, at most 80 characters.
func firstLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > 80 {
				l = string(r[:79]) + "…"
			}
			return l
		}
	}
	return ""
}
