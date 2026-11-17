package approvals_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// fakeActions is the team module's side of action asks: what a run's ref
// check answers, a start's refusal, and each started task's state.
type fakeActions struct {
	fakeLanes
	mu     sync.Mutex
	pin    bool
	pinErr error
	status map[string]orchcore.AskActionStatus
	ids    []string
}

func (f *fakeActions) AskActionPin(context.Context, string, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pin, f.pinErr
}

func (f *fakeActions) StartAskAction(ctx context.Context, sub *authz.Subject, st orchcore.AskActionStart) (string, error) {
	if _, err := f.fakeLanes.StartAskAction(ctx, sub, st); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "rt-" + string(rune('a'+len(f.ids)))
	f.ids = append(f.ids, id)
	return id, nil
}

func (f *fakeActions) AskActionStatus(_ context.Context, _, id string) (orchcore.AskActionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.status[id]; ok {
		return st, nil
	}
	return orchcore.AskActionStatus{State: orchapi.ActionQueued}, nil
}

func (f *fakeActions) set(id string, st orchcore.AskActionStatus) {
	f.mu.Lock()
	f.status[id] = st
	f.mu.Unlock()
}

func (f *fakeActions) refusing(err error) {
	f.fakeLanes.mu.Lock()
	f.fakeLanes.refuse = err
	f.fakeLanes.mu.Unlock()
}

// The incident this guards: an owner approved a run three times and nothing
// ran. A card that could not run is refused when asked; a run that failed
// before it started tells the approver in plain words and starts again,
// identical, without a new card, only while it provably never started,
// only by its asker or an owner, at most 3 times and within an hour of the
// yes; never once it may have run, never after an owner confirmed it, and
// never on a yes whose device is no longer paired.
func TestApprovedActionRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	fa := &fakeActions{pin: true, status: map[string]orchcore.AskActionStatus{}}
	h.svc.SetLaneHost(fa)
	it := h.workItem("publish the release")
	agent, agentAddr := h.session("abababab")
	run := func(ref, commit string) *orchapi.AskAction {
		return &orchapi.AskAction{Kind: orchapi.ActionRun, Machine: "box-a", Script: "scripts/release.sh", Ref: ref, Commit: commit}
	}
	ask := func(topic string, act *orchapi.AskAction) (orchapi.Ask, error) {
		c := card()
		c.Action = act
		return orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: topic, Card: c})
	}

	// Ask time: what the run machine would refuse never reaches a person.
	for name, act := range map[string]*orchapi.AskAction{
		"a full tag name": run("refs/tags/v1", shaA),
		"a 64-hex commit": run("main", strings.Repeat("a", 64)),
		"a short commit":  run("main", shaA[:12]),
	} {
		if _, err := ask("bad-"+strings.ReplaceAll(name, " ", "-"), act); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	fa.mu.Lock()
	fa.pinErr = api.Errorf(api.CodeBadRequest, "commit %s is not what main resolves to on the remote now", shaA)
	fa.mu.Unlock()
	if _, err := ask("moved", run("main", shaA)); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("a commit that is not the ref's head when asked: %v", err)
	}
	fa.mu.Lock()
	fa.pinErr = nil
	fa.mu.Unlock()

	a := must(ask("release", run("main", shaA)))
	if !a.Card.Action.PinChecked || !strings.Contains(a.ActionText, "at commit "+shaA+", the head of main when this was asked") {
		t.Fatalf("a checked pin: %+v %q", a.Card.Action, a.ActionText)
	}
	must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	first := fa.started()
	if len(first) != 1 || first[0].ID != "ask-"+strings.ToLower(a.ID) || first[0].Task.Run == nil || !first[0].Task.Run.CommitOnRef || first[0].Task.Run.Commit != shaA {
		t.Fatalf("first start: %+v", first)
	}

	// Its machine never took it: failed, never started, retryable for an hour.
	fa.set("rt-a", orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "box-a did not take it within 30m0s (busy, at its limit, or offline): zm cancelled it before it ran", NeverStarted: true})
	h.svc.WatchActions(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if x := got.ActionState; x.State != orchapi.ActionFailed || !x.Retryable || x.RetryUntil == nil || !x.RetryUntil.Equal(got.Decision.Time.Add(time.Hour)) {
		t.Fatalf("after the machine never took it: %+v", x)
	}
	if !strings.Contains(got.ActionStateText, "It never reached box-a") || !strings.Contains(got.ActionStateText, "zm ask retry "+a.ID) {
		t.Fatalf("state text: %q", got.ActionStateText)
	}
	h.svc.WatchActions(ctx) // the notices are sent once
	if m := h.mailTo("human:ivo", "Something you approved never reached box-a"); len(m) != 1 || !strings.Contains(m[0].Body, "nothing ran") ||
		!strings.Contains(m[0].Body, "zm ask retry "+a.ID) || !strings.Contains(m[0].Body, "did not take it within") {
		t.Fatalf("the approver's notice: %+v", m)
	}
	if m := h.mailTo(agentAddr, "ACTION FAILED"); len(m) != 1 || !strings.Contains(m[0].Body, "zm ask retry "+a.ID) {
		t.Fatalf("the asker's notice: %+v", m)
	}

	// Who and what: never another session, never another card.
	other, _ := h.session("cdcdcdcd")
	if _, err := orchclient.RetryAction(ctx, other, "", a.ID, a.CardSHA); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a session that did not ask: %v", err)
	}
	if _, err := orchclient.RetryAction(ctx, agent, "", a.ID, strings.Repeat("0", 64)); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("another card's sha: %v", err)
	}
	got = must(orchclient.RetryAction(ctx, agent, "", a.ID, a.CardSHA))
	if got.ActionState.State != orchapi.ActionQueued || got.ActionState.Retries != 1 {
		t.Fatalf("after the retry: %+v", got.ActionState)
	}
	second := fa.started()
	if len(second) != 2 || second[1].ID != "ask-"+strings.ToLower(a.ID)+"-1" {
		t.Fatalf("the retry's start: %+v", second)
	}
	again, was := second[1], first[0]
	again.ID, was.ID = "", ""
	if !reflect.DeepEqual(again, was) {
		t.Fatalf("the retry is not the identical action:\n%+v\n%+v", again, was)
	}
	if _, err := orchclient.RetryAction(ctx, agent, "", a.ID, a.CardSHA); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a retry while it waits: %v", err)
	}

	// A late read of the first attempt never fails the second.
	h.svc.WatchActions(ctx)
	if got = must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.ActionState.State != orchapi.ActionQueued || got.ActionState.Task != "rt-b" {
		t.Fatalf("the first attempt's refusal reached the second: %+v", got.ActionState)
	}
	// It may have run (its machine was lost): never again.
	fa.set("rt-b", orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "its machine was lost"})
	h.svc.WatchActions(ctx)
	if got = must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.ActionState.State != orchapi.ActionFailed || got.ActionState.Retryable {
		t.Fatalf("after a run that may have started: %+v", got.ActionState)
	}
	if _, err := orchclient.RetryAction(ctx, agent, "", a.ID, a.CardSHA); !api.IsCode(err, api.CodeConflict) || len(fa.started()) != 2 {
		t.Fatalf("a retry after it may have run: %v (%d starts)", err, len(fa.started()))
	}
	if m := h.mailTo("human:ivo", "does not run again as it is"); len(m) != 1 || !strings.Contains(m[0].Body, "may have changed things") || strings.Contains(m[0].Body, "nothing ran") {
		t.Fatalf("the approver's notice after a run that may have started: %+v", m)
	}

	// Refused when it was handed over: the owner retries under the same
	// request id (the line answers any start it has, never a second one).
	fa.refusing(api.Errorf(api.CodeForbidden, "box-a takes no tasks from other machines"))
	b := must(ask("release-two", run("main", shaB)))
	must(orchclient.Decide(ctx, h.device(ipPhone), b.ID, "a"))
	if got = must(orchclient.GetAsk(ctx, h.owner, b.ID)); got.ActionState.State != orchapi.ActionFailed || !got.ActionState.Retryable || got.ActionState.Task != "" {
		t.Fatalf("a refused hand-over: %+v", got.ActionState)
	}
	fa.refusing(nil)
	n := len(fa.started())
	must(orchclient.RetryAction(ctx, h.owner, "", b.ID, b.CardSHA))
	if st := fa.started(); len(st) != n+1 || st[n].ID != "ask-"+strings.ToLower(b.ID) {
		t.Fatalf("the owner's retry of a refused hand-over: %+v", st[n:])
	}
	// At most 3 retries.
	for i := 2; i <= 4; i++ {
		task := must(orchclient.GetAsk(ctx, h.owner, b.ID)).ActionState.Task
		fa.set(task, orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "never taken", NeverStarted: true})
		h.svc.WatchActions(ctx)
		_, err := orchclient.RetryAction(ctx, h.owner, "", b.ID, b.CardSHA)
		if i <= 3 && err != nil || i == 4 && !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("retry %d: %v", i, err)
		}
	}

	// A confirmed failure, an old yes and an unpaired device end it.
	c := must(ask("release-three", run("main", shaA)))
	must(orchclient.Decide(ctx, h.device(ipPhone), c.ID, "a"))
	task := must(orchclient.GetAsk(ctx, h.owner, c.ID)).ActionState.Task
	fa.set(task, orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "never taken", NeverStarted: true})
	h.svc.WatchActions(ctx)
	must(orchclient.ConfirmAction(ctx, h.owner, "", c.ID))
	if _, err := orchclient.RetryAction(ctx, agent, "", c.ID, c.CardSHA); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a retry after an owner confirmed the failure: %v", err)
	}
	d := must(ask("release-four", run("main", shaB)))
	must(orchclient.Decide(ctx, h.device(ipPhone), d.ID, "a"))
	task = must(orchclient.GetAsk(ctx, h.owner, d.ID)).ActionState.Task
	fa.set(task, orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "never taken", NeverStarted: true})
	h.svc.WatchActions(ctx)
	h.clock.add(61 * time.Minute)
	if _, err := orchclient.RetryAction(ctx, agent, "", d.ID, d.CardSHA); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a retry more than an hour after the yes: %v", err)
	}
	e := must(ask("release-five", run("main", shaA)))
	must(orchclient.Decide(ctx, h.device(ipPhone), e.ID, "a"))
	task = must(orchclient.GetAsk(ctx, h.owner, e.ID)).ActionState.Task
	fa.set(task, orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "never taken", NeverStarted: true})
	h.svc.WatchActions(ctx)
	// Another device paired under the same name since is not the yes's.
	h.pair("bea", ipTablet)
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "bea"))
	must(orchclient.RemoveApprover(ctx, h.owner, "ivo"))
	h.pair("ivo", ipPhone2)
	if ap := must(orchclient.ConfirmApprover(ctx, h.device(ipTablet), "ivo")); ap.State != orchapi.ApproverActive {
		t.Fatalf("the new ivo device: %+v", ap)
	}
	n = len(fa.started())
	must(orchclient.RetryAction(ctx, agent, "", e.ID, e.CardSHA))
	got = must(orchclient.GetAsk(ctx, h.owner, e.ID))
	if len(fa.started()) != n || got.ActionState.State != orchapi.ActionFailed || got.ActionState.Retryable || !strings.Contains(got.ActionState.Reason, "no longer paired") {
		t.Fatalf("a retry on an unpaired device's yes: %+v (%d starts)", got.ActionState, len(fa.started())-n)
	}
}
