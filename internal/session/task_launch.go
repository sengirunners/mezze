package session

import (
	"context"
	"database/sql"
	"errors"
	"syscall"

	"github.com/sengirunners/mezze/internal/session/holder"
)

// A successful UPDATE with zero rows is not a durable launch authority. Check
// the live task, parent, home and both process identities before releasing fd3.
// A task an authority asked for (remote) does not live or die with its parent.
func (s *Service) recordedTaskLaunch(id string, st *holder.Started) error {
	if s == nil || s.d == nil || s.d.Store == nil || s.d.Store.Registry() == nil {
		return sql.ErrNoRows
	}
	var group, holderPID int
	var groupStart, holderStart int64
	var home taskHomeIdentity
	err := s.d.Store.Registry().QueryRowContext(context.Background(), `
 SELECT t.pgid,t.group_start,h.holder_pid,h.holder_start,t.home_id,t.home_path,t.home_dev,t.home_ino
 FROM session_tasks t JOIN session_holders h ON h.id=t.id
 JOIN sessions child ON child.id=t.id JOIN sessions parent ON parent.address=t.parent
 WHERE t.id=? AND t.state='running' AND child.state!='ended'
 AND (t.remote=1 OR parent.state!='ended' AND NOT EXISTS(SELECT 1 FROM session_task_stopping x WHERE x.parent=t.parent))`, id).
		Scan(&group, &groupStart, &holderPID, &holderStart, &home.id, &home.path, &home.dev, &home.ino)
	if err != nil {
		return err
	}
	current, err := s.currentTaskHome()
	if err != nil || current != home {
		return errors.New("recorded task home differs")
	}
	if group <= 1 || group != st.GroupPID || holderPID != st.Cmd.Process.Pid || groupStart <= 0 || holderStart <= 0 {
		return errors.New("task launch identities are not recorded")
	}
	g, ok := procStat(group)
	if !ok || g.start != groupStart {
		return errors.New("recorded task group changed")
	}
	h, ok := procStat(holderPID)
	if !ok || h.start != holderStart {
		return errors.New("recorded task holder changed")
	}
	pgid, err := syscall.Getpgid(group)
	if err != nil || pgid != group {
		return errors.New("task anchor is not its process group leader")
	}
	return nil
}
