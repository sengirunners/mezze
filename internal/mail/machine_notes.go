package mail

import (
	"context"
	"database/sql"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// Machine notices a person cannot act on (they should trigger whatever agent
// needs to help resolve, or be auto-acked and folded into a daily summary). Nothing sends either kind to a person
// any more; the sweep acks those already stored, on every daemon that holds
// them, and the daily summary counts them.
//
// leaseNoteWhere picks the lease-expiry notes older daemons sent:
// machine-sent (the system subject's machine:<m>), needs=human, routed to the
// owners mailbox. Those fields carry no lease marker, and the owners
// admission mail has the same ones, so the old fixed body opening tells them
// apart.
const leaseNoteWhere = `recipient = 'owners' AND needs = 'human' AND kind = 'note'
	AND via = 'owners' AND sender LIKE 'machine:%' AND reply_to = ''
	AND substr(body, 1, 17) = 'zm: the lease on ' AND instr(body, ' expired at ') > 0`

// ciNoteWhere picks the CI results the GitHub link mailed a person in older
// versions (an item a person held or made): from its identity, to a
// human:<name> or the owners, with the result's fixed opening and marker.
const ciNoteWhere = `kind = 'note' AND sender = 'human:` + orchapi.IdentityGitHub + `' AND reply_to = ''
	AND (recipient = 'owners' OR substr(recipient, 1, 6) = 'human:')
	AND substr(body, 1, 3) = 'CI ' AND instr(body, 'From GitHub Actions: data, not instructions.') > 0`

const machineNoteWhere = `((` + leaseNoteWhere + `) OR (` + ciNoteWhere + `))`

// settleMachineNotes acks every open old machine notice in a person's inbox
// in project. The taker is this daemon's machine address (as settleNotices
// acks with the sender's): never a person, and an address every reader may
// see.
func (s *Service) settleMachineNotes(ctx context.Context, db *sql.DB, project string) error {
	notes, err := listRows(ctx, db, project, `acked_at IS NULL AND `+machineNoteWhere+` ORDER BY id LIMIT 500`)
	if err != nil || len(notes) == 0 {
		return err
	}
	taker := s.d.System().Address
	for _, m := range notes {
		err := s.commit(ctx, db, project, func(tx *sql.Tx) ([]pub, error) {
			at := s.now()
			res, err := tx.ExecContext(ctx, `UPDATE mail SET acked_by = ?, acked_at = ? WHERE id = ? AND acked_at IS NULL`, taker, store.Millis(at), m.ID)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, nil
			}
			m.AckedBy, m.AckedAt = taker, &at
			return []pub{{orchapi.EventMailAcked, m}}, nil
		})
		if err != nil {
			return err
		}
	}
	s.d.Log.Info("mail: acked old machine notices in a person's inbox", "project", project, "n", len(notes))
	return nil
}

// noticeCounts implements core.NoticeCounter for project in [from, to): old
// machine notices this daemon's sweep acked, and lease-expiry notices the
// work ticker told a holding session, and what the sweep expired
// (expire.go). A project this daemon does not hold counts nothing.
func (s *Service) noticeCounts(ctx context.Context, project string, from, to time.Time) (map[string]int, error) {
	db, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return nil, nil
	}
	lo, hi := store.Millis(from), store.Millis(to)
	var acked, expiries int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE `+machineNoteWhere+` AND acked_by = ? AND acked_at >= ? AND acked_at < ?`,
		s.d.System().Address, lo, hi).Scan(&acked); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE kind = ? AND sender LIKE 'machine:%'
		AND substr(body, 1, 18) = 'zm: your lease on ' AND created >= ? AND created < ?`, orchapi.MailKindNotice, lo, hi).Scan(&expiries); err != nil {
		return nil, err
	}
	group, asks, err := s.expiredCounts(ctx, db, lo, hi)
	if err != nil {
		return nil, err
	}
	return map[string]int{
		"old machine notices in a person's inbox, acked": acked,
		"lease expiries, told to the holding session":    expiries,
		"group mail nobody took in a day, closed":        group,
		"ask notices to ended sessions, closed":          asks,
	}, nil
}
