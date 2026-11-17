package team

// The replica's side of shared-session prompts (SPEC 17.10): which prompts
// this machine typed (so each is typed at most once, also across a daemon
// restart) and which the authority has yet to hear were delivered (acks go
// with the next heartbeat, also for a session that ended right after
// typing). A prompt is recorded ("typing") before it is typed and marked
// "typed" after: a prompt that could not be recorded is not typed (the next
// beat tries again), and one recorded but never marked (a crash in between)
// is never typed again nor acked; it expires pending at the authority.

import (
	"context"
	"database/sql"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/store"
)

var promptsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "team/0002_prompts_typed", Script: `
CREATE TABLE team_prompts_typed (
	id      TEXT PRIMARY KEY, -- the authority's prompt id
	project TEXT NOT NULL,    -- the authority's project ULID
	session TEXT NOT NULL,    -- the local session's address
	typed   INTEGER NOT NULL,
	acked   INTEGER NOT NULL DEFAULT 0 -- the authority heard it was delivered
);
CREATE INDEX team_prompts_typed_unacked ON team_prompts_typed (project, acked);`},
	{Kind: store.KindRegistry, Name: "team/0003_prompts_typing", Script: `
ALTER TABLE team_prompts_typed ADD COLUMN state TEXT NOT NULL DEFAULT 'typed'; -- typing | typed`},
}

// typedKeep is how long a typed prompt's record is kept (the authority
// expires a pending prompt long before).
const typedKeep = 24 * time.Hour

// claimTyping records that this machine is about to type prompt id into
// session. ok is false when it has a record of it already (typed, or being
// typed) or the record could not be written: then it is not typed.
func (s *Service) claimTyping(ctx context.Context, project, session, id string) bool {
	res, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_prompts_typed (id, project, session, typed, state) VALUES (?, ?, ?, ?, 'typing')
		ON CONFLICT (id) DO NOTHING`, id, project, session, store.Millis(time.Now()))
	if err != nil {
		s.d.Log.Warn("team: record a prompt before typing it (not typed; the next beat tries again)", "prompt", id, "err", err)
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1
}

// typingDone marks a claimed prompt typed (its ack goes with the next beat),
// or drops the claim when nothing was typed (the next beat tries again).
func (s *Service) typingDone(ctx context.Context, id string, typed bool) {
	q := `UPDATE team_prompts_typed SET state = 'typed' WHERE id = ?`
	if !typed {
		q = `DELETE FROM team_prompts_typed WHERE id = ? AND state = 'typing'`
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, q, id); err != nil {
		s.d.Log.Warn("team: record a typed prompt", "prompt", id, "typed", typed, "err", err)
	}
}

// unacked is project's typed prompts the authority has not heard of, by
// session.
func (s *Service) unacked(ctx context.Context, project string) map[string][]string {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT session, id FROM team_prompts_typed WHERE project = ? AND acked = 0 AND state = 'typed' ORDER BY typed, id`, project)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var sess, id string
		if rows.Scan(&sess, &id) == nil {
			out[sess] = append(out[sess], id)
		}
	}
	return out
}

// markAcked records that the authority took these acks (a heartbeat that
// carried them was answered).
func (s *Service) markAcked(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE team_prompts_typed SET acked = 1 WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.d.Log.Warn("team: mark prompts acked", "err", err)
	}
}

// pruneTyped drops records older than typedKeep.
func (s *Service) pruneTyped(ctx context.Context) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM team_prompts_typed WHERE typed < ?`, store.Millis(time.Now().Add(-typedKeep)))
}

// endedAcks are presence entries for sessions of the project that ended
// with acks still owed (typed just before they ended): the authority marks
// those prompts delivered before it ends the session (which expires what is
// still pending), and refuses the entry itself (an ended state).
func endedAcks(unacked map[string][]string, live map[string]orchapi.Session) []orchapi.PresenceSession {
	var out []orchapi.PresenceSession
	for addr, ids := range unacked {
		if _, ok := live[addr]; ok {
			continue
		}
		out = append(out, orchapi.PresenceSession{Address: addr, State: orchapi.StateEnded, Started: time.Now(), Delivered: ids})
	}
	return out
}
