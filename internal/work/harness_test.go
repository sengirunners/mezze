package work

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// TestMain lets the test binary stand in for tracker adapters: zm-work-fake
// (internal/work/fake, linked onto PATH by fake.Install) and the small ones
// tests define, so tracker tests run end to end through real adapter
// processes (the test binary links no driver: adapters are processes, SPEC 11).
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == adapter.ExecPrefix+"nocursor" {
		os.Exit(adapter.Main(noCursorHandlers()))
	}
	if filepath.Base(os.Args[0]) == adapter.ExecPrefix+"listonly" {
		os.Exit(adapter.Main(listOnlyHandlers()))
	}
	if code, ok := fake.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakeSessions is the session module as the work module sees it.
type fakeSessions struct {
	mu  sync.Mutex
	m   map[string]orchapi.Session
	err error // LookupErr's answer while set (a store that does not answer)
}

func (f *fakeSessions) LookupErr(ctx context.Context, a string) (orchapi.Session, bool, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return orchapi.Session{}, false, err
	}
	s, ok := f.Lookup(ctx, a)
	return s, ok, nil
}

func (f *fakeSessions) put(s orchapi.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[s.Address] = s
}

func (f *fakeSessions) setState(addr string, st orchapi.SessionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.m[addr]
	s.State = st
	f.m[addr] = s
}

func (f *fakeSessions) Lookup(_ context.Context, a string) (orchapi.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[a]
	return s, ok
}

func (f *fakeSessions) Visible(ctx context.Context, sc authz.Scope, a string) bool {
	s, ok := f.Lookup(ctx, a)
	return ok && sc.Can(authz.Read, authz.Object{Project: s.Project, Area: s.Area})
}

func (f *fakeSessions) Live(ctx context.Context, a string) bool {
	s, ok := f.Lookup(ctx, a)
	return ok && s.State != orchapi.StateEnded
}
func (f *fakeSessions) SetState(context.Context, string, orchapi.StateRequest) error { return nil }
func (f *fakeSessions) Nudge(context.Context, string, string) (bool, error)          { return false, nil }
func (f *fakeSessions) ManagedLive(context.Context) int                              { return 0 }
func (f *fakeSessions) RestartBlockers(context.Context) []orchapi.Session            { return nil }

// fakeMail records what the work module sends.
type fakeMail struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct {
	project api.ProjectRef
	area    string
	from    string
	req     orchapi.SendMailRequest
}

func (f *fakeMail) Send(_ context.Context, sub *authz.Subject, p api.ProjectRef, area string, req orchapi.SendMailRequest) (orchapi.Mail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMail{p, area, sub.Address, req})
	return orchapi.Mail{}, nil
}

// Reply is unused by the work module (approvals answers asks with it).
func (f *fakeMail) Reply(context.Context, *authz.Subject, string, string, orchapi.ReplyRequest) (orchapi.Mail, error) {
	return orchapi.Mail{}, api.ErrNotImplemented("reply")
}

func (f *fakeMail) all() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.sent...)
}

type harness struct {
	t        testing.TB
	ctx      context.Context
	d        *core.Deps
	s        *Service
	router   *core.Router
	sessions *fakeSessions
	mail     *fakeMail
	clock    *clock
	owner    *authz.Subject
	harbor   api.Project
	side     api.Project
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newHarness wires the work module alone over a temp ZM_HOME: real store,
// registry and event bus; fake sessions and mail. Projects: harbor (areas
// solver and billing) and side.
func newHarness(t testing.TB) *harness {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
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
	h := &harness{t: t, ctx: ctx, sessions: &fakeSessions{m: map[string]orchapi.Session{}}, mail: &fakeMail{},
		clock: &clock{t: time.Now().UTC().Truncate(time.Millisecond)}}
	h.d = &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Started: time.Now()}
	h.s = New(h.d)
	h.s.now = h.clock.now
	o := orchcore.Wire(h.d)
	o.Work, o.Sessions, o.Mail, o.Trackers, o.Homes = h.s, h.sessions, h.mail, h.s, homes.New(h.d)
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, a string) bool { return h.d.Visible(ctx, sc, a) })
	h.router = core.NewRouter()
	h.s.Routes(h.router)
	t.Cleanup(func() { h.s.Close() })

	h.owner = reg.Local("")
	if h.harbor, err = reg.Create(ctx, h.owner, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "billing"} {
		if _, err := reg.AddArea(ctx, h.owner, "harbor", api.Area{Name: a, Work: []string{"bd:" + a + "-*"}}); err != nil {
			t.Fatal(err)
		}
	}
	if h.side, err = reg.Create(ctx, h.owner, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	return h
}

// as returns a client calling the work routes as sub, with an explicit
// project (from --project) when project != "". It sets the subject directly
// (the transport is the integrator's concern and tested in server).
func (h *harness) as(sub *authz.Subject, proj string) *client.Client {
	hd := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		ctx := core.WithSubject(r.Context(), sub)
		in := project.ResolveInput{}
		if proj != "" {
			in.Explicit, in.ExplicitFrom = proj, project.RuleFlag
		}
		h.router.ServeHTTP(w, r.WithContext(core.WithResolveInput(ctx, in)))
	})
	return client.WithHandler(hd)
}

// guest is a token subject: guest of harbor/solver (remote).
func (h *harness) guest() *authz.Subject {
	h.t.Helper()
	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if err != nil {
		h.t.Fatal(err)
	}
	sub, err := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	if err != nil {
		h.t.Fatal(err)
	}
	return sub
}

// session registers a fake session and returns the owner acting as it.
func (h *harness) session(addr, proj, area string, managed bool, st orchapi.SessionState) *authz.Subject {
	h.sessions.put(orchapi.Session{Address: addr, Project: proj, Area: area, Managed: managed, State: st})
	return h.d.Projects.Local(addr)
}

func (h *harness) events(types ...string) []api.Event {
	h.t.Helper()
	evs, err := h.d.Events.Since(h.ctx, h.d.System(), "", events.Filter{Types: types}, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

// installFake links the test binary onto a fresh PATH dir as zm-work-fake,
// with a fresh state, and returns the state file.
func (h *harness) installFake() string {
	h.t.Helper()
	path, state, err := fake.Install(h.t.TempDir())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", path)
	h.t.Setenv(fake.EnvState, state)
	return state
}

func isCode(err error, code string) bool { return api.IsCode(err, code) }

// sync waits for one fresh sync of every bound tracker's mirror (reads
// are served from it; a test that changed the fake tracker syncs first).
func (h *harness) sync() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute) // -race is slow
	defer cancel()
	if err := h.s.syncAll(ctx); err != nil {
		h.t.Fatal(err)
	}
}

// syncAll waits for one fresh sync of every bound tracker.
func (s *Service) syncAll(ctx context.Context) error {
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			if err := s.m.syncer(p.ID, tr).wait(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
