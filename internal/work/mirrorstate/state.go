// Package mirrorstate holds mirror sync helpers.
package mirrorstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/store"
)

// Hook holds a tracker page before its write in tests; its returned function
// runs after that write completes. The daemon never installs one.
type Hook func(string, []orchapi.WorkItem, bool) func()

var hook atomic.Pointer[Hook]

func SetHook(h Hook) func() { hook.Store(&h); return func() { hook.Store(nil) } }
func Before(project string, items []orchapi.WorkItem, notify bool) func() {
	if h := hook.Load(); h != nil {
		return (*h)(project, items, notify)
	}
	return nil
}

// mirrorFingerprints are what the mirror holds for those of items already
// in it, by id: when they were updated and what a list shows of them, so
// a sync can tell a change from the same state seen again.
func Fingerprints(ctx context.Context, db *sql.DB, source string, items []orchapi.WorkItem) (map[string]string, error) {
	out := map[string]string{}
	for len(items) > 0 {
		n := min(400, len(items))
		args := []any{source}
		for _, it := range items[:n] {
			args = append(args, strings.TrimPrefix(it.ID, source+":"))
		}
		items = items[n:]
		rows, err := db.QueryContext(ctx, `SELECT id, updated, status, owner, title, labels FROM work_mirror WHERE source = ? AND ref IN (`+
			strings.TrimSuffix(strings.Repeat("?, ", len(args)-1), ", ")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, st, owner, title, labels string
			var u int64
			if err := rows.Scan(&id, &u, &st, &owner, &title, &labels); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = rowPrint(u, st, owner, title, labels)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func rowPrint(updated int64, status, owner, title, labels string) string {
	return strings.Join([]string{strconv.FormatInt(updated, 10), status, owner, title, labels}, "\x00")
}

// itemPrint is rowPrint of an item about to be written.
func ItemPrint(it orchapi.WorkItem) string {
	var u int64
	if !it.Updated.IsZero() {
		u = store.Millis(it.Updated)
	}
	labels, _ := json.Marshal(labelsOrEmpty(it.Labels))
	return rowPrint(u, string(it.Status), it.Owner, it.Title, string(labels))
}

func labelsOrEmpty(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}

// DeadlinesChanged lists the items of a tracker's full listing whose due or
// defer time the mirror's row does not hold. A full listing announces nothing,
// and a tracker that starts reporting these fields changes no updated time, so
// without this a replica's copy would never learn them for an item that stays
// quiet. Candidates include both incoming and stored deadlines, so removing
// the last field is a change too. Stored candidates come from the covering
// marked index; only candidates need source/ref lookups, in bounded batches.
func DeadlinesChanged(ctx context.Context, db *sql.DB, source string, items []orchapi.WorkItem) ([]orchapi.WorkItem, error) {
	stored, err := deadlineIDs(ctx, db, source)
	if err != nil {
		return nil, err
	}
	var candidates []orchapi.WorkItem
	for _, it := range items {
		if stored[it.ID] || extraText(it, "due_at") != "" || extraText(it, "defer_until") != "" {
			candidates = append(candidates, it)
		}
	}
	items = candidates
	var out []orchapi.WorkItem
	for len(items) > 0 {
		n := min(400, len(items))
		page := items[:n]
		items = items[n:]
		args := []any{source}
		for _, it := range page {
			args = append(args, strings.TrimPrefix(it.ID, source+":"))
		}
		rows, err := db.QueryContext(ctx, `SELECT id, coalesce(json_extract(item, '$.extra.due_at'), ''), coalesce(json_extract(item, '$.extra.defer_until'), '')
			FROM work_mirror WHERE source = ? AND ref IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(args)-1), ", ")+`)`, args...)
		if err != nil {
			return nil, err
		}
		have := map[string][2]string{}
		for rows.Next() {
			var id, due, deferred string
			if err := rows.Scan(&id, &due, &deferred); err != nil {
				rows.Close()
				return nil, err
			}
			have[id] = [2]string{due, deferred}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, it := range page {
			if h, ok := have[it.ID]; ok && h != [2]string{extraText(it, "due_at"), extraText(it, "defer_until")} {
				out = append(out, it)
			}
		}
	}
	return out, nil
}

func extraText(it orchapi.WorkItem, key string) string {
	s, _ := it.Extra[key].(string)
	return s
}
