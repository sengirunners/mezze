package approvals_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A device signs a receipt as its own name, so a gate that asks the daemon
// for the person (--approver <person>) also passes on that person's paired
// device, and on nothing else: not another person, not a device paired for
// no one, not a receipt that is not the ask's recorded one, not for a caller
// who may not read the ask, and never offline.
func TestReceiptApproverMatchesTheDevicesPerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	add := must(orchclient.AddApproverFor(ctx, h.owner, "wren-z3", "wren"))
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	spare := must(orchclient.AddApprover(ctx, h.owner, "spare-phone")) // paired for no one
	must(orchclient.Pair(ctx, h.device(ipPhone2), spare.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	agent, _ := h.session("7e7e7e7e")

	decided := func(topic string, decide func(id string) orchapi.Decision) (orchapi.Receipt, string) {
		t.Helper()
		it := h.workItem(topic)
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: topic, Owner: "wren", Subject: orchapi.AskSubject{Work: it.ID}}))
		return decide(a.ID).Receipt, it.ID
	}
	onDevice := func(ip string) func(string) orchapi.Decision {
		return func(id string) orchapi.Decision { return must(orchclient.Decide(ctx, h.device(ip), id, "a")) }
	}
	verify := func(r orchapi.Receipt, work, approver string) orchapi.ReceiptCheck {
		t.Helper()
		return must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), orchapi.ReceiptExpect{Project: "harbor", Ask: r.Ask,
			Work: work, Decision: "a", Approver: approver}))
	}

	r, work := decided("on-z3", onDevice(ipPhone))
	if r.Approver != "wren-z3" {
		t.Fatalf("the receipt names the device: %q", r.Approver)
	}
	for _, name := range []string{"wren-z3", "wren"} {
		if c := verify(r, work, name); !c.Valid {
			t.Fatalf("--approver %s on wren's device: %+v", name, c)
		}
	}
	if c := verify(r, work, "tobi"); c.Valid || c.Reason != "receipt is from another approver" {
		t.Fatalf("--approver tobi on wren's device: %+v", c)
	}
	// Every other expectation still holds.
	if c := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), orchapi.ReceiptExpect{Project: "harbor", Ask: r.Ask,
		Work: work, Decision: "b", Approver: "wren"})); c.Valid {
		t.Fatalf("another decision passed: %+v", c)
	}
	// A tampered receipt does not borrow the recorded one's person.
	bad := r
	bad.Time = "2026-01-01T00:00:00.000Z"
	if c := verify(bad, work, "wren"); c.Valid {
		t.Fatalf("tampered receipt: %+v", c)
	}
	// Offline there is no one to ask who holds a device: the name stays exact.
	pub := must(approvals.ParsePublicKey(must(orchclient.ReceiptKey(ctx, h.owner)).Line))
	if c := approvals.Verify(r, pub, orchapi.ReceiptExpect{Ask: r.Ask, Work: work, Decision: "a", Approver: "wren"}); c.Valid {
		t.Fatalf("offline matched the person: %+v", c)
	}
	// A caller who may not read the ask learns nothing of whose device it is.
	must(h.owner.AddArea(ctx, "harbor", api.Area{Name: "elsewhere"}))
	ana := h.tokenClient(api.MintTokenRequest{As: "ana", Project: "harbor", Area: "elsewhere", Role: "member"})
	if _, err := orchclient.GetAsk(ctx, ana, r.Ask); code(err) != api.CodeNotFound {
		t.Fatalf("the test needs a caller who cannot read the ask: %v", err)
	}
	if c := must(orchclient.VerifyReceipt(ctx, ana, mustJSON(r), orchapi.ReceiptExpect{Project: "harbor", Ask: r.Ask,
		Work: work, Decision: "a", Approver: "wren"})); c.Valid {
		t.Fatalf("an area member matched the person: %+v", c)
	}

	// A device paired for no one, where several people answer, is no one's.
	r2, work2 := decided("spare", onDevice(ipPhone2))
	if c := verify(r2, work2, "wren"); c.Valid {
		t.Fatalf("a device paired for no one passed as wren: %+v", c)
	}
	// Another owner's own answer is theirs, not the card's person's.
	r3, work3 := decided("tobi", func(id string) orchapi.Decision {
		return must(orchclient.Choose(ctx, tobi, "", id, orchapi.DecideRequest{Decision: "a"}))
	})
	if c := verify(r3, work3, "wren"); c.Valid {
		t.Fatalf("tobi's answer passed as wren: %+v", c)
	}
	if c := verify(r3, work3, "tobi"); !c.Valid {
		t.Fatalf("tobi's own answer: %+v", c)
	}
	// An answer from a linked account is the person's.
	laptop := h.tokenClient(api.MintTokenRequest{As: "wren-laptop", Project: "harbor", Role: "owner"})
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	must(orchclient.AddPersonLink(ctx, laptop, orchapi.PersonLinkRequest{Identity: "wren-laptop", Person: "wren"}))
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-laptop", Person: "wren"}))
	r4, work4 := decided("laptop", func(id string) orchapi.Decision {
		return must(orchclient.Choose(ctx, laptop, "", id, orchapi.DecideRequest{Decision: "a"}))
	})
	if c := verify(r4, work4, "wren"); r4.Approver != "wren-laptop" || !c.Valid {
		t.Fatalf("wren's linked account (%s): %+v", r4.Approver, c)
	}
	// A device removed after it answered is no one's any more.
	must(orchclient.RemoveApprover(ctx, h.owner, "wren-z3"))
	if c := verify(r, work, "wren"); c.Valid {
		t.Fatalf("a removed device still passed as wren: %+v", c)
	}
	if c := verify(r, work, "wren-z3"); !c.Valid {
		t.Fatalf("the exact name still matches: %+v", c)
	}
}

// Where the machine's owner is the one person who answers, a device paired
// for no one answers as them, but a gate naming that person still needs the
// device paired for them.
func TestReceiptApproverNeedsADevicePairedForThePerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("spare-phone", ipPhone)
	agent, _ := h.session("7f7f7f7f")
	it := h.workItem("solo-gate")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "solo-gate", Subject: orchapi.AskSubject{Work: it.ID}}))
	r := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a")).Receipt
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision.ApproverPerson == "" {
		t.Fatal("the sole owner's phone answers as them")
	}
	if c := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), orchapi.ReceiptExpect{Project: "harbor", Ask: a.ID,
		Work: it.ID, Decision: "a", Approver: "ivo"})); c.Valid {
		t.Fatalf("a device paired for no one passed as ivo: %+v", c)
	}
}
