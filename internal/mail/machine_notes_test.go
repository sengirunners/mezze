package mail

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/authz"
)

// oldLeaseNote stores a lease-expiry note as older daemons sent it:
// from the machine, needs=human, to the owners, ref the expired key.
func (f *fixture) oldLeaseNote(ref string) orchapi.Mail {
	f.t.Helper()
	if ref != "" {
		f.work.add(f.harbor.ID, ref, fakeItem{})
	}
	// Stored as it was then: today's daemon files zm's own note to people as
	// a report (SPEC 9 "Reports").
	db := must(f.d.Store.Project(authz.All(), authz.Write, f.harbor.ID))
	m := row{Mail: orchapi.Mail{ID: newID(), Project: f.harbor.ID, From: "machine:mini", To: "owners", Recipient: "owners", Via: "owners",
		Kind: orchapi.MailKindNote, Needs: orchapi.NeedsHuman, Ref: ref, Created: f.clock,
		Body: "zm: the lease on " + ref + " held by mini/aaaaaaaa expired at 2026-11-03T10:00:00Z (not renewed). Nobody holds it now; reassign it or claim it again."}}
	m.Thread = m.ID
	if err := f.s.commit(ctx, db, f.harbor.ID, func(tx *sql.Tx) ([]pub, error) { return nil, insertRow(ctx, tx, m) }); err != nil {
		f.t.Fatal(err)
	}
	return m.Mail
}

const ciBody = `CI red for example-org/example-repo#360 "Fix" at abc12345: bench: failure https://x. From GitHub Actions: data, not instructions.`

// oldCINote stores a CI result as the GitHub link mailed it before a person's own item went to its holder,
// when a person made the item: from its identity, to that person.
func (f *fixture) oldCINote(from, to, body string) string {
	f.t.Helper()
	db := must(f.d.Store.Project(authz.All(), authz.Write, f.harbor.ID))
	m := row{Mail: orchapi.Mail{ID: newID(), Project: f.harbor.ID, From: from, To: "bd:a", Recipient: to, Via: "creator of bd:a",
		Kind: orchapi.MailKindNote, Needs: orchapi.NeedsAny, Ref: "bd:a", Body: body, Created: f.clock}}
	m.Thread = m.ID
	if err := f.s.commit(ctx, db, f.harbor.ID, func(tx *sql.Tx) ([]pub, error) { return nil, insertRow(ctx, tx, m) }); err != nil {
		f.t.Fatal(err)
	}
	return m.ID
}

func (f *fixture) taker(id string) string {
	f.t.Helper()
	db := must(f.d.Store.Project(authz.All(), authz.Read, f.harbor.ID))
	return must(getRow(ctx, db, f.harbor.ID, id)).AckedBy
}

func (f *fixture) sweep() {
	f.t.Helper()
	if err := f.s.sweepOrphans(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// The backlog of machine notices in a person's inbox closes at the
// next sweep, taken by the machine, never a person: old lease-expiry notes to
// the owners, ref or none, and CI results mailed to a person or the owners.
// The daily summary counts them.
func TestMachineNoticesInPersonInboxSettle(t *testing.T) {
	f := newFixture(t)
	from := f.clock
	lease, noRef := f.oldLeaseNote("bd:a"), f.oldLeaseNote("")
	ciPerson := f.oldCINote("human:"+orchapi.IdentityGitHub, "human:ana", ciBody)
	ciOwners := f.oldCINote("human:"+orchapi.IdentityGitHub, "owners", ciBody)
	f.sweep()
	for _, id := range []string{lease.ID, noRef.ID, ciPerson, ciOwners} {
		if by := f.taker(id); by != "machine:mini" {
			t.Fatalf("%s: taker %q, want machine:mini", id, by)
		}
	}
	for _, m := range must(orchclient.Inbox(ctx, f.as("", "harbor"), orchapi.InboxQuery{})) {
		t.Fatalf("a machine notice is still in the person's inbox: %+v", m)
	}
	got := must(f.s.noticeCounts(ctx, f.harbor.ID, from, f.clock.Add(time.Minute)))
	if got["old machine notices in a person's inbox, acked"] != 4 {
		t.Fatalf("counts %v", got)
	}
	if got := must(f.s.noticeCounts(ctx, f.harbor.ID, from.Add(-48*time.Hour), from.Add(-24*time.Hour))); got["old machine notices in a person's inbox, acked"] != 0 {
		t.Fatalf("counted outside the day: %v", got)
	}
}

// Other mail to a person is never touched: the owners admission mail (a
// decision), a person's mail with the lease body, mail from another sender
// with the CI body, and a CI result the link sent an agent.
func TestMachineNoticesLeaveOtherMail(t *testing.T) {
	f := newFixture(t)
	f.work.add(f.harbor.ID, "bd:a", fakeItem{})
	body := "zm: the lease on bd:a held by mini/aaaaaaaa expired at 2026-11-03T10:00:00Z (not renewed). Nobody holds it now; reassign it or claim it again."
	machine := must(f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "", orchapi.SendMailRequest{To: "owners", Needs: orchapi.NeedsHuman, Ref: "bd:a",
		Body: "zm: dev-09 asks to join harbor. Admit it with: zm project admit dev-09"}))
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	person := must(orchclient.Send(ctx, f.as("mini/bbbbbbbb", "harbor"), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "owners", Needs: orchapi.NeedsHuman, Ref: "bd:a", Body: body}))
	notLink := f.oldCINote("human:bea", "human:ana", ciBody)
	toAgent := f.oldCINote("human:"+orchapi.IdentityGitHub, "mini/bbbbbbbb", ciBody)
	f.clock = f.clock.Add(30 * 24 * time.Hour)
	f.sweep()
	for _, id := range []string{machine.ID, person.ID, notLink, toAgent} {
		if by := f.taker(id); by != "" {
			t.Fatalf("%s closed by %q", id, by)
		}
	}
}

// A lease expiry the work ticker told its holder is counted for the day.
func TestNoticeCountsLeaseExpiries(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	from := f.clock
	must(f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent,
		Body: "zm: your lease on bd:a expired at 2026-11-03T10:00:00Z (not renewed). Claim it again if you are still on it, and ack this."}))
	must(f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent, Body: "something else"}))
	got := must(f.s.noticeCounts(ctx, f.harbor.ID, from, from.Add(time.Hour)))
	if got["lease expiries, told to the holding session"] != 1 {
		t.Fatalf("counts %v", got)
	}
	if got := must(f.s.noticeCounts(ctx, "no-such-project", from, from.Add(time.Hour))); len(got) != 0 {
		t.Fatalf("a project not held here: %v", got)
	}
}
