package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Live writes on the authority (SPEC 17.6, 17.11). A replica sends every
// write it forwards live (a route whose row is a Write, never a read) with a
// Zm-Write-Id, as it does a queued one. The authority answers a write id it
// already ran with the recorded answer, whatever write route it names: a
// native issue, a claim, a status change, mail or a comment. A rescope
// retry, a request delivered twice, or one resent by whoever carried it then
// never runs twice. The record is team_writes, the queued writes' own table
// (scrubbed on redaction), tied to the request and authenticated authority: a
// write id reused for another request is 409, never the old answer. Live
// records are kept LiveWriteKeep and at most MaxLiveRecords per project;
// an answer over MaxRecordedBody keeps only its status. A send limit's
// refusal (429 rate_limited) is never recorded: a retry may run.

var livewritesMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "team/0005_write_req", Script: `ALTER TABLE team_writes ADD COLUMN req TEXT NOT NULL DEFAULT '';
CREATE INDEX team_writes_req_time ON team_writes (req, time);`},
}

// Tunables (tests shorten them).
var (
	LiveWriteKeep         = 24 * time.Hour
	MaxLiveBody     int64 = 8 << 20 // a live write's body; over it, 413
	MaxLiveRecords        = 100_000
	MaxRecordedBody       = 64 << 10
	LivePruneEvery  int64 = 256
)

// reqPrint ties a record to its request.
func reqPrint(method, uri string, body []byte) string {
	h := sha256.Sum256(body)
	return method + " " + uri + " " + hex.EncodeToString(h[:])
}

// dispatchKey marks handleWrites' own calls through the handler: their
// write ids are recorded by the batch.
type dispatchKey struct{}

// replayPrint is server-owned evidence of the complete request and its
// authenticated authority. Equal current scope predicates prove that the
// original response is still authorized, even for errors/acks or deleted mail.
// Mail scope/audience and exact participant addresses are immutable; matching
// the actor, capability and current scope preserves their original checks.
// Changed scopes conservatively hold the replay; they never redispatch it.
func replayPrint(sub *authz.Subject, project, method, uri, base string, body []byte) string {
	if sub == nil {
		return ""
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body) == nil {
		body = compact.Bytes()
	}
	proof := []any{sub.Identity.ID, sub.Address, sub.Node, sub.Machine,
		sub.PersonalMail, sub.Cap, project, method, uri, base, reqPrint(method, uri, body)}
	for _, action := range []authz.Action{authz.Read, authz.Write, authz.Operate, authz.Admin} {
		where, args := sub.Scope.AreaSQL(project, "area", action)
		proof = append(proof, where, args)
	}
	b, _ := json.Marshal(proof)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func replayCheck(record, print string) error {
	if record == cancelTomb {
		return errCancelled()
	}
	if !strings.HasPrefix(record, "q2:") && !strings.HasPrefix(record, "l2:") {
		return api.Errorf(api.CodeUnavailable, "cached write lacks replay authorization evidence; retained for review")
	}
	if print == "" || record[3:] != print {
		return api.Errorf(api.CodeConflict, "cached write request or authorization changed; retained for review")
	}
	return nil
}

// liveInflight holds a write id while it runs, so a second delivery waits
// for the first answer instead of running beside it.
type liveInflight struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (l *liveInflight) enter(key string) (wait <-chan struct{}, done func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]chan struct{}{}
	}
	if ch, ok := l.m[key]; ok {
		return ch, nil
	}
	ch := make(chan struct{})
	l.m[key] = ch
	return nil, func() {
		l.mu.Lock()
		delete(l.m, key)
		l.mu.Unlock()
		close(ch)
	}
}

// lockWrite is shared by live and queued dispatch. Different write IDs remain
// concurrent; a mixed retry waits for its original operation and journal entry.
func (s *Service) lockWrite(ctx context.Context, identity, project, id string) (func(), error) {
	for {
		wait, done := s.live.enter(identity + "\x00" + project + "\x00" + id)
		if done != nil {
			return done, nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, api.Errorf(api.CodeUnavailable, "cancelled")
		}
	}
}

// liveWrite serves r from its record, or runs it and records the answer,
// when r is a replica's live write with a write id. False: not one.
func (s *Service) liveWrite(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	ctx := r.Context()
	if core.IsLocal(ctx) || ctx.Value(dispatchKey{}) != nil || !s.IsForwardedWrite(r) {
		return false
	}
	id := core.WriteID(r)
	sub := core.SubjectOf(ctx)
	if id == "" || sub == nil || sub.Node == "" || sub.Cap == nil || sub.Cap.Project == "" || !s.d.Projects.IsTeam(sub.Cap.Project) {
		return false
	}
	var importTarget *core.Target
	if r.Method == http.MethodPost && r.URL.Path == "/v0/mail/imports" {
		// The actual selected destination must still exist and be writable
		// before serving a cached import result. Capability project alone is
		// not evidence of what this request selected.
		target, err := s.importReplayTarget(r, sub)
		if err != nil {
			api.WriteError(w, err)
			return true
		}
		importTarget = &target
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, sub.Cap.Project)
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "cannot verify cached write"))
		return true
	}
	done, err := s.lockWrite(ctx, sub.Identity.ID, sub.Cap.Project, id)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	defer done()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxLiveBody+1))
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "reading the body: %v", err))
		return true
	}
	if int64(len(body)) > MaxLiveBody {
		// Refused whole, never cut short: a record must match the request.
		api.WriteError(w, api.Errorf(api.CodeTooLarge, "a write's body is over %d bytes", MaxLiveBody))
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	print := replayPrint(sub, sub.Cap.Project, r.Method, r.URL.RequestURI(), api.GetHeader(r.Header, api.HeaderBase), body)
	if importTarget != nil {
		// Bind canonical project+area rather than raw spelling. Equivalent
		// slug/ID and query/header selections may replay; changing the area
		// must hold this write, never redispatch it. Old import cache proofs
		// lack this evidence and conservatively hold until explicit review.
		q := r.URL.Query()
		q.Del("project")
		uri := r.URL.EscapedPath()
		if enc := q.Encode(); enc != "" {
			uri += "?" + enc
		}
		proof := []string{replayPrint(sub, sub.Cap.Project, r.Method, uri, api.GetHeader(r.Header, api.HeaderBase), body), importTarget.Project.ID, importTarget.Area}
		b, _ := json.Marshal(proof)
		h := sha256.Sum256(b)
		print = "import1:" + hex.EncodeToString(h[:])
	}
	var status int
	var recBody, recReq string
	err = db.QueryRowContext(ctx, `SELECT status, body, req FROM team_writes WHERE identity = ? AND id = ?`, sub.Identity.ID, id).
		Scan(&status, &recBody, &recReq)
	if err == nil {
		if err := replayCheck(recReq, print); err != nil {
			api.WriteError(w, err)
			return true
		}
		if recBody == "" {
			recBody = "{}" // over MaxRecordedBody: only the status was kept
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(api.HeaderWriteReplayed, "1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, recBody)
		return true
	}
	if err != sql.ErrNoRows {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "cannot verify cached write"))
		return true
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	out := rec.Body.Bytes()
	_, _ = w.Write(out)
	if transient(rec.Code, json.RawMessage(out)) == "" && !rateLimited(rec.Code, json.RawMessage(out)) {
		keep := string(out)
		if len(out) > MaxRecordedBody || !json.Valid(out) {
			keep = ""
		}
		s.recordLive(context.WithoutCancel(ctx), db, sub.Identity.ID, id, rec.Code, keep, "l2:"+print)
	}
	return true
}

func (s *Service) recordLive(ctx context.Context, db *sql.DB, identity, id string, status int, body, print string) {
	if s.livePrune.Add(1)%LivePruneEvery == 1 || LivePruneEvery == 1 {
		// Live records: a day, and at most MaxLiveRecords per project (the
		// newest kept). Queued records keep WriteKeep; legacy evidence is held for review.
		pruneCancelled(ctx, db) // tombstones: by time only, never counted
		_, _ = db.ExecContext(ctx, `DELETE FROM team_writes WHERE req LIKE 'l2:%' AND time < ?`, store.Millis(time.Now().Add(-LiveWriteKeep)))
		_, _ = db.ExecContext(ctx, `DELETE FROM team_writes WHERE req LIKE 'l2:%' AND rowid IN (
			SELECT rowid FROM team_writes WHERE req LIKE 'l2:%' ORDER BY time DESC LIMIT -1 OFFSET ?)`, MaxLiveRecords)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO team_writes (identity, id, status, body, seen, status_seen, time, req) VALUES (?, ?, ?, ?, '', '', ?, ?)`,
		identity, id, status, body, store.Millis(time.Now()), print); err != nil {
		s.d.Log.Warn("team: record live write", "id", id, "err", err)
	}
}
