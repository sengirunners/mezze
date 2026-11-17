package approvals

// A person's own machine (SPEC 16 "Asks that carry an
// action"): an action on a machine the project's owner marked as one
// person's (zm machine-class <machine> person <name>, kept by the team
// module) runs only on that person's signed yes; a team machine takes either
// owner's. The card is addressed to that person when it is made, so no other
// owner is shown a choice that cannot count; an answer from anyone else is
// refused; the start checks again, as the machine's person is then.

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// machinePerson is the person whose own machine machine is in project, or
// "" for a team machine. Without the team module's answer it is an error:
// an action is never placed on a machine whose person is not known.
func (s *Service) machinePerson(ctx context.Context, project, machine string) (string, error) {
	mp, ok := orchcore.Team(s.d).(orchcore.MachinePeople)
	if !ok {
		return "", api.Errorf(api.CodeUnavailable, "this daemon cannot tell whose machine %s is", machine)
	}
	return mp.MachinePerson(ctx, project, machine)
}

// actionPeople are the people whose yes alone may pick a's action: the
// machine's person frozen on the card and its person now (each "" left
// out). Empty for a team machine, or an ask with no action on a machine.
func (s *Service) actionPeople(ctx context.Context, a orchapi.Ask) ([]string, error) {
	act := actionOf(a)
	if act == nil || !runsSomething(act.Kind) || act.Machine == "" {
		return nil, nil
	}
	var out []string
	if act.MachinePerson != "" {
		out = append(out, act.MachinePerson)
	}
	now, err := s.machinePerson(ctx, a.Project, act.Machine)
	if err != nil {
		return nil, err
	}
	if now != "" && !strings.EqualFold(now, act.MachinePerson) {
		out = append(out, now)
	}
	return out, nil
}

// approverName is the identity name me answers project's asks as: a
// device's or key's person (zm approver add --for); on the owner's own
// socket, its identity. A device paired for no one is this machine's owner
// only while that owner is the one person who answers project
// (soleAnswerer); else it answers as no one (""): zm cannot say who holds
// it, and at a team authority this machine's owner is the box's account,
// whom no tap makes the answerer.
func (s *Service) approverName(ctx context.Context, me approverRow, project string) (string, error) {
	switch {
	case me.owner:
		return me.Name, nil
	case me.identity == "":
		return s.soleAnswerer(ctx, project)
	}
	sub, err := s.d.Projects.IdentitySubject(ctx, me.identity)
	if err != nil {
		return "", err
	}
	return sub.Identity.Name, nil
}

// onlyTheirYes refuses name's answer to a when a's action runs on another
// person's own machine. One person's linked accounts count as theirs
// (samePerson); a person who no longer answers the project counts as no one.
func (s *Service) onlyTheirYes(ctx context.Context, a orchapi.Ask, name string) error {
	people, err := s.actionPeople(ctx, a)
	if err != nil {
		return err
	}
	act := actionOf(a)
	for _, p := range people {
		if name == "" || !s.samePerson(ctx, a.Project, name, p) {
			return api.Errorf(api.CodeForbidden, "%s is %s's own machine: only %s answers an ask that runs something there", act.Machine, p, p).
				WithHint("leave it for " + p + "; an action that either owner may approve runs on a team machine")
		}
	}
	return nil
}

// onlyTheirAnswer is onlyTheirYes for an answer by me, to any of a's
// choices: the ask is that person's to decide.
func (s *Service) onlyTheirAnswer(ctx context.Context, a orchapi.Ask, me approverRow) error {
	if act := actionOf(a); act == nil || !runsSomething(act.Kind) || act.Machine == "" {
		return nil
	}
	name, err := s.approverName(ctx, me, a.Project)
	if err != nil {
		return err
	}
	return s.onlyTheirYes(ctx, a, name)
}

// addressToMachinePerson addresses a new card whose action runs on a
// person's own machine to that person: a card for the project's owners
// becomes theirs; a card that names someone else is refused, since that
// person's yes could never run it.
func (s *Service) addressToMachinePerson(ctx context.Context, a *orchapi.Ask, to *string) error {
	act := actionOf(*a)
	if act == nil || act.MachinePerson == "" {
		return nil
	}
	all, err := s.people(ctx, a.Project)
	if err != nil {
		return err
	}
	p, name, err := resolvePerson(answering(all), act.MachinePerson)
	if err != nil {
		return api.Errorf(api.CodeBadRequest, "%s is %s's own machine, and %s answers no asks in this project", act.Machine, act.MachinePerson, act.MachinePerson).
			WithHint("ask an owner to fix whose machine it is (zm machine-class), or run it on a team machine")
	}
	switch {
	case a.OwnerPerson == p.ID:
		return nil
	case a.Owner == "" && a.OwnerPerson == "" && (*to == "owners" || *to == ""):
		a.Owner, a.OwnerPerson = name, p.ID
		*to = "human:" + name
		a.To = *to
		return nil
	}
	who := strings.TrimPrefix(first(a.Owner, *to), "human:")
	return api.Errorf(api.CodeBadRequest, "%s is %s's own machine: only %s's yes runs something there, so this card cannot go to %s", act.Machine, name, name, who).
		WithHint("address it to " + name + " (--to human:" + name + "), or run it on a team machine")
}

// soleAnswerer is this machine's owner's name when they are the one person
// who answers project's asks, else "". An unreadable list is an error,
// never a guess.
func (s *Service) soleAnswerer(ctx context.Context, project string) (string, error) {
	people, err := s.people(ctx, project)
	if err != nil {
		return "", err
	}
	ans := answering(people)
	local := s.d.Projects.LocalIdentity()
	if len(ans) != 1 || local.Name == "" {
		return "", nil
	}
	if p, _, err := resolvePerson(ans, local.Name); err != nil || p.ID != ans[0].ID {
		return "", nil
	}
	return local.Name, nil
}
