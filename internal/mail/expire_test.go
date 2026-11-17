package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
)

func (f *fixture) end(addr string) {
	f.t.Helper()
	if err := f.sess.SetState(ctx, addr, orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		f.t.Fatal(err)
	}
}

// Mail about a work item to a session that ended goes to the
// live session holding the item now, not to a group mailbox nobody reads.
// A ref held by nobody live still goes to the machine.
func TestOrphanGoesToItsRefHolder(t *testing.T) {
	f := newFixture(t)
	for _, a := range []string{"mini/aaaaaaaa", "mini/bbbbbbbb", "mini/cccccccc", "mini/dddddddd"} {
		f.sess.add(a, f.harbor.ID, "", true, orchapi.StateRunning)
	}
	f.work.add(f.harbor.ID, "bd:held", fakeItem{holder: "mini/bbbbbbbb"})
	f.work.add(f.harbor.ID, "bd:gone", fakeItem{holder: "mini/dddddddd"})
	h := f.as("", "harbor")
	held := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "publish it", Ref: "bd:held"}))
	gone := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "nobody holds it", Ref: "bd:gone"}))
	f.end("mini/aaaaaaaa")
	f.end("mini/dddddddd")
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	f.sweep()
	b := f.as("mini/bbbbbbbb", "")
	var in []orchapi.Mail
	for _, m := range must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})) {
		if m.Recipient == "mini/bbbbbbbb" {
			in = append(in, m)
		}
	}
	if len(in) != 1 || in[0].Via != "orphan of mini/aaaaaaaa" || in[0].Body != "publish it" {
		t.Fatalf("holder's own mail %+v: want the copy of %s", in, held.ID)
	}
	group := must(orchclient.Inbox(ctx, f.as("mini/cccccccc", ""), orchapi.InboxQuery{}))
	if len(group) != 1 || group[0].Recipient != "machine:mini" || group[0].Body != "nobody holds it" {
		t.Fatalf("group mailbox %+v: want only the copy of %s", group, gone.ID)
	}
	must(orchclient.Ack(ctx, b, in[0].ID))
	if got := must(orchclient.GetMail(ctx, b, held.ID)); got.AckedBy != "mini/bbbbbbbb" {
		t.Fatalf("taking the holder's copy did not settle the original: %+v", got)
	}
}

// Group mail nobody takes in GroupKeep expires: zm takes it as the machine
// (settling an orphaned original too) with no owners report, and the daily
// summary counts it. Mail for a person is left alone.
func TestGroupMailExpiresWithoutReport(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	h := f.as("", "harbor")
	from := f.clock
	g := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "machine:mini", Body: "someone rebase main\nsecond line"}))
	o := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "for a, who ended"}))
	forHuman := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "machine:mini", Needs: orchapi.NeedsHuman, Body: "for the machine's human"}))
	f.end("mini/aaaaaaaa")
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	f.sweep()
	b := f.as("mini/bbbbbbbb", "")
	if in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})); len(in) != 2 {
		t.Fatalf("before GroupKeep %+v: want the group mail and the orphan copy", in)
	}
	f.clock = f.clock.Add(GroupKeep)
	f.sweep()
	f.sweep() // idempotent
	if in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("after GroupKeep %+v: want it expired", in)
	}
	for _, id := range []string{g.ID, o.ID} {
		if got := must(orchclient.GetMail(ctx, b, id)); got.AckedBy != "machine:mini" || got.AckedAt == nil {
			t.Fatalf("%s: %+v, want taken by machine:mini", id, got)
		}
	}
	if got := must(orchclient.GetMail(ctx, f.as("", ""), forHuman.ID)); got.AckedAt != nil {
		t.Fatalf("mail for the machine's human expired: %+v", got)
	}
	if rs := f.ownersReports(); len(rs) != 0 {
		t.Fatalf("owners' reports %+v: want none", rs)
	}
	got := must(f.s.noticeCounts(ctx, f.harbor.ID, from, f.clock.Add(time.Minute)))
	if n := got["group mail nobody took in a day, closed"]; n != 2 {
		t.Fatalf("summary counts %v: want 2 expired group mail", got)
	}
}

// Item mail that expires reaches its item first; only item mail the item
// refuses is named in a short owners report. Plain mail sends none.
func TestExpiredItemMailReachesItemOrIsReported(t *testing.T) {
	f := newFixture(t)
	rec := f.recorder()
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	f.work.add(f.harbor.ID, "bd:locked", fakeItem{})
	rec.refuse["bd:locked"] = true
	h := f.as("", "harbor")
	lost := must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "machine:mini", Body: "about locked", Ref: "bd:locked"}))
	must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "machine:mini", Body: "plain note"}))
	f.clock = f.clock.Add(GroupKeep + time.Minute)
	f.sweep()
	rs := f.ownersReports()
	if len(rs) != 1 || !strings.Contains(rs[0].Body, short(lost.ID)+" re bd:locked: about locked") || strings.Contains(rs[0].Body, "plain note") {
		t.Fatalf("owners' reports %+v: want one naming only the locked item mail", rs)
	}
	if got := must(orchclient.GetMail(ctx, f.as("mini/bbbbbbbb", ""), lost.ID)); got.AckedAt == nil {
		t.Fatalf("item mail not expired: %+v", got)
	}
}

// An ask notice to an ended session stays on it past OrphanAfter (the ask
// holds the outcome), and is closed by the machine after NoticeKeep. One to a
// live session stays open.
func TestAskNoticeToEndedSessionExpires(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	sys := f.d.System()
	from := f.clock
	dead := must(f.s.Send(orchcore.AsAskNotice(ctx), sys, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "OWNER DECISION (approved)"}))
	live := must(f.s.Send(orchcore.AsAskNotice(ctx), sys, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "OWNER DECISION (denied)"}))
	f.end("mini/aaaaaaaa")
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	f.sweep()
	if got := must(orchclient.GetMail(ctx, f.as("mini/aaaaaaaa", ""), dead.ID)); got.AckedAt != nil {
		t.Fatalf("closed before NoticeKeep: %+v", got)
	}
	f.clock = f.clock.Add(NoticeKeep)
	f.sweep()
	if got := must(orchclient.GetMail(ctx, f.as("mini/aaaaaaaa", ""), dead.ID)); got.AckedBy != "machine:mini" {
		t.Fatalf("ask notice to the ended session %+v: want taken by machine:mini", got)
	}
	if got := must(orchclient.GetMail(ctx, f.as("mini/bbbbbbbb", ""), live.ID)); got.AckedAt != nil {
		t.Fatalf("ask notice to a live session closed: %+v", got)
	}
	got := must(f.s.noticeCounts(ctx, f.harbor.ID, from, f.clock.Add(time.Minute)))
	if n := got["ask notices to ended sessions, closed"]; n != 1 {
		t.Fatalf("summary counts %v: want 1", got)
	}
}
