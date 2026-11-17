package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// The outbox (SPEC 17.6 "Writes"). Every write to a joined project commits
// to the copy's outbox first; the drain sends pending entries to the
// authority (POST /v0/team/writes) strictly in order, a batch at a time. An
// answer that stands (done, or refused for good) settles the entry; a "not
// now" (unreachable, busy, internal) leaves it and everything after it
// pending, to retry in order. Refused entries are kept as conflict/failed
// for the record and never retried. An acked entry stays (overlaid) until
// the copy has caught up with what it did, then goes.

// Tunables (tests shorten them).
var (
	// ClaimWait bounds how long a claim, release or lease waits for its
	// answer while the authority is reachable; past it the caller is told
	// the write is queued.
	ClaimWait = 5 * time.Second
	// drainBatch is the most writes sent at once.
	drainBatch = 100
	// retryMin/retryMax bound the drain's and sync's backoff.
	retryMin = 250 * time.Millisecond
	retryMax = 5 * time.Second
	// keepSettled is how long refused entries are kept for the record.
	keepSettled = 7 * 24 * time.Hour
	// keepAcked bounds how long an acked entry waits for the copy.
	keepAcked = 5 * time.Minute
)

// outEntry is one outbox row.
type outEntry struct {
	seq                                                         int64
	id, kind, method, path, body, as, item, base, state, errMsg string
	result, seen                                                string
	created, done                                               time.Time
}

const outCols = `seq, id, kind, method, path, body, as_addr, item, base, state, error, result, seen, created, done`

func scanOut(sc interface{ Scan(...any) error }) (outEntry, error) {
	var e outEntry
	var c, d int64
	err := sc.Scan(&e.seq, &e.id, &e.kind, &e.method, &e.path, &e.body, &e.as, &e.item, &e.base, &e.state, &e.errMsg, &e.result, &e.seen, &c, &d)
	e.created = store.FromMillis(c)
	if d > 0 {
		e.done = store.FromMillis(d)
	}
	return e, err
}

// enqueue commits a write to the outbox and wakes the drain. base is the
// item's base for a field write ("" otherwise).
func (rp *replica) enqueue(ctx context.Context, kind, method, path string, body []byte, as, item, base string) (outEntry, error) {
	return rp.enqueueID(ctx, ids.New(), kind, method, path, body, as, item, base)
}

// enqueueID is enqueue with the write's id chosen by the caller (one that
// registered to wait for its answer first: the drain may answer at once).
func (rp *replica) enqueueID(ctx context.Context, id, kind, method, path string, body []byte, as, item, base string, visibility ...outVisibility) (outEntry, error) {
	v := rp.writeVisibility(ctx, kind, path, body, as)
	if len(visibility) > 0 {
		v = visibility[0]
	}
	if v.Area == "" && item != "" {
		_ = rp.db.QueryRowContext(ctx, `SELECT area FROM items WHERE id=?`, item).Scan(&v.Area)
	}
	vis, err := json.Marshal(v)
	if err != nil {
		return outEntry{}, err
	}
	rp.mu.Lock()
	full := rp.pending >= OutboxMax
	old := !rp.synced.IsZero() && time.Since(rp.synced) > RefuseAfter
	rp.mu.Unlock()
	if full {
		return outEntry{}, api.Errorf(api.CodeUnavailable, "the outbox holds %d writes the authority has not taken yet", OutboxMax).
			WithHint("zm team outbox shows them; they drain when the authority answers")
	}
	if old {
		return outEntry{}, api.Errorf(api.CodeUnavailable, "the authority has not answered for over %s: writes are refused (reads still work, marked stale)", RefuseAfter).
			WithHint("check the tailnet (zm tailnet, zm peers)")
	}
	e := outEntry{id: id, kind: kind, method: method, path: path, body: string(body), as: as, item: item, base: base,
		state: api.OutboxPending, created: time.Now().UTC()}
	made := false
	err = rp.write(ctx, func(tx *sql.Tx) error {
		prior, err := scanOut(tx.QueryRowContext(ctx, `SELECT `+outCols+` FROM outbox WHERE id=?`, id))
		if err == nil {
			if prior.kind != kind || prior.method != method || prior.path != path || prior.body != string(body) || prior.as != as {
				return api.Errorf(api.CodeConflict, "write ID already has another payload")
			}
			e = prior
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		made = true
		if kind == "status" {
			// The base is read in the transaction that queues the write, so
			// a settle bumping it in between cannot leave this one behind.
			var b string
			_ = tx.QueryRowContext(ctx, `SELECT base FROM items WHERE id = ?`, item).Scan(&b)
			if b == "" {
				_ = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'cursor'`).Scan(&b)
			}
			if b > e.base {
				e.base = b
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO outbox (id, kind, method, path, body, as_addr, item, base, state, created, visibility) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.id, e.kind, e.method, e.path, e.body, e.as, e.item, e.base, e.state, store.Millis(e.created), string(vis))
		return err
	})
	if err != nil {
		return outEntry{}, err
	}
	rp.mu.Lock()
	if made {
		rp.pending++
	}
	rp.mu.Unlock()
	kick(rp.kickDrain)
	return e, nil
}

// waitFor registers a caller waiting for write id's answer (call before
// the drain can answer it: right after enqueue).
func (rp *replica) waitFor(id string) chan api.WriteResult {
	ch := make(chan api.WriteResult, 1)
	rp.wmu.Lock()
	rp.waiters[id] = append(rp.waiters[id], ch)
	rp.wmu.Unlock()
	return ch
}

func (rp *replica) stopWaiting(id string, ch chan api.WriteResult) {
	rp.wmu.Lock()
	defer rp.wmu.Unlock()
	ws := rp.waiters[id]
	for i, w := range ws {
		if w == ch {
			rp.waiters[id] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(rp.waiters[id]) == 0 {
		delete(rp.waiters, id)
	}
}

// answer hands a write's result to the callers waiting for it and reports
// whether one took it (it then relays the answer itself). Sent under wmu
// (the channels are buffered): a caller that stopped waiting either took it
// or was never handed it, never both. taken, when set, runs under wmu
// before anything is sent and only when a caller takes it, so what it does
// (dropping a refused write from the outbox) is done before any caller can
// see the answer.
func (rp *replica) answer(res api.WriteResult, taken func()) bool {
	rp.wmu.Lock()
	defer rp.wmu.Unlock()
	ws := rp.waiters[res.ID]
	delete(rp.waiters, res.ID)
	if len(ws) > 0 && taken != nil {
		taken()
	}
	for _, ch := range ws {
		ch <- res
	}
	return len(ws) > 0
}

func (rp *replica) countOutbox(ctx context.Context) {
	var p, c int
	_ = rp.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE state = ?`, api.OutboxPending).Scan(&p)
	_ = rp.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE state IN (?, ?)`, api.OutboxConflict, api.OutboxFailed).Scan(&c)
	rp.mu.Lock()
	rp.pending, rp.conflicts = p, c
	rp.mu.Unlock()
}

// pendingBatch is the oldest pending entries, in order.
func (rp *replica) pendingBatch(ctx context.Context) ([]outEntry, error) {
	rows, err := rp.db.QueryContext(ctx, `SELECT `+outCols+` FROM outbox WHERE state = ? ORDER BY seq LIMIT ?`, api.OutboxPending, drainBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outEntry
	for rows.Next() {
		e, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// drainOnce sends one batch. It reports whether more may be waiting, and
// an error when the authority could not be reached or could not run the
// batch now (back off, then retry from the first unsettled entry).
func (s *Service) drainOnce(ctx context.Context, x *remote, rp *replica) (more bool, err error) {
	batch, err := rp.pendingBatch(ctx)
	if err != nil || len(batch) == 0 {
		return false, err
	}
	// A caller waiting on one of these (a claim, a mail) gets its answer
	// when the batch returns: end the batch at the first such write, so it
	// does not also wait for every write queued behind it. Order holds: it
	// still goes after everything queued before it.
	rp.wmu.Lock()
	for i, e := range batch {
		if len(rp.waiters[e.id]) > 0 {
			batch = batch[:i+1]
			break
		}
	}
	rp.wmu.Unlock()
	snap := x.Snapshot()
	if err := teamlink.Ended(snap); err != nil {
		return false, err
	}
	rp.mu.Lock()
	restore := rp.restore
	rp.mu.Unlock()
	req := api.WritesRequest{Project: snap.Project.ID}
	for _, e := range batch {
		w := api.QueuedWrite{ID: e.id, Method: e.method, Path: e.path, As: e.as, Item: e.item, Base: e.base}
		if e.body != "" {
			w.Body = json.RawMessage(e.body)
		}
		req.Writes = append(req.Writes, w)
	}
	var resp api.WritesResponse
	cctx, cancel := context.WithTimeout(ctx, teamlink.CallTimeout)
	fence := rp.copyFence()
	// gen= always, even empty (never restored): see handleWrites.
	err = s.CallRemote(cctx, x, http.MethodPost, "/v0/team/writes?gen="+url.QueryEscape(restore), req, &resp)
	cancel()
	if err != nil {
		if api.IsCode(err, api.CodeResync) {
			// The home was restored since the copy was taken: a snapshot
			// first (ending a long poll in flight), then these go again,
			// against a copy that knows what the restore lost.
			rp.mu.Lock()
			rp.resnap++
			if rp.endPoll != nil {
				rp.endPoll()
			}
			rp.mu.Unlock()
			kick(rp.kickSync)
		} else if api.IsCode(err, api.CodeUnavailable) || ctx.Err() == nil && !isAPIError(err) {
			rp.markDown(err)
		} else {
			// The authority answered but refused the batch itself (forbidden,
			// not found): nothing settles, the entries stay pending and
			// retry, and remotes/sources show why.
			rp.mu.Lock()
			rp.lastErr = sanitize("the authority refused queued writes: " + err.Error())
			rp.mu.Unlock()
		}
		return false, err
	}
	rp.markAnswer(time.Time{})
	byID := map[string]outEntry{}
	refused := false
	for _, e := range batch {
		byID[e.id] = e
	}
	for _, res := range resp.Results {
		e, ok := byID[res.ID]
		if !ok {
			continue
		}
		done := rp.copyReplyHook("settle", e.item)
		s.settle(ctx, rp, e, res, fence)
		if done != nil {
			done()
		}
		if res.Status/100 != 2 {
			refused = true
		}
	}
	if refused {
		s.logMoot(snap.Project.Slug, rp.dropMoot(ctx))
	}
	rp.countOutbox(ctx)
	if resp.Stopped != "" {
		rp.mu.Lock()
		rp.lastErr = sanitize(resp.Stopped)
		rp.mu.Unlock()
		return false, api.Errorf(api.CodeUnavailable, "%s", resp.Stopped)
	}
	kick(rp.kickSync)                           // the copy catches up with what the writes did
	return len(resp.Results) == len(batch), nil // more may follow; the next round finds none if not
}

func isAPIError(err error) bool {
	_, ok := err.(*api.Error)
	return ok
}

// settle records a write's answer: acked (overlaid until the copy shows
// it), conflict (a claim lost, a stale base, group mail taken first) or
// failed (refused otherwise); later pending writes of the same item take
// what it left behind as seen (their base).
func (s *Service) settle(ctx context.Context, rp *replica, e outEntry, res api.WriteResult, fence copyFence) {
	state, msg := api.OutboxAcked, ""
	var refusal api.Error // the authority's code, wait and structured data, for the sender's notice
	lost := false         // refused because the home's restore lost the item
	if res.Status/100 != 2 {
		state = api.OutboxFailed
		var er api.ErrorResponse
		if json.Unmarshal(res.Body, &er) == nil && er.Error != nil {
			msg, refusal = er.Error.Message, *er.Error
			if er.Error.Code == api.CodeConflict {
				state = api.OutboxConflict
			}
		} else {
			msg = http.StatusText(res.Status)
		}
	} else if e.kind == "ack" {
		var ar orchapi.AckResponse
		if json.Unmarshal(res.Body, &ar) == nil && !ar.Acked {
			state, msg = api.OutboxConflict, "taken first by "+ar.AckedBy
		}
	}
	now := time.Now()
	// An answered write settles without waiting on the disk (writeApply): lost
	// to a power cut, it is sent again and the authority answers it from its
	// write record (team_writes), never running it twice. A refusal waits on
	// the disk (write): its caller is told right after, and a refusal replayed
	// with nobody waiting marks the session needs_human for a conflict that
	// caller already saw.
	write := rp.writeApply
	if state != api.OutboxAcked {
		write = rp.write
	}
	// Keep lock order snapshotMu -> writing, and never hold either over the
	// authority call. Outcome bookkeeping survives an obsolete copy answer.
	rp.snapshotMu.RLock()
	defer rp.snapshotMu.RUnlock()
	copyFresh := rp.snapshotEpoch == fence.epoch
	if state != api.OutboxAcked && (e.kind == "mail" || e.kind == "reply") {
		// A refusal carries nothing for the copy: a snapshot installed while
		// the authority decided must not hide it from the outbox listing,
		// where its sender reads the authority's message.
		copyFresh = true
	}
	// Under snapshotMu: a restore's install either recorded the loss before
	// this, or runs after this row is written and un-fences it then
	// (unfenceLostRefusals). Never a refusal hidden by a restore in between.
	if state != api.OutboxAcked && rp.lostToRestore(e.item) {
		lost = true
		msg = lostRefusal(e.item, msg)
	}
	settled := false // this call moved the entry out of pending: only it tells the sender
	limited := rateLimited(res.Status, res.Body)
	err := write(ctx, func(tx *sql.Tx) error {
		if limited && (e.kind == "mail" || e.kind == "reply") {
			// A send limit's refusal is not recorded at the authority: free
			// the client_request_id, so a retry after retry_after with the
			// same key is a new write, not this refusal replayed.
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_mail_requests WHERE write_id = ?`, e.id); err != nil {
				return err
			}
		}
		if copyFresh && strings.HasPrefix(e.path, "/v0/work/") {
			var err error
			copyFresh, err = copyItemCurrent(ctx, tx, e.item, fence)
			if err != nil {
				return err
			}
		}
		if lost {
			// The copy already records (after the restore) that the item is
			// gone, so this refusal is current news even if the request
			// predates that snapshot: a refusal carries no payload for the copy,
			// and the queued write's outcome and its session's notice must stay
			// visible. rp.lost only holds items the copy had in
			// scope, and the outbox listing still filters by the entry's area.
			copyFresh = true
		}
		if copyFresh && (e.kind == "lease" || e.kind == "release") {
			// Generic lease routes can also name a work item, but arbitrary
			// lock keys need no items row. A known move still fences an old
			// answer carried through /v0/leases rather than /v0/work.
			var moved string
			err := tx.QueryRowContext(ctx, `SELECT event FROM moved_items WHERE id = ?`, e.item).Scan(&moved)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			copyFresh = moved <= fence.base
		}
		var base string
		if copyFresh && e.item != "" {
			err := tx.QueryRowContext(ctx, `SELECT base FROM items WHERE id = ?`, e.item).Scan(&base)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			// Seen is optional and may be hidden on recorded-answer replay.
			// Without it, an item that advanced after this request began is
			// no longer ordered against a successful answer. Keep its durable
			// outcome, but do not overwrite or overlay newer copy/cache state.
			// A refusal has no success payload to apply: a newer event on a
			// still-visible item must not hide its conflict or session notice.
			if state == api.OutboxAcked && res.Seen == "" && base > fence.base {
				copyFresh = false
			}
		}
		// Before the entry stops being pending: a heartbeat after that must
		// not report a lease this current-scope write ended.
		if copyFresh && state == api.OutboxAcked && e.as != "" &&
			(res.Seen == "" || base <= res.Seen) &&
			(e.kind == "claim" || e.kind == "lease" || e.kind == "release" || e.kind == "status") {
			// As with the SQL copy below, a strictly newer item/lease event
			// also wins in the presence cache, even within one snapshot epoch.
			s.noteLease(e, res)
		}
		done, err := tx.ExecContext(ctx, `UPDATE outbox SET state = ?, error = ?, result = ?, seen = ?, done = ?, copy_fenced = ? WHERE id = ? AND state = ?`,
			state, msg, string(res.Body), res.Seen, store.Millis(now), !copyFresh, e.id, api.OutboxPending)
		if err != nil {
			return err
		}
		n, _ := done.RowsAffected()
		settled = n > 0
		if copyFresh && state == api.OutboxAcked {
			// Read your writes: the authority's answer goes into the copy
			// now, unless the copy already shows what the write left behind
			// (its item's base reached seen: the answer would be older than
			// the copy). The change's own event re-applies it (rows are
			// upserted whole, comments and mail by id).
			if e.item == "" || res.Seen == "" || base < res.Seen {
				if err := applyResult(ctx, tx, e, res.Body); err != nil {
					return err
				}
			}
		}
		if copyFresh && e.item != "" && res.StatusSeen != "" {
			// Only the status event this write minted counts as seen by later
			// status writes of the item (and by the copy's row, which the
			// answer just updated): another write's events never raise a base,
			// or they would hide a status change the replica never saw. Any
			// status event before it was at or below this write's own base
			// (the authority checked), so nothing unseen hides behind it.
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET base = max(base, ?) WHERE item = ? AND state = ? AND kind = 'status'`, res.StatusSeen, e.item, api.OutboxPending); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE items SET base = max(base, ?) WHERE id = ?`, res.StatusSeen, e.item); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && !copyFresh && state != api.OutboxAcked {
		rp.mu.Lock() // still under snapshotMu: the epoch is the one this settled in
		if rp.fenced == nil {
			rp.fenced = map[string]uint64{}
		}
		rp.fenced[e.id] = rp.snapshotEpoch
		rp.mu.Unlock()
	}
	// Keep the snapshot fence through presence/session metadata too: releasing
	// it between the copyFresh check and these effects lets a narrowing
	// snapshot finish before an old answer publishes its local state. The
	// replica transaction has released writing before SetState; that callback
	// updates the session registry and publishes events, without taking this
	// copy's snapshot lock.
	if err != nil {
		s.d.Log.Warn("team: settle write", "id", e.id, "err", err)
	}
	if copyFresh && state == api.OutboxAcked && (e.kind == "mail" || e.kind == "reply") {
		var m orchapi.Mail
		if json.Unmarshal(res.Body, &m) == nil && m.From == e.as {
			s.waitingOn(e.as, m)
		}
	}
	// Only now does a waiting caller get the answer (the copy and the
	// session already show it). A caller that took a refusal (a claim's
	// 409, a mail's 404) has been told: the entry is not kept as a queued
	// write that lost, and nobody is told twice.
	// The entry goes before the caller hears (a caller that saw
	// the refusal and then read the outbox found it still there).
	drop := func() {
		if state != api.OutboxAcked && !settleStopsAfterAnswer.Load() {
			_ = rp.exec(ctx, `DELETE FROM outbox WHERE id = ?`, e.id)
		}
	}
	if rp.answer(res, drop) {
		return
	}
	if err == nil && settled && state != api.OutboxAcked && e.as != "" && (e.kind == "mail" || e.kind == "reply") {
		// A queued mail or reply the authority refused (a send limit, a
		// reply to mail that is gone): its sender was told it was queued,
		// so it is told this too, once, with the refusal's code, rule and
		// wait. Through the session's own state, a local write that is
		// neither mail (so no send limit reaches it and it cannot start a
		// reply loop) nor held back by the copy's fence. The entry stays in
		// the outbox listing, with the authority's message, for the record.
		s.setLocalState(e.as, orchapi.StateNeedsHuman, queuedMailRefusal(e.kind, state, refusal))
	} else if copyFresh && state != api.OutboxAcked && e.as != "" && (e.kind == "claim" || e.kind == "lease" || e.kind == "status") {
		// A queued write that lost comes back as a clear conflict: the
		// session that made it waits for a human, as for a lost lease.
		s.setLocalState(e.as, orchapi.StateNeedsHuman, "queued "+e.kind+" "+state+": "+e.item)
	} else if copyFresh && state != api.OutboxAcked && e.as != "" && lost {
		// Any queued write on an item a restore lost (a comment, a link):
		// its session was told it was queued, so it is told this too.
		s.setLocalState(e.as, orchapi.StateNeedsHuman, "queued "+e.kind+" not applied: the home was restored and lost "+e.item)
	} else if copyFresh && state != api.OutboxAcked && e.as != "" && linkGoneFor(ctx, rp, outEntry{kind: e.kind, state: state, body: e.body}) != "" {
		// A queued link to another project's item, refused: dropMoot drops
		// it next, so its session (told it was written) is told it was not.
		s.setLocalState(e.as, orchapi.StateNeedsHuman, "queued link "+state+": "+e.item+": "+msg)
	}
}

// queuedMailRefusal is the note a session carries after the authority
// refused its queued mail or reply. A session's note is listed beyond the
// mail's own audience, so it is built from the refusal's code, rule (a rate
// limit's data) and wait only, never the authority's free text, which can name
// the recipient or an earlier mail; the whole message is in zm team outbox.
func queuedMailRefusal(kind, state string, er api.Error) string {
	code := string(er.Code)
	if code == "" {
		code = "refused" // an answer that was not an error body
	}
	note := "queued " + kind + " " + state + ": " + code
	var d struct {
		Limit string `json:"limit"`
	}
	if json.Unmarshal(er.Data, &d) == nil && d.Limit != "" && len(d.Limit) <= 16 && strings.Trim(d.Limit, "abcdefghijklmnopqrstuvwxyz_") == "" {
		note += " (" + d.Limit + " limit)"
	}
	if er.RetryAfter > 0 {
		note += "; retry after " + (time.Duration(er.RetryAfter) * time.Second).String()
	}
	return note + "; zm team outbox has the authority's message"
}

// applyResult puts an acked write's answer into the copy.
func applyResult(ctx context.Context, tx *sql.Tx, e outEntry, body json.RawMessage) error {
	switch e.kind {
	case "claim", "lease":
		var cr orchapi.ClaimResponse
		if json.Unmarshal(body, &cr) != nil || cr.Lease.Key == "" {
			return nil
		}
		// The lease only: the answer's item is a view (a claimed item shows
		// "claimed" even when its row stays in_review), not the row, and no
		// later event would correct a copy that took it as one.
		return putLease(ctx, tx, cr.Lease)
	case "release":
		var rr orchapi.ReleaseResponse
		if json.Unmarshal(body, &rr) == nil && rr.Released {
			_, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE key = ?`, rr.Key)
			return err
		}
	case "comment":
		var c orchapi.Comment
		if json.Unmarshal(body, &c) == nil && c.ID != "" {
			return putComment(ctx, tx, c)
		}
	case "status", "link":
		var it orchapi.WorkItem
		if json.Unmarshal(body, &it) == nil && it.ID != "" {
			return putItem(ctx, tx, it, "", e.kind == "link" && it.Links != nil)
		}
	case "mail", "reply":
		var m orchapi.Mail
		if json.Unmarshal(body, &m) == nil && m.ID != "" {
			if err := putMail(ctx, tx, m); err != nil {
				return err
			}
			if e.kind == "reply" {
				_, err := tx.ExecContext(ctx, `UPDATE mail SET acked = 1 WHERE id = ? AND acked = 0`, e.item)
				return err
			}
		}
	case "ack":
		var ar orchapi.AckResponse
		if json.Unmarshal(body, &ar) == nil && ar.ID != "" && ar.AckedAt != nil {
			var js string
			if tx.QueryRowContext(ctx, `SELECT json FROM mail WHERE id = ?`, ar.ID).Scan(&js) == nil {
				var m orchapi.Mail
				if json.Unmarshal([]byte(js), &m) == nil {
					if m.AckedAt != nil && !ar.Acked {
						return nil // the taker's event already closed it: keep its taker name (a lost answer's is redacted)
					}
					m.AckedBy, m.AckedAt = ar.AckedBy, ar.AckedAt
					return putMail(ctx, tx, m)
				}
			}
		}
	}
	return nil
}

// settleStopsAfterAnswer and applyCommitsLost stand in for a power cut
// (tests): the daemon stops right after telling a caller, and what
// writeApply committed never reached the disk.
var settleStopsAfterAnswer, applyCommitsLost atomic.Bool

// noteLease keeps the presence cache's held keys in step with a claim or
// release the outbox carried (round B2's lost-lease check).
func (s *Service) noteLease(e outEntry, res api.WriteResult) {
	switch e.kind {
	case "claim", "lease":
		var cr orchapi.ClaimResponse
		if json.Unmarshal(res.Body, &cr) == nil && cr.Lease.Holder == e.as && cr.Lease.Key != "" {
			s.pmu.Lock()
			s.cache.claimedIn(cr.Lease.Project, e.as, cr.Lease.Key)
			s.cache.claimed(e.as, cr.Lease.Key)
			s.pmu.Unlock()
		}
	case "release":
		var rr orchapi.ReleaseResponse
		if json.Unmarshal(res.Body, &rr) == nil && rr.Released {
			s.pmu.Lock()
			s.cache.init()
			s.cache.released(e.as, rr.Key)
			s.pmu.Unlock()
		}
	case "status":
		// zm work status on a native item ends its claim like bd close does.
		var it orchapi.WorkItem
		if json.Unmarshal(res.Body, &it) == nil && it.ID != "" && !heldStatus(it.Status) {
			s.pmu.Lock()
			s.cache.endLease(e.as, it.ID)
			s.pmu.Unlock()
		}
	}
}

// gcEvery bounds how often a copy collects its outbox (it ran after every
// sync page: a quarter of the load test's CPU).
var gcEvery = time.Second

// collectOutbox is gcOutbox with a record: each refused write it dropped as
// no longer applying is logged.
func (s *Service) collectOutbox(ctx context.Context, rp *replica, project string) {
	s.logMoot(project, rp.gcOutbox(ctx))
}

func (s *Service) logMoot(project string, moot []mootRefusal) {
	for _, m := range moot {
		s.d.Log.Info("team: dropped a refused write that no longer applies", "project", project, "id", m.e.id,
			"kind", m.e.kind, "item", m.e.item, "as", m.e.as, "state", m.e.state, "error", m.e.errMsg, "why", m.why)
	}
}

// mootRefusal is a refused entry dropped early (dropMoot), and why.
type mootRefusal struct {
	e   outEntry
	why string
}

// mootKinds are the writes whose whole point is the item's state: once the
// item is done, a refusal of one is history, not something to act on.
var mootKinds = map[string]bool{"status": true, "claim": true, "lease": true, "release": true, "link": true}

// refusalMoot says why a refused entry no longer applies to its item as the
// copy shows it ("" while it still does): the item is done, or a refused
// status write asked for the status the item has anyway. A comment, mail or
// ack keeps its text for the record (keepSettled).
func refusalMoot(e outEntry, status string) string {
	if !mootKinds[e.kind] || status == "" {
		return ""
	}
	if e.kind == "status" {
		var req orchapi.StatusRequest
		if json.Unmarshal([]byte(e.body), &req) == nil && string(req.Status) == status {
			return "the item is " + status + " anyway"
		}
	}
	if status == string(orchapi.WorkDone) {
		return "the item is done"
	}
	return ""
}

// gcOutbox drops acked entries the copy now shows (a comment or mail stored
// under the write id, an ack whose mail shows taken, an item whose row
// reached what the write left), acked entries older than keepAcked, refused
// ones that no longer apply (refusalMoot, returned for the record) and the
// other refused ones past keepSettled, then recounts what status reports.
// At most once per gcEvery; it walks only the acked and refused rows (the
// state index) with point lookups.
func (rp *replica) gcOutbox(ctx context.Context) []mootRefusal {
	rp.mu.Lock()
	if time.Since(rp.lastGC) < gcEvery {
		rp.mu.Unlock()
		return nil
	}
	rp.lastGC = time.Now()
	rp.mu.Unlock()
	now := time.Now()
	rows, err := rp.db.QueryContext(ctx, `SELECT id, kind, item, seen, done FROM outbox WHERE state = ?`, api.OutboxAcked)
	if err != nil {
		return nil
	}
	type acked struct {
		id, kind, item, seen string
		done                 int64
	}
	var all []acked
	for rows.Next() {
		var a acked
		if rows.Scan(&a.id, &a.kind, &a.item, &a.seen, &a.done) == nil {
			all = append(all, a)
		}
	}
	rows.Close()
	var drop []string
	one := func(q string, args ...any) bool {
		var n int
		return rp.db.QueryRowContext(ctx, q, args...).Scan(&n) == nil && n > 0
	}
	for _, a := range all {
		if a.done >= store.Millis(now.Add(-keepAcked)) && !ackShown(one, a.id, a.kind, a.item, a.seen) {
			continue
		}
		drop = append(drop, a.id)
	}
	if len(drop) > 0 { // settled rows only: a lost delete is done again
		_ = rp.writeApply(ctx, func(tx *sql.Tx) error {
			for _, id := range drop {
				if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ? AND state = ?`, id, api.OutboxAcked); err != nil {
					return err
				}
			}
			return nil
		})
	}
	aged := int64(0)
	_ = rp.writeApply(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE state IN (?, ?) AND done < ?`, api.OutboxConflict, api.OutboxFailed, store.Millis(now.Add(-keepSettled)))
		if err == nil {
			aged, _ = res.RowsAffected()
		}
		return err
	})
	moot := rp.dropMoot(ctx)
	if len(moot) == 0 && aged > 0 {
		// Status (zm remotes) counts refused entries: without this they
		// read on until the next settle or restart.
		rp.countOutbox(ctx)
	}
	return moot
}

// dropMoot deletes the refused entries that no longer apply (refusalMoot)
// and recounts; it returns them for the record. The drain runs it after a
// refusal settles (the item may be done already, with no change to come),
// gcOutbox after the copy moved.
func (rp *replica) dropMoot(ctx context.Context) []mootRefusal {
	moot := rp.mootRefusals(ctx)
	if len(moot) == 0 {
		return nil
	}
	err := rp.writeApply(ctx, func(tx *sql.Tx) error {
		for _, m := range moot {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ? AND state = ?`, m.e.id, m.e.state); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil
	}
	rp.countOutbox(ctx)
	return moot
}

// mootRefusals is every refused entry that no longer applies (refusalMoot,
// linkGoneFor).
func (rp *replica) mootRefusals(ctx context.Context) []mootRefusal {
	rows, err := rp.db.QueryContext(ctx, `SELECT o.id, o.kind, o.body, o.as_addr, o.item, o.state, o.error, COALESCE(i.status, '')
 FROM outbox o LEFT JOIN items i ON i.id = o.item WHERE o.state IN (?, ?) AND o.item != ''`, api.OutboxConflict, api.OutboxFailed)
	if err != nil {
		return nil
	}
	var out, links []mootRefusal
	for rows.Next() {
		var e outEntry
		var status string
		if rows.Scan(&e.id, &e.kind, &e.body, &e.as, &e.item, &e.state, &e.errMsg, &status) != nil {
			continue
		}
		if why := refusalMoot(e, status); why != "" {
			out = append(out, mootRefusal{e: e, why: why})
		} else if e.kind == "link" {
			links = append(links, mootRefusal{e: e})
		}
	}
	rows.Close()
	for _, m := range links { // after the walk: linkGoneFor reads the copy too
		if m.why = linkGoneFor(ctx, rp, m.e); m.why != "" {
			out = append(out, m)
		}
	}
	return out
}

// ackShown: the copy shows what an acked write did (a comment or mail stored
// under the write id, an ack whose mail shows taken, an item whose row
// reached what the write left, a lease write on an item gone), so its
// overlay can go. one runs a SELECT 1.
func ackShown(one func(q string, args ...any) bool, id, kind, item, seen string) bool {
	switch {
	case kind == "comment" && one(`SELECT 1 FROM comments WHERE id = ?`, id):
	case (kind == "mail" || kind == "reply") && one(`SELECT 1 FROM mail WHERE id = ?`, id):
	case kind == "ack" && one(`SELECT 1 FROM mail WHERE id = ? AND acked = 1`, item):
	case item != "" && seen != "" && one(`SELECT 1 FROM items WHERE id = ? AND base >= ?`, item, seen):
	case item != "" && (kind == "claim" || kind == "release" || kind == "lease") && !one(`SELECT 1 FROM items WHERE id = ?`, item):
	default:
		return false
	}
	return true
}

// failLostAcks runs in a snapshot's transaction after a restore: an acked
// write the snapshot does not show was taken by the home before its restore
// and lost with it. It stops overlaying (no ghost of it) and is kept as
// failed, saying so; the ones it returns are told to their sessions.
func failLostAcks(ctx context.Context, tx *sql.Tx, now time.Time) ([]outEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+outCols+` FROM outbox WHERE state = ?`, api.OutboxAcked)
	if err != nil {
		return nil, err
	}
	var acked []outEntry
	for rows.Next() {
		e, err := scanOut(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		acked = append(acked, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	one := func(q string, args ...any) bool {
		var n int
		return tx.QueryRowContext(ctx, q, args...).Scan(&n) == nil && n > 0
	}
	var lost []outEntry
	for _, e := range acked {
		if ackShown(one, e.id, e.kind, e.item, e.seen) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, e.id); err != nil {
				return nil, err
			}
			continue
		}
		msg := "lost: the home took this " + e.kind + ", then was restored from a backup taken before it"
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = ?, error = ?, done = ? WHERE id = ?`, api.OutboxFailed, msg, store.Millis(now), e.id); err != nil {
			return nil, err
		}
		lost = append(lost, e)
	}
	return lost, nil
}

// lostRefusal says plainly that a write taken here cannot land there.
func lostRefusal(item, msg string) string {
	const say = "not applied: the home was restored from a backup taken before "
	if strings.HasPrefix(msg, say) {
		return msg
	}
	return say + item + " existed, so it no longer has it (" + msg + ")"
}

// unfenceLostRefusals runs before failLostAcks: a refusal for an item this
// restore lost, answered while the restore snapshot streamed (ids: during),
// settled as an obsolete copy answer (copy_fenced, unlisted, its session not
// told). The copy now records the loss, so it is current news: listed,
// saying why, and returned to tell its session. A refusal has no payload for
// the copy. One fenced before this snapshot began stays as it was.
func unfenceLostRefusals(ctx context.Context, tx *sql.Tx, lost, during []string) ([]outEntry, error) {
	if len(lost) == 0 || len(during) == 0 {
		return nil, nil
	}
	window := make(map[string]bool, len(during))
	for _, id := range during {
		window[id] = true
	}
	gone := make(map[string]bool, len(lost))
	for _, id := range lost {
		gone[id] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+outCols+` FROM outbox WHERE copy_fenced = 1 AND state IN (?, ?)`, api.OutboxFailed, api.OutboxConflict)
	if err != nil {
		return nil, err
	}
	var refused []outEntry
	for rows.Next() {
		e, err := scanOut(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if gone[e.item] && window[e.id] {
			refused = append(refused, e)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, e := range refused {
		refused[i].errMsg = lostRefusal(e.item, e.errMsg)
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET copy_fenced = 0, error = ? WHERE id = ?`, refused[i].errMsg, e.id); err != nil {
			return nil, err
		}
	}
	return refused, nil
}

// overlays is every unsettled write (pending, or acked and not yet in the
// copy), oldest first: reads apply them on top of the copy.
func (rp *replica) overlays(ctx context.Context, where string, args ...any) ([]outEntry, error) {
	q := `SELECT ` + outCols + ` FROM outbox WHERE copy_fenced = 0 AND state IN ('pending', 'acked')`
	if where != "" {
		q += " AND " + where
	}
	rows, err := rp.db.QueryContext(ctx, q+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outEntry
	for rows.Next() {
		e, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// entries lists the outbox for GET /v0/team/outbox (newest first).
func (rp *replica) entries(ctx context.Context, p api.ProjectRef, all bool, sc authz.Scope, reader string, personal bool) ([]api.OutboxEntry, error) {
	areaSQL, args := sc.AreaSQL(p.ID, "COALESCE(json_extract(visibility,'$.area'), (SELECT area FROM items WHERE items.id=outbox.item), '')", authz.Read)
	// Legacy mail rows lack a durable audience: fail closed rather than infer
	// public visibility from a missing parent or a caller-controlled body.
	q := `SELECT ` + outCols + ` FROM outbox WHERE copy_fenced = 0 AND (` + areaSQL + `) AND
 ((visibility IS NULL AND kind NOT IN ('mail','reply','ack')) OR
 (visibility IS NOT NULL AND (json_extract(visibility,'$.readers') IS NULL OR
 (? AND EXISTS (SELECT 1 FROM json_each(json_extract(visibility,'$.readers')) WHERE value=?)))))`
	args = append(args, personal, reader)
	if !all {
		q += ` AND state != 'acked'`
	}
	rows, err := rp.db.QueryContext(ctx, q+` ORDER BY seq DESC LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.OutboxEntry
	for rows.Next() {
		e, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		oe := api.OutboxEntry{ID: e.id, Project: p, Kind: e.kind, Item: e.item, As: e.as, State: e.state, Error: e.errMsg, Created: e.created}
		if e.body != "" {
			oe.Body = json.RawMessage(e.body)
		}
		if !e.done.IsZero() {
			d := e.done.UTC()
			oe.Done = &d
		}
		out = append(out, oe)
	}
	return out, rows.Err()
}

// handleOutbox is GET /v0/team/outbox[?project=&all=1].
func (s *Service) handleOutbox(r *http.Request) (any, error) {
	out := api.OutboxList{Entries: []api.OutboxEntry{}}
	name := r.URL.Query().Get("project")
	all := r.URL.Query().Get("all") == "1"
	sub := core.SubjectOf(r.Context())
	// Joined projects live in the remote registry, not Projects.Get. The
	// authenticated scope still gates each row before its listing limit.
	sc := sub.Scope
	for _, x := range s.List() {
		snap := x.Snapshot()
		if name != "" && !strings.EqualFold(name, snap.Project.Slug) && !strings.EqualFold(name, snap.Project.ID) {
			continue
		}
		rp := rep(x)
		if rp == nil {
			continue
		}
		reader := sub.Address
		// The socket owner uses its project identity on a joined authority.
		// Sessions retain their exact address, even with owner scope.
		if sub.Via == authz.ViaLocal && sub.Session == "" && sub.Address == s.d.Config.HumanAddress() {
			_, reader = s.callerAddr(r, snap)
		}
		es, err := rp.entries(r.Context(), snap.Project, all, sc, reader, authz.PersonalMailReader(sub))
		if err != nil {
			return nil, err
		}
		out.Entries = append(out.Entries, es...)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool { return out.Entries[i].ID > out.Entries[j].ID })
	return out, nil
}
