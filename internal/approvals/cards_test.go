package approvals_test

// Decisions as note cards (SPEC 16): non-PR subjects, the receipt binding,
// the answer landing on the work item, key approvers, and the inbox.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

func card() *orchapi.AskCard {
	return &orchapi.AskCard{
		Context:    "Residents can swap call shifts, and last year's swaps still sit in the new schedule.",
		Background: "Three of them fall on holidays.",
		Question:   "Should we keep last year's swaps?",
		Choices: []orchapi.AskChoice{
			{Label: "Keep them", Pros: "nobody is surprised", Cons: "two people work two holidays"},
			{Label: "Drop them", Pros: "fair again", Cons: "residents must redo plans"},
		},
		Recommend: "a", Why: "Keeping them breaks no promise; the holiday load evens out next year.",
		Searched: "past rulings and the program's comments: nothing on this",
	}
}

func (h *harness) workItem(title string, labels ...string) orchapi.WorkItem {
	h.t.Helper()
	it, err := orchclient.CreateWork(context.Background(), h.ownerIn("harbor/solver", ""), orchapi.CreateWorkRequest{Title: title, Labels: labels})
	if err != nil {
		h.t.Fatal(err)
	}
	return it
}

func (h *harness) comments(id string) []string {
	h.t.Helper()
	cs := must(orchclient.Comments(context.Background(), h.ownerIn("harbor", ""), id))
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

// An answer on a work item lands on it, signed and bound to that item, though
// the session that asked has ended (the strandings of 2026-11-01).
func TestAnswerLandsOnWorkAfterAskerEnded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("trade picks")
	agent, addr := h.session("aaaaaaaa")
	a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Priority: new(1)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject.Work != it.ID || a.Area != "solver" || a.Status != orchapi.AskOpen || a.AskedBy != addr || a.Priority != 1 ||
		len(a.Options) != 2 || a.Options[0] != "a" || !strings.Contains(a.Body, "A) Keep them") || a.CardSHA == "" {
		t.Fatalf("ask: %+v", a)
	}
	if got := must(orchclient.Register(ctx, h.ownerIn("harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "aaaaaaaa-uuid", End: true})); got.State != orchapi.StateEnded || got.Address != addr {
		t.Fatalf("the asker: %+v", got)
	}
	d, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "b", Text: "only these three"})
	if err != nil {
		t.Fatal(err)
	}
	if d.RecordedAt == nil || d.WriteError != "" || d.Answer != "B) Drop them [only these three]" || d.MailError != "" {
		t.Fatalf("decision: %+v", d)
	}
	cs := h.comments(it.ID)
	if len(cs) != 1 || !strings.HasPrefix(cs[0], "OWNER DECISION (ivo via \"ivo-phone\", ") ||
		!strings.Contains(cs[0], "B) Drop them [only these three]") || !strings.Contains(cs[0], "Question (asker supplied): \"Should we keep") ||
		!strings.Contains(cs[0], "zm ask receipt "+a.ID) {
		t.Fatalf("comments: %q", cs)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Status != orchapi.AskRecorded || got.Decision == nil || got.Decision.Text != "only these three" {
		t.Fatalf("ask after: %+v", got)
	}

	// The receipt: version 2, bound to the item, what was read and answered.
	pub := must(h.svc.PublicKey())
	rc := d.Receipt
	if rc.V != orchapi.ReceiptVersion2 || rc.Subject.Work != it.ID || rc.CardSHA != a.CardSHA || rc.AnsweredAs != "owner" || rc.TextSHA == "" {
		t.Fatalf("receipt: %+v", rc)
	}
	if c := approvals.Verify(rc, pub, orchapi.ReceiptExpect{Work: it.ID, Decision: "b", Ask: a.ID, Project: a.Project, Approver: "ivo"}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	for name, exp := range map[string]orchapi.ReceiptExpect{
		"another work item": {Work: "local:other"},
		"another decision":  {Work: it.ID, Decision: "a"},
		"a merge gate":      {Repo: "example-org/harbor", PR: 1, HeadSHA: shaA},
		"another question":  {Question: strings.Repeat("a", 64)},
		"another ask":       {Ask: "01JZZZZZZZZZZZZZZZZZZZZZZZ"},
		"another project":   {Project: h.other.ID},
	} {
		if c := approvals.Verify(rc, pub, exp); c.Valid {
			t.Errorf("%s: a receipt for this item passed", name)
		}
	}
	for name, change := range map[string]func(*orchapi.Receipt){
		"subject":    func(r *orchapi.Receipt) { r.Subject.Work = "local:other" },
		"card":       func(r *orchapi.Receipt) { r.CardSHA = strings.Repeat("b", 64) },
		"text":       func(r *orchapi.Receipt) { r.TextSHA = strings.Repeat("b", 64) },
		"as":         func(r *orchapi.Receipt) { r.AnsweredAs = "contact" },
		"version":    func(r *orchapi.Receipt) { r.V = orchapi.ReceiptVersion },
		"decision":   func(r *orchapi.Receipt) { r.Decision = "a" },
		"pr subject": func(r *orchapi.Receipt) { r.Subject.Repo = "github.com/a/b" },
	} {
		bad := rc
		change(&bad)
		if c := approvals.Verify(bad, pub, orchapi.ReceiptExpect{}); c.Valid {
			t.Errorf("tampered %s verified", name)
		}
	}
	// The same answer returns the same receipt; another is refused.
	again, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "b", Text: "only these three"})
	if err != nil || again.Receipt.Sig != rc.Sig {
		t.Fatalf("same answer: %+v %v", again, err)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "b", Text: "something else"}); code(err) != api.CodeConflict {
		t.Fatalf("another text: %v", err)
	}
	if n := len(h.comments(it.ID)); n != 1 {
		t.Fatalf("comments after repeats: %d", n)
	}
}

// A signed version 1 receipt from before this format still verifies, byte for
// byte: this is what a CI gate holds. The signature below came from main at
// a6a0250e, the code as it stood before non-PR subjects.
func TestVersionOneReceiptBytesUnchanged(t *testing.T) {
	key := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	rc := approvals.Sign(key, orchapi.Receipt{Project: "01J9Z3K0QX7Y8AZ5V2N4B6C8D0", Ask: "01J9Z3K0QX7Y8AZ5V2N4B6C8D1",
		Subject: orchapi.AskSubject{Repo: "github.com/example-org/harbor", PR: 7, HeadSHA: shaA}, Decision: "approve",
		Approver: "ivo", Node: "nPHONE", Time: "2026-10-30T18:00:00.000Z"})
	want := `zm-receipt/1` + "\n" + `{"v":"zm-receipt/1","project":"01J9Z3K0QX7Y8AZ5V2N4B6C8D0","ask":"01J9Z3K0QX7Y8AZ5V2N4B6C8D1",` +
		`"subject":{"repo":"github.com/example-org/harbor","pr":7,"head_sha":"` + shaA + `"},"decision":"approve","approver":"ivo",` +
		`"node":"nPHONE","time":"2026-10-30T18:00:00.000Z"}`
	if got := string(approvals.SigningBytes(rc)); got != want {
		t.Fatalf("signing bytes changed:\n%s\n%s", got, want)
	}
	const golden = "Re2R+I1ka4qE8nFSFPJpbp5NYIoR8i7a0hN/AXIVjG1Oa3ZRxawd6pS9JLclNgyQqJmxD3nJzr2YhhgS5gumBw=="
	if rc.Sig != golden {
		t.Fatalf("signature changed: %s", rc.Sig)
	}
	if c := approvals.Verify(rc, key.Public().(ed25519.PublicKey), orchapi.ReceiptExpect{Repo: "example-org/harbor", PR: 7, HeadSHA: shaA, Decision: "approve"}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	// A pull request's ask still gets one, and only one, without the new fields.
	h := newHarness(t)
	h.pair("ivo", ipPhone)
	a := h.ask(h.ownerIn("harbor/solver", ""), 1, shaA)
	d := must(orchclient.Decide(context.Background(), h.device(ipPhone), a.ID, "approve"))
	if d.Receipt.V != orchapi.ReceiptVersion || d.Receipt.CardSHA != "" || d.Receipt.AnsweredAs != "" || d.RecordedAt == nil {
		t.Fatalf("pr decision: %+v", d)
	}
	if _, err := orchclient.DecideWith(context.Background(), h.device(ipPhone), h.ask(h.ownerIn("harbor/solver", ""), 2, shaA).ID,
		orchapi.DecideRequest{Decision: "approve", Text: "because"}); code(err) != api.CodeBadRequest {
		t.Fatalf("text on a PR ask: %v", err)
	}
}

// A version 2 receipt's signed bytes are a published contract (docs/clients.md
// section 5: clients verify offline): field order, the optional fields left
// out when empty, and Go's escaping of <, > and &. A change here breaks every
// offline verifier and needs a new version.
func TestVersionTwoReceiptBytesUnchanged(t *testing.T) {
	key := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	card := strings.Repeat("c", 64)
	rc := approvals.Sign(key, orchapi.Receipt{V: orchapi.ReceiptVersion2, Project: "01J9Z3K0QX7Y8AZ5V2N4B6C8D0",
		Ask: "01J9Z3K0QX7Y8AZ5V2N4B6C8D1", Subject: orchapi.AskSubject{Work: "bd:zm-1"}, Decision: "a", Approver: "ivo",
		Node: "owner:mini", Time: "2026-11-16T18:00:00.000Z", CardSHA: card, TextSHA: strings.Repeat("d", 64),
		AnsweredAs: orchapi.AnsweredAsOwner, Action: "ack-mail count=2 ids_sha256=" + strings.Repeat("e", 64) + " on=a"})
	want := `zm-receipt/2` + "\n" + `{"v":"zm-receipt/2","project":"01J9Z3K0QX7Y8AZ5V2N4B6C8D0","ask":"01J9Z3K0QX7Y8AZ5V2N4B6C8D1",` +
		`"subject":{"repo":"","pr":0,"head_sha":"","work":"bd:zm-1"},"decision":"a","approver":"ivo","node":"owner:mini",` +
		`"time":"2026-11-16T18:00:00.000Z","card_sha256":"` + card + `","text_sha256":"` + strings.Repeat("d", 64) +
		`","answered_as":"owner","action":"ack-mail count=2 ids_sha256=` + strings.Repeat("e", 64) + ` on=a"}`
	if got := string(approvals.SigningBytes(rc)); got != want {
		t.Fatalf("signing bytes changed:\n%s\n%s", got, want)
	}
	if c := approvals.Verify(rc, key.Public().(ed25519.PublicKey), orchapi.ReceiptExpect{Work: "bd:zm-1", Card: card, Decision: "a"}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	// Empty optional fields are left out, and <, >, &, U+2028 and U+2029 are escaped.
	rc.TextSHA, rc.Action, rc.Node = "", "", "owner:a<b>&c\u2028\u2029"
	want = `zm-receipt/2` + "\n" + `{"v":"zm-receipt/2","project":"01J9Z3K0QX7Y8AZ5V2N4B6C8D0","ask":"01J9Z3K0QX7Y8AZ5V2N4B6C8D1",` +
		`"subject":{"repo":"","pr":0,"head_sha":"","work":"bd:zm-1"},"decision":"a","approver":"ivo",` +
		`"node":"owner:a\u003cb\u003e\u0026c\u2028\u2029","time":"2026-11-16T18:00:00.000Z","card_sha256":"` + card + `","answered_as":"owner"}`
	if got := string(approvals.SigningBytes(rc)); got != want {
		t.Fatalf("signing bytes without optional fields:\n%s\n%s", got, want)
	}
}

// A card on a pull request's ask would hide the pull request behind plain words,
// and one tap on a choice keyed "approve" would pass a merge gate (review of PR 201).
func TestCardOnAPullRequestIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	c := card()
	c.Choices[0].Key, c.Choices[1].Key, c.Recommend = "approve", "decline", "approve"
	a, err := orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: c, Subject: orchapi.AskSubject{Repo: "example-org/harbor", PR: 3, HeadSHA: shaA}})
	if code(err) != api.CodeBadRequest {
		t.Fatalf("a card on a pull request's ask: %+v %v", a, err)
	}
}

// A plain question binds to its card; a free-text answer is "other".
func TestPlainQuestion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("bbbbbbbb")
	a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject.Question == a.CardSHA || len(a.Subject.Question) != 64 || a.Note != "" {
		t.Fatalf("ask: %+v", a)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: orchapi.DecisionOther}); code(err) != api.CodeBadRequest {
		t.Fatalf("other with no text: %v", err)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "z"}); code(err) != api.CodeBadRequest {
		t.Fatalf("not a choice: %v", err)
	}
	if _, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a", As: "contact"}); code(err) != api.CodeBadRequest {
		t.Fatalf("contact with no contact: %v", err)
	}
	d, err := orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Text: "ask the chiefs first"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != orchapi.DecisionOther || d.Answer != "ask the chiefs first" || d.RecordedAt == nil {
		t.Fatalf("decision: %+v", d)
	}
	if c := approvals.Verify(d.Receipt, must(h.svc.PublicKey()), orchapi.ReceiptExpect{Question: a.Subject.Question, Card: a.CardSHA}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	// A client cannot name its own question hash.
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Subject: orchapi.AskSubject{Question: a.CardSHA}}); code(err) != api.CodeBadRequest {
		t.Fatalf("named question: %v", err)
	}
}

// The gate: what no note card may be.
func TestCardGate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("cccccccc")
	change := func(f func(*orchapi.AskCard)) *orchapi.AskCard { c := card(); f(c); return c }
	for name, c := range map[string]*orchapi.AskCard{
		"no context":  change(func(c *orchapi.AskCard) { c.Context = " " }),
		"no question": change(func(c *orchapi.AskCard) { c.Question = "" }),
		"no why":      change(func(c *orchapi.AskCard) { c.Why = "" }),
		"no search":   change(func(c *orchapi.AskCard) { c.Searched = "nothing" }),
		"one choice":  change(func(c *orchapi.AskCard) { c.Choices = c.Choices[:1] }),
		"five":        change(func(c *orchapi.AskCard) { c.Choices = append(c.Choices, c.Choices[0], c.Choices[0], c.Choices[0]) }),
		"bad rec":     change(func(c *orchapi.AskCard) { c.Recommend = "z" }),
		"no label":    change(func(c *orchapi.AskCard) { c.Choices[0].Label = "" }),
		"taken key":   change(func(c *orchapi.AskCard) { c.Choices[0].Key = "other" }),
		"same key":    change(func(c *orchapi.AskCard) { c.Choices[0].Key = "x"; c.Choices[1].Key = "x" }),
		"too long": change(func(c *orchapi.AskCard) {
			c.Context, c.Background = strings.Repeat("word ", approvals.MaxCardWords/2), strings.Repeat("word ", approvals.MaxCardWords/2)
		}),
		"a path":     change(func(c *orchapi.AskCard) { c.Context = "see /src/solver/main for it" }),
		"a file":     change(func(c *orchapi.AskCard) { c.Context = "see solver.go for it" }),
		"a hash":     change(func(c *orchapi.AskCard) { c.Why = "since deadbee1 landed" }),
		"a pr":       change(func(c *orchapi.AskCard) { c.Choices[1].Label = "merge #123" }),
		"an env var": change(func(c *orchapi.AskCard) { c.Background = "set MAX_SOLVE_TIME" }),
		"a work id":  change(func(c *orchapi.AskCard) { c.Context = "harbor-z9yg1 is stuck" }),
	} {
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c}); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: change(func(c *orchapi.AskCard) { c.Background = "the harbor-native screen" })}); err != nil {
		t.Errorf("a word that only looks like an id: %v", err)
	}
	for name, req := range map[string]orchapi.NewAskRequest{
		"no subject, no card": {Body: "merge?"},
		"work, no card":       {Body: "merge?", Subject: orchapi.AskSubject{Work: "local:x"}},
		"pr and work":         {Card: card(), Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA, Work: "local:x"}},
		"bad topic":           {Card: card(), Topic: "No Spaces"},
		"past deadline":       {Card: card(), By: new(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))},
		"priority":            {Card: card(), Priority: new(9)},
		"program, no topic":   {Card: card(), For: &orchapi.AskFor{Group: "SM", Contact: "Dr. R"}},
		"program, deadline":   {Card: card(), Topic: "abc", For: &orchapi.AskFor{Group: "SM", Contact: "Dr. R"}, By: new(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
	} {
		if _, err := orchclient.NewAsk(ctx, agent, req); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A work item the caller cannot see answers exactly like a missing one.
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token, guest.Project, guest.ProjectFrom = g.Secret, "harbor", "env"
	hidden, err := orchclient.CreateWork(ctx, h.ownerIn("harbor", ""), orchapi.CreateWorkRequest{Title: "HIDDEN"})
	if err != nil {
		t.Fatal(err)
	}
	_, eh := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: card(), Subject: orchapi.AskSubject{Work: hidden.ID}})
	_, em := orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Card: card(), Subject: orchapi.AskSubject{Work: "local:nope"}})
	if code(eh) != api.CodeNotFound || code(em) != api.CodeNotFound {
		t.Fatalf("hidden %v, missing %v", eh, em)
	}
}

// One ask per subject and topic, and per program and topic; a withdrawn one
// frees the slot, and nobody can take its mail while it is open.
func TestOneAskPerTopicAndWithdraw(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("topics")
	agent, _ := h.session("dddddddd")
	first := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict || !strings.Contains(string(e.Data), first.ID) {
		t.Fatalf("repeat: %v", err)
	}
	second := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "second-one"}))

	prog := func() (orchapi.Ask, error) {
		return orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "holiday-shifts", For: &orchapi.AskFor{Group: "Santa Monica", Contact: "Dr. R"}})
	}
	p := must(prog())
	if _, err := prog(); code(err) != api.CodeConflict {
		t.Fatalf("program repeat: %v", err)
	}
	// "John said": the approver taps in what the contact said.
	d := must(orchclient.DecideWith(ctx, h.device(ipPhone), p.ID, orchapi.DecideRequest{Decision: "a", As: orchapi.AnsweredAsContact}))
	if d.AnsweredAs != orchapi.AnsweredAsContact || d.Receipt.AnsweredAs != orchapi.AnsweredAsContact {
		t.Fatalf("contact answer: %+v", d)
	}
	if _, err := prog(); code(err) != api.CodeConflict {
		t.Fatalf("a program topic stays asked once answered: %v", err)
	}

	// Withdrawing decides nothing; only the asker or an operator may.
	otherGuest := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	g := client.WithHandler(h.dm.Handler)
	g.Token, g.Project, g.ProjectFrom = otherGuest.Secret, "harbor", "env"
	if _, err := orchclient.WithdrawAsk(ctx, g, second.ID); code(err) != api.CodeNotFound {
		t.Fatalf("guest withdraw: %v", err)
	}
	w := must(orchclient.WithdrawAsk(ctx, agent, second.ID))
	if w.Status != orchapi.AskWithdrawn || w.Decision != nil {
		t.Fatalf("withdrawn: %+v", w)
	}
	// Who withdrew is not told what they just did: the ask's mail is taken, nothing sent.
	if th := must(orchclient.Thread(ctx, h.owner, second.ID)); len(th) != 1 || th[0].AckedAt == nil {
		t.Fatalf("self-withdrawn thread %+v", th)
	}
	// An agent's ask the person withdrew: the agent is told.
	third := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "third-one"}))
	must(orchclient.WithdrawAsk(ctx, h.ownerIn("harbor/solver", ""), third.ID))
	if th := must(orchclient.Thread(ctx, h.owner, third.ID)); len(th) != 2 || !strings.HasPrefix(th[1].Body, "withdrawn by human:") {
		t.Fatalf("withdrawn-for-the-agent thread %+v", th)
	}
	if _, err := orchclient.Decide(ctx, h.device(ipPhone), second.ID, "a"); code(err) != api.CodeConflict {
		t.Fatalf("decide a withdrawn ask: %v", err)
	}
	if _, err := orchclient.WithdrawAsk(ctx, agent, p.ID); code(err) != api.CodeConflict {
		t.Fatalf("withdraw a decided ask: %v", err)
	}
	for _, a := range must(orchclient.Asks(ctx, h.owner, "", true)) {
		if a.ID == second.ID {
			t.Fatal("a withdrawn ask is listed as open")
		}
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "second-one"}); err != nil {
		t.Fatalf("after withdraw: %v", err)
	}
	// The open ask's mail is still nobody's to take.
	if _, err := orchclient.Ack(ctx, h.owner, first.ID); code(err) != api.CodeForbidden {
		t.Fatalf("ack an open ask: %v", err)
	}
}

// A failed write-back shows, is retried from the row, and adds one comment.
func TestWriteBackRetries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("retry")
	agent, _ := h.session("ffffffff")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	fail := errors.New("the tracker is away")
	approvals.SetWriteBackHook(func() error { return fail })
	t.Cleanup(func() { approvals.SetWriteBackHook(nil) })
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.RecordedAt != nil || !strings.Contains(d.WriteError, "away") {
		t.Fatalf("decision: %+v", d)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskAnswered || got.Decision.WriteError == "" {
		t.Fatalf("ask: %+v", got)
	}
	if n := len(h.comments(it.ID)); n != 0 {
		t.Fatalf("comments: %d", n)
	}
	h.svc.RetryDeliveries(ctx) // not due yet
	h.clock.add(2 * time.Minute)
	h.svc.RetryDeliveries(ctx) // due, still failing
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskAnswered {
		t.Fatalf("ask: %+v", got)
	}
	approvals.SetWriteBackHook(nil)
	h.clock.add(2 * time.Hour)
	h.svc.RetryDeliveries(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Status != orchapi.AskRecorded || got.Decision.WriteError != "" {
		t.Fatalf("after retry: %+v", got)
	}
	h.clock.add(2 * time.Hour)
	h.svc.RetryDeliveries(ctx)
	if n := len(h.comments(it.ID)); n != 1 {
		t.Fatalf("comments after retries: %d", n)
	}
}

// The device page is a shell: the decisions render in app.js from the
// device view, as text, so no card text reaches the HTML. The view holds every
// decision waiting on the machine owner (a plain card too), and the device
// answers a plain card with the same node-bound decide route.
func TestDevicePageShowsTheCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	c := card()
	c.Background = "<script>alert(1)</script>"
	forAsk := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: c, Topic: "holiday-shifts", For: &orchapi.AskFor{Group: "Santa Monica", Contact: "Dr. R"}}))
	plain := card()
	plain.Title = "Plain card title"
	plainAsk := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: plain, Topic: "plain"}))
	k := newKeyDevice(t)
	p := must(orchclient.PairKey(ctx, h.ownerIn("harbor/solver", ""), "", k.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "ivo-z3")).Code)))
	phone := h.device(ipPhone)
	resp, err := phone.Stream(ctx, http.MethodGet, "/v0/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}
	page := readAll(resp)
	for _, want := range []string{p.Key, `src="/v0/approvals/app.js"`, `href="/v0/approvals/manifest.webmanifest"`, `rel="apple-touch-icon"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "Plain card title") {
		t.Error("card text reached the page's HTML")
	}
	v := must(orchclient.DeviceView(ctx, phone))
	ids := map[string]bool{}
	for _, d := range v.Decisions {
		if d.Ask != nil {
			ids[d.Ask.ID] = true
		}
	}
	if !ids[forAsk.ID] || !ids[plainAsk.ID] || v.Identity == "" {
		t.Fatalf("device view: identity %q, decisions %+v", v.Identity, v.Decisions)
	}
	d := must(orchclient.Decide(ctx, phone, plainAsk.ID, "a"))
	if d.Decision != "a" {
		t.Fatalf("plain card decision: %+v", d)
	}
}

// ---- key approvers ----

type keyDevice struct {
	priv *ecdsa.PrivateKey
	pub  string
}

func newKeyDevice(t *testing.T) keyDevice {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return keyDevice{k, base64.StdEncoding.EncodeToString(ec)}
}

func (k keyDevice) sign(t *testing.T, msg []byte) string {
	t.Helper()
	h := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func (k keyDevice) pairReq(t *testing.T, code string) orchapi.PairKeyRequest {
	t.Helper()
	norm := strings.ToUpper(strings.ReplaceAll(code, "-", ""))
	return orchapi.PairKeyRequest{Code: code, PublicKey: k.pub, Device: "Mezze app on the Mac",
		Proof: k.sign(t, []byte("zm-pair-key/1\n"+norm+"\n"+k.pub))}
}

func (k keyDevice) answer(t *testing.T, approver string, ans orchapi.KeyAnswer) orchapi.SignedKeyAnswer {
	t.Helper()
	ans.V = orchapi.KeyAnswerVersion
	b, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	return orchapi.SignedKeyAnswer{Approver: approver, Payload: base64.StdEncoding.EncodeToString(b),
		Sig: k.sign(t, append([]byte(orchapi.KeyAnswerVersion+"\n"), b...))}
}

func (h *harness) goodAnswer(a orchapi.Ask, decision string) orchapi.KeyAnswer {
	return orchapi.KeyAnswer{Daemon: approvals.KeyID(must(h.svc.PublicKey())), Project: a.Project, Ask: a.ID, CardSHA: a.CardSHA,
		Decision: decision, Time: h.clock.now().Format(time.RFC3339)}
}

// The proof the owner asked for: an agent on the socket holds every right the
// socket has, and still cannot answer its own question, however it tries.
func TestAgentOnTheSocketCannotAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("agent tries")
	agent, _ := h.session("11111111")
	sock := h.ownerIn("harbor/solver", "") // the owner on the socket, no session
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	open := func(when string) {
		t.Helper()
		if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
			t.Fatalf("%s: decided: %+v", when, got.Decision)
		}
	}
	forbidden := func(what string, err error) {
		t.Helper()
		if code(err) != api.CodeForbidden {
			t.Errorf("%s: %v", what, err)
		}
		open(what)
	}
	// The decide route, the ask's mail.
	for name, c := range map[string]*client.Client{"session": agent, "owner": sock} {
		_, err := orchclient.Decide(ctx, c, a.ID, "a")
		forbidden(name+" decide", err)
		_, err = orchclient.Ack(ctx, c, a.ID)
		forbidden(name+" ack", err)
		_, err = orchclient.Reply(ctx, c, a.ID, orchapi.ReplyRequest{Body: "a"})
		forbidden(name+" reply", err)
	}
	// The agent's own key, unpaired.
	k := newKeyDevice(t)
	_, err := orchclient.AnswerSigned(ctx, agent, "", a.ID, k.answer(t, "ivo", h.goodAnswer(a, "a")))
	forbidden("an unknown approver", err)
	// The agent adds an approver and pairs its key: the socket may do that. No
	// bootstrap: it stays unconfirmed, first approver or not.
	add := must(orchclient.AddApprover(ctx, sock, "sneaky"))
	p := must(orchclient.PairKey(ctx, agent, "", k.pairReq(t, add.Code)))
	if p.State != orchapi.ApproverUnconfirmed || p.Key == "" || !strings.HasPrefix(p.Key, "p256:") || !strings.Contains(p.Note, "phone") {
		t.Fatalf("paired: %+v", p)
	}
	_, err = orchclient.AnswerSigned(ctx, agent, "", a.ID, k.answer(t, "sneaky", h.goodAnswer(a, "a")))
	forbidden("an unconfirmed key", err)
	// It cannot confirm itself: the route refuses the socket, and tokens.
	_, err = orchclient.ConfirmApprover(ctx, sock, "sneaky")
	forbidden("confirm from the socket", err)
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "owner"}))
	viaToken := client.WithHandler(h.dm.Handler)
	viaToken.Token, viaToken.Project, viaToken.ProjectFrom = tok.Secret, "harbor", "env"
	_, err = orchclient.ConfirmApprover(ctx, viaToken, "sneaky")
	forbidden("confirm with a token", err)
	_, err = orchclient.Decide(ctx, viaToken, a.ID, "a")
	forbidden("decide with a token", err)
	_, err = orchclient.AnswerSigned(ctx, viaToken, "", a.ID, k.answer(t, "sneaky", h.goodAnswer(a, "a")))
	forbidden("an unconfirmed key with a token", err)

	// It cannot ride in on the first device's bootstrap: pairing a phone later
	// bootstraps that phone only.
	phone := h.pair("ivo", ipPhone)
	if phone.State != orchapi.ApproverActive {
		t.Fatalf("phone: %+v", phone)
	}
	if got := must(orchclient.Approvers(ctx, h.owner)); len(got) != 2 {
		t.Fatalf("approvers: %+v", got)
	}
	_, err = orchclient.AnswerSigned(ctx, agent, "", a.ID, k.answer(t, "sneaky", h.goodAnswer(a, "a")))
	forbidden("still unconfirmed", err)
	// Claiming to be the phone's approver with another key fails too.
	_, err = orchclient.AnswerSigned(ctx, agent, "", a.ID, k.answer(t, "ivo", h.goodAnswer(a, "a")))
	forbidden("the phone's name", err)
	// The device page shows the person the fingerprint they must compare.
	v := must(orchclient.DeviceView(ctx, h.device(ipPhone)))
	if len(v.Confirm) != 1 || v.Confirm[0].Key != p.Key {
		t.Fatalf("confirm list: %+v", v.Confirm)
	}
}

// A confirmed key answers as its person, for exactly what was signed.
func TestKeyApproverAnswers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	k := newKeyDevice(t)
	agent, _ := h.session("22222222")
	socket := h.ownerIn("harbor/solver", "")
	add := must(orchclient.AddApprover(ctx, socket, "ivo-z3"))
	if p, err := orchclient.PairKey(ctx, agent, "", orchapi.PairKeyRequest{Code: add.Code, PublicKey: k.pub, Proof: k.sign(t, []byte("not it"))}); code(err) != api.CodeForbidden {
		t.Fatalf("wrong proof: %+v %v", p, err)
	}
	p := must(orchclient.PairKey(ctx, socket, "", k.pairReq(t, add.Code)))
	var confirmed orchapi.Approver
	if err := h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvers/"+p.Name+"/confirm", orchapi.ConfirmRequest{Node: "key:" + p.Key}, &confirmed); err != nil || confirmed.State != orchapi.ApproverActive {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	it := h.workItem("answered by key")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	other := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "another-one"}))

	refused := map[string]orchapi.SignedKeyAnswer{}
	change := func(f func(*orchapi.KeyAnswer)) orchapi.SignedKeyAnswer {
		x := h.goodAnswer(a, "a")
		f(&x)
		return k.answer(t, p.Name, x)
	}
	refused["another ask's answer"] = k.answer(t, p.Name, h.goodAnswer(other, "a"))
	refused["another daemon"] = change(func(x *orchapi.KeyAnswer) { x.Daemon = "zm:0000000000000000" })
	refused["an old answer"] = change(func(x *orchapi.KeyAnswer) { x.Time = h.clock.now().Add(-6 * time.Minute).Format(time.RFC3339) })
	refused["a future answer"] = change(func(x *orchapi.KeyAnswer) { x.Time = h.clock.now().Add(6 * time.Minute).Format(time.RFC3339) })
	tampered := k.answer(t, p.Name, h.goodAnswer(a, "a"))
	tampered.Payload = base64.StdEncoding.EncodeToString([]byte(`{"v":"zm-answer/1","decision":"b"}`))
	refused["tampered bytes"] = tampered
	wrongKey := newKeyDevice(t).answer(t, p.Name, h.goodAnswer(a, "a"))
	refused["another key"] = wrongKey
	for name, in := range refused {
		if _, err := orchclient.AnswerSigned(ctx, socket, "", a.ID, in); code(err) != api.CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := orchclient.AnswerSigned(ctx, socket, "", a.ID, change(func(x *orchapi.KeyAnswer) { x.CardSHA = strings.Repeat("0", 64) })); code(err) != api.CodeConflict {
		t.Errorf("another card: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
		t.Fatalf("decided by a refused answer: %+v", got.Decision)
	}

	d, err := orchclient.AnswerSigned(ctx, socket, "", a.ID, k.answer(t, p.Name, h.goodAnswer(a, "b")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Approver != "ivo-z3" || d.Receipt.Node != "key:"+p.Key || d.RecordedAt == nil {
		t.Fatalf("decision: %+v", d)
	}
	if c := approvals.Verify(d.Receipt, must(h.svc.PublicKey()), orchapi.ReceiptExpect{Work: it.ID, Ask: a.ID, Card: a.CardSHA, Decision: "b"}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	if cs := h.comments(it.ID); len(cs) != 1 || !strings.Contains(cs[0], "ivo-z3 via \"Mezze app on the Mac\"") {
		t.Fatalf("comments: %q", cs)
	}

	// A pull request's ask stays with a device on the tailnet (a merge gate
	// trusts its receipt), whatever the key signs.
	pr := h.ask(socket, 7, shaA)
	if _, err := orchclient.AnswerSigned(ctx, socket, "", pr.ID, k.answer(t, p.Name, h.goodAnswer(pr, "approve"))); code(err) != api.CodeForbidden {
		t.Fatalf("a key on a pull request's ask: %v", err)
	}
	// A key approver cannot confirm anyone: the route refuses the socket, and
	// no tailnet caller has its node.
	add2 := must(orchclient.AddApprover(ctx, socket, "second-key"))
	k2 := newKeyDevice(t)
	must(orchclient.PairKey(ctx, socket, "", k2.pairReq(t, add2.Code)))
	if _, err := orchclient.ConfirmApprover(ctx, socket, "second-key"); code(err) != api.CodeForbidden {
		t.Fatalf("confirm from the key's socket: %v", err)
	}
	// Removal ends it at once.
	last := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "after-removal"}))
	must(orchclient.RemoveApprover(ctx, socket, p.Name))
	if _, err := orchclient.AnswerSigned(ctx, socket, "", last.ID, k.answer(t, p.Name, h.goodAnswer(last, "a"))); code(err) != api.CodeForbidden {
		t.Fatalf("a removed key: %v", err)
	}
}

// A person compares a fingerprint, then taps. An agent on the socket must not be
// able to swap the key under that name in between (review of PR 201).
func TestConfirmBindsWhatThePersonCompared(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("swap")
	agent, _ := h.session("55555555")
	sock := h.ownerIn("harbor/solver", "")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))

	real := newKeyDevice(t)
	seen := must(orchclient.PairKey(ctx, sock, "", real.pairReq(t, must(orchclient.AddApprover(ctx, sock, "ivo-z3")).Code)))
	shown := must(orchclient.DeviceView(ctx, h.device(ipPhone))).Confirm[0]
	if shown.Key != seen.Key || shown.Node == "" {
		t.Fatalf("shown: %+v", shown)
	}

	// Before the tap: rm, add, pair the agent's own key under the same name.
	must(orchclient.RemoveApprover(ctx, sock, "ivo-z3"))
	evil := newKeyDevice(t)
	swapped := must(orchclient.PairKey(ctx, agent, "", evil.pairReq(t, must(orchclient.AddApprover(ctx, sock, "ivo-z3")).Code)))
	if swapped.Key == seen.Key {
		t.Fatal("same key")
	}
	confirm := func(body map[string]string) error {
		var out orchapi.Approver
		return h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvers/ivo-z3/confirm", body, &out)
	}
	if err := confirm(map[string]string{"node": shown.Node}); code(err) != api.CodeConflict {
		t.Fatalf("a tap on the swapped key: %v", err)
	}
	if err := confirm(map[string]string{}); code(err) != api.CodeBadRequest {
		t.Fatalf("a key confirmed with nothing compared: %v", err)
	}
	if _, err := orchclient.AnswerSigned(ctx, agent, "", a.ID, evil.answer(t, "ivo-z3", h.goodAnswer(a, "a"))); code(err) != api.CodeForbidden {
		t.Fatalf("the agent's key answered: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
		t.Fatalf("decided: %+v", got.Decision)
	}
	// The same swap with a tailnet device under the name, through the page's body.
	must(orchclient.RemoveApprover(ctx, sock, "ivo-z3"))
	must(orchclient.PairKey(ctx, sock, "", real.pairReq(t, must(orchclient.AddApprover(ctx, sock, "bea-z3")).Code)))
	first := must(orchclient.DeviceView(ctx, h.device(ipPhone))).Confirm[0]
	must(orchclient.RemoveApprover(ctx, sock, "bea-z3"))
	add := must(orchclient.AddApprover(ctx, sock, "bea-z3"))
	if p := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code)); p.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("tablet: %+v", p)
	}
	var out orchapi.Approver
	if err := h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvers/bea-z3/confirm", map[string]string{"node": first.Node}, &out); code(err) != api.CodeConflict {
		t.Fatalf("a tap on the swapped device: %v", err)
	}
	// What was compared still goes through.
	now := must(orchclient.DeviceView(ctx, h.device(ipPhone))).Confirm[0]
	if err := h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvers/bea-z3/confirm", map[string]string{"node": now.Node}, &out); err != nil || out.State != orchapi.ApproverActive {
		t.Fatalf("the compared device: %+v %v", out, err)
	}
}

// ---- the inbox ----

func TestInbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("33333333")
	labelled := h.workItem("Santa Monica check", "owner:ivo")
	beas := h.workItem("Bea's check", "owner:bea")
	plain := h.workItem("not waiting on anyone", "solver")
	human := h.workItem("needs a person", "human")
	asked := h.workItem("has an ask", "owner:ivo")
	late := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: asked.ID}, Card: card()}))
	urgent := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "urgent-one", Priority: new(0)}))
	soon := h.clock.now().Add(time.Hour)
	deadline := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "deadline-one", By: &soon}))
	prog := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "holiday-shifts", For: &orchapi.AskFor{Group: "Santa Monica", Contact: "Dr. R"}}))
	done := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "done-one"}))
	must(orchclient.Decide(ctx, h.device(ipPhone), done.ID, "a"))

	in := must(orchclient.Decisions(ctx, h.owner, "", ""))
	var order []string
	for _, x := range in.Decisions {
		switch x.Kind {
		case "ask":
			order = append(order, x.Ask.ID)
		case "work":
			order = append(order, x.Work.ID)
			if x.Work.ID == labelled.ID && x.OwnerName != "ivo" {
				t.Errorf("owner name: %+v", x)
			}
		}
		if x.ProjectSlug != "harbor" {
			t.Errorf("slug: %+v", x)
		}
	}
	if strings.Contains(strings.Join(order, " "), plain.ID) {
		t.Errorf("an item nobody waits on: %v", order)
	}
	if strings.Contains(strings.Join(order, " "), beas.ID) {
		t.Errorf("an item waiting on someone else: %v", order)
	}
	everyone := false
	for _, x := range must(orchclient.DecisionsFor(ctx, h.owner, "", "", "everyone")).Decisions {
		everyone = everyone || (x.Work != nil && x.Work.ID == beas.ID && x.OwnerName == "bea")
	}
	if !everyone {
		t.Errorf("for=everyone leaves out someone else's wait")
	}
	if strings.Contains(strings.Join(order, " "), asked.ID) {
		t.Errorf("an item with an ask is listed twice: %v", order)
	}
	// A deadline, then priority; then a program's, then items with no ask.
	want := []string{deadline.ID, urgent.ID, late.ID, prog.ID}
	if len(order) != 6 || strings.Join(order[:4], " ") != strings.Join(want, " ") {
		t.Fatalf("order: %v, want %v then two items", order, want)
	}
	if !strings.Contains(strings.Join(order[4:], " "), labelled.ID) || !strings.Contains(strings.Join(order[4:], " "), human.ID) {
		t.Fatalf("labelled items: %v", order)
	}
	// Recorded answers on request; items off on request.
	rec := must(orchclient.Decisions(ctx, h.owner, "", "recorded"))
	found := false
	for _, x := range rec.Decisions {
		found = found || (x.Ask != nil && x.Ask.ID == done.ID)
	}
	if !found {
		t.Fatalf("recorded: %+v", rec)
	}
	if _, err := orchclient.Decisions(ctx, h.owner, "", "nonsense"); code(err) != api.CodeBadRequest {
		t.Fatalf("bad status: %v", err)
	}

	// A guest sees its own asks and the items in its area, nothing else; a
	// hidden ask's item is not unmasked by the labelled list.
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token, guest.Project, guest.ProjectFrom = g.Secret, "harbor", "env"
	gi := must(orchclient.Decisions(ctx, guest, "", "open"))
	for _, x := range gi.Decisions {
		if x.Ask != nil {
			t.Errorf("guest sees an ask: %+v", x.Ask)
		}
	}
	hidden := must(orchclient.CreateWork(ctx, h.ownerIn("zz-side", ""), orchapi.CreateWorkRequest{Title: "HIDDEN", Labels: []string{"human"}}))
	for _, x := range must(orchclient.Decisions(ctx, guest, "", "open")).Decisions {
		if x.Work != nil && x.Work.ID == hidden.ID {
			t.Errorf("guest sees a hidden item: %+v", x.Work)
		}
	}
}

// A redacted text leaves no copy on the card or in the answer, and the
// receipt, which signs only hashes, still verifies.
func TestScrubCardAndAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("to scrub")
	agent, _ := h.session("44444444")
	c := card()
	c.Background = "the patient is Jane Doe-Smith, in room nine"
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: c}))
	d := must(orchclient.DecideWith(ctx, h.device(ipPhone), a.ID, orchapi.DecideRequest{Decision: "a", Text: "tell Jane Doe-Smith today"}))
	if err := h.dm.Deps.ScrubAll(ctx, core.Scrub{Project: a.Project, Subjects: []string{it.ID}, Texts: []string{"Jane Doe-Smith"}}); err != nil {
		t.Fatal(err)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if strings.Contains(got.Body, "Jane") || strings.Contains(got.Card.Background, "Jane") || strings.Contains(got.Decision.Text, "Jane") {
		t.Fatalf("after scrub: %+v", got)
	}
	if c := approvals.Verify(got.Decision.Receipt, must(h.svc.PublicKey()), orchapi.ReceiptExpect{Work: it.ID, Ask: a.ID, Card: a.CardSHA}); !c.Valid || got.Decision.Receipt.Sig != d.Receipt.Sig {
		t.Fatalf("receipt after scrub: %+v", c)
	}
}

// The page's files are served to a tailnet device by this daemon's name, and
// to nobody on another Host (DNS rebinding) or on the socket.
func TestDecisionsPageFiles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for path, ctype := range map[string]string{
		"/v0/approvals/app.js":               "text/javascript",
		"/v0/approvals/zm-decisions.js":      "text/javascript",
		"/v0/approvals/manifest.webmanifest": "application/manifest+json",
		"/v0/approvals/icon.png":             "image/png",
	} {
		resp, err := h.device(ipPhone).Stream(ctx, http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body := readAll(resp)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) || body == "" ||
			resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %q (%d bytes)", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
		if _, err := h.deviceAt(ipPhone, "zm-mini.attacker.example").Stream(ctx, http.MethodGet, path, nil); err == nil {
			t.Errorf("%s served on another Host", path)
		}
		if _, err := h.owner.Stream(ctx, http.MethodGet, path, nil); err == nil {
			t.Errorf("%s served on the socket", path)
		}
	}
}

// An approver added for a person (zm approver add --for) is that person's
// device: its Decisions page reads as their own grants, not as the machine's
// owner (an authority whose OS user is not the person deciding). An unknown
// person is refused before any code is issued.
func TestApproverForAPerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := orchclient.AddApproverFor(ctx, h.owner, "nobody-phone", "nobody"); code(err) != api.CodeNotFound {
		t.Fatalf("approver for an unknown person: %v", err)
	}
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	mineAsk := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: card(), Topic: "beas"}))
	sideAsk := must(orchclient.NewAsk(ctx, h.ownerIn("zz-side", ""), orchapi.NewAskRequest{Card: card(), Topic: "side"}))
	beas := h.workItem("bea's step", "owner:bea", "owner-run")
	owners := h.workItem("the machine owner's step", "owner:ivo", "owner-run")

	add := must(orchclient.AddApproverFor(ctx, h.owner, "bea-phone", "bea"))
	if add.Approver.Identity != "" && add.Approver.Identity != "bea" {
		t.Fatalf("add answer: %+v", add.Approver)
	}
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	ls := must(orchclient.Approvers(ctx, h.owner))
	if len(ls) != 1 || ls[0].Identity != "bea" {
		t.Fatalf("approvers: %+v", ls)
	}
	v := must(orchclient.DeviceView(ctx, h.device(ipPhone)))
	got := map[string]bool{}
	for _, d := range v.Decisions {
		if d.Ask != nil {
			got[d.Ask.ID] = true
		}
		if d.Work != nil {
			got[d.Work.ID] = true
		}
	}
	if v.Identity != "bea" || !got[mineAsk.ID] || !got[beas.ID] {
		t.Fatalf("bea's device view: identity %q, decisions %v", v.Identity, got)
	}
	if got[sideAsk.ID] || got[owners.ID] {
		t.Fatalf("bea's device sees beyond bea's grants or waits: %v", got)
	}
}

// Phone alerts (SPEC 16 "Phone alerts"): only an active paired device turns
// them on, they belong to that device's person, and they end with the device.
func TestDeviceAlertsAreThePersonsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	add := must(orchclient.AddApproverFor(ctx, h.owner, "bea-phone", "bea"))
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := key.PublicKey.ECDH()
	var req orchapi.DevicePushRequest
	req.Endpoint = "https://fcm.googleapis.com/bea-phone"
	req.Keys.P256DH = base64.RawURLEncoding.EncodeToString(pub.Bytes())
	req.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	// A device that is no approver gets neither the key nor a subscription.
	if err := h.device(ipPhone2).Do(ctx, http.MethodGet, "/v0/approvals/push-key", nil, nil); code(err) != api.CodeForbidden {
		t.Fatalf("push key for a stranger: %v", err)
	}
	if err := h.device(ipPhone2).Do(ctx, http.MethodPost, "/v0/approvals/push", req, nil); code(err) != api.CodeForbidden {
		t.Fatalf("subscribe from a stranger: %v", err)
	}
	if err := h.owner.Do(ctx, http.MethodPost, "/v0/approvals/push", req, nil); err == nil {
		t.Fatal("the socket subscribed a phone")
	}
	// The page posts the browser's PushSubscription.toJSON(), expirationTime included.
	browserJSON := map[string]any{"endpoint": req.Endpoint, "expirationTime": nil, "keys": req.Keys}
	if err := h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvals/push", browserJSON, nil); err != nil {
		t.Fatal("bea's phone:", err)
	}
	var identity, token string
	if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT identity, token FROM web_push_subscriptions`).Scan(&identity, &token); err != nil {
		t.Fatal(err)
	}
	bea := must(h.dm.Deps.Projects.Identity(ctx, "bea"))
	if identity != bea.ID || token != "device:nPHONE" {
		t.Fatalf("subscription stored for %q by %q, want bea's by its device", identity, token)
	}
	if person, ok := h.svc.DevicePerson(ctx, "nPHONE"); !ok || person.Address != "human:bea" {
		t.Fatalf("device person: %+v %v", person, ok)
	}
	// The page's For you and its Got it are bea's alone (a person's FYI:
	// an agent's is a report, SPEC 9 "Reports").
	agent, _ := h.session("fyi-agent")
	if r := must(orchclient.Send(ctx, agent, orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:bea", Body: "agent FYI", Needs: orchapi.NeedsAny})); r.Kind != orchapi.MailKindReport {
		t.Fatalf("an agent's FYI to a person: %+v", r)
	}
	person := h.ownerIn("harbor/solver", "")
	forBea := must(orchclient.Send(ctx, person, orchapi.SendMailRequest{To: "human:bea", Body: "FYI for bea", Needs: orchapi.NeedsAny}))
	forIvo := must(orchclient.Send(ctx, person, orchapi.SendMailRequest{To: "human:ivo", Body: "FYI for ivo", Needs: orchapi.NeedsAny}))
	v := must(orchclient.DeviceView(ctx, h.device(ipPhone)))
	if len(v.ForYou) != 1 || v.ForYou[0].ID != forBea.ID {
		t.Fatalf("bea's phone lists %+v, want only bea's FYI", v.ForYou)
	}
	gotIt := func(id string) error {
		return h.device(ipPhone).Do(ctx, http.MethodPost, "/v0/approvals/got-it", orchapi.GotItRequest{ID: id}, nil)
	}
	if err := gotIt(forIvo.ID); code(err) != api.CodeNotFound {
		t.Fatalf("bea's phone took ivo's FYI: %v", err)
	}
	if err := gotIt(forBea.ID); err != nil {
		t.Fatal("bea's Got it:", err)
	}
	if v := must(orchclient.DeviceView(ctx, h.device(ipPhone))); len(v.ForYou) != 0 {
		t.Fatalf("after Got it bea's phone still lists %+v", v.ForYou)
	}
	must(orchclient.RemoveApprover(ctx, h.owner, "bea-phone"))
	if _, ok := h.svc.DevicePerson(ctx, "nPHONE"); ok {
		t.Fatal("a removed approver still names a person for alerts")
	}
}

// A person pairs their own phone (SPEC 16 "A person pairs their own phone"):
// only for themselves, never from an agent session, and the phone waits for
// another device's confirm, even on a daemon whose bootstrap is unused.
func TestPersonPairsTheirOwnPhone(t *testing.T) {
	ctx := context.Background()
	beaOn := func(h *harness) *client.Client {
		t.Helper()
		must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea"}))
		tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member"}))
		c := client.WithHandler(h.dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "harbor", "flag"
		return c
	}
	// No approver yet: a self-added phone still never bootstraps.
	fresh := newHarness(t)
	add := must(orchclient.AddMyPhone(ctx, beaOn(fresh), ""))
	if a := must(orchclient.Pair(ctx, fresh.device(ipTablet), add.Code)); a.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("a self-added phone bootstrapped: %+v", a)
	}

	h := newHarness(t)
	h.pair("ivo-phone", ipPhone) // the first approver, bootstrapped
	bea := beaOn(h)
	agent, _ := h.session("pair-agent")
	if _, err := orchclient.AddMyPhone(ctx, agent, ""); code(err) != api.CodeForbidden {
		t.Fatalf("an agent session reserved a phone: %v", err)
	}
	add = must(orchclient.AddMyPhone(ctx, bea, ""))
	if add.Approver.Name != "bea-phone" || add.Approver.Identity != "" && add.Approver.Identity != "bea" {
		t.Fatalf("bea's reservation: %+v", add.Approver)
	}
	a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	if a.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("bea's phone is %s before anyone confirmed it", a.State)
	}
	if _, ok := h.svc.DevicePerson(ctx, "nTABLET"); ok {
		t.Fatal("an unconfirmed phone already acts for bea")
	}
	if _, err := orchclient.ConfirmApprover(ctx, h.device(ipTablet), "bea-phone"); err == nil {
		t.Fatal("bea's phone confirmed itself")
	}
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "bea-phone"))
	if p, ok := h.svc.DevicePerson(ctx, "nTABLET"); !ok || p.Address != "human:bea" {
		t.Fatalf("after confirm bea's phone acts for %+v %v", p, ok)
	}
}

// An ask shows the live attachments of its work item (zm ask new --attach):
// their metadata rides on the ask; one that is not on the item, or an ask
// with no work item, is refused and nothing is created.
func TestAskCarriesItemAttachments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.ownerIn("harbor/solver", "")
	it := must(orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "pick a layout"}))
	if res := must(orchclient.AddTracker(ctx, h.ownerIn("harbor", ""), orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10})); !res.OK {
		t.Fatalf("native tracker: %+v", res)
	}
	work := "issue:" + it.ID
	agent, _ := h.session("aaaaaaaa")
	page := []byte("<!doctype html><title>mock</title>")
	at, err := orchclient.AttachIssueImage(ctx, owner, it.ID, page, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Attachments: []string{at.ID}}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "work") {
		t.Fatalf("no work item: %v", err)
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: work}, Card: card(), Attachments: []string{ids.New()}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("not on the item: %v", err)
	}
	if as := must(orchclient.Asks(ctx, owner, "", false)); len(as) != 0 {
		t.Fatalf("a refused ask was created: %+v", as)
	}
	a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: work}, Card: card(), Attachments: []string{at.ID}})
	if err != nil {
		t.Fatal(err)
	}
	want := orchapi.AskAttachment{ID: at.ID, Type: "text/html", Size: int64(len(page)), SHA256: at.SHA256, Created: at.Created, Expires: at.Expires}
	if len(a.Attachments) != 1 || a.Attachments[0] != want {
		t.Fatalf("ask attachments: %+v", a.Attachments)
	}
	got := must(orchclient.Asks(ctx, owner, "", false))
	if len(got) != 1 || len(got[0].Attachments) != 1 || got[0].Attachments[0] != want {
		t.Fatalf("listed: %+v", got)
	}

	// The ask lists only what is still live on the item: one removed from
	// it goes at once, one expired goes too.
	second := must(orchclient.AttachIssueImage(ctx, owner, it.ID, page, ""))
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: work}, Topic: "two", Card: card(), Attachments: []string{at.ID, second.ID}}))
	if len(b.Attachments) != 2 {
		t.Fatalf("two attached: %+v", b.Attachments)
	}
	must(orchclient.RemoveIssueAttachment(ctx, owner, it.ID, second.ID))
	if b = must(orchclient.GetAsk(ctx, owner, b.ID)); len(b.Attachments) != 1 || b.Attachments[0].ID != at.ID {
		t.Fatalf("after a removal: %+v", b.Attachments)
	}
	h.clock.add(at.Expires.Sub(h.clock.now()) + time.Second)
	if b = must(orchclient.GetAsk(ctx, owner, b.ID)); len(b.Attachments) != 0 {
		t.Fatalf("after expiry: %+v", b.Attachments)
	}
}

// zm ask new --attach (NewAskWithFiles) attaches, then asks; when an attach
// fails partway or the ask is refused, the earlier attaches are removed and
// no ask is created.
func TestNewAskWithFilesRollsBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.ownerIn("harbor/solver", "")
	it := must(orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "pick a layout"}))
	if res := must(orchclient.AddTracker(ctx, h.ownerIn("harbor", ""), orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10})); !res.OK {
		t.Fatalf("native tracker: %+v", res)
	}
	agent, _ := h.session("aaaaaaaa")
	page := []byte("<!doctype html><title>mock</title>")
	bad := card()
	bad.Recommend = "z"
	for name, c := range map[string]struct {
		card  *orchapi.AskCard
		files [][]byte
		want  string
	}{
		"second attach fails": {card(), [][]byte{page, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)}, "attach b:"},
		"ask refused":         {bad, [][]byte{page, page}, "recommend"},
	} {
		req := orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: "issue:" + it.ID}, Card: c.card}
		if _, err := orchclient.NewAskWithFiles(ctx, agent, req, c.files, []string{"a.html", "b"}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if l := must(orchclient.IssueAttachments(ctx, owner, it.ID)); len(l.Attachments) != 0 {
			t.Fatalf("%s: left attached: %+v", name, l.Attachments)
		}
		if as := must(orchclient.Asks(ctx, owner, "", false)); len(as) != 0 {
			t.Fatalf("%s: an ask was created: %+v", name, as)
		}
	}
}

// An ask stores the asking thread's title and a pull request URL beside its
// card and returns them with its work item's title; none of it is
// card text, so the card hash and the card judge never see it.
func TestAskCarriesOriginTitles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("trade picks")
	agent, _ := h.session("aaaaaaaa")
	const pr = "https://github.com/acme/app/pull/7"
	plain := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "plain-ask"}))
	if plain.WorkTitle != "trade picks" || plain.ThreadTitle != "" || plain.PRURL != "" {
		t.Fatalf("no origin sent: %+v", plain)
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "with-origin",
		ThreadTitle: "  Fix the \t shift   swap  ", PRURL: " HTTPS://GitHub.com/acme/app/pull/7 "}))
	if a.WorkTitle != "trade picks" || a.ThreadTitle != "Fix the shift swap" || a.PRURL != pr || a.CardSHA != plain.CardSHA {
		t.Fatalf("created: %+v", a)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.WorkTitle != a.WorkTitle || got.ThreadTitle != a.ThreadTitle || got.PRURL != a.PRURL {
		t.Errorf("show: %+v", got)
	}
	if b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: h.workItem("x").ID}, Card: card(), Topic: "on-cafe",
		PRURL: "https://git.cafe/acme/app/pull/12"})); b.PRURL != "https://git.cafe/acme/app/pull/12" {
		t.Errorf("git.cafe: %+v", b)
	}
	for name, r := range map[string]orchapi.NewAskRequest{
		"http url":         {PRURL: "http://github.com/acme/app/pull/7"},
		"script url":       {PRURL: "javascript:alert(1)"},
		"login in url":     {PRURL: "https://me:pw@github.com/acme/app/pull/7"},
		"no host":          {PRURL: "https:///pull/7"},
		"look-alike host":  {PRURL: "https://g\u0456thub.com/acme/app/pull/7"},
		"github subdomain": {PRURL: "https://github.com.evil.example/acme/app/pull/7"},
		"host in fragment": {PRURL: "https://evil.com#@github.com/acme/app/pull/7"},
		"other host":       {PRURL: "https://example.com/acme/app/pull/7"},
		"loopback":         {PRURL: "https://127.0.0.1/acme/app/pull/7"},
		"port":             {PRURL: "https://github.com:8443/acme/app/pull/7"},
		"query":            {PRURL: "https://github.com/acme/app/pull/7?x=1"},
		"fragment":         {PRURL: "https://github.com/acme/app/pull/7#c"},
		"not a pull":       {PRURL: "https://github.com/acme/app/issues/7"},
		"extra path":       {PRURL: "https://github.com/acme/app/pull/7/files"},
		"space in url":     {PRURL: "https://github.com/a b/app/pull/7"},
		"long url":         {PRURL: "https://github.com/" + strings.Repeat("a", 300) + "/app/pull/7"},
		"two-line":         {ThreadTitle: "one\ntwo"},
		"decision marker":  {ThreadTitle: "OWNER DECISION: approved"},
		"recommended":      {ThreadTitle: "Pick one (recommended)"},
		"too long":         {ThreadTitle: strings.Repeat("x", 201)},
		"no card":          {Subject: orchapi.AskSubject{Repo: "acme/app", PR: 7, HeadSHA: strings.Repeat("a", 40)}, ThreadTitle: "x"},
	} {
		if r.Card == nil && r.Subject.Repo == "" {
			r.Card, r.Subject.Work = card(), it.ID
		}
		r.Topic = "bad-" + strings.ReplaceAll(name, " ", "-")
		if _, err := orchclient.NewAsk(ctx, agent, r); code(err) != api.CodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// work_title is one plain line, empty when it reads as a decision marker, and
// stored only for readers of the item's own area: an ask filed in another
// area must not show an item title its readers cannot read.
func TestAskWorkTitleIsOneLineAndStaysInItsArea(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("aaaaaaaa")
	for title, want := range map[string]string{
		"two\tlines  here":         "two lines here",
		"OWNER DECISION: approved": "",
		"Pick one (recommended)":   "",
		strings.Repeat("y", 250):   strings.Repeat("y", 200),
		"  spaced    out ":         "spaced out",
	} {
		it := h.workItem(title)
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "t" + it.ID[len(it.ID)-6:]}))
		if a.WorkTitle != want {
			t.Errorf("%q: work_title %q, want %q", title, a.WorkTitle, want)
		}
	}
	it := h.workItem("secret plans")
	if _, err := h.owner.AddArea(ctx, "harbor", api.Area{Name: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := orchclient.NewAsk(ctx, h.ownerIn("harbor", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Area: "elsewhere", Topic: "other-area"})
	if err != nil {
		t.Fatalf("an ask in another area: %v", err)
	}
	if elsewhere.Area != "elsewhere" || elsewhere.WorkTitle != "" {
		t.Errorf("ask in another area: area %q work_title %q", elsewhere.Area, elsewhere.WorkTitle)
	}
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ravi", Project: "harbor", Area: "elsewhere", Role: api.RoleMember}))
	member := client.WithHandler(h.dm.Handler)
	member.Token, member.Project, member.ProjectFrom = tok.Secret, "harbor", "env"
	if got := must(orchclient.GetAsk(ctx, member, elsewhere.ID)); got.ID != elsewhere.ID || got.WorkTitle != "" {
		t.Errorf("a member of that area reads: %+v", got)
	}
}
