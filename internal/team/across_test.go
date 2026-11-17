package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// registerNone registers a hook session on d in project none.
func registerNone(t *testing.T, d *daemon, rid string) orchapi.Session {
	t.Helper()
	ss := mustf(orchclient.Register(context.Background(), at(d.owner, "", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Title: rid}))(t)
	if ss.Project != api.ProjectNone {
		t.Fatalf("%s registered in %q, want none", rid, ss.Project)
	}
	return ss
}

// errBody is err's JSON body with typed replaced by X (hidden and missing
// answers must match byte for byte).
func errBody(err error, typed string) string {
	b, _ := json.Marshal(err)
	return strings.ReplaceAll(string(b)+err.Error(), typed, "X")
}

// SPEC 9 "Across projects": a session registered in another project, or in
// none, is reached by mail in the sender's project; replies always reach the
// sender; guests and readers of a project its machine did not join learn
// nothing.
func TestMailAcrossProjects(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	pid := j.Remote.Project.ID

	// Sessions on the replica: one in the team project, one in none.
	own := register(t, r, "harbor/solver", "bea-own")
	nop := registerNone(t, r, "bea-none")
	// On the authority: one in the project, one in none.
	ms := register(t, a, "harbor", "mini-sess")
	anop := registerNone(t, a, "mini-none")
	am := at(a.owner, "harbor", ms.Address)

	heartbeat(t, r)
	// The authority's session mails the replica's no-project session: it
	// lands in harbor, the answer shows the address as typed.
	m1 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: nop.Address, Body: "hello, no project"}))(t)
	if m1.Project != pid || m1.To != nop.Address || m1.Recipient != nop.Address {
		t.Fatalf("mail to a no-project session %+v", m1)
	}
	rn := at(r.owner, "", nop.Address)
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, rn, orchapi.InboxQuery{}))(t); !hasMail(in, m1.ID) {
			return fmt.Errorf("no-project session's inbox %+v", in)
		}
		return nil
	})
	// Its notice counts it (the heartbeat's answer for elsewhere sessions).
	heartbeat(t, r)
	if n := mustf(orchclient.Notice(ctx, rn))(t); n.Unread != 1 || !strings.Contains(n.Line, "harbor") {
		t.Fatalf("no-project session's notice %+v", n)
	}
	// It replies; the reply reaches the sender.
	rep := mustf(orchclient.Reply(ctx, rn, m1.ID, orchapi.ReplyRequest{Body: "got it"}))(t)
	if rep.Recipient != ms.Address {
		t.Fatalf("reply %+v", rep)
	}
	if in := mustf(orchclient.Inbox(ctx, am, orchapi.InboxQuery{}))(t); !hasMail(in, rep.ID) {
		t.Fatalf("sender's inbox %+v", in)
	}

	// The no-project session sends into harbor; the answer to it comes back
	// (the observed failure: zm reply said no session).
	m2 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", nop.Address), orchapi.SendMailRequest{To: ms.Address, Body: "from no project"}))(t)
	back := mustf(orchclient.Reply(ctx, am, m2.ID, orchapi.ReplyRequest{Body: "answer"}))(t)
	if back.Recipient != nop.Address {
		t.Fatalf("reply to a no-project session %+v", back)
	}
	// From the replica: the team project's session mails the no-project
	// session on its own machine (through the authority).
	m3 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", own.Address), orchapi.SendMailRequest{To: nop.Address, Body: "same machine"}))(t)
	if m3.Recipient != nop.Address {
		t.Fatalf("replica to its own no-project session %+v", m3)
	}

	// The authority's own no-project session: the replica's session mails it
	// and gets the reply.
	m4 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", own.Address), orchapi.SendMailRequest{To: anop.Address, Body: "to mini, no project"}))(t)
	if m4.Recipient != anop.Address {
		t.Fatalf("mail to the authority's no-project session %+v", m4)
	}
	r4 := mustf(orchclient.Reply(ctx, at(a.owner, "", anop.Address), m4.ID, orchapi.ReplyRequest{Body: "back to bea"}))(t)
	if r4.Recipient != own.Address {
		t.Fatalf("reply from the authority's no-project session %+v", r4)
	}
	// A reply reaches a sender the authority never knew (no session row, no
	// heartbeat), and one that has ended.
	ghost := "mini/zz99yy88"
	m5 := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ghost), orchapi.SendMailRequest{To: own.Address, Body: "from a ghost"}))(t)
	// (The replier cannot see the ghost's address: it shows as someone.)
	r5 := mustf(orchclient.Reply(ctx, at(r.owner, "", own.Address), m5.ID, orchapi.ReplyRequest{Body: "hi ghost"}))(t)
	if in := mustf(orchclient.Inbox(ctx, at(a.owner, "", ghost), orchapi.InboxQuery{}))(t); !hasMail(in, r5.ID) {
		t.Fatalf("reply to an unknown sender did not reach it: %+v", in)
	}
	// A no-project session that asks a human through the proxy waits, and
	// is answered once the authority says its ask is closed. (An ask: an
	// agent's other mail to people is a report, which waits on nobody.)
	ask := mustf(orchclient.NewAsk(ctx, at(r.owner, "harbor", nop.Address), orchapi.NewAskRequest{Card: &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}}))(t)
	if got := mustf(orchclient.Session(ctx, r.owner, nop.Address))(t); got.State != orchapi.StateNeedsHuman {
		t.Fatalf("asking no-project session %+v", got)
	}
	mustf(orchclient.WithdrawAsk(ctx, at(a.owner, "harbor", ""), ask.ID))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, nop.Address))(t); got.State != orchapi.StateIdle || got.Note != "answered" {
		t.Fatalf("answered no-project session %+v", got)
	}

	// It ends: the next heartbeat (now listing nothing elsewhere) makes it
	// known but not live; fresh mail to it is refused like a missing
	// session's, and its reply still reaches the sender it answers.
	mustf(orchclient.Register(ctx, r.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-none", End: true}))(t)
	heartbeat(t, r)
	if es, ok := sessionModule(t, a).Elsewhere(pid, nop.Address); !ok || es.Live {
		t.Fatalf("ended no-project session at the authority: %+v %v", es, ok)
	}
	if _, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: nop.Address, Body: "after it ended"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail to an ended no-project session: %v", err)
	}
	if r6 := mustf(orchclient.Reply(ctx, am, m2.ID, orchapi.ReplyRequest{Body: "a reply after it ended"}))(t); r6.Recipient != nop.Address {
		t.Fatalf("reply to an ended no-project session %+v", r6)
	}

	// An address no heartbeat listed is refused, not stored.
	if _, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: "bea-mac/zzzzzzzz", Body: "typo"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail to an unknown address: %v", err)
	}
	// The authority's own session in a project the member cannot see is not
	// confirmed: its answer is a missing session's.
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zz-hidden"}))(t)
	hs := register(t, a, "zz-hidden", "mini-hidden")
	cm := at(r.owner, "harbor", own.Address)
	_, want := orchclient.Send(ctx, cm, orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x"})
	if _, err := orchclient.Send(ctx, cm, orchapi.SendMailRequest{To: hs.Address, Body: "x"}); !api.IsCode(err, api.CodeNotFound) ||
		errBody(err, hs.Address) != errBody(want, "mini/zzzzzzzz") {
		t.Fatalf("authority session in a hidden project: %v (missing: %v)", err, want)
	}
}

// A session registered in another team project of the same replica is
// reached from this one (one session, several projects), and a guest, or a
// reader of a project the session's machine did not join, learns nothing.
func TestMailAcrossProjectsScope(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "sengi"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "sengi"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "sengi", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	g, _ := replica(t, tn, a, "vela-mac", "nVELA", "harbor/solver", "vela", "guest")

	sg := register(t, r, "sengi", "bea-sengi")
	nop := registerNone(t, r, "bea-none")
	vs := register(t, g, "harbor", "vela-solver")
	vnop := registerNone(t, g, "vela-none")
	anop := registerNone(t, a, "mini-none")
	ms := register(t, a, "harbor", "mini-sess")
	heartbeat(t, r)
	heartbeat(t, g)
	am := at(a.owner, "harbor", ms.Address)

	// harbor reaches the replica's sengi session; its inbox (every joined
	// project's) shows it.
	m1 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: sg.Address, Body: "harbor to sengi session"}))(t)
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, at(r.owner, "", sg.Address), orchapi.InboxQuery{}))(t); !hasMail(in, m1.ID) {
			return fmt.Errorf("sengi session's inbox %+v", in)
		}
		return nil
	})
	// A guest's replica session in none: mail from a whole-project reader is
	// filed in the guest's one area, and the guest reads it; another area
	// is refused with the fix.
	m2 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: vnop.Address, Body: "to the guest's no-project session"}))(t)
	if m2.Area != "solver" {
		t.Fatalf("mail to a guest's no-project session filed in %q", m2.Area)
	}
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, at(g.owner, "", vnop.Address), orchapi.InboxQuery{}))(t); !hasMail(in, m2.ID) {
			return fmt.Errorf("guest's no-project inbox %+v", in)
		}
		return nil
	})
	if _, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: vnop.Address, Area: "billing", Body: "wrong area"}); !api.IsCode(err, api.CodeBadRequest) ||
		!strings.Contains(err.Error(), "--area") {
		t.Fatalf("mail outside the guest's areas: %v", err)
	}

	// Leaks: a guest (area-only reader) addresses no session outside its
	// area: the replica's no-project and sengi sessions and the authority's
	// no-project session answer exactly like a missing one.
	gs := at(g.owner, "harbor", vs.Address)
	missing := "bea-mac/zzzzzzzz"
	_, want := orchclient.Send(ctx, gs, orchapi.SendMailRequest{To: missing, Body: "x"})
	if !api.IsCode(want, api.CodeNotFound) {
		t.Fatalf("guest to a missing session: %v", want)
	}
	for _, hidden := range []string{nop.Address, sg.Address, anop.Address} {
		_, err := orchclient.Send(ctx, gs, orchapi.SendMailRequest{To: hidden, Body: "x"})
		if !api.IsCode(err, api.CodeNotFound) || errBody(err, hidden) != errBody(want, missing) {
			t.Fatalf("guest to hidden %s: %v (missing: %v)", hidden, err, want)
		}
	}
	// A reader of a project the session's machine did not join (vela-mac
	// never joined sengi) gets the missing answer too.
	ss := register(t, a, "sengi", "mini-sengi")
	sm := at(a.owner, "sengi", ss.Address)
	_, want = orchclient.Send(ctx, sm, orchapi.SendMailRequest{To: "vela-mac/zzzzzzzz", Body: "x"})
	if _, err := orchclient.Send(ctx, sm, orchapi.SendMailRequest{To: vnop.Address, Body: "x"}); !api.IsCode(err, api.CodeNotFound) ||
		errBody(err, vnop.Address) != errBody(want, "vela-mac/zzzzzzzz") {
		t.Fatalf("mail from a project the machine did not join: %v (missing: %v)", err, want)
	}
	// A guest replica that lists a session on another machine as elsewhere
	// is refused, like one in sessions.
	tf := ""
	for _, rm := range mustf(g.owner.Remotes(ctx))(t) {
		tf = rm.TokenFile
	}
	if code, body := presenceAs(t, tn, g, tf, presenceReq{Project: m2.Project, Elsewhere: []string{"bea-mac/abcd1234"}}); code != http.StatusForbidden {
		t.Fatalf("elsewhere on another machine: %d %s", code, body)
	}
	// Past the cap the rest of elsewhere is dropped, never the heartbeat.
	many := []string{}
	for i := 0; i < 600; i++ {
		many = append(many, fmt.Sprintf("vela-mac/x%07d", i))
	}
	if code, body := presenceAs(t, tn, g, tf, presenceReq{Project: m2.Project, Sessions: []orchapi.PresenceSession{}, Elsewhere: many}); code != http.StatusOK {
		t.Fatalf("heartbeat with 600 elsewhere: %d %s", code, body)
	}
}

// The cost of listing sessions outside the project in a heartbeat: the
// authority answers each with its notice parts.
func TestElsewhereHeartbeatCost(t *testing.T) {
	if testing.Short() {
		t.Skip("latency")
	}
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	for i := 0; i < 9; i++ {
		register(t, r, "harbor/solver", fmt.Sprintf("bea-in-%d", i))
	}
	const n = 200
	measure := func() (p50, p95 time.Duration) {
		heartbeat(t, r)
		ds := make([]time.Duration, n)
		for i := range ds {
			t0 := time.Now()
			heartbeat(t, r)
			ds[i] = time.Since(t0)
		}
		sortDurations(ds)
		return ds[n/2], ds[n*95/100]
	}
	in50, in95 := measure()
	for i := 0; i < 9; i++ {
		registerNone(t, r, fmt.Sprintf("bea-out-%d", i))
	}
	out50, out95 := measure()
	t.Logf("loopback, %d runs, heartbeat p50 / p95: 9 sessions %v / %v; 9 sessions + 9 elsewhere %v / %v", n, in50, in95, out50, out95)
}

// SPEC 8 "Names" across projects: a name of a session registered outside
// the sender's project resolves to its address, the mail stores the address
// and the send answer shows it; a guest's answer for such a name is a
// missing name's, never the address.
func TestMailAcrossProjectsByName(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	g, _ := replica(t, tn, a, "vela-mac", "nVELA", "harbor/solver", "vela", "guest")
	own := register(t, r, "harbor/solver", "bea-own")
	nop := registerNone(t, r, "bea-none")
	ms := register(t, a, "harbor", "mini-sess")
	anop := registerNone(t, a, "mini-none")
	vs := register(t, g, "harbor", "vela-solver")
	mustf(orchclient.SetSessionName(ctx, r.owner, nop.Address, "night-shift"))(t)
	mustf(orchclient.SetSessionName(ctx, a.owner, anop.Address, "mini-free"))(t)
	heartbeat(t, r)
	heartbeat(t, g)

	// The replica's session names its own machine's no-project session.
	m1 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", own.Address), orchapi.SendMailRequest{To: "bea-mac/night-shift", Body: "by name"}))(t)
	if m1.To != nop.Address || m1.Recipient != nop.Address {
		t.Fatalf("replica mail by name across projects: %+v", m1)
	}
	// The authority's session names the authority's no-project session.
	m2 := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ms.Address), orchapi.SendMailRequest{To: "mini/mini-free", Body: "by name"}))(t)
	if m2.To != anop.Address || m2.Recipient != anop.Address {
		t.Fatalf("authority mail by name across projects: %+v", m2)
	}
	// Both are stored as addresses: the sessions' inboxes hold them.
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, at(r.owner, "", nop.Address), orchapi.InboxQuery{}))(t); !hasMail(in, m1.ID) {
			return fmt.Errorf("no-project session's inbox %+v", in)
		}
		return nil
	})
	in := mustf(orchclient.Inbox(ctx, at(a.owner, "", anop.Address), orchapi.InboxQuery{}))(t)
	if !hasMail(in, m2.ID) {
		t.Fatalf("authority no-project session's inbox %+v", in)
	}
	for _, m := range in {
		if m.ID == m2.ID && m.To != anop.Address {
			t.Fatalf("stored to %q, want the address %s", m.To, anop.Address)
		}
	}
	// A guest naming the authority's no-project session gets a missing
	// name's answer.
	gs := at(g.owner, "harbor", vs.Address)
	_, want := orchclient.Send(ctx, gs, orchapi.SendMailRequest{To: "mini/nobody-here", Body: "x"})
	_, err := orchclient.Send(ctx, gs, orchapi.SendMailRequest{To: "mini/mini-free", Body: "x"})
	if !api.IsCode(err, api.CodeNotFound) || errBody(err, "mini/mini-free") != errBody(want, "mini/nobody-here") ||
		strings.Contains(errBody(err, "mini/mini-free"), anop.Address) {
		t.Fatalf("guest by name: %v (missing: %v)", err, want)
	}
}

// A replica's token downgraded from the whole project to one area counts
// at the next send, not the next heartbeat: mail to its no-project session
// is filed in that area. And a new no-project session is reachable without
// waiting for the periodic heartbeat (its register kicks one).
func TestMailAcrossProjectsTokenNow(t *testing.T) {
	quietPresence(t) // the periodic beat is an hour: only kicks heartbeat
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	register(t, r, "harbor/solver", "bea-own")
	ms := register(t, a, "harbor", "mini-sess")
	am := at(a.owner, "harbor", ms.Address)
	heartbeat(t, r)

	nop := registerNone(t, r, "bea-late")
	eventually(t, 5*time.Second, func() error {
		_, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: nop.Address, Body: "found without a beat"})
		return err
	})

	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	m := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: nop.Address, Body: "after the downgrade"}))(t)
	if m.Area != "solver" {
		t.Fatalf("after the downgrade, mail filed in %q, want solver", m.Area)
	}
}

// A mail id names its own project: $ZM_PROJECT does not narrow ack, reply or
// thread (a farm job runs with ZM_PROJECT=harbor and is mailed in zm),
// while an explicit -P still does.
func TestMailIDNotNarrowedByEnvProject(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "sengi"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "sengi"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "sengi", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	job := register(t, r, "harbor", "bea-job")
	ss := register(t, a, "sengi", "mini-sengi")
	heartbeat(t, r)
	sm := at(a.owner, "sengi", ss.Address)
	m1 := mustf(orchclient.Send(ctx, sm, orchapi.SendMailRequest{To: job.Address, Body: "a note filed in sengi"}))(t)
	m2 := mustf(orchclient.Send(ctx, sm, orchapi.SendMailRequest{To: job.Address, Body: "another"}))(t)

	env := at(r.owner, "harbor", job.Address)
	env.ProjectFrom = "env"
	flag := at(r.owner, "harbor", job.Address)
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, at(r.owner, "", job.Address), orchapi.InboxQuery{}))(t); !hasMail(in, m2.ID) {
			return fmt.Errorf("job's inbox %+v", in)
		}
		return nil
	})
	if _, err := orchclient.Ack(ctx, flag, m1.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("ack with -P harbor of a sengi mail: %v, want not_found", err)
	}
	if th := mustf(orchclient.Thread(ctx, env, m1.ID))(t); !hasMail(th, m1.ID) {
		t.Fatalf("thread with ZM_PROJECT=harbor %+v", th)
	}
	if ak := mustf(orchclient.Ack(ctx, env, m1.ID))(t); ak.ID != m1.ID || !ak.Acked {
		t.Fatalf("ack with ZM_PROJECT=harbor %+v", ak)
	}
	if rep := mustf(orchclient.Reply(ctx, env, m2.ID, orchapi.ReplyRequest{Body: "on it"}))(t); rep.Recipient != ss.Address || rep.Project != m2.Project {
		t.Fatalf("reply with ZM_PROJECT=harbor %+v (want to %s in %s)", rep, ss.Address, m2.Project)
	}
}
