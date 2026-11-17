package mirrorstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/mirrorquery"
)

type scanner interface{ Scan(...any) error }

// Scan decodes mirror columns, overlaying stored row fields on the JSON item.
// Accepting a scanner keeps transaction reads on their caller's transaction.
func Scan(sc scanner, project, source string) (orchapi.WorkItem, error) {
	var it orchapi.WorkItem
	var id, area, st, title, body, raw string
	if err := sc.Scan(&id, &area, &st, &title, &body, &raw); err != nil {
		return it, err
	}
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		return it, err
	}
	it.ID, it.Area, it.Status, it.Title, it.Body = id, area, orchapi.WorkStatus(st), title, body
	it.Project, it.Source = project, source
	return it, nil
}

// Get reads one mirrored item; a missing item returns ok false without an error.
func Get(ctx context.Context, db *sql.DB, project, source, ref string) (orchapi.WorkItem, bool, error) {
	it, err := Scan(db.QueryRowContext(ctx, `SELECT `+mirrorquery.Columns+` FROM work_mirror WHERE source = ? AND ref = ?`, source, ref), project, source)
	if errors.Is(err, sql.ErrNoRows) {
		return orchapi.WorkItem{}, false, nil
	}
	return it, err == nil, err
}

// List decodes a caller's scoped query in its selected order.
func List(ctx context.Context, db *sql.DB, query string, args []any, project, source string) ([]orchapi.WorkItem, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orchapi.WorkItem
	for rows.Next() {
		it, err := Scan(rows, project, source)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Count returns the number of mirrored items for one source.
func Count(ctx context.Context, db *sql.DB, source string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_mirror WHERE source = ?`, source).Scan(&n)
	return n, err
}

// Sources lists trackers a project's mirror holds state for.
func Sources(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT source FROM work_mirror_sync UNION SELECT DISTINCT source FROM work_mirror`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
