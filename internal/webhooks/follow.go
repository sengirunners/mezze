package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// startFollower starts (or restarts) hook id's follower. No-op before Start.
func (s *Service) startFollower(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	if f := s.followers[id]; f != nil {
		f.cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	f := &follower{cancel: cancel, refresh: make(chan struct{}, 1), done: make(chan struct{})}
	s.followers[id] = f
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(f.done)
		s.follow(ctx, id, f.refresh)
	}()
}

// stopFollower stops hook id's follower and waits for it, so no enqueue
// for the hook can land after it returns.
func (s *Service) stopFollower(id string) {
	s.mu.Lock()
	f := s.followers[id]
	delete(s.followers, id)
	s.mu.Unlock()
	if f != nil {
		f.cancel()
		<-f.done
	}
}

// refreshAll asks every follower to rebuild its subscription with its
// creator's current scope.
func (s *Service) refreshAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.followers {
		select {
		case f.refresh <- struct{}{}:
		default:
		}
	}
}

// watchAuthz refreshes every follower when a grant or token changes (a
// revoked grant or token stops the hook's events at once) and when a
// project's areas change.
func (s *Service) watchAuthz(ctx context.Context) {
	defer s.wg.Done()
	for ctx.Err() == nil {
		ch, cancel := s.d.Events.Subscribe(s.d.System(), events.Filter{Types: []string{"dev.zm.authz.", "dev.zm.project.", "dev.zm.team."}}, 64)
		s.refreshAll() // a cut-off subscription may have missed a change
	loop:
		for {
			select {
			case <-ctx.Done():
				cancel()
				return
			case _, ok := <-ch:
				if !ok {
					break loop
				}
				s.refreshAll()
			}
		}
		cancel()
	}
}

// follow copies the events hook id's creator may read into its queue, from
// its cursor on, until ctx ends or the hook stops being active.
func (s *Service) follow(ctx context.Context, id string, refresh <-chan struct{}) {
	for ctx.Err() == nil {
		h, err := s.load(ctx, id)
		if err != nil || h.State != orchapi.WebhookActive {
			return
		}
		sub := s.subject(ctx, h)
		if sub == nil { // the creator lost its token or every grant: wait for a change
			select {
			case <-ctx.Done():
				return
			case <-refresh:
			case <-time.After(s.Refresh):
			}
			continue
		}
		// Secret names stay off this delivery. NoAuthorityOnly drops the ledger
		// (dev.zm.secret.* and dev.zm.secret_grant.*) for a live event and for
		// catch-up. The bound page in catchUp does not, or a page of them
		// would look like the end of the log and the cursor would sit there.
		f := events.Filter{Types: h.Events, Project: h.Project, NoAuthorityOnly: true}
		ch, cancel := s.d.Events.Subscribe(sub, f, 256)
		cursor, err := s.catchUp(ctx, h, sub, f)
		if err != nil {
			cancel()
			if ctx.Err() == nil {
				s.d.Log.Warn("webhooks: catch up", "hook", id, "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
			continue
		}
		tick := time.NewTimer(s.Refresh)
	live:
		for {
			select {
			case <-ctx.Done():
				break live
			case <-refresh:
				break live
			case <-tick.C:
				break live
			case e, ok := <-ch:
				if !ok { // fell behind: the bus cut us off; resume from the log
					break live
				}
				if e.ID <= cursor {
					continue
				}
				if err := s.enqueue(ctx, h, []api.Event{e}, e.ID); err != nil {
					if ctx.Err() == nil {
						s.d.Log.Warn("webhooks: enqueue", "hook", id, "err", err)
					}
					break live // resume from the stored cursor
				}
				cursor = e.ID
			}
		}
		tick.Stop()
		cancel()
	}
}

// catchUp enqueues every stored event after the hook's cursor that sub may
// read, and returns the new cursor. The bus filters a page after its SQL
// limit (audience, the machine log's read check), so a short page from
// Since(sub) does not mean the log is exhausted: each page is bounded by the
// last id of the same page read unfiltered (Since as the daemon), and the
// cursor moves to that bound, so no stored event is skipped.
func (s *Service) catchUp(ctx context.Context, h hook, sub *authz.Subject, f events.Filter) (string, error) {
	const page = 500
	cursor := h.Cursor
	for {
		logged := f
		logged.NoAuthorityOnly = false
		raw, err := s.d.Events.Since(ctx, s.d.System(), cursor, logged, page)
		if err != nil {
			return cursor, err
		}
		if len(raw) == 0 {
			return cursor, nil
		}
		bound := raw[len(raw)-1].ID
		evs, err := s.d.Events.Since(ctx, sub, cursor, f, page)
		if err != nil {
			return cursor, err
		}
		var mine []api.Event
		for _, e := range evs {
			if e.ID <= bound {
				mine = append(mine, e)
			}
		}
		h.Cursor = cursor
		if err := s.enqueue(ctx, h, mine, bound); err != nil {
			return cursor, err
		}
		cursor = bound
		if len(raw) < page {
			return cursor, nil
		}
	}
}

// enqueue stores evs (already filtered and redacted by the bus for the
// hook's creator) as deliveries and advances the hook's cursor to upto (at
// least past them), in one transaction, so a restart neither loses nor
// repeats an event. The oldest deliveries beyond QueueCap are dropped (and
// logged).
func (s *Service) enqueue(ctx context.Context, h hook, evs []api.Event, upto string) error {
	now := store.Millis(s.now())
	last := ""
	var dropped []droppedRow
	err := store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		// Atomic with the inserts (IMMEDIATE): a hook disabled or deleted
		// meanwhile gets nothing, and events at or before its cursor are
		// already queued.
		var state, cur string
		if err := tx.QueryRowContext(ctx, `SELECT state, cursor FROM webhooks WHERE id = ?`, h.ID).Scan(&state, &cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if state != orchapi.WebhookActive {
			return nil
		}
		for _, e := range evs {
			if e.ID <= cur {
				continue
			}
			last = e.ID
			if h.Area != "" && e.Area != h.Area {
				continue
			}
			body, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO webhook_queue (id, hook, event_id, event_type, project, area, body, next_at, created)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, ids.New(), h.ID, e.ID, e.Type, e.Project, e.Area, body, now, now); err != nil {
				return err
			}
		}
		if upto > last {
			last = upto
		}
		if last <= cur {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET cursor = ? WHERE id = ?`, last, h.ID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM webhook_queue WHERE hook = ?`, h.ID).Scan(&n); err != nil {
			return err
		}
		if cap := max(s.QueueCap, 1); n > cap {
			rows, err := tx.QueryContext(ctx, `SELECT id, event_id, event_type FROM webhook_queue WHERE hook = ? ORDER BY event_id LIMIT ?`, h.ID, n-cap)
			if err != nil {
				return err
			}
			for rows.Next() {
				var d droppedRow
				if err := rows.Scan(&d.id, &d.event, &d.typ); err != nil {
					rows.Close()
					return err
				}
				dropped = append(dropped, d)
			}
			rows.Close()
			for _, d := range dropped {
				if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_queue WHERE id = ?`, d.id); err != nil {
					return err
				}
				if err := insertLog(ctx, tx, h.ID, orchapi.WebhookDelivery{Delivery: d.id, Event: d.event, Type: d.typ,
					Time: s.now(), Outcome: "dropped", Error: "queue full"}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		s.poke()
	}
	return err
}

type droppedRow struct{ id, event, typ string }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertLog(ctx context.Context, db execer, hook string, d orchapi.WebhookDelivery) error {
	_, err := db.ExecContext(ctx, `INSERT INTO webhook_log (hook, delivery, event_id, event_type, attempt, time, status, outcome, error, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, hook, d.Delivery, d.Event, d.Type, d.Attempt, store.Millis(d.Time), d.Status,
		d.Outcome, d.Error, d.DurationMS)
	return err
}
