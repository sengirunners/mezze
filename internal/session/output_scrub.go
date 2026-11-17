package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var outputScrubMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0019_output_holds", Script: `
CREATE TABLE session_output_holds(session_id TEXT NOT NULL,hold_key TEXT NOT NULL,project TEXT NOT NULL,subjects TEXT NOT NULL,texts TEXT NOT NULL,min_len INTEGER NOT NULL DEFAULT 8,created INTEGER NOT NULL,PRIMARY KEY(session_id,hold_key));
CREATE INDEX session_output_holds_project ON session_output_holds(project);
ALTER TABLE sessions ADD COLUMN output_drained INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN output_redacted INTEGER NOT NULL DEFAULT 0;`},
}

// outputIO covers the durable barrier and actual delivery, not just a read of
// output that a handler may cache. Failed barrier queries withhold output too.
func (s *Service) sessionOutputHeld(ctx context.Context, id string) (bool, error) {
	if s == nil || s.d == nil || s.d.Store == nil || s.d.Store.Registry() == nil {
		return true, errRegistryClosed
	}
	var held bool
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_output_holds WHERE session_id=?)`, id).Scan(&held)
	return held, err
}

func (s *Service) withSessionOutput(ctx context.Context, id string, fn func() error) error {
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	held, err := s.sessionOutputHeld(ctx, id)
	if err != nil {
		return err
	}
	if held {
		return api.Errorf(api.CodeConflict, "session output withheld pending redaction")
	}
	return fn()
}

func (s *Service) sessionOutputPaths(id string) []string {
	return []string{s.scrollbackPath(id), holder.PipeEventsPath(s.d.Config.Home, id), holder.PipeErrorPath(s.d.Config.Home, id)}
}

// A holder's exit is written after its output files have closed. Persist that
// evidence before Forget removes the exit, so a later scrub need not infer it
// from a reused PID or a session state label.
func (s *Service) recordSessionDrain(ctx context.Context, id string) {
	if s.d.Store == nil || s.d.Store.Registry() == nil {
		return
	}
	if _, task := s.taskIDs.Load(id); task {
		return
	}
	var prior taskHomeIdentity
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino FROM session_holders WHERE id=?`, id).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino); err != nil {
		return
	}
	home, err := s.currentTaskHome()
	if err != nil || home != prior {
		return
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE sessions SET output_drained=1 WHERE id=?`, id)
	if err != nil {
		s.d.Log.Warn("record session output drain", "session", id, "err", err)
	}
}

func (s *Service) sessionWriterGone(ctx context.Context, r row) bool {
	if s.proc(r.ID) != nil {
		return false
	}
	var drained bool
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT output_drained FROM sessions WHERE id=?`, r.ID).Scan(&drained); err != nil {
		return false
	}
	if drained {
		return true
	}
	if !r.Managed {
		return true
	} // hook sessions have no holder/captured writer
	var prior taskHomeIdentity
	var pid int
	var start int64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT holder_pid,holder_start,home_id,home_path,home_dev,home_ino FROM session_holders WHERE id=?`, r.ID).Scan(&pid, &start, &prior.id, &prior.path, &prior.dev, &prior.ino)
	if err != nil || pid <= 1 {
		return false
	}
	home, err := s.currentTaskHome()
	if err != nil || home != prior {
		return false
	}
	if _, ok := holder.ReadExit(s.d.Config.Home, r.ID); ok {
		return true
	}
	gone, _ := s.recordedHolderGone(ctx, r.ID)
	return gone
}

func outputMinLen(r row, sc core.Scrub) int {
	for _, subject := range sc.Subjects {
		if r.Work != "" && strings.TrimPrefix(subject, "bd:") == strings.TrimPrefix(r.Work, "bd:") {
			return 0
		}
	}
	return core.MinUnscoped
}

func cleanSessionText(v string, texts []string, minLen int) (string, error) {
	out, err := scrubTaskBytes(v, texts, minLen)
	if err != nil {
		return "", err
	}
	safe, err := taskRedactionSafe(out, texts, minLen)
	if err != nil {
		return "", err
	}
	if !safe {
		return "", fmt.Errorf("session output could not be verified after redaction")
	}
	return out, nil
}

// Keep JSON syntax and event envelope identities intact. Decode nested JSON
// strings too: tool arguments are often serialized JSON inside an event.
func cleanSessionJSON(v any, texts []string, minLen, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("session event nesting exceeds redaction bound")
	}
	switch x := v.(type) {
	case string:
		trim := strings.TrimSpace(x)
		if strings.HasPrefix(trim, "{") || strings.HasPrefix(trim, "[") {
			var nested any
			dec := json.NewDecoder(strings.NewReader(x))
			dec.UseNumber()
			if dec.Decode(&nested) == nil && json.Valid([]byte(x)) {
				cleaned, err := cleanSessionJSON(nested, texts, minLen, depth+1)
				if err != nil {
					return nil, err
				}
				b, err := json.Marshal(cleaned)
				return string(b), err
			}
		}
		return cleanSessionText(x, texts, minLen)
	case []any:
		for i := range x {
			cleaned, err := cleanSessionJSON(x[i], texts, minLen, depth+1)
			if err != nil {
				return nil, err
			}
			x[i] = cleaned
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			key, err := cleanSessionText(k, texts, minLen)
			if err != nil {
				return nil, err
			}
			cleaned, err := cleanSessionJSON(value, texts, minLen, depth+1)
			if err != nil {
				return nil, err
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("redacted event keys collide")
			}
			out[key] = cleaned
		}
		return out, nil
	}
	return v, nil
}

func cleanSessionEvents(data []byte, texts []string, minLen int) ([]byte, error) {
	var out bytes.Buffer
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if !json.Valid(line) || dec.Decode(&event) != nil || event == nil {
			return nil, fmt.Errorf("invalid session event spool")
		}
		for k, value := range event {
			switch k {
			case "seq", "type", "at":
				continue
			}
			cleaned, err := cleanSessionJSON(value, texts, minLen, 0)
			if err != nil {
				return nil, err
			}
			event[k] = cleaned
		}
		b, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func (s *Service) scrubSessionOutputs(ctx context.Context, sc core.Scrub) error {
	if len(sc.Texts) == 0 {
		return nil
	}
	s.outputIO.Lock()
	defer s.outputIO.Unlock()
	db := s.d.Store.Registry()
	subjects, _ := json.Marshal(sc.Subjects)
	texts, _ := json.Marshal(sc.Texts)
	sum := sha256.Sum256(append(append([]byte(sc.Project), subjects...), texts...))
	key := fmt.Sprintf("%x", sum)
	// A move changes the current scope, not the redaction already owed by
	// this session. Recover only its exact original project/hold identity.
	rows, err := db.QueryContext(ctx, `SELECT `+cols+` FROM sessions WHERE (project=? OR id IN (SELECT session_id FROM session_output_holds WHERE project=? AND hold_key=?)) AND id NOT IN (SELECT id FROM session_tasks)`, sc.Project, sc.Project, key)
	if err != nil {
		return err
	}
	var copies []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return err
		}
		copies = append(copies, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	pending := false
	for _, r := range copies {
		minLen := outputMinLen(r, sc)
		// A live session is held when the redaction is about its own work (its
		// agent may have the text and emit it later) or when any copy already
		// carries the text. Unrelated live sessions never block a redaction:
		// zm does not hand issue text to a session working something else.
		affected := minLen == 0 && r.Managed && r.State != orchapi.StateEnded
		metadata, e := s.taskSessionTexts(ctx, r.ID)
		if e != nil {
			return e
		}
		for _, f := range metadata {
			switch f.name {
			case "title", "note", "activity", "term_title", "command", "cwd", "worktree", "branch", "scratch", "sandbox", "outcome", "runtime_agent", "subagents":
				safe, e := taskRedactionSafe(f.value, sc.Texts, minLen)
				affected = affected || e != nil || !safe
			}
		}
		activityRows, e := db.QueryContext(ctx, `SELECT text FROM session_activity WHERE session=?`, r.ID)
		if e != nil {
			return e
		}
		for activityRows.Next() {
			var text string
			if e = activityRows.Scan(&text); e != nil {
				break
			}
			safe, checkErr := taskRedactionSafe(text, sc.Texts, minLen)
			affected = affected || checkErr != nil || !safe
		}
		if e == nil {
			e = activityRows.Err()
		}
		activityRows.Close()
		if e != nil {
			return e
		}
		for _, path := range s.sessionOutputPaths(r.ID) {
			data, e := os.ReadFile(path)
			if e == nil {
				safe, e := taskRedactionSafe(string(data), sc.Texts, minLen)
				affected = affected || e != nil || !safe
			} else if !errors.Is(e, os.ErrNotExist) {
				affected = true
			}
		}
		if p := s.proc(r.ID); p != nil && p.out != nil {
			data, _, _, _, _ := p.out.Next(0, scrollbackSize)
			safe, e := taskRedactionSafe(string(data), sc.Texts, minLen)
			affected = affected || e != nil || !safe
		}
		held, e := s.sessionOutputHeld(ctx, r.ID)
		if e != nil {
			return e
		}
		affected = affected || held
		if !affected {
			continue
		}
		if s.outputRevisions == nil {
			s.outputRevisions = map[string]uint64{}
		}
		s.outputRevisions[r.ID]++
		// Commit withholding before touching streams/files. It survives interruption,
		// failed rewrites, and adoption of a still-running old holder.
		if _, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO session_output_holds(session_id,hold_key,project,subjects,texts,min_len,created) VALUES(?,?,?,?,?,?,?)`, r.ID, key, sc.Project, string(subjects), string(texts), minLen, time.Now().UnixMilli()); err != nil {
			return err
		}
		// Keep the creation-time match policy even if session metadata later
		// changes. Replaying a hold must never weaken its original scope.
		if err = db.QueryRowContext(ctx, `SELECT min_len FROM session_output_holds WHERE session_id=? AND hold_key=?`, r.ID, key).Scan(&minLen); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `UPDATE sessions SET output_redacted=1 WHERE id=?`, r.ID); err != nil {
			return err
		}
		if p := s.proc(r.ID); p != nil {
			p.closeConns()
		}
		if !s.sessionWriterGone(ctx, r) {
			pending = true
			continue
		}
		for i, path := range s.sessionOutputPaths(r.ID) {
			data, e := os.ReadFile(path)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return e
			}
			var cleaned []byte
			if i == 1 || i == 0 && r.Mode == orchapi.SessionModePipe {
				cleaned, e = cleanSessionEvents(data, sc.Texts, minLen)
			} else {
				var text string
				text, e = cleanSessionText(string(data), sc.Texts, minLen)
				cleaned = []byte(text)
			}
			// Malformed/ambiguous captured text is discarded, rather than releasing an
			// unverifiable copy. Writer death was established before reaching here.
			if e != nil {
				err = removeTaskOutput(path)
			} else {
				err = replaceTaskOutput(path, cleaned)
			}
			if err != nil {
				return err
			}
			s.pipeIdx.Delete(r.ID)
		}
		if err = s.scrubSessionMetadata(ctx, r.ID, sc, minLen); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO session_task_compactions(project) VALUES(?)`, sc.Project); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `DELETE FROM session_output_holds WHERE session_id=? AND hold_key=?`, r.ID, key); err != nil {
			return err
		}
	}
	if pending {
		return api.Errorf(api.CodeUnavailable, "session output is still being recorded")
	}
	return nil
}

func (s *Service) recoverSessionOutputHolds(ctx context.Context) {
	if s.d.Store == nil || s.d.Store.Registry() == nil {
		return
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT DISTINCT project,subjects,texts FROM session_output_holds`)
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
		if err := s.scrubSessionOutputs(ctx, sc); err != nil && !api.IsCode(err, api.CodeUnavailable) {
			s.d.Log.Warn("recover session output redaction", "err", err)
		}
	}
}

// saveSessionOutput serializes the daemon's final ring save with redaction.
// Held output is scrubbed before writing, and the durable hold stays until the
// holder and this proc have both finished. Task capture keeps its own policy.
func (s *Service) saveSessionOutput(id string, data []byte) error {
	if _, task := s.taskIDs.Load(id); task {
		return s.saveScrollback(id, data)
	}
	s.outputIO.Lock()
	defer s.outputIO.Unlock()
	if s.d.Store == nil || s.d.Store.Registry() == nil {
		return errRegistryClosed
	}
	rows, err := s.d.Store.Registry().QueryContext(context.Background(), `SELECT texts,min_len FROM session_output_holds WHERE session_id=?`, id)
	if err != nil {
		return err
	}
	type heldTexts struct {
		texts  []string
		minLen int
	}
	var holds []heldTexts
	for rows.Next() {
		var raw string
		var hold heldTexts
		if err = rows.Scan(&raw, &hold.minLen); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &hold.texts); err != nil {
			break
		}
		holds = append(holds, hold)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	r, ok := s.load(context.Background(), id)
	if !ok {
		return sql.ErrNoRows
	}
	for _, hold := range holds {
		if r.Mode == orchapi.SessionModePipe {
			data, err = cleanSessionEvents(data, hold.texts, hold.minLen)
		} else {
			var text string
			text, err = cleanSessionText(string(data), hold.texts, hold.minLen)
			data = []byte(text)
		}
		if err != nil {
			return removeTaskOutput(s.scrollbackPath(id))
		}
	}
	return s.saveScrollback(id, data)
}

// Captured output also feeds these descriptive fields. Keep scope, runtime
// identities, lifecycle state and decision metadata untouched.
func (s *Service) scrubSessionMetadata(ctx context.Context, id string, sc core.Scrub, minLen int) error {
	_, _, err := updateRow(ctx, s.d.Store.Registry(), id, func(r *row) (bool, error) {
		clean := func(v *string) {
			out, e := cleanSessionText(*v, sc.Texts, minLen)
			if e != nil {
				out = ""
			}
			*v = out
		}
		for _, v := range []*string{&r.Title, &r.Note, &r.Activity, &r.TermTitle, &r.Cwd, &r.Worktree, &r.Branch, &r.Scratch, &r.Sandbox} {
			clean(v)
		}
		for i := range r.Command {
			clean(&r.Command[i])
		}
		if r.Outcome != nil {
			clean(&r.Outcome.Evidence)
			clean(&r.Outcome.Note)
		}
		if r.RuntimeAgent != nil {
			clean(&r.RuntimeAgent.Task)
		}
		for i := range r.subs {
			clean(&r.subs[i].Description)
			clean(&r.subs[i].Activity)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT seq,text FROM session_activity WHERE session=?`, id)
	if err != nil {
		return err
	}
	type line struct {
		seq  int64
		text string
	}
	var lines []line
	for rows.Next() {
		var l line
		if err = rows.Scan(&l.seq, &l.text); err != nil {
			break
		}
		lines = append(lines, l)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, l := range lines {
		cleaned, e := cleanSessionText(l.text, sc.Texts, minLen)
		if e != nil {
			cleaned = ""
		}
		if cleaned != l.text {
			if _, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_activity SET text=? WHERE seq=? AND session=?`, cleaned, l.seq, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollbackUnstartedSessionRow is only for the three spawn rollback paths before
// startHolder. It must not be used to delete a row whose process might write.
// Taking the same barrier as selection/hold creation makes that rollback one
// operation: a scrub either holds the row before this cleanup, or never sees it.
func (s *Service) rollbackUnstartedSessionRow(ctx context.Context, db *sql.DB, id string) error {
	s.outputIO.Lock()
	defer s.outputIO.Unlock()
	if s.proc(id) != nil {
		return api.Errorf(api.CodeConflict, "cannot roll back a started session")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pid int
	var holderRecorded bool
	err = tx.QueryRowContext(ctx, `SELECT pid,EXISTS(SELECT 1 FROM session_holders WHERE id=sessions.id) FROM sessions WHERE id=?`, id).Scan(&pid, &holderRecorded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if pid > 1 || holderRecorded {
		return api.Errorf(api.CodeConflict, "cannot roll back a session with a recorded child or holder")
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_task_compactions(project) SELECT project FROM session_output_holds WHERE session_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM session_output_holds WHERE session_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// withSessionDesignation keeps generic output redaction from observing a task
// as an ordinary session between its session and task inserts. Only the short
// registry setup belongs here: no launch, rollback, publish, or taskIO lock.
func (s *Service) withSessionDesignation(fn func() error) error {
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	return fn()
}
