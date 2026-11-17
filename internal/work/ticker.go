package work

import (
	"context"
	"fmt"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Start launches the lease ticker and the tracker mirrors' sync. It returns at once; the ticker stops
// when ctx ends or Close is called.
func (s *Service) Start(ctx context.Context) error {
	s.m.start() // every tracker starts syncing now, not on its first read
	ctx, s.stop = context.WithCancel(ctx)
	s.dropStrayLeases(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWriteBack(ctx)
	}()
	every := s.d.Config.LeaseTick()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Close stops the ticker and the tracker syncers, waits for them, and
// stops every serve-mode adapter (their process groups die with them).
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	s.m.close()
	s.pool.Close()
	return nil
}

// tick is one pass of the lease ticker: renew or release bound leases from
// session liveness, expire every lease past its deadline (event, and a notice
// to a live holder), then repair local item statuses that no
// lease backs any more. Errors are logged; the next tick retries.
func (s *Service) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.tickBound(ctx)
	s.tickExpire(ctx)
	s.tickRepair(ctx)
	for _, p := range s.d.Store.TeamProjects() {
		if db, err := s.d.Store.Project(authz.All(), authz.Read, p); err == nil {
			s.trackerConflicts(ctx, p, db)
		}
	}
}

func (s *Service) tickBound(ctx context.Context) {
	if orchcore.Sessions(s.d) == nil {
		return
	}
	// Bound leases, and default-TTL ones (bound if their session runs: SPEC 10).
	ls, err := s.queryLeasesAll(ctx, "bound = 1 OR ttl_set = 0 AND expires - renewed = ?", DefaultTTL.Milliseconds())
	if err != nil {
		s.warn("ticker: bound leases", "err", err)
		return
	}
	for _, l := range ls {
		sess, ok, err := orchcore.LookupSession(ctx, orchcore.Sessions(s.d), l.Holder)
		if err != nil { // never "gone": the lease stays until a tick that can tell
			s.warn("ticker: session lookup (lease kept)", "key", l.Key, "err", err)
			continue
		}
		switch {
		case !l.Bound && (!ok || sess.State != orchapi.StateRunning || !l.live(s.now())): // a TTL lease
		case !ok || sess.State == orchapi.StateEnded:
			if _, err := s.dropLocked(ctx, l, "session ended"); err != nil {
				s.warn("ticker: release ended holder", "key", l.Key, "err", err)
			}
		case sess.State == orchapi.StateRunning && l.live(s.now()): // an expired one is dropped below
			if err := s.renewBound(ctx, l); err != nil {
				s.warn("ticker: renew bound lease", "key", l.Key, "err", err)
			}
		}
		// idle / needs_human: not renewed; past its deadline it expires below.
	}
}

// renewBound renews (binding) l under the project's lease lock: never into the
// file a move is emptying, and after a move into its new one (leaseDB under the lock).
func (s *Service) renewBound(ctx context.Context, l lease) error {
	lk := s.locks.of(l.Project)
	lk.RLock()
	defer lk.RUnlock()
	now := s.now().UTC()
	_, err := s.leaseDB(l.Project).ExecContext(ctx, `UPDATE leases SET bound = 1, ttl_set = 0, expires = ?, renewed = ?
		WHERE project = ? AND area = ? AND key = ? AND holder = ? AND bound = ? AND expires = ?`,
		store.Millis(now.Add(s.d.Config.BoundLeaseTTL())), store.Millis(now),
		l.Project, l.Area, l.Key, l.Holder, boolInt(l.Bound), store.Millis(l.Expires))
	return err
}

func (s *Service) tickExpire(ctx context.Context) {
	ls, err := s.queryLeasesAll(ctx, "expires <= ?", store.Millis(s.now()))
	if err != nil {
		s.warn("ticker: expired leases", "err", err)
		return
	}
	for _, l := range ls {
		ok, err := s.dropLocked(ctx, l, "expired")
		if err != nil {
			s.warn("ticker: expire lease", "key", l.Key, "err", err)
			continue
		}
		if !ok {
			continue // renewed or released meanwhile
		}
		if !s.team(l.Project) { // a team project's lease_expired committed with the drop
			s.publish(ctx, orchapi.EventWorkLeaseExpired, l.Key, l.Project, l.Area, l.api(), l.Holder)
		}
		kind := "bound to a session that stopped running"
		if !l.Bound {
			kind = "not renewed"
		}
		s.tellHolder(ctx, l, fmt.Sprintf("zm: your lease on %s expired at %s (%s). Claim it again if you are still on it, and ack this.",
			l.Key, l.Expires.UTC().Format(time.RFC3339), kind))
	}
}

// tellHolder leaves a session that lost its lease a notice (kind notice: seen at
// its next prompt, wakes nobody), so it can claim again if it is still on the
// work. Nobody else is told: no person is asked to act on an expiry. The
// lease_expired event is the whole signal when the holder is gone or is not a
// session. It reports whether the notice went.
func (s *Service) tellHolder(ctx context.Context, l lease, body string) bool {
	if orchcore.Mail(s.d) == nil || orchcore.Sessions(s.d) == nil {
		return false
	}
	if a, err := api.ParseAddress(l.Holder); err != nil || a.Kind != api.KindSession {
		return false
	}
	ss, ok, err := orchcore.LookupSession(ctx, orchcore.Sessions(s.d), l.Holder)
	if err != nil {
		s.warn("ticker: expired lease's holder not told", "key", l.Key, "err", err)
		return false
	}
	if !ok || ss.State == orchapi.StateEnded {
		return false
	}
	p, ok := s.d.Projects.Lookup(l.Project)
	if !ok {
		return false
	}
	req := orchapi.SendMailRequest{To: l.Holder, Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent, Body: body}
	if _, isItem, err := s.keyItem(ctx, authz.All(), l.Project, l.Key); isItem && err == nil {
		req.Ref = l.Key
	}
	if _, err := orchcore.Mail(s.d).Send(context.WithoutCancel(ctx), s.d.System(), p, l.Area, req); err != nil {
		s.warn("ticker: tell holder of expired lease", "key", l.Key, "err", err)
		return false
	}
	return true
}

// tickRepair moves local items still "claimed" with no live lease back to
// open (the lease is authoritative; project.db follows it).
func (s *Service) tickRepair(ctx context.Context) {
	now := s.now()
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		rows, err := p.DB.QueryContext(ctx, `SELECT ref, area FROM work_items WHERE status = 'claimed'`)
		if err != nil {
			s.warn("ticker: repair", "project", p.ID, "err", err)
			continue
		}
		type ra struct{ ref, area string }
		var claimed []ra
		for rows.Next() {
			var x ra
			if rows.Scan(&x.ref, &x.area) == nil {
				claimed = append(claimed, x)
			}
		}
		rows.Close()
		for _, x := range claimed {
			cur, err := s.liveLease(ctx, p.ID, x.area, LocalSource+":"+x.ref)
			if err != nil || cur != nil {
				continue
			}
			if moved, err := setLocalStatusIf(ctx, p.DB, x.ref, orchapi.WorkClaimed, orchapi.WorkOpen, now); err != nil {
				s.warn("ticker: repair item", "ref", x.ref, "err", err)
			} else if moved {
				if l, isItem, err := s.keyItem(ctx, authz.All(), p.ID, LocalSource+":"+x.ref); isItem && err == nil {
					if it, err := getLocal(ctx, l.db, l.project, l.ref); err == nil {
						s.publishStatus(ctx, l, it)
					}
				}
			}
		}
	}
}

// EndItemLease implements orchcore.WorkService: the item's live lease goes,
// unless keep holds it. Its source's release runs as on any release (a
// native release changes nothing once the claim has ended).
func (s *Service) EndItemLease(ctx context.Context, project, id, keep, note string) error {
	l, isItem, err := s.keyItem(ctx, authz.All(), project, id)
	if a, _ := parseWorkID(id); isItem && api.IsCode(err, api.CodeNotFound) { // unmirrored: maybe a sync dropped an item that just moved out
		l, err = loc{project: project, id: canonID(a)}, nil
	}
	if !isItem || err != nil { // not a work item here: no lease names it
		return err
	}
	cur, err := itemLease(ctx, s.leaseReadDB(l.project), l.project, l.area, l.id, s.now())
	if err != nil || cur == nil || !cur.live(s.now()) || (keep != "" && cur.Holder == keep) {
		return err
	}
	// The GitHub link closing the issue is the work ending, not a lost lease
	// (a person's close still reads as lost): recorded first, so no heartbeat
	// finds the lease gone and not yet recorded. The holder hears why.
	byLink := keep == "" && note == "issue closed"
	if sub := core.SubjectOf(ctx); sub == nil || sub.Identity.Name != orchapi.IdentityGitHub {
		byLink = false
	}
	if byLink {
		s.finished.Add(l.project, cur.Holder, cur.Key, s.now())
	}
	dropped, err := s.dropLocked(ctx, *cur, note)
	if byLink && (err != nil || !dropped) {
		s.finished.Forget(l.project, cur.Holder, cur.Key)
	}
	if byLink && err == nil && dropped &&
		!s.tellHolder(ctx, *cur, fmt.Sprintf("zm: the GitHub link closed %s (a merged PR names it), so your lease on it ended. If that is wrong, reopen it and claim it again; otherwise ack this.", cur.Key)) {
		// Told nothing, so it must not read as finished either: it reads as lost.
		s.finished.Forget(l.project, cur.Holder, cur.Key)
	}
	return err
}

// ItemLeased implements orchcore.WorkService.
func (s *Service) ItemLeased(ctx context.Context, project, id string) (bool, error) {
	l, err := itemLease(ctx, s.leaseReadDB(project), project, "", id, s.now())
	return l != nil, err
}

// FinishedBy implements orchcore.LeaseHolders.
func (s *Service) FinishedBy(project, holder string, keys []string) []string {
	return s.finished.Of(project, holder, keys, s.now())
}
