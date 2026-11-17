package approvals_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func linkOf(t *testing.T, l orchapi.PersonLinks, identity string) orchapi.PersonLink {
	t.Helper()
	for _, x := range l.Links {
		if x.Identity == identity {
			return x
		}
	}
	t.Fatalf("no link for %s in %+v", identity, l.Links)
	return orchapi.PersonLink{}
}

func boolp(b bool) *bool { return &b }

// One person, several accounts: a link is data; it folds the
// accounts into one person only when live, and grants nothing.
func TestPersonLinkFoldsWithoutGranting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	laptop := h.tokenClient(api.MintTokenRequest{As: "wren-laptop", Project: "harbor", Role: "owner"})
	box := h.tokenClient(api.MintTokenRequest{As: "wren-box", Project: "harbor", Role: "member"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	agent, _ := h.session("5a5a5a5a")

	// The account alone: recorded, not live.
	l := must(orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	if x := linkOf(t, l, "wren-box"); x.Live || !x.BySelf || x.ByPerson || x.ByAdmin != "" {
		t.Fatalf("the account's own say alone is not live: %+v", x)
	}
	// An account never labels itself as someone else's by an admin's say,
	// nor touches another link it is no party to.
	if _, err := orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "tobi", Person: "wren"}); code(err) != api.CodeNotFound {
		t.Fatalf("a stranger's link: %v", err)
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "pre", Owner: "wren"}))
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "pre-box", Owner: "wren-box"}); code(err) != api.CodeBadRequest {
		t.Fatalf("before the link is live, wren-box answers nothing and names no one: %v", err)
	}
	// The person's own say too: live.
	l = must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	if x := linkOf(t, l, "wren-box"); !x.Live || !x.BySelf || !x.ByPerson {
		t.Fatalf("both accounts said so: %+v", x)
	}
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "post-box", Owner: "wren-box"}))
	if b.OwnerPerson == "" || b.OwnerPerson != a.OwnerPerson {
		t.Fatalf("a card to either account is one person's: %q vs %q", b.OwnerPerson, a.OwnerPerson)
	}
	// It grants nothing: the box account is a member and still cannot answer.
	if _, err := orchclient.Choose(ctx, box, "", a.ID, orchapi.DecideRequest{Decision: "b"}); code(err) != api.CodeForbidden {
		t.Fatalf("a linked member answered an ask: %v", err)
	}
	// An admin of every project both are in records another account; it is
	// live once the person agrees, without the linked account's say.
	l = must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-laptop", Person: "wren"}))
	if x := linkOf(t, l, "wren-laptop"); x.Live || x.ByAdmin == "" || x.BySelf {
		t.Fatalf("an admin alone gave wren an account: %+v", x)
	}
	l = must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-laptop", Person: "wren"}))
	if x := linkOf(t, l, "wren-laptop"); !x.Live || x.BySelf {
		t.Fatalf("the admin's record and the person's yes: %+v", x)
	}
	// Answered from the person's other account: their answer.
	d := must(orchclient.Choose(ctx, laptop, "", a.ID, orchapi.DecideRequest{Decision: "b"}))
	if d.Approver != "wren-laptop" {
		t.Fatalf("the approver is the account that answered: %q", d.Approver)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Decision == nil || got.Decision.ApproverPerson != got.OwnerPerson {
		t.Fatalf("answered by the person it was for: %+v vs %q", got.Decision, got.OwnerPerson)
	}
	// Someone else's answer stays someone else's.
	c := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "other", Owner: "wren"}))
	must(orchclient.Choose(ctx, tobi, "", c.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, h.owner, c.ID)); got.Decision.ApproverPerson == "" || got.Decision.ApproverPerson == got.OwnerPerson {
		t.Fatalf("another person's answer: %q vs %q", got.Decision.ApproverPerson, got.OwnerPerson)
	}
	// /v0/people shows the person once with every account's name.
	var rows int
	for _, p := range must(orchclient.People(ctx, h.owner, "harbor")).People {
		if slices.Contains(p.Names, "wren") {
			rows++
			for _, n := range []string{"wren-box", "wren-laptop"} {
				if !slices.Contains(p.Names, n) {
					t.Fatalf("person row lacks %s: %+v", n, p)
				}
			}
		}
	}
	if rows != 1 {
		t.Fatalf("wren shows %d times", rows)
	}
	// One level only; either account or the admin removes a link, and the
	// fold goes with it.
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "tobi", Person: "wren-box"})) // pending: blocks nothing
	if _, err := orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "tobi", Person: "wren-box"}); code(err) != api.CodeConflict {
		t.Fatalf("a live link to a linked account: %v", err)
	}
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren", Person: "tobi"}))
	if _, err := orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "wren", Person: "tobi"}); code(err) != api.CodeConflict {
		t.Fatalf("a main account linked away: %v", err)
	}
	must(orchclient.RemovePersonLink(ctx, box, "wren-box"))
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "after", Owner: "wren-box"}); code(err) != api.CodeBadRequest {
		t.Fatalf("the fold outlived its link: %v", err)
	}
}

// A verified GitHub link decides who an account is: a person link that
// contradicts one is refused, and one that agrees changes nothing.
func TestPersonLinkYieldsToGitHub(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "nell", Project: "harbor", Role: "owner"}))
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "nell-2", Project: "harbor", Role: "owner"}))
	d := h.dm.Deps
	d.LinkedAccount = h.svc.LinkedPerson(func(ctx context.Context, id string) (string, string) {
		if who, err := d.Projects.Identity(ctx, id); err == nil && who.Name == "nell-2" {
			return "github:9", "someone"
		}
		return "", ""
	})
	if _, err := orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "nell-2", Person: "nell"}); code(err) != api.CodeConflict {
		t.Fatalf("a link against a verified GitHub link: %v", err)
	}
}

// Mail to an account reaches the person's other accounts only when that
// account itself turned sharing on; nobody else can turn it on.
func TestPersonLinkMailFollowsConsent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	box := h.tokenClient(api.MintTokenRequest{As: "wren-box", Project: "harbor", Role: "owner"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	if _, err := orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(true)}); code(err) != api.CodeForbidden {
		t.Fatalf("an admin shared another account's mail: %v", err)
	}
	if _, err := orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(true)}); err != nil {
		t.Fatal(err) // wren shares wren's own mail, not wren-box's
	}
	inbox := func() map[string]bool {
		out := map[string]bool{}
		for _, m := range must(orchclient.Inbox(ctx, wren, orchapi.InboxQuery{Project: "harbor"})) {
			out[m.Body] = true
		}
		return out
	}
	must(orchclient.Send(ctx, tobi, orchapi.SendMailRequest{To: "human:wren-box", Body: "before sharing"}))
	if inbox()["before sharing"] {
		t.Fatal("wren-box's mail reached wren before wren-box shared it")
	}
	l := must(orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(true)}))
	if x := linkOf(t, l, "wren-box"); !x.SharesMail || !x.PersonSharesMail {
		t.Fatalf("both switches on: %+v", x)
	}
	m := must(orchclient.Send(ctx, tobi, orchapi.SendMailRequest{To: "human:wren-box", Body: "after sharing"}))
	if got := inbox(); !got["after sharing"] || got["before sharing"] {
		t.Fatalf("wren's inbox: %v", got)
	}
	// wren reads it and marks it read (Got it) for both accounts.
	if err := wren.Do(ctx, http.MethodPost, "/v0/mail/"+m.ID+"/got-it", nil, nil); err != nil {
		t.Fatalf("got it from the person's other account: %v", err)
	}
	for _, x := range must(orchclient.Inbox(ctx, box, orchapi.InboxQuery{Project: "harbor"})) {
		if x.ID == m.ID {
			t.Fatal("still unread on the account it was sent to")
		}
	}
	// A stranger to the person still sees none of it.
	if _, err := orchclient.GetMail(ctx, tobi, m.ID); err != nil {
		t.Fatalf("the sender reads its own mail: %v", err)
	}
	other := h.tokenClient(api.MintTokenRequest{As: "ash", Project: "harbor", Role: "owner"})
	if _, err := orchclient.GetMail(ctx, other, m.ID); code(err) != api.CodeNotFound {
		t.Fatalf("an owner outside the person read private mail: %v", err)
	}
	// Turning it off stops new mail reaching the other account.
	must(orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(false)}))
	must(orchclient.Send(ctx, tobi, orchapi.SendMailRequest{To: "human:wren-box", Body: "after stopping"}))
	if inbox()["after stopping"] {
		t.Fatal("mail reached wren after wren-box stopped sharing")
	}
}

// The mail switch belongs to one link: moving the link to another person,
// or removing and recording it again, never carries an account's consent
// over; the sender never owns its own copy; a party names only accounts it
// could already see.
func TestPersonLinkConsentDoesNotMove(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	box := h.tokenClient(api.MintTokenRequest{As: "wren-box", Project: "harbor", Role: "member"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	ash := h.tokenClient(api.MintTokenRequest{As: "ash", Project: "harbor", Role: "owner"})
	listed := func(c *client.Client, body string) bool {
		for _, m := range must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{Project: "harbor"})) {
			if m.Body == body {
				return true
			}
		}
		return false
	}
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	must(orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(true)}))
	must(orchclient.Send(ctx, ash, orchapi.SendMailRequest{To: "human:wren-box", Body: "one"}))
	if !listed(wren, "one") {
		t.Fatal("shared mail did not reach the person")
	}
	// An admin moves the link to tobi: wren-box's switch does not follow.
	l := must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "tobi"}))
	if x := linkOf(t, l, "wren-box"); x.SharesMail || x.BySelf || x.Live {
		t.Fatalf("consent moved with the link: %+v", x)
	}
	must(orchclient.Send(ctx, ash, orchapi.SendMailRequest{To: "human:wren-box", Body: "two"}))
	if listed(tobi, "two") || listed(wren, "two") {
		t.Fatal("wren-box's mail reached someone it never shared with")
	}
	// Removed and recorded again: still off.
	must(orchclient.RemovePersonLink(ctx, h.owner, "wren-box"))
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	must(orchclient.Send(ctx, ash, orchapi.SendMailRequest{To: "human:wren-box", Body: "three"}))
	if listed(wren, "three") {
		t.Fatal("a relinked account's old switch came back")
	}
	// The main account's switch covers only the link it was set on.
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren", ShareMail: boolp(true)}))
	must(orchclient.Send(ctx, ash, orchapi.SendMailRequest{To: "human:wren", Body: "four"}))
	if !listed(box, "four") {
		t.Fatal("the main account's shared mail did not reach its linked account")
	}
	// The sender's own copy is never its for-you item, nor listed as one.
	m := must(orchclient.Send(ctx, wren, orchapi.SendMailRequest{To: "human:wren-box", Body: "five"}))
	if listed(wren, "five") {
		t.Fatal("the sender lists its own mail to its other account")
	}
	if err := wren.Do(ctx, http.MethodPost, "/v0/mail/"+m.ID+"/got-it", nil, nil); code(err) != api.CodeForbidden {
		t.Fatalf("the sender marked its own mail read for the other account: %v", err)
	}
	// An account in another project only is no one a party may name.
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "lone", Project: "zz-side", Role: "member"}))
	if _, err := orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "lone"}); code(err) != api.CodeNotFound {
		t.Fatalf("a hidden account answered unlike a missing one: %v", err)
	}
	if _, err := orchclient.AddPersonLink(ctx, box, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "nobody"}); code(err) != api.CodeNotFound {
		t.Fatalf("a missing account: %v", err)
	}
}

// A third party never cuts someone else's live link, and no answer tells a
// missing account from a hidden one or a link it may not touch.
func TestPersonLinkThirdPartyCannotCut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "wren-box", Project: "harbor", Role: "member"}))
	vic := h.tokenClient(api.MintTokenRequest{As: "vic", Project: "harbor", Role: "member"})
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "member"})
	// A co-member's pending claims on vic block nothing real.
	must(orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "tobi", Person: "vic"}))
	must(orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "tobi"}))
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "wren"}))
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "vic", Person: "wren"}))
	if x := linkOf(t, must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "vic", Person: "wren"})), "vic"); !x.Live {
		t.Fatalf("a pending claim naming vic blocked vic's real link: %+v", x)
	}
	_ = vic
	if _, err := orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "wren-box", Person: "tobi"}); code(err) != api.CodeConflict {
		t.Fatalf("a co-member moved someone else's link: %v", err)
	}
	if x := linkOf(t, must(orchclient.PersonLinks(ctx, h.owner)), "wren-box"); !x.Live || x.Person != "wren" {
		t.Fatalf("the link changed: %+v", x)
	}
	msg := func(err error) string {
		if code(err) != api.CodeNotFound {
			t.Fatalf("want not_found: %v", err)
		}
		return err.Error()
	}
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "lone", Project: "zz-side", Role: "member"}))
	hidden := msg(func() error {
		_, err := orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "tobi", Person: "lone"})
		return err
	}())
	missing := msg(func() error {
		_, err := orchclient.AddPersonLink(ctx, tobi, orchapi.PersonLinkRequest{Identity: "tobi", Person: "nobody"})
		return err
	}())
	if hidden != missing {
		t.Fatalf("hidden %q vs missing %q", hidden, missing)
	}
	stranger := msg(func() error { _, err := orchclient.RemovePersonLink(ctx, tobi, "wren-box"); return err }())
	gone := msg(func() error { _, err := orchclient.RemovePersonLink(ctx, tobi, "nobody"); return err }())
	if strings.Replace(stranger, "wren-box", "nobody", 1) != gone {
		t.Fatalf("delete answers differ: %q vs %q", stranger, gone)
	}
}

// an answer that is the authority's own account only by default (a
// phone paired for no one, or a tap on the authority's own socket) never
// becomes the person that account is linked to.
func TestPersonLinkNeverClaimsDefaultAnswers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	agent, _ := h.session("6b6b6b6b")
	// The authority's own account (the harness's local human) belongs to wren.
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "ivo", Person: "wren"}))
	if x := linkOf(t, must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "ivo", Person: "wren"})), "ivo"); !x.Live {
		t.Fatalf("not live: %+v", x)
	}
	onSocket := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "socket", Owner: "wren"}))
	must(orchclient.Choose(ctx, h.owner, "", onSocket.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, h.owner, onSocket.ID)); got.Decision.ApproverPerson != "" {
		t.Fatalf("a socket tap became wren's: %q", got.Decision.ApproverPerson)
	}
	h.pair("spare-phone", ipPhone) // paired for no one: the machine owner's by default
	viaPhone := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "phone", Owner: "wren"}))
	must(orchclient.Decide(ctx, h.device(ipPhone), viaPhone.ID, "a"))
	if got := must(orchclient.GetAsk(ctx, h.owner, viaPhone.ID)); got.Decision.ApproverPerson != "" {
		t.Fatalf("an unbound phone's answer became wren's: %q", got.Decision.ApproverPerson)
	}
	// The other way round (wren linked to the authority's account as main):
	// still never wren's.
	must(orchclient.RemovePersonLink(ctx, h.owner, "ivo"))
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren", Person: "ivo"}))
	if x := linkOf(t, must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "wren", Person: "ivo"})), "wren"); !x.Live {
		t.Fatalf("reverse link not live: %+v", x)
	}
	rev := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "reverse", Owner: "wren"}))
	must(orchclient.Choose(ctx, h.owner, "", rev.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, h.owner, rev.ID)); got.Decision.ApproverPerson != "" {
		t.Fatalf("a socket tap became wren's through a reverse link: %q", got.Decision.ApproverPerson)
	}
	// Mail is never shared into the authority's own account.
	must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "wren", Person: "ivo", ShareMail: boolp(true)}))
	tobi := h.tokenClient(api.MintTokenRequest{As: "tobi", Project: "harbor", Role: "owner"})
	m := must(orchclient.Send(ctx, tobi, orchapi.SendMailRequest{To: "human:wren", Body: "for wren only"}))
	if slices.Contains(m.Participants, "human:ivo") {
		t.Fatalf("wren's mail shared into the authority's account: %v", m.Participants)
	}
	// wren's own answer from wren's own account is wren's.
	own := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "own", Owner: "wren"}))
	must(orchclient.Choose(ctx, wren, "", own.ID, orchapi.DecideRequest{Decision: "a"}))
	if got := must(orchclient.GetAsk(ctx, h.owner, own.ID)); got.Decision.ApproverPerson == "" || got.Decision.ApproverPerson != got.OwnerPerson {
		t.Fatalf("wren's own answer: %q vs %q", got.Decision.ApproverPerson, got.OwnerPerson)
	}
}
