package team

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// The authority's side of round C (SPEC 17.6): the changelog a replica
// syncs from, the snapshot it starts from, and the ordered write batch its
// outbox drains into, with write records so a replay never runs twice.

var authsyncMigrations = []store.Migration{
	// Write records: a replica's write id -> the answer it got, per
	// identity, kept 30 days (SPEC 17.6).
	{Kind: store.KindProject, Name: "team/0001_writes", Script: `
CREATE TABLE team_writes (
	identity TEXT NOT NULL,
	id       TEXT NOT NULL,
	status   INTEGER NOT NULL,
	body     TEXT NOT NULL,
	seen     TEXT NOT NULL DEFAULT '',
	time     INTEGER NOT NULL,
	PRIMARY KEY (identity, id)
);
CREATE INDEX team_writes_time ON team_writes (time);`},
	{Kind: store.KindProject, Name: "team/0002_status_seen", Script: `ALTER TABLE team_writes ADD COLUMN status_seen TEXT NOT NULL DEFAULT '';`},
}

// Tunables (tests shorten them).
var (
	// SyncWaitMax bounds a long-polling GET /v0/sync.
	SyncWaitMax = 25 * time.Second
	// WriteKeep is how long write records are kept.
	WriteKeep = 30 * 24 * time.Hour
)

// maxBatch bounds one POST /v0/team/writes.
const maxBatch = 200

// teamProject is the team project sub names by ref, or not_found (also for
// a solo project: it has no changelog to sync).
func (s *Service) teamProject(sub *authz.Subject, ref string) (api.Project, error) {
	if strings.TrimSpace(ref) == "" {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "?project= is required")
	}
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return p, err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return api.Project{}, api.NotFound("project", ref)
	}
	return p, nil
}

// ErrRestored is GET /v0/sync's answer to a copy taken under another
// restore generation (config.FileRestored): a resync, so the replica takes
// a snapshot, which drops what the restored home lost.
var ErrRestored = api.Errorf(api.CodeResync, "this home was restored from a backup since the copy was taken").
	WithHint("take a snapshot (GET /v0/sync/snapshot) and sync from its cursor")

// restoreGeneration is this home's restore generation, kept once read. A
// file that cannot be read or holds no generation is an error for every
// sync, snapshot and write batch that names one (503), read again on the
// next call: taking it for "never restored" would keep members' ghosts.
func (s *Service) restoreGeneration() (string, error) {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	if s.genOK {
		return s.gen, nil
	}
	gen, err := config.RestoreGeneration(s.d.Config.Home)
	if err != nil {
		s.d.Log.Error("team: the home's restore generation cannot be read; refusing member syncs", "err", err)
		return "", api.Errorf(api.CodeUnavailable, "this home's restore generation cannot be read").
			WithHint("the owner checks " + config.FileRestored + " in the home (zm backup restore writes it)")
	}
	s.gen, s.genOK = gen, true
	return gen, nil
}

// syncCoalesce is how long a woken long poll waits for more changes before
// answering.
var syncCoalesce = 5 * time.Millisecond

// scopeCheckEvery is how often a waiting GET /v0/sync looks at the caller's
// scope_version.
var scopeCheckEvery = 500 * time.Millisecond

// scopeMoved reports whether a node-bound caller's scope_version changed
// since its request was authenticated.
func (s *Service) scopeMoved(ctx context.Context, sub *authz.Subject) bool {
	if core.TeamSubjectOf(ctx) == nil {
		return false
	}
	v, err := s.d.Projects.ScopeVersion(ctx, sub.Identity.ID)
	return err == nil && v != sub.ScopeVersion
}

// handleSync is GET /v0/sync?project=&since=&limit=&wait_ms=.
func (s *Service) handleSync(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	q := r.URL.Query()
	p, err := s.teamProject(sub, q.Get("project"))
	if err != nil {
		return nil, err
	}
	// A copy taken under another restore generation holds history this
	// home lost in a restore, even where its cursor survived.
	// A replica that sends no gen= is from before generations: unchecked.
	if q.Has("gen") {
		gen, err := s.restoreGeneration()
		if err != nil {
			return nil, err
		}
		if q.Get("gen") != gen {
			return nil, ErrRestored
		}
	}
	since := strings.ToUpper(strings.TrimSpace(q.Get("since")))
	limit, _ := strconv.Atoi(q.Get("limit"))
	waitMS, _ := strconv.Atoi(q.Get("wait_ms"))
	wait := min(time.Duration(waitMS)*time.Millisecond, SyncWaitMax)
	var w interface {
		Wait(context.Context, time.Duration) bool
		Close()
	}
	if wait > 0 {
		ww := s.d.Events.Waiter(sub, p.ID) // before reading: nothing published in between is missed
		defer ww.Close()
		w = ww
	}
	evs, next, more, err := s.d.Events.Changes(ctx, sub, p.ID, since, limit)
	if err == nil && len(evs) == 0 && w != nil {
		// Wait for a change, in short steps: a replica whose scope moved
		// meanwhile (a grant narrowed or revoked) is told at once (409
		// rescope), so it drops its copy and takes a snapshot again.
		// A daemon that stops ends the wait (an empty answer), so shutdown
		// never waits on a replica's long poll.
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if s.rctx != nil {
			defer context.AfterFunc(s.rctx, cancel)()
		}
		deadline := time.Now().Add(wait)
		for left := wait; left > 0; left = time.Until(deadline) {
			step := min(left, scopeCheckEvery)
			if w.Wait(wctx, step) {
				// Woken by a change: let the ones committing with it join
				// the answer (fewer, larger pages for the replica to commit;
				// at most syncCoalesce more latency, against a 1 s budget).
				select {
				case <-time.After(syncCoalesce):
				case <-wctx.Done():
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if wctx.Err() != nil {
				break
			}
			if sub.TokenID != "" && !s.d.Projects.TokenLive(ctx, sub.TokenID) {
				return nil, api.Errorf(api.CodeUnauthenticated, "the token was revoked")
			}
			if s.scopeMoved(ctx, sub) {
				return nil, api.Errorf(api.CodeRescope, "your access to this project changed").
					WithHint("the replica refreshes what it holds")
			}
			if evs, next, more, err = s.d.Events.Changes(ctx, sub, p.ID, since, limit); err != nil || len(evs) > 0 {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	out := orchapi.SyncResponse{Changes: nonNil(evs), Cursor: next, More: more, Now: time.Now().UTC()}
	if sub.Scope.WholeProject(p.ID) {
		out.Started = s.d.Started
		if br, ok := orchcore.Homes(s.d).(orchcore.BackupReporter); ok {
			out.Backup = br.HomeBackup()
		}
	}
	return out, nil
}

// handleSnapshot is GET /v0/sync/snapshot?project=: NDJSON of everything the
// caller may read, ending with the cursor to sync from. The cursor is read
// first: anything committed after it has a larger id (team ids are minted in
// commit order), so sync sends it again, and applying a change twice is
// harmless (rows are upserted whole, comments and mail by id).
func (s *Service) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	p, err := s.teamProject(sub, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	gen, err := s.restoreGeneration()
	if err != nil {
		api.WriteError(w, err)
		return
	}
	cursor, err := s.d.Events.LastVisible(ctx, sub, p.ID)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	emit := func(kind string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return enc.Encode(api.SnapshotLine{Kind: kind, Data: raw})
	}
	for _, m := range []any{orchcore.Work(s.d), orchcore.Mail(s.d)} {
		sn, ok := m.(orchcore.Snapshotter)
		if !ok {
			continue
		}
		if err := sn.Snapshot(ctx, sub, p.ID, emit); err != nil {
			raw, _ := json.Marshal(api.ErrorResponse{Error: asAPIError(err)})
			_ = enc.Encode(api.SnapshotLine{Kind: api.SnapError, Data: raw})
			_ = bw.Flush()
			return
		}
	}
	now := time.Now().UTC()
	_ = enc.Encode(api.SnapshotLine{Kind: api.SnapEnd, Cursor: cursor, Now: &now, Generation: gen})
	_ = bw.Flush()
}

func asAPIError(err error) *api.Error {
	if e, ok := err.(*api.Error); ok {
		return e
	}
	return api.Errorf(api.CodeInternal, "snapshot: %v", err)
}

// writeLocks serialises one identity's batches per project: a replica that
// gave up waiting and sent its batch again must not run a write twice while
// the first run is still going.
type writeLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (l *writeLocks) lock(key string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	m := l.m[key]
	if m == nil {
		m = &sync.Mutex{}
		l.m[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// writablePath reports whether a queued write may name method and path:
// the work, lease and mail writes a replica queues, nothing else.
func writablePath(method, path string) bool {
	if strings.Contains(path, "?") || strings.Contains(path, "..") {
		return false
	}
	switch method {
	case http.MethodPost:
		switch {
		case path == "/v0/work" || path == "/v0/leases" || path == "/v0/mail":
			return true
		case strings.HasPrefix(path, "/v0/work/"):
			for _, v := range []string{"/claim", "/release", "/comment", "/status", "/link"} {
				if strings.HasSuffix(path, v) {
					return true
				}
			}
		case strings.HasPrefix(path, "/v0/mail/"):
			return strings.HasSuffix(path, "/ack") || strings.HasSuffix(path, "/reply")
		}
	case http.MethodDelete:
		return strings.HasPrefix(path, "/v0/leases/")
	}
	return false
}

// isStatusWrite: a field write that carries a base.
func isStatusWrite(w api.QueuedWrite) bool {
	return w.Method == http.MethodPost && strings.HasPrefix(w.Path, "/v0/work/") && strings.HasSuffix(w.Path, "/status")
}

// handleWrites is POST /v0/team/writes: a replica's outbox, in order. Each
// write runs as the route it names, as the replica (and the session it
// names in As), with its write id; its answer is recorded under (identity,
// id) and a replay gets that record. A write the authority cannot run now
// (unavailable, locked, internal) stops the batch unrecorded, so nothing
// after it runs before it: the replica retries from there.
func (s *Service) handleWrites(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "queued writes come from a team replica's daemon (its node-bound token)")
	}
	var req api.WritesRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if len(req.Writes) > maxBatch {
		return nil, api.Errorf(api.CodeBadRequest, "at most %d writes per batch", maxBatch)
	}
	p, err := s.teamProject(sub, req.Project)
	if err != nil {
		return nil, err
	}
	// ?gen= (a query parameter: an older home ignores it, where an unknown
	// body field would refuse the batch): the copy's restore generation.
	// Another one refuses the whole batch before any write runs.
	if q := r.URL.Query(); q.Has("gen") {
		gen, err := s.restoreGeneration()
		if err != nil {
			return nil, err
		}
		if q.Get("gen") != gen {
			return nil, ErrRestored
		}
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, p.ID)
	if err != nil {
		return nil, err
	}
	defer s.wlocks.lock(sub.Identity.ID + "\x00" + p.ID)()
	// The batch runs to its end even if the replica stops waiting: a write
	// cut off between its change and its record would replay as a conflict
	// with itself.
	ctx = context.WithoutCancel(ctx)
	s.pruneWrites(ctx, db)
	out := api.WritesResponse{Results: []api.WriteResult{}}
	// item -> the status event an earlier write of this batch minted: a
	// later status write of the item counts it as seen (its own change), and
	// nothing else does.
	statusSeen := map[string]string{}
	for _, w := range req.Writes {
		w.ID = strings.ToUpper(strings.TrimSpace(w.ID))
		if !ids.Valid(w.ID) {
			return out, api.Errorf(api.CodeBadRequest, "write id %q is not a ULID", w.ID)
		}
		res, err := s.queuedWrite(ctx, r, db, p.ID, w, statusSeen)
		if err != nil {
			out.Stopped = err.Error()
			return out, nil
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// queuedWrite holds the same per-operation lock as liveWrite until its result
// is durable. The outer batch lock still preserves queued ordering.
func (s *Service) queuedWrite(ctx context.Context, r *http.Request, db *sql.DB, projectID string, w api.QueuedWrite, statusSeen map[string]string) (api.WriteResult, error) {
	sub := core.SubjectOf(ctx)
	done, err := s.lockWrite(ctx, sub.Identity.ID, projectID, w.ID)
	if err != nil {
		return api.WriteResult{}, err
	}
	defer done()
	// Reuse authentication's address rules for replay evidence as well as
	// the dispatched operation, including on cached-result replay.
	probe := r.Clone(ctx)
	api.SetHeader(probe.Header, api.HeaderAs, w.As)
	readerCtx := s.d.Authenticate(probe)
	reader := core.SubjectOf(readerCtx)
	if core.AuthErr(readerCtx) != nil {
		reader = nil
	}
	if res, ok, err := s.recorded(ctx, db, sub.Identity.ID, w.ID, replayPrint(reader, projectID, w.Method, w.Path, w.Base, w.Body)); err != nil {
		return api.WriteResult{}, err
	} else if ok {
		if reader == nil || res.Status/100 != 2 {
			res.Seen, res.StatusSeen = "", ""
		} else {
			res.Seen, _ = s.d.Events.VisibleEventID(ctx, reader, projectID, res.Seen)
			res.StatusSeen, _ = s.d.Events.VisibleEventID(ctx, reader, projectID, res.StatusSeen)
		}
		if w.Item != "" && res.StatusSeen > statusSeen[w.Item] {
			statusSeen[w.Item] = res.StatusSeen
		}
		return res, nil
	}
	res := api.WriteResult{ID: w.ID}
	if !writablePath(w.Method, w.Path) {
		res.Status, res.Body = errBody(api.Errorf(api.CodeBadRequest, "%s %s is not a write a replica queues", w.Method, w.Path))
	} else {
		base := ""
		if isStatusWrite(w) && w.Base != "" {
			// Checked by the work module in the transaction that would
			// change the status (no silent last-writer-wins).
			base = max(w.Base, statusSeen[w.Item])
		}
		var minted string
		res.Status, res.Body = s.dispatch(ctx, r, projectID, w, base, &minted)
		if res.Status/100 == 2 && minted != "" {
			res.StatusSeen = minted
			if w.Item != "" {
				statusSeen[w.Item] = minted
			}
		}
	}
	if why := transient(res.Status, res.Body); why != "" {
		return api.WriteResult{}, api.Errorf(api.CodeUnavailable, "%s", why)
	}
	if rateLimited(res.Status, res.Body) {
		// Refused for now, not for good: answered (the batch goes on, its
		// order kept) but never recorded, so the same write id runs again
		// once retry_after has passed.
		return res, nil
	}
	if w.Item != "" && res.Status/100 == 2 && reader != nil {
		if last, err := s.d.Events.LastForVisible(ctx, reader, projectID, w.Item); err == nil && last != "" {
			res.Seen = last
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO team_writes (identity, id, status, body, seen, status_seen, time, req) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.Identity.ID, w.ID, res.Status, string(res.Body), res.Seen, res.StatusSeen, store.Millis(time.Now()), "q2:"+replayPrint(reader, projectID, w.Method, w.Path, w.Base, w.Body)); err != nil {
		// The write ran but its record did not land: answer it (the
		// replica is done with it); a replay would run it again, which
		// the write id makes harmless for comments and mail.
		s.d.Log.Warn("team: record write", "id", w.ID, "err", err)
	}
	return res, nil
}

// recorded is the stored answer to a write id, if any.
func (s *Service) recorded(ctx context.Context, db *sql.DB, identity, id, print string) (api.WriteResult, bool, error) {
	res := api.WriteResult{ID: id, Replayed: true}
	var body, record string
	err := db.QueryRowContext(ctx, `SELECT status, body, seen, status_seen, req FROM team_writes WHERE identity = ? AND id = ?`, identity, id).Scan(&res.Status, &body, &res.Seen, &res.StatusSeen, &record)
	if err == sql.ErrNoRows {
		return res, false, nil
	}
	if err != nil {
		return res, false, err
	}
	if record == cancelTomb {
		// Cancelled before it ran: a definitive answer, so the drain moves on.
		res.Status, res.Body = errBody(errCancelled())
		return res, true, nil
	}
	if err := replayCheck(record, print); err != nil {
		return res, true, err
	}
	// A live delivery may subsequently be retried by the durable outbox.
	// It now needs the queued retention window, not the shorter live cap.
	if strings.HasPrefix(record, "l2:") {
		if _, err := db.ExecContext(ctx, `UPDATE team_writes SET req = ? WHERE identity = ? AND id = ?`, "q2:"+print, identity, id); err != nil {
			return res, true, err
		}
	}
	res.Body = json.RawMessage(body)
	return res, true, nil
}

// pruneWrites drops write records past WriteKeep, now and then.
func (s *Service) pruneWrites(ctx context.Context, db *sql.DB) {
	if s.wprune.Add(1)%256 != 1 {
		return
	}
	pruneCancelled(ctx, db)
	_, _ = db.ExecContext(ctx, `DELETE FROM team_writes WHERE (req LIKE 'q2:%' OR req LIKE 'l2:%') AND time < ?`, store.Millis(time.Now().Add(-WriteKeep)))
}

// dispatch runs one queued write through this daemon's own handler, as the
// replica's request would have: the same transport (so the same node-bound
// token checks), Zm-As, the project by ULID and the write id.
func (s *Service) dispatch(ctx context.Context, outer *http.Request, projectID string, w api.QueuedWrite, base string, minted *string) (int, json.RawMessage) {
	in, err := http.NewRequestWithContext(context.WithValue(core.WithStatusSink(ctx, minted), dispatchKey{}, true), w.Method, w.Path, bytes.NewReader(w.Body))
	if err != nil {
		return errBody(api.Errorf(api.CodeBadRequest, "bad write: %v", err))
	}
	in.RemoteAddr = outer.RemoteAddr
	// Zm-Protocol and Zm-Scope-Version are read under either name (the
	// Potluck-* one first, as kit does) and carried under the new name, the
	// one this build sends; Zm-Personal-Mail is zm's own and has no other.
	for _, h := range []string{api.HeaderAuthorization, api.HeaderProtocol, api.HeaderScopeVersion, api.HeaderPersonalMail} {
		v := outer.Header.Get(h)
		if h == api.HeaderProtocol || h == api.HeaderScopeVersion {
			v = api.GetHeader(outer.Header, h)
		}
		switch {
		case v == "":
		case h == api.HeaderProtocol || h == api.HeaderScopeVersion:
			api.SetHeader(in.Header, h, v)
		default:
			in.Header.Set(h, v)
		}
	}
	in.Header.Set("Content-Type", "application/json")
	api.SetHeader(in.Header, api.HeaderProject, projectID)
	api.SetHeader(in.Header, api.HeaderProjectFrom, project.RuleFlag)
	api.SetHeader(in.Header, api.HeaderWriteID, w.ID)
	if base != "" {
		api.SetHeader(in.Header, api.HeaderBase, base)
	}
	if w.As != "" {
		api.SetHeader(in.Header, api.HeaderAs, w.As)
	}
	rec := httptest.NewRecorder()
	s.d.Handler.ServeHTTP(rec, in)
	body := bytes.TrimSpace(rec.Body.Bytes())
	if len(body) == 0 {
		body = []byte("null")
	}
	return rec.Code, json.RawMessage(body)
}

func errBody(e *api.Error) (int, json.RawMessage) {
	raw, _ := json.Marshal(api.ErrorResponse{Error: e})
	return e.Status(), raw
}

// rateLimited says a write's answer is a send limit's refusal (429
// rate_limited, SPEC 9 "Send limits"). It stands for this attempt, so it is
// answered and the batch goes on, but it is never recorded under its write
// id: a retry after retry_after must run again, not replay the refusal.
func rateLimited(status int, body json.RawMessage) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	var er api.ErrorResponse
	return json.Unmarshal(body, &er) == nil && er.Error != nil && er.Error.Code == api.CodeRateLimited
}

// transient says why a write's answer means "not now" (it is not recorded
// and stops the batch): the authority is unavailable or busy, failed
// internally, or the token's scope moved under it. "" for an answer that
// stands (done, or refused for good).
func transient(status int, body json.RawMessage) string {
	var er api.ErrorResponse
	_ = json.Unmarshal(body, &er)
	code, msg := "", ""
	if er.Error != nil {
		code, msg = er.Error.Code, er.Error.Message
	}
	switch {
	case status >= 500:
		return "the authority could not run it now (" + strconv.Itoa(status) + " " + code + ": " + msg + ")"
	case status == http.StatusUnauthorized:
		return "unauthenticated: " + msg
	case code == api.CodeRescope:
		return "rescope: " + msg
	case strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY"):
		return "busy: " + msg
	}
	return ""
}
