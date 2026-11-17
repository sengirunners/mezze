package mirrorstate

import (
	"context"
	"database/sql"
)

// The marked predicate matches the rollup index exactly, including epics,
// so this reads the covering index rather than every mirrored item's JSON.
const deadlineIDsQuery = `SELECT id FROM work_mirror
 WHERE source = ? AND (json_extract(item, '$.extra.issue_type') = 'epic' OR json_extract(item, '$.extra.due_at') IS NOT NULL OR json_extract(item, '$.extra.defer_until') IS NOT NULL)
 AND (coalesce(json_extract(item, '$.extra.due_at'), '') != '' OR coalesce(json_extract(item, '$.extra.defer_until'), '') != '')`

func deadlineIDs(ctx context.Context, db *sql.DB, source string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, deadlineIDsQuery, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
