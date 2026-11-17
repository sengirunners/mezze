package mail

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// HeldChecker is the approvals module's view of asks held for an outside
// party (SPEC 16, ask.for): their mail is needs=human (only a decision
// answers it) but it waits on that party, not on a person here. So it never
// holds its asker at needs_human, never pushes, and never counts as unread;
// listings carry the party in held_for.
type HeldChecker interface {
	HeldAsks(ctx context.Context, project string) (map[string]string, error)
}

// heldAsks maps project's open held asks (by mail id) to their party; nil
// when the approvals module is absent or cannot say.
func (s *Service) heldAsks(ctx context.Context, project string) map[string]string {
	s.mu.RLock()
	a := s.asks
	s.mu.RUnlock()
	h, ok := a.(HeldChecker)
	if !ok {
		return nil
	}
	m, err := h.HeldAsks(ctx, project)
	if err != nil {
		s.d.Log.Warn("mail: held asks", "project", project, "err", err)
		return nil
	}
	return m
}

// markHeld fills HeldFor on the open needs=human rows that are held asks
// (older rows did not record it).
func (s *Service) markHeld(ctx context.Context, rs []row) {
	byProject := map[string]map[string]string{}
	for i := range rs {
		r := &rs[i]
		if r.Needs != orchapi.NeedsHuman || r.AckedAt != nil || r.HeldFor != "" {
			continue // a held ask's row says so itself
		}
		held, ok := byProject[r.Project]
		if !ok {
			held = s.heldAsks(ctx, r.Project)
			byProject[r.Project] = held
		}
		r.HeldFor = held[r.ID]
	}
}

// openHumanCount counts sender's open needs=human mail in project db,
// leaving out held asks' mail (it waits on an outside party).
func (s *Service) openHumanCount(ctx context.Context, q querier, project, sender string) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM mail WHERE sender = ? AND needs = 'human' AND kind != 'report' AND acked_at IS NULL
		AND coalesce(json_extract(provenance, '$.held_for'), '') = ''`, sender)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	held := s.heldAsks(ctx, project)
	n := 0
	for _, id := range ids {
		if held[id] == "" {
			n++
		}
	}
	return n, nil
}
