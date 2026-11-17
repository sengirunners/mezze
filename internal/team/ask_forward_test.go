package team_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// zm-j63j: a member's ask waits out the card judge at the authority, not
// teamlink's CallTimeout; one whose answer is cut after it went out says it
// may have been stored, never "unreachable".
func TestAskForwardWaitsForTheCardJudge(t *testing.T) {
	if team.AskAnswerWithin() <= orchapi.CardJudgeWait {
		t.Fatalf("a forwarded ask waits %v, under the card judge's %v", team.AskAnswerWithin(), orchapi.CardJudgeWait)
	}
	if approvals.CardJudgeWait != orchapi.CardJudgeWait {
		t.Fatalf("approvals waits %v for the judge, orchapi says %v", approvals.CardJudgeWait, orchapi.CardJudgeWait)
	}
	old := teamlink.CallTimeout
	teamlink.CallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { teamlink.CallTimeout = old })

	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	sess := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "aaaaaaaa-uuid"}))(t)
	agent := at(r.owner, "harbor", sess.Address)
	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	slow := func(d time.Duration) judge.Fake {
		return judge.Fake{Fn: func(string) (json.RawMessage, error) {
			time.Sleep(d)
			return json.RawMessage(`{"decidable":true,"person_step":false,"step_carried":false,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`), nil
		}}
	}
	card := func(q string) *orchapi.AskCard {
		return &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
			Question: q, Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
			Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	}

	// A judge slower than CallTimeout: the ask is answered, once.
	svc.SetJudge(slow(time.Second))
	ask, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "holiday", Card: card("Who gets the holiday?")})
	if err != nil {
		t.Fatalf("an ask judged for longer than CallTimeout: %v", err)
	}
	if got := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t); got.ID != ask.ID {
		t.Fatalf("at the authority: %+v", got)
	}

	// An answer cut after the ask went out: it may have been stored.
	defer team.SetAskAnswerWithin(300 * time.Millisecond)()
	svc.SetJudge(slow(1500 * time.Millisecond))
	defer svc.SetJudge(nil)
	_, err = orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "rota", Card: card("Who covers the rota?")})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "may have been stored") || strings.Contains(e.Message, "unreachable") ||
		!strings.Contains(e.Hint, "zm ask ls --open") {
		t.Fatalf("a cut ask: %+v", err)
	}
	var data api.NoQueueData
	if json.Unmarshal(e.Data, &data) != nil || data.Outcome != api.OutcomeUnknown {
		t.Fatalf("a cut ask's data: %s", e.Data)
	}
}
