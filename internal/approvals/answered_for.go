package approvals

// Whose answer it is (SPEC 16 "Who answers a card"): a card is one
// person's to decide, the person it is addressed to, else (a card for every
// owner) the human whose session asked. Co-owners may answer each other's
// cards (the owners' standing rule), so another owner's answer stands, but
// never silently: the answer delivered to the asker says plainly that the
// person it was for did not give it, so an agent holds an irreversible step
// for them. A device paired for no one answers as no one (approverName).

import (
	"context"
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// cardPerson is the person a is theirs to decide and whether a was
// addressed to them ("" when a has no one person).
func cardPerson(a orchapi.Ask) (name string, addressed bool) {
	if a.Owner != "" {
		return a.Owner, true
	}
	if to, err := api.ParseAddress(a.To); err == nil && to.Kind == api.KindHuman {
		return to.Human, true
	}
	return a.AskerHuman, false
}

// notTheirsLine is the line the answer me gives to a carries when me is
// not shown to be the person a is theirs (another owner, or a device
// paired for no one), and that person. "" when it is their answer, a has no one person, or
// nothing is decided (sent back, or a program contact heard). A card for
// every owner is the asker's human's only when that human answers the
// project and is not this box's own account where several people answer
// (anyone at the box asks as it): else it is no one person's.
func (s *Service) notTheirsLine(ctx context.Context, a orchapi.Ask, me approverRow, req orchapi.DecideRequest) (line, theirs string) {
	person, addressed := cardPerson(a)
	if person == "" || req.Decision == orchapi.DecisionNeedsContext || req.As == orchapi.AnsweredAsContact {
		return "", ""
	}
	ctx = withPeopleCache(ctx)
	whose := "this card was for " + person
	if !addressed {
		whose = "this card went to every owner, and " + person + " started your session"
	}
	hold := fmt.Sprintf("hold anything that cannot be undone until %s agrees (zm ask new --to %s).", person, person)
	var err error
	if !addressed {
		all, perr := s.people(ctx, a.Project)
		if err = perr; err == nil {
			if _, _, rerr := resolvePerson(answering(all), person); rerr != nil {
				return "", "" // the asker's human answers nothing here: no one person's card
			}
			if local := s.d.Projects.LocalIdentity(); strings.EqualFold(person, local.Name) && solo(s.soleAnswerer(ctx, a.Project)) == "" {
				return "", "" // anyone at this box asks as its account
			}
		}
	}
	name := ""
	if err == nil {
		name, err = s.approverName(ctx, me, a.Project)
	}
	switch {
	case err != nil:
		return fmt.Sprintf("ANSWERER NOT CHECKED: %s; zm could not read whose answer this is (%s), so zm cannot say %s decided it: %s",
			whose, err, person, hold), person
	case name == "":
		return fmt.Sprintf("ANSWERED ON A DEVICE PAIRED FOR NO ONE: %s; it was answered on %s, which zm cannot tie to %s or anyone, so zm cannot say %s decided it: %s",
			whose, quoted(first(me.NodeName, me.Node, me.Name)), person, person, hold), person
	case strings.EqualFold(name, person) || s.samePerson(ctx, a.Project, name, person):
		return "", ""
	}
	return fmt.Sprintf("ANSWERED BY ANOTHER OWNER: %s; %s answered it, not %s. It stands (owners may answer each other's cards), but %s did not decide it: %s",
		whose, name, person, person, hold), person
}

var notTheirsMigrations = []store.Migration{
	// The person a decision's answer was not theirs: Decision.NotTheirs.
	{Kind: store.KindRegistry, Name: "approvals/0021_not_theirs", Script: `
ALTER TABLE decisions ADD COLUMN not_theirs TEXT NOT NULL DEFAULT '';
`},
}

// hideAskerHuman blanks d's NotTheirs when it names a's asker's human (a
// card for every owner), for a reader view does not show AskerHuman.
func hideAskerHuman(a orchapi.Ask, d *orchapi.Decision) *orchapi.Decision {
	if _, addressed := cardPerson(a); d == nil || d.NotTheirs == "" || addressed {
		return d
	}
	c := *d
	c.NotTheirs = ""
	return &c
}

// solo is soleAnswerer's name, "" (no one person) when unreadable.
func solo(name string, err error) string {
	if err != nil {
		return ""
	}
	return name
}
