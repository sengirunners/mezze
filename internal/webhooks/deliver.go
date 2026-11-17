package webhooks

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// deliverLoop starts an attempt for the head of every active hook's queue
// that is due, at most Workers at once and one per hook (so a hook gets its
// events in order).
func (s *Service) deliverLoop(ctx context.Context) {
	defer s.wg.Done()
	sem := make(chan struct{}, max(s.Workers, 1))
	t := time.NewTicker(max(s.Tick, 10*time.Millisecond))
	defer t.Stop()
	for {
		s.dispatch(ctx, sem)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

type due struct{ id, hook string }

func (s *Service) dispatch(ctx context.Context, sem chan struct{}) {
	rows, err := s.db().QueryContext(ctx, `SELECT q.id, q.hook FROM webhook_queue q JOIN webhooks w ON w.id = q.hook
		WHERE w.state = 'active' AND q.next_at <= ?
		AND q.event_id = (SELECT min(event_id) FROM webhook_queue q2 WHERE q2.hook = q.hook)`, store.Millis(s.now()))
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("webhooks: due", "err", err)
		}
		return
	}
	var ds []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.hook) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	for _, d := range ds {
		s.mu.Lock()
		busy := s.inflight[d.hook]
		if !busy {
			s.inflight[d.hook] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			s.mu.Lock()
			delete(s.inflight, d.hook)
			s.mu.Unlock()
			return
		}
		s.wg.Add(1)
		go func(d due) {
			defer s.wg.Done()
			defer func() {
				<-sem
				s.mu.Lock()
				delete(s.inflight, d.hook)
				s.mu.Unlock()
				s.poke()
			}()
			s.attempt(ctx, d)
		}(d)
	}
}

type queued struct {
	id, event, typ, project, area string
	body                          []byte
	attempts                      int
}

// attempt sends one queued delivery and records the outcome.
func (s *Service) attempt(ctx context.Context, d due) {
	h, err := s.load(ctx, d.hook)
	if err != nil || h.State != orchapi.WebhookActive {
		return
	}
	var q queued
	err = s.db().QueryRowContext(ctx, `SELECT id, event_id, event_type, project, area, body, attempts FROM webhook_queue WHERE id = ?`, d.id).
		Scan(&q.id, &q.event, &q.typ, &q.project, &q.area, &q.body, &q.attempts)
	if err != nil {
		return
	}
	rec := orchapi.WebhookDelivery{Delivery: q.id, Event: q.event, Type: q.typ, Attempt: q.attempts + 1, Time: s.now()}
	// The creator's access is checked again at send time: a grant revoked
	// after the event was queued stops it here.
	if !s.visibleTo(ctx, h, q.project, q.area) {
		rec.Outcome, rec.Error = "skipped", "no longer visible to the hook's creator"
		s.finish(ctx, h, q, rec, true)
		return
	}
	status, errMsg := s.post(ctx, h, q.id, q.typ, q.body)
	if ctx.Err() != nil {
		return // shutdown: not an attempt; it is sent again after restart
	}
	rec.Status, rec.Error, rec.DurationMS = status, errMsg, time.Since(rec.Time).Milliseconds()
	if errMsg == "" {
		rec.Outcome = "delivered"
		s.finish(ctx, h, q, rec, true)
		return
	}
	if status == http.StatusGone {
		rec.Outcome = "failed"
		s.finish(ctx, h, q, rec, true)
		s.disable(ctx, h.ID, "the receiver answered 410 Gone")
		return
	}
	if rec.Attempt >= max(s.MaxAttempts, 1) {
		rec.Outcome = "failed"
		s.finish(ctx, h, q, rec, true)
	} else {
		rec.Outcome = "retry"
		s.finish(ctx, h, q, rec, false)
	}
}

// finish records an attempt: done removes the delivery (delivered, failed
// for good, skipped), else it is retried after the backoff. The hook's
// failing streak is updated, and a streak long and dense enough disables it.
func (s *Service) finish(ctx context.Context, h hook, q queued, rec orchapi.WebhookDelivery, done bool) {
	now := s.now()
	disable := ""
	err := store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM webhooks WHERE id = ?`, h.ID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return nil // deleted while the attempt was in flight
		}
		if done {
			if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_queue WHERE id = ?`, q.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE webhook_queue SET attempts = ?, next_at = ? WHERE id = ?`,
				rec.Attempt, store.Millis(now.Add(s.backoff(rec.Attempt))), q.id); err != nil {
				return err
			}
		}
		if err := insertLog(ctx, tx, h.ID, rec); err != nil {
			return err
		}
		switch rec.Outcome {
		case "delivered":
			_, err := tx.ExecContext(ctx, `UPDATE webhooks SET last_ok = ?, fail_since = NULL, fail_count = 0, last_error = '' WHERE id = ?`,
				store.Millis(now), h.ID)
			return err
		case "retry", "failed":
			var since sql.NullInt64
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT fail_since, fail_count FROM webhooks WHERE id = ?`, h.ID).Scan(&since, &n); err != nil {
				return err
			}
			if !since.Valid {
				since = sql.NullInt64{Int64: store.Millis(now), Valid: true}
			}
			n++
			if now.Sub(store.FromMillis(since.Int64)) >= s.DisableAfter && n >= s.DisableMinFailures {
				disable = fmt.Sprintf("failing since %s (%d failed attempts)", store.FromMillis(since.Int64).Format(time.RFC3339), n)
			}
			_, err := tx.ExecContext(ctx, `UPDATE webhooks SET fail_since = ?, fail_count = ?, last_error = ? WHERE id = ?`,
				since.Int64, n, clip(rec.Error, 300), h.ID)
			return err
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("webhooks: record attempt", "hook", h.ID, "err", err)
		}
		return
	}
	s.prune(ctx, h.ID)
	if disable != "" {
		s.disable(ctx, h.ID, disable)
	}
}

func (s *Service) backoff(attempt int) time.Duration {
	b := s.Backoff
	if len(b) == 0 {
		b = DefaultBackoff
	}
	d := b[min(attempt-1, len(b)-1)]
	if d > 0 {
		d += time.Duration(rand.Int64N(int64(d)/5 + 1)) // up to 20% jitter
	}
	return d
}

// prune keeps about the newest LogKeep log rows of a hook (it trims every
// 32 attempts, not on each one).
func (s *Service) prune(ctx context.Context, hook string) {
	s.mu.Lock()
	if s.logged == nil {
		s.logged = map[string]int{}
	}
	s.logged[hook]++
	due := s.logged[hook]%32 == 1
	s.mu.Unlock()
	if !due {
		return
	}
	keep := max(s.LogKeep, 10)
	_, _ = s.db().ExecContext(ctx, `DELETE FROM webhook_log WHERE hook = ? AND rowid NOT IN
		(SELECT rowid FROM webhook_log WHERE hook = ? ORDER BY time DESC, rowid DESC LIMIT ?)`, hook, hook, keep)
}

// disable turns a hook off (it keeps its queue; enabling skips the events
// published while it was off) and announces it on the machine log.
func (s *Service) disable(ctx context.Context, id, reason string) {
	res, err := s.db().ExecContext(ctx, `UPDATE webhooks SET state = 'disabled', disabled_reason = ? WHERE id = ? AND state = 'active'`, reason, id)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	// Only workers disable (never a follower), so waiting for the follower
	// here cannot deadlock. A PATCH that enabled the hook again meanwhile
	// started a new follower, which this stop may have caught: restart it.
	s.stopFollower(id)
	if h, err := s.load(ctx, id); err == nil && h.State == orchapi.WebhookActive {
		s.startFollower(id)
		return
	}
	if h, err := s.load(ctx, id); err == nil {
		s.d.Log.Warn("webhook disabled", "hook", id, "reason", reason)
		_, _ = s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: orchapi.EventWebhookDisabled, Subject: id, Data: s.view(ctx, nil, h)})
	}
}

func (s *Service) client(allowPrivate bool) *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients == nil {
		s.clients = map[bool]*http.Client{}
	}
	c := s.clients[allowPrivate]
	if c == nil || c.Timeout != s.Timeout {
		c = newClient(s.Timeout, allowPrivate)
		s.clients[allowPrivate] = c
	}
	return c
}

// post sends body to the hook, signed now. It returns the HTTP status (0
// when none) and "" on a 2xx, else what went wrong.
func (s *Service) post(ctx context.Context, h hook, delivery, typ string, body []byte) (int, string) {
	// The URL is checked again at every attempt (a hook stored by an older
	// daemon, or edited by hand, gets no pass).
	u, err := CheckURL(h.URL, h.AllowPrivate)
	if err != nil {
		return 0, err.Error()
	}
	if h.Secret == "" {
		// Restored from an off-site set, which never holds signing keys:
		// an unsigned delivery is never sent.
		return 0, "the hook has no signing secret (restored from a backup): re-add it (zm webhook rm, then zm webhook add)"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, "bad request: " + err.Error()
	}
	ts := s.now().Unix()
	secret := []byte(h.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zm-webhooks/"+api.Version)
	req.Header.Set(orchapi.HeaderWebhookID, h.ID)
	req.Header.Set(orchapi.HeaderWebhookDelivery, delivery)
	req.Header.Set(orchapi.HeaderWebhookEvent, typ)
	req.Header.Set(orchapi.HeaderWebhookTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(orchapi.HeaderWebhookSignature, Sign(secret, ts, body))
	req.Header.Set(orchapi.HeaderStdWebhookID, delivery)
	req.Header.Set(orchapi.HeaderStdWebhookTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(orchapi.HeaderStdWebhookSignature, SignStd(h.Secret, delivery, ts, body))
	req.Header.Set(orchapi.HeaderHubSignature, SignHub(secret, body))
	resp, err := s.client(h.AllowPrivate).Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return 0, ErrBlockedAddress.Error()
		}
		return 0, sanitize(err.Error())
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, ""
	}
	msg := "HTTP " + strconv.Itoa(resp.StatusCode)
	if t := strings.TrimSpace(sanitize(string(snippet))); t != "" {
		msg += ": " + clip(t, 200)
	}
	return resp.StatusCode, msg
}

// sanitize keeps printable text only (a receiver's answer goes into the log).
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, s)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
