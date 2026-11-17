package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sengirunners/mezze/internal/session/holder"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"syscall"
)

// Only a recorded process with the same kernel start time and its own process
// group can authorize a signal. A reused PID never authorizes cleanup.
func (s *Service) killRecordedTaskGroup(ctx context.Context, id string) {
	if err := s.signalRecordedGroup(ctx, id, syscall.SIGKILL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.d.Log.Warn("task group not signalled", "id", id, "reason", err)
	}
}

func (s *Service) signalRecordedGroup(ctx context.Context, id string, sig syscall.Signal) error {
	// Without a durable record there is no authority to signal a process group.
	// Partial services and late exits after store shutdown have no registry.
	if s == nil || s.d == nil || s.d.Store == nil {
		return sql.ErrNoRows
	}
	db := s.d.Store.Registry()
	if db == nil {
		return sql.ErrNoRows
	}
	var pgid int
	var started int64
	var prior taskHomeIdentity
	err := db.QueryRowContext(ctx, `SELECT pgid,group_start,home_id,home_path,home_dev,home_ino FROM session_tasks WHERE id=?`, id).Scan(&pgid, &started, &prior.id, &prior.path, &prior.dev, &prior.ino)
	if err != nil {
		return err
	}
	if pgid <= 1 || started <= 0 {
		return fmt.Errorf("group id or start time is not recorded")
	}
	h, err := s.currentTaskHome()
	if err != nil || h != prior {
		return fmt.Errorf("home identity does not match")
	}
	st, ok := procStat(pgid)
	if !ok || st.start != started {
		return fmt.Errorf("group leader is absent or its start time differs")
	}
	table, err := procTable()
	if err != nil {
		return fmt.Errorf("cannot inspect process group")
	}
	p, ok := table[pgid]
	if !ok || p.pgid != pgid {
		return fmt.Errorf("recorded process is not the group leader")
	}
	return syscall.Kill(-pgid, sig)
}

func (s *Service) reconcileTaskGroups(ctx context.Context) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT t.id FROM session_tasks t JOIN sessions s ON s.id=t.id WHERE t.state='ended' OR s.state='ended'`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.killRecordedTaskGroup(ctx, id)
	}
}

type taskStopGuard struct {
	mu   sync.Mutex
	refs int
}

// A stop and its completion share this guard. Entries are removed only after
// the last waiter releases them, so a busy daemon does not retain one per task.
func (s *Service) lockTaskStop(id string) func() {
	s.taskStopLocksMu.Lock()
	if s.taskStopLocks == nil {
		s.taskStopLocks = make(map[string]*taskStopGuard)
	}
	guard := s.taskStopLocks[id]
	if guard == nil {
		guard = &taskStopGuard{}
		s.taskStopLocks[id] = guard
	}
	guard.refs++
	s.taskStopLocksMu.Unlock()
	guard.mu.Lock()
	return func() {
		guard.mu.Unlock()
		s.taskStopLocksMu.Lock()
		guard.refs--
		if guard.refs == 0 {
			delete(s.taskStopLocks, id)
		}
		s.taskStopLocksMu.Unlock()
	}
}

var taskStopProcTable = procTable
var taskStopSignal = syscall.Kill

// taskSweep retains start identities across reparenting. New descendants of a
// tracked live process are discovered during each bounded stop-plan step.
type taskSweep struct {
	s            *Service
	r            row
	group        int
	bound        time.Time
	homeVerified bool
	known        map[int]int64
	frozen       map[int]int64
	sent         map[int]syscall.Signal
	err          error
}

func (s *Service) newTaskSweep(ctx context.Context, r row) *taskSweep {
	x := &taskSweep{s: s, r: r, known: map[int]int64{}, frozen: map[int]int64{}, sent: map[int]syscall.Signal{}}
	var prior taskHomeIdentity
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT pgid,home_id,home_path,home_dev,home_ino FROM session_tasks WHERE id=?`, r.ID).Scan(&x.group, &prior.id, &prior.path, &prior.dev, &prior.ino); err != nil {
		x.err = err
		return x
	}
	home, err := s.currentTaskHome()
	if err != nil || home != prior {
		x.err = fmt.Errorf("home identity does not match")
		return x
	}
	x.homeVerified = true
	if r.PID <= 1 || r.pidStart <= 0 {
		x.err = fmt.Errorf("task process identity is not recorded")
		return x
	}
	st, ok := procStat(r.PID)
	if !ok || st.start != r.pidStart {
		x.err = fmt.Errorf("task process identity changed before sweep")
		return x
	}
	if r.PID == os.Getpid() {
		x.err = fmt.Errorf("task root is this daemon")
		return x
	}
	// Retry like every other identity check: the root may be mid-exec (a
	// shell exec'ing its last command), when its argv cannot be read for an
	// instant. One failed read refused the whole stop.
	isHolder, _, holderErr := inspectTaskHolder(r.PID)
	if holderErr != nil || isHolder {
		x.err = fmt.Errorf("task root is a holder or cannot be identified")
		return x
	}
	x.known[r.PID] = r.pidStart
	return x
}

// inspectHolder retries transient exec/exit races. An unidentified live
// process is never assumed to be safe to signal.
func inspectTaskHolder(pid int) (bool, bool, error) {
	first, ok := procStat(pid)
	if !ok || first.zombie {
		return false, false, nil
	}
	for i := 0; i < 5; i++ {
		yes, err := holder.IsHolder(pid)
		if err == nil {
			return yes, true, nil
		}
		current, ok := procStat(pid)
		if !ok || current.zombie || current.start != first.start {
			return false, false, nil
		}
		if i == 4 {
			return false, true, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false, true, fmt.Errorf("cannot identify process %d", pid)
}

// errStopBound is a scan cut short at its step's bound.
var errStopBound = errors.New("descendant inspection exceeded the stop bound")

func (x *taskSweep) withinBound() bool {
	if x.bound.IsZero() || time.Now().Before(x.bound) {
		return true
	}
	if x.err == nil {
		x.err = errStopBound
	}
	return false
}

// startStep bounds the next stop-plan step's scans by bound. A scan an
// earlier step cut short at its own bound fails nothing by itself: this
// step scans again from every process the sweep knows, so that error is
// dropped; any other stays. A process that ignored TERM for its whole wait
// was reported as a failed stop when HUP then ended it.
func (x *taskSweep) startStep(bound time.Time) {
	x.bound = bound
	if errors.Is(x.err, errStopBound) {
		x.err = nil
	}
}

func (x *taskSweep) protected(pid int, table map[int]procEntry) bool {
	// A task root can exit while a child is being reparented. Retry with a
	// fresh table; never authorize a signal using a reused or missing root.
	for attempt := 0; attempt < 3; attempt++ {
		if !x.withinBound() {
			return true
		}
		visited := map[int]bool{}
		retry := false
		for current := pid; current > 1 && !visited[current]; {
			if !x.withinBound() {
				return true
			}
			visited[current] = true
			if current == os.Getpid() {
				return true
			}
			if current == x.r.PID {
				st, ok := procStat(current)
				if ok && !st.zombie && st.start == x.r.pidStart {
					return false
				}
				if pid == current {
					return true
				}
				child, live := procStat(pid)
				if !live || child.zombie {
					return true
				}
				if start, known := x.known[pid]; known && child.start != start {
					return true
				}
				retry = true
				break
			}
			yes, live, err := inspectTaskHolder(current)
			if err != nil {
				x.err = fmt.Errorf("cannot identify process %d: %w", current, err)
				return true
			}
			if !live || yes {
				return true
			}
			p, ok := table[current]
			if !ok {
				retry = true
				break
			}
			current = p.ppid
		}
		if !retry {
			return false
		}
		fresh, err := taskStopProcTable()
		if err != nil {
			x.err = err
			return true
		}
		table = fresh
		time.Sleep(5 * time.Millisecond)
	}
	x.err = fmt.Errorf("cannot verify process %d ancestry", pid)
	return true
}

func (x *taskSweep) homeStillMatches(ctx context.Context) bool {
	current, err := x.s.currentTaskHome()
	if err != nil {
		return false
	}
	var prior taskHomeIdentity
	err = x.s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino FROM session_tasks WHERE id=?`, x.r.ID).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino)
	return err == nil && current == prior
}

func (x *taskSweep) refresh(ctx context.Context) {
	if x.err != nil {
		return
	}
	home, err := x.s.currentTaskHome()
	if err != nil {
		x.err = err
		return
	}
	var prior taskHomeIdentity
	if err = x.s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino FROM session_tasks WHERE id=?`, x.r.ID).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino); err != nil || prior != home {
		x.err = fmt.Errorf("home identity does not match")
		return
	}
	table, err := taskStopProcTable()
	if err != nil {
		x.err = err
		return
	}
	children := childrenOf(table)
	queue := make([]int, 0, len(x.known))
	for pid, start := range x.known {
		if st, ok := procStat(pid); ok && st.start == start && !st.zombie {
			queue = append(queue, pid)
		}
	}
	seen := map[int]bool{}
	for len(queue) > 0 {
		if !x.withinBound() {
			return
		}
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		// A daemon or holder and everything beneath it belongs to the service,
		// even if that service was started by a task tool.
		if x.protected(pid, table) {
			continue
		}
		if pid != x.r.PID {
			isHolder, live, e := inspectTaskHolder(pid)
			if e != nil {
				x.err = fmt.Errorf("cannot identify process %d: %w", pid, e)
				continue
			}
			if !live {
				continue
			}
			if isHolder {
				continue
			}
			st, ok := procStat(pid)
			if !ok || st.zombie {
				continue
			}
			if old, known := x.known[pid]; known && old != st.start {
				continue
			}
			x.known[pid] = st.start
		}
		queue = append(queue, children[pid]...)
	}
}

// Freeze verified task processes only for the final KILL step. Suspending
// supervisors first prevents a fresh orphan between the last scan and KILL.
func (x *taskSweep) freezeForKill(ctx context.Context, until time.Time) {
	if x.err != nil {
		return
	}
	for time.Now().Before(until) {
		x.refresh(ctx)
		if x.err != nil {
			return
		}
		table, err := taskStopProcTable()
		if err != nil {
			x.err = err
			return
		}
		order := []int{x.r.PID}
		for pid := range x.known {
			if pid != x.r.PID {
				order = append(order, pid)
			}
		}
		stable := true
		for _, pid := range order {
			start, ok := x.known[pid]
			if !ok || pid == os.Getpid() || x.protected(pid, table) {
				continue
			}
			st, ok := procStat(pid)
			if !ok || st.zombie || st.start != start {
				continue
			}
			if st.stopped {
				continue
			}
			stable = false
			if _, sent := x.frozen[pid]; !sent {
				if e := taskStopSignal(pid, syscall.SIGSTOP); e != nil && !errors.Is(e, syscall.ESRCH) {
					x.err = fmt.Errorf("pid %d: %w", pid, e)
					return
				}
				x.frozen[pid] = start
			}
		}
		if x.err != nil {
			return
		}
		if stable {
			n := len(x.known)
			x.refresh(ctx)
			if x.err != nil || len(x.known) == n {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	x.err = fmt.Errorf("task tree could not be frozen within the stop bound")
}

// Resume only processes this sweep suspended; KILLed processes need no resume.
func (x *taskSweep) resumeFrozen() {
	if len(x.frozen) == 0 {
		return
	}
	table, err := taskStopProcTable()
	if err != nil {
		if x.err == nil {
			x.err = err
		}
		return
	}
	// Roll back our suspensions even if signal work exhausted its deadline.
	// Keep identity and ancestry checks, with their own finite cleanup budget.
	cleanup := *x
	cleanup.bound = time.Now().Add(2 * time.Second)
	cleanup.err = nil
	order := make([]int, 0, len(x.frozen))
	for pid := range x.frozen {
		if pid != x.r.PID {
			order = append(order, pid)
		}
	}
	order = append(order, x.r.PID)
	for _, pid := range order {
		start, found := x.frozen[pid]
		if !found {
			continue
		}
		st, ok := procStat(pid)
		if !ok || st.zombie || st.start != start {
			delete(x.frozen, pid)
			continue
		}
		if pid == os.Getpid() || cleanup.protected(pid, table) {
			if x.err == nil {
				x.err = fmt.Errorf("cannot safely resume pid %d", pid)
				if cleanup.err != nil {
					x.err = fmt.Errorf("cannot safely resume pid %d: %w", pid, cleanup.err)
				}
			}
			continue
		}
		if e := taskStopSignal(pid, syscall.SIGCONT); e != nil && !errors.Is(e, syscall.ESRCH) {
			if x.err == nil {
				x.err = fmt.Errorf("resume pid %d: %w", pid, e)
			}
			continue
		}
		delete(x.frozen, pid)
	}
}
func taskFreezeDeadline(end time.Time) time.Time {
	d := time.Now().Add(500 * time.Millisecond)
	if end.Before(d) {
		return end
	}
	return d
}

func (x *taskSweep) signal(sig syscall.Signal) {
	if x.err != nil {
		return
	}
	table, err := taskStopProcTable()
	if err != nil {
		x.err = err
		return
	}
	for pid, start := range x.known {
		if !x.withinBound() {
			return
		}
		if pid == x.r.PID || pid == os.Getpid() || x.sent[pid] == sig {
			continue
		}
		st, ok := procStat(pid)
		if !ok || st.zombie || st.start != start {
			continue
		}
		entry, ok := table[pid]
		if !ok {
			continue
		}
		if x.protected(pid, table) || x.err != nil {
			continue
		}

		if entry.pgid == x.group {
			x.sent[pid] = sig
			continue
		} // holder signals the task group
		check, ok := procStat(pid)
		if !ok || check.start != start || check.zombie {
			continue
		}
		if err := taskStopSignal(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			x.err = fmt.Errorf("pid %d: %w", pid, err)
			continue
		}
		x.sent[pid] = sig
	}
}
func (x *taskSweep) survivors() []int {
	var live []int
	for pid, start := range x.known {
		if pid == x.r.PID {
			continue
		}
		if st, ok := procStat(pid); ok && st.start == start && !st.zombie {
			live = append(live, pid)
		}
	}
	sort.Ints(live)
	return live
}

// groupGone reports whether the recorded process group has no remaining
// members. A dead or zombie task root can still belong to its process group
// while the holder is reaping it, even when the sweep has no live survivors.
func (x *taskSweep) groupGone() bool {
	return x.group > 1 && errors.Is(syscall.Kill(-x.group, 0), syscall.ESRCH)
}

func (x *taskSweep) warning() string {
	live := x.survivors()
	if x.err == nil && len(live) == 0 {
		return ""
	}
	reason := "descendant inspection could not be completed"
	if x.err != nil {
		reason = x.err.Error()
	}
	if len(live) > 0 {
		reason = fmt.Sprintf("%s; tool pids %v", reason, live)
	}
	return reason + " may still be running; inspect and stop them manually"
}
func (s *Service) recordTaskSweepWarning(ctx context.Context, id, warning string) {
	if warning == "" {
		return
	}
	db := s.d.Store.Registry()
	if db == nil {
		return
	}
	// A disconnected caller must not erase the warning before completion.
	_, err := db.ExecContext(context.Background(), `UPDATE session_tasks SET failure=CASE WHEN failure='' THEN ? WHEN instr(failure,?)>0 THEN failure ELSE failure || '; ' || ? END,group_stopped=0 WHERE id=? AND state='running'`, warning, warning, warning, id)
	if err != nil {
		s.d.Log.Warn("record task stop warning", "id", id, "err", err)
	}
}

func taskExitReported(p *proc) bool {
	select {
	case <-p.rawExited:
		return true
	default:
		return false
	}
}

func (s *Service) stopUnreachableTask(ctx context.Context, r row, plan []stopStep) (orchapi.Session, []string, error) {
	unlock := s.lockTaskStop(r.ID)
	defer unlock()
	s.cancelTaskChildren(r.Address)
	sweep := s.newTaskSweep(ctx, r)
	var sent []string
	var signalErr error
	for _, step := range plan {
		end := time.Now().Add(step.wait)
		sweep.startStep(end)
		if step.sig == syscall.SIGKILL {
			sweep.freezeForKill(ctx, taskFreezeDeadline(end))
		} else {
			sweep.refresh(ctx)
		}
		gone, _ := processGone(r.PID, r.pidStart, r.Started)
		sweep.signal(step.sig)
		groupGone := sweep.groupGone()
		if !groupGone {
			if e := s.signalRecordedGroup(ctx, r.ID, step.sig); e != nil {
				signalErr = e
			} else {
				sent = append(sent, step.name)
			}
		}
		if step.sig == syscall.SIGKILL {
			sweep.resumeFrozen()
		}
		for time.Now().Before(end) {
			sweep.refresh(ctx)
			sweep.signal(step.sig)
			gone, _ = processGone(r.PID, r.pidStart, r.Started)
			if gone && len(sweep.survivors()) == 0 && sweep.err == nil && sweep.groupGone() {
				break
			}
			if ctx.Err() != nil || contextStopped(s.ctx) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		gone, _ = processGone(r.PID, r.pidStart, r.Started)
		if gone && len(sweep.survivors()) == 0 && sweep.err == nil && sweep.groupGone() {
			break
		}
	}
	if sweep.bound.IsZero() || time.Now().Before(sweep.bound) {
		sweep.refresh(ctx)
	}
	warning := sweep.warning()
	if signalErr != nil {
		warning = fmt.Sprintf("%s; child pid %d may still be running; inspect and stop it manually", signalErr, r.PID)
	}
	s.recordTaskSweepWarning(ctx, r.ID, warning)
	gone, _ := processGone(r.PID, r.pidStart, r.Started)
	if !gone {
		out, _ := s.load(ctx, r.ID)
		if warning != "" {
			return out.Session, sent, api.Errorf(api.CodeConflict, "%s", warning)
		}
		return out.Session, sent, api.Errorf(api.CodeConflict, "child pid %d may still be running; inspect and stop it manually", r.PID)
	}
	if !sweep.homeVerified || !sweep.homeStillMatches(ctx) || r.pidStart <= 0 || sweep.group <= 1 {
		out, _ := s.load(ctx, r.ID)
		return out.Session, sent, api.Errorf(api.CodeConflict, "task identity cannot be verified; child pid %d may still be running; inspect and stop it manually", r.PID)
	}
	if !sweep.groupGone() {
		out, _ := s.load(ctx, r.ID)
		groupWarning := "task group may still be running; inspect and stop it manually"
		if signalErr != nil {
			groupWarning = fmt.Sprintf("%s: %v", groupWarning, signalErr)
		}
		s.recordTaskSweepWarning(ctx, r.ID, groupWarning)
		return out.Session, sent, api.Errorf(api.CodeConflict, "%s", groupWarning)
	}
	o := orchapi.Outcome{Kind: orchapi.OutcomeKilled, Confidence: "high", Evidence: "stopped while holder unreachable"}
	if len(sent) > 0 {
		o.Signal = sent[len(sent)-1]
	}
	if warning != "" {
		o.Kind = orchapi.OutcomeFailed
		o.Evidence = warning
	}
	s.endWithGroup(ctx, r.ID, o, false)
	s.unreachable.Delete(r.ID)
	s.completeTask(r.ID)
	out, _ := s.load(ctx, r.ID)
	return out.Session, sent, nil
}

func contextStopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
