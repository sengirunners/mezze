package homes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/mezze/internal/tools/toolstest"
	"github.com/sengirunners/mezze/internal/work"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// TestMain lets the test binary stand in for a tracker adapter
// (zm-work-fake, internal/work/fake, linked onto PATH by installFake), so
// tracker homes run end to end through a real adapter process.
func TestMain(m *testing.M) {
	if code, ok := fake.Main(); ok {
		os.Exit(code)
	}
	os.Exit(run(m))
}

// run is TestMain's body, so its deferred cleanup runs on every return.
func run(m *testing.M) int {
	defer toolstest.Cleanup() // the real off-site test's shared rclone download, if any
	return m.Run()
}

// harness is a homes module wired to a real work module, as the server wires
// them, over a temp ZM_HOME: real store, registry and event bus, no sessions
// or mail. Projects: harbor (areas solver and billing) and side.
type harness struct {
	t      testing.TB
	ctx    context.Context
	d      *core.Deps
	s      *Service
	w      *work.Service
	router *core.Router
	clock  *clock
	owner  *authz.Subject
	harbor api.Project
	side   api.Project
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

func newHarness(t testing.TB) *harness {
	t.Helper()
	ctx := context.Background()
	zmHome := t.TempDir()
	cfg, err := config.LoadFrom(zmHome)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	cfg.SyncPause = time.Hour // mirrors sync on explicit kicks (h.sync), so a test decides when the tracker is asked
	st, err := storetest.Open(ctx, zmHome)
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
	h := &harness{t: t, ctx: ctx, clock: &clock{t: time.Now().UTC().Truncate(time.Millisecond)}}
	h.d = &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Started: time.Now()}
	h.w, h.s = work.New(h.d), New(h.d)
	h.s.now = h.clock.now
	o := orchcore.Wire(h.d)
	o.Work, o.Trackers, o.Homes = h.w, h.w, h.s
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, a string) bool { return h.d.Visible(ctx, sc, a) })
	h.router = core.NewRouter()
	h.w.Routes(h.router)
	h.s.Routes(h.router)
	t.Cleanup(func() { h.s.Close(); h.w.Close() })

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

// as returns a client calling the routes as sub, with an explicit project
// (from --project) when proj != "".
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

// sync waits for one fresh sync of every bound tracker's mirror, through
// tracker test (POST /v0/trackers/{prefix}/test).
func (h *harness) sync() {
	h.t.Helper()
	for _, p := range h.d.Store.Projects(authz.All(), authz.Read) {
		f, ok := h.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute) // -race is slow
			res, err := orchclient.TestTracker(ctx, h.as(h.owner, f.Slug), tr.Prefix)
			cancel()
			if err != nil {
				h.t.Fatalf("sync of %s: %v", tr.Prefix, err)
			}
			if !res.Synced || res.Tracker.Sync == nil || res.Tracker.Sync.Error != "" {
				h.t.Fatalf("sync of %s: %+v", tr.Prefix, res)
			}
		}
	}
}

func isCode(err error, code string) bool { return api.IsCode(err, code) }

func edit(t testing.TB, state string, fn func(st *fake.State)) {
	t.Helper()
	st, err := fake.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	fn(st)
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
}
