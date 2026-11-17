package mirrorstate

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/store"
)

// Wipe forgets a tracker's mirror entirely (unbound, or bound to another
// adapter or config). A non-empty binding leaves, in the same transaction, a
// sync row owing an announced full listing: a relist, durable across a
// restart.
func Wipe(ctx context.Context, db *sql.DB, source, binding string) error {
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM work_mirror WHERE source = ?`, source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM work_mirror_sync WHERE source = ?`, source); err != nil || binding == "" {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO work_mirror_sync (source, binding, announce) VALUES (?, ?, 1)`, source, binding)
		return err
	})
}
