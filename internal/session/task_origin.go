package session

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// taskOrigin derives the nearest registered session from the kernel peer PID.
// In-process clients lack a peer PID and use their explicitly supplied subject.
func (s *Service) taskOrigin(ctx context.Context, sub *authz.Subject) (string, error) {
	pid := core.PeerPIDOf(ctx)
	if pid < 0 {
		return "", api.Errorf(api.CodeUnavailable, "cannot determine Unix peer process")
	}
	if pid == 0 {
		if sub != nil {
			return sub.Session, nil
		}
		return "", nil
	}
	// The child can connect as soon as its holder starts. Wait until spawn has
	// recorded its PID so that this window cannot be mistaken for socket owner.
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	return s.originWalk(ctx, pid)
}

// hookOrigin is taskOrigin for a hook, which must stay within its budget
// and not wait for a spawn: it stats only the caller's own ancestors, below
// the daemon's process, and reads only the live sessions with those pids.
// A spawned child's row carries its cap before its holder starts, so a
// recorded pid is never seen uncapped. Only a caller no session claims
// below the daemon (as an unrecorded new child would be), or one whose
// walk ended early, waits for spawnMu (taskOrigin), so it fails closed.
func (s *Service) hookOrigin(ctx context.Context, sub *authz.Subject) (string, error) {
	pid := core.PeerPIDOf(ctx)
	if pid <= 0 {
		return s.taskOrigin(ctx, sub)
	}
	origin, unsure, err := s.ancestryOrigin(ctx, pid, os.Getpid())
	if err != nil || origin != "" || !unsure {
		return origin, err
	}
	return s.taskOrigin(ctx, sub)
}

// ancestryMax bounds the ancestors a hook's walk stats.
var ancestryMax = 128

// ancestryOrigin is the nearest live (or lingering, caps.go) session among
// pid and its ancestors below process stop (not pid itself). unsure: the
// walk reached stop, or ended before pid 1 for any other reason (too deep, a stat that failed, a
// loop), so "no session" is not an answer to trust.
func (s *Service) ancestryOrigin(ctx context.Context, pid, stop int) (string, bool, error) {
	var chain []int
	starts := map[int]int64{}
	unsure := false
	for p := pid; p > 1; {
		if p == stop && p != pid || len(chain) >= ancestryMax {
			unsure = true
			break
		}
		st, ok := procStat(p)
		if !ok {
			if p == pid {
				return "", false, api.Errorf(api.CodeUnavailable, "calling process no longer exists")
			}
			unsure = true
			break
		}
		if _, seen := starts[p]; seen {
			unsure = true
			break
		}
		chain, starts[p] = append(chain, p), st.start
		p = st.ppid
	}
	if len(chain) == 0 {
		return "", unsure, nil
	}
	args := []any{string(orchapi.StateEnded)}
	for _, p := range chain {
		args = append(args, p)
	}
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE state != ? AND pid IN (?"+strings.Repeat(",?", len(chain)-1)+")", args...)
	if err != nil {
		return "", false, err
	}
	byPID := map[int]row{}
	for _, r := range rows {
		if r.pidStart == 0 || r.pidStart == starts[r.PID] {
			byPID[r.PID] = r
		}
	}
	for _, p := range chain {
		if r, ok := byPID[p]; ok {
			return r.Address, unsure, nil
		}
		// A capped session's process outlives its row.
		if a, err := s.lingerAt(p, starts[p]); err != nil || a != "" {
			return a, unsure, err
		}
	}
	return "", unsure, nil
}

// originWalk is the nearest live registered session (or lingering capped
// one, caps.go) among pid and its ancestors ("": none).
func (s *Service) originWalk(ctx context.Context, pid int) (string, error) {
	table, err := procTable()
	if err != nil {
		return "", api.Errorf(api.CodeUnavailable, "cannot determine calling process")
	}
	if _, ok := table[pid]; !ok {
		return "", api.Errorf(api.CodeUnavailable, "calling process no longer exists")
	}
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE state != ? AND pid > 0", string(orchapi.StateEnded))
	if err != nil {
		return "", err
	}
	byPID := make(map[int]row, len(rows))
	for _, r := range rows {
		if r.pidStart != 0 {
			st, ok := procStat(r.PID)
			if !ok || st.start != r.pidStart {
				continue
			}
		}
		byPID[r.PID] = r
	}
	for pid > 1 {
		if r, ok := byPID[pid]; ok {
			return r.Address, nil
		}
		if s.anyCaps() { // a capped session's process outlives its row
			st, _ := procStat(pid) // unknown start: matched (fails closed)
			if a, err := s.lingerAt(pid, st.start); err != nil || a != "" {
				return a, err
			}
		}
		p, ok := table[pid]
		if !ok || p.ppid == pid {
			break
		}
		pid = p.ppid
	}
	return "", nil // socket owner outside a registered session
}

func (s *Service) spawnLineage(ctx context.Context, sub *authz.Subject, task *taskStart) (string, int, error) {
	if task != nil {
		return task.parent, task.depth, nil
	}
	parent, err := s.taskOrigin(ctx, sub)
	if err != nil || parent == "" {
		return parent, 0, err
	}
	p, ok := s.Lookup(ctx, parent)
	if !ok {
		return "", 0, nil
	}
	var depth int
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT depth FROM session_lineage WHERE id=?`, p.ID).Scan(&depth)
	if err == sql.ErrNoRows {
		err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT depth FROM session_tasks WHERE id=?`, p.ID).Scan(&depth)
	}
	if err == sql.ErrNoRows {
		return parent, 1, nil
	}
	if err != nil {
		return "", 0, err
	}
	if depth >= taskDepthLimit {
		return "", 0, api.Errorf(api.CodeUnavailable, "task depth limit %d reached", taskDepthLimit)
	}
	return parent, depth + 1, nil
}

func (s *Service) taskSessionAccess(ctx context.Context, sub *authz.Subject, r row) error {
	var parent string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT parent FROM session_tasks WHERE id=?`, r.ID).Scan(&parent)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	origin, err := s.taskOrigin(ctx, sub)
	if err != nil || (origin != "" && origin != parent) {
		return api.NotFound("session", r.ID)
	}
	return nil
}

type taskStopLock struct {
	mu   sync.Mutex
	refs int
}

// Failed and killed parents cancel their descendants; successful parents leave
// them running so they can still report to the work item. A task an authority
// asked for is not theirs to cancel (session_tasks.remote).
func (s *Service) cancelTaskChildren(parent string) {
	s.taskStoppingMu.Lock()
	if s.taskStopping == nil {
		s.taskStopping = make(map[string]*taskStopLock)
	}
	lock := s.taskStopping[parent]
	if lock == nil {
		lock = &taskStopLock{}
		s.taskStopping[parent] = lock
	}
	lock.refs++
	s.taskStoppingMu.Unlock()
	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		s.taskStoppingMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.taskStopping, parent)
		}
		s.taskStoppingMu.Unlock()
	}()
	ctx := context.Background()
	db := s.d.Store.Registry()
	if db == nil {
		return
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_task_stopping(parent) VALUES(?)`, parent); err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id FROM session_lineage l JOIN session_tasks t ON t.id=l.id JOIN sessions s ON s.id=l.id WHERE l.parent=? AND t.remote=0 AND s.state != ?`, parent, string(orchapi.StateEnded))
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
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	for _, id := range ids {
		_, _ = db.ExecContext(ctx, `UPDATE session_tasks SET failure='cancelled' WHERE id=? AND state='running' AND failure=''`, id)
		_, _ = s.Stop(ctx, s.d.System(), id, orchapi.StopRequest{Escalate: true, Force: true})
	}
}

// trackParentStop keeps admission closed across the entire signal plan.
func (s *Service) trackParentStop(r row, p *proc) func() {
	parent := r.Address
	s.taskStoppingMu.Lock()
	if s.activeStops == nil {
		s.activeStops = make(map[string]int)
	}
	s.activeStops[parent]++
	s.taskStoppingMu.Unlock()
	return func() {
		s.taskStoppingMu.Lock()
		defer s.taskStoppingMu.Unlock()
		s.activeStops[parent]--
		if s.activeStops[parent] != 0 {
			return
		}
		delete(s.activeStops, parent)
		alive := p != nil && !p.isExited()
		if p == nil && r.PID > 1 && r.pidStart != 0 {
			st, ok := procStat(r.PID)
			_, exited := holder.ReadExit(s.d.Config.Home, r.ID)
			alive = ok && st.start == r.pidStart && !exited
		}
		if alive {
			_, _ = s.d.Store.Registry().Exec(`DELETE FROM session_task_stopping WHERE parent=? AND EXISTS (SELECT 1 FROM sessions WHERE address=? AND state!=?)`, parent, parent, string(orchapi.StateEnded))
		}
	}
}
