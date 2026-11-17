package session

// Hook sessions' agent processes (SPEC 10, "bound leases"): a session zm
// did not start ends when its agent exits without a session-end hook
// (Codex never sends one). Each hook sends its agent's pid
// (integrations.AgentPID); the daemon records it with the process's start
// (procStat), checks it every 15 s (reapUnmanaged), and ends sessions it
// has no process to watch for once they go quiet (expireQuiet).

import (
	"errors"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// startSlack absorbs clock granularity when a process's start is compared
// with a session's (Linux counts from a boot time in whole seconds).
const startSlack = 2 * time.Second

// agentLive reports whether pid is a live process here (not a zombie).
// pid <= 1 is never an agent: a hook reparented to init or launchd.
func agentLive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if p, ok := procStat(pid); ok {
		return !p.zombie
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// recordedLive reports whether r's recorded agent still runs (true when
// none is recorded: nothing says it is gone).
func recordedLive(r row) bool {
	if r.PID <= 1 {
		return true
	}
	gone, _ := processGone(r.PID, r.pidStart, r.Started)
	return !gone
}

// takePID records a hook's agent pid on r, and reports whether r changed.
// reviving: this hook revives an ended session (a new run starts now). It
// takes only a live process (never pid 1); never replaces a recorded pid
// whose process still runs; and only takes a process that started at or
// before the session did (a reviving run: before this hook), so a process
// spawned later (a stray child, a reused pid) is never mistaken for the
// agent.
func takePID(r *row, pid int, reviving bool, now time.Time) bool {
	if pid <= 1 {
		return false
	}
	p, ok := procStat(pid)
	if ok && p.zombie || !ok && !agentLive(pid) {
		return false
	}
	if pid == r.PID && (p.start == r.pidStart || !ok) {
		return false
	}
	if r.PID > 1 && r.PID != pid && !reviving {
		if gone, _ := processGone(r.PID, r.pidStart, r.Started); !gone {
			return false
		}
	}
	ref := r.Started
	if reviving || ref.IsZero() {
		ref = now
	}
	if !p.started.IsZero() && p.started.After(ref.Add(startSlack)) {
		return false
	}
	r.PID, r.pidStart = pid, p.start
	return true
}

// processGone reports whether a hook session's recorded process (pid,
// start: procStat's start, 0 when recorded without one) has exited: no
// such process, a zombie, or the pid now belongs to another process (a
// different start; with none recorded, one that started after the session).
func processGone(pid int, start int64, sessionStarted time.Time) (bool, string) {
	p, ok := procStat(pid)
	switch {
	case !ok && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH), ok && p.zombie:
		return true, "process gone"
	case !ok:
		return false, ""
	case start != 0 && p.start != start,
		start == 0 && !p.started.IsZero() && p.started.After(sessionStarted.Add(startSlack)):
		return true, "process gone (its pid now belongs to another process)"
	}
	return false, ""
}

// reapUnmanaged ends registered sessions whose process is gone, and
// records the start of a live one an older daemon recorded without it. zm
// never saw their exit status, so the outcome is failed (low confidence),
// never ok.
func (s *Service) reapUnmanaged() {
	rows, err := listRows(s.ctx, s.d.Store.Registry(), `WHERE managed = 0 AND pid > 0 AND state != ?`, string(orchapi.StateEnded))
	if err != nil {
		return
	}
	for _, r := range rows {
		if gone, why := processGone(r.PID, r.pidStart, r.Started); gone {
			s.end(s.ctx, r.ID, orchapi.Outcome{Kind: orchapi.OutcomeFailed, Confidence: "low", Evidence: why,
				Note: "exited without a session-end hook; zm saw no exit status"})
			continue
		}
		if p, ok := procStat(r.PID); ok && r.pidStart == 0 {
			_, _ = s.update(s.ctx, r.ID, "", func(x *row) (bool, error) {
				if x.PID != r.PID || x.pidStart != 0 {
					return false, nil
				}
				x.pidStart = p.start
				return true, nil
			})
		}
	}
}

// expireQuiet ends a hook session with no pid (registered by a zm hook
// that sent none) once no hook has reached it for twice bound_lease_ttl.
// By then quietHooks has set it idle and its bound leases have lapsed, so
// ending it changes nothing but the lists and presence it would stay in for
// ever; its next hook revives it under the same address. Its last hook
// counts from the store, not from this daemon's start, so ghosts left by an
// older build end at the first tick after an upgrade.
func (s *Service) expireQuiet(now time.Time) {
	rows, err := listRows(s.ctx, s.d.Store.Registry(), `WHERE managed = 0 AND pid = 0 AND state != ?`, string(orchapi.StateEnded))
	if err != nil {
		return
	}
	bound := s.d.Config.BoundLeaseTTL()
	ttl := 2 * bound
	for _, r := range rows {
		last := r.Updated
		if r.State == orchapi.StateIdle && r.Note == "no hooks" {
			last = r.Updated.Add(-bound) // quietHooks wrote that a bound TTL after the last hook
		}
		if r.ActivityAt != nil && r.ActivityAt.After(last) {
			last = *r.ActivityAt
		}
		if t, ok := s.hooked.Load(r.ID); ok && t.(time.Time).After(last) {
			last = t.(time.Time)
		}
		if now.Sub(last) < ttl {
			continue
		}
		s.end(s.ctx, r.ID, orchapi.Outcome{Kind: orchapi.OutcomeFailed, Confidence: "low",
			Evidence: "no hook for " + strings.TrimSuffix(strings.TrimSuffix(ttl.String(), "0s"), "0m") + " and no pid to watch",
			Note:     "presumed gone; its next hook revives it"})
	}
}
