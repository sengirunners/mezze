package approvals_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A card the judge cannot check shows in Decisions at once for the project's
// owners, marked not checked, behind the open ones and with the deadline first.
func TestHeldCardListedAtOnceDeadlinesFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("outage list")
	agent, _ := h.session("a1a1a1a1")
	soon, later := h.clock.now().Add(time.Hour), h.clock.now().Add(5*time.Hour)
	ask := func(topic string, by *time.Time) orchapi.Ask {
		return must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: topic, By: by}))
	}
	none := ask("no-deadline", nil)
	lateAsk := ask("late", &later)
	soonAsk := ask("soon", &soon)
	// One the judge did pass sits ahead of the held ones.
	j.set(judgePass)
	passed := ask("passed", nil)
	j.set("")
	var ids []string
	for _, x := range must(orchclient.Decisions(ctx, h.owner, "", "")).Decisions {
		if x.Ask == nil {
			continue
		}
		ids = append(ids, x.Ask.ID)
		switch x.Ask.ID {
		case passed.ID:
			if x.Ask.NotChecked {
				t.Errorf("a checked card is marked not checked: %+v", x.Ask)
			}
		case none.ID, lateAsk.ID, soonAsk.ID:
			if !x.Ask.NotChecked || x.Ask.NotCheckedSince == nil || !strings.Contains(x.Ask.NotCheckedReason, "the provider is down") {
				t.Errorf("a held card is not marked: %+v", x.Ask)
			}
		}
	}
	want := []string{passed.ID, soonAsk.ID, lateAsk.ID, none.ID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("Decisions order %v, want %v", ids, want)
	}
	// A held card cannot be answered, by anyone.
	if _, err := orchclient.Choose(ctx, h.owner, "", soonAsk.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "held") {
		t.Fatalf("answering a held card: %v", err)
	}
}

// The notice time is a setting; an unreadable one never silences it.
func TestHeldNoticeAfterIsASetting(t *testing.T) {
	for value, minutes := range map[string]int{"5m": 5, "banana": 15, "": 15, "1s": 15} {
		t.Run(value, func(t *testing.T) {
			if value != "" {
				t.Setenv("ZM_JUDGE_NOTICE_AFTER", value)
			}
			h := newHarness(t)
			ctx := context.Background()
			h.svc.SetJudge(&switchJudge{})
			it := h.workItem("setting")
			agent, _ := h.session("b2b2b2b2")
			must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
			told := "held for over " + strconv.Itoa(minutes) + " minutes"
			h.clock.add(time.Duration(minutes-1) * time.Minute)
			h.svc.RejudgeHeld(ctx)
			if n := len(h.mailTo("owners", "held for over")); n != 0 {
				t.Fatalf("told %d times a minute early", n)
			}
			h.clock.add(2 * time.Minute)
			h.svc.RejudgeHeld(ctx)
			if ms := h.mailTo("owners", told); len(ms) != 1 || !strings.Contains(ms[0].Body, "marked not checked") {
				t.Fatalf("owners told (%q): %+v", told, h.mailTo("owners", "held for over"))
			}
		})
	}
}

// Releasing a card the judge could not check takes a signed answer: a paired
// device or a key approver. The socket owner and any session cannot, and
// zm itself never does; after a release the card is sent as a passed one, a
// later judge failure cannot withdraw it, and a repeat is the same answer.
func TestHeldCardReleaseNeedsConfirmedAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	j := &switchJudge{}
	h.svc.SetJudge(j)
	it := h.workItem("release")
	agent, _ := h.session("c3c3c3c3")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))

	sock := h.ownerIn("harbor/solver", "")
	for name, c := range map[string]interface {
		Do(context.Context, string, string, any, any) error
	}{"session": agent, "owner on the socket": sock} {
		var out orchapi.Ask
		if err := c.Do(ctx, http.MethodPost, "/v0/asks/"+a.ID+"/decide", orchapi.DecideRequest{Decision: orchapi.DecisionReleaseUnchecked}, &out); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("%s releases: %v", name, err)
		}
	}
	if _, err := orchclient.Choose(ctx, sock, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionReleaseUnchecked}); !api.IsCode(err, api.CodeConflict) ||
		!strings.Contains(err.Error(), orchapi.DecisionReleaseUnchecked) || refusalReason(err) != orchapi.ReasonHeld {
		t.Errorf("the socket owner's answer to a held card: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); !got.NotChecked || got.ReleasedUnchecked != "" {
		t.Fatalf("still held after refusals: %+v", got)
	}
	h.clock.add(2 * time.Hour) // zm never passes it by itself, however long it waits
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); !got.NotChecked {
		t.Fatalf("passed by waiting: %+v", got)
	}

	got := must(orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), a.ID))
	if got.NotChecked || got.ReleasedUnchecked != "ivo" || got.Decision != nil || got.Status != orchapi.AskOpen {
		t.Fatalf("released: %+v", got)
	}
	if len(h.mailTo("human:ivo", "last year's swaps"))+len(h.mailTo("owners", "last year's swaps")) == 0 {
		t.Fatal("a released card was not sent")
	}
	if again := must(orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), a.ID)); again.ReleasedUnchecked != "ivo" {
		t.Fatalf("a repeat release: %+v", again)
	}
	// The judge, back and failing the card, cannot take back what an approver released.
	j.set(judgeFail)
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskOpen || got.ReleasedUnchecked != "ivo" {
		t.Fatalf("withdrawn after release: %+v", got)
	}
	// It is an ordinary open card now.
	if d := must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a"})); d.Decision != "a" {
		t.Fatalf("decision after release: %+v", d)
	}
	// A card that is not held cannot be "released".
	j.set(judgePass)
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "checked"}))
	if _, err := orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), b.ID); err == nil {
		t.Fatal("released a card that was checked")
	}
}

// A key approver's signed release, for exactly this card.
func TestHeldCardReleasedByKeyApprover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	h.svc.SetJudge(&switchJudge{})
	k := newKeyDevice(t)
	socket := h.ownerIn("harbor/solver", "")
	p := must(orchclient.PairKey(ctx, socket, "", k.pairReq(t, must(orchclient.AddApprover(ctx, socket, "ivo-z3")).Code)))
	var confirmed orchapi.Approver
	if err := h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvers/"+p.Name+"/confirm", orchapi.ConfirmRequest{Node: "key:" + p.Key}, &confirmed); err != nil {
		t.Fatal(err)
	}
	it := h.workItem("key release")
	agent, _ := h.session("d4d4d4d4")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	other := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "other"}))

	if _, err := orchclient.ReleaseHeldSigned(ctx, socket, "", a.ID, k.answer(t, p.Name, h.goodAnswer(other, orchapi.DecisionReleaseUnchecked))); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("another ask's signature: %v", err)
	}
	stale := h.goodAnswer(a, orchapi.DecisionReleaseUnchecked)
	stale.CardSHA = "0000"
	if _, err := orchclient.ReleaseHeldSigned(ctx, socket, "", a.ID, k.answer(t, p.Name, stale)); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a signature for another card: %v", err)
	}
	if _, err := orchclient.ReleaseHeldSigned(ctx, agent, "", a.ID, newKeyDevice(t).answer(t, p.Name, h.goodAnswer(a, orchapi.DecisionReleaseUnchecked))); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("another key: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); !got.NotChecked {
		t.Fatalf("released by a refused answer: %+v", got)
	}
	got := must(orchclient.ReleaseHeldSigned(ctx, socket, "", a.ID, k.answer(t, p.Name, h.goodAnswer(a, orchapi.DecisionReleaseUnchecked))))
	if got.NotChecked || got.ReleasedUnchecked != "ivo-z3" {
		t.Fatalf("released by key: %+v", got)
	}
}

// A default never applies to a card the judge has not checked: not while it
// is held, and not after an approver released it unchecked.
func TestHeldCardDefaultNeverApplies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	var mu sync.Mutex
	cardJudgeUp := false
	h.svc.SetJudge(judge.Fake{Fn: func(prompt string) (json.RawMessage, error) {
		if strings.Contains(prompt, "wants its recommended choice applied on its own") {
			return json.RawMessage(`{"pass":true,"fixes":[],"reason":"Every choice is reversible."}`), nil
		}
		mu.Lock()
		defer mu.Unlock()
		if cardJudgeUp {
			return json.RawMessage(judgePass), nil
		}
		return nil, context.DeadlineExceeded
	}})
	a, _, _ := h.defaultAsk("held default", ciCard(), time.Minute)
	if !must(orchclient.GetAsk(ctx, h.owner, a.ID)).NotChecked {
		t.Fatal("the card was not held")
	}
	h.clock.add(time.Hour)
	h.svc.ApplyDefaults(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil {
		t.Fatalf("a default applied while held: %+v", got)
	}
	must(orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), a.ID))
	h.clock.add(24 * time.Hour)
	h.svc.ApplyDefaults(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.DefaultApplied != nil || got.ReleasedUnchecked != "ivo" || got.ApplyDefault {
		t.Fatalf("a default applied to a card released unchecked: %+v", got)
	}
}

// A release that lands while the judge is still reading the card stands: the
// fail that follows withdraws nothing.
func TestHeldCardReleasedWhileJudging(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	h.svc.SetJudge(&switchJudge{})
	it := h.workItem("release while judging")
	agent, _ := h.session("e5e5e5e5")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	var once sync.Once
	h.svc.SetJudge(judge.Fake{Fn: func(string) (json.RawMessage, error) {
		once.Do(func() { must(orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), a.ID)) })
		return json.RawMessage(judgeFail), nil
	}})
	h.clock.add(time.Hour)
	h.svc.RejudgeHeld(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskOpen || got.ReleasedUnchecked != "ivo" {
		t.Fatalf("withdrawn by a fail that came after the release: %+v", got)
	}
}

// A card an approver released unchecked is never joined (a joiner skips the
// judge and would share a card nobody checked), and no card may name the
// release token as its own choice.
func TestReleasedUncheckedCardIsNotJoined(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	h.svc.SetJudge(&switchJudge{})
	it := h.workItem("join after release")
	agent, _ := h.session("f6f6f6f6")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	must(orchclient.ReleaseHeldOnDevice(ctx, h.device(ipPhone), a.ID))
	other, _ := h.session("a7a7a7a7")
	if _, err := orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{SameAs: a.ID}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "without the card judge") {
		t.Fatalf("joining a card released unchecked: %v", err)
	}
	c := card()
	c.Choices[0].Key = orchapi.DecisionReleaseUnchecked
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c, Topic: "reserved"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a choice named like the release: %v", err)
	}
}
