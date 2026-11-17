package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/fencing"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// Lease TTL bounds (SPEC "Leases").
const (
	DefaultTTL = time.Hour
	MaxTTL     = 7 * 24 * time.Hour
	maxKeyLen  = 256
)

// lease is a row of registry.leases. Holder is never redacted here; use
// Service.leaseView before it reaches a caller.
type lease struct {
	Project, Area, Key, Holder string
	Bound                      bool
	TTLSet                     bool // the claim named its ttl_seconds: never bound by the ticker
	Note                       string
	Claimed, Renewed, Expires  time.Time
	Fence                      int64 // fencing token: new per claim, kept by renewals (0: from before fences)
}

func (l lease) live(now time.Time) bool { return l.Expires.After(now) }

func (l lease) api() orchapi.Lease {
	exp := l.Expires
	return orchapi.Lease{Key: l.Key, Project: l.Project, Area: l.Area, Holder: l.Holder, Bound: l.Bound, Note: l.Note,
		Claimed: l.Claimed, Renewed: l.Renewed, Expires: &exp, Fence: l.Fence}
}

const leaseCols = `project, area, key, holder, bound, note, claimed, renewed, expires, ttl_set, fence`

// leaseVals is the placeholders for one row of leaseCols.
const leaseVals = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// leaseArgs are l's values in leaseCols order.
func leaseArgs(l lease) []any {
	return []any{l.Project, l.Area, l.Key, l.Holder, boolInt(l.Bound), l.Note, store.Millis(l.Claimed), store.Millis(l.Renewed),
		store.Millis(l.Expires), boolInt(l.TTLSet), l.Fence}
}

// fenceOK is fencing.OK on cur at now; staleLease refuses a write under a lease that has gone.
func fenceOK(cur *lease, holder string, fence int64, now time.Time, live bool) bool {
	if cur == nil {
		return fencing.OK(nil, holder, fence, live)
	}
	return fencing.OK(&fencing.Lease{Fence: cur.Fence, Holder: cur.Holder, Live: cur.live(now)}, holder, fence, live)
}

var staleLease = orchapi.StaleLease

func scanLease(sc scanner) (lease, error) {
	var l lease
	var bound, ttlSet int
	var claimed, renewed, expires int64
	err := sc.Scan(&l.Project, &l.Area, &l.Key, &l.Holder, &bound, &l.Note, &claimed, &renewed, &expires, &ttlSet, &l.Fence)
	l.Bound, l.TTLSet = bound != 0, ttlSet != 0
	l.Claimed, l.Renewed, l.Expires = store.FromMillis(claimed), store.FromMillis(renewed), store.FromMillis(expires)
	return l, err
}

func getLease(ctx context.Context, q queryer, project, area, key string) (*lease, error) {
	l, err := scanLease(q.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE project = ? AND area = ? AND key = ?`, project, area, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// itemLease is the lease on work item key in project. A work item id is
// unique per project, so its lease is found whatever area it was claimed
// in: the item may have moved since (area rules or its labels changed). A
// live row comes first, then the one in area (the item's area now).
func itemLease(ctx context.Context, q queryer, project, area, key string, now time.Time) (*lease, error) {
	ls, err := queryLeases(ctx, q, "project = ? AND key = ? ORDER BY expires > ? DESC, area = ? DESC, expires DESC LIMIT 1",
		project, key, store.Millis(now), area)
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return &ls[0], nil
}

// moveLease files the lease rows of work item key in project under area,
// the item's area now (it moved: area rules or its labels changed), so area
// readers, expiry and its mail follow the item. An expired row already
// there gives way; a live one there stays, and so does the moving row.
func moveLease(ctx context.Context, db *sql.DB, project, key, area string, now time.Time) error {
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE project = ? AND area = ? AND key = ? AND expires <= ?`,
			project, area, key, store.Millis(now)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE OR IGNORE leases SET area = ? WHERE project = ? AND key = ? AND area != ?`,
			area, project, key, area)
		return err
	})
}

func queryLeases(ctx context.Context, q queryer, where string, args ...any) ([]lease, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// canonKey trims and checks a lease key; a work item id gets its canonical form.
func canonKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", api.Errorf(api.CodeBadRequest, "a lease needs a key (e.g. lock:migrations or a work item id)")
	}
	if len(key) > maxKeyLen || strings.ContainsAny(key, " \t\r\n") {
		return "", api.Errorf(api.CodeBadRequest, "bad lease key %q (at most %d characters, no whitespace)", key, maxKeyLen)
	}
	for _, p := range []string{"db:", "run:", "box:"} {
		if strings.HasPrefix(key, p) {
			// Reserved for run targets (SPEC 20): only their acquire makes these.
			return "", api.Errorf(api.CodeBadRequest, "lease keys starting %s are run-target handles: use zm target", p)
		}
	}
	if a, err := api.ParseAddress(key); err == nil && a.Kind == api.KindWork {
		return canonID(a), nil
	}
	return key, nil
}

func ttlOf(seconds int) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, api.Errorf(api.CodeBadRequest, "ttl_seconds must be positive")
	case seconds == 0:
		return DefaultTTL, nil
	case time.Duration(seconds)*time.Second > MaxTTL:
		return 0, api.Errorf(api.CodeBadRequest, "ttl_seconds %d is over the maximum (%d, 7 days)", seconds, int(MaxTTL/time.Second))
	}
	return time.Duration(seconds) * time.Second, nil
}

// takeReq is one atomic claim.
type takeReq struct {
	project, area, key, holder, note string
	bound, ttlSet                    bool // ttlSet: the caller named ttl_seconds
	ttl                              time.Duration
	renewOnly                        bool
	item                             bool  // key is a work item id: its lease is found in any area (itemLease)
	fence                            int64 // renew only the holder's live lease with this fence (0: any)
}

// takeRes is what a claim did. Conflict is the live lease of another holder
// (nothing changed); Replaced is an expired lease of another holder that the
// claim took over.
type takeRes struct {
	lease    lease
	renewed  bool
	conflict *lease
	replaced *lease
}

// take claims a key atomically (BEGIN IMMEDIATE on the project's lease
// database): free, expired, or already held by the same holder (renewal
// moves the deadline to now + ttl and keeps the claimed time). A team
// project's claims go through teamTake instead.
func (s *Service) take(ctx context.Context, r takeReq) (takeRes, error) {
	var res takeRes
	now := s.now().UTC().Truncate(time.Millisecond)
	err := store.Tx(ctx, s.leaseDB(r.project), func(tx *sql.Tx) error {
		var err error
		res, err = takeTx(ctx, tx, r, now)
		return err
	})
	if err == nil && res.conflict == nil {
		s.finished.Forget(r.project, r.holder, r.key)
	}
	return res, err
}

// takeTx is one claim's compare-and-set inside tx: the same code for
// registry.db and a team project's project.db.
func takeTx(ctx context.Context, tx *sql.Tx, r takeReq, now time.Time) (takeRes, error) {
	var res takeRes
	var cur *lease
	var err error
	if r.item {
		cur, err = itemLease(ctx, tx, r.project, r.area, r.key, now)
	} else {
		cur, err = getLease(ctx, tx, r.project, r.area, r.key)
	}
	if err != nil {
		return res, err
	}
	mine := cur != nil && cur.live(now) && cur.Holder == r.holder
	if !fenceOK(cur, r.holder, r.fence, now, true) {
		return res, staleLease(r.key, r.fence)
	}
	if cur != nil && cur.live(now) && !mine {
		res.conflict = cur
		return res, nil
	}
	// A project-level lease covers every area: it conflicts with a live
	// lease of another holder on the same key in any area, and an area
	// lease conflicts with a project-level one. Two areas stay
	// independent (a hidden area's lock neither conflicts nor shows).
	cross := "area = ''"
	if r.area == "" {
		cross = "area != ''"
	}
	others, err := queryLeases(ctx, tx, "project = ? AND key = ? AND "+cross+" AND holder != ? AND expires > ? ORDER BY area LIMIT 1",
		r.project, r.key, r.holder, store.Millis(now))
	if err != nil {
		return res, err
	}
	if len(others) > 0 {
		res.conflict = &others[0]
		return res, nil
	}
	if r.renewOnly && !mine {
		return res, api.Errorf(api.CodeConflict, "you hold no live lease on %s", r.key).
			WithHint("claim it first (without --renew)")
	}
	if cur != nil && !mine {
		res.replaced = cur
	}
	l := lease{Project: r.project, Area: r.area, Key: r.key, Holder: r.holder, Bound: r.bound, Note: r.note,
		Claimed: now, Renewed: now, Expires: now.Add(r.ttl)}
	if mine {
		res.renewed = true
		// A bound lease stays bound unless this claim names its own TTL.
		l.Claimed, l.Bound, l.Fence = cur.Claimed, r.bound || cur.Bound && !r.ttlSet, cur.Fence
		if l.Note == "" {
			l.Note = cur.Note
		}
	}
	if l.Fence == 0 { // a new lease, or one from before fences: a token of its own
		if l.Fence, err = fencing.Next(ctx, tx, r.project); err != nil {
			return res, err
		}
	}
	l.TTLSet = r.ttlSet && !l.Bound
	res.lease = l
	if cur != nil && cur.Area != r.area { // the item moved: its lease moves with it
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE project = ? AND area = ? AND key = ?`, cur.Project, cur.Area, cur.Key); err != nil {
			return res, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO leases (`+leaseCols+`) VALUES `+leaseVals+`
		ON CONFLICT (project, area, key) DO UPDATE SET holder = excluded.holder, bound = excluded.bound, note = excluded.note,
			claimed = excluded.claimed, renewed = excluded.renewed, expires = excluded.expires, ttl_set = excluded.ttl_set,
			fence = excluded.fence`,
		leaseArgs(l)...)
	return res, err
}

// undo removes a lease taken moments ago (its tracker refused the claim).
func (s *Service) undo(ctx context.Context, l lease) {
	_, _ = s.leaseDB(l.Project).ExecContext(context.WithoutCancel(ctx),
		`DELETE FROM leases WHERE project = ? AND area = ? AND key = ? AND holder = ? AND claimed = ?`,
		l.Project, l.Area, l.Key, l.Holder, store.Millis(l.Claimed))
}

// drop deletes exactly the lease l (same holder and deadline), so a lease
// renewed or retaken meanwhile is never removed. It reports whether it did.
func (s *Service) drop(ctx context.Context, l lease) (bool, error) {
	return deleteExact(ctx, s.leaseDB(l.Project), l)
}

// deleteExact deletes the lease l from q only if it is unchanged (same
// holder, deadline and fence) and reports whether it did.
func deleteExact(ctx context.Context, q queryer, l lease) (bool, error) {
	res, err := q.ExecContext(ctx,
		`DELETE FROM leases WHERE project = ? AND area = ? AND key = ? AND holder = ? AND expires = ? AND fence = ?`,
		l.Project, l.Area, l.Key, l.Holder, store.Millis(l.Expires), l.Fence)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// leaseView is l as sub may see it (holder redacted).
func (s *Service) leaseView(ctx context.Context, sub *authz.Subject, l lease) orchapi.Lease {
	v := l.api()
	v.Holder = s.d.Redact(ctx, sub, l.Holder)
	return v
}

// conflictErr refuses a claim on a key someone else holds; Data is their
// lease, redacted for the caller.
func (s *Service) conflictErr(ctx context.Context, sub *authz.Subject, cur lease) *api.Error {
	v := s.leaseView(ctx, sub, cur)
	return api.Errorf(api.CodeConflict, "%s is held by %s until %s", cur.Key, v.Holder, cur.Expires.Format(time.RFC3339)).
		WithHint("mail the holder: zm send " + cur.Key + " ...; an owner on this machine may: zm release --force " + cur.Key).
		WithData(v)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// adoptNative gives every native issue still claimed in project's native
// work source tr a TTL lease for its claimer (note "adopted"), when tr is
// bound: claims made before the binding (unleased) then
// expire like any other. Idempotent: an issue with a live lease is left alone.
// Team projects take leases at the authority's TeamTx path; they are not
// adopted here.
func (s *Service) adoptNative(ctx context.Context, project string, tr api.Tracker) (int, error) {
	if tr.Adapter != orchapi.AdapterNative || s.team(project) {
		return 0, nil
	}
	t, err := s.tracker(ctx, project, tr)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range []orchapi.WorkStatus{orchapi.WorkClaimed, orchapi.WorkInReview} {
		var l orchapi.WorkList
		if err := s.call(ctx, t, orchapi.VerbList, "", orchapi.AdapterListArgs{Status: st, Limit: maxLimit}, &l); err != nil {
			return n, err
		}
		for _, it := range l.Items {
			if it.Owner == "" {
				continue
			}
			s.fix(t, &it)
			lk := s.locks.of(project)
			lk.RLock()
			cur, err := itemLease(ctx, s.leaseReadDB(project), project, it.Area, it.ID, s.now())
			// Any lease row, live or expired, is left alone: an expired one is
			// the ticker's to release (adopting it would revive it).
			if err == nil && cur == nil {
				var res takeRes
				res, err = s.take(ctx, takeReq{project: project, area: it.Area, key: it.ID, holder: it.Owner,
					note: "adopted", ttl: DefaultTTL, item: true})
				if err == nil && res.conflict == nil {
					n++
					s.publish(ctx, orchapi.EventWorkClaimed, it.ID, project, it.Area, res.lease.api(), res.lease.Holder)
				}
			}
			lk.RUnlock()
			if err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
