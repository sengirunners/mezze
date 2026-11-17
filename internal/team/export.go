package team

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	store.RegisterExportFilter(func(ctx context.Context, db *sql.DB, _ string) error {
		// Delivery response caches contain private mail bodies and ack metadata.
		// They are runtime transport state, never part of a portable project export.
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='team_writes'`).Scan(&n); err != nil || n == 0 {
			return err
		}
		_, err := db.ExecContext(ctx, `DELETE FROM team_writes`)
		return err
	})
}
