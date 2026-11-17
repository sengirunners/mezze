package approvals

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// The step judge (SPEC 16 "Person steps"): before a person step is stored, a
// model reads it (stepjudge.md, with its eval cases in testdata/stepjudge) and
// answers a fixed schema: does it need the person, can their own agent do all
// of it, is the brief numbered to done and self-contained, does it leave no
// real choice, does the check prove it done, is the title plain, and what is
// its risk. Meaning is the model's; the verdict is the producer's rule over
// those answers (stepRule), never the model's own "pass" alone. No answer is
// never a pass: the add is refused as unavailable.

//go:embed stepjudge.md
var stepJudgePrompt string

// stepJudgeSchema: every field required; "reasoning" first, so the model says
// why before it answers. Each boolean is true when the step is fit on that
// count (a test's passing fake answers every boolean true).
const stepJudgeSchema = `{"type":"object","properties":{
"reasoning":{"type":"string"},
"needs_the_person":{"type":"boolean"},"agent_doable":{"type":"boolean"},
"numbered_to_done":{"type":"boolean"},"self_contained":{"type":"boolean"},
"no_real_choice":{"type":"boolean"},"check_proves_done":{"type":"boolean"},"title_plain":{"type":"boolean"},
"risk":{"type":"string","enum":["none","irreversible","spending"]},
"pass":{"type":"boolean"},
"fixes":{"type":"array","items":{"type":"string"}}},
"required":["reasoning","needs_the_person","agent_doable","numbered_to_done","self_contained","no_real_choice","check_proves_done","title_plain","risk","pass","fixes"],
"additionalProperties":false}`

// StepJudgeWait bounds one step judging: the adder waits for it.
var StepJudgeWait = 40 * time.Second

// stepVotes: each step is judged this many times at once and the majority's
// verdict is the step's (a small model answers the same text differently now
// and then), as the card judge does.
const stepVotes = 3

// stepJudgeInput is what the step judge reads.
type stepJudgeInput struct {
	Person string `json:"person"`
	Title  string `json:"title"`
	Brief  string `json:"brief"`
	Check  string `json:"check"`
}

// stepAnswers is one vote's answers; pointers so a missing one is never read
// as false or true.
type stepAnswers struct {
	NeedsThePerson  *bool   `json:"needs_the_person"`
	AgentDoable     *bool   `json:"agent_doable"`
	NumberedToDone  *bool   `json:"numbered_to_done"`
	SelfContained   *bool   `json:"self_contained"`
	NoRealChoice    *bool   `json:"no_real_choice"`
	CheckProvesDone *bool   `json:"check_proves_done"`
	TitlePlain      *bool   `json:"title_plain"`
	Risk            *string `json:"risk"`
}

// stepResult is the step judge's verdict and the risk its votes name.
type stepResult struct {
	judge.Verdict
	Risk string // none, irreversible or spending
}

// decisionFix is the first fix of a step the judge read a real choice in: it
// is a decision, which files its own card.
const decisionFix = "it leaves the person a real choice, so it is a decision, not a step: file its own ask (zm ask new --to human:<name> with choices and a recommendation)"

// stepFixes name, for each answer that is not fit, what to change: the
// producer's own words, so a fail always says why even when the model's
// fixes are thin.
var stepFixes = []struct {
	field func(stepAnswers) *bool
	fix   string
}{
	{func(a stepAnswers) *bool { return a.NoRealChoice }, decisionFix},
	{func(a stepAnswers) *bool { return a.NeedsThePerson }, "it does not need this person's machine or accounts: do it yourself and report that it is done"},
	{func(a stepAnswers) *bool { return a.AgentDoable }, "their agent cannot do all of it on their machine: take out what needs the person's own hands, or file an ask with an owner step (zm ask new --step-label)"},
	{func(a stepAnswers) *bool { return a.NumberedToDone }, "number the brief's steps all the way to done, one-time setup included"},
	{func(a stepAnswers) *bool { return a.SelfContained }, "say in the brief where every link, version, invite or value comes from; an agent that knows nothing of your work must be able to follow it"},
	{func(a stepAnswers) *bool { return a.CheckProvesDone }, "write a check line that exits 0 only once the step's result is true, read from where you can see it (the service's API or CLI, or zm)"},
	{func(a stepAnswers) *bool { return a.TitlePlain }, "make the title one plain line a person who does not code understands: what gets done, no ids, paths or commands"},
}

// judgeStep is the step judge's majority verdict on in, within wait.
func (s *Service) judgeStep(ctx context.Context, in stepJudgeInput, wait time.Duration) stepResult {
	return stepVerdict(ctx, s.caller(), in, wait)
}

// stepVerdict takes stepVotes votes on in at once: two passes pass, two
// fails fail (the first failing vote's fixes), anything else is held. The
// risk is the highest any answering vote names: a split raises it, never
// lowers it.
func stepVerdict(ctx context.Context, c judge.Caller, in stepJudgeInput, wait time.Duration) stepResult {
	votes := make([]stepResult, stepVotes)
	var wg sync.WaitGroup
	for i := range votes {
		wg.Go(func() { votes[i] = stepVote(ctx, c, in, wait) })
	}
	wg.Wait()
	vs := make([]judge.Verdict, len(votes))
	risk := "none"
	for i, v := range votes {
		vs[i] = v.Verdict
		if v.Status != judge.Held && riskRank(bareRisk(v.Risk)) > riskRank(bareRisk(risk)) {
			risk = v.Risk
		}
	}
	r := stepResult{Verdict: tally(vs), Risk: risk}
	if r.Status == judge.Held {
		r.Reason = "step judge: " + strings.TrimPrefix(r.Reason, "card judge: ") // tally names the card judge
	}
	return r
}

// bareRisk is risk as riskRank reads it ("none" is no risk).
func bareRisk(r string) string {
	if r == "none" {
		return ""
	}
	return r
}

// stepVote asks c to judge in once.
func stepVote(ctx context.Context, c judge.Caller, in stepJudgeInput, wait time.Duration) stepResult {
	j := judge.Judge{Name: "step judge", Prompt: stepJudgePrompt, Schema: json.RawMessage(stepJudgeSchema), Timeout: wait}
	v := j.Run(ctx, c, in)
	if v.Status == judge.Held {
		return stepResult{Verdict: v}
	}
	var ans stepAnswers
	if json.Unmarshal(v.Raw, &ans) != nil || !ans.complete() {
		return stepResult{Verdict: judge.Verdict{Status: judge.Held, Reason: "the step judge's answer is not the verdict's shape", Raw: v.Raw}}
	}
	return stepResult{Verdict: stepRule(v, ans), Risk: bare(*ans.Risk)}
}

// complete reports whether every answer is there, with a risk inside the
// schema's world (anything else is held, never read as none).
func (a stepAnswers) complete() bool {
	for _, f := range stepFixes {
		if f.field(a) == nil {
			return false
		}
	}
	if a.Risk == nil {
		return false
	}
	switch bare(*a.Risk) {
	case "none", orchapi.RiskIrreversible, orchapi.RiskSpending:
		return true
	}
	return false
}

// stepRule is the producer's rule over one vote: a pass only when the model
// passed it and every answer is fit; any answer that is not fit fails it,
// with its fix first (a real choice always leads: it files its own ask).
// A "fail" whose answers are all fit stays a fail, with the model's fixes.
func stepRule(v judge.Verdict, ans stepAnswers) judge.Verdict {
	var fixes []string
	for _, f := range stepFixes {
		if !*f.field(ans) {
			fixes = append(fixes, f.fix)
		}
	}
	if v.Status == judge.Pass && len(fixes) == 0 {
		return v
	}
	out := judge.Verdict{Status: judge.Fail, Reason: v.Reason, Raw: v.Raw}
	for _, f := range append(fixes, v.Fixes...) {
		if f != judge.NoFixNamed && len(out.Fixes) < judge.MaxFixes {
			out.Fixes = append(out.Fixes, f)
		}
	}
	if len(out.Fixes) == 0 {
		out.Fixes = []string{judge.NoFixNamed}
	}
	return out
}

// isDecision reports whether a failed verdict is the judge reading a real
// choice in the step.
func isDecision(v judge.Verdict) bool {
	return v.Status == judge.Fail && len(v.Fixes) > 0 && v.Fixes[0] == decisionFix
}
