package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Table describes a table of work items with their tracker fields in a JSON
// column. Its owner gives this description and runs IndexSQL in its own
// migration; it needs the columns source, id, area, status and updated (epoch ms).
type Table struct {
	Name string
	// JSON is the column holding the item (extra.*).
	JSON string
	// Title and Labels are SQL for the item's title and its labels as a JSON
	// array. The table's updated column is its last change, in epoch ms.
	Title, Labels string
	// Key lists the columns that find one item, in index order; Ref is the
	// one of them to read back (a ref, with or without its source).
	Key []string
	Ref string
	// Refs says how to find items by source and ref.
	Refs func(source string, refs string) (where string, args []any)
}

func (t Table) extra(key string) string { return "json_extract(" + t.JSON + ", '$.extra." + key + "')" }

func (t Table) marked() string {
	return "(" + t.extra("issue_type") + " = '" + EpicType + "' OR " + t.extra("due_at") + " IS NOT NULL OR " + t.extra("defer_until") + " IS NOT NULL)"
}

// groupCols are the live items' counting columns, in the order of IndexSQL's
// rollup index, so they are counted from the index, in order.
func (t Table) groupCols() []string {
	return []string{"source", t.extra("parent"), t.extra("issue_type"), "status", t.extra("status"), t.extra("priority")}
}

// live and done split the items for the two indexes that count them. Most
// items are done, and only their count per parent and latest change matter.
func (t Table) live() string { return "status != '" + string(orchapi.WorkDone) + "'" }

func (t Table) done() string {
	return "status = '" + string(orchapi.WorkDone) + "' AND " + t.extra("issue_type") + " IS NOT '" + EpicType + "'"
}

func (t Table) doneCols() []string { return []string{"source", t.extra("parent")} }

// markedCols are the marked rows' columns, for itemSelect and the index that
// answers it.
func (t Table) markedCols() []string {
	return []string{"source", "id", t.extra("parent"), t.Title, t.Labels, t.extra("issue_type"), "status", t.extra("status"), t.extra("priority"),
		t.extra("due_at"), t.extra("defer_until"), "updated", "area"}
}

func (t Table) hasParent() string { return t.extra("parent") + " IS NOT NULL" }

// IndexSQL is the table's migration: the indexes that answer a rollup's reads
// without opening any item's JSON: live items, done items, the few marked ones,
// and those with a parent. The
// expressions must stay byte-identical to the reads' (a test holds the plans).
// updated runs newest first because SQLite re-reads the row (its whole JSON)
// each time max() finds a new maximum: this way once per group, not per item.
// An index entry goes with its row, so redaction reaches a title here too.
func (t Table) IndexSQL() string {
	return "CREATE INDEX " + t.Name + "_rollup ON " + t.Name + " (" + strings.Join(t.groupCols(), ", ") + ", updated DESC, area) WHERE " + t.live() + ";\n" +
		"CREATE INDEX " + t.Name + "_rollup_done ON " + t.Name + " (" + strings.Join(t.doneCols(), ", ") + ", updated DESC, area) WHERE " + t.done() + ";\n" +
		"CREATE INDEX " + t.Name + "_rollup_marked ON " + t.Name + " (" + strings.Join(t.markedCols(), ", ") + ") WHERE " + t.marked() + ";\n" +
		"CREATE INDEX " + t.Name + "_rollup_parent ON " + t.Name + " (" + strings.Join(t.Key, ", ") + ", " + t.extra("parent") + ", area) WHERE " + t.hasParent() + ";"
}

// ChildIndexSQL is the table's second migration: items by parent, for
// Owned's walk down from an epic. Its expressions match childQuery's.
func (t Table) ChildIndexSQL() string {
	return "CREATE INDEX " + t.Name + "_rollup_children ON " + t.Name + " (source, " + t.extra("parent") + ") WHERE " + t.hasParent() + ";"
}

// SQL reads a Table within one reader's sight.
type SQL struct {
	DB    *sql.DB
	Table Table
	// Where and Args narrow every read to what the reader may see; Sources
	// narrows it to the trackers the reader may see (nil: every source).
	Where   string
	Args    []any
	Sources []string
}

var _ Source = SQL{}

// scope is the reads' WHERE clause and arguments.
func (s SQL) scope() (string, []any) {
	where, args := s.Where, append([]any(nil), s.Args...)
	if where == "" {
		where = "1=1"
	}
	if s.Sources != nil {
		where = "source IN (" + strings.TrimSuffix(strings.Repeat("?,", len(s.Sources)), ",") + ") AND " + where
		sources := make([]any, len(s.Sources))
		for i, x := range s.Sources {
			sources[i] = x
		}
		args = append(sources, args...)
	}
	return where, args
}

func (s SQL) liveQuery() (string, []any) {
	where, args := s.scope()
	cols := strings.Join(s.Table.groupCols(), ", ")
	return `SELECT ` + cols + `, count(*), max(updated) FROM ` + s.Table.Name + ` WHERE ` + where + ` AND ` + s.Table.live() + ` GROUP BY ` + cols, args
}

func (s SQL) doneQuery() (string, []any) {
	where, args := s.scope()
	cols := strings.Join(s.Table.doneCols(), ", ")
	return `SELECT ` + cols + `, count(*), max(updated) FROM ` + s.Table.Name + ` WHERE ` + where + ` AND ` + s.Table.done() + ` GROUP BY ` + cols, args
}

func (s SQL) markedQuery() (string, []any) {
	where, args := s.scope()
	return `SELECT ` + s.itemSelect() + ` FROM ` + s.Table.Name + ` WHERE ` + where + ` AND ` + s.Table.marked(), args
}

func (s SQL) parentsQuery(source, refs string) (string, []any) {
	where, args := s.Table.Refs(source, refs)
	scope := s.Where
	if scope == "" {
		scope = "1=1"
	}
	return `SELECT ` + s.Table.Ref + `, ` + s.Table.extra("parent") + ` FROM ` + s.Table.Name + ` INDEXED BY ` + s.Table.Name + `_rollup_parent WHERE ` + where +
		` AND ` + s.Table.hasParent() + ` AND ` + scope, append(args, s.Args...)
}

// Plans is each read's query plan, for the conformance suite: a drifted
// expression would still answer right, by reading every item's JSON.
func (s SQL) Plans(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	queries := map[string]func() (string, []any){"live": s.liveQuery, "done": s.doneQuery, "marked": s.markedQuery,
		"parents": func() (string, []any) { return s.parentsQuery("bd", `["a"]`) },
		"children": func() (string, []any) { // Owned's walk; on ChildIndexSQL's index where the owner runs it
			q, args, _ := s.childQuery("bd", []string{"a"})
			_, scope := s.scope()
			return q, append(args, scope...)
		}}
	for name, q := range queries {
		query, args := q()
		rows, err := s.DB.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			return nil, err
		}
		var steps []string
		for rows.Next() {
			var a, b, c int
			var step string
			if err := rows.Scan(&a, &b, &c, &step); err != nil {
				rows.Close()
				return nil, err
			}
			steps = append(steps, step)
		}
		rows.Close()
		out[name] = strings.Join(steps, "; ")
	}
	return out, nil
}

// Groups implements Source. A done epic is left out (Build counts no epics).
func (s SQL) Groups(ctx context.Context) ([]Group, error) {
	if s.Sources != nil && len(s.Sources) == 0 {
		return nil, nil
	}
	query, args := s.liveQuery()
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		var parent, typ, tracker, priority, changed any
		if err := rows.Scan(&g.Source, &parent, &typ, &g.Status, &tracker, &priority, &g.Count, &changed); err != nil {
			return nil, err
		}
		g.Parent, g.Type, g.TrackerStatus, g.Priority, g.Changed = text(parent), text(typ), text(tracker), number(priority), when(changed)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	query, args = s.doneQuery()
	rows, err = s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		g := Group{Status: string(orchapi.WorkDone), Priority: NoPriority}
		var parent, changed any
		if err := rows.Scan(&g.Source, &parent, &g.Count, &changed); err != nil {
			return nil, err
		}
		g.Parent, g.Changed = text(parent), when(changed)
		out = append(out, g)
	}
	return out, rows.Err()
}

// itemSelect reads an Item, in scanItems' order (area is the index's, unread).
func (s SQL) itemSelect() string {
	cols := s.Table.markedCols()
	return strings.Join(cols[:len(cols)-1], ", ")
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var parent, title, labels, typ, tracker, priority, due, deferred, changed any
		if err := rows.Scan(&it.Source, &it.ID, &parent, &title, &labels, &typ, &it.Status, &tracker, &priority, &due, &deferred, &changed); err != nil {
			return nil, err
		}
		it.Ref = strings.TrimPrefix(it.ID, it.Source+":")
		it.Parent, it.Title, it.Type, it.TrackerStatus = text(parent), text(title), text(typ), text(tracker)
		it.Priority, it.DueAt, it.DeferUntil, it.Changed = number(priority), when(due), when(deferred), when(changed)
		if l := text(labels); l != "" {
			_ = json.Unmarshal([]byte(l), &it.Labels)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Marked implements Source.
func (s SQL) Marked(ctx context.Context) ([]Item, error) {
	if s.Sources != nil && len(s.Sources) == 0 {
		return nil, nil
	}
	query, args := s.markedQuery()
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

// Parents implements Source.
func (s SQL) Parents(ctx context.Context, source string, refs []string) (map[string]string, error) {
	js, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	query, args := s.parentsQuery(source, string(js))
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, len(refs))
	for rows.Next() {
		var ref string
		var parent any
		if err := rows.Scan(&ref, &parent); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(ref, source+":")] = text(parent)
	}
	return out, rows.Err()
}

// Items implements Source.
func (s SQL) Items(ctx context.Context, ids []string) ([]Item, error) {
	js, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	where, args := s.scope()
	rows, err := s.DB.QueryContext(ctx, `SELECT `+s.itemSelect()+` FROM `+s.Table.Name+` WHERE `+where+` AND id IN (SELECT value FROM json_each(?))`, append(args, string(js))...)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

// MaxOwned bounds Owned: an epic owning more is refused, never cut short.
const MaxOwned = 20000

// Owned is the refs (in source) of the items the open epic source:ref owns
// by Build's rule: those whose nearest open epic up the parent chain is it,
// through tasks and closed epics, with the open epics just below it (what is
// under one of those is that one's). Nothing when source:ref is not an open
// epic in sight; an item out of sight ends its chain, as in Build. One read
// per level, by the ChildIndexSQL index.
func (s SQL) Owned(ctx context.Context, source, ref string) ([]string, error) {
	if s.Sources != nil && !slices.Contains(s.Sources, source) {
		return nil, nil
	}
	where, args := s.scope()
	js, err := json.Marshal([]string{ref})
	if err != nil {
		return nil, err
	}
	rw, rargs := s.Table.Refs(source, string(js))
	var one int
	err = s.DB.QueryRowContext(ctx, `SELECT 1 FROM `+s.Table.Name+` WHERE `+rw+` AND `+where+` AND `+s.Table.extra("issue_type")+` = '`+EpicType+
		`' AND status != '`+string(orchapi.WorkDone)+`'`, append(rargs, args...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{ref: true}
	out := []string{}
	for level := []string{ref}; len(level) > 0; {
		query, qargs, err := s.childQuery(source, level)
		if err != nil {
			return nil, err
		}
		rows, err := s.DB.QueryContext(ctx, query, append(qargs, args...)...)
		if err != nil {
			return nil, err
		}
		var next []string
		for rows.Next() {
			var r, st string
			var typ any
			if err := rows.Scan(&r, &typ, &st); err != nil {
				rows.Close()
				return nil, err
			}
			r = strings.TrimPrefix(r, source+":")
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
			if text(typ) != EpicType || st == string(orchapi.WorkDone) {
				next = append(next, r) // not an open epic: what is under it is still ours
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(out) > MaxOwned {
			return nil, api.Errorf(api.CodeBadRequest, "epic %s:%s owns over %d items", source, ref, MaxOwned).
				WithHint("page one of the epics under it instead")
		}
		level = next
	}
	return out, nil
}

// childQuery reads the items in source whose parent is one of parents, in
// sight (the scope's arguments follow the ones it returns).
func (s SQL) childQuery(source string, parents []string) (string, []any, error) {
	js, err := json.Marshal(parents)
	if err != nil {
		return "", nil, err
	}
	where, _ := s.scope()
	return `SELECT ` + s.Table.Ref + `, ` + s.Table.extra("issue_type") + `, status FROM ` + s.Table.Name + ` WHERE source = ? AND ` +
		s.Table.extra("parent") + ` IN (SELECT value FROM json_each(?)) AND ` + s.Table.hasParent() + ` AND ` + where, []any{source, string(js)}, nil
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

// number is an integer a tracker reported, or NoPriority.
func number(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return NoPriority
}

// when is a stored time: epoch ms, or RFC 3339 text as a tracker's extra has it
// (zero when neither).
func when(v any) time.Time {
	switch x := v.(type) {
	case int64:
		if x > 0 {
			return time.UnixMilli(x).UTC()
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC()
		}
	case []byte:
		return when(string(x))
	}
	return time.Time{}
}
