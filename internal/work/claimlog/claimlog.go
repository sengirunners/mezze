// Package claimlog reads who last claimed an item from a project's event log
// (the work.claimed events, whose one mention is the holder), for the items
// a session no longer holds (SPEC 8 "What a session still owes").
package claimlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
)

// Window is how far back the claim history is read.
const Window = 14 * 24 * time.Hour

// LastClaimers returns, for every item (subject) that one of asked claimed
// since the window's start and whose latest claim is still by one of asked,
// that holder. Step 1 finds the items asked claimed in the window (a lock is
// not an item); step 2 reads each one's latest claim, which may be someone
// else's. incomplete: a read failed, or the log no longer reaches back to the
// window's start (pruned), so an older claim may be missing.
func LastClaimers(ctx context.Context, db *sql.DB, team bool, sc authz.Scope, project string, asked map[string]bool, since, started time.Time) (out map[string]string, incomplete bool) {
	out = map[string]string{}
	if started.After(since) { // a session cannot have claimed before it started
		since = started
	}
	addrs := make([]string, 0, len(asked))
	for a := range asked {
		addrs = append(addrs, a)
	}
	js, _ := json.Marshal(addrs)
	where, args := sc.AreaSQL(project, "area", authz.Read)
	subjects, err := subjects(ctx, db, where, args, string(js), since)
	for _, subject := range subjects {
		var raw string
		err2 := db.QueryRowContext(ctx, `SELECT json FROM events WHERE subject = ? AND type = ? AND `+where+` ORDER BY id DESC LIMIT 1`,
			append([]any{subject, orchapi.EventWorkClaimed}, args...)...).Scan(&raw)
		if errors.Is(err2, sql.ErrNoRows) {
			continue
		}
		if err2 == nil {
			var ev struct {
				Data orchapi.Lease `json:"data"`
			}
			if err2 = json.Unmarshal([]byte(raw), &ev); err2 == nil && asked[ev.Data.Holder] {
				out[subject] = ev.Data.Holder
			}
		}
		err = errors.Join(err, err2)
	}
	if err == nil {
		var reaches bool
		if reaches, err = reachesBack(ctx, db, team, since); err == nil && !reaches {
			err = errors.New("claim history pruned")
		}
	}
	return out, err != nil
}

// subjects are the distinct subjects of claims since the window's start that
// name one of the addresses in addrJSON, locks left out.
func subjects(ctx context.Context, db *sql.DB, where string, args []any, addrJSON string, since time.Time) ([]string, error) {
	q := `SELECT DISTINCT e.subject FROM events e, json_each(e.mentions) m WHERE e.type = ? AND e.id >= ? AND e.time >= ? AND e.subject NOT LIKE 'lock:%' ` +
		`AND m.value IN (SELECT value FROM json_each(?)) AND ` + where
	rows, err := db.QueryContext(ctx, q, append([]any{orchapi.EventWorkClaimed, floorID(since), since.UnixMilli(), addrJSON}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			return out, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// reachesBack reports whether the event log still holds everything since the
// window's start: false when pruning removed events and the oldest one left
// is newer than since.
func reachesBack(ctx context.Context, db *sql.DB, team bool, since time.Time) (bool, error) {
	var oldest int64
	if err := db.QueryRowContext(ctx, `SELECT time FROM events ORDER BY id LIMIT 1`).Scan(&oldest); err != nil {
		return true, noRows(err) // an empty log lost nothing
	}
	if oldest <= since.UnixMilli() {
		return true, nil
	}
	if team { // a team log says what it pruned
		var upto string
		err := noRows(db.QueryRowContext(ctx, `SELECT value FROM events_meta WHERE key = 'pruned_upto'`).Scan(&upto))
		return upto == "", err
	}
	// A solo log keeps its newest events.Keep rows.
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM events LIMIT 1 OFFSET ?`, events.Keep-1).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// floorID is the smallest event id minted at t (a ULID with zero
// randomness), so `id >= floorID(t)` seeks the (type, id) index.
func floorID(t time.Time) string {
	var id ulid.ULID
	_ = id.SetTime(ulid.Timestamp(t))
	return id.String()
}

func noRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
