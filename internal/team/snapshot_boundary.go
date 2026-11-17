package team

import (
	"context"
	"github.com/sengirunners/potluck/kit/teamlink"
	"slices"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// snapshotAttempt separates outcomes accepted before the snapshot request
// from fresh writes accepted while it streams. Sequence numbers identify the
// exact accepted rows; a cutoff would also catch older pending writes.
type snapshotAttempt struct {
	epoch     uint64
	acked     []int64
	committed uint64 // the epoch this attempt's install left (applySnapshotAttempt), 0 until then
}

func (rp *replica) beginSnapshot(ctx context.Context) (snapshotAttempt, error) {
	rp.snapshotMu.Lock()
	defer rp.snapshotMu.Unlock()
	rp.writing.Lock()
	defer rp.writing.Unlock()
	rows, err := rp.db.QueryContext(ctx, `SELECT seq FROM outbox WHERE state = ? AND copy_fenced = 0 ORDER BY seq`, api.OutboxAcked)
	if err != nil {
		return snapshotAttempt{}, err
	}
	defer rows.Close()
	var attempt snapshotAttempt
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return snapshotAttempt{}, err
		}
		attempt.acked = append(attempt.acked, seq)
	}
	if err := rows.Err(); err != nil {
		return snapshotAttempt{}, err
	}
	// Fence replies already in flight before requesting canonical state.
	// Never roll this back after a failed request: an obsolete reply must
	// not become current again. Its durable outcome and waiter still settle.
	rp.snapshotEpoch++
	attempt.epoch = rp.snapshotEpoch
	if rp.windowFrom == 0 {
		rp.windowFrom = rp.snapshotEpoch // a retry after a failed attempt keeps the window
	}
	return attempt, nil
}

// tellLostAcks tells each session whose acked write a restore lost, while
// the snapshot that found it is still the copy's: one sent after a newer
// snapshot, a scope change or a leave would speak for a copy that no longer
// holds it. The durable failed outcomes were written by the install and stay.
// Refusals the snapshot un-fenced (unfenceLostRefusals) are told the same way.
// The role lifetime is admitted per note too: a revoke or leave after the
// install stops the rest (role_recovery.go).
func (s *Service) tellLostAcks(ctx context.Context, rp *replica, role teamlink.RoleReplyFence, committed uint64, gen int, acks, refused []outEntry) {
	type note struct{ as, text string }
	var notes []note
	for _, e := range acks {
		notes = append(notes, note{e.as, "queued " + e.kind + " lost: the home was restored from an older backup (" + e.item + ")"})
	}
	for _, e := range refused {
		notes = append(notes, note{e.as, "queued " + e.kind + " not applied: the home was restored and lost " + e.item})
	}
	for _, n := range notes {
		if n.as == "" {
			continue
		}
		if !role.Current() || !s.lostAckCurrent(ctx, role.X, rp, role.Snap, committed, gen) {
			return
		}
		s.setLocalState(n.as, orchapi.StateNeedsHuman, n.text)
	}
}

// lostAckCurrent takes each lock alone and holds none across SetState.
func (s *Service) lostAckCurrent(ctx context.Context, x *remote, rp *replica, snap api.Remote, committed uint64, gen int) bool {
	if ctx.Err() != nil || committed == 0 {
		return false
	}
	if !s.OwnsRoleRemote(x) {
		return false
	}
	x.Mu.Lock()
	cur, same := x.R, copyOf(x) == rp
	sameScope := cur.ScopeVersion == snap.ScopeVersion && cur.Cap == snap.Cap && cur.Role == snap.Role &&
		cur.State == snap.State && slices.Equal(cur.Areas, snap.Areas)
	x.Mu.Unlock()
	if !same || !sameScope {
		return false
	}
	rp.snapshotMu.RLock()
	defer rp.snapshotMu.RUnlock()
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.snapshotEpoch == committed && rp.resnap == gen && rp.snapped == gen
}
