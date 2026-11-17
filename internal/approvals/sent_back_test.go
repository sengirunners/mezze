package approvals_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// "Need more context" sends an ask back: nothing is decided or
// written on the work item; the asker gets the note, else the item's holder.
func TestNeedsContextSendsTheAskBack(t *testing.T) {
	const note = "which residents are affected?"
	for _, tc := range []struct {
		name       string
		viaOwner   bool
		askerEnded bool
		holderMail bool
	}{
		{"device, asker live", false, false, false},
		{"owner, asker live", true, false, false},
		{"device, asker ended: the holder gets it", false, true, true},
		{"owner, asker ended: the holder gets it", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.pair("ivo", ipPhone)
			it := h.workItem("trade picks")
			agent, addr := h.session("cccc0001")
			holder, holderAddr := h.session("cccc0002")
			a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
			must(orchclient.Claim(ctx, holder, it.ID, orchapi.ClaimRequest{}))
			if tc.askerEnded {
				must(orchclient.Register(ctx, h.ownerIn("harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "cccc0001-uuid", End: true}))
			}
			req := orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext, Text: note}
			var d orchapi.Decision
			var err error
			if tc.viaOwner {
				d, err = orchclient.Choose(ctx, h.owner, "", a.ID, req)
			} else {
				d, err = orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, req)
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Decision != orchapi.DecisionNeedsContext || d.Text != note || d.MailError != "" || d.WriteError != "" || d.RecordedAt == nil {
				t.Fatalf("decision: %+v", d)
			}
			// No write-back: nothing lands on the work item, and no OWNER DECISION is mailed.
			if cs := h.comments(it.ID); len(cs) != 0 {
				t.Fatalf("comments on the item: %q", cs)
			}
			if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded || got.Decision == nil || got.Decision.Decision != orchapi.DecisionNeedsContext {
				t.Fatalf("ask after: %+v", got)
			}
			// Delivery: the asker's thread carries the note; the holder only when the asker is gone.
			thread := must(orchclient.Thread(ctx, h.owner, a.ID))
			if len(thread) != 2 || !strings.HasPrefix(thread[1].Body, "SENT BACK, NOT DECIDED") || !strings.Contains(thread[1].Body, note) || strings.Contains(thread[1].Body, "OWNER DECISION") {
				t.Fatalf("thread: %+v", thread)
			}
			held := 0
			for _, m := range must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{})) {
				if m.To == holderAddr && strings.Contains(m.Body, note) {
					held++
				}
				if strings.Contains(m.Body, "OWNER DECISION") {
					t.Errorf("mail reads as an owner decision: %q", m.Body)
				}
			}
			if want := map[bool]int{false: 0, true: 1}[tc.holderMail]; held != want {
				t.Fatalf("holder mails with the note: %d, want %d (asker %s)", held, want, addr)
			}
			// Sent back once; any other answer is a conflict, as for any decision.
			if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeConflict {
				t.Fatalf("a second answer: %v", err)
			}
		})
	}
}

func TestNeedsContextNeedsANote(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("cccc0003")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()}))
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext}); code(err) != api.CodeBadRequest {
		t.Fatalf("no note: %v", err)
	}
	c := card()
	c.Choices[0].Key = orchapi.DecisionNeedsContext
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c, Topic: "reserved-key"}); code(err) != api.CodeBadRequest {
		t.Fatalf("a card using the key: %v", err)
	}
}

// A sent-back ask frees its question to be asked again, is not a ruling to
// join or cite, and cannot be sent back over a default that stands.
func TestSentBackAskCanBeAskedAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("ask twice")
	agent, _ := h.session("cccc0004")
	other, _ := h.session("cccc0005")
	otherItem := h.workItem("joiner")
	req := orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}
	a := must(orchclient.NewAsk(ctx, agent, req))
	must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext, Text: "which residents?"}))
	if _, err := orchclient.NewAsk(ctx, other, orchapi.NewAskRequest{SameAs: a.ID, Subject: orchapi.AskSubject{Work: otherItem.ID}}); code(err) != api.CodeConflict {
		t.Fatalf("joining a sent-back ask: %v", err)
	} else if strings.Contains(err.Error(), "AGENT DECISION") {
		t.Fatalf("hint treats it as a ruling: %v", err)
	}
	again := must(orchclient.NewAsk(ctx, agent, req)) // same item, same default topic
	if again.ID == a.ID || again.Status != orchapi.AskOpen {
		t.Fatalf("asked again: %+v", again)
	}
}

func TestCannotSendBackOverAStandingDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _, _ := h.defaultAsk("pick a runner", ciCard(), time.Minute)
	h.clock.add(2 * time.Minute)
	h.svc.ApplyDefaults(ctx)
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext, Text: "why?"}); code(err) != api.CodeConflict {
		t.Fatalf("send back over a default: %v", err)
	}
	must(orchclient.UndoDefault(ctx, h.owner, "", a.ID))
	must(orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionNeedsContext, Text: "why?"}))
}
