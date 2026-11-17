package approvals_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
)

// The project's owner answers a card ask from their own socket, with no
// approver paired, in one call; the decision records how it was made.
func TestOwnerAnswersFromTheSocket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("trade picks")
	agent, _ := h.session("aaaaaaaa")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	if a.Note != "" {
		t.Errorf("no approver is no longer worth a note: %q", a.Note)
	}

	d, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != orchapi.MethodOwner || d.Machine != "mini" || d.Device != "" || d.Approver != "ivo" ||
		d.Receipt.Node != "owner:mini" || d.RecordedAt == nil || d.MailError != "" {
		t.Fatalf("decision: %+v", d)
	}
	pub := must(h.svc.PublicKey())
	if c := approvals.Verify(d.Receipt, pub, orchapi.ReceiptExpect{Work: it.ID, Decision: "b", Ask: a.ID, Project: a.Project, Approver: "ivo"}); !c.Valid {
		t.Fatalf("verify: %+v", c)
	}
	if cs := h.comments(it.ID); len(cs) != 1 || !strings.HasPrefix(cs[0], "OWNER DECISION (ivo via \"mini\", ") {
		t.Fatalf("comments: %q", cs)
	}
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Status != orchapi.AskRecorded || got.Decision == nil || got.Decision.Method != orchapi.MethodOwner || got.Decision.Machine != "mini" {
		t.Fatalf("ask after: %+v", got.Decision)
	}
	// One decision per ask: the same answer is the same receipt, another 409.
	if again, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "b"}); err != nil || again.Receipt.Sig != d.Receipt.Sig {
		t.Fatalf("same answer: %+v %v", again, err)
	}
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeConflict {
		t.Fatalf("another answer: %v", err)
	}
	if n := len(h.comments(it.ID)); n != 1 {
		t.Fatalf("comments after repeats: %d", n)
	}
}

// A paired approver's decision says so; a minted token (an agent's, a CI's),
// even an owner's, does not decide; nor does anyone on a pull request's ask.
func TestOwnerAnswerLimits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("trade picks")
	a := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))

	// A minted token keeps its cap: an agent's member token does not decide,
	// though its identity owns the project.
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "member"}))
	c := client.WithHandler(h.dm.Handler)
	c.Token, c.Project, c.ProjectFrom = tok.Secret, "harbor", "env"
	if _, err := orchclient.Choose(ctx, c, "", a.ID, orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeForbidden {
		t.Errorf("a member-capped token: %v", err)
	}
	if _, err := orchclient.Choose(ctx, h.owner, "", "01JZZZZZZZZZZZZZZZZZZZZZZZ", orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeNotFound {
		t.Errorf("a missing ask: %v", err)
	}
	pr := h.ask(h.ownerIn("harbor", ""), 7, shaA)
	if _, err := orchclient.Choose(ctx, h.owner, "", pr.ID, orchapi.DecideRequest{Decision: "approve"}); code(err) != api.CodeForbidden {
		t.Errorf("a pull request's ask: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision != nil {
		t.Fatalf("decided by a refused caller: %+v", got.Decision)
	}

	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.Method != orchapi.MethodApprover || d.Device != "ivo-phone" || d.Machine != "" {
		t.Fatalf("approver's decision: %+v", d)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Decision == nil || got.Decision.Method != orchapi.MethodApprover || got.Decision.Device != "ivo-phone" {
		t.Fatalf("read back: %+v", got.Decision)
	}
	// The owner's same choice afterwards is the approver's decision, not a new one.
	if _, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"}); code(err) != api.CodeConflict {
		t.Errorf("after the approver: %v", err)
	}
}

// An item nobody holds, made by someone zm cannot mail (a migrated bead): the
// owner's answer still records and lands on the item; only the holder notice
// has nowhere to go (five harbor asks stuck on a 409).
func TestOwnerAnswersWhenNobodyCanBeMailed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	it := h.workItem("rename shifts")
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Read, h.project.ID))
	if res, err := db.ExecContext(ctx, `UPDATE work_items SET created_by = '' WHERE ref = ?`, strings.TrimPrefix(it.ID, "local:")); err != nil || must(res.RowsAffected()) != 1 {
		t.Fatalf("clear creator: %v", err)
	}
	agent, _ := h.session("aaaaaaab")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	d, err := orchclient.Choose(ctx, h.owner, "", a.ID, orchapi.DecideRequest{Decision: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != "a" || d.Method != orchapi.MethodOwner {
		t.Fatalf("decision: %+v", d)
	}
	if cs := h.comments(it.ID); len(cs) != 1 || !strings.HasPrefix(cs[0], "OWNER DECISION") {
		t.Fatalf("comments: %q", cs)
	}
}
