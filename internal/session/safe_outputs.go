package session

// Safe outputs (SPEC 8 "Safe outputs"): a session started with
// safe_outputs may make at most that many writes of each kind through zm.
// The daemon the session runs on counts them, in front of the router and the
// team proxy, so a write to a joined project counts before it is forwarded.
// The socket's Zm-As is asserted, not authenticated (SPEC 1): this stops an
// over-eager run, not a process that hides who it is.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var safeOutputsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0023_safe_outputs", Script: `CREATE TABLE session_safe_outputs (
 session TEXT NOT NULL, kind TEXT NOT NULL, cap INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (session, kind));`},
}

// insertSafeOutputs records a session's caps: with any, every kind gets a
// row, and a kind left out gets 0.
func insertSafeOutputs(ctx context.Context, db *sql.DB, id8 string, caps map[string]int) error {
	if len(caps) == 0 {
		return nil
	}
	for _, kind := range orchapi.SafeOutputKinds {
		if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO session_safe_outputs(session,kind,cap,used) VALUES(?,?,?,0)`, id8, kind, caps[kind]); err != nil {
			return err
		}
	}
	return nil
}

// safeOutputKind is the kind of write r is, or "".
func safeOutputKind(r *http.Request) string {
	p := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return ""
	case p == "/v0/issues" || strings.HasPrefix(p, "/v0/issues/"):
		// sql reads, setup configures the tracker, claim takes a lease.
		if p == "/v0/issues/sql" || p == "/v0/issues/setup" || strings.HasSuffix(p, "/claim") {
			return ""
		}
		return "bead-write"
	case p == "/v0/work":
		return "bead-write"
	case strings.HasPrefix(p, "/v0/work/") && r.Method == http.MethodPost:
		for _, verb := range []string{"/comment", "/status", "/link"} {
			if strings.HasSuffix(p, verb) {
				return "bead-write"
			}
		}
	case p == "/v0/asks" && r.Method == http.MethodPost:
		return "zm-ask"
	case p == "/v0/mail" && r.Method == http.MethodPost,
		strings.HasPrefix(p, "/v0/mail/") && strings.HasSuffix(p, "/reply") && r.Method == http.MethodPost:
		return "mail-send"
	}
	return ""
}

// startsSession is a request that starts a session or a task.
func startsSession(r *http.Request) bool {
	p := strings.TrimSuffix(r.URL.Path, "/")
	return r.Method == http.MethodPost && (p == "/v0/sessions" || p == "/v0/tasks")
}

// safeOutputCaps are a session's caps, nil when it has none.
func (s *Service) safeOutputCaps(ctx context.Context, id8 string) map[string]int {
	db := s.d.Store.Registry()
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT kind, cap FROM session_safe_outputs WHERE session=?`, id8)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var caps map[string]int
	for rows.Next() {
		var kind string
		var n int
		if rows.Scan(&kind, &n) == nil {
			if caps == nil {
				caps = map[string]int{}
			}
			caps[kind] = n
		}
	}
	return caps
}

// withSafeOutputs echoes the caps a start applied on its answer.
func (s *Service) withSafeOutputs(ctx context.Context, ss orchapi.Session, err error) (orchapi.Session, error) {
	if err == nil {
		ss.SafeOutputs = s.safeOutputCaps(ctx, ss.ID)
	}
	return ss, err
}

// spawnEchoingCaps is spawn for a start on this machine, its answer
// carrying the caps it applied.
func (s *Service) spawnEchoingCaps(ctx context.Context, req orchapi.SpawnRequest) (orchapi.Session, error) {
	ss, err := s.spawn(ctx, req, nil)
	return s.withSafeOutputs(ctx, ss, err)
}

// GateSafeOutputs serves r through serve when it is a capped write by a
// session of this machine, or refuses it when that kind's cap is used up, or
// when a capped session starts a session or task (whose writes would not
// count): true when it answered r. A write zm refuses (4xx) gives its count
// back; a 5xx does not, since a forwarded write may have landed.
func (s *Service) GateSafeOutputs(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
	kind := safeOutputKind(r)
	sub := core.SubjectOf(r.Context())
	if kind == "" && !startsSession(r) || sub == nil || sub.Session == "" {
		return false
	}
	id8, ok := s.id8Of(sub.Session)
	db := s.d.Store.Registry()
	if !ok || db == nil {
		return false
	}
	ctx := r.Context()
	if kind == "" {
		if s.safeOutputCaps(ctx, id8) == nil {
			return false
		}
		api.WriteError(w, api.Errorf(api.CodeForbidden, "safe-outputs: a session started with safe_outputs starts no sessions or tasks"))
		return true
	}
	var limit int
	switch err := db.QueryRowContext(ctx, `SELECT cap FROM session_safe_outputs WHERE session=? AND kind=?`, id8, kind).Scan(&limit); {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "safe-outputs: %v", err))
		return true
	}
	res, err := db.ExecContext(ctx, `UPDATE session_safe_outputs SET used=used+1 WHERE session=? AND kind=? AND used<cap`, id8, kind)
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "safe-outputs: %v", err))
		return true
	}
	if n, _ := res.RowsAffected(); n == 0 {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "safe-outputs: this session may make %d %s writes and has used them", limit, kind).
			WithHint("the session was started with safe_outputs; say what you would have written in your reply instead"))
		return true
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	serve(sw, r)
	if sw.status >= 400 && sw.status < 500 {
		// The write did not happen: give the count back (a background context,
		// since r's may be done).
		if _, err := db.ExecContext(context.WithoutCancel(ctx), `UPDATE session_safe_outputs SET used=used-1 WHERE session=? AND kind=? AND used>0`, id8, kind); err != nil {
			s.d.Log.Warn("safe-outputs refund", "session", id8, "kind", kind, "err", err)
		}
	}
	return true
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
