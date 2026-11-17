package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
)

// ---- comments ----

func (w *writer) comment() error {
	req := w.m.Comment
	if req == nil || strings.TrimSpace(req.Text) == "" {
		return bad("comment text cannot be empty")
	}
	if err := checkText("comment", req.Text); err != nil {
		return err
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	if req.ID != "" {
		if !ids.Valid(req.ID) {
			return bad("a comment id is a ULID")
		}
		var issue string
		err := w.tx.QueryRowContext(w.ctx, `SELECT issue_id FROM comments WHERE id = ?`, req.ID).Scan(&issue)
		switch {
		case err == nil && issue == cur.ID:
			c, err := w.loadComment(req.ID)
			if err == nil && (c.Text != req.Text || c.AuthorAddress != w.m.Actor.Address) {
				return conflict("comment delivery id has different payload or actor")
			}
			w.cmt = &c
			return err // a replay: already stored
		case err == nil:
			return conflict("comment %s belongs to another issue", req.ID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	return w.insertComment(cur, req.ID, req.Text)
}

func (w *writer) insertComment(cur *row, id, text string) error {
	if id == "" {
		id = ids.New()
	}
	at := millis(w.m.At)
	if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO comments (id, issue_id, author, author_addr, text, created_at, identity) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, cur.ID, w.m.Actor.Name, w.m.Actor.Address, text, at, w.m.Actor.Identity); err != nil {
		return err
	}
	c := orchapi.IssueComment{ID: id, IssueID: cur.ID, Author: w.m.Actor.Name, AuthorAddress: w.m.Actor.Address, Text: text, CreatedAt: fromMillis(at)}
	w.cmt = &c
	return w.record(cur.ID, "commented", nil, id, cur.Area, cur.Area)
}

func (w *writer) loadComment(id string) (orchapi.IssueComment, error) {
	return scanComment(w.tx.QueryRowContext(w.ctx, `SELECT `+commentCols+` FROM comments WHERE id = ?`, id))
}

// ownComment loads comment cid of issue cur for an edit or a delete, and
// whether the actor wrote it (same identity; same address for comments
// made before identities were recorded).
func (w *writer) ownComment(cur *row, cid string) (orchapi.IssueComment, bool, error) {
	var issue, identity string
	err := w.tx.QueryRowContext(w.ctx, `SELECT issue_id, identity FROM comments WHERE id = ?`, cid).Scan(&issue, &identity)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && issue != cur.ID) {
		return orchapi.IssueComment{}, false, api.NotFound("comment", cid)
	}
	if err != nil {
		return orchapi.IssueComment{}, false, err
	}
	c, err := w.loadComment(cid)
	if err != nil {
		return c, false, err
	}
	a := w.m.Actor
	mine := (identity != "" && identity == a.Identity) || (identity == "" && c.AuthorAddress != "" && c.AuthorAddress == a.Address)
	return c, mine, nil
}

// commentEdit is the author's edit of a comment's text: history keeps the
// old text; Base, when set, refuses an edit over a change it did not see.
func (w *writer) commentEdit() error {
	req := w.m.CommentEdit
	if req == nil || req.ID == "" {
		return bad("an edit names a comment")
	}
	if strings.TrimSpace(req.Text) == "" {
		return bad("comment text cannot be empty")
	}
	if err := checkText("comment", req.Text); err != nil {
		return err
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	c, mine, err := w.ownComment(cur, req.ID)
	if err != nil {
		return err
	}
	if c.Deleted {
		return api.Errorf(api.CodeConflict, "comment %s was deleted", req.ID)
	}
	if !mine {
		return api.Errorf(api.CodeForbidden, "only a comment's author edits it")
	}
	if req.Base != "" {
		var last string
		if err := w.tx.QueryRowContext(w.ctx, `SELECT id FROM history WHERE issue_id = ? AND comment = ? ORDER BY id DESC LIMIT 1`, cur.ID, req.ID).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if last != req.Base {
			return api.Errorf(api.CodeConflict, "comment %s changed since %s", req.ID, req.Base)
		}
	}
	if req.Text == c.Text {
		w.cmt = &c
		return nil
	}
	at := millis(w.m.At)
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE comments SET text = ?, edited_at = ? WHERE id = ?`, req.Text, at, req.ID); err != nil {
		return err
	}
	d := fieldDiff{}
	d.set("text", c.Text, req.Text)
	if c, err = w.loadComment(req.ID); err != nil {
		return err
	}
	w.cmt = &c
	return w.record(cur.ID, "comment_edited", d, req.ID, cur.Area, cur.Area)
}

// commentRedact overwrites comment cid's text and every history copy of it
// (its edits' old and new texts) with [redacted], and tombstones it: for a
// leaked secret, personal data or abuse. Moderators only; the act is
// recorded (moderator), its texts are not.
func (w *writer) commentRedact() error {
	cid := w.m.CommentID
	if cid == "" {
		return bad("a redaction names a comment")
	}
	cur, err := w.redactRow(cid)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	c, _, err := w.ownComment(cur, cid)
	if err != nil {
		return err
	}
	if w.m.Moderate == nil || !w.m.Moderate(cur.Area) {
		return api.Errorf(api.CodeForbidden, "only an admin of the issue's area, or the machine owner, redacts a comment")
	}
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && t != redactedText && !seen[t] {
			seen[t] = true
			w.redacted = append(w.redacted, t)
		}
	}
	add(c.Text)
	rows, err := w.tx.QueryContext(w.ctx, `SELECT id, fields FROM history WHERE issue_id = ? AND comment = ? AND fields <> ''`, cur.ID, cid)
	if err != nil {
		return err
	}
	type hrow struct{ id, fields string }
	var hs []hrow
	for rows.Next() {
		var h hrow
		if err := rows.Scan(&h.id, &h.fields); err != nil {
			rows.Close()
			return err
		}
		hs = append(hs, h)
	}
	rows.Close()
	for _, h := range hs {
		var f map[string][]*string
		if json.Unmarshal([]byte(h.fields), &f) != nil {
			continue
		}
		pair, ok := f["text"]
		if !ok {
			continue
		}
		for _, v := range pair {
			if v != nil {
				add(*v)
			}
		}
		r := redactedText
		f["text"] = []*string{&r, &r}
		b, _ := json.Marshal(f)
		if _, err := w.tx.ExecContext(w.ctx, `UPDATE history SET fields = ? WHERE id = ?`, string(b), h.id); err != nil {
			return err
		}
	}
	reason, err := w.redactReopenReason(c)
	if err != nil {
		return err
	}
	add(reason)
	if len(w.redacted) == 0 && c.Deleted {
		w.cmt = &c
		return nil // already redacted
	}
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE comments SET text = ?, deleted_at = coalesce(deleted_at, ?), deleted_by = CASE WHEN deleted_by = '' THEN ? ELSE deleted_by END WHERE id = ?`,
		redactedText, millis(w.m.At), w.m.Actor.Address, cid); err != nil {
		return err
	}
	if c, err = w.loadComment(cid); err != nil {
		return err
	}
	w.cmt = &c
	d := fieldDiff{}
	d.set("moderator", false, true)
	return w.record(cur.ID, "comment_redacted", d, cid, cur.Area, cur.Area)
}

// redactedText replaces a redacted comment's text (core.Redacted).
const redactedText = "[redacted]"

// commentDelete leaves a tombstone: by the author, or a moderator (who is
// then recorded as such, so readers can be shown "a moderator").
func (w *writer) commentDelete() error {
	cid := w.m.CommentID
	if cid == "" {
		return bad("a delete names a comment")
	}
	cur, err := w.loadRow(w.m.ID)
	if err != nil {
		return err
	}
	if err := w.m.Authorize(cur.Area, cur.Area); err != nil {
		return err
	}
	w.id = cur.ID
	c, mine, err := w.ownComment(cur, cid)
	if err != nil {
		return err
	}
	mod := !mine && w.m.Moderate != nil && w.m.Moderate(cur.Area)
	if !mine && !mod {
		return api.Errorf(api.CodeForbidden, "only a comment's author, or an admin of its area, deletes it")
	}
	if c.Deleted {
		w.cmt = &c
		return nil
	}
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE comments SET deleted_at = ?, deleted_by = ? WHERE id = ?`, millis(w.m.At), w.m.Actor.Address, cid); err != nil {
		return err
	}
	if c, err = w.loadComment(cid); err != nil {
		return err
	}
	w.cmt = &c
	d := fieldDiff{}
	if mod {
		d.set("moderator", false, true)
	}
	return w.record(cur.ID, "comment_deleted", d, cid, cur.Area, cur.Area)
}
