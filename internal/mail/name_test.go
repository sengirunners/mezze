package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// namedSessions answers session names (<machine>/<name>) the way the
// session module does (orchcore.SessionNames): by the live session that has
// the name now.
type namedSessions struct {
	orchcore.SessionService
	f     *fakeSessions
	names map[string]string // <machine>/<name> -> address
}

func (n namedSessions) ResolveName(ctx context.Context, ref string) (orchapi.Session, bool) {
	if !strings.Contains(ref, "/") {
		ref = "mini/" + ref
	}
	if a, ok := n.names[ref]; ok {
		if ss, ok := n.f.Lookup(ctx, a); ok && ss.State != orchapi.StateEnded {
			return ss, true
		}
	}
	return orchapi.Session{}, false
}

// SPEC 8 "Names": mail to <machine>/<name> stores the address the name
// resolved to, so the name passing to another session later never
// redirects it; a name of nothing, of an ended session or of a hidden one
// answers exactly like a missing session.
func TestMailToSessionName(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateIdle)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "solver", true, orchapi.StateIdle)
	f.sess.add("mini/eeeeeeee", f.harbor.ID, "solver", true, orchapi.StateEnded)
	f.sess.add("mini/hhhhhhhh", f.harbor.ID, "billing", true, orchapi.StateIdle)
	ns := namedSessions{SessionService: f.sess, f: f.sess, names: map[string]string{
		"mini/auth-fix": "mini/aaaaaaaa", "mini/old-one": "mini/eeeeeeee", "mini/billing-bot": "mini/hhhhhhhh",
		"mini/frontend": "mini/aaaaaaaa"}}
	orchcore.Wire(f.d).Sessions = ns

	m := must(orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "mini/Auth-Fix", Body: "first"}))
	if m.To != "mini/aaaaaaaa" || m.Recipient != "mini/aaaaaaaa" || m.Area != "solver" {
		t.Fatalf("mail %+v", m)
	}
	// A session address is never read as a name, even when a name lookup
	// would answer for the string (a non-UUID runtime id gives one such as
	// mini/frontend; its row may be gone).
	if _, err := orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "mini/frontend", Body: "x"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an address read as a name: %v", err)
	}
	ns.names["mini/auth-fix"] = "mini/bbbbbbbb" // the name passes to another session
	m2 := must(orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "mini/auth-fix", Body: "second"}))
	if m2.Recipient != "mini/bbbbbbbb" {
		t.Fatalf("second mail %+v", m2)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/aaaaaaaa", ""), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != m.ID {
		t.Fatalf("the first session's inbox %+v", in)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/bbbbbbbb", ""), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != m2.ID {
		t.Fatalf("the second session's inbox %+v", in)
	}

	// A guest of solver: a name of nothing, of an ended session and of a
	// session in billing all answer the same, naming the caller's input.
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor/solver", "flag"
	_, missing := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/nobody", Body: "x"})
	want := wantCode(t, missing, api.CodeNotFound)
	for _, to := range []string{"mini/old-one", "mini/billing-bot"} {
		_, err := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: to, Body: "x"})
		got := wantCode(t, err, api.CodeNotFound)
		if got.Message != strings.Replace(want.Message, "mini/nobody", to, 1) || got.Hint != want.Hint {
			t.Fatalf("%s: %+v, a missing name: %+v", to, got, want)
		}
	}
	// A bare name is a machine; the answer points at the session.
	_, err := orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "auth-fix", Body: "x"})
	if e := wantCode(t, err, api.CodeNotFound); !strings.Contains(e.Hint, "mini/auth-fix") {
		t.Fatalf("bare name: %+v", e)
	}
	// ... but never to a sender who may not see that session.
	_, err = orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "billing-bot", Body: "x"})
	if e := wantCode(t, err, api.CodeNotFound); strings.Contains(e.Hint, "billing-bot") {
		t.Fatalf("bare name of a hidden session: %+v", e)
	}
	// Another machine's names are not resolved here.
	_, err = orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "far/auth-fix", Body: "x"})
	wantCode(t, err, api.CodeNotFound)
	// The guest reaches a session of its own area by name.
	if gm := must(orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/auth-fix", Body: "from the guest"})); gm.Recipient == "" {
		t.Fatalf("guest mail %+v", gm)
	}
}
