package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

const pushBurst = 3
const pushWindow = time.Minute
const pushDrainDeadline = 3 * time.Second
const pushDrainEndpoints = 8

type pushQueued struct {
	id, hash, project, typ string
	attempts               int
}
type pushItem struct {
	key     string
	message pushMessage
	rows    []pushQueued
	event   api.Event
}

func (s *Service) lockPushPerson(ctx context.Context, id string) (func(), error) {
	s.pushPersonMu.Lock()
	if s.pushPersonLocks == nil {
		s.pushPersonLocks = make(map[string]chan struct{})
	}
	lock := s.pushPersonLocks[id]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		s.pushPersonLocks[id] = lock
	}
	s.pushPersonMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	}
}

func (s *Service) pushStillOpen(ctx context.Context, sub *authz.Subject, e api.Event) (bool, error) {
	switch e.Type {
	case orchapi.EventMailSent:
		var m orchapi.Mail
		if json.Unmarshal(e.Data, &m) != nil {
			return false, nil
		}
		if mail, ok := orchcore.Mail(s.d).(orchcore.PushMail); ok {
			open, err := mail.PushOpen(ctx, sub, m.ID)
			return open, err
		}
		return false, nil
	case orchapi.EventSessionStarted, orchapi.EventSessionRegistered, orchapi.EventSessionStateChanged, orchapi.EventSessionEnded:
		var before orchapi.Session
		if json.Unmarshal(e.Data, &before) != nil || orchcore.Sessions(s.d) == nil {
			return false, nil
		}
		current, ok, err := orchcore.LookupSession(ctx, orchcore.Sessions(s.d), before.Address)
		if err != nil {
			return false, err
		}
		if !ok || !s.pushOwnsSession(sub, current) {
			return false, nil
		}
		if !s.pushCanReadSession(sub, current) {
			return false, nil
		}
		if e.Type == orchapi.EventSessionEnded {
			return current.State == orchapi.StateEnded && current.Outcome != nil && current.Outcome.Kind != orchapi.OutcomeOK && current.Outcome.Kind != orchapi.OutcomeKilled, nil
		}
		return current.State == orchapi.StateNeedsHuman, nil
	}
	return false, nil
}

// drainPush never holds the event follower, limits each round's endpoints and
// wall time, and gives each endpoint its own cancellable request.
func (s *Service) drainPush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pushDrainDeadline)
	defer cancel()
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT endpoint_hash FROM web_push_queue WHERE next_at<=? GROUP BY endpoint_hash ORDER BY MIN(next_at),MIN(event_id) LIMIT ?`, time.Now().Unix(), pushDrainEndpoints)
	if err != nil {
		return err
	}
	var endpoints []string
	for rows.Next() {
		var h string
		if err = rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		endpoints = append(endpoints, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(endpoints))
	for _, h := range endpoints {
		s.pushWorkMu.Lock()
		if s.pushActive == nil {
			s.pushActive = make(map[string]bool)
		}
		if s.pushActive[h] || len(s.pushActive) >= pushDrainEndpoints {
			s.pushWorkMu.Unlock()
			continue
		}
		s.pushActive[h] = true
		s.pushWorkers.Add(1)
		s.pushWorkMu.Unlock()
		wg.Add(1)
		go func(hash string) {
			defer wg.Done()
			defer s.pushWorkers.Done()
			defer func() { s.pushWorkMu.Lock(); delete(s.pushActive, hash); s.pushWorkMu.Unlock() }()
			if e := s.deliverEndpoint(ctx, hash); e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, context.DeadlineExceeded) {
				errs <- e
			}
		}(h)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) deletePushRows(ctx context.Context, rows []pushQueued) error {
	for _, q := range rows {
		if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM web_push_queue WHERE event_id=? AND endpoint_hash=?`, q.id, q.hash); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) deferPushRows(ctx context.Context, rows []pushQueued, when int64, attempt bool) error {
	for _, q := range rows {
		inc := 0
		if attempt {
			inc = 1
		}
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE web_push_queue SET attempts=attempts+?,next_at=? WHERE event_id=? AND endpoint_hash=?`, inc, when, q.id, q.hash); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) deliverEndpoint(ctx context.Context, hash string) error {
	records, err := s.pushRecords(ctx, "")
	if err != nil {
		return err
	}
	var p *pushRecord
	for i := range records {
		if records[i].hash == hash {
			p = &records[i]
			break
		}
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT event_id,endpoint_hash,project,type,attempts FROM web_push_queue WHERE endpoint_hash=? AND next_at<=? ORDER BY event_id LIMIT 5000`, hash, time.Now().Unix())
	if err != nil {
		return err
	}
	var queue []pushQueued
	for rows.Next() {
		var q pushQueued
		if err = rows.Scan(&q.id, &q.hash, &q.project, &q.typ, &q.attempts); err != nil {
			rows.Close()
			return err
		}
		queue = append(queue, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if p == nil {
		return s.deletePushRows(ctx, queue)
	}
	release, err := s.lockPushPerson(ctx, p.identity)
	if err != nil {
		return err
	}
	defer release()
	sub := s.pushSubject(ctx, *p)
	if sub == nil {
		return s.deletePushRows(ctx, queue)
	}
	items := map[string]*pushItem{}
	var order []string
	var discard []pushQueued
	for _, q := range queue {
		if q.id <= p.cursor || ids.Time(q.id).IsZero() || time.Since(ids.Time(q.id)) > time.Hour || q.attempts >= 8 {
			discard = append(discard, q)
			continue
		}
		ev, err := s.pushVisible(ctx, sub, api.Event{ID: q.id, Project: q.project, Type: q.typ})
		if err != nil {
			return err
		}
		if ev == nil {
			discard = append(discard, q)
			continue
		}
		open, err := s.pushStillOpen(ctx, sub, *ev)
		if err != nil {
			return err
		}
		if !open {
			discard = append(discard, q)
			continue
		}
		msg, ok := s.pushEventMessage(sub, *ev)
		if !ok {
			discard = append(discard, q)
			continue
		}
		item := "mail:" + ev.Subject
		if strings.HasPrefix(ev.Type, "dev.zm.session.") {
			var ss orchapi.Session
			if json.Unmarshal(ev.Data, &ss) != nil {
				discard = append(discard, q)
				continue
			}
			item = "session:" + ss.Address
			if ev.Type == orchapi.EventSessionEnded {
				item = "failure:" + ss.Address
			}
		}
		var sent int
		if err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_sent WHERE endpoint_hash=? AND item=? AND sent_at>?`, hash, item, time.Now().Add(-time.Hour).Unix()).Scan(&sent); err != nil {
			return err
		}
		if sent > 0 {
			discard = append(discard, q)
			continue
		}
		if items[item] == nil {
			items[item] = &pushItem{key: item, message: msg, event: *ev}
			order = append(order, item)
		}
		items[item].rows = append(items[item].rows, q)
	}
	if err = s.deletePushRows(ctx, discard); err != nil {
		return err
	}
	if len(order) == 0 {
		return nil
	}
	var since int64
	var used int
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT since,sent FROM web_push_rate WHERE endpoint_hash=?`, p.identity).Scan(&since, &used)
	if err != nil {
		since = time.Now().Unix()
		used = 0
	}
	now := time.Now().Unix()
	if now-since >= int64(pushWindow.Seconds()) {
		since = now
		used = 0
	}
	allSeen := true
	for _, k := range order {
		for _, q := range items[k].rows {
			var n int
			if err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_rate_event WHERE identity=? AND event_id=? AND sent_at>=?`, p.identity, q.id, since).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				allSeen = false
			}
		}
	}
	if used >= pushBurst && !allSeen {
		var all []pushQueued
		for _, k := range order {
			all = append(all, items[k].rows...)
		}
		return s.deferPushRows(ctx, all, since+int64(pushWindow.Seconds()), false)
	}
	groups := [][]*pushItem{}
	if len(order) > pushBurst-used {
		group := []*pushItem{}
		for _, k := range order {
			group = append(group, items[k])
		}
		groups = append(groups, group)
	} else {
		for _, k := range order {
			groups = append(groups, []*pushItem{items[k]})
		}
	}
	for _, group := range groups {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := group[0].message
		var qs []pushQueued
		for _, item := range group {
			qs = append(qs, item.rows...)
		}
		if len(group) > 1 {
			message = pushMessage{Title: fmt.Sprintf("%d things need you", len(group)), Body: "Open zm to review", URL: decisionsPage}
		}
		err = s.sendPush(ctx, *p, message)
		if err != nil {
			if ctx.Err() != nil {
				// The request deadline is itself a transient failure. Persist
				// its retry schedule with a short independent DB deadline.
				maxAttempts := 0
				for _, q := range qs {
					maxAttempts = max(maxAttempts, q.attempts)
				}
				delay := time.Duration(1<<min(maxAttempts, 7)) * 15 * time.Second
				mark, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				markErr := s.deferPushRows(mark, qs, time.Now().Add(delay).Unix(), true)
				stop()
				if markErr != nil {
					return markErr
				}
				return ctx.Err()
			}
			var status *pushHTTPError
			if errors.Is(err, errPushExpired) || errors.As(err, &status) && status.Status >= 400 && status.Status < 500 && status.Status != http.StatusTooManyRequests {
				return s.deletePushRows(ctx, qs)
			}
			maxAttempts := 0
			for _, q := range qs {
				maxAttempts = max(maxAttempts, q.attempts)
			}
			delay := time.Duration(1<<min(maxAttempts, 7)) * 15 * time.Second
			if status != nil {
				delay = max(delay, status.RetryAfter)
			}
			return s.deferPushRows(ctx, qs, time.Now().Add(delay).Unix(), true)
		}
		for _, item := range group {
			if _, err = s.d.Store.Registry().ExecContext(ctx, `INSERT OR REPLACE INTO web_push_sent(endpoint_hash,item,sent_at) VALUES(?,?,?)`, hash, item.key, time.Now().Unix()); err != nil {
				return err
			}
		}
		if err = s.deletePushRows(ctx, qs); err != nil {
			return err
		}
		if !allSeen {
			used++
			if len(group) > 1 {
				used = pushBurst
			}
		}
		for _, q := range qs {
			if _, err = s.d.Store.Registry().ExecContext(ctx, `INSERT OR REPLACE INTO web_push_rate_event(identity,event_id,sent_at) VALUES(?,?,?)`, p.identity, q.id, time.Now().Unix()); err != nil {
				return err
			}
		}
		if _, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO web_push_rate(endpoint_hash,since,sent) VALUES(?,?,?) ON CONFLICT(endpoint_hash) DO UPDATE SET since=excluded.since,sent=excluded.sent`, p.identity, since, used); err != nil {
			return err
		}
	}
	return nil
}
