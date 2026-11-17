package approvals_test

// Acceptance tests for SPEC section 16 (approvals from a paired device):
// pairing, deciding, refusal of Mac, agent and socket callers, receipt replay
// across project, repo, PR, head and ask, lookalike names and fail-closed
// paths. Devices reach the daemon through the real tailnet RemoteHandler with
// a fake WhoIs; nothing touches a real tailnet or GitHub.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
)

// ---- a fake tailnet node: WhoIs by caller IP ----

type fakeNode struct {
	mu     sync.Mutex
	selfID string
	who    map[string]tailnet.Who // caller IP -> identity
}

func (n *fakeNode) Start() error { return nil }
func (n *fakeNode) Status(context.Context) (tailnet.NodeStatus, error) {
	return tailnet.NodeStatus{State: tailnet.StateRunning, SelfID: n.selfID, SelfDNS: "zm-mini.tail1.ts.net", SelfLogin: "ivo@example.com"}, nil
}
func (n *fakeNode) WhoIs(_ context.Context, remote string) (tailnet.Who, error) {
	host, _, _ := net.SplitHostPort(remote)
	n.mu.Lock()
	defer n.mu.Unlock()
	if w, ok := n.who[host]; ok {
		return w, nil
	}
	return tailnet.Who{}, errors.New("no such peer")
}
func (n *fakeNode) Listen(string, string) (net.Listener, error) { return nil, errors.New("fake") }
func (n *fakeNode) HTTPClient() *http.Client                    { return nil }
func (n *fakeNode) Close() error                                { return nil }

func (n *fakeNode) set(ip string, w tailnet.Who) {
	n.mu.Lock()
	n.who[ip] = w
	n.mu.Unlock()
}

// Device IPs.
const (
	ipPhone   = "100.100.1.1"  // ivo's phone
	ipTablet  = "100.100.1.2"  // bea's phone
	ipMac     = "100.100.1.3"  // the Mac's own Tailscale (where agents run): a host address
	ipSelf    = "100.100.1.4"  // the daemon's tsnet node itself
	ipPeer    = "100.100.1.5"  // another zm daemon
	ipTagged  = "100.100.1.6"  // a tagged CI node
	ipUnknown = "100.100.1.9"  // WhoIs fails
	ipPhone2  = "100.100.1.10" // ivo's second phone (android)
	ipFarm    = "100.100.1.11" // a linux box signed in as ivo
)

type harness struct {
	t       *testing.T
	dm      *server.Daemon
	svc     *approvals.Service
	node    *fakeNode
	tailnet http.Handler
	owner   *client.Client
	pub     *fakePublisher
	clock   *clock
	project api.Project
	other   api.Project
	judge   *fixtureJudge
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakePublisher struct {
	mu  sync.Mutex
	got []orchapi.Decision
}

func (p *fakePublisher) Publish(_ context.Context, _ orchapi.Ask, d orchapi.Decision) error {
	p.mu.Lock()
	p.got = append(p.got, d)
	p.mu.Unlock()
	return nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, nil)
}

// newHarnessWith is newHarness with mut applied to the config first.
func newHarnessWith(t *testing.T, mut func(*config.Config)) *harness {
	t.Helper()
	return newHarnessWiring(t, mut, wiring.Full)
}

// newHarnessWiring is newHarnessWith with its own module wiring.
func newHarnessWiring(t *testing.T, mut func(*config.Config), wire func(*core.Deps) ([]core.Module, error)) *harness {
	t.Helper()
	home, err := os.MkdirTemp("", "zm") // short: socket paths max 103 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	if mut != nil {
		mut(cfg)
	}
	dm, err := server.New(context.Background(), cfg, nil, wire)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	h := &harness{t: t, dm: dm, pub: &fakePublisher{}, clock: &clock{t: time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)}}
	for _, m := range dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			h.svc = s
		}
	}
	if h.svc == nil {
		t.Fatal("approvals module not wired")
	}
	h.svc.Publisher = h.pub
	h.svc.SetClock(h.clock.now)
	h.judge = &fixtureJudge{}
	h.svc.SetJudge(judge.Fake{Fn: h.judge.answer})

	h.node = &fakeNode{selfID: "nSELF", who: map[string]tailnet.Who{}}
	h.node.set(ipPhone, tailnet.Who{Login: "ivo@example.com", Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:phone", OS: "iOS"})
	h.node.set(ipTablet, tailnet.Who{Login: "bea@example.com", Node: "bea-phone", NodeID: "nTABLET", NodeKey: "nodekey:tablet", OS: "iOS"})
	h.node.set(ipPhone2, tailnet.Who{Login: "ivo@example.com", Node: "pixel", NodeID: "nPIXEL", NodeKey: "nodekey:pixel", OS: "android"})
	h.node.set(ipFarm, tailnet.Who{Login: "ivo@example.com", Node: "dev-01", NodeID: "nFARM", NodeKey: "nodekey:farm", OS: "linux"})
	h.node.set(ipMac, tailnet.Who{Login: "ivo@example.com", Node: "ivo-mac", NodeID: "nMAC", NodeKey: "nodekey:mac", OS: "macOS"})
	h.node.set(ipSelf, tailnet.Who{Node: "zm-mini", NodeID: "nSELF", NodeKey: "nodekey:self"})
	h.node.set(ipPeer, tailnet.Who{Login: "ivo@example.com", Node: "zm-studio", NodeID: "nPEER", NodeKey: "nodekey:peer"})
	h.node.set(ipTagged, tailnet.Who{Node: "ci-runner", NodeID: "nCI", NodeKey: "nodekey:ci", Tagged: true})
	tn := tailnet.New(dm.Deps)
	tn.HostAddrs = func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr(ipMac)}, nil
	}
	h.tailnet = tn.RemoteHandler(h.node)

	h.owner = client.WithHandler(dm.SocketHandler())
	ctx := context.Background()
	if h.project, err = h.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.owner.AddArea(ctx, "harbor", api.Area{Name: "solver"}); err != nil {
		t.Fatal(err)
	}
	if h.other, err = h.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zz-side"}); err != nil {
		t.Fatal(err)
	}
	return h
}

// device is a client on the tailnet from ip, naming the daemon by host.
func (h *harness) deviceAt(ip, host string) *client.Client {
	return client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = ip + ":41641"
		r.Host = host
		h.tailnet.ServeHTTP(w, r)
	}))
}

// The bare node name: the full MagicDNS name needs a running node to know
// it (core.OwnTailnetHost; TestOwnHostUpperMachine covers it).
func (h *harness) device(ip string) *client.Client {
	return h.deviceAt(ip, "zm-"+h.dm.Deps.Config.Machine+":7433")
}

// ownerIn is the owner on the socket writing into project (as a session when as != "").
func (h *harness) ownerIn(project, as string) *client.Client {
	c := client.WithHandler(h.dm.SocketHandler())
	c.Project, c.ProjectFrom, c.As = project, "flag", as
	return c
}

// session registers an agent session in harbor/solver and returns a client acting as it.
func (h *harness) session(id string) (*client.Client, string) {
	h.t.Helper()
	s, err := orchclient.Register(context.Background(), h.ownerIn("harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: id + "-uuid"})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.ownerIn("harbor/solver", s.Address), s.Address
}

// pairFirst adds and pairs the first approver from ip.
func (h *harness) pair(name, ip string) orchapi.Approver {
	h.t.Helper()
	ctx := context.Background()
	add, err := orchclient.AddApprover(ctx, h.owner, name)
	if err != nil {
		h.t.Fatal(err)
	}
	a, err := orchclient.Pair(ctx, h.device(ip), add.Code)
	if err != nil {
		h.t.Fatalf("pair %s from %s: %v", name, ip, err)
	}
	return a
}

const (
	shaA = "0123456789abcdef0123456789abcdef01234567"
	shaB = "89abcdef0123456789abcdef0123456789abcdef"
)

func (h *harness) ask(c *client.Client, pr int, sha string) orchapi.Ask {
	h.t.Helper()
	a, err := orchclient.NewAsk(context.Background(), c, orchapi.NewAskRequest{Body: fmt.Sprintf("merge PR %d?", pr),
		Subject: orchapi.AskSubject{Repo: "example-org/harbor", PR: pr, HeadSHA: sha}})
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func code(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// must returns v or panics (which fails the test with a stack).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (h *harness) events(types string) []api.Event {
	h.t.Helper()
	var out []api.Event
	err := h.owner.Events(context.Background(), api.EventQuery{Types: types, Since: "0", NoLive: true}, func(e api.Event) error {
		out = append(out, e)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// ---- pairing and deciding ----

func TestPairAndDecide(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	phone := h.device(ipPhone)

	add := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	if add.Approver.State != orchapi.ApproverPending || add.Confirm || add.Code == "" || !strings.Contains(add.PairPath, "/v0/approvals?code=") {
		t.Fatalf("add: %+v", add)
	}
	// The pairing page, from the QR's URL, offers the code.
	resp, err := phone.Stream(ctx, http.MethodGet, add.PairPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	page := readAll(resp)
	if !strings.Contains(page, `id="pair"`) || !strings.Contains(page, strings.ReplaceAll(add.Code, "-", "")) ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("pairing page: %s", page)
	}
	v := must(orchclient.DeviceView(ctx, phone))
	if v.Approver != nil || len(v.Asks) != 0 {
		t.Fatalf("unpaired device sees: %+v", v)
	}
	a := must(orchclient.Pair(ctx, phone, strings.ToLower(add.Code))) // a code may be typed loosely
	if a.State != orchapi.ApproverActive || a.ConfirmedBy != "bootstrap" || a.Node != "nPHONE" || a.NodeName != "ivo-phone" {
		t.Fatalf("first approver: %+v", a)
	}
	if _, err := orchclient.Pair(ctx, phone, add.Code); code(err) != api.CodeConflict {
		t.Fatalf("pairing twice: %v", err)
	}

	// An agent asks; its session goes needs_human until the decision.
	agent, addr := h.session("aaaaaaaa")
	ask := h.ask(agent, 7, shaA)
	if ask.Subject.Repo != "github.com/example-org/harbor" || ask.AskedBy != addr || ask.To != "owners" || ask.Area != "solver" {
		t.Fatalf("ask: %+v", ask)
	}
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State != orchapi.StateNeedsHuman {
		t.Fatalf("asking session state %s, want needs_human", s.State)
	}
	if m := must(orchclient.GetMail(ctx, h.owner, ask.ID)); m.Needs != orchapi.NeedsHuman || m.To != "owners" {
		t.Fatalf("ask mail: %+v", m)
	}

	v = must(orchclient.DeviceView(ctx, phone))
	if len(v.Asks) != 1 || v.Asks[0].ID != ask.ID {
		t.Fatalf("approver view: %+v", v)
	}
	resp, err = phone.Stream(ctx, http.MethodGet, "/v0/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}
	if page := readAll(resp); !strings.Contains(page, `src="/v0/approvals/app.js"`) || strings.Contains(page, "merge PR 7?") {
		t.Fatalf("approver page does not hand the asks to app.js: %s", page)
	}
	if !slices.ContainsFunc(v.Decisions, func(d orchapi.InboxItem) bool { return d.Ask != nil && d.Ask.ID == ask.ID }) {
		t.Fatalf("approver view's decisions lack the pull request's ask: %+v", v.Decisions)
	}

	if _, err := orchclient.Decide(ctx, phone, ask.ID, "maybe"); code(err) != api.CodeBadRequest {
		t.Fatalf("decision outside the options: %v", err)
	}
	d := must(orchclient.Decide(ctx, phone, ask.ID, orchapi.DecisionApprove))
	r := d.Receipt
	if d.Approver != "ivo" || r.Decision != "approve" || r.Node != "nPHONE" || r.Project != h.project.ID || r.Ask != ask.ID ||
		r.Subject != ask.Subject || r.V != orchapi.ReceiptVersion || r.Sig == "" {
		t.Fatalf("decision: %+v", d)
	}
	// The same answer again is the same receipt; any other answer is refused.
	if again := must(orchclient.Decide(ctx, phone, ask.ID, orchapi.DecisionApprove)); again.Receipt != r {
		t.Fatalf("repeat decision changed the receipt: %+v", again.Receipt)
	}
	if _, err := orchclient.Decide(ctx, phone, ask.ID, orchapi.DecisionDecline); code(err) != api.CodeConflict {
		t.Fatalf("conflicting second answer: %v", err)
	}

	// The ask's mail is answered and taken: the agent is no longer blocked.
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("asking session still needs_human after the decision")
	}
	if m := must(orchclient.GetMail(ctx, h.owner, ask.ID)); m.AckedBy == "" {
		t.Fatalf("ask mail not taken: %+v", m)
	}
	got := must(orchclient.GetAsk(ctx, agent, ask.ID))
	if got.Decision == nil || got.Decision.Receipt != r {
		t.Fatalf("asker sees: %+v", got)
	}

	// Receipts verify on the daemon and offline with the published key.
	chk := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), orchapi.ReceiptExpect{Project: "harbor", Ask: ask.ID,
		Repo: "example-org/harbor", PR: 7, HeadSHA: shaA, Decision: "approve", Approver: "ivo"}))
	if !chk.Valid || len(chk.Checked) != 7 {
		t.Fatalf("daemon verify: %+v", chk)
	}
	pubInfo := must(orchclient.ReceiptKey(ctx, h.device(ipUnknown))) // public: even an unknown node may fetch the key
	pub := must(approvals.ParsePublicKey(pubInfo.Line))
	if c := approvals.Verify(r, pub, orchapi.ReceiptExpect{Repo: "github.com/example-org/harbor", PR: 7, HeadSHA: shaA}); !c.Valid {
		t.Fatalf("offline verify: %+v", c)
	}

	// Events: approver added (machine-level), decision recorded (project).
	if es := h.events("dev.zm.approver."); len(es) != 1 || es[0].Type != orchapi.EventApproverAdded || es[0].Subject != "ivo" {
		t.Fatalf("approver events: %+v", es)
	}
	es := h.events("dev.zm.decision.")
	if len(es) != 1 || es[0].Project != h.project.ID || es[0].Area != "solver" || es[0].Subject != ask.ID {
		t.Fatalf("decision events: %+v", es)
	}
	h.svc.Close() // waits for the background publish
	if len(h.pub.got) != 1 || h.pub.got[0].Receipt != r {
		t.Fatalf("publisher got %+v", h.pub.got)
	}
}

func newKey(t *testing.T) ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- refusals: socket, agents, tokens, the Mac, other nodes ----

func TestSocketAgentAndMacNeverDecide(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("bbbbbbbb")
	ask := h.ask(agent, 9, shaA)

	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "owner"}))
	// ivo@ (the shared login) is a mapped identity: the login must not help.
	if _, err := h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "jtail", TailscaleLogin: "ivo@example.com"}); err != nil {
		t.Fatal(err)
	}
	withTok := func(c *client.Client) *client.Client { c.Token = tok.Secret; return c }
	cases := []struct {
		name string
		c    *client.Client
		want string
	}{
		{"socket owner", h.owner, api.CodeForbidden},
		{"socket agent session", h.ownerIn("harbor/solver", addr), api.CodeForbidden},
		{"socket with an owner token", withTok(client.WithHandler(h.dm.SocketHandler())), api.CodeForbidden},
		{"paired phone carrying a token", withTok(h.device(ipPhone)), api.CodeForbidden},
		{"the Mac's own Tailscale node (shared login)", h.device(ipMac), api.CodeForbidden},
		{"the daemon's own tsnet node", h.device(ipSelf), api.CodeForbidden},
		{"another zm daemon", h.device(ipPeer), api.CodeForbidden},
		{"a tagged node", h.device(ipTagged), api.CodeForbidden},
		{"an unknown node (WhoIs fails)", h.device(ipUnknown), api.CodeUnauthenticated},
		{"an unmarked network listener", client.WithHandler(h.dm.Handler), api.CodeUnauthenticated},
	}
	for _, c := range cases {
		if _, err := orchclient.Decide(ctx, c.c, ask.ID, orchapi.DecisionApprove); code(err) != c.want {
			t.Errorf("%s: decide answered %v, want %s", c.name, err, c.want)
		}
		// Hidden and missing asks answer the same: the check is the device, not the ask.
		_, e1 := orchclient.Decide(ctx, c.c, ask.ID, orchapi.DecisionApprove)
		_, e2 := orchclient.Decide(ctx, c.c, "01JZZZZZZZZZZZZZZZZZZZZZZZ", orchapi.DecisionApprove)
		if fmt.Sprint(e1) != fmt.Sprint(e2) {
			t.Errorf("%s: existing ask %v != missing ask %v", c.name, e1, e2)
		}
		if v, err := orchclient.DeviceView(ctx, c.c); err == nil && (v.Approver != nil || len(v.Asks) > 0) {
			t.Errorf("%s: the device view showed an approver or asks: %+v", c.name, v)
		}
	}
	if a := must(orchclient.GetAsk(ctx, agent, ask.ID)); a.Decision != nil {
		t.Fatalf("decided by a refused caller: %+v", a.Decision)
	}

	// Pairing is refused from this machine and from non-person nodes, even with a valid code.
	add := must(orchclient.AddApprover(ctx, h.owner, "bea"))
	for _, ip := range []string{ipMac, ipSelf, ipPeer, ipTagged} {
		if _, err := orchclient.Pair(ctx, h.device(ip), add.Code); code(err) != api.CodeForbidden {
			t.Errorf("pair from %s: %v", ip, err)
		}
	}
	for _, c := range []*client.Client{h.owner, withTok(h.device(ipTablet))} {
		if _, err := orchclient.Pair(ctx, c, add.Code); code(err) != api.CodeForbidden {
			t.Errorf("pair from the socket or with a token: %v", err)
		}
	}
	// The code survived those refusals: the right device can still use it.
	if a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code)); a.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("bea: %+v", a)
	}
	// Remote callers never pass the local approver routes.
	if _, err := orchclient.AddApprover(ctx, h.device(ipPhone), "mallory"); err == nil {
		t.Fatal("a device added an approver")
	}
	if _, err := orchclient.RemoveApprover(ctx, withTok(h.device(ipPhone)), "bea"); code(err) != api.CodeForbidden {
		t.Fatalf("token removed an approver: %v", err)
	}
}

// Every later approver needs an existing approver's confirmation, from that
// approver's own device.
func TestLaterApproverNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	add := must(orchclient.AddApprover(ctx, h.owner, "bea"))
	if !add.Confirm {
		t.Fatal("a later add does not say it needs confirmation")
	}
	tablet := h.device(ipTablet)
	if _, err := orchclient.Pair(ctx, h.device(ipPhone), add.Code); code(err) != api.CodeConflict {
		t.Fatalf("a bound device paired again: %v", err)
	}
	if a := must(orchclient.Pair(ctx, tablet, add.Code)); a.State != orchapi.ApproverUnconfirmed || a.ConfirmedBy != "" {
		t.Fatalf("bea after pairing: %+v", a)
	}
	agent, _ := h.session("cccccccc")
	ask := h.ask(agent, 3, shaA)
	if _, err := orchclient.Decide(ctx, tablet, ask.ID, orchapi.DecisionApprove); code(err) != api.CodeForbidden {
		t.Fatalf("unconfirmed approver decided: %v", err)
	}
	if v := must(orchclient.DeviceView(ctx, tablet)); len(v.Asks) != 0 {
		t.Fatalf("unconfirmed approver sees asks: %+v", v.Asks)
	}
	if _, err := orchclient.ConfirmApprover(ctx, tablet, "bea"); code(err) != api.CodeForbidden {
		t.Fatalf("self-confirmation: %v", err)
	}
	if _, err := orchclient.ConfirmApprover(ctx, h.owner, "bea"); code(err) != api.CodeForbidden {
		t.Fatalf("socket confirmation: %v", err)
	}
	if v := must(orchclient.DeviceView(ctx, h.device(ipPhone))); len(v.Confirm) != 1 || v.Confirm[0].NodeName != "bea-phone" {
		t.Fatalf("confirmer sees: %+v", v.Confirm)
	}
	if _, err := orchclient.ConfirmApprover(ctx, h.device(ipPhone), "Bea"); code(err) != api.CodeBadRequest {
		t.Fatalf("confirm by a non-canonical name: %v", err)
	}
	a := must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "bea"))
	if a.State != orchapi.ApproverActive || a.ConfirmedBy != "ivo" {
		t.Fatalf("bea confirmed: %+v", a)
	}
	if d := must(orchclient.Decide(ctx, tablet, ask.ID, orchapi.DecisionDecline)); d.Approver != "bea" || d.Receipt.Node != "nTABLET" {
		t.Fatalf("bea's decision: %+v", d)
	}
	if es := h.events("dev.zm.approver.added"); len(es) != 2 {
		t.Fatalf("added events: %d", len(es))
	}
}

// ---- receipts ----

func TestReceiptReplayFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("dddddddd")
	a1, a2 := h.ask(agent, 7, shaA), h.ask(agent, 8, shaB)
	r := must(orchclient.Decide(ctx, h.device(ipPhone), a1.ID, orchapi.DecisionApprove)).Receipt
	pub := must(h.svc.PublicKey())
	good := orchapi.ReceiptExpect{Project: h.project.ID, Ask: a1.ID, Repo: "example-org/harbor", PR: 7, HeadSHA: shaA, Decision: "approve", Approver: "ivo"}
	if c := approvals.Verify(r, pub, good); !c.Valid {
		t.Fatalf("good receipt: %+v", c)
	}
	replays := map[string]func(*orchapi.ReceiptExpect){
		"another project": func(e *orchapi.ReceiptExpect) { e.Project = h.other.ID },
		"another ask":     func(e *orchapi.ReceiptExpect) { e.Ask = a2.ID },
		"another repo":    func(e *orchapi.ReceiptExpect) { e.Repo = "example-org/other" },
		"another PR":      func(e *orchapi.ReceiptExpect) { e.PR = 8 },
		"another head":    func(e *orchapi.ReceiptExpect) { e.HeadSHA = shaB },
		"a head prefix":   func(e *orchapi.ReceiptExpect) { e.HeadSHA = shaA[:12] },
		"a decline gate":  func(e *orchapi.ReceiptExpect) { e.Decision = "decline" },
		"a lookalike approver": func(e *orchapi.ReceiptExpect) {
			e.Approver = "Ivo"
		},
		"an approver prefix": func(e *orchapi.ReceiptExpect) { e.Approver = "iv" },
	}
	for name, f := range replays {
		e := good
		f(&e)
		if c := approvals.Verify(r, pub, e); c.Valid || c.Reason == "" {
			t.Errorf("replay to %s verified: %+v", name, c)
		}
		// The daemon's verify route agrees.
		if c := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), e)); c.Valid {
			t.Errorf("daemon verified a replay to %s", name)
		}
	}
	// Tampering with any signed field breaks the signature.
	tamper := map[string]func(*orchapi.Receipt){
		"pr":       func(x *orchapi.Receipt) { x.Subject.PR = 8 },
		"head":     func(x *orchapi.Receipt) { x.Subject.HeadSHA = shaB },
		"ask":      func(x *orchapi.Receipt) { x.Ask = a2.ID },
		"project":  func(x *orchapi.Receipt) { x.Project = h.other.ID },
		"decision": func(x *orchapi.Receipt) { x.Decision = "decline" },
		"approver": func(x *orchapi.Receipt) { x.Approver = "bea" },
		"node":     func(x *orchapi.Receipt) { x.Node = "nMAC" },
		"time":     func(x *orchapi.Receipt) { x.Time = "2027-01-01T00:00:00.000Z" },
	}
	for name, f := range tamper {
		x := r
		f(&x)
		if c := approvals.Verify(x, pub, orchapi.ReceiptExpect{}); c.Valid {
			t.Errorf("tampered %s verified", name)
		}
	}
	// Another key, garbage, and a receipt for one project checked by slug on the daemon.
	other := approvals.Sign(newKey(t), r)
	if c := approvals.Verify(other, pub, orchapi.ReceiptExpect{}); c.Valid {
		t.Error("a receipt signed by another key verified")
	}
	if c := must(orchclient.VerifyReceipt(ctx, h.owner, []byte(`{"v":"zm-receipt/1"}`), orchapi.ReceiptExpect{})); c.Valid {
		t.Error("garbage verified")
	}
	if c := must(orchclient.VerifyReceipt(ctx, h.owner, mustJSON(r), orchapi.ReceiptExpect{Project: "zz-side"})); c.Valid {
		t.Error("receipt verified for another project's slug")
	}
	// A guest naming a hidden project slug gets the same answer as a missing one.
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	gc := h.device(ipTablet)
	gc.Token = g.Secret
	_, eh := orchclient.VerifyReceipt(ctx, gc, mustJSON(r), orchapi.ReceiptExpect{Project: "zz-side"})
	_, em := orchclient.VerifyReceipt(ctx, gc, mustJSON(r), orchapi.ReceiptExpect{Project: "nope-zz"})
	if strings.ReplaceAll(fmt.Sprint(eh), "zz-side", "X") != strings.ReplaceAll(fmt.Sprint(em), "nope-zz", "X") || code(eh) != api.CodeNotFound {
		t.Fatalf("hidden %v vs missing %v", eh, em)
	}
}

// ---- name hygiene ----

func TestLookalikeNamesRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// "ivo" is the machine owner's identity.
	for _, name := range []string{"Ivo", " ivo", "ivo ", "i vo", "ｉｖｏ", "ivо" /* Cyrillic o */, "none", "owners", ""} {
		if _, err := orchclient.AddApprover(ctx, h.owner, name); code(err) != api.CodeBadRequest {
			t.Errorf("add %q: %v, want bad_request", name, err)
		}
	}
	for _, name := range []string{"iv0", "IV0", "i-vo", "i.vo", "ivo_"} {
		if _, err := orchclient.AddApprover(ctx, h.owner, name); code(err) == "ok" {
			t.Errorf("add %q (a lookalike of identity ivo) was accepted", name)
		}
	}
	must(orchclient.AddApprover(ctx, h.owner, "mallory"))
	for _, name := range []string{"rnallory", "ma11ory", "mal_lory"} {
		if _, err := orchclient.AddApprover(ctx, h.owner, name); code(err) != api.CodeConflict {
			t.Errorf("add %q next to approver mallory: %v, want conflict", name, err)
		}
	}
	must(orchclient.AddApprover(ctx, h.owner, "ivo")) // the exact identity name is fine
	h.pair("bea", ipPhone)
	// Removal matches exact names only: never a prefix, suffix, case or lookalike.
	for _, name := range []string{"be", "bea2", "ea", "BEA", "bеa"} {
		if _, err := orchclient.RemoveApprover(ctx, h.owner, name); code(err) == "ok" {
			t.Errorf("rm %q removed something", name)
		}
	}
	if as := must(orchclient.Approvers(ctx, h.owner)); len(as) != 3 {
		t.Fatalf("approvers after refused removals: %+v", as)
	}
}

// ---- fail closed ----

func TestFailClosed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	phone := h.device(ipPhone)

	// An expired code.
	add := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	h.clock.add(approvals.CodeTTL + time.Second)
	if _, err := orchclient.Pair(ctx, phone, add.Code); code(err) != api.CodeForbidden {
		t.Fatalf("expired code: %v", err)
	}
	// A new code for the still-pending name works; the old one never again.
	add2 := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	if _, err := orchclient.Pair(ctx, phone, add.Code); code(err) != api.CodeForbidden {
		t.Fatalf("replaced code: %v", err)
	}
	// Guessing is cut off per node.
	for i := 0; i < 5; i++ {
		_, _ = orchclient.Pair(ctx, h.device(ipTablet), "AAAA-AAAA")
	}
	if _, err := orchclient.Pair(ctx, h.device(ipTablet), add2.Code); code(err) != api.CodeForbidden {
		t.Fatalf("a node past the guess limit paired: %v", err)
	}
	must(orchclient.Pair(ctx, phone, add2.Code))

	agent, _ := h.session("eeeeeeee")
	ask := h.ask(agent, 5, shaA)
	// The same stable node ID with another node key (re-registered or stolen ID): refused.
	h.node.set(ipPhone, tailnet.Who{Login: "ivo@example.com", Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:other"})
	if _, err := orchclient.Decide(ctx, phone, ask.ID, orchapi.DecisionApprove); code(err) != api.CodeForbidden {
		t.Fatalf("decide with a changed node key: %v", err)
	}
	h.node.set(ipPhone, tailnet.Who{Login: "ivo@example.com", Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:phone"})
	// A device on the Mac claiming the phone's node ID still comes from a host address.
	h.node.set(ipMac, tailnet.Who{Login: "ivo@example.com", Node: "ivo-mac", NodeID: "nPHONE", NodeKey: "nodekey:phone"})
	if _, err := orchclient.Decide(ctx, h.device(ipMac), ask.ID, orchapi.DecisionApprove); code(err) != api.CodeForbidden {
		t.Fatalf("decide from the Mac with the phone's identity: %v", err)
	}
	// rm takes effect at once.
	must(orchclient.RemoveApprover(ctx, h.owner, "ivo"))
	if _, err := orchclient.Decide(ctx, phone, ask.ID, orchapi.DecisionApprove); code(err) != api.CodeForbidden {
		t.Fatalf("removed approver decided: %v", err)
	}
	if es := h.events("dev.zm.approver.removed"); len(es) != 1 {
		t.Fatalf("removed events: %d", len(es))
	}
	// Device POSTs need JSON with the protocol header, from this daemon's host name.
	for name, req := range map[string]*http.Request{
		"form post": func() *http.Request {
			r := httptest.NewRequest("POST", "/v0/approvals/pair", strings.NewReader("code=AAAA"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}(),
		"cross-site": func() *http.Request {
			r := httptest.NewRequest("POST", "/v0/approvals/pair", strings.NewReader(`{"code":"x"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(api.HeaderProtocol, api.Protocol)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			return r
		}(),
	} {
		req.RemoteAddr, req.Host = ipTablet+":1", "zm-mini"
		rec := httptest.NewRecorder()
		h.tailnet.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// The protocol header counts under its Potluck-* name too (rename step
	// 1: read both); with neither the device POST is refused as before.
	for _, c := range []struct {
		name, header, value string
		refused             bool
	}{
		{"old name", api.HeaderProtocol, api.Protocol, false},
		{"new name", api.HeaderNew(api.HeaderProtocol), api.WireProtocol, false},
		{"neither", "", "", true},
		{"new name empty", api.HeaderNew(api.HeaderProtocol), "", true},
	} {
		r := httptest.NewRequest("POST", "/v0/approvals/pair", strings.NewReader(`{"code":"x"}`))
		r.Header.Set("Content-Type", "application/json")
		if c.header != "" {
			r.Header[c.header] = []string{c.value}
		}
		r.RemoteAddr, r.Host = ipTablet+":1", "zm-mini"
		rec := httptest.NewRecorder()
		h.tailnet.ServeHTTP(rec, r)
		if got := strings.Contains(rec.Body.String(), "Potluck-Protocol header"); got != c.refused {
			t.Errorf("protocol header %s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if _, err := orchclient.DeviceView(ctx, h.deviceAt(ipTablet, "evil.example.com")); code(err) != api.CodeForbidden {
		t.Errorf("rebound host name: %v", err)
	}
	if _, err := orchclient.DeviceView(ctx, h.deviceAt(ipTablet, "100.100.1.4:7433")); err != nil {
		t.Errorf("tailnet IP host: %v", err)
	}
	// A daemon that is not running: the client refuses (exit 3 in the CLI).
	if _, err := orchclient.VerifyReceipt(ctx, client.NewUnix("/nonexistent/zm.sock"), []byte("{}"), orchapi.ReceiptExpect{}); code(err) != api.CodeUnavailable {
		t.Fatalf("no daemon: %v", err)
	}
}

// ---- asks follow the restricted-mail rule ----

func TestAsksScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("ffffffff")
	ask := h.ask(agent, 1, shaA)
	sideAsk := must(orchclient.NewAsk(ctx, h.ownerIn("zz-side", ""), orchapi.NewAskRequest{Body: "HIDDEN side",
		Subject: orchapi.AskSubject{Repo: "a/b", PR: 2, HeadSHA: shaA}}))
	g := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token, guest.Project, guest.ProjectFrom = g.Secret, "harbor", "env"
	mine := must(orchclient.NewAsk(ctx, guest, orchapi.NewAskRequest{Body: "may I?", Subject: orchapi.AskSubject{Repo: "a/b", PR: 3, HeadSHA: shaB}}))
	if mine.Area != "solver" || mine.AskedBy != "human:vida" {
		t.Fatalf("guest ask: %+v", mine)
	}
	list := must(orchclient.Asks(ctx, guest, "", false))
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("guest lists: %+v", list)
	}
	for _, id := range []string{ask.ID, sideAsk.ID} {
		_, eh := orchclient.GetAsk(ctx, guest, id)
		_, em := orchclient.GetAsk(ctx, guest, "01JZZZZZZZZZZZZZZZZZZZZZZZ")
		if code(eh) != api.CodeNotFound || strings.ReplaceAll(fmt.Sprint(eh), id, "X") != strings.ReplaceAll(fmt.Sprint(em), "01JZZZZZZZZZZZZZZZZZZZZZZZ", "X") {
			t.Errorf("hidden ask %s: %v vs %v", id, eh, em)
		}
	}
	if all := must(orchclient.Asks(ctx, h.owner, "", true)); len(all) != 3 {
		t.Fatalf("owner open asks: %d", len(all))
	}

	// dev.zm.ask.created carries the Ask with the restricted-mail audience:
	// the owner sees all three, the guest only its own (replay and live
	// share the stored audience).
	if es := h.events("dev.zm.ask."); len(es) != 3 {
		t.Fatalf("owner ask events: %d", len(es))
	}
	var gev []api.Event
	if err := guest.Events(ctx, api.EventQuery{Types: "dev.zm.ask.", Since: "0", NoLive: true}, func(e api.Event) error {
		gev = append(gev, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(gev) != 1 || gev[0].Subject != mine.ID || gev[0].Type != orchapi.EventAskCreated || gev[0].Area != "solver" {
		t.Fatalf("guest ask events: %+v", gev)
	}
	var evAsk orchapi.Ask
	if err := json.Unmarshal(gev[0].Data, &evAsk); err != nil || evAsk.Subject.PR != 3 || evAsk.AskedBy != "human:vida" {
		t.Fatalf("ask event data: %s (%v)", gev[0].Data, err)
	}

	// An open ask's mail is answered only by a decision (cockpit gap G2): the
	// socket's human, the asker and an owner token are refused ack and
	// reply; a hidden ask answers exactly like a missing mail id.
	ownerTok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "owner"}))
	ownerNet := client.WithHandler(h.dm.Handler)
	ownerNet.Token, ownerNet.Project, ownerNet.ProjectFrom = ownerTok.Secret, "harbor", "env"
	for _, tc := range []struct {
		name string
		c    *client.Client
		id   string
	}{{"socket human", h.ownerIn("harbor", ""), ask.ID}, {"owner token", ownerNet, ask.ID}, {"guest asker", guest, mine.ID}} {
		if _, err := orchclient.Ack(ctx, tc.c, tc.id); code(err) != api.CodeForbidden || !strings.Contains(fmt.Sprint(err), "open ask") {
			t.Errorf("%s acked an open ask's mail: %v", tc.name, err)
		}
		if _, err := orchclient.Reply(ctx, tc.c, tc.id, orchapi.ReplyRequest{Body: "fine"}); code(err) != api.CodeForbidden || !strings.Contains(fmt.Sprint(err), "open ask") {
			t.Errorf("%s replied to an open ask's mail: %v", tc.name, err)
		}
	}
	if a := must(orchclient.GetAsk(ctx, h.owner, ask.ID)); a.Decision != nil {
		t.Fatalf("ask decided by a refused ack: %+v", a)
	}
	const missing = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	for _, id := range []string{ask.ID, sideAsk.ID} {
		for _, call := range []func(string) error{
			func(id string) error { _, err := orchclient.Ack(ctx, guest, id); return err },
			func(id string) error {
				_, err := orchclient.Reply(ctx, guest, id, orchapi.ReplyRequest{Body: "x"})
				return err
			},
		} {
			eh, em := call(id), call(missing)
			if code(eh) != api.CodeNotFound || strings.ReplaceAll(fmt.Sprint(eh), id, "X") != strings.ReplaceAll(fmt.Sprint(em), missing, "X") {
				t.Errorf("hidden ask %s ack/reply: %v vs %v", id, eh, em)
			}
		}
	}
	// Once decided, the mail is taken by the decision; replies in the thread work again.
	h.pair("ivo", ipPhone)
	must(orchclient.Decide(ctx, h.device(ipPhone), ask.ID, orchapi.DecisionApprove))
	if _, err := orchclient.Reply(ctx, h.ownerIn("harbor", ""), ask.ID, orchapi.ReplyRequest{Body: "thanks"}); err != nil {
		t.Fatalf("reply after the decision: %v", err)
	}
	if _, err := orchclient.NewAsk(ctx, h.owner, orchapi.NewAskRequest{Body: "x", Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA[:7]}}); code(err) != api.CodeBadRequest {
		t.Fatalf("short head SHA: %v", err)
	}
	if _, err := orchclient.NewAsk(ctx, h.ownerIn("harbor", ""), orchapi.NewAskRequest{To: "machine:mini", Body: "x", Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}}); code(err) != api.CodeBadRequest {
		t.Fatalf("ask to a machine: %v", err)
	}
}

// A phone signed in as the daemon's own login pairs with one tap; nothing
// else does, and a non-phone page offers a QR instead of a code box.
func TestPairPhoneOneTap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Nodes that claim to be phones as the same login but are refused for
	// what they are, plus the honest non-phones and another login's phone.
	const ipSpoofZm, ipSpoofTag, ipSpoofHost, ipSpoofPotluck = "100.100.1.12", "100.100.1.13", "100.100.1.14", "100.100.1.15"
	h.node.set(ipSpoofZm, tailnet.Who{Login: "ivo@example.com", Node: "zm-box", NodeID: "nSZM", NodeKey: "nodekey:szm", OS: "iOS"})
	// A daemon node under the Potluck name (rename step 2) is refused the same.
	h.node.set(ipSpoofPotluck, tailnet.Who{Login: "ivo@example.com", Node: "potluck-box", NodeID: "nSPL", NodeKey: "nodekey:spl", OS: "iOS"})
	h.node.set(ipSpoofTag, tailnet.Who{Node: "ci-phone", NodeID: "nSTAG", NodeKey: "nodekey:stag", OS: "iOS", Tagged: true})
	h.node.set(ipSpoofHost, tailnet.Who{Login: "ivo@example.com", Node: "iphone", NodeID: "nSHOST", NodeKey: "nodekey:shost", OS: "iOS"})
	h.node.set(ipSelf, tailnet.Who{Login: "ivo@example.com", Node: "zm-mini", NodeID: "nSELF", NodeKey: "nodekey:self", OS: "iOS"})
	for _, c := range []struct{ name, ip, why string }{
		{"the Mac (agents run there)", ipMac, ""},
		{"a linux box as the same login", ipFarm, ""},
		{"another zm daemon", ipPeer, ""},
		{"a zm node claiming iOS", ipSpoofZm, "runs a zm daemon"},
		{"a potluck node claiming iOS", ipSpoofPotluck, "runs a zm daemon"},
		{"a tagged node claiming iOS", ipSpoofTag, "tagged nodes"},
		{"this daemon's own node claiming iOS", ipSelf, "daemon's own machine"},
		{"bea's phone (another login)", ipTablet, "signed in to Tailscale as someone other"},
	} {
		dev := h.device(c.ip)
		_, err := orchclient.PairPhone(ctx, dev)
		if code(err) != api.CodeForbidden || (c.why != "" && !strings.Contains(err.Error(), c.why)) {
			t.Fatalf("%s: one-tap pairing %v, want forbidden for %q", c.name, err, c.why)
		}
		if v := must(orchclient.DeviceView(ctx, dev)); v.OneTap {
			t.Fatalf("%s: view offers one tap: %+v", c.name, v)
		}
	}
	if ls := must(orchclient.Approvers(ctx, h.owner)); len(ls) != 0 {
		t.Fatalf("refusals left names behind: %+v", ls)
	}
	resp, err := h.device(ipMac).Stream(ctx, http.MethodGet, "/v0/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}
	if page := readAll(resp); !strings.Contains(page, `<svg class="qr"`) || !strings.Contains(page, "http://zm-mini:7433/v0/approvals") ||
		strings.Contains(page, `id="code"`) {
		t.Fatalf("a non-phone's page should hand the page to a phone by QR, with no code box: %s", page)
	}

	phone := h.device(ipPhone)
	if v := must(orchclient.DeviceView(ctx, phone)); !v.OneTap {
		t.Fatalf("ivo's phone view: %+v", v)
	}
	resp, err = phone.Stream(ctx, http.MethodGet, "/v0/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}
	if page := readAll(resp); !strings.Contains(page, `id="pairphone"`) || strings.Contains(page, `id="code"`) {
		t.Fatalf("phone page: %s", page)
	}
	added := len(h.events(orchapi.EventApproverAdded))
	a := must(orchclient.PairPhone(ctx, phone))
	if a.Name != "ivo-phone" || a.State != orchapi.ApproverActive || a.ConfirmedBy != "bootstrap" || a.Node != "nPHONE" {
		t.Fatalf("first phone: %+v", a)
	}
	if n := len(h.events(orchapi.EventApproverAdded)); n != added+1 {
		t.Fatalf("approver.added events %d, want %d", n, added+1)
	}
	if again := must(orchclient.PairPhone(ctx, phone)); again.Name != a.Name || again.State != orchapi.ApproverActive {
		t.Fatalf("tapping again: %+v", again)
	}

	// A same-login node claiming iOS gets no further than a code pairing would:
	// unconfirmed until the human's phone taps.
	if s := must(orchclient.PairPhone(ctx, h.device(ipSpoofHost))); s.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("spoofed phone after bootstrap: %+v", s)
	}
	must(orchclient.RemoveApprover(ctx, h.owner, "iphone"))

	// A second phone waits for the first one's tap.
	pixel := h.device(ipPhone2)
	b := must(orchclient.PairPhone(ctx, pixel))
	if b.Name != "pixel" || b.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("second phone: %+v", b)
	}
	if c := must(orchclient.ConfirmApprover(ctx, phone, "pixel")); c.State != orchapi.ApproverActive {
		t.Fatalf("confirmed second phone: %+v", c)
	}

	// No approver left after bootstrap stays stuck: one tap never recovers it.
	for _, n := range []string{"ivo-phone", "pixel"} {
		must(orchclient.RemoveApprover(ctx, h.owner, n))
	}
	if a = must(orchclient.PairPhone(ctx, phone)); a.State != orchapi.ApproverUnconfirmed || a.Note == "" {
		t.Fatalf("phone after every approver was removed: %+v", a)
	}

	// A re-keyed phone is told to pair again and leaves no pending name.
	must(orchclient.RemoveApprover(ctx, h.owner, a.Name))
	h.pair("other", ipTablet)
	h.node.set(ipTablet, tailnet.Who{Login: "ivo@example.com", Node: "bea-phone", NodeID: "nTABLET", NodeKey: "nodekey:new", OS: "iOS"})
	if _, err := orchclient.PairPhone(ctx, h.device(ipTablet)); code(err) != api.CodeConflict {
		t.Fatalf("re-keyed phone: %v", err)
	}
	for _, l := range must(orchclient.Approvers(ctx, h.owner)) {
		if l.State == orchapi.ApproverPending {
			t.Fatalf("pending name left behind: %+v", l)
		}
	}
}
