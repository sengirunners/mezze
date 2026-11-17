package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"syscall"

	"github.com/sengirunners/mezze/internal/session/holder"
)

func (s *Service) recordStartedHolder(ctx context.Context, id string, pid int) error {
	h, err := s.currentTaskHome()
	if err != nil {
		return err
	}
	var start int64
	if st, ok := procStat(pid); ok {
		start = st.start
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO session_holders(id,holder_pid,holder_start,home_id,home_path,home_dev,home_ino) VALUES(?,?,?,?,?,?,?)`, id, pid, start, h.id, h.path, h.dev, h.ino)
	return err
}

// Adoption verifies the live holder while preserving missing or changed signal identities.
func (s *Service) recordAdoptedHolder(ctx context.Context, id string, pid int) error {
	h, err := s.currentTaskHome()
	if err != nil {
		return err
	}
	var recorded int
	var start int64
	var prior taskHomeIdentity
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT holder_pid,holder_start,home_id,home_path,home_dev,home_ino FROM session_holders WHERE id=?`, id).Scan(&recorded, &start, &prior.id, &prior.path, &prior.dev, &prior.ino)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && recorded != pid {
		return fmt.Errorf("recorded holder belongs to another process")
	}
	st, ok := procStat(pid)
	if !ok || (start > 0 && start != st.start) {
		return fmt.Errorf("holder process identity cannot be verified")
	}
	if err == sql.ErrNoRows {
		// A socket authenticates adoption, but cannot reconstruct the home
		// identity of a session created by an older daemon.
		return nil
	} else if prior != h {
		// The authenticated socket can be adopted; keep the old identity so
		// direct signals remain forbidden after a home move or copy.
		return nil
	} else if start == 0 {
		if _, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_holders SET holder_start=? WHERE id=?`, st.start, id); err != nil {
			return err
		}
	}
	return nil
}

// Missing sockets and failed dials are not death evidence. A PID mismatch,
// zombie, or ESRCH is evidence; missing records and inspection failures are not.
func (s *Service) recordedHolderGone(ctx context.Context, id string) (bool, string) {
	var err error
	var pid int
	var start int64
	var prior taskHomeIdentity
	if err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT holder_pid,holder_start,home_id,home_path,home_dev,home_ino FROM session_holders WHERE id=?`, id).Scan(&pid, &start, &prior.id, &prior.path, &prior.dev, &prior.ino); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if r, ok := s.load(ctx, id); ok {
				if r.PID <= 1 {
					return true, "legacy session has no valid child pid"
				}
				if r.PID <= math.MaxInt32 {
					if gone, why := processGone(r.PID, r.pidStart, r.Started); gone {
						return true, why
					}
				}
			}
		}
		return false, "holder identity is not recorded"
	}
	if pid <= 1 || pid > math.MaxInt32 {
		return false, "invalid holder pid"
	}
	if st, ok := procStat(pid); ok {
		if start > 0 && st.start != start {
			return true, "holder pid was reused"
		}
		// Session creation precedes holder launch by an unbounded delay. A
		// wall-clock difference cannot prove reuse when its kernel start is unknown.
		if st.zombie {
			return true, "holder exited"
		}
		return false, "recorded holder is still alive"
	}
	if err = syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true, "holder pid is dead"
	}
	return false, "holder process state cannot be verified"
}

// signalUnreachable verifies the home and process identity before bypassing a
// broken holder socket. A pre-identity row can never authorize a signal.
func (s *Service) signalUnreachable(ctx context.Context, r row, sig syscall.Signal) error {
	var isTask bool
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_tasks WHERE id=?)`, r.ID).Scan(&isTask); err != nil {
		return err
	}
	if isTask {
		return s.signalRecordedGroup(ctx, r.ID, sig)
	}
	var prior taskHomeIdentity
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino FROM session_holders WHERE id=?`, r.ID).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino); err != nil {
		return fmt.Errorf("holder identity is not recorded")
	}
	h, err := s.currentTaskHome()
	if err != nil || prior != h {
		return fmt.Errorf("home identity does not match")
	}
	if r.PID <= 1 || r.pidStart <= 0 {
		return fmt.Errorf("process identity is not recorded")
	}
	st, ok := procStat(r.PID)
	if !ok || st.start != r.pidStart {
		return fmt.Errorf("process is absent or its start time differs")
	}
	table, err := procTable()
	if err != nil || table[r.PID].pgid != r.PID {
		return fmt.Errorf("cannot verify process group")
	}
	return syscall.Kill(-r.PID, sig)
}

// holderSignalAllowed authenticates the holder on this home's socket before
// asking it to signal. It does not authorize direct process-group signals.
func (s *Service) holderSignalAllowed(ctx context.Context, id string) error {
	r, ok := s.load(ctx, id)
	if !ok {
		return fmt.Errorf("session is not recorded")
	}
	p := s.proc(id)
	if p == nil || p.hc == nil {
		return fmt.Errorf("holder connection is unavailable")
	}
	hc, err := holder.Dial(s.d.Config.Home, id)
	if err != nil {
		return fmt.Errorf("holder cannot be verified on this home's socket: %w", err)
	}
	defer hc.Close()
	if hc.Hello.HolderPID != p.hc.Hello.HolderPID || hc.Hello.ChildPID != r.PID || hc.Hello.Exited {
		return fmt.Errorf("holder or child does not match the live session")
	}
	return nil
}
