package mail

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// For you (SPEC 9 "For you"): mail addressed to a person is one item in that
// person's Decisions. It is private to them (participants audience, set at
// send), and only their own Got it marks it read: no ack, reply or agent
// does. A decision for a person is an ask (SPEC 16), whose mail its decision
// answers; asks keep their own rules.

// askMail is the approvals module's side of AskChecker that also knows
// decided asks: their mail is answered by the decision, so it is not a
// for-you item even when it went to a person.
type askMail interface {
	IsAsk(ctx context.Context, project, id string) (bool, error)
}

// isAsk reports whether mail id of project is an ask, open or decided.
func (s *Service) isAsk(ctx context.Context, project, id string) (bool, error) {
	s.mu.RLock()
	a := s.asks
	s.mu.RUnlock()
	if a == nil || id == "" {
		return false, nil
	}
	if am, ok := a.(askMail); ok {
		return am.IsAsk(ctx, project, id)
	}
	return a.OpenAsk(ctx, project, id)
}

// personal reports whether m went to a person (human:<name>), the shape of a
// for-you item before the ask check. A report is never one (SPEC 9
// "Reports"): it is read in the person's reports, not their Decisions.
func personal(m row) bool {
	return strings.HasPrefix(m.Recipient, "human:") && m.Kind != orchapi.MailKindNotice && m.Kind != orchapi.MailKindReport
}

// forYou reports whether m is a for-you item: mail to a person that is not
// an ask and not the person's own note to themselves.
func (s *Service) forYou(ctx context.Context, m row) (bool, error) {
	if !personal(m) || m.From == m.Recipient {
		return false, nil
	}
	ask, err := s.isAsk(ctx, m.Project, m.ID)
	return !ask, err
}

// refuseForYou keeps every ack path off a for-you item: only its person's
// Got it in Decisions (GotIt) marks it read. m is mail the caller can see.
func (s *Service) refuseForYou(ctx context.Context, m row) error {
	fy, err := s.forYou(ctx, m)
	if err != nil || !fy {
		return err
	}
	who := strings.TrimPrefix(m.Recipient, "human:")
	return api.Errorf(api.CodeForbidden, "mail %s is for %s; only their Got it in Decisions marks it read", short(m.ID), who).
		WithHint("it waits in " + who + "'s Decisions (the Mezze app, the web app or the phone); nobody else takes it")
}

// pacific is where the digest's 08:00 is; a fixed UTC-8 if the zone
// database is missing (an hour early in summer, never lost).
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PST", -8*3600)
}()

// nextDigest is the next 08:00 Pacific strictly after t.
func nextDigest(t time.Time) time.Time {
	p := t.In(pacific)
	at := time.Date(p.Year(), p.Month(), p.Day(), 8, 0, 0, 0, pacific)
	if !at.After(t) {
		at = time.Date(p.Year(), p.Month(), p.Day()+1, 8, 0, 0, 0, pacific)
	}
	return at.UTC()
}

// heldSQL leaves out digest items not yet due (now, in ms, is its argument).
const heldSQL = "coalesce(json_extract(provenance, '$.held_until'), 0) <= ?"

// ForYou lists sub's own unread for-you mail (orchcore.ForYouMail).
func (s *Service) ForYou(ctx context.Context, sub *authz.Subject, limit int) ([]orchapi.Mail, error) {
	c := s.callerOf(ctx, sub)
	if !c.human() {
		return nil, nil
	}
	me := c.addr.String()
	now := store.Millis(s.now())
	mine, ma := c.mineSQL()
	rs, err := s.query(ctx, sub.Scope, me, limitOf(limit), func(p scoped) (string, []any, bool) {
		return mine + " AND acked_at IS NULL AND sender != ? AND kind != ? AND " + heldSQL,
			append(append([]any{}, ma...), me, orchapi.MailKindNotice, now), true
	})
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Mail, 0, len(rs))
	for _, r := range rs {
		if fy, err := s.forYou(ctx, r); err != nil {
			return nil, err
		} else if fy {
			out = append(out, s.redact(ctx, sub, r))
		}
	}
	return out, nil
}

// GotIt marks for-you mail id read as its person (orchcore.ForYouMail). Hidden
// and missing answer the same not_found; mail sub sees but that is not its
// own for-you item is forbidden.
func (s *Service) GotIt(ctx context.Context, sub *authz.Subject, id string) (orchapi.AckResponse, error) {
	return s.gotIt(ctx, sub, sub.Scope, id)
}

func (s *Service) gotIt(ctx context.Context, sub *authz.Subject, sc authz.Scope, id string) (orchapi.AckResponse, error) {
	m, db, err := s.find(ctx, sc, sub.Address, id)
	if err != nil {
		return orchapi.AckResponse{}, err
	}
	c := s.callerOf(ctx, sub)
	fy, err := s.forYou(ctx, m)
	if err != nil {
		return orchapi.AckResponse{}, err
	}
	if !fy {
		return orchapi.AckResponse{}, api.Errorf(api.CodeForbidden, "mail %s is not a for-you item", short(m.ID)).
			WithHint("Got it is for news addressed to a person; other mail is acked (zm ack) and asks are answered")
	}
	if !c.human() || !c.ownsPersonal(m) {
		return orchapi.AckResponse{}, api.Errorf(api.CodeForbidden, "only %s marks mail %s read, with Got it in Decisions",
			strings.TrimPrefix(m.Recipient, "human:"), short(m.ID))
	}
	if err := sub.Check(authz.Read, authz.Object{Project: m.Project, Area: m.Area}, "mail", m.ID); err != nil {
		return orchapi.AckResponse{}, err
	}
	return s.ack(ctx, c, db, m)
}

// handleGotIt is POST /v0/mail/{id}/got-it: a Decisions client's Got it, for
// the person the mail is addressed to. No zm command calls it.
func (s *Service) handleGotIt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.gotIt(ctx, core.SubjectOf(ctx), sc, r.PathValue("id"))
	respond(w, a, err)
}
