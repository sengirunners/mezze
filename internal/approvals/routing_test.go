package approvals_test

// Routing asks so people are bugged less: an agent looks up what
// was already ruled (similar), joins an open ask instead of asking again
// (same_as), and an ask goes to the person its work names (owner).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

func colorCard() *orchapi.AskCard {
	c := card()
	c.Context, c.Background = "The schedule page has one button that publishes the month.", "Residents miss it today."
	c.Question = "Which color should the publish button be?"
	c.Choices = []orchapi.AskChoice{{Label: "Blue", Pros: "matches the rest", Cons: "easy to miss"}, {Label: "Orange", Pros: "stands out", Cons: "looks like a warning"}}
	return c
}

// An agent finds an earlier ruling by its words before asking: answered asks
// first with their decision, open ones too; withdrawn and unreadable never.
func TestSimilarAsksFindEarlierRulings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("51515151")
	ruled := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	must(orchclient.Choose(ctx, h.owner, "", ruled.ID, orchapi.DecideRequest{Decision: "b"}))
	open := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "button-color"}))
	gone := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps-again"}))
	must(orchclient.WithdrawAsk(ctx, agent, gone.ID))

	got := must(orchclient.SimilarAsks(ctx, agent, "", "keep last year's call swaps on holidays"))
	if len(got) == 0 || got[0].ID != ruled.ID || got[0].Decision == nil || got[0].Decision.Answer == "" {
		t.Fatalf("similar: %+v", got)
	}
	for _, a := range got {
		if a.ID == gone.ID {
			t.Errorf("a withdrawn ask is no ruling: %+v", a)
		}
	}
	if c := must(orchclient.SimilarAsks(ctx, agent, "", "publish button color")); len(c) == 0 || c[0].ID != open.ID {
		t.Fatalf("an open ask: %+v", c)
	}
	if c := must(orchclient.SimilarAsks(ctx, agent, "", "zebra")); len(c) != 0 {
		t.Fatalf("no shared word: %+v", c)
	}
	if _, err := orchclient.SimilarAsks(ctx, agent, "", " "); code(err) != api.CodeBadRequest {
		t.Fatalf("no words: %v", err)
	}
	guest := h.tokenClient(api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if c := must(orchclient.SimilarAsks(ctx, guest, "", "keep last year's swaps")); len(c) != 0 {
		t.Fatalf("a guest reads others' asks: %+v", c)
	}
}

// A second agent with the same question joins the open ask: its own ask on
// its own work, grouped with the first. The owner's one answer decides both,
// each with its own receipt, comment and mail. Joining an answered ask hands
// back the answer and asks nobody.
func TestJoinGroupsAndOneAnswerClosesEvery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	w1, w2, w3 := h.workItem("first item", "owner:ivo"), h.workItem("second item"), h.workItem("third item")
	a1c, _ := h.session("61616161")
	a2c, a2 := h.session("62626262")
	first := must(orchclient.NewAsk(ctx, a1c, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: w1.ID}, Card: card(), Topic: "keep-swaps"}))

	joined := must(orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID, Subject: orchapi.AskSubject{Work: w2.ID}}))
	if joined.ID == first.ID || joined.SameAs != first.ID || joined.Status != orchapi.AskOpen || joined.Subject.Work != w2.ID ||
		joined.CardSHA != first.CardSHA || joined.Owner != "ivo" {
		t.Fatalf("joined: %+v", joined)
	}
	// Joining a joined ask joins its first.
	third, _ := h.session("63636363")
	j3 := must(orchclient.NewAsk(ctx, third, orchapi.NewAskRequest{SameAs: joined.ID, Subject: orchapi.AskSubject{Work: w3.ID}}))
	if j3.SameAs != first.ID {
		t.Fatalf("chain: %+v", j3)
	}
	// Nothing to join: missing, a pull request's ask, a card given beside it.
	if _, err := orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: "01JZZZZZZZZZZZZZZZZZZZZZZZ"}); code(err) != api.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	pr := h.ask(h.ownerIn("harbor", ""), 7, shaA)
	if _, err := orchclient.NewAsk(ctx, h.ownerIn("harbor", ""), orchapi.NewAskRequest{SameAs: pr.ID}); code(err) != api.CodeBadRequest {
		t.Errorf("a pull request's ask: %v", err)
	}
	if _, err := orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID, Card: colorCard()}); code(err) != api.CodeBadRequest {
		t.Errorf("a card beside same_as: %v", err)
	}

	// The owner answers the joined one; every open ask of the group is decided.
	d := must(orchclient.Choose(ctx, h.owner, "", joined.ID, orchapi.DecideRequest{Decision: "b"}))
	pub := must(h.svc.PublicKey())
	for _, x := range []orchapi.Ask{first, joined, j3} {
		got := must(orchclient.GetAsk(ctx, h.owner, x.ID))
		if got.Decision == nil || got.Decision.Decision != "b" || got.Decision.Method != orchapi.MethodOwner || got.Status != orchapi.AskRecorded {
			t.Fatalf("%s after: %+v", x.ID, got)
		}
		if c := approvals.Verify(got.Decision.Receipt, pub, orchapi.ReceiptExpect{Work: x.Subject.Work, Decision: "b", Ask: x.ID, Project: x.Project, Approver: "ivo"}); !c.Valid {
			t.Fatalf("receipt of %s: %+v", x.ID, c)
		}
	}
	if d.Ask != joined.ID {
		t.Fatalf("decision: %+v", d)
	}
	for _, w := range []orchapi.WorkItem{w1, w2, w3} {
		if cs := h.comments(w.ID); len(cs) != 1 || !strings.HasPrefix(cs[0], "OWNER DECISION") {
			t.Fatalf("comments on %s: %q", w.ID, cs)
		}
	}
	answered := false
	for _, m := range must(orchclient.Inbox(ctx, a2c, orchapi.InboxQuery{})) {
		answered = answered || (m.ReplyTo == joined.ID && strings.Contains(m.Body, "Drop them"))
	}
	if !answered {
		t.Fatalf("the joiner %s got no answer", a2)
	}

	// Late to it: the answer comes back, nobody is asked.
	late, _ := h.session("64646464")
	before := len(must(orchclient.Asks(ctx, h.owner, "", false)))
	_, err := orchclient.NewAsk(ctx, late, orchapi.NewAskRequest{SameAs: first.ID})
	if code(err) != api.CodeConflict || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("join an answered ask: %v", err)
	}
	if e, ok := err.(*api.Error); !ok || !strings.Contains(string(e.Data), "Drop them") {
		t.Fatalf("the answer travels with the refusal: %#v", err)
	}
	if after := len(must(orchclient.Asks(ctx, h.owner, "", false))); after != before {
		t.Fatalf("a new ask was made: %d -> %d", before, after)
	}
}

// A paired device signs for one ask: its decision does not decide the group.
func TestDeviceDecisionStaysOnItsAsk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	a1c, _ := h.session("71717171")
	a2c, _ := h.session("72727272")
	first := must(orchclient.NewAsk(ctx, a1c, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	joined := must(orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID}))
	must(orchclient.Decide(ctx, h.device(ipPhone), first.ID, "a"))
	if got := must(orchclient.GetAsk(ctx, h.owner, joined.ID)); got.Status != orchapi.AskOpen {
		t.Fatalf("joined after a device answer: %+v", got)
	}
}

// An ask goes to the person its work names; a name nobody has names nobody.
func TestAskGoesToTheWorksOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "owner"}))
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "member", Project: "harbor", Role: "member"}))
	agent, _ := h.session("81818181")
	beas := h.workItem("Bea's call", "owner:bea")
	ghosts := h.workItem("nobody's", "owner:ghost")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: beas.ID}, Card: card()}))
	if a.Owner != "bea" || a.To != "human:bea" {
		t.Fatalf("owner from the label: %+v", a)
	}
	if g := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: ghosts.ID}, Card: card()})); g.Owner != "" {
		t.Fatalf("a name nobody has: %q", g.Owner)
	}
	q := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "button-color", Owner: "bea"}))
	if q.Owner != "bea" {
		t.Fatalf("owner named: %+v", q)
	}
	for _, who := range []string{"ghost", "member"} { // nobody; a person who cannot answer
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "button-color-2", Owner: who}); code(err) != api.CodeBadRequest {
			t.Fatalf("owner %s: %v", who, err)
		}
		if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "button-color-2", To: "human:" + who}); code(err) != api.CodeBadRequest {
			t.Fatalf("to human:%s: %v", who, err)
		}
	}
	// --to human:bea and --owner human:bea reach bea, and only
	// bea: the mail goes to human:bea, not to every owner.
	for i, req := range []orchapi.NewAskRequest{{To: "human:bea"}, {Owner: "human:bea"}, {To: "human:bea", Owner: "bea"}} {
		req.Card, req.Topic = colorCard(), fmt.Sprintf("to-bea-%d", i)
		x := must(orchclient.NewAsk(ctx, agent, req))
		if x.Owner != "bea" || x.To != "human:bea" {
			t.Fatalf("%+v: owner %q to %q", req, x.Owner, x.To)
		}
		if got := must(orchclient.GetAsk(ctx, h.owner, x.ID)); got.Owner != "bea" {
			t.Fatalf("ask show lost its person: %+v", got)
		}
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "two-people", To: "human:bea", Owner: "ivo"}); code(err) != api.CodeBadRequest {
		t.Fatalf("to and owner naming two people: %v", err)
	}
	// The asker's own human is the common case: the card still
	// names them. to and owner that name two people are refused here too.
	own := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "to-self", To: "human:ivo"}))
	if own.Owner != "ivo" || own.To != "human:ivo" {
		t.Fatalf("own human: owner %q to %q", own.Owner, own.To)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, own.ID)); got.Owner != "ivo" {
		t.Fatalf("ask show lost the asker's own person: %+v", got)
	}
	// An area guest may not name a person, but still asks its own human.
	if g, err := orchclient.NewAsk(ctx, h.guest(), orchapi.NewAskRequest{Card: colorCard(), Topic: "guest-to-self", To: "human:ivo"}); err != nil || g.To != "human:ivo" || g.Owner != "" {
		t.Fatalf("guest to its own human: %+v %v", g, err)
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "self-and-other", To: "human:ivo", Owner: "bea"}); code(err) != api.CodeBadRequest {
		t.Fatalf("own to and another owner: %v", err)
	}
	found := 0
	for _, x := range must(orchclient.Decisions(ctx, h.owner, "", "open")).Decisions {
		if x.Ask != nil && (x.Ask.ID == a.ID || x.Ask.ID == q.ID) {
			found++
			if x.OwnerName != "bea" {
				t.Errorf("inbox row: %+v", x)
			}
		}
	}
	if found != 2 {
		t.Fatalf("inbox rows: %d", found)
	}
}

// The owner answers the group while a join is on its way: the join sees the
// answer (409 carrying it) instead of staying open behind it.
func TestJoinRacingTheAnswerSeesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a1c, _ := h.session("a1a1a1a1")
	a2c, _ := h.session("a2a2a2a2")
	first := must(orchclient.NewAsk(ctx, a1c, orchapi.NewAskRequest{Card: card(), Topic: "keep-swaps"}))
	approvals.SetBeforeJoinHook(func(string) {
		must(orchclient.Choose(ctx, h.owner, "", first.ID, orchapi.DecideRequest{Decision: "b"}))
	})
	t.Cleanup(func() { approvals.SetBeforeJoinHook(nil) })
	if _, err := orchclient.NewAsk(ctx, a2c, orchapi.NewAskRequest{SameAs: first.ID}); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("join behind the answer: %v", err)
	}
	for _, a := range must(orchclient.Asks(ctx, h.owner, "", true)) {
		t.Errorf("an open ask is left: %+v", a.ID)
	}
}

// One person with two identity names (one GitHub account): an ask
// to any name, an alias or the login, in any case, carries the same
// owner_person; a name nobody answers to is refused with the list.
func TestAskToAnyNameOfOnePerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, as := range []string{"bea", "cyd", "rae"} {
		must(h.owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "harbor", Role: "owner"}))
	}
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "box", Project: "harbor", Role: "member"}))
	must(orchclient.AddAlias(ctx, h.owner, orchapi.AliasRequest{Identity: "cyd", Alias: "cj"}))
	d := h.dm.Deps
	d.LinkedAccount = func(ctx context.Context, id string) (string, string) {
		ids := must(d.Projects.Identities(ctx))
		for _, x := range ids {
			switch {
			case x.ID == id && (x.Name == "bea" || x.Name == "cyd"):
				return "github:42", "BeaGH"
			case x.ID == id && x.Name == "rae":
				return "github:7", "CJ" // bea's alias: ambiguous
			}
		}
		return "", ""
	}
	agent, _ := h.session("91919191")
	hint := func(err error) string {
		var e *api.Error
		if errors.As(err, &e) {
			return e.Hint
		}
		return ""
	}
	var person string
	for i, who := range []string{"bea", "CYD", "human:cyd", "beagh", "@BeaGH"} {
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: fmt.Sprintf("p-%d", i), Owner: who}))
		if a.OwnerPerson != "github:42" || a.AskerHuman != "ivo" {
			t.Fatalf("owner %q: person %q asker %q", who, a.OwnerPerson, a.AskerHuman)
		}
		if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.OwnerPerson != a.OwnerPerson || got.AskerHuman != "ivo" {
			t.Fatalf("ask show lost its person: %+v", got)
		}
		person = a.OwnerPerson
	}
	if r := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "rae", Owner: "rae"})); r.OwnerPerson == "" || r.OwnerPerson == person {
		t.Fatalf("rae is another person: %+v", r.OwnerPerson)
	}
	// rae's login is bea's alias: the alias wins (a login is never checked
	// against names); rae is still reached by name.
	if a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "alias-cj", Owner: "CJ"})); a.OwnerPerson != person || a.Owner != "cyd" || a.To != "human:cyd" {
		t.Fatalf("an alias outranks a login, kept and mailed as its identity: %q %q %q", a.OwnerPerson, a.Owner, a.To)
	}
	// to and owner may be two names of one person, never two people.
	if a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "two-names", To: "human:bea", Owner: "cyd"})); a.OwnerPerson != person {
		t.Fatalf("to and owner, one person: %+v", a.OwnerPerson)
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "two-people", To: "human:bea", Owner: "rae"}); code(err) != api.CodeBadRequest {
		t.Fatalf("to and owner, two people: %v", err)
	}
	// An area member who reads an ask learns no one's account or name.
	ana := h.tokenClient(api.MintTokenRequest{As: "ana", Project: "harbor", Area: "solver", Role: "member"})
	inArea := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "in-area", Area: "solver", Owner: "bea"}))
	if got := must(orchclient.GetAsk(ctx, ana, inArea.ID)); got.OwnerPerson != "" || got.AskerHuman != "" {
		t.Fatalf("area member sees %q %q", got.OwnerPerson, got.AskerHuman)
	}
	for who, want := range map[string]string{"box": "name one of:", "ghost": "bea (cyd, cj, @beagh); ivo; rae (@cj)"} {
		_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "x-" + who, Owner: who})
		if code(err) != api.CodeBadRequest || !strings.Contains(fmt.Sprint(err)+hint(err), want) {
			t.Fatalf("owner %s: %v %s", who, err, hint(err))
		}
	}
	// The work item's owner:<name> label resolves the same way.
	it := h.workItem("Bea's call", "owner:Cyd")
	if a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()})); a.OwnerPerson != person || a.Owner != "cyd" {
		t.Fatalf("from the label: %+v %+v", a.Owner, a.OwnerPerson)
	}
}

// replicaSession is a client acting as session sess on a member machine
// that joined harbor as identity as (a node-bound token, SPEC 17.4),
// calling through the tailnet from ip.
func (h *harness) replicaSession(as, machine, nodeID, ip, sess string) *client.Client {
	h.t.Helper()
	ctx := context.Background()
	d := h.dm.Deps
	if err := d.Projects.SetTeam(ctx, h.project.ID, true); err != nil {
		h.t.Fatal(err)
	}
	inv := must(d.Projects.CreateInvite(ctx, d.Projects.Local(""), project.InviteInput{
		InviteRequest: api.InviteRequest{Project: "harbor", As: as, Role: "member"},
		Authority:     "mini", Fingerprint: project.NodeFingerprint("nSELF")}))
	code := must(project.ParseJoinCode(inv.Code))
	out := must(d.Projects.Redeem(ctx, project.RedeemInput{Secret: code.Secret, Node: nodeID, Machine: machine}))
	c := h.device(ip)
	c.Token, c.Project, c.ProjectFrom, c.As = out.Token, "harbor", "env", machine+"/"+sess
	return c
}

// "me": the asking session, then its parents, to the first
// launcher who is a person answering the project; else the work item's
// owner: label; else the owners, and the reply says which. A box, a
// runner's account or a guest's machine never names the host's owner.
func TestAskToMe(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "owner"}))
	mine, mineAddr := h.session("71717171")
	box := h.replicaSession("dev-01", "dev-01", "nFARM", ipFarm, "b0b0b0b0")
	task := h.replicaSession("dev-02", "dev-02", "nPEER", ipPeer, "7a5c7a5c")
	guest := h.replicaSession("vida", "vida-vm", "nTABLET", ipTablet, "9e5e9e5e")
	runners := h.tokenClient(api.MintTokenRequest{As: "runners", Project: "harbor", Role: "member"})
	// dev-02/7a5c7a5c runs a remote task the owner's session started.
	must(h.dm.Deps.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_line (id, project, from_machine, from_identity, parent, payload, line, state, machine, session, status, created, updated)
		VALUES ('rt-1', ?, 'mini', 'ivo', ?, '{}', 1, 'placed', 'dev-02', 'dev-02/7a5c7a5c', '{}', 0, 0)`, h.project.ID, mineAddr))
	// dev-02 reports the guest's session as its task's: not on dev-02, so it lends no parent.
	must(h.dm.Deps.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_line (id, project, from_machine, from_identity, parent, payload, line, state, machine, session, status, created, updated)
		VALUES ('rt-2', ?, 'mini', 'ivo', ?, '{}', 2, 'placed', 'dev-02', 'vida-vm/9e5e9e5e', '{}', 0, 0)`, h.project.ID, mineAddr))
	forBea := h.workItem("Bea's call", "owner:bea")
	for i, c := range []struct {
		name  string
		c     *client.Client
		to    string
		work  string
		owner string
		via   string
	}{
		{name: "the owner's own session", c: mine, to: "me", owner: "ivo", via: "who launched " + mineAddr},
		{name: "a remote task: its parent's launcher", c: task, to: "ME", owner: "ivo", via: "who launched " + mineAddr},
		{name: "a farm box: the owners, never the box's tailnet login", c: box, to: "me", via: "the project's owners"},
		{name: "a farm box on a labelled item: the label", c: box, to: "me", work: forBea.ID, owner: "bea", via: "label names bea"},
		{name: "the runners account: the owners", c: runners, to: "me", via: "the project's owners"},
		{name: "a guest's machine: never the host's owner", c: guest, to: "me", via: "the project's owners"},
		{name: "a bare name", c: box, to: "Bea", owner: "bea"},
		{name: "a bare GitHub-less name of the host", c: runners, to: "ivo", owner: "ivo"},
	} {
		a, err := orchclient.NewAsk(ctx, c.c, orchapi.NewAskRequest{Card: colorCard(), Topic: fmt.Sprintf("me-%d", i), To: c.to, Subject: orchapi.AskSubject{Work: c.work}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if a.Owner != c.owner || c.owner != "" && a.To != "human:"+c.owner || c.owner == "" && a.To != "owners" || !strings.Contains(a.OwnerVia, c.via) {
			t.Errorf("%s: owner %q to %q via %q", c.name, a.Owner, a.To, a.OwnerVia)
		}
	}
	if _, err := orchclient.NewAsk(ctx, mine, orchapi.NewAskRequest{Card: colorCard(), Topic: "me-twice", To: "me", Owner: "bea"}); code(err) != api.CodeBadRequest {
		t.Fatalf("to me and an owner: %v", err)
	}
	if _, err := orchclient.NewAsk(ctx, mine, orchapi.NewAskRequest{Card: colorCard(), Topic: "ghost", To: "ghost"}); code(err) != api.CodeBadRequest {
		t.Fatalf("a bare name nobody has: %v", err)
	}
}
