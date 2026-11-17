package approvals_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
)

// keyHarness is an authority (machine alder, owner robin) that runs GitHub
// sign-ins against a fake GitHub, with project orchard.
type keyHarness struct {
	*harness
	gh   *githubfake.GitHub
	user int64 // robin's GitHub account
}

func newKeyHarness(t *testing.T) *keyHarness {
	t.Helper()
	gh := githubfake.New(t)
	h := newHarnessWith(t, func(cfg *config.Config) {
		cfg.Human, cfg.Machine = "robin", "alder"
		cfg.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
	})
	must(h.owner.CreateProject(context.Background(), api.NewProjectRequest{Slug: "orchard"}))
	u, _ := gh.AddUser("robin-gh", "User")
	gh.Approver(u.ID, 0)
	return &keyHarness{harness: h, gh: gh, user: u.ID}
}

func nonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// keyLogin is a key login's start request for k, in project, signed now.
func (h *keyHarness) keyLogin(t *testing.T, k keyDevice, project, n string) api.GitHubLoginRequest {
	t.Helper()
	at := h.clock.now().Format(time.RFC3339)
	rk := approvals.KeyID(must(h.svc.PublicKey()))
	proof := k.sign(t, []byte(strings.Join([]string{orchapi.BindKeyVersion, rk, project, k.pub, n, at}, "\n")))
	return api.GitHubLoginRequest{Purpose: "key", Project: project,
		KeyAttach: api.KeyAttach{Key: k.pub, Device: "Mezze app", Install: "install-1", Nonce: n, Time: at, Proof: proof}}
}

// bind signs in on c with k for project and waits for the outcome.
func (h *keyHarness) bind(t *testing.T, c *client.Client, k keyDevice, project string) (api.GitHubLoginOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.StartGitHubLogin(ctx, h.keyLogin(t, k, project, nonce()))
	if err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	for {
		out, err := c.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 2000})
		if err != nil || !out.Pending {
			return out, err
		}
	}
}

// keyAnswer signs decision on a with k as approver and sends it on c.
func (h *keyHarness) keyAnswer(t *testing.T, c *client.Client, k keyDevice, approver string, a orchapi.Ask, decision string) (orchapi.Decision, error) {
	t.Helper()
	return orchclient.AnswerSigned(context.Background(), c, "", a.ID, k.answer(t, approver, h.goodAnswer(a, decision)))
}

func (h *keyHarness) askIn(t *testing.T, project string) orchapi.Ask {
	t.Helper()
	return must(orchclient.NewAsk(context.Background(), h.ownerIn(project, ""), orchapi.NewAskRequest{Card: card(), Topic: "t-" + nonce()}))
}

// A key bound by one GitHub sign-in answers at once, with no confirm step,
// and the decision records the account, the machine and the surface.
func TestKeyLoginBindsAndAnswers(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	out, err := h.bind(t, h.owner, k, "orchard")
	if err != nil {
		t.Fatal(err)
	}
	b := out.Key
	acc := "github:" + jsonNum(h.user)
	if b == nil || b.State != orchapi.ApproverActive || b.Account != acc || b.Login != "robin-gh" || b.Machine != "alder" ||
		b.ReceiptKey != approvals.KeyID(must(h.svc.PublicKey())) || !strings.HasPrefix(b.Approver, "key-alder-") {
		t.Fatalf("binding: %+v", out)
	}
	st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", b.Fingerprint))
	if st.Via != orchapi.ViaLogin || st.Account != acc || st.Machine != "alder" || st.Login != "robin-gh" || st.Bound == nil || st.Identity != "" && st.Identity != "robin" {
		t.Fatalf("key state: %+v", st)
	}
	a := h.askIn(t, "orchard")
	d, err := h.keyAnswer(t, h.owner, k, b.Approver, a, "a")
	if err != nil {
		t.Fatal(err)
	}
	if d.Surface != orchapi.SurfaceKey || d.Account != acc || d.Login != "robin-gh" || d.Machine != "alder" || d.Method != orchapi.MethodApprover {
		t.Fatalf("decision: %+v", d)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Decision == nil || got.Decision.Surface != orchapi.SurfaceKey || got.Decision.Account != acc || got.Decision.Machine != "alder" {
		t.Fatalf("decision read back: %+v", got.Decision)
	}
	// Binding the same key again refreshes it in place, still one row.
	again, err := h.bind(t, h.owner, k, "orchard")
	if err != nil || again.Key == nil || again.Key.Approver != b.Approver {
		t.Fatalf("bound again: %+v %v", again, err)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A start is refused on a bad proof, a reused nonce or a project the
// person does not answer, and nothing is reserved.
func TestKeyLoginStartRefusals(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	n := nonce()
	req := h.keyLogin(t, k, "orchard", n)
	if _, err := h.owner.StartGitHubLogin(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := h.owner.StartGitHubLogin(ctx, req); code(err) != api.CodeForbidden {
		t.Errorf("a reused nonce: %v", err)
	}
	bad := h.keyLogin(t, k, "orchard", nonce())
	bad.Proof = k.sign(t, []byte("something else"))
	if _, err := h.owner.StartGitHubLogin(ctx, bad); code(err) != api.CodeForbidden {
		t.Errorf("a bad proof: %v", err)
	}
	stale := h.keyLogin(t, k, "orchard", nonce())
	h.clock.add(10 * time.Minute)
	if _, err := h.owner.StartGitHubLogin(ctx, stale); code(err) != api.CodeForbidden {
		t.Errorf("a stale proof: %v", err)
	}
}

// One active bound key per person per node: a second sign-in ends the
// first, which says by which key; the first is refused from then on.
func TestSecondKeyLoginReplacesTheFirst(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k1, k2 := newKeyDevice(t), newKeyDevice(t)
	b1 := must(h.bind(t, h.owner, k1, "orchard")).Key
	b2 := must(h.bind(t, h.owner, k2, "orchard")).Key
	st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", b1.Fingerprint))
	if st.State != orchapi.ApproverRemoved || st.EndedWhy != orchapi.EndedReplaced || st.ReplacedBy != b2.Fingerprint || st.Ended == nil {
		t.Fatalf("first key after the second sign-in: %+v", st)
	}
	a := h.askIn(t, "orchard")
	if _, err := h.keyAnswer(t, h.owner, k1, b1.Approver, a, "a"); code(err) != api.CodeForbidden {
		t.Fatalf("the replaced key answered: %v", err)
	}
	if _, err := h.keyAnswer(t, h.owner, k2, b2.Approver, a, "a"); err != nil {
		t.Fatal(err)
	}
	// Signing in again with the first key brings it back and ends the second.
	if b := must(h.bind(t, h.owner, k1, "orchard")).Key; b.Approver != b1.Approver {
		t.Fatalf("rebound: %+v", b)
	}
	if st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", b2.Fingerprint)); st.State != orchapi.ApproverRemoved || st.ReplacedBy != b1.Fingerprint {
		t.Fatalf("second key after the first came back: %+v", st)
	}
}

// A bound key answers only from its node, while its person stays linked
// to the account it was bound as.
func TestBoundKeyChecksEveryAnswer(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	a := h.askIn(t, "orchard")
	// A minted token is never the key's node, whatever its scope.
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "robin", Project: "orchard", Role: "owner"}))
	viaToken := client.WithHandler(h.dm.Handler)
	viaToken.Token, viaToken.Project, viaToken.ProjectFrom = tok.Secret, "orchard", "env"
	// Marked origin_checked, so rule d passes and the node rule (c) is what
	// refuses it.
	var d orchapi.Decision
	err := viaToken.Do(ctx, "POST", client.Query("/v0/asks/"+a.ID+"/answer", orchapi.OriginChecked, "1"),
		k.answer(t, b.Approver, h.goodAnswer(a, "a")), &d)
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "not signed by a paired approver") {
		t.Fatalf("through a minted token: %v", err)
	}
	// Unlinked from GitHub: the next answer is refused.
	if _, err := h.owner.UnlinkGitHubIdentity(ctx, "robin", false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.keyAnswer(t, h.owner, k, b.Approver, a, "a"); code(err) != api.CodeForbidden {
		t.Fatalf("after unlink: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
		t.Fatalf("decided: %+v", got.Decision)
	}
}

// A key signs itself out once per nonce; it answers nothing after.
func TestKeySignsOut(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	rk := approvals.KeyID(must(h.svc.PublicKey()))
	n, at := nonce(), h.clock.now().Format(time.RFC3339)
	req := orchapi.UnbindKeyRequest{Key: b.Fingerprint, Nonce: n, Time: at,
		Proof: k.sign(t, []byte(strings.Join([]string{orchapi.UnbindKeyVersion, rk, b.Fingerprint, n, at}, "\n")))}
	bad := req
	bad.Proof = k.sign(t, []byte("not it"))
	if _, err := orchclient.UnbindKey(ctx, h.owner, "orchard", bad); code(err) != api.CodeForbidden {
		t.Fatalf("a bad proof: %v", err)
	}
	got := must(orchclient.UnbindKey(ctx, h.owner, "orchard", req))
	if got.State != orchapi.ApproverRemoved || got.EndedWhy != orchapi.EndedSignedOut {
		t.Fatalf("signed out: %+v", got)
	}
	if _, err := orchclient.UnbindKey(ctx, h.owner, "orchard", req); code(err) != api.CodeForbidden {
		t.Fatalf("a replay: %v", err)
	}
	if _, err := h.keyAnswer(t, h.owner, k, b.Approver, h.askIn(t, "orchard"), "a"); code(err) != api.CodeForbidden {
		t.Fatalf("a signed-out key answered: %v", err)
	}
}

// Keys paired the old way: checked against the project now, ended at day
// 7 after sign-in binding first ran (or by their person's sign-in), and an
// old pairing never confirmed is dropped after a day.
func TestLegacyKeys(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	h.pair("robin", ipPhone)
	old := newKeyDevice(t)
	p := must(orchclient.PairKey(ctx, h.owner, "", old.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3")).Code)))
	phone := h.device(ipPhone)
	confirmKey(t, phone, p)
	stale := newKeyDevice(t)
	sp := must(orchclient.PairKey(ctx, h.owner, "", stale.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3b")).Code)))
	if err := h.svc.SetKeyLoginMark(ctx, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	a := h.askIn(t, "orchard")
	if _, err := h.keyAnswer(t, h.owner, old, p.Name, a, "a"); err != nil {
		t.Fatalf("a legacy key within the grace period: %v", err)
	}
	h.clock.add(25 * time.Hour)
	h.svc.SweepKeys(ctx)
	if _, err := orchclient.KeyApprover(ctx, h.owner, "orchard", sp.Key); code(err) != api.CodeNotFound {
		t.Fatalf("an unconfirmed pairing after a day: %v", err)
	}
	h.clock.add(6 * 24 * time.Hour)
	h.svc.SweepKeys(ctx)
	st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", p.Key))
	if st.State != orchapi.ApproverRemoved || st.EndedWhy != orchapi.EndedLegacy {
		t.Fatalf("a legacy key at day 7: %+v", st)
	}
	if _, err := h.keyAnswer(t, h.owner, old, p.Name, h.askIn(t, "orchard"), "a"); code(err) != api.CodeForbidden {
		t.Fatalf("an ended legacy key answered: %v", err)
	}
}

// A sign-in with the key paired the old way upgrades it in place, and ends
// the person's other old keys.
func TestKeyLoginUpgradesALegacyKey(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	h.pair("robin", ipPhone)
	phone := h.device(ipPhone)
	k, other := newKeyDevice(t), newKeyDevice(t)
	p := must(orchclient.PairKey(ctx, h.owner, "", k.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3")).Code)))
	confirmKey(t, phone, p)
	q := must(orchclient.PairKey(ctx, h.owner, "", other.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3b")).Code)))
	confirmKey(t, phone, q)
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	if b.Approver != p.Name {
		t.Fatalf("upgraded under another name: %+v (was %s)", b, p.Name)
	}
	if st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", p.Key)); st.Via != orchapi.ViaLogin || st.State != orchapi.ApproverActive {
		t.Fatalf("upgraded key: %+v", st)
	}
	if st := must(orchclient.KeyApprover(ctx, h.owner, "orchard", q.Key)); st.State != orchapi.ApproverRemoved {
		t.Fatalf("the other legacy key: %+v", st)
	}
}

// confirmKey confirms key approver p from an active approver's device.
func confirmKey(t *testing.T, phone *client.Client, p orchapi.Approver) {
	t.Helper()
	var out orchapi.Approver
	if err := phone.Do(context.Background(), "POST", "/v0/approvers/"+p.Name+"/confirm", orchapi.ConfirmRequest{Node: p.Node}, &out); err != nil {
		t.Fatal(err)
	}
}

// Keys paired the old way stop at day 7 when used, before any sweep: the
// answer, a new pairing and a confirm are refused. A confirm also waits at
// most a day.
func TestLegacyKeysEndWithoutTheSweep(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	h.pair("robin", ipPhone)
	phone := h.device(ipPhone)
	old := newKeyDevice(t)
	p := must(orchclient.PairKey(ctx, h.owner, "", old.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3")).Code)))
	confirmKey(t, phone, p)
	late := newKeyDevice(t)
	lp := must(orchclient.PairKey(ctx, h.owner, "", late.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3b")).Code)))
	if err := h.svc.SetKeyLoginMark(ctx, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	h.clock.add(25 * time.Hour)
	var out orchapi.Approver
	if err := phone.Do(ctx, "POST", "/v0/approvers/"+lp.Name+"/confirm", orchapi.ConfirmRequest{Node: lp.Node}, &out); code(err) != api.CodeConflict {
		t.Fatalf("a confirm after a day: %v", err)
	}
	pending := must(orchclient.AddApprover(ctx, h.owner, "robin-z3c")).Code
	fresh := newKeyDevice(t)
	fp := must(orchclient.PairKey(ctx, h.owner, "", fresh.pairReq(t, must(orchclient.AddApprover(ctx, h.owner, "robin-z3d")).Code)))
	h.clock.add(6 * 24 * time.Hour) // day 7; no sweep runs
	if err := phone.Do(ctx, "POST", "/v0/approvers/"+fp.Name+"/confirm", orchapi.ConfirmRequest{Node: fp.Node}, &out); code(err) != api.CodeForbidden {
		t.Fatalf("a key confirm after day 7: %v", err)
	}
	if _, err := h.keyAnswer(t, h.owner, old, p.Name, h.askIn(t, "orchard"), "a"); code(err) != api.CodeForbidden {
		t.Fatalf("a legacy key answered after day 7: %v", err)
	}
	if _, err := orchclient.PairKey(ctx, h.owner, "", newKeyDevice(t).pairReq(t, pending)); code(err) != api.CodeForbidden {
		t.Fatalf("a pairing after day 7: %v", err)
	}
}

// A key sign-out refused for its node does not spend its nonce; the same
// statement from the bound node then signs out.
func TestKeySignOutChecksTheNodeFirst(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	rk := approvals.KeyID(must(h.svc.PublicKey()))
	n, at := nonce(), h.clock.now().Format(time.RFC3339)
	req := orchapi.UnbindKeyRequest{Key: b.Fingerprint, Nonce: n, Time: at,
		Proof: k.sign(t, []byte(strings.Join([]string{orchapi.UnbindKeyVersion, rk, b.Fingerprint, n, at}, "\n")))}
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "robin", Project: "orchard", Role: "owner"}))
	viaToken := client.WithHandler(h.dm.Handler)
	viaToken.Token, viaToken.Project, viaToken.ProjectFrom = tok.Secret, "orchard", "env"
	var out orchapi.Approver
	if err := viaToken.Do(ctx, "DELETE", client.Query("/v0/approvals/key", orchapi.OriginChecked, "1"), req, &out); code(err) != api.CodeForbidden {
		t.Fatalf("a sign-out through a minted token: %v", err)
	}
	if got := must(orchclient.UnbindKey(ctx, h.owner, "orchard", req)); got.State != orchapi.ApproverRemoved {
		t.Fatalf("signed out from the bound node: %+v", got)
	}
}

// A bind never takes a row by name: a name someone took first gets a
// suffix, and the key still binds.
func TestKeyBindSkipsATakenName(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	raw, _ := base64.StdEncoding.DecodeString(k.pub)
	sum := sha256.Sum256(raw)
	taken := "key-alder-" + hex.EncodeToString(sum[:8])
	must(orchclient.AddApprover(ctx, h.owner, taken))
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	if b.Approver != taken+"-2" || b.State != orchapi.ApproverActive {
		t.Fatalf("bound under: %+v", b)
	}
	if _, err := h.keyAnswer(t, h.owner, k, b.Approver, h.askIn(t, "orchard"), "a"); err != nil {
		t.Fatal(err)
	}
}

// A sign-in-bound key answers a risky action's choice only with its word
// in the signed answer (the Mezze app's type-a-word).
func TestKeyAnswerTakesTheRiskWord(t *testing.T) {
	h := newKeyHarness(t)
	ctx := context.Background()
	k := newKeyDevice(t)
	b := must(h.bind(t, h.owner, k, "orchard")).Key
	a := must(orchclient.NewAsk(ctx, h.ownerIn("orchard", ""), orchapi.NewAskRequest{Card: handCard("irreversible"), Topic: "t-" + nonce()}))
	if a.Card.Action.ConfirmWord != "orchard" {
		t.Fatalf("word: %+v", a.Card.Action)
	}
	if _, err := h.keyAnswer(t, h.owner, k, b.Approver, a, "a"); refusalReason(err) != orchapi.ReasonConfirmWord {
		t.Fatalf("without the word: %v", err)
	}
	ans := h.goodAnswer(a, "a")
	ans.Confirm = "orchard"
	if _, err := orchclient.AnswerSigned(ctx, h.owner, "", a.ID, k.answer(t, b.Approver, ans)); err != nil {
		t.Fatal(err)
	}
}
