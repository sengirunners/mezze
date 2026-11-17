// Package connections shows outside systems (CI, infra, a vendor) in zm as
// one typed summary each (SPEC 24). A provider registers an endpoint (HTTPS,
// or a Unix socket for the machine owner) and a token; zm fetches its
// summary when someone looks, checks it against the contract
// (zm.connection.show/1, Validate), caches the last good one with its
// freshness stamp, and marks it stale or refuses it when it is old or
// malformed. There is no provider-specific code here: a third party plugs
// in with a zm project, a token and an endpoint.
//
// Show only. Act (guarded tools) and ask (zm cards) come later, behind the
// same registration.
package connections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

const schema = `
CREATE TABLE connections (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL,
	project         TEXT NOT NULL,
	area            TEXT NOT NULL DEFAULT '',
	endpoint        TEXT NOT NULL,
	allow_private   INTEGER NOT NULL DEFAULT 0,
	token           TEXT NOT NULL DEFAULT '',
	token_ref       TEXT NOT NULL DEFAULT '',
	note            TEXT NOT NULL DEFAULT '',
	creator_address TEXT NOT NULL,
	created         INTEGER NOT NULL,
	last_body       BLOB,
	last_fetch      INTEGER,
	last_check      INTEGER,
	last_error      TEXT NOT NULL DEFAULT '',
	UNIQUE (project, name)
);`

// Migrations are this module's tables; its New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{{Kind: store.KindRegistry, Name: "connections/0001_init", Script: schema}}

const (
	// DefaultRefresh: a show within this long of the last fetch answers from the cache.
	DefaultRefresh = 15 * time.Second
	maxConnections = 200
)

// Service is the connections module.
type Service struct {
	d *core.Deps

	Refresh time.Duration
	Now     func() time.Time
	Secrets *secrets.Reader
	// Fetcher overrides the driver (tests); nil = the endpoint's own.
	Fetcher func(Endpoint, bool) Fetcher

	mu       sync.Mutex
	inflight map[string]*fetchCall
}

type fetchCall struct{ done chan struct{} }

// New returns the connections module.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	d.AddBackupScrubber(func(ctx context.Context, tx *sql.Tx) error {
		// A plaintext bearer never goes off-site (token_ref, a reference,
		// stays): a restored connection fetches without one, the provider
		// refuses, and its admin registers it again (rm, then add).
		_, err := tx.ExecContext(ctx, `UPDATE connections SET token = '' WHERE token != ''`)
		return err
	})
	return &Service{d: d, Refresh: DefaultRefresh, Now: time.Now, Secrets: secrets.Default(), inflight: map[string]*fetchCall{}}
}

func (s *Service) db() *sql.DB { return s.d.Store.Registry() }

type conn struct {
	ID, Name, Project, Area, Endpoint string
	AllowPrivate                      bool
	Token, TokenRef, Note, Creator    string
	Created                           int64
	Body                              []byte
	Fetch, Check                      sql.NullInt64
	LastError                         string
}

const cols = `id, name, project, area, endpoint, allow_private, token, token_ref, note, creator_address, created,
	last_body, last_fetch, last_check, last_error`

func scan(sc interface{ Scan(...any) error }) (conn, error) {
	var c conn
	var ap int
	err := sc.Scan(&c.ID, &c.Name, &c.Project, &c.Area, &c.Endpoint, &ap, &c.Token, &c.TokenRef, &c.Note, &c.Creator,
		&c.Created, &c.Body, &c.Fetch, &c.Check, &c.LastError)
	c.AllowPrivate = ap != 0
	return c, err
}

func (s *Service) load(ctx context.Context, id string) (conn, error) {
	c, err := scan(s.db().QueryRowContext(ctx, `SELECT `+cols+` FROM connections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, api.NotFound("connection", id)
	}
	return c, err
}

func (s *Service) list(ctx context.Context) ([]conn, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT `+cols+` FROM connections ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []conn
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (c conn) object() authz.Object { return authz.Object{Project: c.Project, Area: c.Area} }

// Routes registers /v0/connections.
func (s *Service) Routes(r *core.Router) {
	r = r.For("connections")
	r.HandleFunc("GET /v0/connections", core.Authed, "read on the connection's project/area; ?project= like ReadScope", s.h(s.handleList))
	r.HandleFunc("POST /v0/connections", core.Authed, "admin on the named project/area; token_ref, allow_private and unix: endpoints: machine owner on the socket only", s.h(s.handleCreate))
	r.HandleFunc("DELETE /v0/connections/{id}", core.Authed, "admin on the connection, else not_found (read without admin: forbidden)", s.h(s.handleDelete))
	r.HandleFunc("GET /v0/connections/{id}/show", core.Authed, "read on the connection, else not_found; fetches from the provider at most once per refresh", s.h(s.handleShow))
}

func (s *Service) h(fn func(r *http.Request, sub *authz.Subject) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r, core.SubjectOf(r.Context()))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// machineOwner is the OS user on the socket without a token.
func machineOwner(sub *authz.Subject) bool {
	return sub != nil && sub.Via == authz.ViaLocal && !sub.Remote && sub.TokenID == ""
}

// visible loads connection id for sub; hidden is exactly missing.
func (s *Service) visible(ctx context.Context, sub *authz.Subject, id string) (conn, error) {
	key := strings.ToUpper(strings.TrimSpace(id))
	if !ids.Valid(key) {
		return conn{}, api.NotFound("connection", id)
	}
	c, err := s.load(ctx, key)
	if err != nil || !sub.Can(authz.Read, c.object()) {
		if err != nil && !api.IsCode(err, api.CodeNotFound) {
			return conn{}, err
		}
		return conn{}, api.NotFound("connection", id)
	}
	return c, nil
}

func (s *Service) view(ctx context.Context, sub *authz.Subject, c conn) orchapi.Connection {
	v := orchapi.Connection{ID: c.ID, Name: c.Name, Project: s.d.Projects.Slug(c.Project), Area: c.Area, Note: c.Note,
		Created: store.FromMillis(c.Created)}
	if sub.Can(authz.Admin, c.object()) {
		v.Endpoint, v.AllowPrivate, v.CreatedBy = c.Endpoint, c.AllowPrivate, s.d.RedactFor(ctx, sub, c.Creator)
	}
	return v
}

func (s *Service) handleList(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	explicit := r.URL.Query().Get("project")
	if in := core.ResolveInputOf(ctx); explicit == "" && in.ExplicitFrom == project.RuleFlag {
		explicit = in.Explicit
	}
	only, onlyArea := "", ""
	if explicit != "" {
		if _, err := s.d.ReadScope(ctx, explicit); err != nil {
			return nil, err
		}
		ref, area, _ := strings.Cut(explicit, "/")
		p, err := s.d.Projects.Get(sub.Scope, ref)
		if err != nil {
			return nil, err
		}
		only, onlyArea = p.ID, area
	}
	all, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	out := orchapi.ConnectionList{Connections: []orchapi.Connection{}}
	for _, c := range all {
		if !sub.Can(authz.Read, c.object()) || (only != "" && c.Project != only) || (onlyArea != "" && c.Area != onlyArea) {
			continue
		}
		out.Connections = append(out.Connections, s.view(ctx, sub, c))
	}
	return out, nil
}

func (s *Service) handleCreate(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	var req orchapi.NewConnectionRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	owner := machineOwner(sub)
	ref := strings.TrimSpace(req.Project)
	if ref == "" {
		return nil, api.Errorf(api.CodeBadRequest, "a connection needs a project").WithHint("pass --project <slug>[/<area>]")
	}
	slug, area, _ := strings.Cut(ref, "/")
	p, err := s.d.Projects.Get(sub.Scope, slug)
	if err != nil {
		return nil, err
	}
	if area != "" && (!s.d.Projects.HasArea(p.ID, area) || !sub.Can(authz.Read, authz.Object{Project: p.ID, Area: area})) {
		return nil, api.NotFound("area", area)
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: p.ID, Area: area}, "project", ref); err != nil {
		return nil, err
	}
	c := conn{ID: ids.New(), Name: strings.TrimSpace(req.Name), Project: p.ID, Area: area, Note: strings.TrimSpace(req.Note),
		Creator: sub.Address, Created: store.Millis(s.now())}
	if c.Name == "" || len(c.Name) > 60 || strings.ContainsAny(c.Name, "\r\n\t") {
		return nil, api.Errorf(api.CodeBadRequest, "a connection needs a name (one line, 60 bytes max)")
	}
	if len(c.Note) > 200 {
		return nil, api.Errorf(api.CodeBadRequest, "note too long (200 bytes max)")
	}
	if (req.AllowPrivate || req.TokenRef != "" || strings.HasPrefix(strings.TrimSpace(req.Endpoint), "unix:")) && !owner {
		return nil, api.Errorf(api.CodeForbidden, "only the machine owner on its socket may use a socket endpoint, a private address or a token reference")
	}
	e, err := ParseEndpoint(req.Endpoint, req.AllowPrivate)
	if err != nil {
		return nil, err
	}
	c.Endpoint, c.AllowPrivate = e.Raw, req.AllowPrivate
	switch {
	case req.Token != "" && req.TokenRef != "":
		return nil, api.Errorf(api.CodeBadRequest, "pass token or token_ref, not both")
	case len(req.Token) > 4096 || strings.ContainsAny(req.Token, " \r\n\t"):
		return nil, api.Errorf(api.CodeBadRequest, "token must be one word, 4096 bytes max")
	case req.TokenRef != "":
		r, err := secrets.Parse(req.TokenRef)
		if err != nil {
			return nil, secrets.Recode(err, api.CodeBadRequest)
		}
		c.TokenRef = r.String()
	default:
		c.Token = req.Token
	}
	err = store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM connections`).Scan(&n); err != nil {
			return err
		}
		if n >= maxConnections {
			return api.Errorf(api.CodeConflict, "this daemon already has %d connections", n)
		}
		var dup int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM connections WHERE project = ? AND name = ?`, c.Project, c.Name).Scan(&dup); err != nil {
			return err
		}
		if dup > 0 {
			return api.Errorf(api.CodeConflict, "project %s already has a connection named %q", slug, c.Name)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO connections (id, name, project, area, endpoint, allow_private, token, token_ref,
			note, creator_address, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.Name, c.Project, c.Area, c.Endpoint,
			b2i(c.AllowPrivate), c.Token, c.TokenRef, c.Note, c.Creator, c.Created)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.view(ctx, sub, c), nil
}

func (s *Service) handleDelete(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	c, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if err := sub.Check(authz.Admin, c.object(), "connection", c.ID); err != nil {
		return nil, err
	}
	v := s.view(ctx, sub, c)
	if _, err := s.db().ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, c.ID); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Service) handleShow(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	c, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if c, err = s.refresh(ctx, c); err != nil {
		return nil, err
	}
	out := Judge(c.Body, store.NullMillis(c.Fetch), store.NullMillis(c.Check), c.LastError, s.now())
	out.Connection = s.view(ctx, sub, c)
	if acct := strings.TrimSpace(r.URL.Query().Get("account")); acct != "" && out.Summary != nil {
		out.Summary = forAccount(*out.Summary, acct)
	}
	return out, nil
}

// refresh fetches c's summary when the last attempt is older than Refresh;
// concurrent shows of one connection share one fetch.
func (s *Service) refresh(ctx context.Context, c conn) (conn, error) {
	now := s.now()
	if c.Check.Valid && now.Sub(store.FromMillis(c.Check.Int64)) < s.Refresh {
		return c, nil
	}
	s.mu.Lock()
	call, waiting := s.inflight[c.ID]
	if !waiting {
		call = &fetchCall{done: make(chan struct{})}
		s.inflight[c.ID] = call
	}
	s.mu.Unlock()
	if !waiting {
		s.fetchOnce(c)
		s.mu.Lock()
		delete(s.inflight, c.ID)
		s.mu.Unlock()
		close(call.done)
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return c, ctx.Err()
	}
	return s.load(ctx, c.ID)
}

// fetchOnce fetches and records the outcome. It runs detached from the
// request: a caller that gives up does not cancel what the others wait for.
func (s *Service) fetchOnce(c conn) {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout+2*time.Second)
	defer cancel()
	now := s.now()
	body, err := s.fetch(ctx, c)
	if err == nil {
		_, err = Validate(body, now)
	}
	if err != nil {
		_, _ = s.db().ExecContext(ctx, `UPDATE connections SET last_check = ?, last_error = ? WHERE id = ?`,
			store.Millis(now), clip(err.Error(), 400), c.ID)
		return
	}
	_, _ = s.db().ExecContext(ctx, `UPDATE connections SET last_body = ?, last_fetch = ?, last_check = ?, last_error = '' WHERE id = ?`,
		body, store.Millis(now), store.Millis(now), c.ID)
}

func (s *Service) fetch(ctx context.Context, c conn) ([]byte, error) {
	e, err := ParseEndpoint(c.Endpoint, c.AllowPrivate)
	if err != nil {
		return nil, err
	}
	token := c.Token
	if c.TokenRef != "" {
		ref, err := secrets.Parse(c.TokenRef)
		if err != nil {
			return nil, err
		}
		v, err := s.Secrets.Get(ctx, ref)
		if err != nil {
			return nil, errors.New("zm could not read the connection's token on this machine")
		}
		token = strings.TrimSpace(v.Reveal())
	}
	f := e.Fetcher(c.AllowPrivate)
	if s.Fetcher != nil {
		f = s.Fetcher(e, c.AllowPrivate)
	}
	return f.Fetch(ctx, token)
}

// Judge is what a client is told about the last good body (fetched at
// fetched) and the last attempt (at checked, failing with lastErr).
func Judge(body []byte, fetched, checked *time.Time, lastErr string, now time.Time) orchapi.ConnectionShow {
	out := orchapi.ConnectionShow{CheckedAt: checked, FetchedAt: fetched}
	var sum orchapi.ConnectionSummary
	if len(body) == 0 || json.Unmarshal(body, &sum) != nil {
		out.State = orchapi.ConnectionUnavailable
		out.Reason = "zm has not had an answer from it yet"
		if lastErr != "" {
			out.Reason = "zm could not read it: " + lastErr
		}
		out.FetchedAt = nil
		return out
	}
	age := max(now.Sub(sum.AsOf), 0)
	switch {
	case age > MaxAge:
		out.State = orchapi.ConnectionUnavailable
		out.Reason = "its newest numbers are " + Ago(age) + " old, too old to show"
		if lastErr != "" {
			out.Reason += "; zm could not refresh it: " + lastErr
		}
		return out
	case lastErr != "":
		out.State = orchapi.ConnectionStale
		out.Reason = "zm could not refresh it: " + lastErr + "; showing numbers from " + Ago(age) + " ago"
	case age > StaleAfter(sum):
		out.State = orchapi.ConnectionStale
		out.Reason = "the provider has not updated its numbers for " + Ago(age)
	default:
		out.State = orchapi.ConnectionFresh
	}
	out.Summary = &sum
	return out
}

// forAccount keeps only account's items.
func forAccount(s orchapi.ConnectionSummary, account string) *orchapi.ConnectionSummary {
	h, m, r := []orchapi.ConnectionRow{}, []orchapi.ConnectionMeter{}, []orchapi.ConnectionRunning{}
	for _, x := range s.Headline {
		if x.Account == account {
			h = append(h, x)
		}
	}
	for _, x := range s.Meters {
		if x.Account == account {
			m = append(m, x)
		}
	}
	for _, x := range s.Running {
		if x.Account == account {
			r = append(r, x)
		}
	}
	s.Headline, s.Meters, s.Running = h, m, r
	return &s
}

// Ago is d in plain words: "40 seconds", "3 minutes", "2 hours", "5 days".
func Ago(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d < time.Minute:
		return unit(int(d/time.Second), "second")
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return unit(int(d/time.Hour), "hour")
	}
	return unit(int(d/(24*time.Hour)), "day")
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
