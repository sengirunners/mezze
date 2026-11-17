package session

// Session caps (SPEC 1, rule 3): a session started for another machine is
// capped to one project and role, and the cap follows it however it reaches
// the daemon: by its process, by Zm-As, through a session it registers or
// starts, and along its parent chain.

import (
	"context"
	"database/sql"
	"sync"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// processCaller is the caller as the session its process runs in (when it
// runs in one; origin), else as given (Zm-As, or none), narrowed by every
// cap along that session's parent chain (capChain). Every route that finds
// its caller by process uses it, so a capped session acts within its cap
// however it reached the daemon (SPEC 1.3).
func (s *Service) processCaller(ctx context.Context, sub *authz.Subject) (*authz.Subject, string, error) {
	return s.callerFrom(ctx, sub, s.taskOrigin)
}

// hookCaller is processCaller for a hook: found without spawnMu when it can
// be (hookOrigin).
func (s *Service) hookCaller(ctx context.Context, sub *authz.Subject) (*authz.Subject, string, error) {
	return s.callerFrom(ctx, sub, s.hookOrigin)
}

func (s *Service) callerFrom(ctx context.Context, sub *authz.Subject, find func(context.Context, *authz.Subject) (string, error)) (*authz.Subject, string, error) {
	origin, err := find(ctx, sub)
	if err != nil {
		return nil, "", err
	}
	if origin != "" && origin != sub.Session {
		sub = sub.ForSession(origin)
	}
	return s.capChain(ctx, sub), origin, nil
}

// capChainMax bounds the parent chain walked for caps.
const capChainMax = 32

// chainCaps are the caps of session address and of its parents on this
// machine (session_lineage), nearest first.
func (s *Service) chainCaps(ctx context.Context, address string) []sessionCap {
	var out []sessionCap
	seen := map[string]bool{}
	for i := 0; i < capChainMax && address != "" && !seen[address]; i++ {
		seen[address] = true
		if p, r, ok := s.SessionCap(address); ok {
			out = append(out, sessionCap{p, r})
		}
		a, err := api.ParseAddress(address)
		if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine {
			break
		}
		var parent string
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT parent FROM session_lineage WHERE id = ?`, a.Session).Scan(&parent) != nil {
			break
		}
		address = parent
	}
	return out
}

// capChain is sub narrowed by every cap along its session's chain: the
// narrowest of them all (two caps on different projects leave nothing).
func (s *Service) capChain(ctx context.Context, sub *authz.Subject) *authz.Subject {
	if sub == nil || sub.Session == "" {
		return sub
	}
	caps := s.chainCaps(ctx, sub.Session)
	if len(caps) == 0 {
		return sub
	}
	c := *sub
	for _, k := range caps {
		c.Scope = c.Scope.Capped(k.project, k.role)
	}
	return &c
}

// inheritedCap is the cap a session started or registered by a capped
// session takes: the nearest along its chain (chainCaps), so a child never
// acts beyond it (nil: not capped).
func (s *Service) inheritedCap(ctx context.Context, sub *authz.Subject) *remotePin {
	if sub == nil || sub.Session == "" {
		return nil
	}
	caps := s.chainCaps(ctx, sub.Session)
	if len(caps) == 0 {
		return nil
	}
	return &remotePin{project: caps[0].project, role: caps[0].role, asker: "inherited from " + sub.Session}
}

// spawnCaller is spawnFor's caller: the subject in ctx, narrowed by its
// caps when the spawn is its own (no task, no pin), the cap its child
// inherits, and the child's lineage parent and depth.
func (s *Service) spawnCaller(ctx context.Context, task *taskStart, pin *remotePin) (context.Context, *authz.Subject, *remotePin, string, int, error) {
	sub := core.SubjectOf(ctx)
	capFrom := sub
	if task == nil && pin == nil {
		// Found by its process or by Zm-As, a capped caller spawns within
		// its cap (processCaller), keeping the address it acts as.
		caller, _, err := s.processCaller(ctx, sub)
		if err != nil {
			return ctx, nil, nil, "", 0, err
		}
		capFrom = caller
		c := *sub
		c.Scope = caller.Scope
		sub = &c
		ctx = core.WithSubject(ctx, sub)
	}
	inherited := s.inheritedCap(ctx, capFrom) // a capped session's child stays capped (SPEC 1.3)
	lineageParent, lineageDepth, err := s.spawnLineage(ctx, sub, task)
	return ctx, sub, inherited, lineageParent, lineageDepth, err
}

// mayRebind: a caller with no caps along its chain may bind, revive or move
// the pid of any session it may write; a capped one only its own session or
// one below it (session_lineage), which carries its caps already. Never
// another session: being found as it by process would shed the cap.
func (s *Service) mayRebind(ctx context.Context, caller *authz.Subject, target string) bool {
	if caller == nil || caller.Session == "" || len(s.chainCaps(ctx, caller.Session)) == 0 {
		return true
	}
	seen := map[string]bool{}
	for i := 0; i < capChainMax && target != "" && !seen[target]; i++ {
		if target == caller.Session {
			return true
		}
		seen[target] = true
		a, err := api.ParseAddress(target)
		if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine {
			return false
		}
		var parent string
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT parent FROM session_lineage WHERE id = ?`, a.Session).Scan(&parent) != nil {
			return false
		}
		target = parent
	}
	return false
}

// capGate is the process check of a write to an existing session (SPEC 1,
// rule 3): the caller found by its process, within
// its caps, acts only on its own session or one below it (mayRebind), so a
// capped process naming another session's id, with or without Zm-As,
// cannot write it. Not looked up while no capped session runs here
// (anyLiveCaps), looked up once per gate, and without spawnMu when it can
// be (hookCaller); a failed lookup fails closed.
type capGate struct {
	s                *Service
	sub, caller      *authz.Subject
	err              error
	looked, capsLive bool
}

func (s *Service) capGate(sub *authz.Subject) *capGate { return &capGate{s: s, sub: sub} }

func (g *capGate) lookup(ctx context.Context) {
	if !g.looked {
		g.looked = true
		if g.capsLive = g.s.anyLiveCaps(ctx); g.capsLive {
			g.caller, _, g.err = g.s.hookCaller(ctx, g.sub)
		}
	}
}

// allows reports whether the caller may act (rel) on r past its caps;
// self: its own session needs no rel.
func (g *capGate) allows(ctx context.Context, r row, rel authz.Action, self bool) bool {
	g.lookup(ctx)
	if !g.capsLive {
		return true
	}
	if g.err != nil || g.caller == nil {
		return false
	}
	return (self && g.caller.Session == r.Address || g.caller.Can(rel, object(r.Session))) && g.s.mayRebind(ctx, g.caller, r.Address)
}

// uncapped reports whether the caller carries no cap at all (for a write
// with no row here: another machine's session).
func (g *capGate) uncapped(ctx context.Context) bool {
	g.lookup(ctx)
	if !g.capsLive {
		return true
	}
	return g.err == nil && g.caller != nil && (g.caller.Session == "" || len(g.s.chainCaps(ctx, g.caller.Session)) == 0)
}

// anyLiveCaps reports whether a capped session here is still running, or
// a process one recorded (lingering). One indexed query, and none on a
// machine never capped.
func (s *Service) anyLiveCaps(ctx context.Context) bool {
	if !s.anyCaps() {
		return false
	}
	var one int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT 1 FROM session_remote_starts r JOIN sessions s ON s.id = r.id
		WHERE s.state != ? LIMIT 1`, string(orchapi.StateEnded)).Scan(&one)
	if err != sql.ErrNoRows {
		return true // an error fails closed: checked
	}
	// Only after the rows: a row that ended since was put in the set
	// before its end committed (keepCap), so it is in one or the other.
	return s.anyLingering()
}

// anyCaps reports whether any session here is capped (none: no caller can
// be, and no check needs to find one).
func (s *Service) anyCaps() bool {
	m, loaded := s.capsLoaded()
	if !loaded {
		return true // unknown: fails closed
	}
	found := false
	m.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}

// A capped session's cap outlives its row's hold on a process:
// when the row ends while the process it recorded
// lives, that process is still found as the session (taskOrigin,
// hookOrigin) and so still within its caps, until it exits; the row never
// moves its pid off that process (keepCap).
// Otherwise a capped agent would shed its cap by ending its own session.

type lingerProc struct {
	address string
	start   int64 // pid_start (0: unknown)
}

// lingerSet: pid -> the capped sessions that recorded it and ended since,
// whose process may still run. Loaded from the registry (a daemon restart
// keeps them) until a load succeeds, pruned as their processes exit.
type lingerSet struct {
	loadMu sync.Mutex // one load at a time
	mu     sync.Mutex
	loaded bool
	pids   map[int][]lingerProc
}

// lingerLoaded loads the set once; an error leaves it unloaded (retried
// next time), and its callers fail closed.
func (s *Service) lingerLoaded() (*lingerSet, error) {
	l := &s.lingering
	l.mu.Lock()
	done := l.loaded
	l.mu.Unlock()
	if done {
		return l, nil
	}
	l.loadMu.Lock()
	defer l.loadMu.Unlock()
	l.mu.Lock()
	done = l.loaded
	l.mu.Unlock()
	if done {
		return l, nil
	}
	m := map[int][]lingerProc{}
	// Not the caller's context: a cancelled request must not leave the set
	// half loaded.
	rows, err := s.d.Store.Registry().QueryContext(context.Background(), `SELECT s.pid, s.pid_start, s.id FROM session_remote_starts r JOIN sessions s ON s.id = r.id
		WHERE s.state = ? AND s.pid > 1`, string(orchapi.StateEnded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		var start int64
		var id string
		if err := rows.Scan(&pid, &start, &id); err != nil {
			return nil, err
		}
		if lingerAlive(pid, start) {
			// Recorded with no start (before the start was recorded): the process
			// at pid now, never a later one.
			m[pid] = append(m[pid], lingerProc{api.SessionAddress(s.d.Config.Machine, id), pinStart(pid, start)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	for pid, ps := range l.pids { // kept before the load finished
		m[pid] = append(m[pid], ps...)
	}
	l.pids, l.loaded = m, true
	l.mu.Unlock()
	return l, nil
}

// lingerAlive reports whether pid is still the process recorded with start
// (start 0: any process at pid, failing closed).
func lingerAlive(pid int, start int64) bool {
	st, ok := procStat(pid)
	return ok && !st.zombie && (start == 0 || st.start == start)
}

// keepCap wraps an update of a row: a capped row that ends while the
// process it recorded lives leaves that process lingering, recorded
// before the write commits so no request between sees it uncapped.
// A capped row never moves its pid off a live process (a revive onto
// another pid would leave the old one with no record of its cap after a
// restart): that update is refused.
func (s *Service) keepCap(fn func(*row) (bool, error)) func(*row) (bool, error) {
	s.capsLoaded() // loaded before the write's transaction holds the registry
	return func(r *row) (bool, error) {
		loaded := s.rcaps.loaded.Load() // as of now: a load may have finished since
		live, pid, start := r.State != orchapi.StateEnded, r.PID, r.pidStart
		changed, err := fn(r)
		if err != nil || !changed {
			return changed, err
		}
		if _, _, capped := s.capKnown(r.Address, loaded); !capped {
			return changed, err
		}
		if r.PID > 1 && r.pidStart == 0 {
			// A capped row's process is always recorded with its start, so
			// its cap never follows a later process at a reused pid.
			r.pidStart = pinStart(r.PID, 0)
		}
		if pid <= 1 || r.PID == pid && r.pidStart == start && (!live || r.State != orchapi.StateEnded) || !lingerAlive(pid, start) {
			return changed, err
		}
		if r.PID != pid {
			return false, api.Errorf(api.CodeConflict, "session %s is capped and its process %d still runs: it keeps that process", r.Address, pid)
		}
		if live && r.State == orchapi.StateEnded {
			s.addLingering(pid, lingerProc{r.Address, pinStart(pid, start)})
		}
		return changed, err
	}
}

// pinStart is start, or when unknown (0) the start of the process at pid
// now (0: none readable).
func pinStart(pid int, start int64) int64 {
	if start != 0 {
		return start
	}
	if st, ok := procStat(pid); ok && !st.zombie {
		return st.start
	}
	return 0
}

// addLingering records p at pid, dropping the entries whose processes
// exited, so the set stays as large as its live processes.
func (s *Service) addLingering(pid int, p lingerProc) {
	l := &s.lingering
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pids == nil {
		l.pids = map[int][]lingerProc{}
	}
	l.pruneLocked()
	l.pids[pid] = append(l.pids[pid], p)
}

// pruneLocked drops the entries whose processes exited (l.mu held) and
// reports whether any is left.
func (l *lingerSet) pruneLocked() bool {
	left := false
	for pid, ps := range l.pids {
		kept := ps[:0]
		for _, p := range ps {
			if lingerAlive(pid, p.start) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(l.pids, pid)
			continue
		}
		l.pids[pid] = kept
		left = true
	}
	return left
}

// lingerAt is the capped session still holding pid, whose start is start
// (0: unknown), if any ("": none). A set that cannot load is an error.
func (s *Service) lingerAt(pid int, start int64) (string, error) {
	if !s.anyCaps() {
		return "", nil
	}
	l, err := s.lingerLoaded()
	if err != nil {
		return "", api.Errorf(api.CodeUnavailable, "cannot load capped sessions' processes: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.pids[pid][:0]
	found := ""
	for _, p := range l.pids[pid] {
		if !lingerAlive(pid, p.start) {
			continue // exited: dropped
		}
		kept = append(kept, p)
		if found == "" && (p.start == 0 || start == 0 || p.start == start) {
			found = p.address
		}
	}
	if len(kept) == 0 {
		delete(l.pids, pid)
	} else {
		l.pids[pid] = kept
	}
	return found, nil
}

// anyLingering reports whether a lingering process still runs, dropping
// the ones that exited; a set that cannot load counts as one (fails
// closed).
func (s *Service) anyLingering() bool {
	l, err := s.lingerLoaded()
	if err != nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pruneLocked()
}
