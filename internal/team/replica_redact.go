package team

// A redaction at the project's authority, as this machine's copy takes it
// : the copy's old text
// is scrubbed from the outbox and by this machine's own modules, then the
// copy is compacted.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/kit/core"
)

// noteRedaction handles a comment copy the authority now answers as
// redacted (identifiers design, stage 2b part 2). The authority sends no
// text, so the copy's old body is the text: it is scrubbed from this item's
// outbox rows (a comment written from this machine keeps its body and the
// authority's answer there), queued for this machine's other modules (their
// session outputs, events: compactIfMarked hands it to them after this
// transaction), and the replica database is marked for
// compaction (compactIfMarked), since SQLite keeps overwritten text in free
// pages and the write-ahead log.
func noteRedaction(ctx context.Context, tx *sql.Tx, item, oldBody, newBody string) error {
	if newBody != core.Redacted || oldBody == "" || oldBody == core.Redacted {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, body, result, error FROM outbox WHERE item = ?`, item)
	if err != nil {
		return err
	}
	type row struct{ id, body, result, errMsg string }
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.body, &r.result, &r.errMsg); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_scrubs (item, text) VALUES (?, ?)`, item, oldBody); err != nil {
		return err
	}
	texts := []string{oldBody}
	for _, r := range rs {
		b, res, em := scrubOutboxJSON(r.body, texts), scrubOutboxJSON(r.result, texts), core.ScrubText(r.errMsg, texts, core.MinUnscoped)
		if b == r.body && res == r.result && em == r.errMsg {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET body = ?, result = ?, error = ? WHERE id = ?`, b, res, em, r.id); err != nil {
			return err
		}
	}
	return setMeta(ctx, tx, metaCompact, "1")
}

// scrubOutboxJSON redacts texts in an outbox body or result: exact text
// fields of the JSON (any length, so a comment that says "done" leaves a
// status write alone), then, for a text long enough to be safe, anywhere.
func scrubOutboxJSON(js string, texts []string) string {
	js, _ = core.ScrubJSON(js, texts)
	return core.ScrubText(js, texts, core.MinUnscoped)
}

// drainScrubs has this machine's modules scrub each queued redacted text
// (session holds and scrubs, the event log), then forgets it. A failed scrub
// stays queued for the next round.
func (rp *replica) drainScrubs(ctx context.Context) error {
	if rp.scrub == nil {
		return rp.exec(ctx, `DELETE FROM pending_scrubs`) // no modules here to scrub
	}
	if !rp.drainMu.TryLock() {
		return nil // another caller drains now (the mark keeps what it misses)
	}
	defer rp.drainMu.Unlock()
	rows, err := rp.db.QueryContext(ctx, `SELECT id, item, text FROM pending_scrubs ORDER BY id`)
	if err != nil {
		return err
	}
	type row struct {
		id         int64
		item, text string
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.item, &r.text); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	for _, r := range rs {
		subjects := []string{r.item}
		if _, id, ok := strings.Cut(r.item, ":"); ok {
			subjects = append(subjects, id) // the issue id itself, as the authority files copies
		}
		if err := rp.scrub(ctx, core.Scrub{Project: rp.project, Subjects: subjects, Texts: []string{r.text}}); err != nil {
			return fmt.Errorf("scrub a redacted text here: %w", err)
		}
		if err := rp.exec(ctx, `DELETE FROM pending_scrubs WHERE id = ?`, r.id); err != nil {
			return err
		}
	}
	return nil
}

// errScrubPending: this machine's scrub of a redacted text did not go
// through yet (a live session still recording, say); the sync loop retries
// it every round.
var errScrubPending = errors.New("a redacted text is still to be scrubbed here")

// compactAfterApply is compactIfMarked after an apply that already
// committed: a scrub still pending is not the apply's failure (its caller
// would fetch again for nothing; the sync loop retries the scrub).
func (rp *replica) compactAfterApply(ctx context.Context) error {
	if err := rp.compactIfMarked(ctx); err != nil && !errors.Is(err, errScrubPending) {
		return err
	}
	return nil
}

// metaCompact marks the replica database for compaction after a redaction.
const metaCompact = "compact"

// compactIfMarked rewrites the replica database without free pages and
// truncates its write-ahead log when a redaction marked it.
func (rp *replica) compactIfMarked(ctx context.Context) error {
	var v string
	if rp.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaCompact).Scan(&v) != nil {
		return nil
	}
	// First this machine's own copies (session outputs, events) of what was
	// redacted; the mark stays until they and the compaction are done.
	if err := rp.drainScrubs(ctx); err != nil {
		return fmt.Errorf("%w: %w", errScrubPending, err)
	}
	// The mark is cleared only after both steps succeed (the sync loop runs
	// this after every round, so a busy checkpoint is retried), and before
	// a second checkpoint folds the clearing write itself. VACUUM cannot run
	// in a transaction, so this holds the writers' queue itself.
	rp.writing.Lock()
	defer rp.writing.Unlock()
	if _, err := rp.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	// A redaction applied since the drain queued a text: the mark stays, so
	// the next round scrubs it (and compacts again).
	var queued int
	if err := rp.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_scrubs`).Scan(&queued); err != nil || queued > 0 {
		return err
	}
	if _, err := rp.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, metaCompact); err != nil {
		return err
	}
	var busy, frames, done int
	if err := rp.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil || busy != 0 || frames != done {
		_, _ = rp.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, '1') ON CONFLICT (key) DO UPDATE SET value = '1'`, metaCompact)
		if err != nil {
			return err
		}
		return fmt.Errorf("replica checkpoint busy (%d of %d frames folded)", done, frames)
	}
	return nil
}
