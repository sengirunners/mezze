package team_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

type signer struct {
	k   *ecdsa.PrivateKey
	pub string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return signer{k, base64.StdEncoding.EncodeToString(raw)}
}

func (s signer) sign(t *testing.T, msg string) string {
	t.Helper()
	h := sha256.Sum256([]byte(msg))
	sig, err := ecdsa.SignASN1(rand.Reader, s.k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func newNonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// A person on a member machine binds the Mezze app's key with one GitHub sign-in
// through the machine's socket, then answers in a project they administer.
// The key answers nothing in a project they do not, nothing from another
// node, and a member too old to check its caller is refused by name.
func TestKeyLoginFromAMember(t *testing.T) {
	ctx := context.Background()
	gh := githubfake.New(t)
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "alder", tailnettest.Identity{NodeID: "nALDER", NodeKey: "nodekey:alder", Login: "robin@example.com", Host: "alder"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(cfg *config.Config) {
			cfg.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
		})
	for _, p := range []string{"orchard", "quarry"} {
		mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: p}))(t)
		mustf(a.owner.ProjectTeam(ctx, p))(t)
	}
	r, j := replica(t, tn, a, "birch", "nBIRCH", "orchard", "sage", "member")
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "sage", Project: "orchard", Role: "owner"}))(t)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "quarry", As: "sage", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	u, _ := gh.AddUser("sage-gh", "User")
	gh.Approver(u.ID, 0)

	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	rk := approvals.KeyID(mustf(svc.PublicKey())(t))
	k := newSigner(t)
	start := func(n string) api.GitHubLoginRequest {
		at := time.Now().UTC().Format(time.RFC3339)
		return api.GitHubLoginRequest{Purpose: "key", Project: "orchard", KeyAttach: api.KeyAttach{Key: k.pub, Device: "Mezze app", Nonce: n, Time: at,
			Proof: k.sign(t, strings.Join([]string{orchapi.BindKeyVersion, rk, "orchard", k.pub, n, at}, "\n"))}}
	}
	member := at(r.owner, "orchard", "")
	st := mustf(member.StartGitHubLogin(ctx, start(newNonce())))(t)
	var out api.GitHubLoginOutcome
	for wctx, cancel := context.WithTimeout(ctx, 30*time.Second); ; {
		o, err := member.PollGitHubLogin(wctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 2000})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if !o.Pending {
			cancel()
			out = o
			break
		}
	}
	b := out.Key
	if b == nil || b.State != orchapi.ApproverActive || b.Machine != "birch" || b.Login != "sage-gh" || !strings.HasPrefix(b.Approver, "key-birch-") {
		t.Fatalf("binding: %+v", out)
	}

	var newAskTo func(project, to string) orchapi.Ask
	newAsk := func(project string) orchapi.Ask {
		to := ""
		if project == "orchard" {
			to = "human:sage"
		}
		return newAskTo(project, to)
	}
	newAskTo = func(project, to string) orchapi.Ask {
		return mustf(orchclient.NewAsk(ctx, at(a.owner, project, ""), orchapi.NewAskRequest{Card: &orchapi.AskCard{
			Context: "The orchard's gate code changes each season and the old one still works.", Question: "Change it now?",
			Choices: []orchapi.AskChoice{{Label: "Change it"}, {Label: "Wait"}}, Recommend: "a", Why: "The old code is widely known.",
			Searched: "past seasons: always changed in spring"}, To: to, Topic: "gate-" + newNonce()}))(t)
	}
	signed := func(ask orchapi.Ask) orchapi.SignedKeyAnswer {
		p, _ := json.Marshal(orchapi.KeyAnswer{V: orchapi.KeyAnswerVersion, Daemon: rk, Project: ask.Project, Ask: ask.ID, CardSHA: ask.CardSHA,
			Decision: "a", Time: time.Now().UTC().Format(time.RFC3339)})
		return orchapi.SignedKeyAnswer{Approver: b.Approver, Payload: base64.StdEncoding.EncodeToString(p), Sig: k.sign(t, orchapi.KeyAnswerVersion+"\n"+string(p))}
	}

	// The two-project rule: no grant in quarry, no answer there.
	q := newAsk("quarry")
	if _, err := orchclient.AnswerSigned(ctx, r.owner, "quarry", q.ID, signed(q)); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an answer in a project the person does not administer: %v", err)
	}
	// From another node (the authority's own socket), even marked: refused.
	o := newAsk("orchard")
	var d orchapi.Decision
	if err := a.owner.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+o.ID+"/answer", orchapi.OriginChecked, "1"), signed(o), &d); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an answer from another node: %v", err)
	}
	// Straight to the authority with the member's token, unmarked: the
	// member is named as too old.
	direct := func(path string, body any) (int, api.Error) {
		bb, _ := json.Marshal(body)
		hr, _ := http.NewRequest(http.MethodPost, "http://potluck-alder:7433"+path, bytes.NewReader(bb))
		hr.Header.Set(api.HeaderAuthorization, "Bearer "+readToken(t, j.Remote.TokenFile))
		hr.Header.Set(api.HeaderProtocol, api.Protocol)
		hr.Header.Set("Content-Type", "application/json")
		resp, err := tailnetClientAs(t, tn, "birch").Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct {
			Error api.Error `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Error
	}
	if code, e := direct("/v0/asks/"+o.ID+"/answer?project=orchard", signed(o)); code != http.StatusForbidden || !strings.Contains(string(e.Data), orchapi.ReasonMemberTooOld) || !strings.Contains(e.Message, "birch") {
		t.Fatalf("an unmarked answer from the member's token: %d %+v", code, e)
	}
	if code, e := direct("/v0/login/github/start?project=orchard", start(newNonce())); code != http.StatusForbidden || !strings.Contains(string(e.Data), orchapi.ReasonMemberTooOld) {
		t.Fatalf("an unmarked key sign-in from the member's token: %d %+v", code, e)
	}
	if code, e := directDelete(t, tn, j.Remote.TokenFile, b.Fingerprint); code != http.StatusForbidden || !strings.Contains(string(e.Data), orchapi.ReasonMemberTooOld) {
		t.Fatalf("an unmarked key removal from the member's token: %d %+v", code, e)
	}
	// A missing ask looks like any refusal (no oracle).
	missing := o
	missing.ID = "01M0000000000000000000NONE"
	if _, err := orchclient.AnswerSigned(ctx, r.owner, "orchard", missing.ID, signed(missing)); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an answer to a missing ask: %v", err)
	}
	// Through the member's socket, in a project they administer: answered,
	// recorded as their account from their machine.
	d = mustf(orchclient.AnswerSigned(ctx, r.owner, "orchard", o.ID, signed(o)))(t)
	if d.Surface != orchapi.SurfaceKey || d.Machine != "birch" || d.Login != "sage-gh" || !strings.HasPrefix(d.Account, "github:") {
		t.Fatalf("decision: %+v", d)
	}
	// Demoted: the next answer is refused.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "sage", Project: "orchard", Role: "member"}))(t)
	o2 := newAskTo("orchard", "")
	if _, err := orchclient.AnswerSigned(ctx, r.owner, "orchard", o2.ID, signed(o2)); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "not signed by a paired approver's key") {
		t.Fatalf("an answer after the grant was dropped: %v", err)
	}
}

// directDelete sends DELETE /v0/approvals/key for fp straight to the
// authority alder with the member's token, unmarked.
func directDelete(t *testing.T, tn *tailnettest.Net, tokenFile, fp string) (int, api.Error) {
	t.Helper()
	bb, _ := json.Marshal(orchapi.UnbindKeyRequest{Key: fp, Nonce: newNonce(), Time: time.Now().UTC().Format(time.RFC3339), Proof: "x"})
	hr, _ := http.NewRequest(http.MethodDelete, "http://potluck-alder:7433/v0/approvals/key?project=orchard", bytes.NewReader(bb))
	hr.Header.Set(api.HeaderAuthorization, "Bearer "+readToken(t, tokenFile))
	hr.Header.Set(api.HeaderProtocol, api.Protocol)
	hr.Header.Set("Content-Type", "application/json")
	resp, err := tailnetClientAs(t, tn, "birch").Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct {
		Error api.Error `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Error
}
