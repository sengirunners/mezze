package approvals

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// The card judge (SPEC 16 "Card judge"): a model reads every note card
// before it is stored and answers three questions (cardjudge.md): can a
// person who does not code decide from it alone; does a choice leave them a
// step, and does the card's action then do all of it; and with no machine of
// theirs that takes tasks, does it carry a complete hand brief. A fail is
// refused with the judge's fixes as the hint. No answer (a timeout, no
// provider, a model error) never passes: the ask is stored held, with no
// mail, no event and no place in Decisions, and judged again until it
// passes (then it is sent) or fails (then it is withdrawn and its asker told
// the fixes).
//
// The same call also answers three things zm acts on: whether the card only reports or asks about status (sent
// back: a status is a report, not a decision); which of the project's owner
// rulings (rulings.go), if any, already answers it (sent back naming the
// ruling); and which open card of its area, if any, asks the same question
// (repeats.go: the ask joins that card, or is linked to it). The judge never
// tells an agent to proceed: a ruling's bounce quotes the ruling and nothing
// else.

//go:embed cardjudge.md
var cardJudgePrompt string

// cardJudgeSchema's "reasoning" comes first: a model that says why before
// it answers flips less between runs on the same card.
const cardJudgeSchema = `{"type":"object","properties":{
"reasoning":{"type":"string"},
"decidable":{"type":"boolean"},"person_step":{"type":"boolean"},"step_carried":{"type":"boolean"},
"brief_needed":{"type":"boolean"},"brief_complete":{"type":"boolean"},
"asks_decision":{"type":"boolean"},"ruled_by":{"type":"string"},"repeat_of":{"type":"string"},
"risk":{"type":"string","enum":["none","irreversible","spending"]},
"pass":{"type":"boolean"},
"fixes":{"type":"array","items":{"type":"string"}}},
"required":["reasoning","decidable","person_step","step_carried","brief_needed","brief_complete","asks_decision","ruled_by","repeat_of","risk","pass","fixes"],
"additionalProperties":false}`

// Tunables (tests change them).
var (
	// CardJudgeWait is how long an asker waits for a verdict before the ask
	// is stored held.
	CardJudgeWait = orchapi.CardJudgeWait
	// cardRejudgeWait bounds one judging of a held ask; maxRejudgeGap is
	// the longest pause between two.
	cardRejudgeWait = 2 * time.Minute
	maxRejudgeGap   = 30 * time.Minute
	// heldAlertAfter is how long a card waits on the judge before health says
	// so and the project's owners get one notice (the judge is down or keeps
	// splitting on it, not slow). ZM_JUDGE_NOTICE_AFTER (a Go duration, at
	// least a minute) sets it for a daemon.
	heldAlertAfter = 15 * time.Minute
)

// heldNoticeAfter is the wait before the owners' notice: the daemon's setting,
// else heldAlertAfter. An unreadable or shorter-than-a-minute setting is
// ignored, so a typo never silences it.
func heldNoticeAfter() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("ZM_JUDGE_NOTICE_AFTER"))); err == nil && d >= time.Minute {
		return d
	}
	return heldAlertAfter
}

var cardjudgeMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0010_card_judge", Script: `
CREATE TABLE ask_judging (
	ask      TEXT PRIMARY KEY,
	held     INTEGER NOT NULL,
	tries    INTEGER NOT NULL DEFAULT 0,
	next_try INTEGER NOT NULL,
	reason   TEXT NOT NULL DEFAULT '',
	passed   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ask_judging_due ON ask_judging (next_try);`},
	// A project whose owners were told cards are held past the notice time (judgeOutage).
	{Kind: store.KindRegistry, Name: "approvals/0011_card_judge_alert", Script: `
CREATE TABLE ask_judge_alert (
	project TEXT PRIMARY KEY,
	since   INTEGER NOT NULL,
	told    INTEGER NOT NULL
);`},
	// A card an approver released without the judge's check (releaseUnchecked):
	// who, from which key or device, when, and why it was held. Its default
	// never applies by itself.
	{Kind: store.KindRegistry, Name: "approvals/0012_card_judge_release", Script: `
CREATE TABLE ask_unchecked_release (
	ask      TEXT PRIMARY KEY,
	approver TEXT NOT NULL,
	node     TEXT NOT NULL,
	released INTEGER NOT NULL,
	reason   TEXT NOT NULL DEFAULT ''
);`},
	// operator: the held ask's asker may operate on its area, so it reads
	// every card there (openCards); 0, the default, shows it only its own.
	{Kind: store.KindRegistry, Name: "approvals/0014_card_judge_operator", Script: `
ALTER TABLE ask_judging ADD COLUMN operator INTEGER NOT NULL DEFAULT 0;`},
}

// notHeld is a WHERE clause part that keeps held asks (alias a) out.
const notHeld = ` AND NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id)`

// notReleased keeps out an ask an approver released without the judge's check:
// its default never applies by itself (SPEC 16 "Card judge").
const notReleased = ` AND NOT EXISTS (SELECT 1 FROM ask_unchecked_release r WHERE r.ask = a.id)`

// heldVisible is a WHERE clause part (asks alias a) that keeps out a held ask
// sc's reader may not see. A held ask is its asker's, and shows to who may
// operate on it (the project's owners) only while the judge has not checked
// it: a card that passed and waits to be sent is nobody's yet. In Decisions
// (people's lists) the asker has no special right: only who may operate.
func heldVisible(sc authz.Scope, me string, decisions bool) (string, []any) {
	operate, args := sc.SQL("a.project", "a.area", authz.Operate)
	if decisions {
		return ` AND NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id AND (j.passed = 1 OR NOT (` + operate + `)))`, args
	}
	return ` AND NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id AND a.asked_by != ? AND (j.passed = 1 OR NOT (` + operate + `)))`,
		append([]any{me}, args...)
}

// SetJudge replaces the model the card judge asks (tests); nil restores
// the configured provider CLI.
func (s *Service) SetJudge(c judge.Caller) {
	s.judgeMu.Lock()
	s.judgeC = c
	s.judgeMu.Unlock()
}

// caller is the model the judge asks: SetJudge's, else the configured
// provider CLI (judge.Default; a test binary's passes).
func (s *Service) caller() judge.Caller {
	s.judgeMu.Lock()
	c := s.judgeC
	s.judgeMu.Unlock()
	if c != nil {
		return c
	}
	return judge.Default(s.d.Config.File.Agents)
}

type judgeMachine struct {
	Machine string `json:"machine"`
	Ready   bool   `json:"ready"`
	WhyNot  string `json:"why_not,omitempty"`
}

type judgeCard struct {
	Title      string              `json:"title"`
	Context    string              `json:"context"`
	Background string              `json:"background,omitempty"`
	Question   string              `json:"question"`
	Choices    []orchapi.AskChoice `json:"choices"`
	Recommend  string              `json:"recommend"`
	Why        string              `json:"why"`
	AfterPress string              `json:"after_press,omitempty"`
	// FaceWords is what a person reads before More: the question, context,
	// each choice's label and effect, why and after_press (cardjudge.md).
	FaceWords int `json:"face_words"`
}

// faceWords counts the words of c a person reads before More.
func faceWords(c orchapi.AskCard) int {
	n := words(c.Question) + words(c.Context) + words(c.Why) + words(c.AfterPress)
	for _, ch := range c.Choices {
		n += words(ch.Label) + words(ch.Effect)
	}
	return n
}

type judgeAction struct {
	Kind    string `json:"kind"`
	On      string `json:"on"`
	Machine string `json:"machine,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Brief   string `json:"brief,omitempty"`
	Command string `json:"command,omitempty"`
	Script  string `json:"script,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Count   int    `json:"mails,omitempty"`
	Shown   string `json:"shown"` // the action as the card shows it
}

// judgeOpenCard is an open card of the new card's area, as the judge
// compares it (repeat_of).
type judgeOpenCard struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Context  string   `json:"context"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Action   string   `json:"action,omitempty"`
}

// judgeStep is a card's owner step as the judge reads it: the full link,
// and the domain a person's button shows.
type judgeStep struct {
	Label      string   `json:"label"`
	Steps      []string `json:"steps"`
	Link       string   `json:"link"`
	LinkDomain string   `json:"link_domain"`
	Minutes    int      `json:"minutes,omitempty"`
}

type cardJudgeInput struct {
	Approver         string          `json:"approver"`
	ApproverMachines []judgeMachine  `json:"approver_machines"`
	MachinesKnown    bool            `json:"machines_known"`
	Card             judgeCard       `json:"card"`
	Action           *judgeAction    `json:"action,omitempty"`
	OwnerStep        *judgeStep      `json:"owner_step,omitempty"`
	Verify           string          `json:"verify,omitempty"`
	Rulings          []Ruling        `json:"rulings,omitempty"`
	OpenCards        []judgeOpenCard `json:"open_cards,omitempty"`
}

// judgeInput is what the judge reads of a: the card as the person reads
// it, its action in full, the approver's machines, the project's rulings
// and the open cards a's asker may read (operator: it may operate on a's
// area).
func (s *Service) judgeInput(ctx context.Context, a orchapi.Ask, operator bool) cardJudgeInput {
	c := a.Card
	in := cardJudgeInput{Approver: "the project's owners (no one person is named)", ApproverMachines: []judgeMachine{},
		Card: judgeCard{Title: c.Title, Context: c.Context, Background: c.Background, Question: c.Question,
			Choices: c.Choices, Recommend: c.Recommend, Why: c.Why, AfterPress: c.AfterPress, FaceWords: faceWords(*c)}}
	if person := strings.TrimPrefix(a.Owner, "human:"); person != "" {
		in.Approver = person
		if l, err := s.runTargets(ctx, a.Project, person); err == nil {
			in.MachinesKnown = true
			for _, t := range l.Targets {
				in.ApproverMachines = append(in.ApproverMachines, judgeMachine{Machine: t.Machine, Ready: t.Ready, WhyNot: t.Why})
			}
		}
	}
	if act := c.Action; act != nil {
		in.Action = &judgeAction{Kind: act.Kind, On: act.On, Machine: act.Machine, Agent: act.Agent, Brief: act.Brief,
			Command: act.Command, Script: act.Script, Plan: act.Plan, Count: act.Count, Shown: actionText(act)}
	}
	if c.Verify != nil {
		in.Verify = c.Verify.Command
	}
	if st := c.OwnerStep; st != nil {
		in.OwnerStep = &judgeStep{Label: st.Label, Steps: st.Steps, Link: st.Link, LinkDomain: st.Domain, Minutes: st.Minutes}
	}
	for _, r := range s.rulings(a.Project) {
		in.Rulings = append(in.Rulings, Ruling{Name: r.Name, Text: r.Text}) // the judge reads what a ruling says, not where it came from
	}
	in.OpenCards = s.openCards(ctx, a, operator)
	return in
}

// judgeSlots bounds the cards judged at once: each is cardVotes CLI
// processes at the authority. Waiting for a slot counts against the wait.
const judgeSlots = 3

// cardResult is a card's verdict and the open card it repeats ("" for
// none). A held verdict repeats nothing. Firm: a fail because a ruling
// answers the card or it is a status report, which a repeat does not
// override.
type cardResult struct {
	judge.Verdict
	RepeatOf string
	Firm     bool
	// Risk is the action's risk the votes found ("" none): a majority's,
	// else the highest any answering vote named (voteRisk, risk.go).
	Risk string
}

func heldResult(reason string) cardResult {
	return cardResult{Verdict: judge.Verdict{Status: judge.Held, Reason: reason}}
}

// judgeCard asks the judge about a's card, waiting at most wait
// (operator: a's asker may operate on its area).
func (s *Service) judgeCard(ctx context.Context, a orchapi.Ask, wait time.Duration, operator bool) cardResult {
	start := time.Now()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.judging <- struct{}{}:
		defer func() { <-s.judging }()
	case <-t.C:
		return heldResult("card judge: busy with other cards")
	case <-ctx.Done():
		return heldResult("card judge: " + ctx.Err().Error())
	}
	left := wait - time.Since(start)
	if left <= time.Second {
		return heldResult("card judge: busy with other cards")
	}
	return cardVerdict(ctx, s.caller(), s.judgeInput(ctx, a, operator), left)
}

// cardVotes is how many times one card is judged, at once: a small model
// answers the same card differently now and then, and the majority's verdict
// is the card's. Each vote is one CLI process.
const cardVotes = 3

// cardVerdict is the majority of cardVotes votes on in, taken in parallel.
// Two passes pass and two fails fail (with the first failing vote's fixes);
// anything else (votes that split around a vote with no answer, or no
// answers) is held, never a pass. The open card it repeats is the one a
// majority of votes name; a majority for none is none, and votes that split
// with no majority either way hold it.
func cardVerdict(ctx context.Context, c judge.Caller, in cardJudgeInput, wait time.Duration) cardResult {
	votes := make([]cardResult, cardVotes)
	var wg sync.WaitGroup
	for i := range votes {
		wg.Go(func() { votes[i] = cardVote(ctx, c, in, wait) })
	}
	wg.Wait()
	vs := make([]judge.Verdict, len(votes))
	repeats := map[string]int{}
	firm := 0
	for i, v := range votes {
		vs[i] = v.Verdict
		if v.Status != judge.Held {
			repeats[v.RepeatOf]++
		}
		if v.Firm {
			firm++
		}
	}
	need := len(votes)/2 + 1
	r := cardResult{Verdict: tally(vs), Firm: firm >= need, Risk: voteRisk(votes, need)}
	if r.Status == judge.Held || (r.Status == judge.Fail && r.Firm) {
		return r // a ruling or status bounce stands whatever card it repeats
	}
	for id, n := range repeats {
		if n >= need {
			r.RepeatOf = id
			return r
		}
	}
	return heldResult(fmt.Sprintf("card judge: its votes split on which open card this one repeats (%d answers)", len(repeats)))
}

// voteRisk is the action's risk the answering votes name: the one need of
// them agree on, as pass and fail are decided, else (a split: unknown) the
// highest any names, so a split raises it, never lowers it.
func voteRisk(votes []cardResult, need int) string {
	n := map[string]int{}
	high := ""
	for _, v := range votes {
		if v.Status == judge.Held {
			continue
		}
		n[v.Risk]++
		if riskRank(v.Risk) > riskRank(high) {
			high = v.Risk
		}
	}
	for r, c := range n {
		if c >= need {
			return r
		}
	}
	return high
}

func tally(votes []judge.Verdict) judge.Verdict {
	n := map[judge.Status]int{}
	first := map[judge.Status]judge.Verdict{}
	for _, v := range votes {
		if n[v.Status]++; n[v.Status] == 1 {
			first[v.Status] = v
		}
	}
	switch need := len(votes)/2 + 1; {
	case n[judge.Pass] >= need:
		return first[judge.Pass]
	case n[judge.Fail] >= need:
		return first[judge.Fail]
	case n[judge.Held] >= need:
		return first[judge.Held]
	}
	return judge.Verdict{Status: judge.Held, Reason: fmt.Sprintf("card judge: its %d votes split (%d pass, %d fail, %d no answer: %s)",
		len(votes), n[judge.Pass], n[judge.Fail], n[judge.Held], first[judge.Held].Reason)}
}

// cardVote asks c to judge in once. The verdict is the producer's rule over
// the model's three answers, not the model's own "pass": a pass whose
// answers leave a gap is a fail, and a "fail" whose answers have none is a
// pass (its fixes, then, are about nothing the card must change).
func cardVote(ctx context.Context, c judge.Caller, in cardJudgeInput, wait time.Duration) cardResult {
	j := judge.Judge{Name: "card judge", Prompt: cardJudgePrompt, Schema: json.RawMessage(cardJudgeSchema), Timeout: wait}
	v := j.Run(ctx, c, in)
	if v.Status == judge.Held {
		return cardResult{Verdict: v}
	}
	var ans cardAnswers
	notShape := func() cardResult {
		return cardResult{Verdict: judge.Verdict{Status: judge.Held, Reason: "the card judge's answer is not the verdict's shape", Raw: v.Raw}}
	}
	if err := json.Unmarshal(v.Raw, &ans); err != nil || ans.Decidable == nil || ans.AsksDecision == nil || ans.RuledBy == nil || ans.RepeatOf == nil || ans.Risk == nil {
		return notShape()
	}
	risk := ""
	switch bare(*ans.Risk) {
	case "none":
	case orchapi.RiskIrreversible, orchapi.RiskSpending:
		risk = bare(*ans.Risk)
	default:
		return notShape() // outside the schema's world: held, never read as none
	}
	// A ruling or open card the input does not hold is an answer outside
	// the schema's world: held, never acted on.
	var ruling *Ruling
	if name := bare(*ans.RuledBy); name != "" {
		i := slices.IndexFunc(in.Rulings, func(r Ruling) bool { return r.Name == name })
		if i < 0 {
			return notShape()
		}
		ruling = &in.Rulings[i]
	}
	repeat := bare(*ans.RepeatOf)
	if repeat != "" && !slices.ContainsFunc(in.OpenCards, func(o judgeOpenCard) bool { return o.ID == repeat }) {
		return notShape()
	}
	verdict, firm := cardRule(in, v, ans, ruling)
	if in.Action == nil {
		risk = "" // only an action has a choice to guard
	}
	return cardResult{Verdict: verdict, RepeatOf: repeat, Firm: firm, Risk: risk}
}

// bare is a name or id the model answered, without the spaces or quote
// marks a model sometimes wraps it in (an empty answer written as "").
func bare(s string) string { return strings.Trim(s, " \t\n\"'`") }

// cardAnswers is one vote's answers (cardJudgeSchema); the pointers are
// the fields a missing value must not read as false.
type cardAnswers struct {
	Decidable     *bool   `json:"decidable"`
	PersonStep    bool    `json:"person_step"`
	StepCarried   bool    `json:"step_carried"`
	BriefNeeded   bool    `json:"brief_needed"`
	BriefComplete bool    `json:"brief_complete"`
	AsksDecision  *bool   `json:"asks_decision"`
	RuledBy       *string `json:"ruled_by"`
	RepeatOf      *string `json:"repeat_of"`
	Risk          *string `json:"risk"`
}

// cardRule is the producer's rule over one vote's answers (ruling: the
// one it names, checked against the input); firm when a ruling or a status
// report failed it.
func cardRule(in cardJudgeInput, v judge.Verdict, ans cardAnswers, ruling *Ruling) (judge.Verdict, bool) {
	// A ruling or a status report sends the card back whatever else it
	// says: the person would only repeat the ruling, or has nothing to
	// decide. These fixes are zm's own, so the bounce names the ruling and
	// quotes it, and says nothing more (never "go ahead").
	var firm []string
	if ruling != nil {
		firm = append(firm, fmt.Sprintf("the owners' standing ruling %q already answers this question, so the card was not sent: %q. If your case is outside that ruling, say in the card exactly how it differs", ruling.Name, ruling.Text))
	}
	if !*ans.AsksDecision {
		firm = append(firm, "this card reports or asks about a status (whether something works, ran or finished), which is not a decision for a person: check it yourself, and put a status in a report or a comment on the work item, not a card")
	}
	if len(firm) > 0 {
		return judge.Verdict{Status: judge.Fail, Fixes: firm, Raw: v.Raw}, true
	}
	var fixes []string // the rule's own, for a model that named none
	if !*ans.Decidable {
		fixes = append(fixes, "a person who does not code could not decide from this card alone: say what it is, why it matters, what each choice changes and its cost or risk, in plain words")
	}
	if ans.PersonStep && !ans.StepCarried {
		fixes = append(fixes, "a choice leaves the person a step: carry the whole step as the card's action")
	}
	if ans.BriefNeeded && !ans.BriefComplete {
		// Whether a machine of theirs takes tasks is zm's fact, not the model's.
		if slices.ContainsFunc(in.ApproverMachines, func(m judgeMachine) bool { return m.Ready }) {
			fixes = append(fixes, "the action's brief does not reach done: number its steps to done, with any one-time setup, plus --verify")
		} else {
			fixes = append(fixes, "the approver has no machine that takes tasks: carry a complete numbered brief (--hand --agent claude --brief <file>) with any one-time setup, plus --verify")
		}
	}
	if len(fixes) == 0 {
		return judge.Verdict{Status: judge.Pass, Raw: v.Raw}, false
	}
	if v.Status == judge.Fail && !slices.Equal(v.Fixes, []string{judge.NoFixNamed}) {
		fixes = v.Fixes // the model's own, specific to this card
	}
	return judge.Verdict{Status: judge.Fail, Fixes: fixes, Raw: v.Raw}, false
}

// cardRefused is the refusal of a card the judge failed: its fixes, and a
// hint naming the action flags and the approver's machines.
func (s *Service) cardRefused(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, named string, v judge.Verdict) error {
	return api.Errorf(api.CodeBadRequest, "the card judge sent this card back: %s", numbered(v.Fixes)).
		WithHint("rewrite the card and ask again; " + s.actionHintFor(ctx, sub, a, named))
}

func numbered(fixes []string) string {
	var b strings.Builder
	for i, f := range fixes {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, f)
	}
	return b.String()
}

// heldNote is what the asker of a held ask is told.
func heldNote(reason string) string {
	return "held, not sent: the card judge did not answer (" + reason + "). zm judges it again and it reaches the person only after a pass; if it fails, zm withdraws it and mails you the fixes. It shows to the project's owners as not checked, and one can release it from a paired device or key"
}

// holdAsk records ask id as held, before its row exists (so it is never
// visible unheld). The caller drops it if the ask is not stored.
func (s *Service) holdAsk(ctx context.Context, id, reason string, operator bool) error {
	now := s.now()
	_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO ask_judging (ask, held, next_try, reason, operator) VALUES (?, ?, ?, ?, ?)`,
		id, ms(now), ms(now.Add(retryTick)), reason, operator)
	return err
}

func (s *Service) unhold(ctx context.Context, id string) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM ask_judging WHERE ask = ?`, id)
	return err
}

// heldReason is why ask id is held, and whether it is.
func (s *Service) heldReason(ctx context.Context, id string) (string, bool) {
	var reason string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT reason FROM ask_judging WHERE ask = ?`, id).Scan(&reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "card judge: held state unreadable: " + err.Error(), true // unreadable: held, never decided or joined
	}
	return reason, err == nil
}

func (s *Service) isHeld(ctx context.Context, id string) bool {
	_, held := s.heldReason(ctx, id)
	return held
}

// markChecked fills a's NotChecked fields while the judge has not checked it
// (a held ask that passed is only waiting to be sent), and ReleasedUnchecked
// once an approver released it without the check.
func (s *Service) markChecked(ctx context.Context, a *orchapi.Ask) {
	var reason, by sql.NullString
	var held, passed sql.NullInt64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT j.reason, j.held, j.passed, r.approver FROM asks a
		LEFT JOIN ask_judging j ON j.ask = a.id LEFT JOIN ask_unchecked_release r ON r.ask = a.id WHERE a.id = ?`, a.ID).Scan(&reason, &held, &passed, &by)
	if err != nil {
		return
	}
	if held.Valid && passed.Int64 == 0 {
		since := time.UnixMilli(held.Int64).UTC()
		a.NotChecked, a.NotCheckedReason, a.NotCheckedSince = true, reason.String, &since
	}
	a.ReleasedUnchecked = by.String
}

// releasedUnchecked: an approver released ask id without the judge's check.
func (s *Service) releasedUnchecked(ctx context.Context, id string) bool {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM ask_unchecked_release WHERE ask = ?`, id).Scan(&n)
	return err != nil || n > 0 // unreadable: treated as released, so it is never joined
}

// errHeldAnswer is errHeld as an answer's refusal: reason held. (Joining a held
// ask, POST /v0/asks with same_as, is errHeld alone: its 409 data is an Ask.)
func errHeldAnswer(id string) *api.Error {
	return errHeld(id).WithData(orchapi.Refusal{Reason: orchapi.ReasonHeld})
}

// errHeld refuses deciding or joining a held ask: nobody has been asked yet.
func errHeld(id string) *api.Error {
	return api.Errorf(api.CodeConflict, "ask %s is held: the card judge has not passed it yet, so nobody has been asked", id).
		WithHint("an owner releases a card the judge could not check with a signed answer: a paired device or a key approver sends " + orchapi.DecisionReleaseUnchecked)
}

// heldUnchecked is a held ask's try count when the judge has not checked it.
func (s *Service) heldUnchecked(ctx context.Context, id string) (tries int, ok bool) {
	var passed int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT tries, passed FROM ask_judging WHERE ask = ?`, id).Scan(&tries, &passed); err != nil {
		return 0, false
	}
	return tries, passed == 0
}

// isRelease: decision is the release token and ask id is held, or was released
// that way (a repeat answers the same). A held ask can be answered no other
// way, so a card whose own choice is named so is untouched while it is sent.
func (s *Service) isRelease(ctx context.Context, id, decision string) bool {
	if strings.ToLower(strings.TrimSpace(decision)) != orchapi.DecisionReleaseUnchecked {
		return false
	}
	id = strings.ToUpper(strings.TrimSpace(id))
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM ask_judging WHERE ask = ?) + (SELECT count(*) FROM ask_unchecked_release WHERE ask = ?)`, id, id).Scan(&n)
	return err == nil && n > 0
}

// releaseUnchecked is an approver's signed release of a held ask the judge
// has not checked: it is sent to the people it is for as a passed card is
// (release), and the release is recorded with who did it. It is never zm's
// own call, and its default never applies by itself (applyDefaults). Under
// holdMu, so the judge's own answer, a withdrawal or a second release is
// ordered against it: the judge failing the card afterwards cannot withdraw
// it (rejectHeld). Releasing a released ask again is the same answer.
func (s *Service) releaseUnchecked(ctx context.Context, id string, me approverRow) (orchapi.Ask, error) {
	ctx = context.WithoutCancel(ctx)
	s.holdMu.Lock()
	a, err := s.ask(ctx, id)
	if err != nil {
		s.holdMu.Unlock()
		return a, err
	}
	if a.Card == nil || a.Subject.IsPR() {
		s.holdMu.Unlock()
		return a, api.Errorf(api.CodeConflict, "ask %s is not a card the judge checks", a.ID)
	}
	tries, unchecked := s.heldUnchecked(ctx, a.ID)
	switch {
	case a.Withdrawn != nil:
		s.holdMu.Unlock()
		return a, api.Errorf(api.CodeConflict, "ask %s was withdrawn", a.ID)
	case a.Decision != nil:
		s.holdMu.Unlock()
		return a, api.Errorf(api.CodeConflict, "ask %s was already decided", a.ID)
	case !unchecked && s.isHeld(ctx, a.ID):
		s.holdMu.Unlock()
		return a, api.Errorf(api.CodeConflict, "ask %s passed the card judge and is being sent", a.ID).WithHint("zm sends it on its next try")
	case !unchecked:
		s.holdMu.Unlock()
		s.markChecked(ctx, &a)
		if a.ReleasedUnchecked != "" {
			return a, nil // released before: the same answer
		}
		return a, api.Errorf(api.CodeConflict, "ask %s is not held", a.ID)
	}
	reason, _ := s.heldReason(ctx, a.ID)
	now := ms(s.now())
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE ask_judging SET passed = 1, next_try = ? WHERE ask = ?`, now, a.ID); err != nil {
			return err
		}
		// Its default never applies by itself: the card stops promising one.
		if _, err := tx.ExecContext(ctx, `UPDATE asks SET apply_default = 0 WHERE id = ?`, a.ID); err != nil {
			return err
		}
		// Nobody judged its action's risk: the choice takes the typed word.
		if s.raiseRisk(&a, orchapi.RiskUnchecked) {
			if err := storeRisk(ctx, tx, a); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ask_unchecked_release (ask, approver, node, released, reason) VALUES (?, ?, ?, ?, ?)`,
			a.ID, me.Name, me.Node, now, reason)
		return err
	})
	s.holdMu.Unlock()
	if err != nil {
		return a, err
	}
	s.d.Log.Warn("approvals: an approver released a card the judge had not checked", "ask", a.ID, "approver", me.Name, "node", me.Node, "reason", reason)
	s.release(ctx, a.ID, tries)
	if a, err = s.ask(ctx, a.ID); err == nil {
		s.markChecked(ctx, &a)
	}
	return a, err
}

// rejudgeInBackground starts one sweep of held asks unless one is running:
// a judge call can take minutes, which must not stall the retry ticker's
// other work (deliveries, defaults, actions).
func (s *Service) rejudgeInBackground(ctx context.Context) {
	if !s.rejudging.CompareAndSwap(false, true) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.rejudging.Store(false)
		s.rejudgeHeld(ctx)
	}()
}

// rejudgeHeld judges every held ask whose next try is due: a pass sends it,
// a fail withdraws it and tells its asker, no answer waits longer. One that
// passed but whose mail did not send is only sent again.
func (s *Service) rejudgeHeld(ctx context.Context) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT ask, tries, passed, operator FROM ask_judging WHERE next_try <= ? ORDER BY next_try LIMIT 10`, ms(s.now()))
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("approvals: read held asks", "err", err)
		}
		return
	}
	type due struct {
		id       string
		tries    int
		passed   bool
		operator bool
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tries, &d.passed, &d.operator); err == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		if ctx.Err() != nil {
			return
		}
		a, err := s.ask(ctx, d.id)
		if api.IsCode(err, api.CodeNotFound) || (err == nil && (a.Card == nil || a.Withdrawn != nil || a.Decision != nil)) {
			_ = s.unhold(ctx, d.id) // gone or withdrawn: nothing to send
			continue
		}
		if err != nil {
			continue // the next tick tries again
		}
		if d.passed {
			s.releaseJoined(ctx, a, d.tries)
			continue
		}
		v := s.judgeCard(ctx, a, cardRejudgeWait, d.operator)
		// The risk first, so a join compares the card's final action.
		if ok, err := s.raiseHeldRisk(ctx, &a, v.Risk); err != nil {
			s.retryHeld(ctx, a.ID, d.tries, false, "its risk was not stored: "+err.Error())
			continue
		} else if !ok {
			continue // released, answered, withdrawn or changed while it was judged: that stands
		}
		if v.RepeatOf != "" {
			root, err := s.ask(ctx, v.RepeatOf)
			if err == nil && joinsRepeat(a, root, v) && s.joinHeld(ctx, a, root.ID, d.tries, d.operator) {
				continue
			}
			if err == nil && v.Status == judge.Pass {
				s.link(ctx, a.ID, root.ID)
			}
		}
		switch v.Status {
		case judge.Pass:
			if f := s.routedOnRejudge(ctx, a); f != nil {
				if err := s.holdForRouted(ctx, a, *f); err != nil {
					s.d.Log.Warn("approvals: hold a passed ask for its outside party", "ask", a.ID, "err", err)
				}
			}
			s.release(ctx, a.ID, d.tries)
		case judge.Fail:
			s.rejectHeld(ctx, a.ID, v.Verdict)
		default:
			s.retryHeld(ctx, a.ID, d.tries, false, v.Reason)
		}
	}
	s.judgeOutage(ctx)
}

// judgeOutage tells each project's owners once when a card of theirs has
// waited on the judge over heldNoticeAfter (15 minutes unless the daemon
// says otherwise), and forgets it when none has (the judge answers again), so
// a later outage is told again. A card held
// after it passed (its mail did not send) is not the judge's.
func (s *Service) judgeOutage(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	reg := s.d.Store.Registry()
	after := heldNoticeAfter()
	type hold struct {
		n      int
		since  int64
		reason string // the oldest's (SQLite's row for min())
	}
	held := map[string]hold{}
	rows, err := reg.QueryContext(ctx, `SELECT a.project, count(*), min(j.held), j.reason FROM ask_judging j JOIN asks a ON a.id = j.ask
		WHERE j.passed = 0 AND j.held <= ? GROUP BY a.project`, ms(s.now().Add(-after)))
	if err != nil {
		s.d.Log.Warn("approvals: read held asks for the judge alert", "err", err)
		return
	}
	for rows.Next() {
		var p string
		var h hold
		if rows.Scan(&p, &h.n, &h.since, &h.reason) == nil {
			held[p] = h
		}
	}
	rows.Close()
	told := map[string]bool{}
	if rows, err := reg.QueryContext(ctx, `SELECT project FROM ask_judge_alert`); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				told[p] = true
			}
		}
		rows.Close()
	}
	for p := range told {
		if _, still := held[p]; !still {
			if _, err := reg.ExecContext(ctx, `DELETE FROM ask_judge_alert WHERE project = ?`, p); err == nil {
				s.d.Log.Info("approvals: no card is held past the notice time by the card judge now", "project", p)
			}
		}
	}
	for p, h := range held {
		if told[p] {
			continue
		}
		since := time.UnixMilli(h.since).UTC()
		s.d.Log.Error("approvals: ask cards held past the notice time by the card judge", "project", p, "held", h.n, "since", since, "reason", h.reason)
		ref := api.ProjectRef{ID: p, Slug: s.d.Projects.Slug(p)}
		err := s.d.Notify(ctx, core.Notice{Project: ref, To: "owners", Body: judgeOutageNote(ref.Slug, s.d.Config.Machine, h.n, since, h.reason, after)})
		if err != nil && !errors.Is(err, core.ErrNoNotifier) {
			s.d.Log.Warn("approvals: tell owners cards are held by the judge", "project", p, "err", err)
			continue // told on the next sweep
		}
		if _, err := reg.ExecContext(ctx, `INSERT OR IGNORE INTO ask_judge_alert (project, since, told) VALUES (?, ?, ?)`, p, h.since, ms(s.now())); err != nil {
			s.d.Log.Warn("approvals: record the card judge alert", "project", p, "err", err)
		}
	}
}

func judgeOutageNote(project, machine string, n int, since time.Time, reason string, after time.Duration) string {
	return fmt.Sprintf("Hello,\n\nzm checks every decision card with a small AI model before it reaches you, so a thin card is sent back to the agent that wrote it. "+
		"%d card(s) for %s have been held for over %d minutes on %s and have not been sent to you, the oldest since %s. Nothing is lost: each one is sent as soon as the check passes it. "+
		"They are listed in Decisions now, marked not checked; if one cannot wait, release it there from a paired device or key and it goes out as it is, unchecked.\n\n"+
		"Why the oldest is held: %s\n\n"+
		"A held card means the check could not give a clear answer: it may not be running (the claude command on %s, installed and signed in to your plan), or it keeps giving split answers on that card. "+
		"Running zm status on %s shows this too. You will not get this note again until the held cards have cleared and one is held again.\n\nzm",
		n, project, int(after.Minutes()), machine, since.Format("Jan 2 15:04 UTC"), reason, machine, machine)
}

// judgeHealth is health's card_judge section: the cards held over
// heldNoticeAfter, or nil.
func (s *Service) judgeHealth(ctx context.Context) any {
	var h orchapi.CardJudgeHold
	var since sql.NullInt64
	var reason sql.NullString
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*), min(held), reason FROM ask_judging WHERE passed = 0 AND held <= ?`,
		ms(s.now().Add(-heldNoticeAfter()))).Scan(&h.Held, &since, &reason); err != nil || h.Held == 0 {
		return nil
	}
	h.Since, h.Reason = time.UnixMilli(since.Int64).UTC(), reason.String
	return &h
}

// retryHeld puts a held ask's next try further out (doubling, at most
// maxRejudgeGap); passed keeps a pass, so the next try only sends.
func (s *Service) retryHeld(ctx context.Context, id string, tries int, passed bool, reason string) {
	gap := min(retryTick<<min(tries+1, 10), maxRejudgeGap)
	s.d.Log.Warn("approvals: ask held, the card judge did not pass it", "ask", id, "tries", tries+1, "reason", reason, "next_try_in", gap)
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_judging SET tries = tries + 1, next_try = ?, reason = ?, passed = max(passed, ?) WHERE ask = ?`,
		ms(s.now().Add(gap)), reason, passed, id); err != nil {
		s.d.Log.Warn("approvals: reschedule a held ask", "ask", id, "err", err)
	}
}

// release sends a held ask that passed, as handleAsk sends one: its mail
// from its asker, then its event. Under holdMu, so a withdrawal either comes
// first (nothing is sent) or finds it sent. A deadline that passed while it
// was held moves out by the window its asker gave, so the person still gets
// that time. A failed send keeps it held, passed, for the next try.
//
// The mail goes out as the asker with the authority's own scope: the asker
// passed every check when it asked (handleAsk), and nothing it asked
// changes while held.
func (s *Service) release(ctx context.Context, id string, tries int) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	a, err := s.ask(ctx, id)
	if err != nil || a.Withdrawn != nil || a.Decision != nil || !s.isHeld(ctx, id) {
		return
	}
	if a.SameAs != "" { // joined while held (joinHeld): never sent behind an answered group
		root, err := s.ask(ctx, a.SameAs)
		if err != nil {
			s.retryHeld(ctx, a.ID, tries, true, "passed; its first ask could not be read: "+err.Error())
			return
		}
		if root.Decision != nil || root.Withdrawn != nil {
			s.dropJoined(ctx, a, root)
			return
		}
	}
	if now := s.now(); a.By != nil && !a.By.After(now) {
		by := now.Add(a.By.Sub(a.Created)).Truncate(time.Millisecond)
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET by_ms = ? WHERE id = ?`, ms(by), a.ID); err != nil {
			s.retryHeld(ctx, a.ID, tries, true, "passed; its deadline did not move: "+err.Error())
			return
		}
		a.By = &by
	}
	ref := api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}
	m, err := orchcore.Mail(s.d).Send(ctx, s.d.Projects.Local(a.AskedBy), ref, a.Area,
		orchapi.SendMailRequest{To: a.To, Body: a.Body, Needs: orchapi.NeedsHuman, ID: a.ID})
	if err != nil {
		s.d.Log.Warn("approvals: send a held ask that passed", "ask", a.ID, "err", err)
		s.retryHeld(ctx, a.ID, tries, true, "passed; its mail did not send: "+err.Error())
		return
	}
	if m.Area != a.Area || m.To != a.To {
		a.Area, a.To = m.Area, m.To
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET area = ?, to_addr = ? WHERE id = ?`, a.Area, a.To, a.ID); err != nil {
			s.d.Log.Warn("approvals: align ask with its mail", "ask", a.ID, "err", err)
		}
	}
	if err := s.unhold(ctx, a.ID); err != nil {
		s.d.Log.Warn("approvals: unhold a sent ask", "ask", a.ID, "err", err)
	}
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: orchapi.EventAskCreated, Subject: a.ID,
		Project: a.Project, Area: a.Area, Data: a, Mentions: []string{a.AskedBy},
		Audience: &events.Audience{Need: authz.Operate, Parties: []string{a.AskedBy, a.To}}}); err != nil {
		s.d.Log.Warn("approvals: publish ask", "ask", a.ID, "err", err)
	}
}

// rejectHeld withdraws a held ask the judge failed and mails its asker (an
// agent) the fixes. Nobody else was ever asked, so nobody else is told; an
// ask withdrawn meanwhile is left as it is.
func (s *Service) rejectHeld(ctx context.Context, id string, v judge.Verdict) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	a, err := s.ask(ctx, id)
	if _, unchecked := s.heldUnchecked(ctx, id); err != nil || !unchecked { // released meanwhile: an approver's call stands
		return
	}
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET withdrawn = ?, dedupe = '' WHERE id = ? AND withdrawn IS NULL`, ms(s.now()), a.ID)
	if err != nil {
		s.d.Log.Warn("approvals: withdraw a held ask that failed", "ask", a.ID, "err", err)
		return
	}
	if err := s.unhold(ctx, a.ID); err != nil {
		s.d.Log.Warn("approvals: unhold a failed ask", "ask", a.ID, "err", err)
	}
	if n, _ := res.RowsAffected(); n == 0 || !orchapi.AgentSender(a.AskedBy) {
		return
	}
	text := fmt.Sprintf("Ask %s (%s) was not sent: the card judge sent it back, and zm withdrew it. Rewrite the card and ask again: %s",
		a.ID, quoted(a.Card.Question), numbered(v.Fixes))
	if _, err := orchcore.Mail(s.d).Send(orchcore.AsAskNotice(ctx), s.d.Projects.Local(""), api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)},
		a.Area, orchapi.SendMailRequest{To: a.AskedBy, Body: text}); err != nil {
		s.d.Log.Warn("approvals: tell the asker a held ask failed", "ask", a.ID, "err", err)
	}
}
