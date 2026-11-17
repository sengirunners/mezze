package team_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// On a member machine, asks live at the project's authority: an agent files
// one, the person's device there answers (the Mezze app's key, through this machine's
// socket), and the answer lands on the item. This machine's socket holds no
// decide right, here or there.
func TestAskFromAMemberMachine(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	sess := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "aaaaaaaa-uuid"}))(t)
	agent := at(r.owner, "harbor", sess.Address)

	card := &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	ask, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card})
	if err != nil {
		t.Fatal(err)
	}
	if ask.Subject.Work != it.ID || ask.AskedBy != sess.Address {
		t.Fatalf("ask: %+v", ask)
	}
	if ask.Note != "" { // the owner can always answer: no approver is no note
		t.Errorf("note: %q", ask.Note)
	}
	// It lives at the authority, and the member reads it through its socket.
	if got := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t); got.ID != ask.ID {
		t.Fatalf("at the authority: %+v", got)
	}
	if got := mustf(orchclient.GetAsk(ctx, rc, ask.ID))(t); got.ID != ask.ID {
		t.Fatalf("from the member: %+v", got)
	}
	if got := mustf(orchclient.Session(ctx, r.owner, sess.Address))(t); got.State != orchapi.StateNeedsHuman {
		t.Errorf("asker state: %s", got.State)
	}
	in := mustf(orchclient.Decisions(ctx, r.owner, "", ""))(t)
	if len(in.Decisions) != 1 || in.Decisions[0].Ask == nil || in.Decisions[0].Ask.ID != ask.ID || in.Decisions[0].ProjectSlug != "harbor" {
		t.Fatalf("inbox through the member's socket: %+v", in)
	}

	// The socket's decide is refused on this machine, and never forwarded.
	for _, c := range []struct {
		name string
		err  error
	}{{"owner", func() error { _, err := orchclient.Decide(ctx, rc, ask.ID, "b"); return err }()},
		{"agent", func() error { _, err := orchclient.Decide(ctx, agent, ask.ID, "b"); return err }()}} {
		if !api.IsCode(c.err, api.CodeForbidden) || !strings.Contains(c.err.Error(), "no socket caller") {
			t.Errorf("%s decide: %v", c.name, c.err)
		}
	}

	// A phone at the authority bootstraps; the Mezze app's key pairs from the member
	// with a code from the authority and waits for the phone.
	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	phone := &core.Remote{Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:phone"}
	code := mustf(orchclient.AddApprover(ctx, a.owner, "ivo"))(t)
	if _, err := svc.Pair(ctx, phone, code.Code); err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	pub := base64.StdEncoding.EncodeToString(raw)
	sign := func(msg []byte) string {
		h := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
	code = mustf(orchclient.AddApprover(ctx, a.owner, "ivo-z3"))(t)
	norm := strings.ReplaceAll(code.Code, "-", "")
	key, err := orchclient.PairKey(ctx, r.owner, "harbor", orchapi.PairKeyRequest{Code: code.Code, PublicKey: pub, Device: "Mezze app", Proof: sign([]byte("zm-pair-key/1\n" + norm + "\n" + pub))})
	if err != nil {
		t.Fatal(err)
	}
	if key.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("key: %+v", key)
	}
	answer := func() (orchapi.Decision, error) {
		b, _ := json.Marshal(orchapi.KeyAnswer{V: orchapi.KeyAnswerVersion, Daemon: approvals.KeyID(mustf(svc.PublicKey())(t)), Project: ask.Project,
			Ask: ask.ID, CardSHA: ask.CardSHA, Decision: "b", Time: time.Now().UTC().Format(time.RFC3339)})
		return orchclient.AnswerSigned(ctx, r.owner, "harbor", ask.ID, orchapi.SignedKeyAnswer{Approver: key.Name, Payload: base64.StdEncoding.EncodeToString(b),
			Sig: sign(append([]byte(orchapi.KeyAnswerVersion+"\n"), b...))})
	}
	if _, err := answer(); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an unconfirmed key's answer through the member: %v", err)
	}
	// The member cannot list approvers (local to the authority), but reads its own key's state there.
	if got := mustf(orchclient.KeyApprover(ctx, r.owner, "harbor", key.Key))(t); got.Name != key.Name || got.State != orchapi.ApproverUnconfirmed {
		t.Fatalf("the key's state through the member: %+v", got)
	}
	if _, err := svc.ConfirmNode(ctx, phone, key.Name, key.Node); err != nil {
		t.Fatal(err)
	}
	if got := mustf(orchclient.KeyApprover(ctx, r.owner, "harbor", key.Key))(t); got.State != orchapi.ApproverActive {
		t.Fatalf("the key after the phone confirmed it: %+v", got)
	}
	if _, err := orchclient.KeyApprover(ctx, r.owner, "harbor", "p256:0000000000000000"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unpaired key: %v", err)
	}
	if _, err := orchclient.KeyApprover(ctx, r.owner, "harbor", "key:nope"); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a malformed key: %v", err)
	}
	d, err := answer()
	if err != nil {
		t.Fatal(err)
	}
	if d.Approver != "ivo-z3" || d.RecordedAt == nil || d.MailError != "" {
		t.Fatalf("decision: %+v", d)
	}
	cs := mustf(orchclient.Comments(ctx, a.owner, it.ID))(t)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "OWNER DECISION (ivo-z3 via \"Mezze app\", ") {
		t.Fatalf("comments at the authority: %+v", cs)
	}
}

// Owner beads with no ask reach the inbox through a member's socket: labelled
// owner:<name>, ask:<who> or human. An epic does not, a bead that already has
// an ask does not, and a question nobody wrote a card for (typed decision)
// waits on an agent instead. An ask finds a bead by its bare
// id, the way agents write it.
func TestInboxShowsOwnerIssues(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	mk := func(title, typ string, labels ...string) orchapi.Issue {
		return mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: title, Area: "solver", IssueType: typ, Labels: labels}))(t)
	}
	owner := mk("Santa Monica: check the standing pattern", "task", "owner:bea")
	chiefs := mk("Rename the lane", "task", "ask:chiefs", "human")
	typed := mk("Which one", "decision")
	human := mk("Needs a person", "task", "human")
	epic := mk("A theme", "epic", "owner:bea")
	plain := mk("Nobody waits", "task", "solver")
	asked := mk("Already asked", "task", "owner:bea")

	card := &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	ask, err := orchclient.NewAsk(ctx, at(r.owner, "harbor/solver", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: asked.ID}, Card: card})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ask.Subject.Work, ":"+asked.ID) {
		t.Fatalf("bare id: %+v", ask.Subject)
	}

	var in orchapi.Inbox
	var rows map[string]orchapi.InboxItem
	deadline := time.Now().Add(10 * time.Second)
	for {
		in = mustf(orchclient.Decisions(ctx, r.owner, "", ""))(t)
		rows = map[string]orchapi.InboxItem{}
		for _, x := range in.Decisions {
			if x.Work != nil {
				_, ref, _ := strings.Cut(x.Work.ID, ":")
				rows[ref] = x
			}
		}
		if len(rows) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []orchapi.Issue{owner, human} {
		if _, ok := rows[want.ID]; !ok {
			t.Errorf("missing %s: %+v", want.Title, rows)
		}
	}
	for _, not := range []orchapi.Issue{typed, epic, plain, asked, chiefs} {
		if _, ok := rows[not.ID]; ok {
			t.Errorf("listed %s", not.Title)
		}
	}
	if got := rows[owner.ID]; got.OwnerName != "bea" || got.ProjectSlug != "harbor" {
		t.Errorf("owner row: %+v", got)
	}

	// Through the member's socket, the authority scopes the list to bea.
	short := mk("Under a short name", "task", "owner:cc")
	owners := mk("For an owner", "task", "ask:owner", "owner-run")
	ids := func(in orchapi.Inbox) map[string]bool {
		out := map[string]bool{}
		for _, x := range in.Decisions {
			if x.Work != nil {
				_, ref, _ := strings.Cut(x.Work.ID, ":")
				out[ref] = true
			}
		}
		return out
	}
	rc := at(r.owner, "harbor", "")
	if got := ids(mustf(orchclient.Decisions(ctx, r.owner, "", ""))(t)); got[short.ID] || got[owners.ID] {
		t.Errorf("before the alias and the grant: %v", got)
	}
	// Regression: the member machine cannot
	// claim a short name for itself; the call reaches the authority, which
	// refuses it, and the authority's owner can still give the name.
	if _, err := orchclient.AddAlias(ctx, rc, orchapi.AliasRequest{Alias: "jj"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("bea's member machine claimed jj: %v", err)
	}
	if al := mustf(orchclient.Aliases(ctx, a.owner, "bea"))(t); len(al.Aliases) != 0 {
		t.Fatalf("aliases at the authority after a refused claim: %+v", al)
	}
	mustf(orchclient.AddAlias(ctx, a.owner, orchapi.AliasRequest{Alias: "jj"}))(t)
	// The authority's owner sets bea's; the member reads it through its socket.
	mustf(orchclient.AddAlias(ctx, a.owner, orchapi.AliasRequest{Alias: "cc", Identity: "bea"}))(t)
	if al := mustf(orchclient.Aliases(ctx, rc, ""))(t); al.Identity != "bea" || len(al.Aliases) != 1 || al.Aliases[0] != "cc" {
		t.Fatalf("bea reads its alias through the member: %+v", al)
	}
	// Owning harbor by its own grant counts, though the replica's token is a member's.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "owner", Project: "harbor"}))(t)
	var got map[string]bool
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got = ids(mustf(orchclient.Decisions(ctx, r.owner, "", ""))(t)); (got[short.ID] && got[owners.ID]) || time.Now().After(deadline) {
			break
		}
	}
	if !got[short.ID] || !got[owners.ID] || got[chiefs.ID] {
		t.Errorf("after the alias and the grant: %v", got)
	}
	if got := ids(mustf(orchclient.DecisionsFor(ctx, r.owner, "", "", "everyone"))(t)); !got[chiefs.ID] {
		t.Errorf("for=everyone: %v", got)
	}
}

// Finding 5: the key flow on a member machine, as API.md says it, using only
// what the member's socket answers.
func TestKeyFlowOnAMemberUsesOnlyItsSocket(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	theirs := approvals.KeyID(mustf(svc.PublicKey())(t))
	if got := mustf(orchclient.ReceiptKey(ctx, rc))(t); got.KeyID != theirs {
		t.Errorf("the receipt key named by project: %s, the authority's is %s", got.KeyID, theirs)
	}
	it := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	card := &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	ask := mustf(orchclient.NewAsk(ctx, at(r.owner, "harbor/solver", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card}))(t)
	if ask.ReceiptKey != theirs {
		t.Errorf("the ask names key %q, the authority's is %s", ask.ReceiptKey, theirs)
	}
	for _, surface := range []orchapi.Ask{mustf(orchclient.GetAsk(ctx, rc, ask.ID))(t), mustf(orchclient.Asks(ctx, rc, "harbor", true))(t)[0], *mustf(orchclient.Decisions(ctx, rc, "harbor", "open"))(t).Decisions[0].Ask} {
		if surface.ReceiptKey != theirs {
			t.Errorf("ask surface lacks authority key: %+v", surface)
		}
	}

	phone := &core.Remote{Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:phone"}
	if _, err := svc.Pair(ctx, phone, mustf(orchclient.AddApprover(ctx, a.owner, "ivo"))(t).Code); err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	pub := base64.StdEncoding.EncodeToString(raw)
	sign := func(msg []byte) string {
		h := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
	code := mustf(orchclient.AddApprover(ctx, a.owner, "ivo-z3"))(t)
	norm := strings.ReplaceAll(code.Code, "-", "")
	key := mustf(orchclient.PairKey(ctx, r.owner, "harbor", orchapi.PairKeyRequest{Code: code.Code, PublicKey: pub, Proof: sign([]byte("zm-pair-key/1\n" + norm + "\n" + pub))}))(t)
	if _, err := svc.ConfirmNode(ctx, phone, key.Name, key.Node); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(orchapi.KeyAnswer{V: orchapi.KeyAnswerVersion, Daemon: ask.ReceiptKey, Project: ask.Project, Ask: ask.ID, CardSHA: ask.CardSHA,
		Decision: "b", Time: time.Now().UTC().Format(time.RFC3339)})
	d, err := orchclient.AnswerSigned(ctx, r.owner, "harbor", ask.ID, orchapi.SignedKeyAnswer{Approver: key.Name, Payload: base64.StdEncoding.EncodeToString(b),
		Sig: sign(append([]byte(orchapi.KeyAnswerVersion+"\n"), b...))})
	if err != nil {
		t.Fatalf("signing the key the ask names: %v", err)
	}
	// The receipt names its project, so the member's socket checks it at the
	// authority that signed it, named or not.
	rb, _ := json.Marshal(d.Receipt)
	exp := orchapi.ReceiptExpect{Work: ask.Subject.Work, Ask: ask.ID, Decision: "b"}
	if chk := mustf(orchclient.VerifyReceipt(ctx, r.owner, rb, exp))(t); !chk.Valid {
		t.Errorf("verify on the member: %s", chk.Reason)
	}
	var direct orchapi.ReceiptCheck
	if err := r.owner.Do(ctx, http.MethodGet, client.Query("/v0/receipts/verify", "receipt", base64.RawURLEncoding.EncodeToString(rb), "ask", ask.ID, "work", ask.Subject.Work), nil, &direct); err != nil || !direct.Valid {
		t.Fatalf("direct socket verification: %+v %v", direct, err)
	}
	bad := d.Receipt
	bad.Decision = "a"
	rb, _ = json.Marshal(bad)
	exp.Decision = "a"
	if chk := mustf(orchclient.VerifyReceipt(ctx, r.owner, rb, exp))(t); chk.Valid {
		t.Error("a tampered receipt verifies through the member")
	}
}

// Finding 13: no spelling of decide leaves the member machine.
func TestDecideNeverLeavesTheMachine(t *testing.T) {
	for _, p := range []string{"/v0/asks/ID/decide", "/v0/asks/ID/decide/", "/v0/asks/ID/decide/.", "/v0/asks/ID/./decide",
		"/v0/asks/ID//decide", "/v0/asks/ID/x/../decide", "/v0/asks/ID/DECIDE"} {
		if k := team.ForwardedKind(http.MethodPost, p, "bea-mac"); k != "" {
			t.Errorf("%s is forwarded as %q", p, k)
		}
	}
	for _, p := range []string{"/v0/asks/ID/answer", "/v0/asks/ID/withdraw"} {
		if k := team.ForwardedKind(http.MethodPost, p, "bea-mac"); k == "" {
			t.Errorf("%s is not forwarded", p)
		}
	}
}

// Finding 7: an ask by a moved bead's old id lives where the bead is, and its
// answer lands.
func TestAskByAMovedItemsOldID(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	mustf(orchclient.SetupIssues(ctx, ao, "harbor"))(t)
	mustf(orchclient.SetupIssues(ctx, az, "zm"))(t)
	for _, c := range []*client.Client{ao, az} {
		if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("tracker: %+v %v", res, err)
		}
	}
	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	phone := &core.Remote{Node: "ivo-phone", NodeID: "nPHONE", NodeKey: "nodekey:phone"}
	if _, err := svc.Pair(ctx, phone, mustf(orchclient.AddApprover(ctx, a.owner, "ivo"))(t).Code); err != nil {
		t.Fatal(err)
	}
	card := func() *orchapi.AskCard {
		return &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
			Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
			Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	}
	it := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "moved before the ask", Area: "solver"}))(t)
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	ask := mustf(orchclient.NewAsk(ctx, ao, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card(), Topic: "after-move"}))(t)
	zm := mustf(a.owner.GetProject(ctx, "zm"))(t)
	if ask.Project != zm.ID {
		t.Errorf("the ask lives in %s, the item in %s", ask.Project, zm.ID)
	}
	d, err := svc.Decide(ctx, phone, ask.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if d.WriteError != "" || d.RecordedAt == nil {
		t.Fatalf("the answer did not land: %+v", d)
	}
	if cs := mustf(orchclient.IssueComments(ctx, az, res.Items[0].NewID))(t); len(cs) != 1 || !strings.HasPrefix(cs[0].Text, "OWNER DECISION") {
		t.Fatalf("comments on the moved item: %+v", cs)
	}
}

// An ask held for an outside party reaches a member's copy saying so: the
// copy has no asks table, so its mail.sent event carries held_for, and the
// replica's listings and counts read it there.
func TestHeldAskMailReachesTheCopyHeld(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	agent := at(a.owner, "harbor", register(t, a, "harbor", "mini-asker").Address)
	card := &orchapi.AskCard{Context: "Santa Monica sets its own holiday shifts, and they have not said who takes Christmas.",
		Question: "Who takes Christmas at Santa Monica?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}
	held := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card, Topic: "holiday-shifts",
		For: &orchapi.AskFor{Group: "Santa Monica", Contact: "Dr. R"}}))(t)
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	plain := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card}))(t)

	rc := at(r.owner, "harbor", "")
	eventually(t, 2*time.Second, func() error {
		for id, want := range map[string]string{held.ID: "Santa Monica", plain.ID: ""} {
			m, err := orchclient.GetMail(ctx, rc, id)
			if err != nil {
				return err
			}
			if m.HeldFor != want {
				return fmt.Errorf("mail %s on the copy: held_for %q, want %q", id, m.HeldFor, want)
			}
		}
		return nil
	})
}

// An ask from a member machine may go to that machine's own human (the
// identity its link answers as), not the authority's OS user. The
// refusal names the asker's machine, and the decision answers the mail as
// the human it was put to, so the ask's mail is taken.
func TestAskToTheMemberMachinesHuman(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	sess := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bbbbbbbb-uuid"}))(t)
	agent := at(r.owner, "harbor", sess.Address)
	card := &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}

	// A card may name any person who owns or administers the
	// project, the authority's user among them (its mail goes to owners).
	// Anyone else is refused.
	_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{To: "human:stranger", Subject: orchapi.AskSubject{Work: it.ID}, Card: card})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "not a person who owns or administers") {
		t.Fatalf("an ask from a member to a person outside the project: %v", err)
	}
	ask, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{To: "human:bea", Subject: orchapi.AskSubject{Work: it.ID}, Card: card})
	if err != nil {
		t.Fatalf("an ask to the member machine's human: %v", err)
	}
	if ask.To != "human:bea" || ask.AskedBy != sess.Address {
		t.Fatalf("ask: %+v", ask)
	}
	// A second agent on the machine joining it goes to the same human, not owners.
	sess2 := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "cccccccc-uuid"}))(t)
	j := mustf(orchclient.NewAsk(ctx, at(r.owner, "harbor", sess2.Address), orchapi.NewAskRequest{SameAs: ask.ID}))(t)
	if j.To != "human:bea" || j.Owner != ask.Owner || j.SameAs != ask.ID {
		t.Fatalf("an ask joining it: to %q owner %q (first's %q) same_as %q", j.To, j.Owner, ask.Owner, j.SameAs)
	}
	d, err := orchclient.Choose(ctx, a.owner, "harbor", ask.ID, orchapi.DecideRequest{Decision: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if d.MailError != "" || d.RecordedAt == nil {
		t.Fatalf("decision: %+v", d)
	}
	if m := mustf(orchclient.GetMail(ctx, a.owner, ask.ID))(t); m.AckedBy != "human:bea" {
		t.Fatalf("the ask's mail at the authority: %+v", m)
	}
	if m := mustf(orchclient.GetMail(ctx, a.owner, j.ID))(t); m.AckedBy != "human:bea" {
		t.Fatalf("the joined ask's mail at the authority: %+v", m)
	}
}
