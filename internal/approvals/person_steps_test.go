package approvals_test

// Person steps (SPEC 16 "Person steps"): a step for one person waits at the
// authority until their daily steps card carries it; the step judge reads it
// first; the person's agent reports it and only its adder's check makes it
// done.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// stepJudge answers the step judge with fit answers, but for the ones the
// test sets; every other judge (the card judge) passes.
type stepJudge struct {
	mu     sync.Mutex
	answer any    // map[string]any overrides, or error
	during func() // runs inside every step-judge call, while set
	calls  atomic.Int32
}

func (j *stepJudge) fn(fallback func(string) (json.RawMessage, error)) func(string) (json.RawMessage, error) {
	return func(prompt string) (json.RawMessage, error) {
		if !strings.Contains(prompt, "daily steps card") {
			return fallback(prompt)
		}
		j.calls.Add(1)
		ans := map[string]any{"reasoning": "a step", "needs_the_person": true, "agent_doable": true, "numbered_to_done": true,
			"self_contained": true, "no_real_choice": true, "check_proves_done": true, "title_plain": true, "risk": "none", "pass": true, "fixes": []string{}}
		j.mu.Lock()
		set, during := j.answer, j.during
		j.mu.Unlock()
		if during != nil {
			during()
		}
		switch o := set.(type) {
		case error:
			return nil, o
		case map[string]any:
			for k, v := range o {
				ans[k] = v
			}
		}
		return json.Marshal(ans)
	}
}

func (j *stepJudge) set(v any) {
	j.mu.Lock()
	j.answer = v
	j.mu.Unlock()
}

func newStepHarness(t *testing.T) (*harness, *stepJudge) {
	h := newHarness(t)
	j := &stepJudge{}
	j.set(map[string]any{})
	h.svc.SetJudge(judge.Fake{Fn: j.fn(h.judge.answer)})
	return h, j
}

const stepBrief = "1. Open a terminal.\n2. Run brew upgrade zm.\n3. Run zm version and see it print the newest version listed at https://github.com/example-org/zm/releases/latest."

func stepReq(work string) orchapi.StepAddRequest {
	return orchapi.StepAddRequest{To: "human:ivo", Work: work, Title: "Update zm on your Mac", Brief: stepBrief,
		Check: "zm machines --json | grep -q '\"zm_version\":\"1.4.0\"'"}
}

func TestPersonStepAddIsJudged(t *testing.T) {
	h, j := newStepHarness(t)
	ctx := context.Background()
	it := h.workItem("update zm everywhere")
	agent, agentAddr := h.session("aaaa0001")

	st := must(orchclient.AddStep(ctx, agent, stepReq(it.ID)))
	if st.State != orchapi.StepPending || st.Person != "ivo" || st.Risk != "none" || st.Work != it.ID || st.AddedBy != agentAddr || st.Bundle != "" {
		t.Fatalf("added step: %+v", st)
	}
	if n := j.calls.Load(); n != 3 {
		t.Fatalf("the step judge voted %d times, want 3", n)
	}

	// The plain limits are checked before any model is asked.
	j.calls.Store(0)
	for name, mut := range map[string]func(r *orchapi.StepAddRequest){
		"long title":       func(r *orchapi.StepAddRequest) { r.Title = strings.Repeat("a", orchapi.MaxPersonStepTitle+1) },
		"no title":         func(r *orchapi.StepAddRequest) { r.Title = " " },
		"two-line title":   func(r *orchapi.StepAddRequest) { r.Title = "Update\nzm" },
		"long brief":       func(r *orchapi.StepAddRequest) { r.Brief = strings.Repeat("b", orchapi.MaxPersonStepBrief+1) },
		"long check":       func(r *orchapi.StepAddRequest) { r.Check = "true " + strings.Repeat("c", orchapi.MaxPersonStepCheck) },
		"two-line check":   func(r *orchapi.StepAddRequest) { r.Check = "true\nfalse" },
		"no check":         func(r *orchapi.StepAddRequest) { r.Check = "" },
		"owners":           func(r *orchapi.StepAddRequest) { r.To = "owners" },
		"no person":        func(r *orchapi.StepAddRequest) { r.To = "" },
		"no work":          func(r *orchapi.StepAddRequest) { r.Work = "" },
		"draft and to":     func(r *orchapi.StepAddRequest) { r.Draft = "01JZZZZZZZZZZZZZZZZZZZZZZZ" },
		"draft and ask":    func(r *orchapi.StepAddRequest) { r.To, r.Work, r.Draft, r.FromAsk = "", "", "x", "y" },
		"ask and the work": func(r *orchapi.StepAddRequest) { r.To, r.FromAsk = "", "01JZZZZZZZZZZZZZZZZZZZZZZZ" },
	} {
		r := stepReq(it.ID)
		mut(&r)
		if _, err := orchclient.AddStep(ctx, agent, r); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := orchclient.AddStep(ctx, agent, func() orchapi.StepAddRequest { r := stepReq(it.ID); r.To = "human:nobody"; return r }()); err == nil {
		t.Error("a step for nobody here was added")
	}
	if n := j.calls.Load(); n != 0 {
		t.Fatalf("the judge was asked %d times about steps the plain limits refuse", n)
	}

	// A fail carries the judge's fixes, the producer's first; a pass whose
	// answers leave a gap is a fail.
	j.set(map[string]any{"self_contained": false, "pass": false, "fixes": []string{"Name the release page."}})
	_, err := orchclient.AddStep(ctx, agent, stepReq(it.ID))
	if code(err) != api.CodeBadRequest || !strings.Contains(err.Error(), "where every link") || !strings.Contains(err.Error(), "Name the release page.") {
		t.Fatalf("judge fail: %v", err)
	}
	j.set(map[string]any{"check_proves_done": false})
	if _, err := orchclient.AddStep(ctx, agent, stepReq(it.ID)); code(err) != api.CodeBadRequest || !strings.Contains(err.Error(), "check line") {
		t.Fatalf("a pass with a gap: %v", err)
	}
	// A real choice is a decision: its own ask.
	j.set(map[string]any{"no_real_choice": false, "pass": false})
	_, err = orchclient.AddStep(ctx, agent, stepReq(it.ID))
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeBadRequest || !strings.Contains(e.Message, "1) it leaves the person a real choice") || !strings.Contains(e.Hint, "zm ask new --to human:ivo") {
		t.Fatalf("a decision as a step: %v", err)
	}
	// No answer, or an answer outside the schema, is never a pass.
	for name, v := range map[string]any{
		"no answer":    errors.New("the model did not answer"),
		"missing":      map[string]any{"title_plain": nil},
		"unknown risk": map[string]any{"risk": "huge"},
	} {
		j.set(v)
		if _, err := orchclient.AddStep(ctx, agent, stepReq(it.ID)); code(err) != api.CodeUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The highest risk any vote names is the step's.
	j.set(map[string]any{"risk": "spending"})
	if st := must(orchclient.AddStep(ctx, agent, stepReq(it.ID))); st.Risk != "spending" {
		t.Fatalf("risk %q", st.Risk)
	}
	l := must(orchclient.Steps(ctx, agent, "", "human:ivo", ""))
	if len(l.Steps) != 2 || l.Steps[0].ID != st.ID {
		t.Fatalf("ls: %+v", l.Steps)
	}
	if l := must(orchclient.Steps(ctx, agent, "", "human:wren", "")); len(l.Steps) != 0 {
		t.Fatalf("ls for another person: %+v", l.Steps)
	}

	// An area guest who neither added it nor is its person sees nothing:
	// hidden answers like missing.
	g := h.guest()
	if l := must(orchclient.Steps(ctx, g, "", "", "all")); len(l.Steps) != 0 {
		t.Fatalf("a guest listed steps: %+v", l.Steps)
	}
	_, hid := orchclient.GetStep(ctx, g, st.ID)
	_, mis := orchclient.GetStep(ctx, g, "01JZZZZZZZZZZZZZZZZZZZZZZZ")
	if code(hid) != api.CodeNotFound || code(mis) != api.CodeNotFound {
		t.Fatalf("a guest read a step: %v / %v", hid, mis)
	}
	if _, err := orchclient.DropStep(ctx, g, st.ID); code(err) != api.CodeNotFound {
		t.Fatalf("a guest dropped a step: %v", err)
	}
	if _, err := orchclient.AddStep(ctx, g, stepReq(it.ID)); err == nil {
		t.Fatal("a guest added a step for a person")
	}

	// Dropped steps leave the default list.
	if d := must(orchclient.DropStep(ctx, agent, st.ID)); d.State != orchapi.StepDropped {
		t.Fatalf("drop: %+v", d)
	}
	if _, err := orchclient.DropStep(ctx, agent, st.ID); code(err) != api.CodeConflict {
		t.Fatalf("drop twice: %v", err)
	}
	if l := must(orchclient.Steps(ctx, agent, "", "", "")); len(l.Steps) != 1 {
		t.Fatalf("ls after a drop: %+v", l.Steps)
	}
	if l := must(orchclient.Steps(ctx, agent, "", "", "all")); len(l.Steps) != 2 {
		t.Fatalf("ls --state all: %+v", l.Steps)
	}
}

func TestPersonStepCardReportCheck(t *testing.T) {
	h, _ := newStepHarness(t)
	ctx := context.Background()
	it := h.workItem("the daily pile")
	agent, agentAddr := h.session("aaaa0002")
	s1 := must(orchclient.AddStep(ctx, agent, stepReq(it.ID)))
	r2 := stepReq(it.ID)
	r2.Title = "Accept the team network invite"
	s2 := must(orchclient.AddStep(ctx, agent, r2))
	if got := must(h.svc.PendingSteps(ctx, h.project.ID, "ivo")); len(got) != 2 || got[0] != s1.ID || got[1] != s2.ID {
		t.Fatalf("pending, oldest first: %v", got)
	}

	// Nothing to report before a card carries it.
	if _, err := orchclient.ReportStep(ctx, h.owner, s1.ID, 1, orchapi.StepReportRequest{Result: "done", Note: "did it"}); code(err) != api.CodeNotFound {
		t.Fatalf("a report with no card: %v", err)
	}
	const bundle, ask = "01JBUNDLE00000000000000001", "01JASK0000000000000000001"
	must(0, h.svc.CardSteps(ctx, bundle, ask, []string{s1.ID, s2.ID}))
	if err := h.svc.CardSteps(ctx, "01JBUNDLE00000000000000002", "01JASK0000000000000000002", []string{s1.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a step on two cards: %v", err)
	}
	b := must(orchclient.GetBundle(ctx, agent, ask))
	if b.ID != bundle || b.Ask != ask || len(b.Steps) != 2 || b.Steps[0].Number != 1 || b.Steps[1].Number != 2 || b.Done {
		t.Fatalf("card by its ask: %+v", b)
	}
	if b := must(orchclient.GetBundle(ctx, agent, bundle)); len(b.Steps) != 2 {
		t.Fatalf("card by its bundle: %+v", b)
	}
	if _, err := orchclient.GetBundle(ctx, h.guest(), bundle); code(err) != api.CodeNotFound {
		t.Fatalf("a guest read the card: %v", err)
	}

	// The person reports step 1 done: recorded, on the work item, and its
	// adder is asked to run the check. Nothing is done yet.
	for name, req := range map[string]orchapi.StepReportRequest{
		"no note":    {Result: "done"},
		"two lines":  {Result: "done", Note: "a\nb"},
		"bad result": {Result: "maybe", Note: "x"},
	} {
		if _, err := orchclient.ReportStep(ctx, h.owner, bundle, 1, req); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := orchclient.ReportStep(ctx, h.owner, bundle, 3, orchapi.StepReportRequest{Result: "done", Note: "x"}); code(err) != api.CodeNotFound {
		t.Fatalf("step 3 of 2: %v", err)
	}
	rep := must(orchclient.ReportStep(ctx, h.owner, ask, 1, orchapi.StepReportRequest{Result: "done", Note: "zm version prints 1.4.0"}))
	if rep.State != orchapi.StepReported || rep.ReportNote != "zm version prints 1.4.0" || rep.ReportedAt == nil {
		t.Fatalf("reported: %+v", rep)
	}
	if !strings.Contains(strings.Join(h.comments(it.ID), "\n"), "STEP 1 REPORTED DONE") {
		t.Fatalf("no report on the work item: %q", h.comments(it.ID))
	}
	if len(h.mailTo(agentAddr, "zm ask bundle step-check "+s1.ID)) != 1 {
		t.Fatal("the adder was not asked to run the check")
	}
	// A failing check after a done report fails it; a passing one makes it done.
	if c := must(orchclient.CheckStep(ctx, agent, s1.ID, orchapi.StepCheckRequest{Exit: exit(1), Output: "zm 1.3.9"})); c.State != orchapi.StepFailed || *c.CheckExit != 1 {
		t.Fatalf("failing check: %+v", c)
	}
	if _, err := orchclient.CheckStep(ctx, agent, s1.ID, orchapi.StepCheckRequest{}); code(err) != api.CodeBadRequest {
		t.Fatalf("a check with no exit: %v", err)
	}
	if c := must(orchclient.CheckStep(ctx, agent, s1.ID, orchapi.StepCheckRequest{Exit: exit(0)})); c.State != orchapi.StepDone || c.CheckedBy != agentAddr {
		t.Fatalf("passing check: %+v", c)
	}
	if !strings.Contains(strings.Join(h.comments(it.ID), "\n"), "STEP DONE (verified by its check") {
		t.Fatalf("no done record on the work item: %q", h.comments(it.ID))
	}
	if _, err := orchclient.ReportStep(ctx, h.owner, bundle, 1, orchapi.StepReportRequest{Result: "failed", Note: "x"}); code(err) != api.CodeConflict {
		t.Fatalf("a report on a done step: %v", err)
	}
	// Step 2 failed by its agent; the card is not done until it is done or dropped.
	if f := must(orchclient.ReportStep(ctx, h.owner, bundle, 2, orchapi.StepReportRequest{Result: "failed", Note: "the invite expired"})); f.State != orchapi.StepFailed {
		t.Fatalf("failed: %+v", f)
	}
	if len(h.mailTo(agentAddr, "STEP FAILED")) != 1 {
		t.Fatal("the adder was not told the step failed")
	}
	if must(orchclient.GetBundle(ctx, agent, bundle)).Done {
		t.Fatal("a card with a failed step is done")
	}
	must(orchclient.DropStep(ctx, agent, s2.ID))
	if !must(orchclient.GetBundle(ctx, agent, bundle)).Done {
		t.Fatal("every step done or dropped, and the card is not done")
	}
	if err := h.svc.CardSteps(ctx, bundle, "01JASK0000000000000000009", []string{must(orchclient.AddStep(ctx, agent, stepReq(it.ID))).ID}); code(err) != api.CodeConflict {
		t.Fatalf("a bundle id cut twice: %v", err)
	}

	// A card is done only when all of it is: an adder who reads only its own
	// step (a whole-project guest operates nowhere) never sees it done early.
	guest := h.tokenClient(api.MintTokenRequest{As: "vida", Project: "harbor", Role: "guest"})
	mine := must(orchclient.AddStep(ctx, guest, stepReq(it.ID)))
	theirs := must(orchclient.AddStep(ctx, agent, stepReq(it.ID)))
	if l := must(orchclient.Steps(ctx, guest, "", "", "all")); len(l.Steps) != 1 || l.Steps[0].ID != mine.ID {
		t.Fatalf("a guest adder lists: %+v", l.Steps)
	}
	const mixed = "01JBUNDLE00000000000000006"
	must(0, h.svc.CardSteps(ctx, mixed, "01JASK0000000000000000006", []string{mine.ID, theirs.ID}))
	must(orchclient.CheckStep(ctx, guest, mine.ID, orchapi.StepCheckRequest{Exit: exit(0)}))
	if b := must(orchclient.GetBundle(ctx, guest, mixed)); len(b.Steps) != 1 || b.Done {
		t.Fatalf("a reader of part of a card: %d steps, done %v", len(b.Steps), b.Done)
	}
	if _, err := orchclient.CheckStep(ctx, guest, theirs.ID, orchapi.StepCheckRequest{Exit: exit(0)}); code(err) != api.CodeNotFound {
		t.Fatalf("a guest checked another's step: %v", err)
	}
}

func TestPersonStepNotTodayAndDrafts(t *testing.T) {
	h, j := newStepHarness(t)
	ctx := context.Background()
	it := h.workItem("steps that wait")
	agent, agentAddr := h.session("aaaa0003")
	s1 := must(orchclient.AddStep(ctx, agent, stepReq(it.ID)))
	const bundle = "01JBUNDLE00000000000000003"
	must(0, h.svc.CardSteps(ctx, bundle, "01JASK0000000000000000003", []string{s1.ID}))
	// "Not today": back to pending, off the card.
	must(0, h.svc.ReturnSteps(ctx, bundle))
	if st := must(orchclient.GetStep(ctx, agent, s1.ID)); st.State != orchapi.StepPending || st.Bundle != "" || st.Number != 0 {
		t.Fatalf("returned: %+v", st)
	}
	// Cut again: a card withdrawn to be re-cut returns its steps first, so a
	// step is never on two cards.
	must(0, h.svc.CardSteps(ctx, "01JBUNDLE00000000000000004", "01JASK0000000000000000004", []string{s1.ID}))
	if err := h.svc.CardSteps(ctx, "01JBUNDLE00000000000000005", "01JASK0000000000000000005", []string{s1.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a carded step cut onto another card: %v", err)
	}
	must(0, h.svc.ReturnSteps(ctx, "01JBUNDLE00000000000000004"))
	must(0, h.svc.CardSteps(ctx, "01JBUNDLE00000000000000005", "01JASK0000000000000000005", []string{s1.ID}))
	if _, err := orchclient.GetBundle(ctx, agent, "01JBUNDLE00000000000000004"); code(err) != api.CodeNotFound {
		t.Fatalf("the old card still holds the step: %v", err)
	}

	// A draft (a refused report, saved whole) is completed by its sender
	// through the judge; the plain limits apply then, not before.
	long := strings.Repeat("please do this. ", 100)
	d := must(h.svc.SaveDraftStep(ctx, h.project.ID, it.Area, "ivo", agentAddr, it.ID, long))
	if st := must(orchclient.GetStep(ctx, agent, d)); st.State != orchapi.StepDraft {
		t.Fatalf("draft: %+v", st)
	}
	if _, err := orchclient.CheckStep(ctx, agent, d, orchapi.StepCheckRequest{Exit: exit(0)}); code(err) != api.CodeConflict {
		t.Fatalf("a check on a draft: %v", err)
	}
	j.set(errors.New("down"))
	done := orchapi.StepAddRequest{Draft: d, Title: "Update zm on your Mac", Brief: stepBrief, Check: "zm machines --json | grep -q 1.4.0"}
	if _, err := orchclient.AddStep(ctx, agent, done); code(err) != api.CodeUnavailable {
		t.Fatalf("draft, judge down: %v", err)
	}
	if st := must(orchclient.GetStep(ctx, agent, d)); st.State != orchapi.StepDraft {
		t.Fatalf("a draft the judge never read left draft: %+v", st)
	}
	j.set(map[string]any{})
	if st := must(orchclient.AddStep(ctx, agent, done)); st.ID != d || st.State != orchapi.StepPending || st.Brief != stepBrief || st.Person != "ivo" || st.Work != it.ID {
		t.Fatalf("completed draft: %+v", st)
	}
	if _, err := orchclient.AddStep(ctx, agent, done); code(err) != api.CodeConflict {
		t.Fatalf("a draft completed twice: %v", err)
	}
}

func TestPersonStepFromAHandAsk(t *testing.T) {
	h, j := newStepHarness(t)
	ctx := context.Background()
	it := h.workItem("a hand ask that should be a step")
	agent, _ := h.session("aaaa0005")
	handCard := func(brief string) *orchapi.AskCard {
		c := card()
		c.Action = &orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief}
		c.Verify = &orchapi.AskVerify{Command: "test -f /tmp/zm-updated"}
		return c
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "hand-update", Owner: "ivo", Card: handCard(stepBrief)}))
	plain := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "plain-card", Owner: "ivo", Card: card()}))
	big := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "hand-big", Owner: "ivo", Card: handCard(stepBrief + strings.Repeat("\n4. And one more line.", 60))}))

	if _, err := orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: plain.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a card with no hand brief: %v", err)
	}
	if _, err := orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: big.ID}); code(err) != api.CodeBadRequest {
		t.Fatalf("a hand brief past a step's limit: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, agent, big.ID)); got.Status != orchapi.AskOpen {
		t.Fatalf("a refused move withdrew the ask: %s", got.Status)
	}
	if _, err := orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: a.ID, Check: "true"}); code(err) != api.CodeBadRequest {
		t.Fatalf("from_ask with its own check: %v", err)
	}
	// Withdrawn while the judge reads it: nothing is added.
	gone := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "hand-gone", Owner: "ivo", Card: handCard(stepBrief)}))
	var once sync.Once
	j.mu.Lock()
	j.during = func() { once.Do(func() { must(orchclient.WithdrawAsk(ctx, agent, gone.ID)) }) }
	j.mu.Unlock()
	if _, err := orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: gone.ID}); code(err) != api.CodeConflict {
		t.Fatalf("an ask withdrawn meanwhile: %v", err)
	}
	j.mu.Lock()
	j.during = nil
	j.mu.Unlock()
	if l := must(orchclient.Steps(ctx, agent, "", "", "all")); len(l.Steps) != 0 {
		t.Fatalf("a step stayed for a withdrawn ask: %+v", l.Steps)
	}
	st := must(orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: a.ID}))
	if st.State != orchapi.StepPending || st.Person != "ivo" || st.Work != it.ID || st.Brief != stepBrief || st.Check != "test -f /tmp/zm-updated" || st.Title != a.Card.Title {
		t.Fatalf("moved step: %+v", st)
	}
	if got := must(orchclient.GetAsk(ctx, agent, a.ID)); got.Status != orchapi.AskWithdrawn {
		t.Fatalf("the hand ask was not withdrawn: %s", got.Status)
	}
	if _, err := orchclient.AddStep(ctx, agent, orchapi.StepAddRequest{FromAsk: a.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a withdrawn ask moved again: %v", err)
	}
}
