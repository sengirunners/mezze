package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/potluck/kit/ids"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// Import implements istore.ProjectStore: one batch, one transaction,
// upserting by id so an import that is run again converges.
func (p *project) Import(ctx context.Context, b istore.ImportBatch) error {
	return zmstore.Tx(ctx, p.w, func(tx *sql.Tx) error {
		for i := range b.Issues {
			it := &b.Issues[i]
			var meta, extra string
			if len(it.Metadata) > 0 && string(it.Metadata) != "{}" && string(it.Metadata) != "null" {
				meta = string(it.Metadata)
			}
			if len(it.Extra) > 0 {
				e, err := json.Marshal(it.Extra)
				if err != nil {
					return err
				}
				extra = string(e)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO issues (id, area, title, description, design, acceptance, notes, spec_id, status,
				priority, issue_type, assignee, owner, estimated_minutes, created_at, created_by, updated_at, started_at, closed_at,
				close_reason, closed_by_session, due_at, defer_until, external_ref, source_system, metadata, pinned, is_template,
				ephemeral, extra)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET area = excluded.area, title = excluded.title, description = excluded.description,
				design = excluded.design, acceptance = excluded.acceptance, notes = excluded.notes, spec_id = excluded.spec_id,
				status = excluded.status, priority = excluded.priority, issue_type = excluded.issue_type, assignee = excluded.assignee,
				owner = excluded.owner, estimated_minutes = excluded.estimated_minutes, created_at = excluded.created_at,
				created_by = excluded.created_by, updated_at = excluded.updated_at, started_at = excluded.started_at,
				closed_at = excluded.closed_at, close_reason = excluded.close_reason, closed_by_session = excluded.closed_by_session,
				due_at = excluded.due_at, defer_until = excluded.defer_until, external_ref = excluded.external_ref,
				source_system = excluded.source_system, metadata = excluded.metadata, pinned = excluded.pinned,
				is_template = excluded.is_template, ephemeral = excluded.ephemeral, extra = excluded.extra`,
				it.ID, it.Area, it.Title, it.Description, it.Design, it.AcceptanceCriteria, it.Notes, it.SpecID, it.Status,
				it.Priority, it.IssueType, it.Assignee, it.Owner, intOrNil(it.EstimatedMinutes), millis(it.CreatedAt), it.CreatedBy,
				millis(it.UpdatedAt), nullMillis(it.StartedAt), nullMillis(it.ClosedAt), it.CloseReason, it.ClosedBySession,
				nullMillis(it.DueAt), nullMillis(it.DeferUntil), it.ExternalRef, it.SourceSystem, meta, b2i(it.Pinned),
				b2i(it.IsTemplate), b2i(it.Ephemeral), extra); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ?`, it.ID); err != nil {
				return err
			}
			for _, l := range it.Labels {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO labels (issue_id, label) VALUES (?, ?)`, it.ID, l); err != nil {
					return err
				}
			}
		}
		for _, d := range b.Deps {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deps (issue_id, depends_on, type, external, created_at, created_by, metadata, thread_id, source_id)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (issue_id, depends_on) DO UPDATE SET type = excluded.type, external = excluded.external,
				created_at = excluded.created_at, created_by = excluded.created_by, metadata = excluded.metadata, thread_id = excluded.thread_id,
				source_id = excluded.source_id`,
				d.IssueID, d.DependsOnID, d.Type, b2i(d.External), millis(d.CreatedAt), d.CreatedBy, d.Metadata, d.ThreadID, d.SourceID); err != nil {
				return err
			}
		}
		for _, c := range b.Comments {
			if _, err := tx.ExecContext(ctx, `INSERT INTO comments (id, issue_id, author, author_addr, text, created_at) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET issue_id = excluded.issue_id, author = excluded.author, text = excluded.text, created_at = excluded.created_at`,
				c.ID, c.IssueID, c.Author, c.AuthorAddress, c.Text, millis(c.CreatedAt)); err != nil {
				return err
			}
		}
		for _, e := range b.History {
			fields := ""
			if e.Old != "" || e.New != "" {
				raw, _ := json.Marshal(map[string]json.RawMessage{"value": mustJSON([]any{nullable(e.Old), nullable(e.New)})})
				fields = string(raw)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history (id, issue_id, kind, actor, fields, comment, created_at, source_id)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ids.New(), e.IssueID, e.Kind, e.Actor, fields, e.Comment, millis(e.At), e.SourceID); err != nil {
				return err
			}
		}
		for k, v := range b.KV {
			if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		for parent, n := range b.Counters {
			if _, err := tx.ExecContext(ctx, `INSERT INTO child_counters (parent, last_child) VALUES (?, ?)
				ON CONFLICT (parent) DO UPDATE SET last_child = max(last_child, excluded.last_child)`, parent, n); err != nil {
				return err
			}
		}
		return nil
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// FinishImport derives what beads materialised: parents from parent-child
// edges, blocked state (with blocked parents), and the area of imported
// history rows.
func (p *project) FinishImport(ctx context.Context, areaOf istore.AreaFunc) error {
	if err := zmstore.Tx(ctx, p.w, func(tx *sql.Tx) error {
		for _, q := range []string{
			`UPDATE issues SET parent = coalesce((SELECT min(depends_on) FROM deps WHERE issue_id = issues.id AND type = 'parent-child'), '')`,
			`UPDATE issues SET is_blocked = CASE
				WHEN status IN ('closed', 'pinned') OR pinned = 1 THEN 0
				WHEN EXISTS (SELECT 1 FROM deps d JOIN issues t ON t.id = d.depends_on WHERE d.issue_id = issues.id
					AND d.type IN ('blocks', 'conditional-blocks') AND t.status NOT IN ('closed', 'pinned') AND t.pinned = 0) THEN 1
				ELSE 0 END`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		for i := 0; i < 64; i++ {
			r, err := tx.ExecContext(ctx, `UPDATE issues SET is_blocked = 1 WHERE is_blocked = 0 AND status NOT IN ('closed', 'pinned')
				AND pinned = 0 AND parent != '' AND EXISTS (SELECT 1 FROM issues pp WHERE pp.id = issues.parent AND pp.is_blocked = 1)`)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				break
			}
		}
		// Imported issues are numbered in creation order, after any that have one.
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET number = n.rn + (SELECT coalesce(max(number), 0) FROM issues)
			FROM (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn FROM issues WHERE number IS NULL) AS n
			WHERE n.id = issues.id`); err != nil {
			return err
		}
		// The version of an imported issue is its last history row.
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET version = coalesce((SELECT max(id) FROM history h WHERE h.issue_id = issues.id), version)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE history SET area_before = coalesce((SELECT area FROM issues i WHERE i.id = history.issue_id), ''),
			area_after = coalesce((SELECT area FROM issues i WHERE i.id = history.issue_id), '') WHERE source_id IS NOT NULL`)
		return err
	}); err != nil {
		return err
	}
	_, _ = p.w.ExecContext(ctx, `INSERT INTO issues_fts (issues_fts) VALUES ('optimize')`)
	// Planner statistics for the store as imported: the file was opened
	// empty, so without them a write's lookups can plan as if it still were.
	_, _ = p.w.ExecContext(ctx, `ANALYZE`)
	_, _ = p.ck.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

// Rearea implements istore.ProjectStore.
func (p *project) Rearea(ctx context.Context, areaOf istore.AreaFunc) ([]istore.Moved, error) {
	var moved []istore.Moved
	err := zmstore.Tx(ctx, p.w, func(tx *sql.Tx) error {
		w := &writer{ctx: ctx, tx: tx, p: p, m: istore.Mutation{At: time.Now(), Actor: istore.Actor{Name: "zm"}, AreaOf: areaOf}}
		type cand struct{ id, area, home string }
		var cs []cand
		rows, err := tx.QueryContext(ctx, `SELECT id, area, home_area FROM issues`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.area, &c.home); err != nil {
				rows.Close()
				return err
			}
			cs = append(cs, c)
		}
		rows.Close()
		labels := map[string][]string{}
		rows, err = tx.QueryContext(ctx, `SELECT issue_id, label FROM labels ORDER BY issue_id, label`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, l string
			if err := rows.Scan(&id, &l); err != nil {
				rows.Close()
				return err
			}
			labels[id] = append(labels[id], l)
		}
		rows.Close()
		for _, c := range cs {
			after := w.areaOf(c.id, c.home, labels[c.id])
			if after == c.area {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET area = ? WHERE id = ?`, after, c.id); err != nil {
				return err
			}
			w.changes = nil
			if err := w.record(c.id, "area_changed", fieldDiff{"area": {c.area, after}}, "", c.area, after); err != nil {
				return err
			}
			it, err := w.load(c.id)
			if err != nil {
				return err
			}
			moved = append(moved, istore.Moved{Issue: it, Change: w.changes[0]})
		}
		return nil
	})
	return moved, err
}
