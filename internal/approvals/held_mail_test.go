package approvals_test

import (
	"time"

	"context"
	"github.com/sengirunners/mezze/internal/mail"
	"github.com/sengirunners/potluck/kit/authz"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
)

// An ask held for an outside party waits on them, not on anyone here: its
// asker stays out of needs_human, its mail says whom it waits on, and it is
// not counted as anyone's open ask.
func TestHeldAskWaitsOnItsParty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("eeeeeeee")
	held := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "holiday-shifts",
		For: &orchapi.AskFor{Group: "Santa Monica", Contact: "Dr. R"}}))
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("a held ask put its asker at needs_human")
	}
	m := must(orchclient.GetMail(ctx, h.owner, held.ID))
	if m.Needs != orchapi.NeedsHuman || m.HeldFor != "Santa Monica" {
		t.Fatalf("held ask's mail: needs %s held_for %q", m.Needs, m.HeldFor)
	}
	list := must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{Needs: orchapi.NeedsHuman, Open: true}))
	for _, x := range list {
		if x.ID == held.ID && x.HeldFor != "Santa Monica" {
			t.Fatalf("listed held mail lacks held_for: %+v", x)
		}
	}
	if n, err := orchcore.Mail(h.dm.Deps).(orchcore.TeamMail).OpenAsks(ctx, h.project.ID, addr); err != nil || n != 0 {
		t.Fatalf("open asks counted %d (%v), want 0", n, err)
	}

	// A plain ask from the same session still waits on a person.
	it := h.workItem("held-and-plain")
	plain := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State != orchapi.StateNeedsHuman {
		t.Fatalf("plain ask: state %s, want needs_human", s.State)
	}
	if m := must(orchclient.GetMail(ctx, h.owner, plain.ID)); m.HeldFor != "" {
		t.Fatalf("plain ask marked held: %+v", m)
	}
	// Answering the plain one frees the session though the held one is open.
	must(orchclient.Decide(ctx, h.device(ipPhone), plain.ID, "a"))
	if s := must(orchclient.Session(ctx, h.owner, addr)); s.State == orchapi.StateNeedsHuman {
		t.Fatal("session still needs_human with only a held ask open")
	}
	// Once answered, the held ask's mail is taken and no longer marked.
	must(orchclient.DecideWith(ctx, h.device(ipPhone), held.ID, orchapi.DecideRequest{Decision: "a", As: orchapi.AnsweredAsContact}))
	if m := must(orchclient.GetMail(ctx, h.owner, held.ID)); m.AckedBy == "" || m.HeldFor != "" {
		t.Fatalf("answered held ask's mail: %+v", m)
	}
}

// An ask's answer and a withdrawal sent to an asker whose session then ended
// stay on it: the orphan sweep never copies them to the machine's mailbox.
func TestAskNoticesAreNotOrphanedToMachine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("fafafafa")
	decided := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "decided"}))
	withdrawn := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "withdrawn"}))
	must(orchclient.Decide(ctx, h.device(ipPhone), decided.ID, "a"))
	must(orchclient.WithdrawAsk(ctx, h.owner, withdrawn.ID))
	if got := must(orchclient.Register(ctx, h.ownerIn("harbor/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "fafafafa-uuid", End: true})); got.State != orchapi.StateEnded || got.Address != addr {
		t.Fatalf("end: %+v", got)
	}
	sw, ok := orchcore.Mail(h.dm.Deps).(interface {
		SetClock(func() time.Time)
		SweepOrphans(context.Context) error
	})
	if !ok {
		t.Fatal("mail service lacks the sweep seam")
	}
	sw.SetClock(func() time.Time { return time.Now().Add(mail.OrphanAfter + time.Minute) })
	if err := sw.SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Read, decided.Project))
	var machine, notices int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE recipient LIKE 'machine:%'`).Scan(&machine); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE recipient = ? AND json_extract(provenance, '$.ask_notice') = 1`, addr).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if machine != 0 || notices != 2 {
		t.Fatalf("machine copies %d, flagged notices on the ended session %d; want 0 and 2", machine, notices)
	}
}
