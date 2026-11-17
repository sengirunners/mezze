package team

// Lease writes that never queue, and durable cancel by write id (SPEC 17.6,
// "No-queue lease writes"). A claim, lease or release sent with Zm-No-Queue
// never enters the outbox: it is one live write with a write id, and when it
// gets no answer the replica asks the authority to cancel that id, so the
// write can never apply later.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// cancelTomb is the req of a tombstone row in team_writes: a write id that
// was cancelled before it ran.
const cancelTomb = "x1:cancelled"

// Tunables (tests shorten them).
var (
	// CancelKeep: how long a tombstone is kept (by time only; never counted
	// against MaxLiveRecords).
	CancelKeep = 7 * 24 * time.Hour
	// CancelWait bounds the replica's cancel call and its resend.
	CancelWait = 3 * time.Second
	// CancelSkew is the clock skew allowed between a replica (which mints
	// the write id) and the authority when judging whether the id's record
	// could have been pruned (settleable).
	CancelSkew = time.Hour
)

// noQueue reports whether r asks never to be queued (Zm-No-Queue: 1).
func noQueue(r *http.Request) bool { return api.GetHeader(r.Header, api.HeaderNoQueue) == "1" }

// refuseNoQueueForward is the lease rows' Refuse: a no-queue write the copy
// did not serve (the copy not ready, the item or lease not in it) is never
// forwarded, since a forward has no cancel. Nothing went out.
func refuseNoQueueForward(r *http.Request, _ []byte, snap api.Remote) error {
	if !noQueue(r) {
		return nil
	}
	return api.Errorf(api.CodeUnavailable, "this machine's copy of %s cannot take this write live yet: nothing was sent, nothing was queued", snap.Authority).
		WithHint("try again in a moment").WithData(api.NoQueueData{Outcome: api.OutcomeNotSent})
}

// settleable reports whether id is recent enough that its record, had it
// run, is still in team_writes: older than the live records kept (by time,
// or by the count cap), a missing record proves nothing.
func settleable(ctx context.Context, db *sql.DB, id string) bool {
	t := ids.Time(id)
	if t.IsZero() || t.Before(time.Now().Add(-LiveWriteKeep+CancelSkew)) {
		return false
	}
	var n int
	var oldest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT count(*), min(time) FROM team_writes WHERE req >= 'l2:' AND req < 'l2;'`).Scan(&n, &oldest); err != nil {
		return false
	}
	if n >= MaxLiveRecords && oldest.Valid && !t.After(time.UnixMilli(oldest.Int64).Add(CancelSkew)) {
		return false // the count cap may have taken its record
	}
	return true
}

func errCancelled() *api.Error {
	return api.Errorf(api.CodeConflict, "this write was cancelled; it never ran")
}

// pruneCancelled drops tombstones past CancelKeep.
func pruneCancelled(ctx context.Context, db *sql.DB) {
	_, _ = db.ExecContext(ctx, `DELETE FROM team_writes WHERE req = ? AND time < ?`, cancelTomb, store.Millis(time.Now().Add(-CancelKeep)))
}

// handleCancel is POST /v0/team/writes/{id}/cancel?project=. At the
// authority (a replica's node-bound token) it settles the write id: already
// run (answers its status), else tombstoned so no delivery ever runs it. On
// a replica, the machine owner's call is sent on to the authority.
func (s *Service) handleCancel(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	replica := sub != nil && sub.Node != "" && sub.Machine != ""
	if !replica && (sub == nil || sub.Via != authz.ViaLocal) {
		return nil, api.Errorf(api.CodeForbidden, "cancelling a write is for a team replica's daemon (its node-bound token) or its machine's owner")
	}
	id := strings.ToUpper(strings.TrimSpace(r.PathValue("id")))
	if !ids.Valid(id) {
		return nil, api.Errorf(api.CodeBadRequest, "write id %q is not a ULID", id)
	}
	ref := r.URL.Query().Get("project")
	if ref == "" && !replica {
		ref = api.GetHeader(r.Header, api.HeaderProject) // $ZM_PROJECT or the repo
		ref, _, _ = strings.Cut(ref, "/")
	}
	if !replica {
		if ref == "" {
			return nil, api.Errorf(api.CodeBadRequest, "name the joined project the write went to").WithHint("zm project team cancel -P <project> <write>")
		}
		x := s.ByName(ref)
		if x == nil {
			return nil, api.NotFound("project", ref)
		}
		if err := teamlink.Ended(x.Snapshot()); err != nil {
			return nil, err
		}
		var out api.WriteCancelResponse
		err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/writes/"+url.PathEscape(id)+"/cancel?project="+url.QueryEscape(x.Snapshot().Project.ID), struct{}{}, &out)
		return out, teamlink.OlderFor(err, x, "cancelling a write")
	}
	p, err := s.teamProject(sub, ref)
	if err != nil {
		return nil, err
	}
	if sub.Cap == nil || sub.Cap.Project != p.ID {
		// The journal is keyed as liveWrite keys it: the token's own project.
		return nil, api.NotFound("project", ref)
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, p.ID)
	if err != nil {
		return nil, err
	}
	done, err := s.lockWrite(ctx, sub.Identity.ID, p.ID, id) // waits behind an in-flight run
	if err != nil {
		return nil, err
	}
	defer done()
	ctx = context.WithoutCancel(ctx)
	pruneCancelled(ctx, db)
	var status int
	var req string
	err = db.QueryRowContext(ctx, `SELECT status, req FROM team_writes WHERE identity = ? AND id = ?`, sub.Identity.ID, id).Scan(&status, &req)
	switch {
	case err == nil && req == cancelTomb:
		return api.WriteCancelResponse{State: api.WriteCancelled}, nil
	case err == nil:
		return api.WriteCancelResponse{State: api.WriteRan, Status: status}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, api.Errorf(api.CodeUnavailable, "cannot verify the write")
	}
	if !settleable(ctx, db, id) {
		return nil, api.Errorf(api.CodeConflict, "write %s is too old to settle: its record may have been pruned, so whether it ran stays unknown", id).
			WithHint("check the lease itself (zm leases); it ends with its TTL")
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO team_writes (identity, id, status, body, seen, status_seen, time, req) VALUES (?, ?, 409, '', '', '', ?, ?)`,
		sub.Identity.ID, id, store.Millis(time.Now()), cancelTomb); err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "cannot record the cancel: %v", err)
	}
	return api.WriteCancelResponse{State: api.WriteCancelled}, nil
}

// noQueueFail is the 503 of a no-queue write that did not complete.
func noQueueFail(w http.ResponseWriter, msg, hint, write, outcome string) bool {
	e := api.Errorf(api.CodeUnavailable, "%s", msg)
	if hint != "" {
		e = e.WithHint(hint)
	}
	api.WriteError(w, e.WithData(api.NoQueueData{Write: write, Outcome: outcome}))
	return true
}

// liveAs sends one write live to x's authority as `as` under write id id,
// and returns the authority's status and body.
func (s *Service) liveAs(ctx context.Context, x *remote, as, id, method, path string, body []byte) (int, []byte, error) {
	snap := x.Snapshot()
	req, err := http.NewRequestWithContext(ctx, method, snap.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	api.SetHeader(req.Header, api.HeaderWriteID, id)
	api.SetHeader(req.Header, api.HeaderProject, snap.Project.ID)
	api.SetHeader(req.Header, api.HeaderProjectFrom, project.RuleFlag)
	api.SetHeader(req.Header, api.HeaderAs, as)
	resp, err := s.Send(ctx, x, req, body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, buf.Bytes(), nil
}

// leaseNoQueue serves a claim, lease or release that must never queue.
func (s *Service) leaseNoQueue(w http.ResponseWriter, r *http.Request, x *remote, rp *replica, kind, method, path string, body []byte, as string) bool {
	snap := x.Snapshot()
	if !rp.isReachable() {
		return noQueueFail(w, "the authority "+snap.Authority+" is unreachable: nothing was sent, nothing was queued", "try again once it answers", "", api.OutcomeNotSent)
	}
	id := ids.New()
	relay := func(status int, out []byte) bool {
		if status/100 == 2 {
			s.noteLease(outEntry{kind: kind, as: as}, api.WriteResult{Body: out})
			s.kickPresence()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
		return true
	}
	// An answer that stands (done, or refused for good) is relayed; a
	// transient one (unavailable, busy, rescope) is no answer.
	answered := func(status int, out []byte, err error) bool {
		return err == nil && transient(status, out) == ""
	}
	cctx, cancel := context.WithTimeout(r.Context(), ClaimWait)
	status, out, err := s.liveAs(cctx, x, as, id, method, path, body)
	cancel()
	if answered(status, out, err) {
		return relay(status, out)
	}
	// No answer: settle the write id, so it can never apply later.
	bg := context.WithoutCancel(r.Context())
	kctx, kcancel := context.WithTimeout(bg, CancelWait)
	var cr api.WriteCancelResponse
	err = s.CallRemote(kctx, x, http.MethodPost, "/v0/team/writes/"+url.PathEscape(id)+"/cancel?project="+url.QueryEscape(snap.Project.ID), struct{}{}, &cr)
	kcancel()
	hint := "zm project team cancel -P " + x.Slug() + " " + id
	unknown := func() bool {
		return noQueueFail(w, "no answer from the authority "+snap.Authority+", and it did not confirm a cancel: the write may have applied",
			hint, id, api.OutcomeUnknown)
	}
	if err != nil {
		return unknown() // no answer, or an old authority (404): never guess
	}
	switch cr.State {
	case api.WriteCancelled:
		return noQueueFail(w, "no answer from the authority "+snap.Authority+": the write was cancelled, nothing was claimed or released", "try again", id, api.OutcomeCancelled)
	case api.WriteRan:
		// It ran: the same id gets the recorded answer.
		rctx, rcancel := context.WithTimeout(bg, CancelWait)
		status, out, err = s.liveAs(rctx, x, as, id, method, path, body)
		rcancel()
		if answered(status, out, err) {
			return relay(status, out)
		}
		return noQueueFail(w, fmt.Sprintf("the authority %s says the write ran (status %d), but its recorded answer did not come back", snap.Authority, cr.Status),
			hint, id, api.OutcomeUnknown)
	}
	return unknown()
}
