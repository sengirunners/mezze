package mail

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// ---- fakes for the other modules (they are wired through core.Deps) ----

type fakeSessions struct {
	mu     sync.Mutex
	m      map[string]*orchapi.Session
	nudged []string
}

func (f *fakeSessions) add(addr, project, area string, managed bool, st orchapi.SessionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, _ := api.ParseAddress(addr)
	f.m[addr] = &orchapi.Session{Address: addr, ID: a.Session, Machine: a.Machine, Project: project, Area: area, Managed: managed, State: st}
}

func (f *fakeSessions) state(addr string) orchapi.SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[addr].State
}

func (f *fakeSessions) Lookup(_ context.Context, addr string) (orchapi.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.m[addr]; ok {
		return *s, true
	}
	return orchapi.Session{}, false
}

func (f *fakeSessions) Visible(ctx context.Context, sc authz.Scope, addr string) bool {
	s, ok := f.Lookup(ctx, addr)
	return ok && sc.Can(authz.Read, authz.Object{Project: s.Project, Area: s.Area})
}

func (f *fakeSessions) Live(ctx context.Context, addr string) bool {
	s, ok := f.Lookup(ctx, addr)
	return ok && s.State != orchapi.StateEnded
}

func (f *fakeSessions) SetState(_ context.Context, addr string, req orchapi.StateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.m[addr]; ok {
		s.State, s.Note = req.State, req.Note
		return nil
	}
	return api.NotFound("session", addr)
}

func (f *fakeSessions) Nudge(_ context.Context, addr, line string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if line != NudgeLine {
		panic("mail typed something other than the nudge line")
	}
	if s, ok := f.m[addr]; ok && s.Managed && s.State == orchapi.StateIdle {
		f.nudged = append(f.nudged, addr)
		return true, nil
	}
	return false, nil
}

func (f *fakeSessions) ManagedLive(context.Context) int { return 0 }

func (f *fakeSessions) RestartBlockers(context.Context) []orchapi.Session { return nil }

func (f *fakeSessions) IdleManaged(_ context.Context, project string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for a, s := range f.m {
		if s.Project == project && s.Managed && s.State == orchapi.StateIdle {
			out = append(out, a)
		}
	}
	return out
}

func (f *fakeSessions) takeNudged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nudged
	f.nudged = nil
	return n
}

type fakeItem struct{ area, holder, creator string }

type fakeWork struct {
	mu      sync.Mutex
	items   map[string]fakeItem // project|id
	failing []orchapi.TrackerFailure
}

// FailingTrackers implements orchcore.TrackerHealth like the work module:
// whole-project readers only.
func (w *fakeWork) FailingTrackers(sc authz.Scope, atLeast time.Duration) []orchapi.TrackerFailure {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []orchapi.TrackerFailure
	for _, f := range w.failing {
		if sc.WholeProject(f.Project) && time.Since(f.Since) >= atLeast {
			out = append(out, f)
		}
	}
	return out
}

func (w *fakeWork) add(project, id string, it fakeItem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.items[project+"|"+id] = it
}

func (w *fakeWork) MailTarget(_ context.Context, sub *authz.Subject, project, id string) (string, string, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	it, ok := w.items[project+"|"+id]
	if !ok || !sub.Can(authz.Read, authz.Object{Project: project, Area: it.area}) {
		return "", "", "", api.NotFound("work item", id)
	}
	switch {
	case it.holder != "":
		return it.holder, "holder of " + id, it.area, nil
	case it.creator != "":
		return it.creator, "creator of " + id, it.area, nil
	}
	return "", "", it.area, api.Errorf(api.CodeConflict, "nobody holds or created %s", id) // like work: the area comes along
}

func (w *fakeWork) BindLease(context.Context, *authz.Subject, string, string, string) (orchapi.Lease, error) {
	return orchapi.Lease{}, api.ErrNotImplemented("x")
}
func (w *fakeWork) SessionEnded(context.Context, string) error { return nil }

// OwnerOf is the item's holder (its live lease), like work.
func (w *fakeWork) OwnerOf(_ context.Context, sub *authz.Subject, project, id string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	it, ok := w.items[project+"|"+id]
	if !ok || !sub.Can(authz.Read, authz.Object{Project: project, Area: it.area}) {
		return "", api.NotFound("work item", id)
	}
	return it.holder, nil
}

func (w *fakeWork) ClaimItem(context.Context, *authz.Subject, string, string, orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	return orchapi.ClaimResponse{}, nil
}

func (w *fakeWork) EndItemLease(context.Context, string, string, string, string) error { return nil }
func (w *fakeWork) ItemLeased(context.Context, string, string) (bool, error)           { return false, nil }

type fakeForwarder struct{ got []orchapi.Mail }

func (f *fakeForwarder) Forward(_ context.Context, machine string, m orchapi.Mail) (orchapi.Mail, error) {
	f.got = append(f.got, m)
	return m, nil
}

// ---- fixture ----

type fixture struct {
	t        *testing.T
	d        *core.Deps
	s        *Service
	sess     *fakeSessions
	work     *fakeWork
	h        http.Handler // network: token only
	local    http.Handler // the Unix socket
	harbor   api.Project
	side     api.Project
	guestTok string
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now()}
	f := &fixture{t: t, d: d, sess: &fakeSessions{m: map[string]*orchapi.Session{}}, work: &fakeWork{items: map[string]fakeItem{}}}
	f.s = New(d)
	f.clock = time.Now().UTC().Truncate(time.Millisecond)
	f.s.now = func() time.Time { return f.clock }
	orchcore.Wire(d).Mail, orchcore.Wire(d).Work, orchcore.Wire(d).Sessions = f.s, f.work, f.sess
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, addr string) bool { return d.Visible(ctx, sc, addr) })
	r := core.NewRouter()
	f.s.Routes(r)
	f.h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		r.ServeHTTP(w, req.WithContext(d.Authenticate(req)))
	})
	f.local = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.h.ServeHTTP(w, req.WithContext(core.WithLocal(req.Context())))
	})

	owner := reg.Local("")
	if f.harbor, err = reg.Create(ctx, owner, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "billing"} {
		if _, err := reg.AddArea(ctx, owner, "harbor", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	if f.side, err = reg.Create(ctx, owner, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, owner, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	f.guestTok = tok.Secret
	if _, err := reg.Mint(ctx, owner, api.MintTokenRequest{As: "bea", Project: "side", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// as is a socket client acting as addr ("" = the human) in project.
func (f *fixture) as(addr, proj string) *client.Client {
	c := client.WithHandler(f.local)
	c.As = addr
	if proj != "" {
		c.Project, c.ProjectFrom = proj, "flag"
	}
	return c
}

// guest is vida, a guest of harbor/solver, over the network.
func (f *fixture) guest() *client.Client {
	c := client.WithHandler(f.h)
	c.Token = f.guestTok
	return c
}

// must panics on err (a failed test with a stack); it takes a call's two results directly.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func wantCode(t *testing.T, err error, code string) *api.Error {
	t.Helper()
	e, ok := err.(*api.Error)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

var ctx = context.Background()

// ---- tests ----

func TestSendToSessionInboxAck(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateIdle)
	m := must(orchclient.Send(ctx, f.as("", "harbor/solver"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "review the migration"}))
	if m.Recipient != "mini/aaaaaaaa" || m.Via != "direct" || m.Area != "solver" || m.From != "human:ana" || m.Needs != orchapi.NeedsAny ||
		m.Kind != orchapi.MailKindNote || m.Project != f.harbor.ID {
		t.Fatalf("mail %+v", m)
	}
	if n := f.sess.takeNudged(); len(n) != 1 || n[0] != "mini/aaaaaaaa" {
		t.Fatalf("nudged %v", n)
	}
	s := f.as("mini/aaaaaaaa", "")
	in := must(orchclient.Inbox(ctx, s, orchapi.InboxQuery{}))
	if len(in) != 1 || in[0].ID != m.ID {
		t.Fatalf("inbox %+v", in)
	}
	n := must(orchclient.Notice(ctx, s))
	if n.Unread != 1 || n.Line != "zm: 1 unread in harbor from human:ana. Run: zm inbox" {
		t.Fatalf("notice %+v", n)
	}
	// the human's own inbox does not show mail to a session
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("human inbox %+v", in)
	}
	a := must(orchclient.Ack(ctx, s, short(m.ID)))
	if !a.Acked || a.AckedBy != "mini/aaaaaaaa" || a.AckedAt == nil {
		t.Fatalf("ack %+v", a)
	}
	if a2 := must(orchclient.Ack(ctx, s, m.ID)); !a2.Acked { // idempotent for the taker
		t.Fatalf("second ack %+v", a2)
	}
	if in := must(orchclient.Inbox(ctx, s, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("inbox after ack %+v", in)
	}
	if n := must(orchclient.Notice(ctx, s)); n.Line != "" || n.Unread != 0 {
		t.Fatalf("notice after ack %+v", n)
	}
	if in := must(orchclient.Inbox(ctx, s, orchapi.InboxQuery{All: true})); len(in) != 1 || in[0].AckedBy != "mini/aaaaaaaa" {
		t.Fatalf("inbox all %+v", in)
	}
	// events: sent then acked, in the mail's project and area
	evs := must(f.d.Events.Since(ctx, f.d.System(), "", events.Filter{Types: []string{"dev.zm.mail."}}, 0))
	if len(evs) != 2 || evs[0].Type != orchapi.EventMailSent || evs[1].Type != orchapi.EventMailAcked ||
		evs[0].Project != f.harbor.ID || evs[0].Area != "solver" || evs[0].Subject != m.ID {
		t.Fatalf("events %+v", evs)
	}
	// sent
	if sent := must(orchclient.Sent(ctx, f.as("", ""), "", 0)); len(sent) != 1 || sent[0].ID != m.ID {
		t.Fatalf("sent %+v", sent)
	}
}

func TestSendValidation(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	for _, tc := range []struct {
		req  orchapi.SendMailRequest
		code string
	}{
		{orchapi.SendMailRequest{To: "human:ana", Body: "  "}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "all", Body: "x"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Kind: "announce"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Needs: "agent"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "owners", Body: "x", Needs: "agent"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Needs: "bogus"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Ref: "not a ref"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "Bad Address", Body: "x"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x"}, api.CodeNotFound},
		{orchapi.SendMailRequest{To: "human:nobody", Body: "x"}, api.CodeNotFound},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Area: "nope"}, api.CodeNotFound},
		{orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x", Kind: "nudge"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "all", Body: "x", Kind: "nudge"}, api.CodeBadRequest},
		{orchapi.SendMailRequest{To: "human:ana", Body: "x", Kind: "nudge", Needs: "human"}, api.CodeBadRequest},
	} {
		if _, err := orchclient.Send(ctx, c, tc.req); err == nil {
			t.Fatalf("%+v accepted", tc.req)
		} else {
			wantCode(t, err, tc.code)
		}
	}
	// No project resolves: refused, never filed into a default.
	_, err := orchclient.Send(ctx, f.as("", ""), orchapi.SendMailRequest{To: "human:ana", Body: "x"})
	wantCode(t, err, api.CodeBadRequest)
	// Mail to a human defaults to needs=human.
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:ana", Body: "x"}))
	if m.Needs != orchapi.NeedsHuman || m.Recipient != "human:ana" {
		t.Fatalf("%+v", m)
	}
	// A nudge to a person or the owners needs nothing.
	for _, to := range []string{"human:ana", "owners"} {
		n := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: to, Body: "prod is down", Kind: "nudge"}))
		if n.Kind != orchapi.MailKindNudge || n.Needs != orchapi.NeedsAny {
			t.Fatalf("%+v", n)
		}
	}
}

func TestCrossMachine(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	_, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "studio/aaaaaaaa", Body: "x"})
	if e := wantCode(t, err, api.CodeNotFound); !strings.Contains(e.Hint, "another machine") {
		t.Fatalf("hint %q", e.Hint)
	}
	_, err = orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "machine:studio", Body: "x"})
	wantCode(t, err, api.CodeNotFound)
	fw := &fakeForwarder{}
	f.s.SetForwarder(fw)
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "studio/aaaaaaaa", Body: "x"}))
	if len(fw.got) != 1 || m.Recipient != "studio/aaaaaaaa" || !strings.Contains(m.Via, "forwarded to studio") {
		t.Fatalf("forwarded %+v %+v", m, fw.got)
	}
}

func TestGroupMailboxFirstAckWins(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateIdle)
	f.sess.add("mini/cccccccc", f.side.ID, "", true, orchapi.StateIdle) // other project: not its group mail
	m := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "someone take the deploy"}))
	if m.Recipient != "machine:mini" || m.Via != "group" {
		t.Fatalf("%+v", m)
	}
	if n := f.sess.takeNudged(); len(n) != 1 || n[0] != "mini/bbbbbbbb" {
		t.Fatalf("nudged %v (want the one idle session of the project)", n)
	}
	a, b, c := f.as("mini/aaaaaaaa", ""), f.as("mini/bbbbbbbb", ""), f.as("mini/cccccccc", "")
	for _, x := range []*client.Client{a, b} {
		if n := must(orchclient.Notice(ctx, x)); !strings.Contains(n.Line, "(1 group)") {
			t.Fatalf("notice %q", n.Line)
		}
	}
	if in := must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("other project's session sees group mail: %+v", in)
	}
	if _, err := orchclient.Ack(ctx, c, m.ID); err == nil {
		t.Fatal("a session of another project took the group mail")
	}
	if r := must(orchclient.Ack(ctx, a, m.ID)); !r.Acked || r.AckedBy != "mini/aaaaaaaa" {
		t.Fatalf("first ack %+v", r)
	}
	if r := must(orchclient.Ack(ctx, b, m.ID)); r.Acked || r.AckedBy != "mini/aaaaaaaa" {
		t.Fatalf("second ack %+v", r)
	}
	if in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("taken group mail still in inbox: %+v", in)
	}
}

func TestWorkItemRouting(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateRunning)
	f.work.add(f.harbor.ID, "local:held01", fakeItem{area: "solver", holder: "mini/aaaaaaaa"})
	f.work.add(f.harbor.ID, "local:made01", fakeItem{area: "billing", creator: "human:ana"})
	f.work.add(f.harbor.ID, "bd:orphan-1", fakeItem{area: "solver"})
	f.work.add(f.harbor.ID, "local:hidden", fakeItem{area: "billing", holder: "mini/aaaaaaaa"})
	c := f.as("", "harbor")
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "local:held01", Body: "PR is up"}))
	if m.Recipient != "mini/aaaaaaaa" || m.Via != "holder of local:held01" || m.Ref != "local:held01" || m.Area != "solver" {
		t.Fatalf("%+v", m)
	}
	m = must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "local:made01", Body: "x", Area: "solver"}))
	if m.Recipient != "human:ana" || m.Via != "creator of local:made01" || m.Area != "billing" {
		t.Fatalf("the item's area wins: %+v", m)
	}
	_, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "bd:orphan-1", Body: "x"})
	if e := wantCode(t, err, api.CodeConflict); !strings.Contains(e.Hint, "zm who bd:orphan-1") {
		t.Fatalf("hint %q", e.Hint)
	}
	// A ref to an item nobody holds takes the item's area as work located it
	// (bd:orphan-1 is in solver by its labels, not by an id glob).
	m = must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:ana", Body: "about the orphan", Ref: "bd:orphan-1"}))
	if m.Area != "solver" {
		t.Fatalf("orphan ref area: %+v", m)
	}
	// machine:<m> with a ref held here goes to the holder.
	m = must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "machine:mini", Body: "x", Ref: "local:held01"}))
	if m.Recipient != "mini/aaaaaaaa" || m.Area != "solver" {
		t.Fatalf("%+v", m)
	}
	// A ref sets the area when none is named.
	m = must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:ana", Body: "x", Ref: "local:made01"}))
	if m.Area != "billing" {
		t.Fatalf("ref area: %+v", m)
	}

	// The guest: a hidden item answers exactly like a missing one.
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor", "flag"
	_, eh := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "local:hidden", Body: "x", Area: "solver"})
	_, em := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "local:nope01", Body: "x", Area: "solver"})
	if a, b := wantCode(t, eh, api.CodeNotFound), wantCode(t, em, api.CodeNotFound); strings.ReplaceAll(a.Message, "local:hidden", "X") != strings.ReplaceAll(b.Message, "local:nope01", "X") {
		t.Fatalf("%q != %q", a.Message, b.Message)
	}
	_, eh = orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "human:ana", Body: "x", Area: "solver", Ref: "local:hidden"})
	wantCode(t, eh, api.CodeNotFound)
	// The guest's holder lookup of a visible item it may write works; the holder is redacted
	// because the fake hides sessions outside the guest's scope only (this one is in solver).
	m = must(orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "local:held01", Body: "from the guest"}))
	if m.Recipient != "mini/aaaaaaaa" || m.From != "human:vida" {
		t.Fatalf("%+v", m)
	}
}

func TestNeedsHuman(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	// asks (the approvals module's sends): an agent's other mail to a person
	// is a report (report_test.go)
	sub := f.d.Projects.Local("mini/aaaaaaaa")
	asks := fakeAsks{"01ASKAAAAAAAAAAAAAAAAAAAAA": true, "01ASKBBBBBBBBBBBBBBBBBBBBB": true}
	f.s.SetAsks(asks)
	m := must(f.s.Send(ctx, sub, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "owners", Body: "deploy to prod?", ID: "01ASKAAAAAAAAAAAAAAAAAAAAA"}))
	if m.Needs != orchapi.NeedsHuman || m.Recipient != "owners" {
		t.Fatalf("%+v", m)
	}
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateNeedsHuman {
		t.Fatalf("sender state %s", st)
	}
	// a second needs=human mail directly to another session: never shown to it
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateIdle)
	m2 := must(f.s.Send(ctx, sub, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "ask your human", Needs: "human", ID: "01ASKBBBBBBBBBBBBBBBBBBBBB"}))
	if n := f.sess.takeNudged(); len(n) != 0 {
		t.Fatalf("needs=human mail nudged %v", n)
	}
	b := f.as("mini/bbbbbbbb", "")
	if in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("an agent sees needs=human mail: %+v", in)
	}
	if _, err := orchclient.Ack(ctx, b, m2.ID); err == nil {
		t.Fatal("an agent acked needs=human mail")
	} else {
		wantCode(t, err, api.CodeForbidden)
	}
	h := f.as("", "")
	in := must(orchclient.Inbox(ctx, h, orchapi.InboxQuery{}))
	if len(in) != 2 {
		t.Fatalf("human inbox %+v", in)
	}
	if n := must(orchclient.Notice(ctx, h)); !strings.Contains(n.Line, "2 unread in harbor (1 owners)") {
		t.Fatalf("notice %q", n.Line)
	}
	// the guest is no owner: owners mail is not in its inbox
	if in := must(orchclient.Inbox(ctx, f.guest(), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("guest inbox %+v", in)
	}
	must(orchclient.Ack(ctx, h, m.ID))
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateNeedsHuman {
		t.Fatalf("still one open needs=human mail, state %s", st)
	}
	r := must(orchclient.Reply(ctx, h, m2.ID, orchapi.ReplyRequest{Body: "yes, go"}))
	if r.Recipient != "mini/aaaaaaaa" || r.ReplyTo != m2.ID {
		t.Fatalf("reply %+v", r)
	}
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateIdle {
		t.Fatalf("answered sender state %s", st)
	}
	if n := f.sess.takeNudged(); len(n) != 1 || n[0] != "mini/aaaaaaaa" {
		t.Fatalf("answered sender not woken: %v", n)
	}
}

func TestHumanRecipientScope(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:vida", Body: "x", Area: "solver"}))
	_, eh := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:vida", Body: "x", Area: "billing"})
	_, ec := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:bea", Body: "x", Area: "solver"})
	_, em := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:nobody", Body: "x", Area: "solver"})
	for _, e := range []error{eh, ec, em} {
		wantCode(t, e, api.CodeNotFound)
	}
	v := must(orchclient.Inbox(ctx, f.guest(), orchapi.InboxQuery{}))
	if len(v) != 1 || v[0].Recipient != "human:vida" {
		t.Fatalf("guest inbox %+v", v)
	}
	if n := must(orchclient.Notice(ctx, f.guest())); n.Line != "zm: 1 unread in harbor from human:ana. Run: zm inbox" {
		t.Fatalf("guest notice %q", n.Line)
	}
}

func TestReplyAndThread(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/cccccccc", f.harbor.ID, "", true, orchapi.StateRunning)
	a, b, c := f.as("mini/aaaaaaaa", "harbor"), f.as("mini/bbbbbbbb", "harbor"), f.as("mini/cccccccc", "harbor")
	m := must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "q1", Ref: ""}))
	r := must(orchclient.Reply(ctx, b, m.ID, orchapi.ReplyRequest{Body: "a1"}))
	if r.Recipient != "mini/aaaaaaaa" || r.ReplyTo != m.ID {
		t.Fatalf("reply %+v", r)
	}
	if got := must(orchclient.GetMail(ctx, b, m.ID)); got.AckedBy != "mini/bbbbbbbb" {
		t.Fatalf("reply did not take the parent: %+v", got)
	}
	r2 := must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "q2", ReplyTo: short(r.ID)}))
	th := must(orchclient.Thread(ctx, c, r2.ID))
	if len(th) != 3 || th[0].ID != m.ID || th[1].ID != r.ID || th[2].ID != r2.ID {
		t.Fatalf("thread %+v", th)
	}
	if _, err := orchclient.Reply(ctx, c, m.ID, orchapi.ReplyRequest{Body: "butting in"}); err == nil {
		t.Fatal("non-party replied")
	} else {
		wantCode(t, err, api.CodeForbidden)
	}
	if _, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "x", ReplyTo: m.ID}); err == nil {
		t.Fatal("non-party threaded onto mail")
	}
	if _, err := orchclient.Ack(ctx, c, m.ID); err == nil {
		t.Fatal("non-party acked")
	}
	// reply_to must be in the same project
	_, err := orchclient.Send(ctx, f.as("mini/bbbbbbbb", "side"), orchapi.SendMailRequest{To: "machine:mini", Body: "x", ReplyTo: m.ID})
	wantCode(t, err, api.CodeNotFound)
}

func TestAnnounce(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateIdle)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", false, orchapi.StateRunning)
	m := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "all", Kind: "announce", Body: "main is frozen"}))
	if m.Recipient != "all" || m.Via != "announce" {
		t.Fatalf("%+v", m)
	}
	if n := f.sess.takeNudged(); len(n) != 0 {
		t.Fatalf("announcement nudged %v", n)
	}
	for _, addr := range []string{"mini/aaaaaaaa", "mini/bbbbbbbb"} {
		c := f.as(addr, "")
		if n := must(orchclient.Notice(ctx, c)); !strings.Contains(n.Line, "1 announce") {
			t.Fatalf("%s notice %q", addr, n.Line)
		}
		if in := must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{})); len(in) != 1 {
			t.Fatalf("%s inbox %+v", addr, in)
		}
		if in := must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{})); len(in) != 0 {
			t.Fatalf("%s saw the announcement twice", addr)
		}
	}
	if got := must(orchclient.GetMail(ctx, f.as("", ""), m.ID)); got.AckedBy != "" {
		t.Fatalf("announcement acked: %+v", got)
	}
}

func TestGuestSeesOnlyItsArea(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/hhhhhhhh", f.harbor.ID, "", true, orchapi.StateRunning) // project-level: hidden from the guest
	f.sess.add("mini/ssssssss", f.harbor.ID, "solver", true, orchapi.StateRunning)
	var hidden []string
	for _, p := range []string{"harbor/billing", "harbor", "side"} {
		m := must(orchclient.Send(ctx, f.as("", p), orchapi.SendMailRequest{To: "human:ana", Body: "HIDDEN " + p}))
		hidden = append(hidden, m.ID)
	}
	vis := must(orchclient.Send(ctx, f.as("mini/hhhhhhhh", "harbor/solver"), orchapi.SendMailRequest{To: "mini/ssssssss", Body: "visible"}))
	g := f.guest()
	all := must(orchclient.Mail(ctx, g, orchapi.MailQuery{}))
	if len(all) != 1 || all[0].ID != vis.ID || all[0].From != events.Someone || all[0].Recipient != "mini/ssssssss" {
		t.Fatalf("guest mail %+v", all)
	}
	if q := must(orchclient.Mail(ctx, g, orchapi.MailQuery{From: "mini/hhhhhhhh"})); len(q) != 0 {
		t.Fatalf("filter on a hidden session revealed %+v", q)
	}
	_, em := orchclient.GetMail(ctx, g, "01ZZZZZZZZZZZZZZZZZZZZZZZZ")
	me := wantCode(t, em, api.CodeNotFound)
	for _, id := range hidden {
		for _, call := range []func(string) error{
			func(id string) error { _, err := orchclient.GetMail(ctx, g, id); return err },
			func(id string) error { _, err := orchclient.Ack(ctx, g, id); return err },
			func(id string) error { _, err := orchclient.Thread(ctx, g, id); return err },
			func(id string) error {
				_, err := orchclient.Reply(ctx, g, id, orchapi.ReplyRequest{Body: "x"})
				return err
			},
		} {
			e := wantCode(t, call(id), api.CodeNotFound)
			if strings.ReplaceAll(e.Message, id, "X") != strings.ReplaceAll(me.Message, "01ZZZZZZZZZZZZZZZZZZZZZZZZ", "X") {
				t.Fatalf("hidden %q != missing %q", e.Message, me.Message)
			}
		}
	}
	// ?project= naming a hidden project answers like a missing one; notice never fails.
	_, eh := orchclient.Inbox(ctx, g, orchapi.InboxQuery{Project: "side"})
	_, en := orchclient.Inbox(ctx, g, orchapi.InboxQuery{Project: "nope"})
	if a, b := wantCode(t, eh, api.CodeNotFound), wantCode(t, en, api.CodeNotFound); strings.ReplaceAll(a.Message, "side", "X") != strings.ReplaceAll(b.Message, "nope", "X") {
		t.Fatalf("%q vs %q", a.Message, b.Message)
	}
	// Live events: the guest's subscription gets the solver mail with the hidden sender redacted.
	ch, cancel := f.d.Events.Subscribe(&authz.Subject{Scope: mustGuestScope(t, f), Address: "human:vida"}, events.Filter{Types: []string{"dev.zm.mail."}}, 16)
	defer cancel()
	must(orchclient.Send(ctx, f.as("", "side"), orchapi.SendMailRequest{To: "human:ana", Body: "HIDDEN live"}))
	must(orchclient.Send(ctx, f.as("mini/hhhhhhhh", "harbor/solver"), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:vida", Body: "live"}))
	select {
	case e := <-ch:
		if strings.Contains(string(e.Data), "HIDDEN") || strings.Contains(string(e.Data), "hhhhhhhh") || !strings.Contains(string(e.Data), events.Someone) {
			t.Fatalf("event %s", e.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}

func mustGuestScope(t *testing.T, f *fixture) authz.Scope {
	sub, err := f.d.Projects.FromToken(ctx, f.guestTok, true)
	if err != nil {
		t.Fatal(err)
	}
	return sub.Scope
}

func TestOrphanSweep(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	m := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "x"}))
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.sess.SetState(ctx, "mini/aaaaaaaa", orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.sweepOrphans(ctx); err != nil { // too young
		t.Fatal(err)
	}
	b := f.as("mini/bbbbbbbb", "")
	if in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("orphaned too early: %+v", in)
	}
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	for i := 0; i < 2; i++ { // copied once
		if err := f.s.sweepOrphans(ctx); err != nil {
			t.Fatal(err)
		}
	}
	in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{}))
	if len(in) != 1 || in[0].Recipient != "machine:mini" || in[0].Via != "orphan of mini/aaaaaaaa" || in[0].ID == m.ID {
		t.Fatalf("orphan copy %+v", in)
	}
	must(orchclient.Ack(ctx, b, in[0].ID))
	if got := must(orchclient.GetMail(ctx, b, m.ID)); got.AckedBy != "mini/bbbbbbbb" {
		t.Fatalf("taking the copy did not settle the original: %+v", got)
	}
}

// An ask notice to an ended session stays on it: a machine has nobody to
// act on it. An ordinary note to the same session is still copied.
func TestOrphanSweepSkipsAskNotices(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	sys := f.d.System()
	receipt := must(f.s.Send(orchcore.AsAskNotice(ctx), sys, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "OWNER DECISION (approved)"}))
	must(f.s.Send(ctx, sys, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "ordinary"}))
	if err := f.sess.SetState(ctx, "mini/aaaaaaaa", orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	b := f.as("mini/bbbbbbbb", "")
	in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{}))
	if len(in) != 1 || in[0].Via != "orphan of mini/aaaaaaaa" || in[0].Body != "ordinary" {
		t.Fatalf("machine inbox %+v: want only the ordinary note's copy", in)
	}
	if got := must(orchclient.GetMail(ctx, f.as("mini/aaaaaaaa", ""), receipt.ID)); got.AckedAt != nil {
		t.Fatalf("receipt %+v", got)
	}
}

// Taking one copy settles its sibling in the same statement, so the
// sibling's acked state must reach replicas (they learn only from events) and
// the loser of an ack race must be told when it was taken.
func TestSettledSiblingPublishesAcked(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	m := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "x"}))
	if err := f.sess.SetState(ctx, "mini/aaaaaaaa", orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	b := f.as("mini/bbbbbbbb", "")
	in := must(orchclient.Inbox(ctx, b, orchapi.InboxQuery{}))
	if len(in) != 1 {
		t.Fatalf("orphan copy %+v", in)
	}
	cp := in[0]
	must(orchclient.Ack(ctx, b, cp.ID))
	acked := map[string]bool{}
	for _, e := range must(f.d.Events.Since(ctx, f.d.System(), "", events.Filter{Types: []string{orchapi.EventMailAcked}}, 0)) {
		acked[e.Subject] = true
	}
	if !acked[cp.ID] || !acked[m.ID] {
		t.Fatalf("mail.acked events for %v: want the copy %s and the settled original %s", acked, cp.ID, m.ID)
	}
	// The loser of the race is told when it was taken.
	if r := must(orchclient.Ack(ctx, f.as("mini/aaaaaaaa", ""), cp.ID)); r.Acked || r.AckedBy != "mini/bbbbbbbb" || r.AckedAt == nil {
		t.Fatalf("losing ack %+v: want AckedAt set", r)
	}
}

// The other way round: the original's own session takes it after the sweep
// copied it, and the group copy's settling is published too.
func TestSettledCopyPublishesAcked(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	m := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "x"}))
	if err := f.sess.SetState(ctx, "mini/aaaaaaaa", orchapi.StateRequest{State: orchapi.StateEnded}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(OrphanAfter + time.Minute)
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	must(orchclient.Ack(ctx, f.as("mini/aaaaaaaa", ""), m.ID))
	acked := map[string]bool{}
	for _, e := range must(f.d.Events.Since(ctx, f.d.System(), "", events.Filter{Types: []string{orchapi.EventMailAcked}}, 0)) {
		acked[e.Subject] = true
	}
	if len(acked) != 2 || !acked[m.ID] {
		t.Fatalf("mail.acked events for %v: want the original and its group copy", acked)
	}
}

func TestIDSuffix(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:ana", Body: "x"}))
	if got := must(orchclient.GetMail(ctx, c, strings.ToLower(m.ID[len(m.ID)-6:]))); got.ID != m.ID {
		t.Fatalf("%+v", got)
	}
	_, err := orchclient.GetMail(ctx, c, m.ID[len(m.ID)-5:])
	wantCode(t, err, api.CodeNotFound)
}

func TestNoticeLineFits(t *testing.T) {
	long := strings.Repeat("p", 63)
	var ps []*projectCount
	for i := 0; i < 5; i++ {
		ps = append(ps, &projectCount{slug: long, n: 3, group: 1, announce: 1, h: 1})
	}
	from := []string{"mini/aaaaaaaa", "human:" + strings.Repeat("x", 60), "studio/bbbbbbbb", "human:ana"}
	for np := 1; np <= len(ps); np++ {
		l := noticeLine(15, ps[:np], from)
		if len(l) > orchapi.MaxNoticeLen || !strings.HasPrefix(l, "zm: ") || !strings.HasSuffix(l, "Run: zm inbox") {
			t.Fatalf("%d bytes: %q", len(l), l)
		}
	}
	got := noticeLine(3, []*projectCount{{slug: "harbor", n: 2, group: 1}, {slug: "side", n: 1}}, []string{"mini/9c0e11aa", "human:ana"})
	if got != "zm: 3 unread: harbor 2 (1 group), side 1; from mini/9c0e11aa, human:ana. Run: zm inbox" {
		t.Fatalf("%q", got)
	}
}

func TestInProcessSend(t *testing.T) {
	f := newFixture(t)
	// The ref is unknown to the work module: refused like any caller, never
	// stored without it (and never a success).
	_, err := f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "solver",
		orchapi.SendMailRequest{To: "owners", Body: "lease bd:solver-1 expired", Ref: "bd:solver-1"})
	wantCode(t, err, api.CodeNotFound)
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("refused mail was stored: %+v", in)
	}
	f.work.add(f.harbor.ID, "bd:solver-1", fakeItem{area: "solver", creator: "human:ana"})
	m, err := f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "",
		orchapi.SendMailRequest{To: "owners", Body: "lease bd:solver-1 expired", Ref: "bd:solver-1"})
	if err != nil || m.From != "machine:mini" || m.Needs != orchapi.NeedsHuman || m.Area != "solver" || m.Kind != orchapi.MailKindReport {
		t.Fatalf("%+v %v", m, err)
	}
	// zm's own note to people is a report (SPEC 9 "Reports")
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{Reports: true})); len(in) != 1 || in[0].ID != m.ID {
		t.Fatalf("owner reports %+v", in)
	}
}

// Regression: POST /v0/mail with reply_to must not let a party to any thread
// mail (or probe for) a session it cannot see; hidden and missing answer the
// same not_found, naming the caller's input.
func TestReplyToDoesNotBypassSessionVisibility(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/hhhhhhhh", f.harbor.ID, "", true, orchapi.StateRunning)        // project-level: hidden from the guest
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "billing", true, orchapi.StateRunning) // billing: hidden from the guest
	par := must(orchclient.Send(ctx, f.as("mini/hhhhhhhh", "harbor/solver"), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:vida", Body: "hi"}))
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor", "flag"
	_, eh := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "x", Area: "solver", ReplyTo: par.ID})
	_, em := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x", Area: "solver", ReplyTo: par.ID})
	// Mail to a person is private (SPEC 9 "For you"): a reply stays with its
	// participants, refused alike for a hidden and a missing session.
	a, b := wantCode(t, eh, api.CodeForbidden), wantCode(t, em, api.CodeForbidden)
	if strings.ReplaceAll(a.Message, "mini/bbbbbbbb", "X") != strings.ReplaceAll(b.Message, "mini/zzzzzzzz", "X") ||
		strings.Contains(a.Message, events.Someone) || a.Hint != b.Hint {
		t.Fatalf("hidden %q != missing %q", a.Message, b.Message)
	}
	// Even the parent's (hidden) sender is not nameable through reply_to ...
	_, err := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/hhhhhhhh", Body: "x", Area: "solver", ReplyTo: par.ID})
	wantCode(t, err, api.CodeNotFound)
	// ... but the reply endpoint, where the daemon picks the recipient, reaches it, redacted.
	r := must(orchclient.Reply(ctx, g, par.ID, orchapi.ReplyRequest{Body: "thanks"}))
	if r.Recipient != events.Someone || r.ReplyTo != par.ID {
		t.Fatalf("reply %+v", r)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/bbbbbbbb", ""), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("hidden session got the guest's mail: %+v", in)
	}
}

// Regression: a ref in another area than the mail's never reaches the event
// stream (the bus filters by the mail's area only).
func TestEventOmitsCrossAreaRef(t *testing.T) {
	f := newFixture(t)
	f.work.add(f.harbor.ID, "local:bill01", fakeItem{area: "billing", creator: "human:ana"})
	f.work.add(f.harbor.ID, "local:solv01", fakeItem{area: "solver", creator: "human:ana"})
	ch, cancel := f.d.Events.Subscribe(&authz.Subject{Scope: mustGuestScope(t, f), Address: "human:vida"}, events.Filter{Types: []string{"dev.zm.mail."}}, 16)
	defer cancel()
	c := f.as("", "harbor")
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:vida", Body: "cross", Area: "solver", Ref: "local:bill01"}))
	if m.Ref != "local:bill01" || m.Area != "solver" {
		t.Fatalf("owner's copy %+v", m)
	}
	next := func() string {
		select {
		case e := <-ch:
			return string(e.Data)
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		return ""
	}
	if d := next(); strings.Contains(d, "bill01") || !strings.Contains(d, "cross") {
		t.Fatalf("event leaks the billing ref: %s", d)
	}
	if got := must(orchclient.GetMail(ctx, f.guest(), m.ID)); got.Ref != "" {
		t.Fatalf("REST leaks the billing ref: %+v", got)
	}
	// A ref in the mail's own area stays in the event.
	must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:vida", Body: "same", Area: "solver", Ref: "local:solv01"}))
	if d := next(); !strings.Contains(d, "local:solv01") {
		t.Fatalf("same-area ref dropped: %s", d)
	}
}

// Regression: an answer is not a question. A session replying to a human
// does not go into needs_human; an explicit --needs human still does.
func TestReplyToHumanIsReport(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	q := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "ship the fix"}))
	s := f.as("mini/aaaaaaaa", "harbor")
	r := must(orchclient.Reply(ctx, s, q.ID, orchapi.ReplyRequest{Body: "done"}))
	if r.Recipient != "human:ana" || r.Kind != orchapi.MailKindReport {
		t.Fatalf("reply %+v", r)
	}
	if st := f.sess.state("mini/aaaaaaaa"); st != orchapi.StateRunning {
		t.Fatalf("replying put the session in %s", st)
	}
	r = must(orchclient.Send(ctx, s, orchapi.SendMailRequest{To: "human:ana", Body: "also done", ReplyTo: q.ID}))
	if r.Kind != orchapi.MailKindReport || f.sess.state("mini/aaaaaaaa") != orchapi.StateRunning {
		t.Fatalf("reply_to %+v state %s", r, f.sess.state("mini/aaaaaaaa"))
	}
	// the human reads the answers as reports, not in the inbox
	if in := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{})); len(in) != 0 {
		t.Fatalf("human inbox %+v", in)
	}
	if rs := must(orchclient.Inbox(ctx, f.as("", ""), orchapi.InboxQuery{Reports: true})); len(rs) != 2 {
		t.Fatalf("human reports %+v", rs)
	}
	// needs=human mail from an agent is no ask either: a report, nobody waits
	r = must(orchclient.Reply(ctx, s, q.ID, orchapi.ReplyRequest{Body: "which branch?", Needs: orchapi.NeedsHuman}))
	if r.Kind != orchapi.MailKindReport || f.sess.state("mini/aaaaaaaa") != orchapi.StateRunning {
		t.Fatalf("explicit needs=human reply %+v state %s", r, f.sess.state("mini/aaaaaaaa"))
	}
}

// plainSessions hides the fake's IdleManaged: only orchcore.SessionService.
type plainSessions struct{ orchcore.SessionService }

// listSessions is a session module with List (like internal/session) but no IdleManaged.
type listSessions struct {
	orchcore.SessionService
	f *fakeSessions
}

func (l listSessions) List(_ context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error) {
	l.f.mu.Lock()
	defer l.f.mu.Unlock()
	var out []orchapi.Session
	for _, s := range l.f.m {
		if sc.Can(authz.Read, authz.Object{Project: s.Project, Area: s.Area}) && (q.State == "" || s.State == q.State) {
			out = append(out, *s)
		}
	}
	return out, nil
}

// Regression: group mail wakes an idle session through the session module's
// List when it lacks IdleManaged, and says so loudly when it can do neither.
func TestGroupMailWakeWithoutIdleLister(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateIdle)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateIdle)
	f.sess.add("mini/cccccccc", f.harbor.ID, "", false, orchapi.StateIdle) // unmanaged: never typed into
	f.sess.add("mini/dddddddd", f.side.ID, "", true, orchapi.StateIdle)    // other project
	now := time.Now()
	f.sess.m["mini/aaaaaaaa"].Started = now
	f.sess.m["mini/bbbbbbbb"].Started = now.Add(-time.Hour) // oldest
	f.sess.m["mini/cccccccc"].Started = now.Add(-2 * time.Hour)
	f.sess.m["mini/dddddddd"].Started = now.Add(-3 * time.Hour)

	var buf bytes.Buffer
	f.d.Log = slog.New(slog.NewTextHandler(&buf, nil))
	orchcore.Wire(f.d).Sessions = listSessions{SessionService: f.sess, f: f.sess}
	sctx, stop := context.WithCancel(ctx)
	stop()
	_ = f.s.Start(sctx)
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "someone take it"}))
	if n := f.sess.takeNudged(); len(n) != 1 || n[0] != "mini/bbbbbbbb" {
		t.Fatalf("nudged %v (want the oldest idle managed session of the project)", n)
	}
	if strings.Contains(buf.String(), "wakes nobody") {
		t.Fatalf("warned although List works: %s", buf.String())
	}

	orchcore.Wire(f.d).Sessions = plainSessions{f.sess}
	_ = f.s.Start(sctx)
	if !strings.Contains(buf.String(), "group mail wakes nobody") {
		t.Fatalf("no warning at Start: %q", buf.String())
	}
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "again"}))
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "and again"}))
	if n := f.sess.takeNudged(); len(n) != 0 {
		t.Fatalf("nudged %v", n)
	}
	if c := strings.Count(buf.String(), "group mail wakes nobody"); c != 1 {
		t.Fatalf("warned %d times, want once: %s", c, buf.String())
	}
}

// A session address the daemon has no row for (hooks not installed), or one
// registered in project none, is in no project: it gets direct mail only,
// never a project's group mailbox or announcements, and cannot take them.
func TestUnknownSessionGetsNoGroupMail(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/nonenone", api.ProjectNone, "", false, orchapi.StateRunning)
	g1 := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "harbor group"}))
	g2 := must(orchclient.Send(ctx, f.as("", "side"), orchapi.SendMailRequest{To: "machine:mini", Body: "side group"}))
	ann := must(orchclient.Send(ctx, f.as("", "side"), orchapi.SendMailRequest{To: "all", Kind: orchapi.MailKindAnnounce, Body: "side announce"}))
	for _, addr := range []string{"mini/deadbeef", "mini/nonenone"} {
		c := f.as(addr, "")
		if n := must(orchclient.Notice(ctx, c)); n.Line != "" || n.Unread != 0 {
			t.Fatalf("%s notice %+v", addr, n)
		}
		if in := must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{})); len(in) != 0 {
			t.Fatalf("%s inbox %+v", addr, in)
		}
		for _, id := range []string{g1.ID, g2.ID, ann.ID} {
			_, err := orchclient.Ack(ctx, c, id)
			e := wantCode(t, err, api.CodeForbidden)
			if !strings.Contains(e.Hint, "zm install") {
				t.Fatalf("%s ack %s: hint %q", addr, id, e.Hint)
			}
		}
	}
	// Direct mail still reaches a session in project none.
	d := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/nonenone", Body: "direct"}))
	if in := must(orchclient.Inbox(ctx, f.as("mini/nonenone", ""), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != d.ID {
		t.Fatalf("direct mail %+v", in)
	}
	// Nobody took the group mail.
	for _, m := range []orchapi.Mail{g1, g2} {
		if got := must(orchclient.GetMail(ctx, f.as("", ""), m.ID)); got.AckedBy != "" {
			t.Fatalf("group mail taken by %q", got.AckedBy)
		}
	}
}

// In an area a guest may only read, owners-mailbox and needs=human mail stay
// with those who can act on them, unless the guest sent or received it: in
// lists, gets, threads, the live stream and replay.
func TestGuestOnlySeesRestrictedMailItIsPartyTo(t *testing.T) {
	f := newFixture(t)
	ch, cancel := f.d.Events.Subscribe(&authz.Subject{Scope: mustGuestScope(t, f), Address: "human:vida"}, events.Filter{Types: []string{"dev.zm.mail."}}, 16)
	defer cancel()
	owner := f.as("", "harbor/solver")
	esc := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "owners", Body: "HIDDEN escalation"}))
	nh := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:ana", Needs: orchapi.NeedsHuman, Body: "HIDDEN decision"}))
	toG := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:vida", Needs: orchapi.NeedsHuman, Body: "for vida"}))
	f.sess.add("mini/pppppppp", f.harbor.ID, "solver", true, orchapi.StateRunning)
	plain := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "mini/pppppppp", Needs: orchapi.NeedsAny, Body: "area note"}))
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor", "flag"
	fromG := must(orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "owners", Body: "vida asks the owners"}))

	ids := func(ms []orchapi.Mail) map[string]bool {
		out := map[string]bool{}
		for _, m := range ms {
			out[m.ID] = true
		}
		return out
	}
	got := ids(must(orchclient.Mail(ctx, g, orchapi.MailQuery{})))
	if got[esc.ID] || got[nh.ID] || !got[toG.ID] || !got[plain.ID] || !got[fromG.ID] {
		t.Fatalf("guest mail list %v", got)
	}
	if got := ids(must(orchclient.Mail(ctx, g, orchapi.MailQuery{Needs: "human"}))); got[esc.ID] || got[nh.ID] {
		t.Fatalf("guest needs=human filter %v", got)
	}
	_, em := orchclient.GetMail(ctx, g, "01ZZZZZZZZZZZZZZZZZZZZZZZZ")
	me := wantCode(t, em, api.CodeNotFound)
	for _, id := range []string{esc.ID, nh.ID} {
		for _, call := range []func(string) error{
			func(id string) error { _, err := orchclient.GetMail(ctx, g, id); return err },
			func(id string) error { _, err := orchclient.Thread(ctx, g, id); return err },
			func(id string) error { _, err := orchclient.Ack(ctx, g, id); return err },
		} {
			e := wantCode(t, call(id), api.CodeNotFound)
			if strings.ReplaceAll(e.Message, id, "X") != strings.ReplaceAll(me.Message, "01ZZZZZZZZZZZZZZZZZZZZZZZZ", "X") {
				t.Fatalf("restricted %q != missing %q", e.Message, me.Message)
			}
		}
	}
	if must(orchclient.GetMail(ctx, g, toG.ID)).Body != "for vida" {
		t.Fatal("guest cannot read needs=human mail addressed to it")
	}
	// The owner still sees everything.
	if got := ids(must(orchclient.Mail(ctx, f.as("", "harbor"), orchapi.MailQuery{}))); !got[esc.ID] || !got[nh.ID] {
		t.Fatalf("owner mail list %v", got)
	}
	check := func(what string, evs []api.Event) {
		t.Helper()
		var subj []string
		for _, e := range evs {
			if strings.Contains(string(e.Data), "HIDDEN") {
				t.Fatalf("%s: guest got restricted mail %s", what, e.Data)
			}
			subj = append(subj, e.Subject)
		}
		want := []string{toG.ID, plain.ID, fromG.ID}
		if strings.Join(subj, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: guest events %v, want %v", what, subj, want)
		}
	}
	var live []api.Event
	for len(live) < 3 {
		select {
		case e := <-ch:
			live = append(live, e)
		case <-time.After(5 * time.Second):
			t.Fatalf("live events %d", len(live))
		}
	}
	check("live", live)
	gsub, err := f.d.Projects.FromToken(ctx, f.guestTok, true)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.d.Events.Since(ctx, gsub, "", events.Filter{Types: []string{"dev.zm.mail."}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	check("replay", replay)
}

// A reply under restricted mail at any depth (an ask's decision
// reply, plain needs=any to the asker, and replies to it) follows the restricted rule: a guest of the area
// who is not a party never lists, fetches, threads or streams it.
func TestGuestNeverSeesReplyToRestrictedMail(t *testing.T) {
	f := newFixture(t)
	ch, cancel := f.d.Events.Subscribe(&authz.Subject{Scope: mustGuestScope(t, f), Address: "human:vida"}, events.Filter{Types: []string{"dev.zm.mail."}}, 16)
	defer cancel()
	owner := f.as("", "harbor/solver")
	ask := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:ana", Needs: orchapi.NeedsHuman, Body: "HIDDEN ask"}))
	rep := must(orchclient.Reply(ctx, owner, ask.ID, orchapi.ReplyRequest{Needs: orchapi.NeedsAny, Body: "HIDDEN OWNER DECISION approved"}))
	if rep.Needs != orchapi.NeedsAny || rep.ReplyTo != ask.ID {
		t.Fatalf("reply %+v", rep)
	}
	// Deeper in the thread too, and the ack of such a reply.
	rep2 := must(orchclient.Reply(ctx, owner, rep.ID, orchapi.ReplyRequest{Needs: orchapi.NeedsAny, Body: "HIDDEN reply to the decision"}))
	if _, err := orchclient.Ack(ctx, f.as("", "harbor/solver"), rep2.ID); err != nil && !api.IsCode(err, api.CodeConflict) {
		t.Fatal(err)
	}
	// Mail the guest may see: its own. (Plain mail to another person is
	// private to them now, so it is no longer the control.)
	plain := must(orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:vida", Needs: orchapi.NeedsAny, Body: "area note"}))
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor", "flag"
	for _, m := range must(orchclient.Mail(ctx, g, orchapi.MailQuery{})) {
		if strings.Contains(m.Body, "HIDDEN") {
			t.Fatalf("guest lists %q", m.Body)
		}
	}
	for _, call := range []func(string) error{
		func(id string) error { _, err := orchclient.GetMail(ctx, g, id); return err },
		func(id string) error { _, err := orchclient.Thread(ctx, g, id); return err },
	} {
		wantCode(t, call(rep.ID), api.CodeNotFound)
		wantCode(t, call(rep2.ID), api.CodeNotFound)
	}
	// Owners still read the reply.
	if must(orchclient.GetMail(ctx, f.as("", "harbor"), rep.ID)).Body == "" {
		t.Fatal("owner cannot read the reply")
	}
	select {
	case e := <-ch:
		if e.Subject != plain.ID {
			t.Fatalf("guest event %s %s", e.Subject, e.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live event for the plain mail")
	}
	gsub, err := f.d.Projects.FromToken(ctx, f.guestTok, true)
	if err != nil {
		t.Fatal(err)
	}
	replay := must(f.d.Events.Since(ctx, gsub, "", events.Filter{Types: []string{"dev.zm.mail."}}, 100))
	for _, e := range replay {
		if strings.Contains(string(e.Data), "HIDDEN") {
			t.Fatalf("replay leaks %s", e.Data)
		}
	}
}

// SPEC 9 "Across projects", on one machine: a session registered in another
// project or in none is reached by mail in the sender's project, filed in
// the sender's area (never the session's own project's area); a reply
// reaches a sender this daemon never knew; a guest gets the missing answer.
func TestMailAcrossProjectsLocal(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/nonenone", api.ProjectNone, "", false, orchapi.StateIdle)
	f.sess.add("mini/sideside", f.side.ID, "", false, orchapi.StateIdle)
	f.sess.add("mini/solvsess", f.harbor.ID, "solver", false, orchapi.StateIdle)
	m := must(orchclient.Send(ctx, f.as("mini/sideside", "side"), orchapi.SendMailRequest{To: "mini/nonenone", Body: "from side"}))
	if m.Project != f.side.ID || m.Recipient != "mini/nonenone" {
		t.Fatalf("mail to a no-project session %+v", m)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/nonenone", ""), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != m.ID {
		t.Fatalf("no-project inbox %+v", in)
	}
	m2 := must(orchclient.Send(ctx, f.as("mini/sideside", "side"), orchapi.SendMailRequest{To: "mini/solvsess", Body: "side to a harbor session"}))
	if m2.Project != f.side.ID || m2.Area != "" {
		t.Fatalf("mail to another project's session filed in %s/%q", m2.Project, m2.Area)
	}
	if in := must(orchclient.Inbox(ctx, f.as("mini/solvsess", ""), orchapi.InboxQuery{})); len(in) != 1 || in[0].ID != m2.ID {
		t.Fatalf("other project's session inbox %+v", in)
	}
	// By name (SPEC 8 "Names"), from a whole-project reader who cannot see
	// the session's own project: the mail stores the address, and the send
	// answer shows it (not the name typed, not redacted).
	ns := namedSessions{SessionService: f.sess, f: f.sess, names: map[string]string{"mini/free-agent": "mini/nonenone"}}
	orchcore.Wire(f.d).Sessions = ns
	tok := must(f.d.Projects.Mint(ctx, f.d.Projects.Local(""), api.MintTokenRequest{As: "dot", Project: "side", Role: "member"}))
	mem := client.WithHandler(f.h)
	mem.Token, mem.Project, mem.ProjectFrom = tok.Secret, "side", "flag"
	mn := must(orchclient.Send(ctx, mem, orchapi.SendMailRequest{To: "mini/free-agent", Body: "by name"}))
	if mn.To != "mini/nonenone" || mn.Recipient != "mini/nonenone" {
		t.Fatalf("mail by name to a no-project session %+v", mn)
	}
	orchcore.Wire(f.d).Sessions = f.sess
	// A reply reaches a sender with no session row.
	m3 := must(orchclient.Send(ctx, f.as("mini/ghost123", "harbor/solver"), orchapi.SendMailRequest{To: "mini/solvsess", Body: "from a ghost"}))
	r := must(orchclient.Reply(ctx, f.as("mini/solvsess", ""), m3.ID, orchapi.ReplyRequest{Body: "hi"}))
	if r.Recipient != "mini/ghost123" || r.Area != "solver" {
		t.Fatalf("reply to an unknown sender %+v", r)
	}
	// An unknown address is refused; a guest gets exactly that answer for a
	// session outside its area.
	_, err := orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x"})
	wantCode(t, err, api.CodeNotFound)
	g := f.guest()
	g.Project, g.ProjectFrom = "harbor/solver", "flag"
	_, want := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x"})
	wantCode(t, want, api.CodeNotFound)
	for _, hidden := range []string{"mini/nonenone", "mini/sideside"} {
		_, err := orchclient.Send(ctx, g, orchapi.SendMailRequest{To: hidden, Body: "x"})
		e := wantCode(t, err, api.CodeNotFound)
		if strings.ReplaceAll(e.Message, hidden, "X") != strings.ReplaceAll(want.(*api.Error).Message, "mini/zzzzzzzz", "X") || e.Hint != want.(*api.Error).Hint {
			t.Fatalf("guest to hidden %s: %v, missing: %v", hidden, err, want)
		}
	}
}

// An area-level leak (PR 101 review): a whole-project reader of side who
// is only a guest of harbor/billing must not reach, from side, this
// machine's session in harbor/solver: the answer is a missing session's,
// byte for byte. Its own area's session is reached.
func TestMailAcrossProjectsAreaGuestElsewhere(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/solvsess", f.harbor.ID, "solver", false, orchapi.StateIdle)
	f.sess.add("mini/billsess", f.harbor.ID, "billing", false, orchapi.StateIdle)
	owner := f.d.Projects.Local("")
	must(f.d.Projects.Grant(ctx, owner, api.GrantRequest{Identity: "gina", Role: "member", Project: "side"}))
	must(f.d.Projects.Grant(ctx, owner, api.GrantRequest{Identity: "gina", Role: "guest", Project: "harbor", Area: "billing"}))
	tok := must(f.d.Projects.Mint(ctx, owner, api.MintTokenRequest{As: "gina", Role: "member"}))
	c := client.WithHandler(f.h)
	c.Token, c.Project, c.ProjectFrom = tok.Secret, "side", "flag"
	_, missing := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/zzzzzzzz", Body: "x"})
	want := wantCode(t, missing, api.CodeNotFound)
	_, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/solvsess", Body: "x"})
	got := wantCode(t, err, api.CodeNotFound)
	if strings.ReplaceAll(got.Message, "mini/solvsess", "X") != strings.ReplaceAll(want.Message, "mini/zzzzzzzz", "X") || got.Hint != want.Hint {
		t.Fatalf("to a session in an area the sender cannot read: %+v, missing: %+v", got, want)
	}
	if m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/billsess", Body: "x"})); m.Recipient != "mini/billsess" || m.Project != f.side.ID {
		t.Fatalf("to a session in the sender's own area of harbor: %+v", m)
	}
}

func TestSendCleansHiddenText(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateIdle)
	c := f.as("", "harbor/solver")
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "hi\u202e 👩‍💻"}))
	if m.Body != "hi 👩‍💻" {
		t.Fatalf("body %q", m.Body)
	}
	if _, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "\u200b"}); err == nil {
		t.Fatal("invisible-only mail accepted")
	}
}

func TestReviewBlankMail(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "solver", true, orchapi.StateIdle)
	c := f.as("", "harbor/solver")
	for _, body := range []string{"\ufe0f", "\u2800"} {
		if _, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: body}); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// A notice nobody acked settles itself, so it never piles up in front of mail.
func TestNoticeSettles(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	n := must(f.s.Send(ctx, f.s.d.System(), api.ProjectRef{ID: f.harbor.ID, Slug: f.harbor.Slug}, "", orchapi.SendMailRequest{To: "mini/aaaaaaaa", Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent, Body: "x"}))
	note := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "real"}))
	f.clock = f.clock.Add(NoticeKeep + time.Minute)
	if err := f.s.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{}
	for _, m := range must(orchclient.Inbox(ctx, f.as("mini/aaaaaaaa", ""), orchapi.InboxQuery{})) {
		open[m.ID] = true
	}
	if open[n.ID] || !open[note.ID] {
		t.Fatalf("after a day: notice open %v, real mail open %v", open[n.ID], open[note.ID])
	}
}

func TestNoticePartsCountsDirectWakeableWithoutOverlap(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	ref := api.ProjectRef{ID: f.harbor.ID, Slug: f.harbor.Slug}
	for _, req := range []orchapi.SendMailRequest{
		{To: "mini/aaaaaaaa", Body: "direct"},
		{To: "machine:mini", Body: "group notice", Kind: orchapi.MailKindNotice},
		{To: "mini/aaaaaaaa", Body: "direct notice", Kind: orchapi.MailKindNotice},
	} {
		if _, err := f.s.Send(ctx, f.d.System(), ref, "", req); err != nil {
			t.Fatal(err)
		}
	}
	parts, err := f.s.NoticeParts(ctx, f.d.Projects.Local("mini/aaaaaaaa"))
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts: %+v %v", parts, err)
	}
	n := parts[0]
	if n.Unread != 3 || n.Group != 1 || n.Quiet != 2 || n.DirectWakeable == nil || *n.DirectWakeable != 1 {
		t.Fatalf("overlapping notice counts lost direct mail: %+v", n)
	}
}

// A hook's poll: GET /v0/mail/inbox/peek lists what the inbox would (direct mail,
// the session's own project's group mail, unseen announcements) and leaves
// announcements unseen; it never shows another project's group mail, which
// the session could not take.
func TestPeekInboxLeavesAnnouncementsUnseen(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateIdle)
	f.sess.add("mini/cccccccc", f.side.ID, "", true, orchapi.StateIdle)
	ann := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "all", Kind: "announce", Body: "main is frozen"}))
	grp := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Body: "someone take the deploy"}))
	dir := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "yours"}))
	ids := func(in []orchapi.Mail) map[string]bool {
		got := map[string]bool{}
		for _, m := range in {
			got[m.ID] = true
		}
		return got
	}
	a, c := f.as("mini/aaaaaaaa", ""), f.as("mini/cccccccc", "")
	for i := 0; i < 3; i++ {
		got := ids(must(orchclient.PeekInbox(ctx, a, orchapi.InboxQuery{})))
		if len(got) != 3 || !got[ann.ID] || !got[grp.ID] || !got[dir.ID] {
			t.Fatalf("peek %d: %v", i, got)
		}
	}
	if n := must(orchclient.Notice(ctx, a)); !strings.Contains(n.Line, "1 announce") {
		t.Fatalf("peek ate the announcement: notice %q", n.Line)
	}
	if got := must(orchclient.PeekInbox(ctx, c, orchapi.InboxQuery{})); len(got) != 0 {
		t.Fatalf("other project's session peeks group mail or announcement: %+v", got)
	}
	if got := ids(must(orchclient.Inbox(ctx, a, orchapi.InboxQuery{}))); len(got) != 3 {
		t.Fatalf("inbox after peek: %v", got)
	}
	if got := ids(must(orchclient.PeekInbox(ctx, a, orchapi.InboxQuery{}))); len(got) != 2 || got[ann.ID] {
		t.Fatalf("plain inbox did not mark the announcement seen: %v", got)
	}
}
