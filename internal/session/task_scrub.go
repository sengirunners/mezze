package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// scrubTasks removes redacted issue text from the task-owned copies. A task
// linked to that issue also replaces short exact text; unrelated tasks use the
// shared minimum length to avoid changing coincidental short strings.
func (s *Service) scrubTasks(ctx context.Context, sc core.Scrub) error {
	return s.scrubTasksMode(ctx, sc, "", false)
}

type taskScrubCopy struct {
	id, work, result, request, mailErr, commentErr, command, state, handoffErr, failure string
	transcript                                                                          []byte
	started                                                                             int64 // session start, ms
}

// retry: this module's own rerun of a redaction it already holds tasks for
// (recoverTaskHolds), never a call through ScrubAll, which may be a fresh
// redaction of the same text after it came back (same hold key).
func (s *Service) scrubTasksMode(ctx context.Context, sc core.Scrub, discard string, retry bool) error {
	if len(sc.Texts) == 0 {
		return nil
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	s.taskDeliveryMu.Lock()
	defer s.taskDeliveryMu.Unlock()
	s.taskIO.Lock()
	defer s.taskIO.Unlock()
	db := s.d.Store.Registry()
	if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET request_verified=0 WHERE id IN (SELECT id FROM sessions WHERE project=?)`, sc.Project); err != nil {
		return err
	}
	// The activity log is a task-owned copy too: dropped, not rewritten.
	if _, err := db.ExecContext(ctx, `DELETE FROM session_activity WHERE session IN (SELECT t.id FROM session_tasks t JOIN sessions s ON s.id=t.id WHERE s.project=?)`, sc.Project); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT t.id,t.work,t.transcript,t.result,t.request_json,t.mail_error,t.comment_error,s.command,t.state,COALESCE(h.error,''),t.failure,s.started FROM session_tasks t JOIN sessions s ON s.id=t.id LEFT JOIN session_handoffs h ON h.source=t.id WHERE s.project=?`, sc.Project)
	if err != nil {
		return err
	}
	var copies []taskScrubCopy
	for rows.Next() {
		var r taskScrubCopy
		if err = rows.Scan(&r.id, &r.work, &r.transcript, &r.result, &r.request, &r.mailErr, &r.commentErr, &r.command, &r.state, &r.handoffErr, &r.failure, &r.started); err != nil {
			rows.Close()
			return err
		}
		copies = append(copies, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	subjects, _ := json.Marshal(sc.Subjects)
	texts, _ := json.Marshal(sc.Texts)
	sum := sha256.Sum256(append(append([]byte(sc.Project), subjects...), texts...))
	key := fmt.Sprintf("%x", sum)
	// A retry of this redaction (recoverTaskHolds, as each held task ends)
	// holds a task started after its first hold only for what that task's
	// own copies show: it cannot have received text already redacted at the
	// source, and holding every new running task would disconnect each one
	// for as long as the original holds last. A task it held
	// already stays held (the held check below).
	var since int64
	if retry {
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MIN(created),0) FROM session_task_holds WHERE hold_key=?`, key).Scan(&since); err != nil {
			return err
		}
	}
	// The redaction's own time, when its caller knows it (a pending-scrub
	// retry from the issues module keeps the first attempt's).
	if !sc.At.IsZero() {
		since = sc.At.UnixMilli()
	}
	pending := false
	for _, r := range copies {
		forced := discard == r.id && r.state == "ended" && s.proc(r.id) == nil
		scoped := false
		for _, subject := range sc.Subjects {
			if r.work != "" && (subject == r.work || strings.TrimPrefix(subject, "bd:") == strings.TrimPrefix(r.work, "bd:")) {
				scoped = true
				break
			}
		}
		minLen := core.MinUnscoped
		if scoped {
			minLen = 0
		}
		// A running agent may have received text that is absent from every
		// captured copy, and may emit it later. Hold all running tasks until
		// their output is complete, including already-started continuations.
		uncertain := strings.Contains(r.failure, "may still be running")
		writerPending := s.proc(r.id) == nil && ((r.state == "running" && uncertain) || (r.state == "ended" && !s.taskWriterGone(ctx, r.id)))
		newer := since > 0 && r.started > since
		affected := uncertain || writerPending || (!newer && (scoped || r.state == "running" || s.proc(r.id) != nil))
		inspect := func(data string) {
			safe, err := taskRedactionSafe(data, sc.Texts, minLen)
			if err != nil || !safe {
				affected = true
			}
		}
		inspect(string(r.transcript) + r.result + r.request + r.command + r.handoffErr)
		fields, err := s.taskSessionTexts(ctx, r.id)
		if err != nil {
			return err
		}
		for _, f := range fields {
			if f.valid {
				inspect(f.value)
			}
		}
		for _, path := range []string{holder.TaskOutputPath(s.d.Config.Home, r.id), holder.TaskErrorPath(s.d.Config.Home, r.id), s.scrollbackPath(r.id)} {
			if data, err := os.ReadFile(path); err == nil {
				inspect(string(data))
			} else if !errors.Is(err, os.ErrNotExist) {
				affected = true
			}
		}
		if p := s.proc(r.id); p != nil {
			data, _, _, _, _ := p.out.Next(0, scrollbackSize)
			inspect(string(data))
		}
		if !affected {
			var held bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=? AND hold_key=?)`, r.id, key).Scan(&held); err != nil {
				return err
			}
			affected = held
		}
		if !affected {
			if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET request_verified=1 WHERE id=? AND NOT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=?)`, r.id, r.id); err != nil {
				return err
			}
			continue
		}
		{
			if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO session_task_holds(task_id,hold_key,project,subjects,texts,created) VALUES(?,?,?,?,?,?)`, r.id, key, sc.Project, string(subjects), string(texts), time.Now().UnixMilli()); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET text_withheld=1 WHERE id=?`, r.id); err != nil {
				return err
			}
		}
		if forced {
			if err := s.sealTaskOutput(r.id); err != nil {
				return err
			}
		}
		if writerPending && !forced {
			if r.state == "ended" {
				// An uncertain external writer can still append output, but it
				// cannot change these ended task copies through captureTask.
				// Purge them now without treating that as writer-exit proof.
				if err := s.scrubPendingTaskDBCopies(ctx, r, fields, sc.Texts, minLen); err != nil {
					return err
				}
			}
			// These files are opened by the holder at spawn. Unlinking them
			// prevents a surviving writer from leaving later output on disk.
			// Keep the hold: identity failure cannot prove the writer is gone.
			for _, path := range []string{holder.TaskOutputPath(s.d.Config.Home, r.id), holder.TaskErrorPath(s.d.Config.Home, r.id), s.scrollbackPath(r.id)} {
				if err := removeTaskOutput(path); err != nil {
					return err
				}
			}
			pending = true
			continue
		}
		if r.state == "running" || s.proc(r.id) != nil {
			if p := s.proc(r.id); p != nil {
				p.closeConns()
			}
			pending = true
			continue
		}
		unsafe := false
		clean := func(v string) string {
			cleaned, err := scrubTaskBytes(v, sc.Texts, minLen)
			if err != nil {
				unsafe = true
				return ""
			}
			safe, err := taskRedactionSafe(cleaned, sc.Texts, minLen)
			if err != nil || !safe {
				unsafe = true
			}
			return cleaned
		}
		transcript := clean(string(r.transcript))
		paths := []string{holder.TaskOutputPath(s.d.Config.Home, r.id), holder.TaskErrorPath(s.d.Config.Home, r.id), s.scrollbackPath(r.id)}
		for _, path := range paths {
			if forced {
				continue
			}
			if data, e := os.ReadFile(path); e == nil {
				cleaned := clean(string(data))
				if !unsafe {
					if e = replaceTaskOutput(path, []byte(cleaned)); e != nil {
						return e
					}
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				unsafe = true
			}
		}
		result := r.result
		if result != "" {
			var v orchapi.TaskResult
			if json.Unmarshal([]byte(result), &v) == nil {
				v.Text = clean(v.Text)
				v.Outcome.Evidence = clean(v.Outcome.Evidence)
				b, _ := json.Marshal(v)
				result = string(b)
			} else {
				unsafe = true
				result = ""
			}
		}
		request := r.request
		var req orchapi.TaskStartRequest
		requestSafe := false
		if json.Unmarshal([]byte(request), &req) == nil {
			cleaned, e := scrubTaskBytes(req.Prompt, sc.Texts, minLen)
			if e == nil {
				safe, verifyErr := taskRedactionSafe(cleaned, sc.Texts, minLen)
				requestSafe = verifyErr == nil && safe
			}
			if requestSafe {
				req.Prompt = cleaned
				b, _ := json.Marshal(req)
				request = string(b)
			} else {
				unsafe = true
				request = ""
			}
		} else {
			unsafe = true
			request = ""
		}
		for i := range fields {
			if fields[i].valid {
				fields[i].value = scrubTaskSessionValue(fields[i].value, clean)
			}
		}
		mailErr, commentErr, handoffErr := clean(r.mailErr), clean(r.commentErr), clean(r.handoffErr)
		if handoffErr != r.handoffErr {
			if _, err = db.ExecContext(ctx, `UPDATE session_handoffs SET error=? WHERE source=?`, handoffErr, r.id); err != nil {
				return err
			}
		}
		if unsafe {
			if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET text_withheld=1 WHERE id=?`, r.id); err != nil {
				return err
			}
			for _, path := range paths {
				if forced {
					continue
				}
				if e := removeTaskOutput(path); e != nil {
					return e
				}
			}
			transcript = ""
			var v orchapi.TaskResult
			if result != "" && json.Unmarshal([]byte(result), &v) == nil {
				v.Text = ""
				v.TextWithheld = true
				v.Outcome.Evidence = "result text withheld after redaction"
				b, _ := json.Marshal(v)
				result = string(b)
			}
		}
		if _, err = db.ExecContext(ctx, `UPDATE session_tasks SET transcript=?,result=?,request_json=?,mail_error=?,comment_error=? WHERE id=?`, []byte(transcript), result, request, mailErr, commentErr, r.id); err != nil {
			return err
		}
		if err = s.writeTaskSessionTexts(ctx, r.id, fields); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO session_task_compactions(project) SELECT project FROM session_task_holds WHERE task_id=? AND hold_key=?`, r.id, key); err != nil {
			return err
		}
		removed, err := db.ExecContext(ctx, `DELETE FROM session_task_holds WHERE task_id=? AND hold_key=?`, r.id, key)
		if err != nil {
			return err
		}
		if requestSafe {
			if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET request_verified=1 WHERE id=? AND NOT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=?)`, r.id, r.id); err != nil {
				return err
			}
		}
		if n, _ := removed.RowsAffected(); n > 0 && !unsafe {
			if _, err := db.ExecContext(ctx, `UPDATE session_tasks SET text_withheld=0 WHERE id=? AND NOT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=?)`, r.id, r.id); err != nil {
				return err
			}
		}
	}
	if pending {
		return api.Errorf(api.CodeUnavailable, "task output is still being recorded")
	}
	return nil
}

// scrubPendingTaskDBCopies runs under the caller's task/delivery/I/O locks.
// Only ended tasks enter it: running capture retains its existing hold policy.
// It never releases a hold, verifies a request, or rewrites a writer's files.
func (s *Service) scrubPendingTaskDBCopies(ctx context.Context, r taskScrubCopy, fields []taskSessionText, texts []string, minLen int) error {
	clean := func(v string) string {
		cleaned, err := scrubTaskBytes(v, texts, minLen)
		if err != nil {
			return ""
		}
		if safe, err := taskRedactionSafe(cleaned, texts, minLen); err != nil || !safe {
			return ""
		}
		return cleaned
	}
	cleanValue := func(v string) string {
		cleaned := scrubTaskSessionValue(v, clean)
		if safe, err := taskRedactionSafe(cleaned, texts, minLen); err != nil || !safe {
			return ""
		}
		return cleaned
	}
	cleanJSON := func(v string) string {
		if v == "" || !json.Valid([]byte(v)) {
			return ""
		}
		cleaned := cleanValue(v)
		if !json.Valid([]byte(cleaned)) {
			return ""
		}
		return cleaned
	}
	transcript := []byte(clean(string(r.transcript)))
	result, request := cleanJSON(r.result), cleanJSON(r.request)
	mailErr, commentErr, handoffErr := clean(r.mailErr), clean(r.commentErr), clean(r.handoffErr)
	for i := range fields {
		if fields[i].valid {
			fields[i].value = cleanValue(fields[i].value)
		}
	}
	db := s.d.Store.Registry()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE session_tasks SET transcript=?,result=?,request_json=?,mail_error=?,comment_error=? WHERE id=? AND state='ended'`, transcript, result, request, mailErr, commentErr, r.id)
	if err != nil {
		return err
	}
	if n, err := updated.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return api.Errorf(api.CodeConflict, "task changed while scrubbing retained database copies")
	}
	for _, f := range fields {
		if !f.valid || f.value == f.original {
			continue
		}
		name := `"` + strings.ReplaceAll(f.name, `"`, `""`) + `"`
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET `+name+`=? WHERE id=?`, f.value, r.id); err != nil {
			return err
		}
	}
	if handoffErr != r.handoffErr {
		if _, err := tx.ExecContext(ctx, `UPDATE session_handoffs SET error=? WHERE source=?`, handoffErr, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// A directory at an output path is a permanent discard barrier, never a file
// to unlink during a later redaction.
func removeTaskOutput(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Empty directories deny both old atomic scrollback writers and holders with
// open descriptors. Keep these barriers permanently after explicit discard.
func (s *Service) sealTaskOutput(id string) error {
	base := s.scrollbackPath(id)
	for _, path := range []string{base + ".tmp", base, holder.TaskOutputPath(s.d.Config.Home, id), holder.TaskErrorPath(s.d.Config.Home, id)} {
		info, err := os.Lstat(path)
		if err == nil && info.IsDir() {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
		if err = os.Mkdir(path, 0700); err != nil {
			return err
		}
	}
	return nil
}

// Replace the inode so a writer holding an old descriptor cannot append to
// the verified copy. The temporary file is private and on the same volume.
func replaceTaskOutput(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".task-scrub-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// taskWriterGone only probes. The child's recorded start identifies it even
// if its PID has since been reused; an absent group and a holder exit record
// prove the holder finished draining and cannot create another output file.
func (s *Service) taskWriterGone(ctx context.Context, id string) bool {
	var stopped, neverStarted bool
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT group_stopped,child_never_started FROM session_tasks WHERE id=?`, id).Scan(&stopped, &neverStarted); err != nil {
		return false
	}
	if stopped || neverStarted {
		return true
	}
	// The holder writes this only after draining and closing its output files.
	if exit, ok := holder.ReadExit(s.d.Config.Home, id); ok && exit.GroupStopped != nil && *exit.GroupStopped {
		return true
	}
	r, ok := s.load(ctx, id)
	if !ok || r.PID <= 1 || r.pidStart <= 0 {
		return false
	}
	if gone, _ := processGone(r.PID, r.pidStart, r.Started); !gone {
		return false
	}
	var pgid int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT pgid FROM session_tasks WHERE id=?`, id).Scan(&pgid); err != nil || pgid <= 1 {
		return false
	}
	if !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		return false
	}
	// A dead holder cannot write an exit record. Only trust that inference
	// when this is the same home in which its identity was recorded.
	var prior taskHomeIdentity
	var holderPID int
	var start int64
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino,holder_pid,holder_start FROM session_holders WHERE id=?`, id).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino, &holderPID, &start); err != nil || holderPID <= 1 {
		return false
	}
	home, err := s.currentTaskHome()
	if err != nil || home.id != prior.id || home.dev != prior.dev || home.ino != prior.ino {
		return false
	}
	if start <= 0 {
		// A zero start cannot identify a live PID, but ESRCH proves it absent.
		return errors.Is(syscall.Kill(holderPID, 0), syscall.ESRCH)
	}
	gone, _ := s.recordedHolderGone(ctx, id)
	return gone
}

func (s *Service) recoverTaskHolds(ctx context.Context, id string) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project,subjects,texts FROM session_task_holds WHERE task_id=?`, id)
	if err != nil {
		return
	}
	var holds []core.Scrub
	for rows.Next() {
		var sc core.Scrub
		var subjects, texts string
		if rows.Scan(&sc.Project, &subjects, &texts) == nil && json.Unmarshal([]byte(subjects), &sc.Subjects) == nil && json.Unmarshal([]byte(texts), &sc.Texts) == nil {
			holds = append(holds, sc)
		}
	}
	rows.Close()
	for _, sc := range holds {
		if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT OR IGNORE INTO session_task_compactions(project) VALUES(?)`, sc.Project); err != nil {
			continue
		}
		_ = s.scrubTasksMode(ctx, sc, "", true)
	}
}

func (s *Service) compactTaskScrubs(ctx context.Context) {
	// Same lock order as output delivery: task capture, then session output.
	// Keep a new hold from appearing between the checks and compaction.
	s.taskIO.Lock()
	defer s.taskIO.Unlock()
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project FROM session_task_compactions`)
	if err != nil {
		return
	}
	var projects []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			projects = append(projects, p)
		}
	}
	rows.Close()
	for _, p := range projects {
		var n int
		if s.d.Store.Registry().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM session_task_holds WHERE project=?) + (SELECT count(*) FROM session_output_holds WHERE project=?)`, p, p).Scan(&n) != nil || n > 0 {
			continue
		}
		if orchcore.Homes(s.d) != nil {
			orchcore.Homes(s.d).WaitIdle(ctx, p)
		}
		if err := s.d.Store.Compact(ctx, p); err == nil {
			if orchcore.Homes(s.d) != nil {
				if err := orchcore.Homes(s.d).PurgeBackups(ctx, p, true); err != nil {
					continue
				}
			}
			_, _ = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM session_task_compactions WHERE project=?`, p)
		}
	}
}

func scrubTaskTranscript(transcript string, texts []string, scoped bool) string {
	minLen := core.MinUnscoped
	if scoped {
		minLen = 0
	}
	cleaned, err := scrubTaskBytes(transcript, texts, minLen)
	if err != nil {
		return ""
	}
	return cleaned
}
