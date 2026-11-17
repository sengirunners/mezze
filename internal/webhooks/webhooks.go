// Package webhooks delivers the daemon's events to HTTP endpoints (SPEC
// "Webhooks and GitHub linking"). Each hook follows the event bus as its
// creator: a bus subscription with the creator's current scope (rebuilt on
// every grant or token change), so the bus's own read check, audience and
// redaction decide what it receives. Events are copied into a durable queue
// in registry.db in the same transaction that advances the hook's cursor,
// and a small pool of workers delivers them, signed, in event order per
// hook, with retries and backoff. A hook that keeps failing is disabled.
//
// Nothing here can block the bus: a follower that falls behind is cut off
// by the bus and catches up from the event log at its cursor.
package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

const schema = `
CREATE TABLE webhooks (
	id              TEXT PRIMARY KEY,
	url             TEXT NOT NULL,
	project         TEXT NOT NULL DEFAULT '',
	area            TEXT NOT NULL DEFAULT '',
	events          TEXT NOT NULL DEFAULT '[]',
	secret          TEXT NOT NULL,
	allow_private   INTEGER NOT NULL DEFAULT 0,
	creator_id      TEXT NOT NULL DEFAULT '',
	creator_address TEXT NOT NULL,
	creator_all     INTEGER NOT NULL DEFAULT 0,
	token           TEXT NOT NULL DEFAULT '',
	cap_project     TEXT NOT NULL DEFAULT '',
	cap_area        TEXT NOT NULL DEFAULT '',
	cap_role        TEXT NOT NULL DEFAULT '',
	note            TEXT NOT NULL DEFAULT '',
	created         INTEGER NOT NULL,
	cursor          TEXT NOT NULL,
	state           TEXT NOT NULL DEFAULT 'active',
	disabled_reason TEXT NOT NULL DEFAULT '',
	fail_since      INTEGER,
	fail_count      INTEGER NOT NULL DEFAULT 0,
	last_ok         INTEGER,
	last_error      TEXT NOT NULL DEFAULT ''
);
CREATE TABLE webhook_queue (
	id         TEXT PRIMARY KEY,
	hook       TEXT NOT NULL,
	event_id   TEXT NOT NULL,
	event_type TEXT NOT NULL,
	project    TEXT NOT NULL,
	area       TEXT NOT NULL DEFAULT '',
	body       BLOB NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    INTEGER NOT NULL,
	created    INTEGER NOT NULL,
	UNIQUE (hook, event_id)
);
CREATE INDEX webhook_queue_head ON webhook_queue (hook, event_id);
CREATE TABLE webhook_log (
	hook        TEXT NOT NULL,
	delivery    TEXT NOT NULL,
	event_id    TEXT NOT NULL DEFAULT '',
	event_type  TEXT NOT NULL,
	attempt     INTEGER NOT NULL,
	time        INTEGER NOT NULL,
	status      INTEGER NOT NULL DEFAULT 0,
	outcome     TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX webhook_log_hook ON webhook_log (hook, time);`

// Migrations are this module's tables; its New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{{Kind: store.KindRegistry, Name: "webhooks/0001_init", Script: schema}}

// Defaults (Service fields; tests lower them).
var (
	DefaultBackoff = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute,
		15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}
)

const (
	DefaultMaxAttempts        = 12
	DefaultDisableAfter       = 24 * time.Hour
	DefaultDisableMinFailures = 10
	DefaultTimeout            = 10 * time.Second
	DefaultQueueCap           = 5000
	DefaultLogKeep            = 500
	DefaultWorkers            = 8
	maxHooks                  = 200
)

// Service is the webhooks module.
type Service struct {
	d *core.Deps

	// Tunables, read when used. Backoff[i] is the pause after the (i+1)th
	// failed attempt (the last value repeats), with up to 20% jitter.
	Backoff            []time.Duration
	MaxAttempts        int           // attempts per delivery before it fails
	DisableAfter       time.Duration // a failing streak this long ...
	DisableMinFailures int           // ... with at least this many failed attempts disables the hook
	Timeout            time.Duration // per attempt
	QueueCap           int           // queued deliveries per hook; the oldest are dropped beyond it
	LogKeep            int           // log rows kept per hook
	Workers            int
	Tick               time.Duration // how often due retries are looked for
	Refresh            time.Duration // how often followers rebuild their scope (token expiry)
	Now                func() time.Time

	mu        sync.Mutex
	followers map[string]*follower
	inflight  map[string]bool
	wake      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	started   bool
	clients   map[bool]*http.Client
	logged    map[string]int // log rows written per hook since start (prune pacing)
}

type follower struct {
	cancel  context.CancelFunc
	refresh chan struct{}
	done    chan struct{}
}

// New returns the webhooks module.
func New(d *core.Deps) *Service {
	s := newService(d)
	d.AddScrubber(s.scrub)
	d.AddBackupScrubber(func(ctx context.Context, tx *sql.Tx) error {
		// A hook's signing key never goes off-site: a restored home
		// re-keys its hooks (zm webhook rm, then add); until then they do not send.
		_, err := tx.ExecContext(ctx, `UPDATE webhooks SET secret = ''`)
		return err
	})
	return s
}

// scrub overwrites a redaction's texts (at least core.MinUnscoped long) in
// queued, undelivered payloads of the project. Delivered payloads cannot be
// recalled; the redaction event tells receivers to drop their copy.
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	db := s.d.Store.Registry()
	for _, t := range sc.Texts {
		if len(t) < core.MinUnscoped {
			continue
		}
		enc, _ := json.Marshal(t)
		rows, err := db.QueryContext(ctx, `SELECT id, body FROM webhook_queue WHERE project = ? AND (instr(CAST(body AS TEXT), ?) > 0 OR instr(CAST(body AS TEXT), ?) > 0)`,
			sc.Project, t, string(enc[1:len(enc)-1]))
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
			if _, err := db.ExecContext(ctx, `UPDATE webhook_queue SET body = ? WHERE id = ?`, []byte(nb), x.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func newService(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	return &Service{d: d, Backoff: DefaultBackoff, MaxAttempts: DefaultMaxAttempts, DisableAfter: DefaultDisableAfter,
		DisableMinFailures: DefaultDisableMinFailures, Timeout: DefaultTimeout, QueueCap: DefaultQueueCap,
		LogKeep: DefaultLogKeep, Workers: DefaultWorkers, Tick: time.Second, Refresh: 5 * time.Minute, Now: time.Now,
		followers: map[string]*follower{}, inflight: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func (s *Service) now() time.Time { return s.Now().UTC() }

func (s *Service) db() *sql.DB { return s.d.Store.Registry() }

// Start resumes every active hook's follower and the delivery workers.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()
	hooks, err := s.list(s.ctx)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if h.State == orchapi.WebhookActive {
			s.startFollower(h.ID)
		}
	}
	s.wg.Add(2)
	go s.watchAuthz(s.ctx)
	go s.deliverLoop(s.ctx)
	return nil
}

// Close stops the followers and workers. An attempt cut off by shutdown is
// not recorded: its delivery stays queued and is sent again after restart
// (at least once; receivers dedupe on Zm-Webhook-Delivery).
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

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ---- hook rows ----

type hook struct {
	ID, URL, Project, Area string
	Events                 []string
	Secret                 string
	AllowPrivate           bool
	CreatorID, CreatorAddr string
	CreatorAll             bool
	Token                  string
	CapProject, CapArea    string
	CapRole                string
	Note                   string
	Created                int64
	Cursor                 string
	State, Reason          string
	FailSince              sql.NullInt64
	FailCount              int
	LastOK                 sql.NullInt64
	LastError              string
}

const hookCols = `id, url, project, area, events, secret, allow_private, creator_id, creator_address, creator_all, token,
	cap_project, cap_area, cap_role, note, created, cursor, state, disabled_reason, fail_since, fail_count, last_ok, last_error`

func scanHook(sc interface{ Scan(...any) error }) (hook, error) {
	var h hook
	var ev string
	var ap, all int
	err := sc.Scan(&h.ID, &h.URL, &h.Project, &h.Area, &ev, &h.Secret, &ap, &h.CreatorID, &h.CreatorAddr, &all, &h.Token,
		&h.CapProject, &h.CapArea, &h.CapRole, &h.Note, &h.Created, &h.Cursor, &h.State, &h.Reason, &h.FailSince, &h.FailCount,
		&h.LastOK, &h.LastError)
	_ = json.Unmarshal([]byte(ev), &h.Events)
	h.AllowPrivate, h.CreatorAll = ap != 0, all != 0
	return h, err
}

func (s *Service) load(ctx context.Context, id string) (hook, error) {
	h, err := scanHook(s.db().QueryRowContext(ctx, `SELECT `+hookCols+` FROM webhooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return h, api.NotFound("webhook", id)
	}
	return h, err
}

func (s *Service) list(ctx context.Context) ([]hook, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT `+hookCols+` FROM webhooks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hook
	for rows.Next() {
		h, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// subject is who the hook follows the bus as: its creator's current scope
// (the identity's grants now, narrowed by the token it was created with),
// or nil when the creator can no longer act (the token was revoked or
// expired, the identity is gone).
func (s *Service) subject(ctx context.Context, h hook) *authz.Subject {
	if h.CreatorAll {
		return &authz.Subject{Identity: s.d.Projects.LocalIdentity(), Address: h.CreatorAddr, Via: authz.ViaSystem, Scope: authz.All()}
	}
	var cp *authz.Cap
	if h.Token != "" {
		var rev, exp sql.NullInt64
		err := s.db().QueryRowContext(ctx, `SELECT revoked, expires FROM tokens WHERE id = ?`, h.Token).Scan(&rev, &exp)
		if err != nil || rev.Valid || (exp.Valid && exp.Int64 <= store.Millis(s.now())) {
			return nil
		}
		cp = &authz.Cap{Project: h.CapProject, Area: h.CapArea, Role: authz.Relation(h.CapRole)}
	}
	tuples, err := s.d.Projects.Tuples(ctx, h.CreatorID)
	if err != nil || len(tuples) == 0 {
		return nil
	}
	return &authz.Subject{Address: h.CreatorAddr, Via: authz.ViaSystem, Scope: authz.FromTuples(tuples, cp)}
}

// visibleTo reports whether h's creator may still read an event in project/area.
func (s *Service) visibleTo(ctx context.Context, h hook, project, area string) bool {
	sub := s.subject(ctx, h)
	if sub == nil {
		return false
	}
	if project == "" {
		project = api.ProjectNone
	}
	return sub.Can(authz.Read, authz.Object{Project: project, Area: area})
}

// view renders h for an admin (sub; nil = the daemon, for its own event).
func (s *Service) view(ctx context.Context, sub *authz.Subject, h hook) orchapi.Webhook {
	if sub == nil {
		sub = s.d.System()
	}
	w := orchapi.Webhook{ID: h.ID, URL: h.URL, Area: h.Area, Events: h.Events, AllowPrivate: h.AllowPrivate,
		CreatedBy: s.d.RedactFor(ctx, sub, h.CreatorAddr), Note: h.Note, State: h.State, Disabled: h.Reason, Created: store.FromMillis(h.Created),
		LastOK: store.NullMillis(h.LastOK), FailingSince: store.NullMillis(h.FailSince), LastError: h.LastError}
	if h.Project != "" {
		w.Project = s.d.Projects.Slug(h.Project)
	}
	_ = s.db().QueryRowContext(ctx, `SELECT count(*) FROM webhook_queue WHERE hook = ?`, h.ID).Scan(&w.Pending)
	return w
}

// NormalizeEvents turns "work.*", "mail.sent", "dev.zm.work." into type
// prefixes. "*" or nothing = every event.
func NormalizeEvents(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, e := range strings.Split(raw, ",") {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if e == "*" {
				return nil, nil
			}
			e = strings.TrimSuffix(e, "*")
			if !strings.HasPrefix(e, api.EventTypePrefix) {
				e = api.EventTypePrefix + e
			}
			for _, c := range e {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_') {
					return nil, api.Errorf(api.CodeBadRequest, "bad event filter %q (want e.g. work.*, mail.sent)", raw)
				}
			}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}
