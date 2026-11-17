package approvals

// Risky actions (SPEC 16 "Risky actions"). An action
// whose choice cannot be undone or spends money takes one more thing from
// the person who picks it: a word typed in place (AskAction.ConfirmWord),
// never a second device. What makes an action risky is never read from its
// words by zm: the asker declares it (zm ask new --risk), the card judge
// answers it for every action card (cardjudge.md, its eval cases in
// testdata/cardjudge), and the higher of the two holds. A card released
// without the judge's check is unknown, so it is treated as risky
// (orchapi.RiskUnchecked). This guards against a slip, not an agent: an
// agent can read the word.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// riskRank orders risks; a card's risk only ever moves up.
func riskRank(r string) int {
	switch r {
	case "":
		return 0
	case orchapi.RiskIrreversible:
		return 1
	case orchapi.RiskSpending:
		return 2 // money comes first, as the judge is told (cardjudge.md 7)
	}
	return 3 // unchecked, or anything unknown: the most careful
}

// takesRisk reports whether an action of kind can carry a risk: what runs
// something or hands a brief. Acking mail is neither.
func takesRisk(kind string) bool { return signedOnly(kind) }

// cleanRisk is the asker's declared risk for an action of kind.
func cleanRisk(in, kind string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(in))
	switch {
	case r == "":
		return "", nil
	case r != orchapi.RiskIrreversible && r != orchapi.RiskSpending:
		return "", api.Errorf(api.CodeBadRequest, "risk %q: %s or %s", in, orchapi.RiskIrreversible, orchapi.RiskSpending)
	case !takesRisk(kind):
		return "", api.Errorf(api.CodeBadRequest, "risk is for an action that runs something or hands a brief, not %s", kind)
	}
	return r, nil
}

// raiseRisk raises a's action to risk if that is higher, freezes the
// confirm word, and recomputes what the card's bytes decide: card_sha256,
// a plain question's subject and the body. It answers whether anything
// changed. Only before anyone is asked: at creation, or while held.
func (s *Service) raiseRisk(a *orchapi.Ask, risk string) bool {
	if a.Card == nil || a.Card.Action == nil || !takesRisk(a.Card.Action.Kind) || riskRank(risk) <= riskRank(a.Card.Action.Risk) {
		return false
	}
	act := *a.Card.Action
	act.Risk = risk
	act.ConfirmWord = first(act.Machine, s.d.Projects.Slug(a.Project), a.Project)
	c := *a.Card
	c.Action = &act
	a.Card = &c
	a.CardSHA = cardSHA(marshal(c))
	if a.Subject.Work == "" {
		a.Subject.Question = textSHA("zm-question/1\n" + a.ID + "\n" + a.CardSHA)
	}
	a.Body = renderCard(c, a.For)
	return true
}

// raiseHeldRisk raises a held ask's risk after the judge passed it, under
// holdMu on a fresh read: only while it is still held and unchecked,
// unanswered, and its card is the one judged (a judging can take minutes,
// and a release unchecked meanwhile raised it already and sent it). ok
// false: leave it as it stands now. *a is the ask as stored after.
func (s *Service) raiseHeldRisk(ctx context.Context, a *orchapi.Ask, risk string) (bool, error) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	cur, err := s.ask(ctx, a.ID)
	if err != nil {
		return false, err
	}
	if _, unchecked := s.heldUnchecked(ctx, a.ID); !unchecked || cur.Decision != nil || cur.Withdrawn != nil || cur.CardSHA != a.CardSHA {
		return false, nil
	}
	judged := cur.CardSHA
	if !s.raiseRisk(&cur, risk) {
		*a = cur
		return true, nil
	}
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET card = ?, card_sha = ?, question = ?, body = ? WHERE id = ? AND card_sha = ?`,
		marshal(*cur.Card), cur.CardSHA, cur.Subject.Question, cur.Body, cur.ID, judged)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	*a = cur
	return true, nil
}

// raisedCard is card after raiseRisk(a, risk): the stored JSON to write.
func (s *Service) raisedCard(a *orchapi.Ask, risk, card string) string {
	if s.raiseRisk(a, risk) {
		return marshal(*a.Card)
	}
	return card
}

// cardNotRead is a 409 (reason card-changed) when the answer names a card
// (card_sha256) that is not a's: the person read another one.
func cardNotRead(req orchapi.DecideRequest, a orchapi.Ask) error {
	if sha := strings.TrimSpace(req.CardSHA); sha != "" && !strings.EqualFold(sha, a.CardSHA) {
		return api.Errorf(api.CodeConflict, "ask %s: its card is not the one you read (card_sha256); read it again", a.ID).
			WithHint("read the ask again (zm ask show " + a.ID + ")").WithData(orchapi.Refusal{Reason: orchapi.ReasonCardChanged})
	}
	return nil
}

// stillAnswerable is record's check in the decision's transaction, on the
// ask as it stands (prev): its card is the one the answer named and was
// checked against (a release unchecked or a rejudge may raise it while held), a
// risky choice carries its word, and a key that answers is still active.
func (s *Service) stillAnswerable(ctx context.Context, tx *sql.Tx, me approverRow, prev, read orchapi.Ask, req orchapi.DecideRequest) error {
	if err := cardNotRead(req, prev); err != nil {
		return err
	}
	if prev.CardSHA != read.CardSHA {
		return api.Errorf(api.CodeConflict, "ask %s: its card changed while you answered; read it again", read.ID).
			WithData(orchapi.Refusal{Reason: orchapi.ReasonCardChanged})
	}
	if err := confirmRisk(prev, req); err != nil {
		return err
	}
	return s.keyStillActive(ctx, tx, me)
}

// storeRisk writes a held ask's card after raiseRisk (q: the registry or
// a transaction). Nobody has been asked yet, so nothing signed it.
func storeRisk(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, a orchapi.Ask) error {
	_, err := q.ExecContext(ctx, `UPDATE asks SET card = ?, card_sha = ?, question = ?, body = ? WHERE id = ?`,
		marshal(*a.Card), a.CardSHA, a.Subject.Question, a.Body, a.ID)
	return err
}

// riskText is what the card says about a risky action's choice.
func riskText(act *orchapi.AskAction) string {
	if act == nil || act.Risk == "" {
		return ""
	}
	why := "This cannot be undone"
	switch act.Risk {
	case orchapi.RiskSpending:
		why = "This spends money"
	case orchapi.RiskUnchecked:
		why = "Nobody checked whether this can be undone"
	}
	return fmt.Sprintf(" %s: to choose %s, type %s.", why, strings.ToUpper(act.On), act.ConfirmWord)
}

// confirmRisk refuses an answer that picks a risky action's choice
// without its word, typed exactly (spaces around it ignored).
func confirmRisk(a orchapi.Ask, req orchapi.DecideRequest) error {
	if a.Card == nil || a.Card.Action == nil || a.Card.Action.Risk == "" || req.Decision != a.Card.Action.On {
		return nil
	}
	act := a.Card.Action
	if act.ConfirmWord != "" && strings.TrimSpace(req.Confirm) == act.ConfirmWord {
		return nil
	}
	return api.Errorf(api.CodeConflict, "choice %s of ask %s is marked %s: type %s to choose it", strings.ToUpper(act.On), a.ID, act.Risk, first(act.ConfirmWord, "its word")).
		WithHint("send the word as confirm with the answer").WithData(orchapi.Refusal{Reason: orchapi.ReasonConfirmWord})
}
