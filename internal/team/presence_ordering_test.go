package team

import (
	"context"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/mezze/internal/work"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

type orderingPresenceSource struct {
	orchcore.SessionService
	mu     sync.Mutex
	rows   []orchapi.Session
	known  orchapi.Session
	err    error
	x      *remote
	reads  atomic.Int32
	unheld atomic.Bool
}

func (s *orderingPresenceSource) PresenceSessions(context.Context, int, int) ([]orchapi.Session, error) {
	s.reads.Add(1)
	if s.x.BeatMu.TryLock() {
		s.unheld.Store(true)
		s.x.BeatMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]orchapi.Session(nil), s.rows...), s.err
}

func (s *orderingPresenceSource) Lookup(_ context.Context, address string) (orchapi.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.known, s.known.Address == address
}

func orderingDeps(t *testing.T, machine string) *core.Deps {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "owner", machine
	st, err := storetest.Open(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := project.Open(context.Background(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(context.Background(), st, machine)
	if err != nil {
		t.Fatal(err)
	}
	return &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A held old beat must not send its earlier empty inventory after a newer
// session has been acknowledged and has acquired a real authority-bound lease.
func TestPresenceQueuedBeatKeepsNewSessionAndBoundLease(t *testing.T) {
	ctx := context.Background()
	ad := orderingDeps(t, "authority")
	owner := ad.Projects.Local("")
	proj, err := ad.Projects.Create(ctx, owner, api.NewProjectRequest{Slug: "ordering"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Projects.SetTeam(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	as, aw := session.New(ad), work.New(ad)
	orchcore.Wire(ad).Sessions, orchcore.Wire(ad).Work = as, aw
	t.Cleanup(func() { as.Close(); aw.Close() })
	authority := New(ad, teamlink.New(ad))
	// The fixture supplies a node-bound subject; the real presence handler
	// still validates its project, machine, areas, body and inventory bounds.
	node := *owner
	node.Node, node.Machine, node.Via, node.Remote = "ordering-node", "replica", authz.ViaToken, true
	presenceCtx := core.WithSubject(ctx, &node)
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/team/machine" {
			api.WriteJSON(w, http.StatusOK, struct{}{}) // the machine's numbers, after a beat went through
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v0/team/presence" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			api.WriteError(w, api.NotFound("route", r.URL.Path))
			return
		}
		sent.Add(1)
		out, err := authority.HandleBeat(r.WithContext(core.WithSubject(r.Context(), &node)))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, out)
	}))
	t.Cleanup(server.Close)
	rd := orderingDeps(t, "replica")
	x := &remote{R: api.Remote{Project: api.ProjectRef{ID: proj.ID, Slug: proj.Slug}, URL: server.URL, State: api.RemoteOK}, HC: server.Client()}
	source := &orderingPresenceSource{x: x}
	orchcore.Wire(rd).Sessions = source
	link := teamlink.New(rd)
	link.Add(x)
	replica := New(rd, link)

	// The beat is queued before the new session exists; it reads the
	// inventory only once it holds the send lock (scheduling-independent).
	x.BeatMu.Lock()
	locked := true
	done := make(chan struct{})
	go func() { replica.BeatRemote(ctx, x); close(done) }()
	defer func() {
		if locked {
			x.BeatMu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("queued heartbeat did not stop before fixture cleanup")
		}
	}()
	newborn := orchapi.Session{Address: "replica/abcd1234", Machine: "replica", Project: proj.ID,
		Agent: "codex", State: orchapi.StateRunning, Managed: true, Started: time.Now()}
	source.mu.Lock()
	source.rows, source.known = []orchapi.Session{newborn}, newborn
	source.mu.Unlock()
	if err := as.RecordPresence(presenceCtx, proj.ID, "replica", owner.Identity.Name, []orchapi.PresenceSession{{
		Address: newborn.Address, Agent: newborn.Agent, State: newborn.State, Managed: true, Started: newborn.Started}}); err != nil {
		t.Fatal(err)
	}
	router := core.NewRouter()
	aw.Routes(router)
	ownerClient := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		cctx := core.WithResolveInput(core.WithSubject(r.Context(), owner), project.ResolveInput{Explicit: proj.ID, ExplicitFrom: project.RuleFlag})
		router.ServeHTTP(w, r.WithContext(cctx))
	}))
	item, err := orchclient.CreateWork(ctx, ownerClient, orchapi.CreateWorkRequest{Title: "held by newborn"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := aw.BindLease(core.WithSubject(ctx, owner), owner, proj.ID, item.ID, newborn.Address)
	if err != nil || !lease.Bound || lease.Holder != newborn.Address {
		t.Fatalf("newborn authority lease: %+v err=%v", lease, err)
	}
	x.BeatMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("queued heartbeat did not finish")
	}
	got, found := as.Lookup(ctx, newborn.Address)
	held, holderErr := aw.OwnerOf(core.WithSubject(ctx, owner), owner, proj.ID, item.ID)
	if !found || got.State != orchapi.StateRunning || holderErr != nil || held != newborn.Address {
		t.Fatalf("stale queued heartbeat changed new authority state: found=%v state=%s holder=%q err=%v", found, got.State, held, holderErr)
	}
	if sent.Load() != 1 || source.reads.Load() != 1 || source.unheld.Load() {
		t.Fatalf("heartbeat must read once under send lock: sent=%d reads=%d unlocked=%v", sent.Load(), source.reads.Load(), source.unheld.Load())
	}

	// The ordinary asynchronous entry point must also enumerate under beatMu.
	replica.Heartbeat(ctx)
	if source.reads.Load() != 2 || source.unheld.Load() || sent.Load() != 2 {
		t.Fatalf("ordinary heartbeat read outside lock: reads=%d unlocked=%v sent=%d", source.reads.Load(), source.unheld.Load(), sent.Load())
	}
	// A real omission still ends presence and releases its bound lease.
	source.mu.Lock()
	source.rows, source.known.State = nil, orchapi.StateEnded
	source.mu.Unlock()
	replica.Heartbeat(ctx)
	if got, ok := as.Lookup(ctx, newborn.Address); !ok || got.State != orchapi.StateEnded {
		t.Fatalf("genuinely ended presence was kept: found=%v state=%s", ok, got.State)
	}
	if holder, err := aw.OwnerOf(core.WithSubject(ctx, owner), owner, proj.ID, item.ID); err != nil || holder != "" {
		t.Fatalf("genuinely ended session kept a lease: holder=%q err=%v", holder, err)
	}

	before := sent.Load()
	source.mu.Lock()
	source.err = fmt.Errorf("complete inventory unavailable")
	source.mu.Unlock()
	replica.Heartbeat(ctx)
	if sent.Load() != before {
		t.Fatal("inventory error sent an empty replacement heartbeat")
	}
	reads := source.reads.Load()
	cctx, cancel := context.WithCancel(ctx)
	x.BeatMu.Lock()
	canceledDone := make(chan struct{})
	go func() { replica.BeatRemote(cctx, x); close(canceledDone) }()
	cancel() // while this heartbeat is queued for the send lock
	x.BeatMu.Unlock()
	select {
	case <-canceledDone:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled queued heartbeat did not finish")
	}
	if sent.Load() != before || source.reads.Load() != reads {
		t.Fatal("canceled heartbeat read or sent after taking the lock")
	}
	// Revocation must clear cached notices even when the source is failing.
	x.Mu.Lock()
	x.R.State = api.RemoteRevoked
	x.Mu.Unlock()
	replica.pmu.Lock()
	replica.cache.notices[proj.ID] = map[string][]orchapi.NoticePart{newborn.Address: {{Project: proj.ID, Unread: 1}}}
	replica.pmu.Unlock()
	replica.BeatRemote(ctx, x)
	replica.pmu.Lock()
	_, cached := replica.cache.notices[proj.ID]
	replica.pmu.Unlock()
	if cached || source.reads.Load() != reads || sent.Load() != before {
		t.Fatal("revoked remote retained notices or read/sent an inventory")
	}
}
