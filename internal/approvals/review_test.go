package approvals_test

// Regression tests for the approvals review findings: one-time bootstrap,
// ask addressees the decision can answer, re-keyed devices, zm nodes by
// hostname, identities that look like approvers, and a failed ask insert.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/tailnet"
)

// Bootstrap happens once per daemon: removing every approver and pairing
// again never yields an active approver without a confirmation. Only the
// human deleting the marker file by hand re-opens it.
func TestBootstrapIsOneTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if a := h.pair("ivo", ipPhone); a.State != orchapi.ApproverActive || a.ConfirmedBy != "bootstrap" {
		t.Fatalf("first approver: %+v", a)
	}
	marker := filepath.Join(h.dm.Deps.Config.Home, approvals.BootstrapFile)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no bootstrap marker after the first approver: %v", err)
	}
	// Every local caller (any agent on the Mac) can rm and add.
	must(orchclient.RemoveApprover(ctx, h.owner, "ivo"))
	add := must(orchclient.AddApprover(ctx, h.owner, "eve"))
	if !add.Confirm {
		t.Fatalf("add after bootstrap does not say it needs confirmation: %+v", add)
	}
	a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	if a.State != orchapi.ApproverUnconfirmed || a.ConfirmedBy != "" {
		t.Fatalf("re-pair after removing every approver bootstrapped again: %+v", a)
	}
	agent, _ := h.session("abababab")
	ask := h.ask(agent, 4, shaA)
	if _, err := orchclient.Decide(ctx, h.device(ipTablet), ask.ID, orchapi.DecisionApprove); code(err) != api.CodeForbidden {
		t.Fatalf("unconfirmed device decided: %v", err)
	}
	// Recovery: the human deletes the marker by hand on the Mac.
	must(orchclient.RemoveApprover(ctx, h.owner, "eve"))
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if a := h.pair("ivo", ipPhone); a.State != orchapi.ApproverActive || a.ConfirmedBy != "bootstrap" {
		t.Fatalf("after manual recovery: %+v", a)
	}
}

// An ask only goes to an addressee the decision can answer: owners or this
// machine's human. Its mail is answered and taken after the decision.
func TestAskAddressees(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("acacacac")
	// bea is a real member of the project, but not this machine's human.
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{To: "human:bea", Body: "x",
		Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}}); code(err) != api.CodeBadRequest {
		t.Fatalf("ask to another human: %v", err)
	}
	ask := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{To: "human:ivo", Body: "merge?",
		Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}}))
	d := must(orchclient.Decide(ctx, h.device(ipPhone), ask.ID, orchapi.DecisionApprove))
	if d.MailError != "" {
		t.Fatalf("mail error: %s", d.MailError)
	}
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("asking session still needs_human after a decision on a human:ivo ask")
	}
	if m := must(orchclient.GetMail(ctx, h.owner, ask.ID)); m.AckedBy == "" {
		t.Fatalf("ask mail not taken: %+v", m)
	}
}

// A failed answer to the ask's mail is reported in the decision, not only logged.
func TestDecisionReportsMailFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("adadadad")
	ask := h.ask(agent, 2, shaA)
	db, err := h.dm.Deps.Store.Project(authz.All(), authz.Write, h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER no_reply BEFORE INSERT ON mail BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), ask.ID, orchapi.DecisionApprove))
	if d.MailError == "" || d.Receipt.Sig == "" {
		t.Fatalf("decision hides the failed mail answer: %+v", d)
	}
}

// A failed ask insert does not strand the asking session in needs_human.
func TestFailedAskInsertAnswersTheMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, addr := h.session("aeaeaeae")
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx,
		`CREATE TRIGGER no_ask BEFORE INSERT ON asks BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Body: "x", Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}}); err == nil {
		t.Fatal("ask succeeded without its row")
	}
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("a failed ask left the session needs_human")
	}
	for _, m := range must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{Needs: orchapi.NeedsHuman})) {
		if m.AckedBy == "" {
			t.Fatalf("needs=human mail left open by a failed ask: %+v", m)
		}
	}
}

// A device whose node key changed must pair again, and the refusal says how.
func TestRekeyedDeviceRepairs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	add := must(orchclient.AddApprover(ctx, h.owner, "bea"))
	must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "bea"))

	// bea's phone re-keys: its right is gone, and pairing says what to do.
	h.node.set(ipTablet, tailnet.Who{Login: "bea@example.com", Node: "bea-phone", NodeID: "nTABLET", NodeKey: "nodekey:new"})
	add = must(orchclient.AddApprover(ctx, h.owner, "bea2"))
	_, err := orchclient.Pair(ctx, h.device(ipTablet), add.Code)
	if code(err) != api.CodeConflict || !strings.Contains(err.Error(), "zm approver rm bea") {
		t.Fatalf("re-keyed pair: %v", err)
	}
	must(orchclient.RemoveApprover(ctx, h.owner, "bea2"))
	must(orchclient.RemoveApprover(ctx, h.owner, "bea"))
	add = must(orchclient.AddApprover(ctx, h.owner, "bea"))
	if a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code)); a.State != orchapi.ApproverUnconfirmed || a.Node != "nTABLET" {
		t.Fatalf("re-paired: %+v", a)
	}
	if a := must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "bea")); a.State != orchapi.ApproverActive {
		t.Fatalf("re-confirmed: %+v", a)
	}
}

// Another zm daemon is refused by its own hostname even when its tailnet
// name was changed (e.g. renamed in the admin console).
func TestZmNodeByHostname(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const ipRenamed = "100.100.1.7"
	h.node.set(ipRenamed, tailnet.Who{Login: "ivo@example.com", Node: "build-box", Host: "zm-studio", NodeID: "nREN", NodeKey: "nodekey:ren"})
	add := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	if _, err := orchclient.Pair(ctx, h.device(ipRenamed), add.Code); code(err) != api.CodeForbidden {
		t.Fatalf("zm daemon under another tailnet name paired: %v", err)
	}
}

// No identity may look like an approver, whichever was created first.
func TestIdentityLookingLikeApprover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(orchclient.AddApprover(ctx, h.owner, "mallory"))
	for _, name := range []string{"rnallory", "ma11ory"} {
		if _, err := h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: name}); code(err) != api.CodeConflict {
			t.Errorf("identity %q next to approver mallory: %v", name, err)
		}
	}
	if _, err := h.owner.MintToken(ctx, api.MintTokenRequest{As: "rnallory", Project: "harbor", Role: "guest"}); code(err) != api.CodeConflict {
		t.Errorf("token for a new identity rnallory: %v", err)
	}
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "mallory"})) // the exact name is the same person
}

// Dogfood follow-up: with the bootstrap marker present and no active
// approver left, a new pairing waited forever with no explanation. The add
// answer, the pairing answer, the device view and page, and the approver
// list now say nobody can confirm it and how the Mac's human recovers.
func TestStuckPairingExplainsRecovery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	if as := must(orchclient.Approvers(ctx, h.owner)); as[0].Note != "" {
		t.Fatalf("active approver carries a note: %+v", as[0])
	}
	// While an approver is active, a new one is merely waiting: no note.
	add := must(orchclient.AddApprover(ctx, h.owner, "bea"))
	if add.Note != "" || !add.Confirm {
		t.Fatalf("add with an active approver: %+v", add)
	}
	must(orchclient.RemoveApprover(ctx, h.owner, "bea"))
	must(orchclient.RemoveApprover(ctx, h.owner, "ivo"))

	marker := filepath.Join(h.dm.Deps.Config.Home, approvals.BootstrapFile)
	add = must(orchclient.AddApprover(ctx, h.owner, "eve"))
	if !add.Confirm || !strings.Contains(add.Note, "nobody can confirm") || !strings.Contains(add.Note, marker) {
		t.Fatalf("add with nobody to confirm: %+v", add)
	}
	a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	if a.State != orchapi.ApproverUnconfirmed || !strings.Contains(a.Note, "nobody can confirm") ||
		!strings.Contains(a.Note, "$ZM_HOME/"+approvals.BootstrapFile) || strings.Contains(a.Note, h.dm.Deps.Config.Home) {
		t.Fatalf("pair answer (a device never learns the Mac's paths): %+v", a)
	}
	v := must(orchclient.DeviceView(ctx, h.device(ipTablet)))
	if v.Approver == nil || !strings.Contains(v.Approver.Note, "zm approver rm eve") {
		t.Fatalf("device view: %+v", v)
	}
	resp, err := h.device(ipTablet).Stream(ctx, http.MethodGet, "/v0/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}
	if page := readAll(resp); !strings.Contains(page, "Nobody can confirm it right now") || !strings.Contains(page, "approvals/bootstrapped") {
		t.Fatalf("device page does not explain: %s", page)
	}
	as := must(orchclient.Approvers(ctx, h.owner))
	if len(as) != 1 || !strings.Contains(as[0].Note, marker) {
		t.Fatalf("approver ls: %+v", as)
	}
	// Following the note recovers: delete the marker, rm, add, pair again.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	must(orchclient.RemoveApprover(ctx, h.owner, "eve"))
	add = must(orchclient.AddApprover(ctx, h.owner, "eve"))
	if add.Note != "" || add.Confirm {
		t.Fatalf("add after recovery: %+v", add)
	}
	if a := must(orchclient.Pair(ctx, h.device(ipTablet), add.Code)); a.State != orchapi.ApproverActive || a.Note != "" {
		t.Fatalf("pair after recovery: %+v", a)
	}
}

// An ask's mail is never open without its ask row (cockpit gap G2): right
// after the mail is sent, the socket's human is already refused ack and
// reply. Sending the mail before recording the ask left a window where
// either took the mail with nothing decided.
func TestAskRecordedBeforeItsMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("abababab")
	human := h.ownerIn("harbor", "")
	var ackErr, replyErr error
	calls := 0
	approvals.SetAskMailSentHook(func(id string) {
		calls++
		_, ackErr = orchclient.Ack(ctx, human, id)
		_, replyErr = orchclient.Reply(ctx, human, id, orchapi.ReplyRequest{Body: "fine"})
	})
	t.Cleanup(func() { approvals.SetAskMailSentHook(nil) })
	ask := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Body: "merge?", Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}}))
	if calls != 1 {
		t.Fatalf("hook ran %d times", calls)
	}
	for name, err := range map[string]error{"ack": ackErr, "reply": replyErr} {
		if code(err) != api.CodeForbidden || !strings.Contains(fmt.Sprint(err), "open ask") {
			t.Errorf("%s of a just-sent ask's mail: %v", name, err)
		}
	}
	if a := must(orchclient.GetAsk(ctx, h.owner, ask.ID)); a.Decision != nil || a.Area != ask.Area {
		t.Fatalf("ask after the refused calls: %+v", a)
	}
	// A send that fails leaves no ask behind.
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Body: "   ", Subject: orchapi.AskSubject{Repo: "a/b", PR: 2, HeadSHA: shaA}}); err == nil {
		t.Fatal("an ask with an empty body was sent")
	}
	if all := must(orchclient.Asks(ctx, h.owner, "", false)); len(all) != 1 {
		t.Fatalf("asks after a failed send: %d", len(all))
	}
}

func TestReviewAskCleansBody(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("acacacac")
	ask, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Body: "merge\u202e?", Subject: orchapi.AskSubject{Repo: "a/b", PR: 1, HeadSHA: shaA}})
	if err != nil || ask.Body != "merge?" {
		t.Fatalf("ask %+v %v", ask, err)
	}
}
