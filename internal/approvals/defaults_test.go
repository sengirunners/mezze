package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// fixtureJudge answers the model's part for these tests: the irreversible
// judge (irreversible.md; its real-model cases are testdata/irreversible)
// fails exactly the irreversible fixtures the tests write, and holds while
// held is set; every other judge (the card judge) passes.
type fixtureJudge struct {
	mu    sync.Mutex
	held  bool
	asked int
	gate  chan struct{} // non-nil: an irreversible answer waits for it to close
}

var irreversibleFixtures = []string{"on production", "app store", "email every resident", "refund to the customer", "admin access", "token rotated", "deploy it to production"}

func (j *fixtureJudge) answer(prompt string) (json.RawMessage, error) {
	if !strings.Contains(prompt, "wants its recommended choice applied on its own") {
		return json.RawMessage(`{"decidable":true,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`), nil
	}
	j.mu.Lock()
	j.asked++
	gate := j.gate
	j.mu.Unlock()
	if gate != nil {
		<-gate
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.held {
		return nil, errors.New("the model did not answer")
	}
	_, input, _ := strings.Cut(prompt, "<input>")
	for _, f := range irreversibleFixtures {
		if strings.Contains(input, f) {
			return json.RawMessage(`{"pass":false,"fixes":["Ask without the default."],"reason":"It names ` + f + `."}`), nil
		}
	}
	return json.RawMessage(`{"pass":true,"fixes":[],"reason":"Every choice is reversible."}`), nil
}

func (j *fixtureJudge) hold(on bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.held = on
}

// ciCard is a reversible choice, like solver PR 32's: which runner a check uses.
func ciCard() *orchapi.AskCard {
	return &orchapi.AskCard{
		Context:    "A check needs a build tool that one test machine lacks, so the check fails there.",
		Background: "Both fixes can be changed back in a minute.",
		Question:   "Should the check install the tool, or run only on the machine that has it?",
		Choices: []orchapi.AskChoice{
			{Label: "Install the tool", Pros: "runs anywhere", Cons: "a slower first run"},
			{Label: "Use the one machine", Pros: "no change to the setup", Cons: "waits when that machine is busy"},
		},
		Recommend: "a", Why: "Installing keeps every machine usable; it costs a minute once.",
		Searched: "the check's history and the machine list: nothing settles it",
	}
}

var defaultAskers int

func (h *harness) defaultAsk(title string, c *orchapi.AskCard, by time.Duration) (orchapi.Ask, string, string) {
	h.t.Helper()
	it := h.workItem(title)
	defaultAskers++
	agent, addr := h.session(fmt.Sprintf("dfdf%04d", defaultAskers))
	at := h.clock.now().Add(by)
	a, err := orchclient.NewAsk(context.Background(), agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: c, By: &at, ApplyDefault: true})
	if err != nil {
		h.t.Fatal(err)
	}
	return a, it.ID, addr
}

func (h *harness) mailTo(addr, word string) []orchapi.Mail {
	h.t.Helper()
	var out []orchapi.Mail
	for _, m := range must(orchclient.Mail(context.Background(), h.owner, orchapi.MailQuery{})) {
		if m.To == addr && strings.Contains(m.Body, word) {
			out = append(out, m)
		}
	}
	return out
}

func countWith(xs []string, word string) int {
	n := 0
	for _, x := range xs {
		if strings.Contains(x, word) {
			n++
		}
	}
	return n
}

// Oracle 1: a 1-minute deadline passes with nobody answering. The default is
// recorded as DEFAULT APPLIED (never an owner decision), the asker is woken,
// and the card stays in Decisions so the person can Undo.
func TestDefaultAppliesAtDeadline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, item, addr := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	if !a.ApplyDefault || a.DefaultApplied != nil {
		t.Fatalf("new ask: %+v", a)
	}
	if st := must(orchclient.Session(ctx, h.owner, addr)); st.State != orchapi.StateNeedsHuman {
		t.Fatalf("asker before the deadline: %s", st.State)
	}
	h.svc.ApplyDefaults(ctx) // not yet due
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil {
		t.Fatal("applied before its deadline")
	}
	h.clock.add(61 * time.Second)
	h.svc.ApplyDefaults(ctx)
	h.svc.ApplyDefaults(ctx) // a second tick changes nothing

	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.DefaultApplied == nil || got.Decision != nil || got.Status != orchapi.AskOpen {
		t.Fatalf("after the deadline: %+v", got)
	}
	cs := h.comments(item)
	if countWith(cs, "DEFAULT APPLIED") != 1 || countWith(cs, "OWNER DECISION") != 0 || countWith(cs, "Install the tool") != 1 {
		t.Fatalf("work item comments: %q", cs)
	}
	if st := must(orchclient.Session(ctx, h.owner, addr)); st.State == orchapi.StateNeedsHuman {
		t.Fatal("the asker was not woken: still needs_human")
	}
	if n := len(h.mailTo(addr, "DEFAULT APPLIED")); n != 1 {
		t.Fatalf("asker's DEFAULT APPLIED mail: %d", n)
	}
	if n := len(h.events(orchapi.EventDefaultApplied)); n != 1 {
		t.Fatalf("default_applied events: %d", n)
	}
	in := must(orchclient.Decisions(ctx, h.owner, "", ""))
	found := false
	for _, x := range in.Decisions {
		if x.Ask != nil && x.Ask.ID == a.ID {
			found = x.Ask.DefaultApplied != nil && x.Ask.DefaultUndone == nil
		}
	}
	if !found {
		t.Fatalf("Decisions lost the card or its Undo state: %+v", in.Decisions)
	}
}

// Oracle 2: Undo records the reversal on the work item and tells the asker;
// the card then waits for the person's own answer.
func TestUndoDefaultTellsTheAsker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, item, addr := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	if _, err := orchclient.UndoDefault(ctx, h.owner, "", a.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("undo before any default: %v", err)
	}
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	u := must(orchclient.UndoDefault(ctx, h.owner, "", a.ID))
	if u.DefaultUndone == nil || u.DefaultUndoneBy != "ivo" || u.Decision != nil {
		t.Fatalf("undo: %+v", u)
	}
	again := must(orchclient.UndoDefault(ctx, h.owner, "", a.ID)) // the same answer, nothing sent twice
	if !again.DefaultUndone.Equal(*u.DefaultUndone) {
		t.Fatalf("second undo: %+v", again)
	}
	h.svc.ApplyDefaults(ctx)
	cs := h.comments(item)
	if countWith(cs, "DEFAULT UNDONE") != 1 || countWith(cs, "DEFAULT APPLIED") != 1 {
		t.Fatalf("work item comments: %q", cs)
	}
	if n := len(h.mailTo(addr, "DEFAULT UNDONE")); n != 1 {
		t.Fatalf("asker's DEFAULT UNDONE mail: %d", n)
	}
	if n := len(h.events(orchapi.EventDefaultUndone)); n != 1 {
		t.Fatalf("default_undone events: %d", n)
	}
	d := must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "b"}))
	if d.Decision != "b" {
		t.Fatalf("answer after undo: %+v", d)
	}
	if _, err := orchclient.UndoDefault(ctx, h.owner, "", a.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("undo after an answer: %v", err)
	}
}

// A person who asked gets no notice from zm when a default applies or is
// undone (SPEC 9 "No notice about what you just did"): the ask's mail is
// taken, nothing is sent, the card stays in Decisions.
func TestDefaultTellsAPersonNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("person asks")
	person := h.ownerIn("harbor/solver", "")
	at := h.clock.now().Add(time.Minute)
	a := must(orchclient.NewAsk(ctx, person, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: ciCard(), By: &at, ApplyDefault: true}))
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	must(orchclient.UndoDefault(ctx, h.owner, "", a.ID))
	h.svc.ApplyDefaults(ctx)
	th := must(orchclient.Thread(ctx, h.owner, a.ID))
	if len(th) != 1 || th[0].AckedAt == nil {
		t.Fatalf("a person was mailed about their own ask: %+v", th)
	}
	for _, m := range must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{})) {
		if strings.Contains(m.Body, "DEFAULT") {
			t.Fatalf("default mail to a person: %+v", m)
		}
	}
	if countWith(h.comments(it.ID), "DEFAULT UNDONE") != 1 {
		t.Fatal("the work item lost its record")
	}
}

// Oracle 3: an irreversible step never auto-applies; zm refuses the flag.
func TestIrreversibleAskRefusesDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("dfdf0100")
	at := h.clock.now().Add(time.Hour)
	ask := func(title string, c *orchapi.AskCard, mod func(*orchapi.NewAskRequest)) error {
		it := h.workItem(title)
		r := orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: c, By: &at, ApplyDefault: true, Topic: "t" + it.ID[len(it.ID)-6:]}
		if mod != nil {
			mod(&r)
		}
		_, err := orchclient.NewAsk(ctx, agent, r)
		return err
	}
	edit := func(f func(*orchapi.AskCard)) *orchapi.AskCard { c := ciCard(); f(c); return c }
	for name, err := range map[string]error{
		"prod write in card": ask("runner", edit(func(c *orchapi.AskCard) { c.Choices[0].Label = "Run the migration on production" }), nil),
		"publish in item":    ask("Publish the release to the app store", ciCard(), nil),
		"outbound message":   ask("runner", edit(func(c *orchapi.AskCard) { c.Why = "We email every resident tonight." }), nil),
		"money":              ask("runner", edit(func(c *orchapi.AskCard) { c.Background = "It means a refund to the customer." }), nil),
		"access":             ask("Grant Vida admin access", ciCard(), nil),
		"secret":             ask("runner", edit(func(c *orchapi.AskCard) { c.Context = "The check needs the deploy token rotated." }), nil),
		"irreversible label": func() error {
			it := h.workItem("runner", "irreversible")
			_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: ciCard(), By: &at, ApplyDefault: true})
			return err
		}(),
		"no deadline":       ask("runner", ciCard(), func(r *orchapi.NewAskRequest) { r.By = nil }),
		"no recommendation": ask("runner", edit(func(c *orchapi.AskCard) { c.Recommend = "" }), nil),
	} {
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}
	// The plain question flag-free still goes out: the refusal is about the flag.
	it := h.workItem("Publish the release to the app store")
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: ciCard(), By: &at}); err != nil {
		t.Fatalf("the same ask without the flag: %v", err)
	}
}

// Oracle 4: an answer before the deadline means no default ever applies.
func TestAnswerBeforeDeadlinePreventsDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, item, addr := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	h.clock.add(30 * time.Second)
	must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "b"}))
	h.clock.add(time.Hour)
	h.svc.ApplyDefaults(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.DefaultApplied != nil || got.Decision == nil || got.Decision.Decision != "b" {
		t.Fatalf("answered ask: %+v", got)
	}
	if cs := h.comments(item); countWith(cs, "DEFAULT APPLIED") != 0 || countWith(cs, "OWNER DECISION") != 1 {
		t.Fatalf("work item comments: %q", cs)
	}
	if n := len(h.mailTo(addr, "DEFAULT APPLIED")); n != 0 {
		t.Fatalf("asker got DEFAULT APPLIED mail: %d", n)
	}
	// A withdrawn ask never applies either.
	w, _, _ := h.defaultAsk("pick a CI cache", ciCard(), time.Minute)
	must(orchclient.WithdrawAsk(ctx, h.owner, w.ID))
	h.clock.add(time.Hour)
	h.svc.ApplyDefaults(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, w.ID)); got.DefaultApplied != nil {
		t.Fatalf("withdrawn ask applied: %+v", got)
	}
}

// A paired device undoes a default as its approver; a device that is not
// one learns nothing.
func TestDeviceUndoesDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	a, item, addr := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	if _, err := orchclient.UndoDefaultOnDevice(ctx, h.device(ipTablet), a.ID); err == nil {
		t.Fatal("an unpaired device undid a default")
	}
	u := must(orchclient.UndoDefaultOnDevice(ctx, h.device(ipPhone), a.ID))
	if u.DefaultUndone == nil || u.DefaultUndoneBy != "ivo" {
		t.Fatalf("device undo: %+v", u)
	}
	if countWith(h.comments(item), "DEFAULT UNDONE") != 1 || len(h.mailTo(addr, "DEFAULT UNDONE")) != 1 {
		t.Fatal("device undo was not recorded and told")
	}
}

// Review fixes: the irreversible check runs again at the deadline; a default
// that stands is undone or answered, never withdrawn; an undone default's
// stale record is never sent after its undo.
func TestDefaultRechecksAndOrders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, item, addr := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	// The item changes after the ask: now it names a production write.
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Write, a.Project))
	if _, err := db.ExecContext(ctx, `UPDATE work_items SET body = 'Then deploy it to production.' WHERE ref = ?`, strings.TrimPrefix(item, "local:")); err != nil {
		t.Fatal(err)
	}
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil || got.ApplyDefault {
		t.Fatalf("applied though its item became irreversible: %+v", got)
	}
	if n := len(h.mailTo(addr, "no default")); n != 1 {
		t.Fatalf("asker not told the default was dropped: %d", n)
	}

	b, _, _ := h.defaultAsk("pick a CI cache", ciCard(), time.Minute)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	if _, err := orchclient.WithdrawAsk(ctx, h.owner, b.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("withdraw over an applied default: %v", err)
	}
	must(orchclient.UndoDefault(ctx, h.owner, "", b.ID))
	must(orchclient.WithdrawAsk(ctx, h.owner, b.ID)) // undone: withdrawing is fine again
	if _, err := orchclient.UndoDefault(ctx, h.owner, "", b.ID); err != nil {
		t.Fatalf("undo again on a withdrawn ask is the same answer: %v", err)
	}

	c, citem, _ := h.defaultAsk("pick a CI image", ciCard(), time.Minute)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	must(orchclient.Choose(ctx, h.owner, "", c.ID, orchapi.DecideRequest{Decision: "b"}))
	if cs := h.comments(citem); countWith(cs, "This replaces the default applied at") != 1 {
		t.Fatalf("decision over a default does not say it replaces it: %q", cs)
	}
}

// a judge that cannot answer never sets or applies a default: the
// flag is refused as unavailable when the ask is made, and a due default
// waits for the next tick until the judge answers.
func TestHeldJudgeNeverAppliesDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.judge.hold(true)
	agent, _ := h.session("dfdf0200")
	at := h.clock.now().Add(time.Hour)
	it := h.workItem("pick a CI runner")
	_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: ciCard(), By: &at, ApplyDefault: true})
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "the model did not answer") {
		t.Fatalf("held at the ask: %v", err)
	}
	h.judge.hold(false)
	a, _, addr := h.defaultAsk("pick a CI cache", ciCard(), time.Minute)
	h.judge.hold(true)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.DefaultApplied != nil || !got.ApplyDefault {
		t.Fatalf("held at the deadline: applied or dropped: %+v", got)
	}
	if n := len(h.mailTo(addr, "no default")); n != 0 {
		t.Fatalf("held at the deadline told the asker the default was dropped")
	}
	h.judge.hold(false)
	h.svc.ApplyDefaults(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied == nil {
		t.Fatalf("not applied once the judge answered: %+v", got)
	}
}

// The deadline sweep asks the irreversible judge, so it runs
// off the retry ticker, one at a time: a judge that hangs leaves the ticker
// free, a second tick starts no second sweep, and the default still waits
// for the judge's pass before it is applied.
func TestHungJudgeLeavesTheTickerFree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _, _ := h.defaultAsk("pick a CI runner", ciCard(), time.Minute)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	h.judge.mu.Lock()
	h.judge.gate, h.judge.asked = gate, 0
	h.judge.mu.Unlock()
	h.clock.add(2 * time.Minute)

	returned := make(chan struct{})
	go func() {
		h.svc.ApplyDefaultsInBackground(ctx)
		h.svc.ApplyDefaultsInBackground(ctx) // a second tick while the first sweep waits
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the deadline sweep blocked the ticker while the judge hung")
	}
	asked := func() int {
		h.judge.mu.Lock()
		defer h.judge.mu.Unlock()
		return h.judge.asked
	}
	for end := time.Now().Add(5 * time.Second); asked() == 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := asked(); n != 1 {
		t.Fatalf("judge asked %d times with one sweep waiting, want 1", n)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil {
		t.Fatalf("applied before the judge answered: %+v", got)
	}
	release()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil {
			return
		}
	}
	t.Fatal("not applied once the hung judge passed")
}
