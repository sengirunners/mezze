package approvals

import (
	"context"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// The owner decides (SPEC 16 "Owners decide"). The project's owner or admin
// answers a card ask from their own machine: on the authority's socket, or on
// a member machine whose socket forwards the call to the authority with its
// node-bound token. A paired approver device stays a way to decide, and the
// opt-in for when an agent on the owner's machine must not be able to: every
// process running as the owner's OS user reaches the socket too. There is no
// confirmation step: the right is the grant.

var ownerMigrations = []store.Migration{
	// method: how the decision was made. Every decision before this was an
	// approver's (a device or a key).
	{Kind: store.KindRegistry, Name: "approvals/0005_decided_by", Script: `
ALTER TABLE decisions ADD COLUMN method TEXT NOT NULL DEFAULT 'approver';
`},
}

// ownerRefused is the answer to a caller who may read the ask but holds no
// owner or admin grant on its project.
func ownerRefused() error {
	return api.Errorf(api.CodeForbidden, "only the project's owner or an admin answers an ask").
		WithHint("answer from the owner's own machine (zm ask answer), or on a paired approver device")
}

// ownerOf is the decider sub stands for on project, when sub owns or
// administers it (core.Deps.ProjectAdmin: by its scope, which covers this
// machine's socket, or a member machine's node-bound token by its identity's
// own grants). The machine is where the owner sat; an answer-only token's
// is answer-token:<token id>.
func (s *Service) ownerOf(ctx context.Context, sub *authz.Subject, project string) (approverRow, error) {
	ok, err := s.d.ProjectAdmin(ctx, sub, project)
	if err != nil {
		return approverRow{}, err
	}
	if !ok {
		return approverRow{}, ownerRefused()
	}
	name, machine := first(sub.Identity.Name, s.d.Config.Human), s.d.Config.Machine
	if l := sub.Limit(); l != "" {
		// An answer-only token: the kit let it reach only the routes that
		// admit its limit. It answers as its owner, unsigned, and its
		// decision names the token (SPEC 16 "Answer-only tokens").
		if l != orchapi.AnswerLimit || sub.TokenID == "" || sub.Identity.Name == "" {
			return approverRow{}, ownerRefused()
		}
		machine = orchapi.AnswerTokenMachine + sub.TokenID
		return approverRow{Approver: orchapi.Approver{Name: sub.Identity.Name, Node: orchapi.OwnerNodePrefix + machine, NodeName: machine}, owner: true, answerToken: true}, nil
	}
	switch {
	case sub.Machine != "": // a member machine's link
		machine = sub.Machine
	case sub.TokenID != "": // a token whose scope reaches admin: where it sat is unknown
		machine = "token:" + sub.TokenID
	case sub.Via != authz.ViaLocal:
		machine = sub.Via
	}
	return approverRow{Approver: orchapi.Approver{Name: name, Node: orchapi.OwnerNodePrefix + machine, NodeName: machine}, owner: true}, nil
}

// handleOwnerAnswer is POST /v0/asks/{id}/choose: the project's owner answers
// a card ask. The ask is looked up first so a member machine's socket moves on
// to the next authority on 404; one the caller cannot read is that same 404.
func (s *Service) handleOwnerAnswer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.DecideRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	me, err := s.ownerOf(ctx, sub, a.Project)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if a.Card != nil {
		s.groupMu.Lock() // a join lands before this answer reaches its group, or sees it
		defer s.groupMu.Unlock()
	}
	d, err := s.record(ctx, me, a.ID, req, func(cur orchapi.Ask) error {
		if cur.Subject.IsPR() {
			// A merge gate trusts that receipt to be a device's (SPEC 16).
			return api.Errorf(api.CodeForbidden, "a pull request's ask is decided on a paired device over the tailnet").
				WithHint("the owner answers note cards; a merge gate's receipt stays a paired device's")
		}
		if cur.Project != a.Project {
			return api.NotFound("ask", id)
		}
		if err := s.joinedActionAsk(ctx, cur); err != nil {
			return err
		}
		act := actionOf(cur)
		if err := answerTokenConfirmWord(me, act, req.Decision); err != nil {
			return err
		}
		if act != nil && signedOnly(act.Kind) && strings.ToLower(strings.TrimSpace(req.Decision)) == act.On {
			// Every process of the owner's OS user reaches this socket: an
			// agent there must not approve its own command (SPEC 16).
			return api.Errorf(api.CodeForbidden, "answer on a paired device or in the Mezze app: this choice runs something").
				WithHint("a choice that runs something takes a signed answer (zm ask decide on a paired device, or the Mezze app's key)").
				WithData(orchapi.Refusal{Reason: orchapi.ReasonSignedOnly})
		}
		return nil
	})
	if err == nil && a.Card != nil {
		s.decideGroup(ctx, sub, me, a, req) // the same question asked again: one answer for every ask of it
	}
	respond(w, d, err)
}

// answerTokenConfirmWord refuses an answer-only token's pick of a choice
// that carries a confirm word, whatever the action's kind: the word is
// printed on the card, so it guards a slip, not a token, and the choice it
// guards is a signed answer's.
func answerTokenConfirmWord(me approverRow, act *orchapi.AskAction, decision string) error {
	if !me.answerToken || act == nil || act.ConfirmWord == "" || strings.ToLower(strings.TrimSpace(decision)) != act.On {
		return nil
	}
	return api.Errorf(api.CodeForbidden, "answer on a paired device or in the Mezze app: this choice takes a confirm word").
		WithHint("an answer-only token never picks a choice that carries a confirm word").
		WithData(orchapi.Refusal{Reason: orchapi.ReasonSignedOnly})
}
