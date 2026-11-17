package sqlite

import (
	"database/sql"
	"errors"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// redactReopenReason follows a native comment's creation history to the
// immediately preceding reopen in the same transaction. The reason lives in
// that history row too, even after an older binary redacted the comment.
// Imported or ambiguous histories prove no association and are left alone.
func (w *writer) redactReopenReason(c orchapi.IssueComment) (string, error) {
	var id, reason string
	err := w.tx.QueryRowContext(w.ctx, `WITH anchor AS (
		SELECT * FROM history WHERE issue_id = ? AND kind = 'commented' AND comment = ? AND source_id IS NULL
	)
	SELECT h.id, h.comment FROM anchor a JOIN history h ON h.id = (
		SELECT p.id FROM history p WHERE p.issue_id = a.issue_id AND p.id < a.id ORDER BY p.id DESC LIMIT 1
	)
	WHERE (SELECT count(*) FROM anchor) = 1
		AND a.created_at = ? AND a.actor = ? AND a.actor_addr = ?
		AND h.kind = 'reopened' AND h.source_id IS NULL AND h.created_at = a.created_at
		AND h.actor = a.actor AND h.actor_addr = a.actor_addr
		AND h.area_before = a.area_before AND h.area_after = a.area_after
		AND h.comment != '' AND h.comment != ?`, c.IssueID, c.ID, millis(c.CreatedAt), c.Author, c.AuthorAddress, redactedText).Scan(&id, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := w.exec(`UPDATE history SET comment = ? WHERE id = ?`, redactedText, id); err != nil {
		return "", err
	}
	return reason, nil
}

// redactRow is the issue a redaction writes: its ID, or with none (a restore
// whose registry row names another id the issue had here) the comment's.
func (w *writer) redactRow(cid string) (*row, error) {
	if w.m.ID == "" {
		err := w.tx.QueryRowContext(w.ctx, `SELECT issue_id FROM comments WHERE id = ?`, cid).Scan(&w.m.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, api.NotFound("comment", cid)
		}
		if err != nil {
			return nil, err
		}
	}
	return w.loadRow(w.m.ID)
}
