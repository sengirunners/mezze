package approvals

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// The card router (SPEC 16 "Card judge"): beside the card
// judge, a model reads a new card that names no outside party and says
// whether its question is really for one of the outside parties the project
// already keeps a held list for (ask.for). Such a card is stored held for
// that party, out of the owners' queue. The parties are the project's own
// records, never a list in code. Anything short of a clear majority for one
// party (no answer, a split, a party not on the list) leaves the card with
// the owners, as before.

//go:embed cardroute.md
var cardRoutePrompt string

const cardRouteSchema = `{"type":"object","properties":{
"reasoning":{"type":"string"},"party":{"type":"integer"}},
"required":["reasoning","party"],"additionalProperties":false}`

// maxRouteParties bounds the parties a router reads: the project's most
// recently used ones.
const maxRouteParties = 20

type routeParty struct {
	N       int    `json:"n"`
	Group   string `json:"group"`
	Contact string `json:"contact"`
}

type cardRouteInput struct {
	Parties []routeParty `json:"parties"`
	Card    judgeCard    `json:"card"`
}

// knownParties are the outside parties of project's area, newest first:
// each group and contact pair the operators' asks there were held for (open
// or answered and not held by the judge: a guest's asks, and withdrawn ones, which free their slot,
// never name a party), with the newest ref of each.
func (s *Service) knownParties(ctx context.Context, project, area string) []orchapi.AskFor {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT for_group FROM asks WHERE project = ? AND area = ? AND for_group != ''
		AND withdrawn IS NULL AND instr(dedupe, ?) > 0 AND NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = asks.id)
		ORDER BY created DESC LIMIT 500`, project, area, operatorsSlot)
	if err != nil {
		s.d.Log.Warn("approvals: read outside parties", "project", project, "err", err)
		return nil
	}
	defer rows.Close()
	seen := map[[2]string]bool{}
	var out []orchapi.AskFor
	for rows.Next() && len(out) < maxRouteParties {
		var raw string
		var f orchapi.AskFor
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &f) != nil || f.Group == "" || f.Contact == "" {
			continue
		}
		k := [2]string{f.Group, f.Contact}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// operatorsSlot marks an ask's dedupe slot as the operators' (fillCard).
const operatorsSlot = "\x1foperators\x1f"

// routeOutside is the outside party a's card is for, or nil (it stays with
// the owners). It asks only for an operator's card (operator) with a topic
// (one ask per party and topic) and no party, person, deadline, default or
// action of its own, in an area that keeps any
// party, and takes one of the card judge's slots within wait.
func (s *Service) routeOutside(ctx context.Context, operator bool, a orchapi.Ask, wait time.Duration) *orchapi.AskFor {
	// A party's list holds no deadline, default or action (cleanFor); a card
	// put to a named person is theirs.
	if !operator || a.Card == nil || a.For != nil || a.Topic == "" || a.By != nil || a.ApplyDefault || a.Card.Action != nil || a.Owner != "" || a.To != "owners" {
		return nil
	}
	parties := s.knownParties(ctx, a.Project, a.Area)
	if len(parties) == 0 || wait <= time.Second {
		return nil
	}
	start := time.Now()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.judging <- struct{}{}:
		defer func() { <-s.judging }()
	case <-t.C:
		return nil
	case <-ctx.Done():
		return nil
	}
	if wait -= time.Since(start); wait <= time.Second {
		return nil
	}
	c := a.Card
	in := cardRouteInput{Card: judgeCard{Title: c.Title, Context: c.Context, Background: c.Background, Question: c.Question,
		Choices: c.Choices, Recommend: c.Recommend, Why: c.Why}}
	for i, p := range parties {
		in.Parties = append(in.Parties, routeParty{N: i + 1, Group: p.Group, Contact: p.Contact})
	}
	n := routeVotes(ctx, s.caller(), in, wait)
	if n == 0 {
		return nil
	}
	f := parties[n-1]
	s.d.Log.Info("approvals: card router held an ask for an outside party", "ask", a.ID, "group", f.Group)
	return &f
}

// routedOnRejudge is routeOutside for a held ask the judge passed later:
// only an operator's (its slot says so; an ask with no slot stays).
func (s *Service) routedOnRejudge(ctx context.Context, a orchapi.Ask) *orchapi.AskFor {
	var dedupe string
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT dedupe FROM asks WHERE id = ?`, a.ID).Scan(&dedupe); err != nil {
		return nil
	}
	return s.routeOutside(ctx, strings.Contains(dedupe, operatorsSlot), a, cardRejudgeWait)
}

// heldForNote tells the asker its card went to an outside party's list.
func heldForNote(f orchapi.AskFor) string {
	return fmt.Sprintf("held for %s (%s): the card router found this question is theirs to answer, so it waits on their list, not in the owners' queue; an owner records their answer", f.Group, f.Contact)
}

// routeVotes is the party every vote that answered names, with at least a
// majority of cardVotes answering. Anything else (a vote to stay, a split,
// too few answers, an answer off the list or out of shape, which counts as
// a vote to stay) is 0: a wrong route hides the
// owners' question, so one unsure vote keeps it with them.
func routeVotes(ctx context.Context, c judge.Caller, in cardRouteInput, wait time.Duration) int {
	j := judge.Judge{Name: "card router", Prompt: cardRoutePrompt, Schema: json.RawMessage(cardRouteSchema), Timeout: wait}
	votes := make([]int, cardVotes)
	var wg sync.WaitGroup
	for i := range votes {
		wg.Go(func() {
			votes[i] = -1 // no answer
			raw, err := j.Ask(ctx, c, in)
			if err != nil {
				return
			}
			var ans struct {
				Party *int `json:"party"`
			}
			votes[i] = 0 // an answer off the list or out of shape is unsure: stay
			if json.Unmarshal(raw, &ans) == nil && ans.Party != nil && *ans.Party >= 0 && *ans.Party <= len(in.Parties) {
				votes[i] = *ans.Party
			}
		})
	}
	wg.Wait()
	party, answered := 0, 0
	for _, v := range votes {
		switch {
		case v < 0:
			continue
		case v == 0 || (party != 0 && v != party):
			return 0
		}
		party, answered = v, answered+1
	}
	if answered <= len(votes)/2 {
		return 0
	}
	return party
}

// holdForRouted puts a held ask the router sent to party f on that party's
// list before it is released (it has not been sent, so nobody read it).
func (s *Service) holdForRouted(ctx context.Context, a orchapi.Ask, f orchapi.AskFor) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET for_group = ?, body = ? WHERE id = ? AND for_group = '' AND withdrawn IS NULL`,
		marshal(f), renderCard(*a.Card, &f), a.ID)
	return err
}
