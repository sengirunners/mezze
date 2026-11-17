package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
)

// An answer-only token (SPEC 16 "Answer-only tokens"): minted on the socket
// for the caller's own identity and one tailnet device, it reads the
// project's asks and answers card asks as its owner, unsigned, and nothing
// else.

// peerNet is the tailnet as the daemon's node sees it: the devices a token
// can be pinned to (the harness's WhoIs answers for the same nodes).
type peerNet struct{}

func (peerNet) Dial(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
func (peerNet) Peer(context.Context, string) (core.Remote, error) {
	return core.Remote{}, net.ErrClosed
}
func (peerNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: true, NodeID: "nSELF", DNSName: "zm-mini.example.ts.net", PeerList: []core.NetPeer{
		{Host: "ivo-phone", DNSName: "ivo-phone.example.ts.net", NodeID: "nPHONE", NodeKey: "nodekey:phone", Login: "ivo@example.com", OS: "iOS"},
		{Host: "bea-phone", DNSName: "bea-phone.example.ts.net", NodeID: "nTABLET", NodeKey: "nodekey:tablet", Login: "bea@example.com", OS: "iOS"},
	}}, nil
}
func (peerNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

// newAnswerHarness is newHarness on a daemon whose tailnet lists the
// devices above.
func newAnswerHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWiring(t, nil, func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Full(d)
		d.Net = peerNet{}
		return mods, err
	})
}

// answerToken mints an answer-only token for harbor pinned to ivo's phone,
// and a client holding it on the phone.
func (h *harness) answerToken() (api.MintTokenResponse, *client.Client) {
	h.t.Helper()
	out := must(h.owner.MintToken(context.Background(), api.MintTokenRequest{Limit: orchapi.AnswerLimit, Project: "harbor", Node: "ivo-phone", TTLSeconds: 3600}))
	return out, h.holding(out.Secret, ipPhone)
}

// holding is a client on the tailnet from ip carrying secret.
func (h *harness) holding(secret, ip string) *client.Client {
	c := h.device(ip)
	c.Token, c.Project, c.ProjectFrom = secret, "harbor", "env"
	return c
}

// commandActions is fakeActions on machines that run command actions.
type commandActions struct{ *fakeActions }

func (commandActions) AskActionMachine(context.Context, string, string, bool) (api.MachineStats, error) {
	return api.MachineStats{CommandRuns: true}, nil
}

// isTokenLimit reports the kit's refusal of a limited token on a route
// that does not admit it.
func isTokenLimit(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == api.CodeForbidden && string(e.Data) == `{"reason":"token-limit"}`
}

// keysOf is v's JSON object keys, nested ones as parent.key.
func keysOf(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			out = append(out, prefix+k)
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
			}
		}
	}
	walk("", m)
	slices.Sort(out)
	return out
}

// The token reads the project's asks and answers a card ask: the decision
// is the owner's unsigned one (method owner), names the token as its
// surface, machine and receipt node, carries the same fields and person as
// a socket answer, and verifies; with signed=1 it does not.
func TestAnswerTokenAnswersACard(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	tok, phone := h.answerToken()
	if tok.Token.Limit != orchapi.AnswerLimit || tok.Token.Identity != "ivo" || tok.Token.Device != "ivo-phone.example.ts.net" || tok.Device == nil || tok.Device.NodeID != "nPHONE" {
		t.Fatalf("minted: %+v device %+v", tok.Token, tok.Device)
	}
	agent, _ := h.session("a7a7a7a7")
	it := h.workItem("pick the header colour")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	sockIt := h.workItem("pick the footer colour")
	sockAsk := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: sockIt.ID}, Card: card(), Topic: "footer"}))

	// It reads: the list, the one ask, the Decisions list.
	if got := must(orchclient.Asks(ctx, phone, "", true)); len(got) != 2 {
		t.Fatalf("asks: %d", len(got))
	}
	if got := must(orchclient.GetAsk(ctx, phone, a.ID)); got.ID != a.ID || got.Card == nil {
		t.Fatalf("ask: %+v", got)
	}
	if got := must(orchclient.Decisions(ctx, phone, "", "open")); len(got.Decisions) != 2 {
		t.Fatalf("decisions: %+v", got.Decisions)
	}

	d := must(orchclient.Choose(ctx, phone, "", a.ID, orchapi.DecideRequest{Decision: "b"}))
	machine := "answer-token:" + tok.Token.ID
	if d.Method != orchapi.MethodOwner || d.Surface != orchapi.SurfaceAnswerToken || d.Machine != machine || d.Device != "" ||
		d.Approver != "ivo" || d.Receipt.Node != "owner:"+machine || d.RecordedAt == nil {
		t.Fatalf("decision: %+v", d)
	}
	if cs := h.comments(it.ID); len(cs) != 1 || !strings.HasPrefix(cs[0], `OWNER DECISION (ivo via "`+machine+`", `) {
		t.Fatalf("comments: %q", cs)
	}
	sock := must(orchclient.Choose(ctx, h.owner, "", sockAsk.ID, orchapi.DecideRequest{Decision: "b"}))
	if sock.Surface != orchapi.SurfaceOwner || sock.Machine != "mini" {
		t.Fatalf("socket decision: %+v", sock)
	}
	// The same fields as the socket's answer, and the same person.
	if k, want := keysOf(t, d), keysOf(t, sock); !slices.Equal(k, want) {
		t.Errorf("an answer-token decision's fields %v, a socket one's %v", k, want)
	}
	tokRead, sockRead := must(orchclient.GetAsk(ctx, h.owner, a.ID)), must(orchclient.GetAsk(ctx, h.owner, sockAsk.ID))
	if tokRead.Decision == nil || tokRead.Decision.Surface != orchapi.SurfaceAnswerToken || tokRead.Decision.Machine != machine ||
		sockRead.Decision == nil || tokRead.Decision.ApproverPerson != sockRead.Decision.ApproverPerson {
		t.Fatalf("read back: %+v, socket's %+v", tokRead.Decision, sockRead.Decision)
	}

	// It verifies, from the token too; signed=1 refuses it, as the socket's.
	rc := mustJSON(d.Receipt)
	exp := orchapi.ReceiptExpect{Ask: a.ID, Work: it.ID, Decision: "b", Approver: "ivo"}
	if c := must(orchclient.VerifyReceipt(ctx, phone, rc, exp)); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	signed := exp
	signed.Signed = true
	if c := must(orchclient.VerifyReceipt(ctx, phone, rc, signed)); c.Valid || !strings.Contains(c.Reason, "unsigned") {
		t.Fatalf("verify signed=1: %+v", c)
	}
	pub := must(h.svc.PublicKey())
	if c := approvals.Verify(sock.Receipt, pub, orchapi.ReceiptExpect{Signed: true}); c.Valid {
		t.Fatalf("a socket answer passed signed: %+v", c)
	}
	// A paired device's answer is signed; signed=1 takes it.
	h.pair("ivo-pixel", ipPhone2)
	dev := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "device"}))
	dd := must(orchclient.Decide(ctx, h.device(ipPhone2), dev.ID, "a"))
	if c := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(dd.Receipt), orchapi.ReceiptExpect{Ask: dev.ID, Signed: true})); !c.Valid || !slices.Contains(c.Checked, "signed") {
		t.Fatalf("a device's answer with signed=1: %+v", c)
	}
	if err := phone.Do(ctx, http.MethodGet, client.Query("/v0/receipts/verify", "receipt", string(rc), "signed", "yes"), nil, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("signed=yes: %v", err)
	}
}

// What the token never does: a pull request's ask; a choice that runs
// something or hands a brief; a choice with a confirm word, the word
// typed or not; work items on the Decisions list, whatever the query says;
// any route that does not admit it; any listener but its device's.
func TestAnswerTokenRefusals(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	tok, phone := h.answerToken()
	h.svc.SetLaneHost(commandActions{&fakeActions{pin: true}})
	agent, _ := h.session("b7b7b7b7")
	it := h.workItem("the actions")

	pr := h.ask(h.ownerIn("harbor", ""), 7, shaA)
	if _, err := orchclient.Choose(ctx, phone, "", pr.ID, orchapi.DecideRequest{Decision: "approve"}); code(err) != api.CodeForbidden {
		t.Errorf("a pull request's ask: %v", err)
	}
	withAction := func(topic string, act *orchapi.AskAction) orchapi.Ask {
		c := card()
		c.Action = act
		return must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: topic, Card: c}))
	}
	for name, a := range map[string]orchapi.Ask{
		"a run":     withAction("run", &orchapi.AskAction{Kind: orchapi.ActionRun, Machine: "box-a", Script: "scripts/release.sh", Ref: "main", Commit: shaA}),
		"a command": withAction("command", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "box-a", Command: "make it", Ref: "main", Commit: shaB}),
		"a hand":    must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "hand", Card: handCard("")})),
	} {
		if _, err := orchclient.Choose(ctx, phone, "", a.ID, orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeForbidden || refusalReason(err) != orchapi.ReasonSignedOnly {
			t.Errorf("%s choice: %v", name, err)
		}
	}
	risky := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "risky", Card: handCard(orchapi.RiskIrreversible)}))
	if risky.Card.Action.ConfirmWord == "" {
		t.Fatalf("no confirm word: %+v", risky.Card.Action)
	}
	for _, word := range []string{"", risky.Card.Action.ConfirmWord} {
		if _, err := orchclient.Choose(ctx, phone, "", risky.ID, orchapi.DecideRequest{Decision: "a", Confirm: word}); code(err) != api.CodeForbidden || refusalReason(err) != orchapi.ReasonSignedOnly {
			t.Errorf("a confirm-word choice (word %q): %v", word, err)
		}
	}
	for _, a := range []string{pr.ID, risky.ID} {
		if got := must(orchclient.GetAsk(ctx, h.owner, a)); got.Decision != nil {
			t.Fatalf("decided by a refused answer: %+v", got.Decision)
		}
	}

	// Decisions: asks only, whatever the query says; an owner token sees the
	// work item the same queries name.
	waitingItem := h.workItem("waits on a person", "human")
	owner := h.tokenClient(api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "owner"})
	work := func(c *client.Client, q string) (asks, items int) {
		var in orchapi.Inbox
		if err := c.Do(ctx, http.MethodGet, "/v0/decisions?"+q, nil, &in); err != nil {
			t.Fatalf("decisions?%s: %v", q, err)
		}
		for _, x := range in.Decisions {
			if x.Work != nil {
				items++
			} else if x.Ask != nil {
				asks++
			}
		}
		return asks, items
	}
	for _, q := range []string{"label=human", "label=human&work=1", "for=everyone", "for=agents", "for=everyone&label=human&work=1"} {
		if _, items := work(owner, q); items == 0 && strings.Contains(q, "label=") {
			t.Fatalf("an owner token's decisions?%s list no work item (the fixture is wrong): %s", q, waitingItem.ID)
		}
		if asks, items := work(phone, q); items != 0 || asks == 0 {
			t.Errorf("the answer token's decisions?%s: %d asks, %d work items", q, asks, items)
		}
	}

	// Routes that do not admit it, before any lookup: the same for a real id.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v0/asks"},
		{http.MethodPost, "/v0/asks/" + risky.ID + "/undo"},
		{http.MethodPost, "/v0/asks/" + risky.ID + "/object"},
		{http.MethodPost, "/v0/asks/01JZZZZZZZZZZZZZZZZZZZZZZZ/object"},
		{http.MethodPost, "/v0/asks/" + risky.ID + "/withdraw"},
		{http.MethodGet, "/v0/mail"},
		{http.MethodGet, "/v0/rulings"},
		{http.MethodPost, "/v0/tokens"},
		{http.MethodGet, "/v0/no-such-route"},
	} {
		if err := phone.Do(ctx, c.method, c.path, struct{}{}, nil); !isTokenLimit(err) {
			t.Errorf("%s %s: %v", c.method, c.path, err)
		}
	}

	// Only from its device, over a listener with WhoIs: elsewhere it is an
	// unknown token.
	sock := client.WithHandler(h.dm.SocketHandler())
	sock.Token = tok.Secret
	for name, c := range map[string]*client.Client{
		"the socket":                   sock,
		"another device":               h.holding(tok.Secret, ipTablet),
		"this host's own address":      h.holding(tok.Secret, ipMac),
		"a listener that names nobody": func() *client.Client { c := client.WithHandler(h.dm.Handler); c.Token = tok.Secret; return c }(),
	} {
		if _, err := orchclient.Asks(ctx, c, "harbor", true); code(err) != api.CodeUnauthenticated {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An answer-token decision is never a signed yes: no ruling and no permit
// come of it.
func TestAnswerTokenNeverCountsAsSigned(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	_, phone := h.answerToken()
	agent, _ := h.session("c7c7c7c7")
	const text = "A reviewer agent's clean pass is enough to land a change."
	c := card()
	c.Question = "Add this standing ruling?"
	c.Choices = []orchapi.AskChoice{{Label: "Yes, add it"}, {Label: "No"}}
	c.Context = "Agents keep asking. The ruling, word for word: name \"review-by-agent\", text \"" + text + "\""
	ruling := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: c, Topic: "ruling"}))
	must(orchclient.Choose(ctx, phone, "", ruling.ID, orchapi.DecideRequest{Decision: "a"}))
	if _, err := orchclient.AddRuling(ctx, h.ownerIn("harbor", ""), orchapi.RulingAddRequest{Name: "review-by-agent", Text: text, Ask: ruling.ID}); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a ruling from an answer-token yes: %v", err)
	}
	lane := orchapi.PermitCardRequest{Kind: "lane-start", Days: 30}
	permit := h.permitAsk(agent, lane)
	must(orchclient.Choose(ctx, phone, "", permit.ID, orchapi.DecideRequest{Decision: "yes"}))
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: lane, Ask: permit.ID}); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a permit from an answer-token yes: %v", err)
	}
}

// Revoked, or its identity no longer owning the project: 401 at once.
func TestAnswerTokenLifecycle(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	revoked, phone := h.answerToken()
	if _, err := orchclient.Asks(ctx, phone, "", true); err != nil {
		t.Fatal(err)
	}
	must(h.owner.RevokeToken(ctx, revoked.Token.ID))
	if _, err := orchclient.Asks(ctx, phone, "", true); code(err) != api.CodeUnauthenticated {
		t.Fatalf("a revoked token: %v", err)
	}

	_, phone = h.answerToken()
	// The machine owner always owns its projects through the API; a grant
	// that changes under the token (a backup restored, a future rule) is
	// set by hand here, as the kit's own test does.
	setRole := func(role string) {
		t.Helper()
		res, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE grants SET relation = ? WHERE subject = (SELECT id FROM identities WHERE name = 'ivo') AND project = ? AND area = ''`, role, h.project.ID)
		if err != nil || must(res.RowsAffected()) != 1 {
			t.Fatalf("set ivo's grant to %s: %v", role, err)
		}
	}
	setRole("member")
	if _, err := orchclient.Asks(ctx, phone, "", true); code(err) != api.CodeUnauthenticated {
		t.Fatalf("an owner whose grant fell to member: %v", err)
	}
	setRole("owner")
	if _, err := orchclient.Asks(ctx, phone, "", true); err != nil {
		t.Fatalf("the grant back: %v", err)
	}
}

// Minting is the person's own: refused from a session, for another
// identity, without a device, with an area, without a ttl; a refused mint
// writes nothing and no mint adds a grant.
func TestAnswerTokenMintRefusals(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	grants := func() []api.Grant { return must(h.owner.Grants(ctx, "harbor")) }
	before := grants()
	base := api.MintTokenRequest{Limit: orchapi.AnswerLimit, Project: "harbor", Node: "ivo-phone", TTLSeconds: 3600}
	agent, addr := h.session("d7d7d7d7")
	if _, err := agent.MintToken(ctx, base); code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "inside agent session "+addr) {
		t.Errorf("from a session: %v", err)
	}
	for name, mut := range map[string]func(*api.MintTokenRequest){
		"for another identity": func(r *api.MintTokenRequest) { r.As = "bea" },
		"without a device":     func(r *api.MintTokenRequest) { r.Node = "" },
		"with an area":         func(r *api.MintTokenRequest) { r.Area = "solver" },
		"without a ttl":        func(r *api.MintTokenRequest) { r.TTLSeconds = 0 },
		"another role":         func(r *api.MintTokenRequest) { r.Role = "member" },
		"another limit":        func(r *api.MintTokenRequest) { r.Limit = "admin" },
		"an unknown device":    func(r *api.MintTokenRequest) { r.Node = "nowhere" },
	} {
		req := base
		mut(&req)
		if out, err := h.owner.MintToken(ctx, req); err == nil || out.Secret != "" {
			t.Errorf("%s: minted %+v", name, out.Token)
		}
	}
	if got := must(h.owner.Tokens(ctx)); len(got) != 0 {
		t.Fatalf("a refused mint wrote a token: %+v", got)
	}
	tok, _ := h.answerToken()
	if !maps.Equal(grantSet(before), grantSet(grants())) || tok.Token.Identity != "ivo" {
		t.Fatalf("a mint changed the grants: %+v -> %+v", before, grants())
	}
}

func grantSet(gs []api.Grant) map[string]bool {
	out := map[string]bool{}
	for _, g := range gs {
		out[g.Identity+" "+g.Role+" "+g.Project+"/"+g.Area] = true
	}
	return out
}

// The token is the authority's own account, as the socket is: its answer
// never becomes the person that account is linked to.
func TestAnswerTokenNeverClaimsALinkedPerson(t *testing.T) {
	h := newAnswerHarness(t)
	ctx := context.Background()
	_, phone := h.answerToken()
	wren := h.tokenClient(api.MintTokenRequest{As: "wren", Project: "harbor", Role: "owner"})
	agent, _ := h.session("e7e7e7e7")
	must(orchclient.AddPersonLink(ctx, h.owner, orchapi.PersonLinkRequest{Identity: "ivo", Person: "wren"}))
	if x := linkOf(t, must(orchclient.AddPersonLink(ctx, wren, orchapi.PersonLinkRequest{Identity: "ivo", Person: "wren"})), "ivo"); !x.Live {
		t.Fatalf("not live: %+v", x)
	}
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "token", Owner: "wren"}))
	must(orchclient.Choose(ctx, phone, "", a.ID, orchapi.DecideRequest{Decision: "a"}))
	sock := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: colorCard(), Topic: "socket", Owner: "wren"}))
	must(orchclient.Choose(ctx, h.owner, "", sock.ID, orchapi.DecideRequest{Decision: "a"}))
	got, bySocket := must(orchclient.GetAsk(ctx, h.owner, a.ID)).Decision, must(orchclient.GetAsk(ctx, h.owner, sock.ID)).Decision
	if got == nil || got.Surface != orchapi.SurfaceAnswerToken || got.ApproverPerson != "" || got.NotTheirs != bySocket.NotTheirs {
		t.Fatalf("an answer-token answer became wren's: %+v (socket's: %+v)", got, bySocket)
	}
}
