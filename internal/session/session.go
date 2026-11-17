// Package session is the session module: agents in PTYs held by per-session
// holder processes that outlive the daemon (holders.go, package holder)
// (spawn with admission control, attach, input, output, stop), agents
// registered by their hooks, states, typed outcomes, per-session worktree and
// scratch dir, and `gc` (lists, never deletes). Owned by the session builder.
// See SPEC.md "Sessions".
//
// Storage: sessions in registry.db (every session has a project, "none"
// included; store.go).
//
// State comes from hooks (POST /v0/sessions/{id}/state, or register with a
// state), mail (needs=human, via SetState), the process exiting (ended with a
// typed outcome, outcome.go) and the idle timer: a managed session with no
// PTY output for d.Config.IdleAfter() becomes idle (note "no output"), and
// output moves it back to running. A hook-set needs_human (a permission
// prompt) of a managed session goes back to running on human input followed
// by output; a mail-set one only through mail. The work ticker renews bound leases only
// while running, so this is what lets a hung agent's lease expire.
//
// Admission is a pure function (Admit) with a table test; refusals never kill
// anything. Outcome detection is one table of detectors per runtime, tested
// against fixture transcripts in testdata/outcomes.
package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// NudgeLine is the only text zm types into an agent for mail.
const NudgeLine = "zm: you have mail. Run: zm inbox"

// Service implements orchcore.SessionService and the /v0/sessions routes.
type Service struct {
	d *core.Deps

	// pushes: remote tasks' push checks (task_acted.go), by task id.
	pushes sync.Map

	// Probe measures the machine for admission control (tests replace it).
	Probe func(dirs ...string) orchapi.AdmissionProbe
	// PromptQuiet is how long the agent's output must be quiet before a
	// SpawnRequest.Prompt is typed.
	PromptQuiet time.Duration
	// NudgeEvery limits nudges per session.
	NudgeEvery time.Duration

	// HolderExe is the binary session holders run (default: this one).
	HolderExe string
	// pinnedExe: this daemon's clone of its own binary that new holders run
	// when it keeps sessions (holder_exe.go); "" runs the installed one.
	pinnedExe string

	outputIO        sync.RWMutex      // durable session-output holds vs delivery and final save
	outputRevisions map[string]uint64 // guarded by outputIO, invalidates cached stream bytes
	pipeIdx         sync.Map          // id8 -> *eventIndex (pipe.go)
	adapters        sync.Map          // adapter argv -> orchapi.AgentDescribe (pipe.go)

	spawnMu         sync.Mutex // admission + start are one step
	taskStoppingMu  sync.Mutex
	activeStops     map[string]int
	taskStopping    map[string]*taskStopLock
	taskMu          sync.Mutex
	taskIO          sync.Mutex
	taskDeliveryMu  sync.Mutex
	taskStopLocksMu sync.Mutex
	taskStopLocks   map[string]*taskStopGuard
	taskIDs         sync.Map
	sandboxStops    sync.Map // sandbox task ids being stopped (stopSandbox)
	homeIdentity    taskHomeIdentity
	TaskNoOutput    time.Duration
	// CloudPoll, when set, is every wait between a cloud task's status
	// checks (task_cloud.go; tests).
	CloudPoll   time.Duration
	taskDeliver func(context.Context, *authz.Subject, orchapi.Session, string, string, string) error

	keep        atomic.Bool // sessions outlive this daemon (KeepSessions, holders.go)
	unreachable sync.Map    // session8 -> why this daemon could not adopt its holder
	ties        sync.Map    // session8 -> a tied holder's stdin while its session is between adoptions
	adopting    sync.Map    // session8 -> *sync.Mutex: one readopt at a time

	mu    sync.Mutex
	procs map[string]*proc // live managed sessions by session8

	scopes sync.Map // address -> authz.Object (kept current when a session moves, move.go)
	// rcaps and remoteStartMu: sessions started for another machine
	// (pipe_remote.go).
	rcaps         remoteCaps
	remoteStartMu sync.Mutex
	// lingering: capped sessions' processes still alive after their row
	// let them go (caps.go).
	lingering lingerSet
	// followed: session8 -> the last hook cwd tried for a session in none (move.go).
	followed sync.Map
	// presenceScopes: a replica session's (project, area) at an authority
	// (presence may move it, RecordPresence updates it).
	presenceScopes sync.Map
	// elsewhere: replicas' sessions registered outside a project, as their
	// heartbeats list them (elsewhere.go).
	elsewhere elsewhereSet

	idents   sync.Map     // identity name or ULID -> identEntry (who.go)
	updates  updates      // coalesced dev.zm.session.updated per session (updates.go)
	askedAt  sync.Map     // session8 -> when the owner last read it (sample.go)
	sampleMu sync.Mutex   // one sample at a time: the cycle or a GET ?sample=1
	machine  machineCache // this machine's numbers (machine.go)
	// lastPrompts: address -> orchapi.SessionLastPrompt a replica typed into
	// its own shared session (the authority keeps the log; shared.go).
	lastPrompts sync.Map
	// hooked: session8 -> when a hook (or register) last reached an
	// unmanaged session; startedAt: this daemon's start (quietHooks).
	hooked    sync.Map
	startedAt time.Time
	// lastStep: session8 -> the subagent whose step came last (sidebar.go:
	// a wait a hook reports next is most likely its prompt).
	lastStep sync.Map

	vmu      sync.Mutex
	versions map[string]string // binary path + mtime + size -> `--version` line

	usage usageTracker // transcript tails for token totals (usage.go)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	runs   sync.WaitGroup // run goroutines: Close waits for them (bounded) before the store closes
}

var (
	_ orchcore.SessionService = (*Service)(nil)
	_ core.Module             = (*Service)(nil)
	_ core.Starter            = (*Service)(nil)
	_ core.Closer             = (*Service)(nil)

	_ orchcore.SessionWorkRecorder = (*Service)(nil)
)

// Migrations are this module's tables (core.Deps.AddMigrations); New adds them.
var Migrations = slices.Concat(storeMigrations, presenceMigrations, machineMigrations, taskMigrations, outputScrubMigrations, pipeRemoteMigrations, pipeHandoffMigrations, safeOutputsMigrations)

// New returns the session service.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{d: d, Probe: probeMachine, PromptQuiet: time.Second, NudgeEvery: 10 * time.Second,
		procs: map[string]*proc{}, outputRevisions: map[string]uint64{}, versions: map[string]string{}, TaskNoOutput: 5 * time.Minute, ctx: ctx, cancel: cancel}
	d.AddScrubber(s.scrubTasks)
	d.AddScrubber(s.scrubSessionOutputs)
	d.AddHolderCheck(s.holderLive)
	orchcore.AddOwedSource(d, s.owedSource())
	return s
}

// holderLive answers core.Deps.HolderLive for a session: live while it
// exists and has not ended, idle or not (run targets renew the boxes it
// holds).
func (s *Service) holderLive(ctx context.Context, a api.Address) (live, known bool) {
	if a.Kind != api.KindSession {
		return false, false
	}
	ss, ok := s.Lookup(ctx, a.String())
	return ok && ss.State != orchapi.StateEnded, true
}

// Start adopts the holders of managed sessions a previous daemon started
// (holders.go; a session whose holder is gone ends with the exit it left, else
// outcome infra "daemon restarted"), prunes old scrollback files and starts
// the idle timer and the unmanaged-session reaper. It does not block on
// anything but the adoption.
func (s *Service) Start(ctx context.Context) error {
	s.startedAt = time.Now()
	s.pinHolder()
	var err error
	if s.homeIdentity, err = taskHomeIdentityAt(s.d.Config.Home, true); err != nil {
		return err
	}
	if err := s.loadTaskIDs(ctx); err != nil {
		return err
	}
	s.reconcileTaskGroups(ctx)
	if err := s.readoptAll(ctx); err != nil {
		return err
	}
	s.recoverSessionOutputHolds(ctx)
	s.pruneScrollback(7 * 24 * time.Hour)
	s.pruneActivity()

	s.wg.Add(1)
	go func() { // off the start path: it asks systemctl
		defer s.wg.Done()
		if why := s.RestartNote(s.ctx); why != "" {
			s.d.Log.Warn("sessions will not survive this daemon: " + why)
		}
	}()
	s.wg.Add(1)
	go s.sampleLoop(ctx)
	s.wg.Add(1)
	go s.taskLoop()
	s.wg.Add(2)
	go s.usageLoop()
	go s.workLoop()

	tick := min(max(s.d.Config.IdleAfter()/4, 100*time.Millisecond), time.Second)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		last, rechecked, pruned := time.Now(), time.Now(), time.Now()
		for {
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			case <-s.ctx.Done():
				return
			}
			s.idleTimer()
			if time.Since(last) >= 15*time.Second {
				last = time.Now()
				s.reapUnmanaged()
				s.reapBound()
				s.quietHooks(time.Now())
				s.expireQuiet(time.Now())
				s.ReapPresence(s.ctx)
				s.prunePresence()
				s.prunePrompts()
			}
			if time.Since(pruned) >= activityPruneGap {
				pruned = time.Now()
				s.pruneActivity()
			}
			if time.Since(rechecked) >= recheckEvery {
				rechecked = time.Now()
				s.recheck(s.ctx)
				s.recoverSessionOutputHolds(s.ctx)
			}
		}
	}()
	return nil
}

// Close lets go of every session's holder when sessions outlive the daemon
// (KeepSessions): they keep running and the next daemon adopts them.
// Otherwise it ends them: TERM, then KILL after 2 s, outcome infra ("daemon
// stopped").
func (s *Service) Close() error {
	s.cancel()
	// Wait for an in-flight task launch before taking the live-process snapshot.
	s.taskMu.Lock()
	s.taskMu.Unlock()
	if !s.keep.Load() { // tied holders between adoptions end with this daemon
		s.ties.Range(func(k, t any) bool {
			_ = t.(io.Closer).Close()
			s.ties.Delete(k)
			return true
		})
	}
	ps := s.live()
	if s.keep.Load() {
		for _, p := range ps {
			p.detached.Store(true)
			_ = p.hc.Close()
			_ = p.pty.Close()
			p.closeConns() // attached clients see the stream end and attach again
		}
		s.wg.Wait()
		s.waitRuns(runsWait)
		return nil
	}
	var stoppable []*proc
	for _, p := range ps {
		if err := s.holderSignalAllowed(context.Background(), p.id8); err != nil {
			s.d.Log.Warn("session not signalled on daemon stop", "id", p.id8, "reason", err)
			p.detached.Store(true)
			_ = p.hc.Close()
			_ = p.pty.Close()
			p.closeConns()
			continue
		}
		stoppable = append(stoppable, p)
		p.mu.Lock()
		p.daemonStop = true
		p.mu.Unlock()
		_ = p.signal(syscall.SIGTERM)
	}
	waitAll(stoppable, 2*time.Second)
	for _, p := range stoppable {
		if !p.isExited() {
			_ = p.signal(syscall.SIGKILL)
		}
	}
	waitAll(stoppable, 3*time.Second)
	s.wg.Wait()
	s.waitRuns(runsWait)
	return nil
}

// runsWait bounds Close's wait for run goroutines: one still finishing a
// session writes to the store, which the daemon closes next.
const runsWait = 5 * time.Second

// goRun starts s.run(p), tracked for Close.
func (s *Service) goRun(p *proc) {
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		s.run(p)
	}()
}

func (s *Service) waitRuns(d time.Duration) {
	done := make(chan struct{})
	go func() { s.runs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		s.d.Log.Warn("session close: run goroutines still busy", "waited", d)
	}
}

func waitAll(ps []*proc, d time.Duration) {
	deadline := time.After(d)
	for _, p := range ps {
		select {
		case <-p.exited:
		case <-deadline:
			return
		}
	}
}

func (s *Service) live() []*proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*proc, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, p)
	}
	return out
}

func (s *Service) proc(id8 string) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[id8]
}

// id8Of turns a session8 or a full address on this machine into a session8.
func (s *Service) id8Of(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if strings.Contains(id, "/") {
		a, err := api.ParseAddress(id)
		if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine {
			return "", false
		}
		return a.Session, true
	}
	return id, api.ValidSession8(id)
}

// load returns a session by session8 or address, ignoring scope. It never
// reads a name: internal callers hold stored addresses, and a name would
// reach whichever session has it now (routes use loadRef).
func (s *Service) load(ctx context.Context, id string) (row, bool) {
	r, ok, err := s.loadErr(ctx, id)
	if err != nil {
		s.d.Log.Warn("session load", "id", id, "err", err)
	}
	return r, ok
}

// loadErr is load telling a missing session (false, nil) from a store that
// did not answer (an error).
func (s *Service) loadErr(ctx context.Context, id string) (row, bool, error) {
	id8, ok := s.id8Of(id)
	if !ok {
		return row{}, false, nil
	}
	db := s.d.Store.Registry()
	if db == nil { // Store.Close cleared it: a run finishing during daemon Close
		return row{}, false, errRegistryClosed
	}
	r, err := loadRow(ctx, db, id8)
	if errors.Is(err, errNoRow) {
		return row{}, false, nil
	}
	return r, err == nil, err
}

func object(ss orchapi.Session) authz.Object { return authz.Object{Project: ss.Project, Area: ss.Area} }

// visible loads id for sub: a session sub may not read is api.NotFound,
// byte-identical to a missing one.
func (s *Service) visible(ctx context.Context, sub *authz.Subject, id string) (row, error) {
	r, ok := s.loadRef(ctx, id)
	if !ok || !sub.Can(authz.Read, object(r.Session)) {
		return row{}, api.NotFound("session", id)
	}
	return r, nil
}

// readable is visible plus, on this machine's socket, the process check
// (capGate) for a read: a capped process reads only its own sessions or
// ones below it, however it names the session.
func (s *Service) readable(ctx context.Context, sub *authz.Subject, id string) (row, error) {
	r, err := s.visible(ctx, sub, id)
	if err != nil {
		return r, err
	}
	if core.IsLocal(ctx) && !s.capGate(sub).allows(ctx, r, authz.Read, true) {
		return row{}, api.NotFound("session", id)
	}
	return r, nil
}

// operable is visible plus the operate check (403 only for sessions sub can
// see). On this machine's socket it is also the process check (capGate): a
// capped process operates only its own sessions or ones below it, however
// it names the session.
func (s *Service) operable(ctx context.Context, sub *authz.Subject, id string) (row, error) {
	return s.operableBy(ctx, sub, s.capGate(sub), id)
}

// operableBy is operable with the caller's gate (one lookup for many ids).
func (s *Service) operableBy(ctx context.Context, sub *authz.Subject, gate *capGate, id string) (row, error) {
	r, err := s.visible(ctx, sub, id)
	if err != nil {
		return r, err
	}
	if core.IsLocal(ctx) && !gate.allows(ctx, r, authz.Operate, true) {
		return row{}, api.NotFound("session", id)
	}
	return r, sub.Check(authz.Operate, object(r.Session), "session", id)
}

// update changes a stored session, keeps the live proc's state in sync and
// publishes typ (when non-empty) if anything changed.
func (s *Service) update(ctx context.Context, id8, typ string, fn func(*row) (bool, error)) (orchapi.Session, error) {
	r, changed, err := updateRow(ctx, s.d.Store.Registry(), id8, s.keepCap(fn))
	if err != nil {
		return orchapi.Session{}, err
	}
	if changed {
		if p := s.proc(id8); p != nil {
			p.mu.Lock()
			p.state = r.State
			p.mu.Unlock()
		}
		if typ != "" {
			s.publish(typ, r.Session)
		}
	}
	return r.Session, nil
}

// activityOnly is publish's type for a dev.zm.session.updated whose only
// change is the activity (activity, activity_at): it goes to live
// subscribers but is never stored (SPEC 12), so an agent's step-by-step
// activity does not fill the replayable log.
const activityOnly = "activity-only"

// publish publishes a session event. dev.zm.session.updated (activity,
// term_title, samples) is coalesced per session (updates.go), and stored
// unless every change it carries was activityOnly; any other event goes
// out at once and makes a pending updated for the session unnecessary.
func (s *Service) publish(typ string, ss orchapi.Session) {
	if typ == orchapi.EventSessionUpdated || typ == activityOnly {
		s.publishUpdated(ss, typ == activityOnly)
		return
	}
	s.updates.settle(ss.Address, typ == orchapi.EventSessionEnded)
	s.publishNow(typ, ss)
	if typ == orchapi.EventSessionStarted {
		s.releaseUpdated(ss.Address)
	}
}

// logOf is the log a session event of project/area goes to.
func (s *Service) logOf(proj, area string) (string, string) {
	if proj != api.ProjectNone && proj != "" && s.d.Projects.Slug(proj) == "" {
		// A session on a replica in a team project this machine joined (SPEC
		// 17.3): the project has no log here, so the event goes to the
		// machine log, which only this machine's owner reads.
		return api.ProjectNone, ""
	}
	return proj, area
}

// publishLeft tells the readers of the project/area a session moved out of
// that it left, and nothing about where it went. Its address is not a
// mention: they saw the session there.
func (s *Service) publishLeft(address, project, area string) {
	proj, ar := s.logOf(project, area)
	if _, err := s.d.Events.Publish(context.Background(), events.Input{Type: orchapi.EventSessionLeft, Subject: address,
		Project: proj, Area: ar, Data: orchapi.SessionLeft{Address: address, Project: project, Area: area}}); err != nil {
		s.d.Log.Warn("session event", "type", orchapi.EventSessionLeft, "err", err)
	}
}

// publishNow publishes a session event as it is, stored.
func (s *Service) publishNow(typ string, ss orchapi.Session) { s.publishEvent(typ, ss, false) }

// publishEvent publishes a session event; live: to live subscribers only,
// not stored (events.Input.Live).
func (s *Service) publishEvent(typ string, ss orchapi.Session, live bool) {
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	proj, area := s.logOf(ss.Project, ss.Area)
	ss = s.dress(context.Background(), ss)
	ownerOnly := orchapi.OwnerOnlySessionKeys // fg, busy, ports, jobs, unreachable: this machine's owner only
	if ss.TermTitle != "" && !orchapi.SharesTermTitle(ss.Agent, ss.Command) {
		ownerOnly = append(slices.Clip(ownerOnly), "term_title") // a shell's title: never to a token or tailnet reader
	}
	if ss.Parent != "" {
		ownerOnly = append(slices.Clip(ownerOnly), "parent") // an event cannot check each reader's view of the parent; reads do
	}
	if _, err := s.d.Events.Publish(context.Background(), events.Input{Type: typ, Subject: ss.Address,
		Project: proj, Area: area, Data: ss, Mentions: []string{ss.Address}, OwnerOnly: ownerOnly,
		MembersOnly: membersOnlySessionKeys, Live: live}); err != nil {
		s.d.Log.Warn("session event", "type", typ, "err", err)
	}
}

// ---- orchcore.SessionService ----

// Lookup implements orchcore.SessionService (ignores scope).
func (s *Service) Lookup(ctx context.Context, address string) (orchapi.Session, bool) {
	ss, ok, err := s.LookupErr(ctx, address)
	if err != nil {
		s.d.Log.Warn("session lookup", "address", address, "err", err)
	}
	return ss, ok
}

// LookupErr implements orchcore.SessionLookupErr: Lookup telling a missing
// session (false, nil) from a store that did not answer (an error), so a
// caller can wait for the next try instead of taking the session for gone.
func (s *Service) LookupErr(ctx context.Context, address string) (orchapi.Session, bool, error) {
	if s.remoteAddr(address) {
		return s.loadPresenceErr(ctx, address)
	}
	r, ok, err := s.loadErr(ctx, address)
	if err == nil && ok {
		r.Session = s.maskSessionOutputMetadata(ctx, r.Session)
	}
	return r.Session, ok, err
}

// Visible implements orchcore.SessionService: the session's project/area is
// readable with sc. Unknown sessions are invisible to restricted readers.
func (s *Service) Visible(ctx context.Context, sc authz.Scope, address string) bool {
	if sc.IsAll() {
		return true
	}
	if s.remoteAddr(address) {
		return s.presenceVisible(ctx, sc, address) || s.elsewhereVisible(ctx, sc, address)
	}
	if o, ok := s.scopes.Load(address); ok {
		return sc.Can(authz.Read, o.(authz.Object))
	}
	r, ok := s.load(ctx, address)
	if !ok {
		return false
	}
	o := object(r.Session)
	s.scopes.Store(r.Address, o)
	return sc.Can(authz.Read, o)
}

// Live implements orchcore.SessionService.
func (s *Service) Live(ctx context.Context, address string) bool {
	if s.remoteAddr(address) {
		ss, ok := s.loadPresence(ctx, address)
		return ok && ss.State != orchapi.StateEnded
	}
	r, ok := s.load(ctx, address)
	return ok && r.State != orchapi.StateEnded
}

// SetState implements orchcore.SessionService (mail sets needs_human and clears it).
func (s *Service) SetState(ctx context.Context, address string, req orchapi.StateRequest) error {
	r, ok := s.load(ctx, address)
	if !ok {
		return api.NotFound("session", address)
	}
	_, err := s.setState(ctx, r.ID, req.State, req.Note, false, "", orchapi.SessionNeeds{Kind: orchapi.NeedsDecision}, nil)
	return err
}

// setState records a reported state; hook says it came from an agent hook
// (POST state, register), not mail. It clears the idle timer's mark, so only
// a timer-set idle is undone by output alone. A hook-set needs_human (a
// permission prompt) is undone by human input followed by output; a
// mail-set one only by mail. A hook's activity ("" = none) is recorded in
// the same write, and a hook reporting idle with none clears it: one state_changed event when the state or note changed,
// else one updated event when the activity did, else none. needs is why a
// needs_human state waits (its Kind "" = manual); mut (may be nil)
// is a further change in the same write, reporting the event it is worth.
func (s *Service) setState(ctx context.Context, id8 string, state orchapi.SessionState, note string, hook bool, activity string, needs orchapi.SessionNeeds, mut func(*row, time.Time) int) (orchapi.Session, error) {
	switch state {
	case orchapi.StateRunning, orchapi.StateIdle, orchapi.StateNeedsHuman:
	default:
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "state must be running, idle or needs_human, not %q", state).
			WithHint("a session ends when its process exits (zm session stop)")
	}
	note = oneLine(note)
	if p := s.proc(id8); p != nil && p.pipe != nil && hook {
		// A structured session's stream decides its state; its runtime's
		// own hooks (user settings) do not.
		r, ok := s.load(ctx, id8)
		if !ok {
			return orchapi.Session{}, api.NotFound("session", id8)
		}
		return r.Session, nil
	}
	if p := s.proc(id8); p != nil {
		p.mu.Lock()
		p.timerIdle = false
		p.hookHuman, p.answered = hook && state == orchapi.StateNeedsHuman, false
		p.stateGen++
		p.mu.Unlock()
	}
	if state == orchapi.StateNeedsHuman && needs.Kind == "" {
		needs.Kind = orchapi.NeedsManual
	}
	typ, logged := "", false
	ss, err := s.update(ctx, id8, "", func(r *row) (bool, error) {
		if r.State == orchapi.StateEnded {
			return false, api.Errorf(api.CodeConflict, "session %s has ended", r.Address)
		}
		now := time.Now().UTC()
		wrote, newText := applyActivity(r, activity, now)
		logged = newText
		if hook && state == orchapi.StateIdle && activity == "" && r.Activity != "" {
			// The agent stopped (Claude's Stop hook, Codex notify): it is doing
			// nothing now, so an idle row never shows its last step.
			r.Activity, r.ActivityAt = "", nil
			wrote, newText = true, true
		}
		ch := chNone
		if mut != nil {
			ch = mut(r, now)
		}
		needsChanged := false
		if state == orchapi.StateNeedsHuman && (r.Needs == nil || r.Needs.Kind != needs.Kind || r.Needs.Subagent != needs.Subagent) {
			needs.At = storedTime(now)
			r.Needs, needsChanged = &needs, true
		}
		switch {
		case r.State != state || r.Note != note || needsChanged || ch == chState:
			r.State, r.Note, typ = state, note, orchapi.EventSessionStateChanged
		case ch == chDurable:
			typ = orchapi.EventSessionUpdated
		case newText || ch == chActivity:
			typ = activityOnly
		}
		return typ != "" || wrote || ch != chNone, nil
	})
	if err == nil && typ != "" {
		s.publish(typ, ss)
	}
	if err == nil && logged {
		s.logActivity(id8, "", activity, time.Now())
	}
	return ss, err
}

// activityRefresh is how often a repeated activity bumps activity_at (a row
// write, no event): an agent running one search after another is still
// searching.
const activityRefresh = 30 * time.Second

// applyActivity records a hook's activity on r. wrote: r changed (a new
// text, or a repeat refreshing activity_at past activityRefresh); newText:
// the text itself changed (worth an event).
func applyActivity(r *row, activity string, now time.Time) (wrote, newText bool) {
	if activity == "" || (activity == r.Activity && r.ActivityAt != nil && now.Sub(*r.ActivityAt) < activityRefresh) {
		return false, false
	}
	newText = activity != r.Activity
	r.Activity, r.ActivityAt = activity, &now
	return true, newText
}

// setActivity records a hook's activity alone (no state), and mut (may be
// nil: setState's) in the same write.
func (s *Service) setActivity(ctx context.Context, id8, activity string, mut func(*row, time.Time) int) (orchapi.Session, error) {
	typ, logged := "", false
	ss, err := s.update(ctx, id8, "", func(r *row) (bool, error) {
		if r.State == orchapi.StateEnded {
			return false, nil
		}
		now := time.Now().UTC()
		wrote, newText := applyActivity(r, activity, now)
		logged = newText
		ch := chNone
		if mut != nil {
			ch = mut(r, now)
		}
		switch {
		case ch == chState:
			typ = orchapi.EventSessionStateChanged
		case ch == chDurable:
			typ = orchapi.EventSessionUpdated
		case newText || ch == chActivity:
			typ = activityOnly
		}
		return wrote || ch != chNone, nil
	})
	if err == nil && typ != "" {
		s.publish(typ, ss)
	}
	if err == nil && logged {
		s.logActivity(id8, "", activity, time.Now())
	}
	return ss, err
}

// Nudge implements orchcore.SessionService: it types one fixed line (never a
// mail body) into an idle managed session nobody can be typing in. ok=false
// when the session is not managed, not idle, has a client attached that can
// type, holds keys a person has not sent (draft.go), or was nudged within
// NudgeEvery.
func (s *Service) Nudge(ctx context.Context, address, line string) (bool, error) {
	r, ok := s.load(ctx, address)
	if !ok {
		return false, api.NotFound("session", address)
	}
	p := s.proc(r.ID)
	if p == nil || p.pipe != nil { // never typed into a structured session
		return false, nil
	}
	if !p.mayNudge() {
		return false, nil
	}
	p.mu.Lock()
	ok = time.Since(p.lastNudge) >= s.NudgeEvery
	if ok {
		p.lastNudge = time.Now()
	}
	p.mu.Unlock()
	if !ok {
		return false, nil
	}
	// Only zm's own fixed lines are ever typed.
	if line = oneLine(line); !strings.HasPrefix(line, "zm: ") || len(line) > orchapi.MaxNoticeLen {
		line = NudgeLine
	}
	go func() { _ = p.typeNudge(line) }()
	return true, nil
}

// ManagedLive implements orchcore.SessionService.
func (s *Service) ManagedLive(ctx context.Context) int {
	n := 0
	s.unreachable.Range(func(any, any) bool { n++; return true }) // they run too
	s.mu.Lock()
	defer s.mu.Unlock()
	return n + len(s.procs)
}

// RestartBlockers implements orchcore.SessionService: the sessions a restart
// would end. A kept session runs in its holder and survives (holders.go),
// and so does one this daemon cannot reach, unless a systemd unit would kill
// its holder along with the daemon (RestartNote); a daemon that ties its
// holders (tests, in-process) ends them with itself.
func (s *Service) RestartBlockers(ctx context.Context) []orchapi.Session {
	if s.keep.Load() && s.RestartNote(ctx) == "" {
		return []orchapi.Session{}
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.procs))
	for id8 := range s.procs {
		ids = append(ids, id8)
	}
	s.mu.Unlock()
	slices.Sort(ids)
	out := []orchapi.Session{}
	for _, id8 := range ids {
		if ss, ok := s.Lookup(ctx, api.SessionAddress(s.d.Config.Machine, id8)); ok && ss.State != orchapi.StateEnded {
			out = append(out, ss)
		}
	}
	return out
}

// ---- ending ----

// end records a session's end with outcome o, publishes ended and releases
// its bound leases. Ending an ended session does nothing.
func (s *Service) end(ctx context.Context, id8 string, o orchapi.Outcome, mut ...func(*row)) (orchapi.Session, bool) {
	return s.endWithGroup(ctx, id8, o, true, mut...)
}

func (s *Service) endWithGroup(ctx context.Context, id8 string, o orchapi.Outcome, signalGroup bool, mut ...func(*row)) (orchapi.Session, bool) {
	now := time.Now().UTC()
	did := false
	ss, err := s.update(ctx, id8, orchapi.EventSessionEnded, func(r *row) (bool, error) {
		if r.State == orchapi.StateEnded {
			return false, nil
		}
		did = true
		r.State, r.Outcome, r.Ended = orchapi.StateEnded, &o, &now
		r.Note = string(o.Kind)
		if o.Note != "" {
			r.Note += ": " + o.Note
		}
		for _, m := range mut {
			m(r)
		}
		return true, nil
	})
	if err != nil {
		s.d.Log.Warn("session end", "session", id8, "err", err)
		return ss, false
	}
	s.scopes.Store(ss.Address, object(ss))
	if did {
		s.unreachable.Delete(id8)
		s.lastStep.Delete(id8)
		s.usageEnded(id8)
	}
	if did && ss.Shared {
		s.expirePrompts(ctx, ss.Address)
		s.lastPrompts.Delete(ss.Address)
	}
	if did && (o.Signal != "" || (o.ExitCode != nil && *o.ExitCode != 0)) {
		s.cancelTaskChildren(ss.Address)
	}
	if did && signalGroup {
		s.killRecordedTaskGroup(ctx, id8)
	}
	if did && orchcore.Work(s.d) != nil {
		if err := orchcore.Work(s.d).SessionEnded(context.Background(), ss.Address); err != nil && !api.IsCode(err, api.CodeNotImplemented) {
			s.d.Log.Warn("release bound leases", "session", ss.Address, "err", err)
		}
	}
	return ss, did
}

// idleTimer marks quiet running managed sessions idle ("no output").
func (s *Service) idleTimer() {
	after := s.d.Config.IdleAfter()
	for _, p := range s.live() {
		if p.pipe != nil {
			continue // its stream says when it is idle
		}
		p.mu.Lock()
		last := p.lastOut
		if last.IsZero() {
			last = p.started
		}
		quiet := p.state == orchapi.StateRunning && time.Since(last) >= after
		p.mu.Unlock()
		if !quiet {
			continue
		}
		ss, err := s.update(s.ctx, p.id8, orchapi.EventSessionStateChanged, func(r *row) (bool, error) {
			if r.State != orchapi.StateRunning {
				return false, nil
			}
			r.State, r.Note = orchapi.StateIdle, "no output"
			return true, nil
		})
		if err == nil && ss.State == orchapi.StateIdle && ss.Note == "no output" {
			p.mu.Lock()
			p.timerIdle = true
			p.mu.Unlock()
		}
	}
}

// quietHooks sets idle (note "no hooks") a running hook session that no
// hook has reached for bound_lease_ttl: its agent may be gone without a
// session-end hook and with no pid to reap (a crash, a pid zm cannot watch),
// and a running holder's bound leases are renewed for ever. The next hook
// makes it running again.
func (s *Service) quietHooks(now time.Time) {
	rows, err := listRows(s.ctx, s.d.Store.Registry(), `WHERE managed = 0 AND state = ?`, string(orchapi.StateRunning))
	if err != nil {
		return
	}
	quiet := s.d.Config.BoundLeaseTTL()
	for _, r := range rows {
		last := r.Updated
		if s.startedAt.After(last) {
			last = s.startedAt
		}
		if r.ActivityAt != nil && r.ActivityAt.After(last) {
			last = *r.ActivityAt
		}
		if t, ok := s.hooked.Load(r.ID); ok && t.(time.Time).After(last) {
			last = t.(time.Time)
		}
		if now.Sub(last) < quiet {
			continue
		}
		_, _ = s.update(s.ctx, r.ID, orchapi.EventSessionStateChanged, func(x *row) (bool, error) {
			if x.State != orchapi.StateRunning {
				return false, nil
			}
			x.State, x.Note = orchapi.StateIdle, "no hooks"
			return true, nil
		})
	}
}

func (s *Service) scrollbackPath(id8 string) string {
	return filepath.Join(s.d.Config.Home, "sessions", id8+".out")
}

func (s *Service) saveScrollback(id8 string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.scrollbackPath(id8)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.scrollbackPath(id8), data, 0o600)
}

func (s *Service) pruneScrollback(age time.Duration) {
	dir := filepath.Join(s.d.Config.Home, "sessions")
	ents, _ := os.ReadDir(dir)
	live := map[string]bool{}
	for _, p := range s.live() {
		live[p.id8] = true
	}
	for _, e := range ents {
		// Directories at task output paths are permanent discard barriers.
		if e.IsDir() {
			continue
		}
		// A live session's files (a pipe session's event spool) stay
		// however old: an idle session writes nothing for days.
		if id, _, ok := strings.Cut(e.Name(), "."); ok && (live[id] || s.unreachableWhy(id) != "") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
