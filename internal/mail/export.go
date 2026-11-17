package mail

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	store.RegisterExportFilter(func(ctx context.Context, db *sql.DB, addr string) error {
		return store.Tx(ctx, db, func(tx *sql.Tx) error {
			// Older databases may predate this module's migration.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('mail') WHERE name='audience'`).Scan(&n); err != nil || n == 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM mail WHERE audience != '' AND (audience != 'participants' OR NOT EXISTS (SELECT 1 FROM json_each(mail.participants) WHERE value=?))`, addr); err != nil {
				return err
			}
			for _, q := range []string{
				`DELETE FROM mail_seen WHERE mail_id NOT IN (SELECT id FROM mail)`,
				`DELETE FROM mail_import_sink WHERE mail_id NOT IN (SELECT id FROM mail)`,
				`DELETE FROM mail_requests WHERE mail_id NOT IN (SELECT id FROM mail)`,
			} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			// The send-limit ledger is the authority's short-lived count, never exported.
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mail_sends'`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM mail_sends`); err != nil {
					return err
				}
			}
			// Read marks are their person's: an export keeps only the exporter's.
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mail_read_marks'`).Scan(&n); err != nil || n == 0 {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM mail_read_marks WHERE identity != ?`, addr)
			return err
		})
	})
}
