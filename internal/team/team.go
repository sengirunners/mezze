// Package team is orchestration across machines (SPEC 17) on top of
// teamlink (joined projects, join, leave, invites, the proxy): on a
// member's machine, the orchestration routes' rows for the proxy and its
// hooks (proxy.go, routes_table.go), the replica copy, its outbox and sync
// loops, presence and machine ops; on the authority, sync, snapshots, queued writes, presence
// and remote tasks, sessions and the Mezze app.
package team

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// remote is a joined project (teamlink's); its Copy is a *replica here.
type remote = teamlink.Remote

// Service is the team module.
type Service struct {
	// The joined projects, their clients, join and leave, invites and
	// operator classes (KIT); team builds on them.
	*teamlink.Service
	d *core.Deps

	// judgeC is the model the replica's mail-chore check asks (SetJudge);
	// nil is the configured provider CLI (judge.Default).
	judgeMu sync.Mutex
	judgeC  judge.Caller

	// parentStarts serializes remote task starts per parent session
	// (remote_tasks.go StartTaskOn).
	parentStarts parentLocks

	// The home's restore generation (authsync.go restoreGeneration).
	genMu sync.Mutex
	gen   string
	genOK bool

	// The authority's side of round C: write batches per identity, status
	// writes with a base, write-record pruning.
	wlocks    writeLocks
	live      liveInflight // shared live/queued in-flight write IDs
	livePrune atomic.Int64 // live records since the last prune check
	wprune    atomic.Int64

	// The replica's presence (round B2): the cache its notice reads.
	pmu   sync.Mutex
	cache replicaCache
	// streams: shared structured sessions' events relayed through the
	// authority (session_relay.go).
	streams relayState
	// doorMem: the door deliveries relayed to other machines (door_relay.go).
	doorMem teamlink.OpMem
	// gates: task starts running here for an authority (start_gates.go).
	gates startGates
	// lineMu: one change at a time to the waiting line (task_line.go).
	lineMu     sync.Mutex
	lineSweeps atomic.Int64 // full sweeps so far (sweepLine)
	// prunedTyped: when team_prompts_typed was last pruned (UnixNano).
	prunedTyped atomic.Int64
	// t3: this machine's Mezze app server (t3.go).
	t3 t3Host
	// t3Waiting: the t3_ensure op ids a requester waits for here (only
	// those take a credential).
	t3Waiting sync.Map
	wg        sync.WaitGroup
	// started: when this daemon started (UnixNano; waitNode).
	started atomic.Int64
	stopPL  context.CancelFunc
	rctx    context.Context // ends at shutdown: replica loops stop with it
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// Migrations are this module's tables, its replica copies' included
// (core.Deps.AddMigrations; a copy opens through Store.OpenReplica); New adds
// them.
var Migrations = slices.Concat(replicaMigrations, authsyncMigrations, livewritesMigrations, promptsMigrations, mailRequestsMigrations, taskLineMigrations, outboxVisibilityMigrations, remoteTasksMigrations, machinePersonMigrations)

// New returns the team module over link (started before it: Start finds
// the remotes link loaded) and sets link's hooks.
func New(d *core.Deps, link *teamlink.Service) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{Service: link, d: d}
	s.hook()
	s.T3Block()                    // before any module saves config.json (t3.go)
	d.AddRequestGate(s.gateOrigin) // the caller check on the socket (origin_gate.go)
	d.AddScrubber(s.scrub)
	return s
}

// hook sets s's teamlink's hooks to team's.
func (s *Service) hook() {
	link := s.Service
	link.Hooks.Joined = s.joined
	link.Hooks.Removed = s.removed
	link.Hooks.DropCopy = s.dropCopy
	link.Hooks.OpenCopy = s.startReplicaLocked
	link.Hooks.ScopeMoved = s.copyRescope
	link.Hooks.View = s.remoteView
	link.Hooks.MoveLeases = s.moveLeases
	link.Hooks.Machines = s.projectMachines
	link.Hooks.ClassChanged = append(link.Hooks.ClassChanged, s.classChanged)
	s.registerBeatParts() // the heartbeat's sessions part
	// The machine-op kinds team owns (teamlink's queue carries them).
	s.registerTaskOps()
	s.registerAskActionOps()
	s.registerSessionOps()
	s.registerSessionStartOps()
	s.registerT3Ops()
	s.registerDoorOps()
	// The proxy (proxy.go): live writes at the authority, the copy, the
	// relayed answers, and the orchestration route rows.
	link.Hooks.FromMember = s.fromMember // a lost task copy's writes, outward actions, live writes
	link.Hooks.Copy = s.copyState
	link.Hooks.ServeCopy = s.serveFromCopy
	link.Hooks.ServeHeld = s.serveHeld
	link.Hooks.ListShare = s.listShare
	link.Hooks.Relay = s.relay
	link.RegisterRoutes("team", s.routeRows()...)
}

// scrub overwrites a redaction's texts (at least core.MinUnscoped long) in
// the project's recorded write answers (team_writes: a replica's replay is
// answered from them for 30 days).
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	s.streams.redact(sc.Project, sc.Texts) // relayed session events may hold the text (session_relay.go)
	db := s.d.Store.Writer(sc.Project)
	if db == nil {
		return nil
	}
	for _, t := range sc.Texts {
		if len(t) < core.MinUnscoped {
			continue
		}
		enc, _ := json.Marshal(t)
		rows, err := db.QueryContext(ctx, `SELECT identity, id, body FROM team_writes WHERE instr(body, ?) > 0 OR instr(body, ?) > 0`, t, string(enc[1:len(enc)-1]))
		if err != nil {
			return err
		}
		type w struct{ identity, id, body string }
		var ws []w
		for rows.Next() {
			var x w
			if err := rows.Scan(&x.identity, &x.id, &x.body); err != nil {
				rows.Close()
				return err
			}
			ws = append(ws, x)
		}
		rows.Close()
		for _, x := range ws {
			nb := core.ScrubText(x.body, sc.Texts, core.MinUnscoped)
			if _, err := db.ExecContext(ctx, `UPDATE team_writes SET body = ? WHERE identity = ? AND id = ?`, nb, x.identity, x.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Start opens the joined projects' copies (teamlink loaded them and runs
// the heartbeat). It never blocks on the network.
func (s *Service) Start(ctx context.Context) error {
	s.started.Store(time.Now().UnixNano())
	ctx, s.stopPL = context.WithCancel(ctx)
	s.rctx = ctx
	for _, x := range s.List() {
		s.startReplica(ctx, x)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.lineLoop(ctx) // the waiting line (task_line.go)
	}()
	return nil
}

// Close stops teamlink's heartbeat and waits for it and the ops running
// here, then closes the copies.
func (s *Service) Close() error {
	s.StopLoops()
	if s.stopPL != nil {
		s.stopPL()
	}
	s.wg.Wait()
	s.WaitOps()
	for _, x := range s.List() {
		s.stopReplica(x, false)
	}
	return nil
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func jsonH(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// SetJudge replaces the model the replica's mail-chore check asks (tests); nil restores the
// configured provider CLI.
func (s *Service) SetJudge(c judge.Caller) {
	s.judgeMu.Lock()
	s.judgeC = c
	s.judgeMu.Unlock()
}

// judgeCaller is the model the replica's mail-chore check asks.
func (s *Service) judgeCaller() judge.Caller {
	s.judgeMu.Lock()
	c := s.judgeC
	s.judgeMu.Unlock()
	if c != nil {
		return c
	}
	return judge.Default(s.d.Config.File.Agents)
}
