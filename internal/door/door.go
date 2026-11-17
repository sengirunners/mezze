// Package door is the Mezze app door (SPEC 18.4): a stable public URL per Mezze app
// webhook task at a team project's authority, through the authority's
// tunnel host, whichever linked machine's Mezze app owns the task. Deliveries are
// queued on disk and handed to that machine through orchcore.DoorRelay (the
// team module's door_delivery machine op), in order per machine, retried
// while it is offline. The Mezze app keeps its own signature check and filters: zm
// never holds a task's HMAC secret; the URL's token is the door's
// credential.
package door

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Limits and tunables (SPEC 18.4; tests shorten the times).
const (
	maxBody        = 1 << 20
	maxHeaders     = 64
	maxHeaderBytes = 32 << 10
	maxTasks       = 500 // per machine and project
	maxQueued      = 200 // per task
	keepPerTask    = 100
	tokenPrefix    = "dh_"
	hookPrefix     = "/v0/door/hook/"
)

var (
	queueKeep  = 72 * time.Hour
	rowsKeep   = 7 * 24 * time.Hour
	backoffMin = 10 * time.Second
	backoffMax = 10 * time.Minute
	relayWait  = 40 * time.Second // two heartbeats and the report
	tick       = time.Second
)

// Service is the door module.
type Service struct {
	d   *core.Deps
	Now func() time.Time
	// Relay overrides the team module's orchcore.DoorRelay (tests).
	Relay orchcore.DoorRelay

	wake   chan struct{}
	mu     sync.Mutex
	busy   map[string]bool // project+" "+machine with a delivery in flight
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	pruned time.Time
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// New returns the door module.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, Now: time.Now, wake: make(chan struct{}, 1), busy: map[string]bool{}}
	d.AddScrubber(s.scrub)
	d.AddBackupScrubber(func(ctx context.Context, tx *sql.Tx) error {
		// A task's URL token never goes off-site: a restored home re-mints
		// each (rotate); until then its old URL answers 404.
		_, err := tx.ExecContext(ctx, `UPDATE door_tasks SET token = ''`)
		return err
	})
	return s
}

func (s *Service) relay() (orchcore.DoorRelay, bool) {
	if s.Relay != nil {
		return s.Relay, true
	}
	r, ok := orchcore.Team(s.d).(orchcore.DoorRelay)
	return r, ok
}

func (s *Service) now() int64 { return store.Millis(s.Now()) }

// baseURL is where task URLs start ("" without a public URL).
func (s *Service) baseURL() string {
	c := s.d.Config.File
	b := c.Door.PublicURL
	if b == "" {
		b = c.Tunnel.URL
	}
	return strings.TrimRight(b, "/")
}

func (s *Service) available() bool { return s.d.Config.File.Door.Listen != "" }

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func validToken(t string) bool {
	if len(t) != len(tokenPrefix)+43 || !strings.HasPrefix(t, tokenPrefix) {
		return false
	}
	for _, c := range t[len(tokenPrefix):] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// kick wakes the worker.
func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start opens door.listen (loopback, the hook route only) and runs the
// delivery worker.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	if addr := s.d.Config.File.Door.Listen; addr != "" {
		if err := s.listen(ctx, addr); err != nil {
			cancel()
			return err
		}
	}
	s.ctx, s.cancel = ctx, cancel
	s.wg.Add(1)
	go s.loop(ctx)
	return nil
}

// Close stops the worker and the listener.
func (s *Service) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	return nil
}

// PublicHandler serves only the hook route, for door.listen: what the
// tunnel routes there reaches nothing else of the daemon.
func (s *Service) PublicHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST "+hookPrefix+"{token}", s.hook)
	return m
}

func (s *Service) listen(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("door.listen %q: a loopback address and port (127.0.0.1:PORT), routed to by zm tunnel setup", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.MaxBytesHandler(s.PublicHandler(), maxBody+maxHeaderBytes), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 2 * maxHeaderBytes}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); _ = srv.Serve(ln) }()
	go func() { defer s.wg.Done(); <-ctx.Done(); _ = srv.Close() }()
	return nil
}

// ---- the worker ----

func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		s.expire(ctx)
		s.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// expire drops deliveries queued past queueKeep and, hourly, prunes ended
// rows past each task's last keepPerTask or older than rowsKeep.
func (s *Service) expire(ctx context.Context) {
	db := s.d.Store.Registry()
	now := s.now()
	_, _ = db.ExecContext(ctx, `UPDATE door_deliveries SET state = ?, error = ?, headers = '', body = x'', ended = ? WHERE state = ? AND received < ?`,
		orchapi.DoorDropped, "not delivered within "+queueKeep.String(), now, orchapi.DoorQueued, store.Millis(s.Now().Add(-queueKeep)))
	if s.Now().Sub(s.pruned) < time.Hour {
		return
	}
	s.pruned = s.Now()
	_, _ = db.ExecContext(ctx, `DELETE FROM door_deliveries WHERE state != ? AND received < ?`, orchapi.DoorQueued, store.Millis(s.Now().Add(-rowsKeep)))
	_, _ = db.ExecContext(ctx, `DELETE FROM door_deliveries WHERE state != ? AND id IN (
		SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY project, machine, task ORDER BY id DESC) AS n FROM door_deliveries) WHERE n > ?)`,
		orchapi.DoorQueued, keepPerTask)
}

type queued struct {
	id, project, machine, task, headers string
	body                                []byte
	attempts                            int
	nextAt                              int64
}

// dispatch starts the oldest queued delivery of each machine that has
// none in flight, once it is due (its body is read only then).
func (s *Service) dispatch(ctx context.Context) {
	db := s.d.Store.Registry()
	rows, err := db.QueryContext(ctx, `SELECT id, project, machine, next_at FROM door_deliveries
		WHERE id IN (SELECT MIN(id) FROM door_deliveries WHERE state = ? GROUP BY project, machine)`, orchapi.DoorQueued)
	if err != nil {
		return
	}
	var heads []queued
	now := s.now()
	for rows.Next() {
		var q queued
		if rows.Scan(&q.id, &q.project, &q.machine, &q.nextAt) == nil && q.nextAt <= now {
			heads = append(heads, q)
		}
	}
	rows.Close()
	for _, h := range heads {
		key := h.project + " " + h.machine
		s.mu.Lock()
		if s.busy[key] || s.ctx == nil {
			s.mu.Unlock()
			continue
		}
		var q queued
		if db.QueryRowContext(ctx, `SELECT id, project, machine, task, headers, body, attempts, next_at FROM door_deliveries WHERE id = ? AND state = ?`, h.id, orchapi.DoorQueued).
			Scan(&q.id, &q.project, &q.machine, &q.task, &q.headers, &q.body, &q.attempts, &q.nextAt) != nil {
			s.mu.Unlock()
			continue
		}
		s.busy[key] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.send(ctx, q)
			s.mu.Lock()
			delete(s.busy, key)
			s.mu.Unlock()
			s.kick()
		}()
	}
}

// send hands one delivery to its machine and records the outcome.
func (s *Service) send(ctx context.Context, q queued) {
	db := s.d.Store.Registry()
	var target string
	if db.QueryRowContext(ctx, `SELECT target FROM door_tasks WHERE project = ? AND machine = ? AND task = ?`, q.project, q.machine, q.task).Scan(&target) != nil {
		s.end(ctx, q.id, orchapi.DoorDropped, 0, "the task was removed")
		return
	}
	op := orchapi.DoorDeliveryOp{Task: q.task, Target: target, Body: q.body}
	if q.headers != "" {
		_ = json.Unmarshal([]byte(q.headers), &op.Headers)
	}
	var res orchapi.DoorDeliveryResult
	r, ok := s.relay()
	var err error = api.Errorf(api.CodeUnavailable, "this daemon relays to no machine")
	if ok {
		res, err = r.RelayDoor(ctx, q.project, q.machine, op, relayWait)
	}
	if ctx.Err() != nil {
		return // shutting down: it stays queued as it was
	}
	switch {
	case err == nil && res.Status/100 == 2:
		s.end(ctx, q.id, orchapi.DoorDelivered, res.Status, "")
	case err == nil && res.Status/100 == 4 && res.Status != http.StatusRequestTimeout && res.Status != http.StatusTooManyRequests:
		s.end(ctx, q.id, orchapi.DoorRefused, res.Status, "")
	default:
		msg := ""
		if err != nil {
			msg = err.Error()
			var e *api.Error
			if errors.As(err, &e) {
				msg = e.Message
			}
		} else {
			msg = fmt.Sprintf("the Mezze app answered %d", res.Status)
		}
		wait := backoffMin << min(q.attempts, 16)
		wait = min(wait, backoffMax)
		_, _ = db.ExecContext(ctx, `UPDATE door_deliveries SET attempts = attempts + 1, status = ?, error = ?, next_at = ? WHERE id = ? AND state = ?`,
			res.Status, cut(msg, 300), store.Millis(s.Now().Add(wait)), q.id, orchapi.DoorQueued)
	}
}

// end records a delivery's last word and blanks its copy.
func (s *Service) end(ctx context.Context, id, state string, status int, msg string) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE door_deliveries SET state = ?, status = ?, error = ?, attempts = attempts + ?, headers = '', body = x'', ended = ? WHERE id = ? AND state = ?`,
		state, status, cut(msg, 300), b2i(state != orchapi.DoorDropped), s.now(), id, orchapi.DoorQueued)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cut is s on one line, at most n bytes of valid UTF-8.
func cut(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

// scrub overwrites a redaction's texts (at least core.MinUnscoped long)
// in the project's queued delivery bodies: they are third-party text a
// member may have pasted a secret into. Delivered bodies are gone already.
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	db := s.d.Store.Registry()
	for _, t := range sc.Texts {
		if len(t) < core.MinUnscoped {
			continue
		}
		enc, _ := json.Marshal(t)
		rows, err := db.QueryContext(ctx, `SELECT id, body FROM door_deliveries WHERE project = ? AND state = ? AND (instr(CAST(body AS TEXT), ?) > 0 OR instr(CAST(body AS TEXT), ?) > 0)`,
			sc.Project, orchapi.DoorQueued, t, string(enc[1:len(enc)-1]))
		if err != nil {
			return err
		}
		type q struct {
			id   string
			body []byte
		}
		var qs []q
		for rows.Next() {
			var x q
			if err := rows.Scan(&x.id, &x.body); err != nil {
				rows.Close()
				return err
			}
			qs = append(qs, x)
		}
		rows.Close()
		for _, x := range qs {
			nb := core.ScrubText(string(x.body), sc.Texts, core.MinUnscoped)
			if _, err := db.ExecContext(ctx, `UPDATE door_deliveries SET body = ? WHERE id = ?`, []byte(nb), x.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// newID is a delivery's id.
func newID() string { return ids.New() }
