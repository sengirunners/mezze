package sqlite

// bd sql over native issues: one read-only SELECT over in-memory copies of the
// tables it names, shaped like beads' Dolt tables (bd v1.0.5).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"modernc.org/sqlite"

	istore "github.com/sengirunners/mezze/internal/issues/store"
)

// KV implements istore.ProjectStore.
func (p *project) KV(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT key, value FROM kv WHERE substr(key, 1, ?) = ? ORDER BY key`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetKV implements istore.ProjectStore.
func (p *project) SetKV(ctx context.Context, key, value string) (string, error) {
	var old string
	err := p.kvTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&old); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	})
	return old, err
}

// DeleteKV implements istore.ProjectStore.
func (p *project) DeleteKV(ctx context.Context, key string) (string, error) {
	var old string
	err := p.kvTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&old); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
		return err
	})
	return old, err
}

func (p *project) kvTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// bdCol is one column of a bd-shaped table: its name, its expression over
// the native table, and its declared type (DATETIME marks a time column).
type bdCol struct{ name, expr, typ string }

// bdTable is one of beads' tables, built from a native one.
type bdTable struct {
	name, from, where string
	cols              []bdCol
}

func ex(key, def string) string {
	return "coalesce(json_extract(nullif(extra, ''), '$." + key + "'), " + def + ")"
}

func dt(col string) string {
	return "strftime('%Y-%m-%d %H:%M:%S', " + col + " / 1000, 'unixepoch')"
}

// bdTables are beads' tables (bd v1.0.5's Dolt schema) over the native ones.
// Times are DATETIME text in UTC ("YYYY-MM-DD HH:MM:SS"), so MySQL-style
// comparisons with such literals and SQLite's date functions work; ReadSQL
// prints them as bd sql did ("...T...Z"). Empty optional strings are NULL
// where beads stores NULL (assignee, external_ref, spec_id). zm adds number,
// area and parent to issues.
var bdTables = []bdTable{
	{name: "issues", from: "issues", cols: []bdCol{
		{"id", "id", "TEXT"}, {"content_hash", "NULL", "TEXT"}, {"title", "title", "TEXT"}, {"description", "description", "TEXT"},
		{"design", "design", "TEXT"}, {"acceptance_criteria", "acceptance", "TEXT"}, {"notes", "notes", "TEXT"},
		{"status", "status", "TEXT"}, {"priority", "priority", "INTEGER"}, {"issue_type", "issue_type", "TEXT"},
		{"assignee", "nullif(assignee, '')", "TEXT"}, {"estimated_minutes", "estimated_minutes", "INTEGER"},
		{"created_at", dt("created_at"), "DATETIME"}, {"created_by", "created_by", "TEXT"}, {"owner", "owner", "TEXT"},
		{"updated_at", dt("updated_at"), "DATETIME"}, {"started_at", dt("started_at"), "DATETIME"},
		{"closed_at", dt("closed_at"), "DATETIME"}, {"closed_by_session", "closed_by_session", "TEXT"},
		{"external_ref", "nullif(external_ref, '')", "TEXT"}, {"spec_id", "nullif(spec_id, '')", "TEXT"},
		{"compaction_level", ex("compaction_level", "0"), "INTEGER"}, {"sender", ex("sender", "''"), "TEXT"},
		{"ephemeral", "ephemeral", "INTEGER"}, {"wisp_type", ex("wisp_type", "''"), "TEXT"}, {"pinned", "pinned", "INTEGER"},
		{"is_template", "is_template", "INTEGER"}, {"mol_type", ex("mol_type", "''"), "TEXT"},
		{"work_type", ex("work_type", "'mutex'"), "TEXT"}, {"source_system", "source_system", "TEXT"},
		{"metadata", "coalesce(nullif(metadata, ''), '{}')", "TEXT"}, {"close_reason", "close_reason", "TEXT"},
		{"event_kind", ex("event_kind", "''"), "TEXT"}, {"actor", ex("actor", "''"), "TEXT"}, {"target", ex("target", "''"), "TEXT"},
		{"payload", ex("payload", "''"), "TEXT"}, {"await_type", ex("await_type", "''"), "TEXT"},
		{"await_id", ex("await_id", "''"), "TEXT"}, {"timeout_ns", ex("timeout_ns", "0"), "INTEGER"},
		{"waiters", ex("waiters", "''"), "TEXT"}, {"due_at", dt("due_at"), "DATETIME"}, {"defer_until", dt("defer_until"), "DATETIME"},
		{"no_history", ex("no_history", "0"), "INTEGER"}, {"is_blocked", "is_blocked", "INTEGER"},
		{"number", "number", "INTEGER"}, {"area", "area", "TEXT"}, {"parent", "nullif(parent, '')", "TEXT"},
	}},
	{name: "dependencies", from: "deps", cols: []bdCol{
		{"issue_id", "issue_id", "TEXT"}, {"depends_on_issue_id", "CASE WHEN external = 0 THEN depends_on END", "TEXT"},
		{"depends_on_wisp_id", "NULL", "TEXT"}, {"depends_on_external", "CASE WHEN external != 0 THEN depends_on END", "TEXT"},
		{"type", "type", "TEXT"}, {"created_at", dt("created_at"), "DATETIME"}, {"created_by", "created_by", "TEXT"},
		{"metadata", "coalesce(nullif(metadata, ''), '{}')", "TEXT"}, {"thread_id", "thread_id", "TEXT"},
	}},
	{name: "labels", from: "labels", cols: []bdCol{{"issue_id", "issue_id", "TEXT"}, {"label", "label", "TEXT"}}},
	{name: "comments", from: "comments", where: "deleted_at IS NULL", cols: []bdCol{
		{"id", "id", "TEXT"}, {"issue_id", "issue_id", "TEXT"}, {"author", "author", "TEXT"}, {"text", "text", "TEXT"},
		{"created_at", dt("created_at"), "DATETIME"},
	}},
	{name: "events", from: "history", cols: []bdCol{
		{"id", "id", "TEXT"}, {"issue_id", "issue_id", "TEXT"}, {"event_type", "kind", "TEXT"}, {"actor", "actor", "TEXT"},
		{"old_value", "json_extract(nullif(fields, ''), '$.value[0]')", "TEXT"},
		{"new_value", "json_extract(nullif(fields, ''), '$.value[1]')", "TEXT"},
		{"comment", "nullif(comment, '')", "TEXT"}, {"created_at", dt("created_at"), "DATETIME"},
	}},
	{name: "config", from: "kv", cols: []bdCol{{"key", "key", "TEXT"}, {"value", "value", "TEXT"}}},
	{name: "child_counters", from: "child_counters", cols: []bdCol{{"parent_id", "parent", "TEXT"}, {"last_child", "last_child", "INTEGER"}}},
}

func (t bdTable) ddl() (create, fill string) {
	var defs, exprs []string
	for _, c := range t.cols {
		defs = append(defs, `"`+c.name+`" `+c.typ)
		exprs = append(exprs, c.expr)
	}
	fill = "INSERT INTO main." + t.name + " SELECT " + strings.Join(exprs, ", ") + " FROM src." + t.from
	if t.where != "" {
		fill += " WHERE " + t.where
	}
	return "CREATE TABLE main." + t.name + " (" + strings.Join(defs, ", ") + ")", fill
}

var datetimeText = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

// sqlSlots bounds concurrent bd sql queries (each copies its tables).
var sqlSlots = make(chan struct{}, 2)

const (
	// maxSQLValue caps one string or blob inside a query (SQLITE_LIMIT_LENGTH);
	// maxSQLAnswer caps the whole answer's text.
	maxSQLValue       = 4 << 20
	maxSQLAnswer      = 32 << 20
	sqliteLimitLength = 0 // SQLITE_LIMIT_LENGTH
)

// ReadSQL implements istore.SQLReader. The query never sees the project
// file: the bd tables it names are copied, in one read transaction, from a
// read-only attachment into an in-memory database, the attachment is
// detached, and the connection is made query_only. So native columns that
// zm redacts (session addresses) and the file's path are out of reach.
func (p *project) ReadSQL(ctx context.Context, query string, maxRows int, visibleEdge func(string) (bool, error)) (istore.SQLResult, error) {
	q, err := oneSelect(query)
	if err != nil {
		return istore.SQLResult{}, err
	}
	select {
	case sqlSlots <- struct{}{}:
	case <-ctx.Done():
		return istore.SQLResult{}, ctx.Err()
	}
	defer func() { <-sqlSlots }()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return istore.SQLResult{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return istore.SQLResult{}, err
	}
	defer conn.Close()
	// A URI: the path percent-encoded, so ?, # or % in ZM_HOME cannot change which file opens.
	src := (&url.URL{Scheme: "file", Path: p.path, RawQuery: "mode=ro"}).String()
	setup := []string{`ATTACH DATABASE '` + strings.ReplaceAll(src, "'", "''") + `' AS src`, `BEGIN`}
	for _, t := range bdTables {
		if mentions(q, t.name) {
			c, f := t.ddl()
			setup = append(setup, c, f)
		}
	}
	setup = append(setup, `COMMIT`, `DETACH DATABASE src`)
	for _, s := range setup {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return istore.SQLResult{}, fmt.Errorf("bd tables: %w", err)
		}
	}
	if visibleEdge != nil && mentions(q, "dependencies") {
		rows, err := conn.QueryContext(ctx, `SELECT DISTINCT depends_on_external FROM dependencies WHERE depends_on_external IS NOT NULL`)
		if err != nil {
			return istore.SQLResult{}, err
		}
		var hidden []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return istore.SQLResult{}, err
			}
			ok, err := visibleEdge(id)
			if err != nil {
				rows.Close()
				return istore.SQLResult{}, err
			}
			if !ok {
				hidden = append(hidden, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return istore.SQLResult{}, err
		}
		rows.Close()
		for _, id := range hidden {
			if _, err := conn.ExecContext(ctx, `DELETE FROM dependencies WHERE depends_on_external = ?`, id); err != nil {
				return istore.SQLResult{}, err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		return istore.SQLResult{}, err
	}
	if _, err := sqlite.Limit(conn, sqliteLimitLength, maxSQLValue); err != nil {
		return istore.SQLResult{}, err
	}
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return istore.SQLResult{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return istore.SQLResult{}, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return istore.SQLResult{}, err
	}
	out := istore.SQLResult{Columns: cols, Rows: [][]any{}}
	size := 0
	for rows.Next() {
		if len(out.Rows) >= maxRows {
			return istore.SQLResult{}, istore.ErrTooManyRows
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return istore.SQLResult{}, err
		}
		for i, x := range vals {
			switch t := x.(type) {
			case []byte:
				vals[i] = string(t)
				size += len(t)
			case string:
				size += len(t)
				// A time column, or an expression over one (max(created_at)); never a TEXT column.
				if dtType := strings.ToUpper(types[i].DatabaseTypeName()); (dtType == "DATETIME" || dtType == "") && datetimeText.MatchString(t) {
					vals[i] = t[:10] + "T" + t[11:] + "Z"
				}
			case bool:
				vals[i] = map[bool]int64{false: 0, true: 1}[t]
			}
			size += 8
		}
		if size > maxSQLAnswer {
			return istore.SQLResult{}, istore.ErrTooLarge
		}
		out.Rows = append(out.Rows, vals)
	}
	return out, rows.Err()
}

var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// mentions reports whether name appears as a word in q (a bd table the
// query may read; copying more than it names costs time only).
func mentions(q, name string) bool {
	for _, w := range wordRe.FindAllString(q, -1) {
		if strings.EqualFold(w, name) {
			return true
		}
	}
	return false
}

// oneSelect returns query without trailing semicolons when it is one SELECT
// or WITH statement: no other semicolon outside quotes and comments.
func oneSelect(query string) (string, error) {
	q := strings.TrimSpace(query)
	for strings.HasSuffix(q, ";") {
		q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	}
	head := strings.ToUpper(q)
	if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") {
		return "", istore.ErrNotSelect
	}
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case '\'', '"', '`':
			j := i + 1
			for j < len(q) && q[j] != c {
				j++
			}
			i = j
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				for i < len(q) && q[i] != '\n' {
					i++
				}
			}
		case '/':
			if i+1 < len(q) && q[i+1] == '*' {
				end := strings.Index(q[i+2:], "*/")
				if end < 0 {
					return "", istore.ErrNotSelect
				}
				i += end + 3
			}
		case ';':
			return "", istore.ErrNotSelect
		default:
			// A WITH may front a write; say so before SQLite does.
			if isWordStart(q, i) {
				j := i
				for j < len(q) && isWordByte(q[j]) {
					j++
				}
				switch strings.ToUpper(q[i:j]) {
				case "INSERT", "UPDATE", "DELETE", "ATTACH", "DETACH", "PRAGMA", "VACUUM", "CREATE", "DROP", "ALTER":
					return "", istore.ErrNotSelect
				}
				i = j - 1
			}
		}
	}
	return q, nil
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isWordStart(q string, i int) bool {
	return isWordByte(q[i]) && (i == 0 || !isWordByte(q[i-1]))
}
