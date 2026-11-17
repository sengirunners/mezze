package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// switchJudge answers with whatever the test last set; it records what the
// judge was shown.
type switchJudge struct {
	mu     sync.Mutex
	answer string // "" = no answer (an error: held)
	seen   []string
}

func (j *switchJudge) set(answer string) { j.mu.Lock(); j.answer = answer; j.mu.Unlock() }

func (j *switchJudge) Call(_ context.Context, prompt string, _ json.RawMessage) (json.RawMessage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seen = append(j.seen, prompt)
	if j.answer == "" {
		return nil, errors.New("the provider is down")
	}
	return json.RawMessage(j.answer), nil
}

const (
	judgePass = `{"decidable":true,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`
	judgeFail = `{"decidable":false,"person_step":true,"step_carried":false,"brief_needed":true,"brief_complete":false,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":false,"fixes":["Say what the shelf is in plain words","Choice A leaves Bea a step: carry it as a hand brief"]}`
	// The model says pass, but its own answers say a step is left: a fail.
	judgeLies = `{"decidable":true,"person_step":true,"step_carried":false,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`
)

func (h *harness) inDecisions(id string) bool {
	h.t.Helper()
	for _, x := range must(orchclient.Decisions(context.Background(), h.owner, "", "")).Decisions {
		if x.Ask != nil && x.Ask.ID == id {
			return true
		}
	}
	return false
}

// SPEC 16 "Card judge": a fail is refused with its fixes; a pass is sent.
// The judge reads the card, its action and the approver.
func TestCardJudgeRefusesWithFixes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{answer: judgeFail}
	h.svc.SetJudge(j)
	it := h.workItem("package shelf")
	agent, _ := h.session("aaaaaaaa")
	_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()})
	var e *api.Error
	if !api.IsCode(err, api.CodeBadRequest) || !errors.As(err, &e) || !strings.Contains(e.Message, "1) Say what the shelf is in plain words 2) Choice A leaves") ||
		!strings.Contains(e.Hint, "--hand") {
		t.Fatalf("refusal: %v (%+v)", err, e)
	}
	if len(j.seen) != 3 || !strings.Contains(j.seen[0], "Should we keep last year's swaps?") || !strings.Contains(j.seen[0], `"approver"`) {
		t.Fatalf("the judge saw: %q", j.seen)
	}
	if n := len(h.mailTo("human:ivo", "last year's swaps")); n != 0 {
		t.Fatalf("a refused card sent %d mails", n)
	}
	j.set(judgeLies)
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}); !api.IsCode(err, api.CodeBadRequest) ||
		!strings.Contains(err.Error(), "carry the whole step") {
		t.Fatalf("a pass that leaves a step: %v", err)
	}
	j.set(judgePass)
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	if a.Note != "" || !h.inDecisions(a.ID) {
		t.Fatalf("a passed card: %+v", a)
	}
}

// No answer never passes: the ask is stored held, reaches nobody and cannot
// be decided; judged again, a pass sends it and a fail withdraws it and
// tells its asker the fixes.
func TestCardJudgeHoldsThenRejudges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("held card")
	agent, addr := h.session("bbbbbbbb")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "held-one"}))
	if !strings.Contains(a.Note, "held, not sent") || !strings.Contains(a.Note, "the provider is down") {
		t.Fatalf("held ask: %+v", a)
	}
	// Held, it is listed to the owner at once, marked not checked, but it is
	// sent to nobody: no mail, and it cannot be answered.
	if !h.inDecisions(a.ID) || len(h.mailTo("human:ivo", "last year's swaps")) != 0 {
		t.Fatal("a held ask is not listed to the owner, or reached them")
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); !got.NotChecked || !strings.Contains(got.NotCheckedReason, "the provider is down") {
		t.Fatalf("the owner reads a held ask: %+v", got)
	}
	if got := must(orchclient.GetAsk(ctx, agent, a.ID)); !strings.Contains(got.Note, "held") {
		t.Fatalf("its asker reads: %+v", got)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "held") {
		t.Fatalf("deciding a held ask: %v", err)
	}

	// Still no answer: it stays held. Then a pass sends it.
	h.clock.add(time.Minute)
	h.svc.RejudgeHeld(ctx)
	if !must(orchclient.GetAsk(ctx, h.owner, a.ID)).NotChecked || len(h.mailTo("human:ivo", "last year's swaps")) != 0 {
		t.Fatal("sent with no answer")
	}
	j.set(judgePass)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.NotChecked || len(h.mailTo("owners", "last year's swaps"))+len(h.mailTo("human:ivo", "last year's swaps")) == 0 {
		t.Fatalf("a passed held ask was not sent: %+v", must(orchclient.GetAsk(ctx, agent, a.ID)))
	}
	d := must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}))
	if d.Decision != "a" {
		t.Fatalf("decision: %+v", d)
	}

	// A second held ask the judge then fails: withdrawn, its asker told.
	j.set("")
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "held-two"}))
	j.set(judgeFail)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	got := must(orchclient.GetAsk(ctx, agent, b.ID))
	if got.Status != orchapi.AskWithdrawn || h.inDecisions(b.ID) {
		t.Fatalf("a held ask that failed: %+v", got)
	}
	if ms := h.mailTo(addr, "card judge sent it back"); len(ms) != 1 || !strings.Contains(ms[0].Body, "1) Say what the shelf is") {
		t.Fatalf("the asker's notice: %+v", ms)
	}
	// Its topic is free again.
	j.set(judgePass)
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "held-two"}))
}

// A withdrawal while the judge is still reading a held ask wins: the pass
// that follows sends nothing.
func TestCardJudgeWithdrawnWhileJudging(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("withdrawn while judging")
	agent, _ := h.session("eeeeeeee")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	h.svc.SetJudge(judge.Fake{Fn: func(string) (json.RawMessage, error) {
		must(orchclient.WithdrawAsk(ctx, agent, a.ID))
		return json.RawMessage(judgePass), nil
	}})
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, agent, a.ID)); got.Status != orchapi.AskWithdrawn || h.inDecisions(a.ID) ||
		len(h.mailTo("human:ivo", "last year's swaps"))+len(h.mailTo("owners", "last year's swaps")) != 0 {
		t.Fatalf("withdrawn while judged, then sent: %+v", got)
	}
}

// A deadline that passed while the ask was held moves out by the window its
// asker gave, so the person still gets that time.
func TestCardJudgeHeldDeadlineMoves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("held deadline")
	agent, _ := h.session("ffffffff")
	by := h.clock.now().Add(2 * time.Hour)
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), By: &by}))
	h.clock.add(3 * time.Hour)
	j.set(judgePass)
	h.svc.RejudgeHeld(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.By == nil || !got.By.Equal(h.clock.now().Add(2*time.Hour)) {
		t.Fatalf("deadline after release: %v (now %v)", got.By, h.clock.now())
	}
	// A held ask is not even found by anyone but its asker and who may operate.
	j.set("")
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "second"}))
	if _, err := orchclient.GetAsk(ctx, h.guest(), b.ID); !api.IsCode(err, api.CodeNotFound) && !api.IsCode(err, api.CodeForbidden) && !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("a guest reads a held ask: %v", err)
	}
}

// A held ask its asker withdraws is never sent.
func TestCardJudgeHeldWithdrawn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("withdrawn held card")
	agent, _ := h.session("cccccccc")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	if w := must(orchclient.WithdrawAsk(ctx, agent, a.ID)); w.Status != orchapi.AskWithdrawn {
		t.Fatalf("withdraw: %+v", w)
	}
	j.set(judgePass)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if h.inDecisions(a.ID) || len(h.mailTo("human:ivo", "last year's swaps")) != 0 {
		t.Fatal("a withdrawn held ask was sent")
	}
}

// A hand brief: an agent and a brief, no machine, and a verify check; only
// a signed answer picks it, and the yes stays approved, not done until the
// check passes.
func TestHandBrief(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("hand brief")
	agent, _ := h.session("dddddddd")
	hand := func(act orchapi.AskAction, verify bool) *orchapi.AskCard {
		c := card()
		c.Action = &act
		if verify {
			c.Verify = &orchapi.AskVerify{Command: "test -f /tmp/done"}
		}
		return c
	}
	brief := "1. Turn on remote tasks for this machine.\n2. Switch the shelf on.\n3. Run the check."
	for name, c := range map[string]*orchapi.AskCard{
		"no verify":   hand(orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief}, false),
		"a machine":   hand(orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief, Machine: "mini"}, true),
		"no brief":    hand(orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude"}, true),
		"other agent": hand(orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "gemini", Brief: brief}, true),
	} {
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: c, Topic: "hand-" + strings.ReplaceAll(name, " ", "-")}); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: hand(orchapi.AskAction{Kind: orchapi.ActionHand, Agent: "claude", Brief: brief}, true)}))
	if a.Card.Action.BriefSHA == "" || !strings.Contains(a.ActionText, "your own agent (claude)") || !strings.Contains(a.ActionText, "Hand to my agent") {
		t.Fatalf("hand ask: %+v %q", a.Card.Action, a.ActionText)
	}
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("choose from the socket: %v", err)
	}
	must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"}))
	got := must(orchclient.GetAsk(ctx, agent, a.ID))
	if got.ActionState == nil || got.ActionState.State != orchapi.ActionApproved {
		t.Fatalf("a yes to a hand brief: %+v", got.ActionState)
	}
}

func TestCardJudgeDefaultInTests(t *testing.T) {
	// A test binary's default judge passes, so tests about other things never
	// call a model; production's default is the provider CLI.
	if v := (judge.Judge{Name: "t", Prompt: "p"}).Run(context.Background(), judge.Default(nil), "x"); v.Status != judge.Pass {
		t.Fatalf("default in a test: %+v", v)
	}
}

// A judge down for over 15 minutes is not silent: health says so and the
// project's owners get one notice, not one per sweep; once it answers again
// both clear, so a later outage is told again.
func TestCardJudgeOutageAlert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("judge outage")
	agent, _ := h.session("abababab")
	const told = "held for over 15 minutes"
	health := func() *orchapi.CardJudgeHold { return must(orchclient.Health(ctx, h.owner)).CardJudge }
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "outage-one"}))
	h.clock.add(10 * time.Minute)
	h.svc.RejudgeHeld(ctx)
	if n := len(h.mailTo("owners", told)); n != 0 || health() != nil {
		t.Fatalf("alerted after 10 minutes: %d mails, health %+v", n, health())
	}
	h.clock.add(10 * time.Minute) // 20 minutes: past the 15 of the notice
	h.svc.RejudgeHeld(ctx)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if ms := h.mailTo("owners", told); len(ms) != 1 || !strings.Contains(ms[0].Body, "the provider is down") {
		t.Fatalf("owners told %d times: %+v", len(ms), ms)
	}
	if hh := health(); hh == nil || hh.Held != 1 || !strings.Contains(hh.Reason, "the provider is down") {
		t.Fatalf("health: %+v", hh)
	}
	j.set(judgePass)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if hh := health(); hh != nil {
		t.Fatalf("health after the judge answered: %+v", hh)
	}
	j.set("")
	must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "outage-two"}))
	h.clock.add(2 * time.Hour)
	h.svc.RejudgeHeld(ctx)
	if n := len(h.mailTo("owners", told)); n != 2 {
		t.Fatalf("a second outage: owners told %d times in all", n)
	}
}
