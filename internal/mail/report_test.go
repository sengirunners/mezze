package mail

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// SPEC 9 "Reports": an agent's mail that would reach a person is filed as a
// report: readable, never in an inbox, the notice count or a wake-up.
func TestAgentMailToPersonIsReport(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateIdle)
	s := f.as("mini/aaaaaaaa", "harbor")
	toPeople := []orchapi.SendMailRequest{
		{To: "human:ana", Body: "night digest"},
		{To: "owners", Body: "deploy to prod?", Kind: orchapi.MailKindNote},
		{To: "machine:mini", Body: "tell your human", Needs: orchapi.NeedsHuman},
		{To: "mini/bbbbbbbb", Body: "ask your human", Needs: orchapi.NeedsHuman},
	}
	// A plain note to a person fails loudly, names zm ask new and
	// stores nothing.
	for _, req := range toPeople {
		_, err := orchclient.Send(ctx, s, req)
		wantCode(t, err, api.CodeBadRequest)
		if !strings.Contains(err.Error(), "zm ask new") || !strings.Contains(err.Error(), "not sent") {
			t.Fatalf("refusal to %s does not point at zm ask new: %v", req.To, err)
		}
	}
	if sent := must(orchclient.Mail(ctx, s, orchapi.MailQuery{From: "mini/aaaaaaaa"})); len(sent) != 0 {
		t.Fatalf("a refused mail was stored: %+v", sent)
	}
	var reports []orchapi.Mail
	for _, req := range toPeople {
		req.Kind = orchapi.MailKindReport
		m := must(orchclient.Send(ctx, s, req))
		if m.Kind != orchapi.MailKindReport || m.Needs != orchapi.NeedsHuman {
			t.Fatalf("not filed as a report: %+v", m)
		}
		reports = append(reports, m)
	}
	session := reports[3]
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateRunning {
		t.Fatalf("a report put its sender in %s", st)
	}
	if n := f.sess.takeNudged(); len(n) != 0 {
		t.Fatalf("a report woke %v", n)
	}
	h := f.as("", "")
	if in := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("reports in the person's inbox: %+v", in)
	}
	if n := must(orchclient.Notice(ctx, h)); n.Line != "" || n.Unread != 0 {
		t.Fatalf("reports counted in the notice: %+v", n)
	}
	for _, addr := range []string{"mini/bbbbbbbb", "machine:mini"} {
		if in := must(orchclient.Inbox(ctx, f.as(addr, "harbor"), orchapi.InboxQuery{})); len(in) != 0 {
			t.Fatalf("%s sees reports: %+v", addr, in)
		}
	}
	rs := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{Reports: true}))
	if len(rs) != 4 {
		t.Fatalf("reports %+v", rs)
	}
	// an agent has no reports box
	if rs := must(orchclient.Inbox(ctx, s, orchapi.InboxQuery{Reports: true})); len(rs) != 0 {
		t.Fatalf("agent reports %+v", rs)
	}
	// mark read: an ack, by the person only
	if _, err := orchclient.Ack(ctx, f.as("mini/bbbbbbbb", "harbor"), session.ID); err == nil {
		t.Fatal("an agent marked a report read")
	}
	for _, m := range rs {
		must(orchclient.Ack(ctx, h, m.ID))
	}
	if rs := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{Reports: true})); len(rs) != 0 {
		t.Fatalf("read reports still unread: %+v", rs)
	}
	if rs := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{Reports: true, All: true})); len(rs) != 4 {
		t.Fatalf("read reports gone: %+v", rs)
	}
	// a reply to the person is a report too
	q := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "status?"}))
	r := must(orchclient.Reply(ctx, s, q.ID, orchapi.ReplyRequest{Body: "fine"}))
	if r.Kind != orchapi.MailKindReport || r.Note == "" {
		t.Fatalf("reply %+v", r)
	}
	if in := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("reply in the person's inbox: %+v", in)
	}
	// reply_to that answers the parent's sender is a reply; naming anyone
	// else is fresh mail and refused.
	if r := must(orchclient.Send(ctx, s, orchapi.SendMailRequest{To: q.From, ReplyTo: q.ID, Body: "fine too"})); r.Kind != orchapi.MailKindReport {
		t.Fatalf("reply_to its sender %+v", r)
	}
	_, err := orchclient.Send(ctx, s, orchapi.SendMailRequest{To: "owners", ReplyTo: q.ID, Body: "should I merge #12?"})
	wantCode(t, err, api.CodeBadRequest)
}

// Mail between people, the person's own mail to agents, an ask's mail and a
// person's explicit report keep their rules.
func TestReportsLeavePeopleAndAsksAlone(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	h := f.as("", "harbor")
	toPerson := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "owners", Body: "lunch?"}))
	toAgent := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "OWNER DECISION: ship it"}))
	if toPerson.Kind != orchapi.MailKindNote || toPerson.Note != "" || toAgent.Kind != orchapi.MailKindNote {
		t.Fatalf("people's mail changed: %+v %+v", toPerson, toAgent)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/aaaaaaaa", "harbor"), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != toAgent.ID {
		t.Fatalf("agent inbox %+v", in)
	}
	f.s.SetAsks(fakeAsks{"01ASKAAAAAAAAAAAAAAAAAAAAA": true})
	ask, err := f.s.Send(ctx, f.d.Projects.Local("mini/aaaaaaaa"), f.harbor.Ref(), "",
		orchapi.SendMailRequest{To: "owners", Body: "merge #12?", Needs: orchapi.NeedsHuman, ID: "01ASKAAAAAAAAAAAAAAAAAAAAA"})
	if err != nil || ask.Kind != orchapi.MailKindNote || ask.Note != "" {
		t.Fatalf("ask %+v %v", ask, err)
	}
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateNeedsHuman {
		t.Fatalf("asker state %s", st)
	}
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{})); len(in) != 2 {
		t.Fatalf("person's inbox %+v", in)
	}
	// a person (or a job running as one) may file a report on purpose
	rep := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "human:ana", Body: "daily summary", Kind: orchapi.MailKindReport}))
	if rep.Kind != orchapi.MailKindReport || rep.Needs != orchapi.NeedsHuman {
		t.Fatalf("explicit report %+v", rep)
	}
	if rs := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{Reports: true})); len(rs) != 1 || rs[0].ID != rep.ID {
		t.Fatalf("reports %+v", rs)
	}
	_, err = orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "x", Kind: orchapi.MailKindReport})
	wantCode(t, err, api.CodeBadRequest)
}

// A reply marked Silent takes the parent and tells nobody: the approvals
// module's notice would only tell a person what they just did.
func TestSilentReplyOnlyTakes(t *testing.T) {
	f := newFixture(t)
	local := f.d.Projects.Local("")
	ask, err := f.s.Send(ctx, local, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "owners", Body: "q?", Needs: orchapi.NeedsHuman})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Reply(ctx, local, f.harbor.ID, ask.ID, orchapi.ReplyRequest{Body: "withdrawn by human:ana", Silent: true}); err != nil {
		t.Fatal(err)
	}
	all := must(orchclient.Mail(ctx, f.as("", ""), orchapi.MailQuery{}))
	if len(all) != 1 || all[0].AckedAt == nil {
		t.Fatalf("silent reply sent mail or left the ask open: %+v", all)
	}
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("inbox %+v", in)
	}
}

// A report to owners is in every owner's reports box; a person's reply to it
// reaches the posting session, and once that session has ended the reply is
// copied to its machine's group mailbox after OrphanAfter (SPEC 9 rule 4). A
// private report's reply stays private and never orphans.
func TestReportReplyOrphansToMachine(t *testing.T) {
	f := newFixture(t)
	owner := f.d.Projects.Local("")
	tok, err := f.d.Projects.Mint(ctx, owner, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	bea := client.WithHandler(f.h)
	bea.Token = tok.Secret
	ana := f.as("", "")
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	poster := f.as("mini/aaaaaaaa", "harbor")
	team := must(orchclient.Send(ctx, poster, orchapi.SendMailRequest{To: "owners", Body: "daily summary", Kind: orchapi.MailKindReport}))
	private := must(orchclient.Send(ctx, poster, orchapi.SendMailRequest{To: "human:ana", Body: "your digest", Kind: orchapi.MailKindReport}))
	for who, c := range map[string]*client.Client{"ana": ana, "bea": bea} {
		var ids []string
		for _, m := range must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{Reports: true, All: true})) {
			ids = append(ids, m.ID)
		}
		if !slices.Contains(ids, team.ID) || slices.Contains(ids, private.ID) != (who == "ana") {
			t.Fatalf("%s's reports %v", who, ids)
		}
	}
	if err := f.sess.SetState(ctx, "mini/aaaaaaaa", orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		t.Fatal(err)
	}
	teamReply := must(orchclient.Reply(ctx, bea, team.ID, orchapi.ReplyRequest{Body: "thanks"}))
	privateReply := must(orchclient.Reply(ctx, ana, private.ID, orchapi.ReplyRequest{Body: "noted"}))
	if teamReply.Recipient != "mini/aaaaaaaa" || privateReply.Recipient != "mini/aaaaaaaa" || privateReply.Audience != orchapi.MailAudienceParticipants {
		t.Fatalf("replies %+v %+v", teamReply, privateReply)
	}
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	in := must(orchclient.Inbox(ctx, f.as("mini/bbbbbbbb", "harbor"), orchapi.InboxQuery{}))
	if len(in) != 1 || in[0].Recipient != "machine:mini" || in[0].Via != "orphan of mini/aaaaaaaa" || in[0].Body != "thanks" {
		t.Fatalf("machine mailbox %+v", in)
	}
}

// fakeAsks is the approvals module's ask check: these mail ids are asks.
type fakeAsks map[string]bool

func (f fakeAsks) OpenAsk(context.Context, string, string) (bool, error) { return false, nil }
func (f fakeAsks) IsAsk(_ context.Context, _, id string) (bool, error)   { return f[id], nil }
