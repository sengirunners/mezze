package team

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Tunables (tests shorten them).
var (
	// SyncWait is how long one GET /v0/sync waits at the authority for a
	// change (long poll). A change reaches the replica as soon as it
	// commits, not on the next poll.
	SyncWait = 20 * time.Second
	// JoinSnapshotWait bounds how long zm join waits for the first snapshot.
	JoinSnapshotWait = 10 * time.Second
	// syncPage is the most changes asked for at once.
	syncPage = 1000
)

// copyOpenDelay slows opening a copy (tests: SlowCopyOpen).
var copyOpenDelay time.Duration

// startReplica opens x's copy and starts its loops (sync, drain, refresh).
// Never blocks on the network.
func (s *Service) startReplica(ctx context.Context, x *remote) {
	x.ReplicaMu.Lock()
	defer x.ReplicaMu.Unlock()
	if snap := x.Snapshot(); snap.State != api.RemoteOK {
		// Startup also removes a copy left by a crash during revocation.
		s.ReconcileRoleLocked(ctx, x)
		return
	}
	if s.d.Config.File.RemoteTasks {
		// It takes remote tasks: it holds the ops poll, so a task the
		// waiting line places here starts within a second (SPEC 8).
		s.WantsOps(x.Snapshot().Project.ID, "remote_tasks", true)
	}
	s.startReplicaLocked(ctx, x)
}

// Caller holds replicaMu; never called inline by observe from a copy loop.
func (s *Service) startReplicaLocked(ctx context.Context, x *remote) {
	if ctx.Err() != nil {
		return
	}
	snap := x.Snapshot()
	if snap.State != api.RemoteOK {
		return
	}
	x.Mu.Lock()
	if x.Copy != nil || x.Left || x.Recovery.Closing {
		x.Mu.Unlock()
		return
	}
	x.Mu.Unlock()
	if copyOpenDelay > 0 {
		time.Sleep(copyOpenDelay) // tests: a slow disk
	}
	rp, err := s.openReplica(ctx, snap.Project.ID)
	if err != nil {
		x.Mu.Lock()
		if !x.Left && !x.Recovery.Closing && x.R.State == api.RemoteOK {
			x.Recovery.Opening = true
		}
		x.Mu.Unlock()
		s.d.Log.Warn("team: open replica copy", "project", snap.Project.Slug, "err", err)
		return
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	x.Mu.Lock()
	if x.Left || x.Recovery.Closing || x.R.State != api.RemoteOK || ctx.Err() != nil {
		drop := x.Left || x.R.State != api.RemoteOK
		x.Mu.Unlock()
		cancel()
		saved, err := rp.close(drop)
		if drop {
			s.logUnsent(snap.Project.ID, saved, err)
		}
		return
	}
	x.Copy, x.StopCopy = rp, cancel
	x.Mu.Unlock()
	if s.rctx != nil {
		// Stop with the daemon, too.
		go func() {
			select {
			case <-s.rctx.Done():
				cancel()
			case <-lctx.Done():
			}
		}()
	}
	// teamlink runs the heartbeat and the ops poll.
	for _, loop := range []func(context.Context, *remote, *replica){s.syncLoop, s.drainLoop, s.refreshLoop, s.rosterLoop} {
		x.CopyWG.Add(1)
		go func() {
			defer x.CopyWG.Done()
			loop(lctx, x, rp)
		}()
	}
}

// stopReplica stops x's loops and closes its copy (drop: delete it too).
func (s *Service) stopReplica(x *remote, drop bool) {
	if !drop { // daemon close: no observer may admit another lifecycle job
		x.PublicationMu.Lock()
		x.Mu.Lock()
		x.Recovery.Closing = true
		x.Recovery.Ready = false
		x.Mu.Unlock()
		x.PublicationMu.Unlock()
	}
	x.ReplicaMu.Lock()
	s.stopReplicaLocked(x, drop)
	x.ReplicaMu.Unlock()
	if !drop {
		x.RoleWG.Wait()
	}
}

func (s *Service) stopReplicaLocked(x *remote, drop bool) {
	x.Mu.Lock()
	rp, cancel := copyOf(x), x.StopCopy
	x.Copy, x.StopCopy = nil, nil
	x.Mu.Unlock()
	if cancel != nil {
		cancel()
	}
	x.CopyWG.Wait()
	if rp != nil {
		rp.roster.clear()
	}
	id := x.Snapshot().Project.ID
	if rp == nil && drop {
		// No copy was open, but its file may still hold queued writes.
		saved, err := exportUnsentFile(context.Background(), s.d.Store, s.replicaPath(id))
		s.logUnsent(id, saved, err)
		return
	}
	saved, err := rp.close(drop)
	if drop {
		s.logUnsent(id, saved, err)
	}
}

// syncLoop keeps the copy current: a snapshot first (and after 410 resync
// or a scope change), then the changelog, long-polled.
func (s *Service) syncLoop(ctx context.Context, x *remote, rp *replica) {
	backoff := retryMin
	for ctx.Err() == nil {
		if err := teamlink.Ended(x.Snapshot()); err != nil {
			return // purged or revoked: the copy goes with remove/observe
		}
		rp.mu.Lock()
		need := !rp.ready || rp.resnap != rp.snapped
		rp.mu.Unlock()
		var err error
		if need {
			err = s.snapshotOnce(ctx, x, rp)
		} else {
			// Not reachable yet (just started, or a call failed): ask without
			// waiting, so the copy is ok as soon as the authority answers, not
			// when a quiet project's long poll runs out.
			wait := SyncWait
			if !rp.isReachable() {
				wait = 0
			}
			err = s.syncOnce(ctx, x, rp, wait)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// A redaction's compaction that met a busy checkpoint runs again.
			if cerr := rp.compactIfMarked(ctx); errors.Is(cerr, errScrubPending) {
				s.d.Log.Debug("replica: a redacted text is still to be scrubbed here", "project", x.Snapshot().Project.ID, "err", cerr)
			} else if cerr != nil {
				s.d.Log.Warn("replica compaction after a redaction", "project", x.Snapshot().Project.ID, "err", cerr)
			}
		}
		if err != nil {
			if api.IsCode(err, api.CodeResync) {
				rp.mu.Lock()
				rp.resnap++
				rp.mu.Unlock()
				continue
			}
			if !isAPIError(err) || api.IsCode(err, api.CodeUnavailable) {
				rp.markDown(err)
			} else {
				rp.mu.Lock()
				rp.lastErr = sanitize(err.Error())
				rp.mu.Unlock()
			}
			s.d.Log.Debug("team: sync", "project", x.Snapshot().Project.Slug, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			case <-rp.kickSync:
			}
			backoff = min(backoff*2, retryMax)
			continue
		}
		backoff = retryMin
		kick(rp.kickDrain) // reachable again: anything queued goes now
	}
}

// snapshotOnce takes a snapshot and applies it.
func (s *Service) snapshotOnce(ctx context.Context, x *remote, rp *replica) error {
	role := x.RoleReply() // the lifetime this snapshot's session notes speak for
	snap := role.Snap
	rp.mu.Lock()
	gen := rp.resnap
	rp.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, snap.URL+"/v0/sync/snapshot?project="+url.QueryEscape(snap.Project.ID), nil)
	if err != nil {
		return err
	}
	attempt, err := rp.beginSnapshot(cctx)
	if err != nil {
		return err
	}
	leases := s.prepareLeaseCache(x, snap)
	resp, err := s.Send(cctx, x, req, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return teamlink.Decode(resp, nil)
	}
	var lines []api.SnapshotLine
	var end *api.SnapshotLine
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var l api.SnapshotLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return errors.New("snapshot: bad line")
		}
		switch l.Kind {
		case api.SnapEnd:
			end = &l
		case api.SnapError:
			var er api.ErrorResponse
			if json.Unmarshal(l.Data, &er) == nil && er.Error != nil {
				return er.Error
			}
			return errors.New("snapshot failed at the authority")
		default:
			lines = append(lines, l)
		}
	}
	if err := sc.Err(); err != nil {
		return api.Errorf(api.CodeUnavailable, "snapshot cut short: %v", err)
	}
	if end == nil {
		return api.Errorf(api.CodeUnavailable, "snapshot cut short (no end line)")
	}
	var now time.Time
	if end.Now != nil {
		now = *end.Now
	}
	leases.now = now
	rp.mu.Lock()
	before, wasReady := rp.restore, rp.ready && rp.known
	rp.mu.Unlock()
	if err := rp.applySnapshotAttempt(ctx, lines, end.Cursor, now, gen, &attempt,
		snapshotAnswer{restore: end.Generation, leases: leases}); err != nil {
		return err
	}
	if wasReady && end.Generation != before {
		rp.mu.Lock()
		lost, acks, refused := len(rp.lost), rp.lostAcks, rp.refused
		rp.lostAcks, rp.refused = nil, nil
		rp.mu.Unlock()
		s.tellLostAcks(ctx, rp, role, attempt.committed, gen, acks, refused)
		rp.countOutbox(ctx)
		s.d.Log.Warn("team: the authority was restored from a backup; the copy now matches it", "project", snap.Project.Slug, "items_lost", lost, "writes_lost", len(acks))
	}
	s.collectOutbox(ctx, rp, snap.Project.Slug)
	s.d.Log.Debug("team: snapshot applied", "project", snap.Project.Slug, "rows", len(lines), "cursor", end.Cursor)
	return nil
}

// syncOnce asks for the changes after the cursor (waiting up to wait for
// one) and applies them, page after page while more follow.
func (s *Service) syncOnce(ctx context.Context, x *remote, rp *replica, wait time.Duration) error {
	for {
		snap := x.Snapshot()
		leases := s.prepareLeaseCache(x, snap)
		rp.mu.Lock()
		cursor, restore := rp.cursor, rp.restore
		rp.mu.Unlock()
		// gen= always, even empty: the authority checks it only when sent.
		q := url.Values{"project": {snap.Project.ID}, "since": {cursor}, "limit": {strconv.Itoa(syncPage)}, "gen": {restore}}
		if wait > 0 {
			q.Set("wait_ms", strconv.Itoa(int(wait/time.Millisecond)))
		}
		cctx, cancel := context.WithTimeout(ctx, wait+teamlink.CallTimeout)
		// A scope change seen by any other call (a heartbeat) ends this wait
		// (observe): its own 409 would be sent again with the new version and
		// wait on, and the copy would keep what left the scope meanwhile.
		rp.mu.Lock()
		resnap := rp.resnap != rp.snapped
		rp.endPoll = cancel
		rp.mu.Unlock()
		var sr orchapi.SyncResponse
		var err error
		if !resnap {
			err = s.CallRemote(cctx, x, http.MethodGet, "/v0/sync?"+q.Encode(), nil, &sr)
		}
		rp.mu.Lock()
		resnap = rp.resnap != rp.snapped
		rp.endPoll = nil
		rp.mu.Unlock()
		cancel()
		if resnap {
			return nil // the scope changed while waiting: snapshot first
		}
		if err != nil {
			return err
		}
		rp.mu.Lock()
		rp.backup = sr.Backup // the authority's off-site backup (nil from one without it)
		rp.started = sr.Started
		rp.mu.Unlock()
		leases.now = sr.Now
		fetch, err := rp.applyChanges(ctx, sr.Changes, sr.Cursor, sr.Now, leases)
		if err != nil {
			return err
		}
		if len(fetch) > 0 {
			rp.want(fetch...)
		}
		if len(sr.Changes) > 0 {
			s.collectOutbox(ctx, rp, snap.Project.Slug)
		}
		if !sr.More {
			return nil
		}
		wait = 0
	}
}

// drainLoop sends the outbox whenever something is queued or the
// authority answers again, backing off while it cannot take writes.
func (s *Service) drainLoop(ctx context.Context, x *remote, rp *replica) {
	backoff := retryMin
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rp.kickDrain:
		case <-t.C:
		}
		for ctx.Err() == nil {
			more, err := s.drainOnce(ctx, x, rp)
			if err != nil {
				s.d.Log.Debug("team: drain", "project", x.Snapshot().Project.Slug, "err", err)
				t.Reset(backoff)
				backoff = min(backoff*2, retryMax)
				break
			}
			backoff = retryMin
			if !more {
				t.Reset(time.Minute) // a safety net; kicks do the work
				break
			}
		}
	}
}

// want queues items to fetch whole.
func (rp *replica) want(ids ...string) {
	rp.rmu.Lock()
	for _, id := range ids {
		rp.refresh[id] = true
	}
	rp.rmu.Unlock()
}

func (rp *replica) takeWanted() []string {
	rp.rmu.Lock()
	defer rp.rmu.Unlock()
	var out []string
	for id := range rp.refresh {
		out = append(out, id)
		if len(out) == 50 {
			break
		}
	}
	for _, id := range out {
		delete(rp.refresh, id)
	}
	return out
}

// refreshLoop fetches items whole (links, comments) in the background: an
// item whose links changed, one that came into this replica's view, the
// items an import touched. Never on a caller's path.
func (s *Service) refreshLoop(ctx context.Context, x *remote, rp *replica) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, id := range rp.takeWanted() {
			snap := x.Snapshot()
			rp.snapshotMu.RLock()
			rp.mu.Lock()
			base := rp.cursor
			epoch := rp.snapshotEpoch
			hook := rp.testRefreshHook
			rp.mu.Unlock()
			rp.snapshotMu.RUnlock()
			var it orchapi.WorkItem
			cctx, cancel := context.WithTimeout(ctx, teamlink.CallTimeout)
			err := s.CallRemote(cctx, x, http.MethodGet, "/v0/work/"+url.PathEscape(id)+"?project="+url.QueryEscape(snap.Project.ID), nil, &it)
			cancel()
			var done func()
			if hook != nil && err == nil {
				done = hook(id)
			}
			switch {
			case err == nil && it.ID != "":
				if err := rp.putWhole(ctx, it, base, epoch); err != nil && !errors.Is(err, errMovedRefresh) {
					rp.want(id) // later: a redaction's refetch must land
				}
			case api.IsCode(err, api.CodeNotFound):
				done404 := rp.copyReplyHook("refresh404", id)
				if err := rp.dropItemAt(ctx, id, copyFence{epoch: epoch, base: base}); err != nil {
					rp.want(id)
				}
				if done404 != nil {
					done404()
				}
			case err != nil:
				rp.want(id) // later
			}
			if done != nil {
				done()
			}
		}
	}
}
