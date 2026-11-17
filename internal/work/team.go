package work

// Team projects (SPEC 17.3): their leases live in their project.db, with the
// same schema and the same code as registry.db's, given a database handle.
// A claim is a compare-and-set at the authority in one transaction on the
// file's writer, with its events (commit-ordered ids, events.Bus.TeamTx);
// the tracker's assignee follows from the lease by background write-back,
// and a tracker that disagrees later releases the lease ("tracker
// conflict").

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/fencing"
	"github.com/sengirunners/mezze/internal/work/wbcause"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

var teamMigrations = []store.Migration{
	// The same table in every project.db; only a team project's is used
	// (zm project team moves the rows here; solo moves them back).
	{Kind: store.KindProject, Name: "work/0002_leases", Script: `
CREATE TABLE leases (
	project TEXT NOT NULL,
	area    TEXT NOT NULL DEFAULT '',
	key     TEXT NOT NULL,
	holder  TEXT NOT NULL,
	bound   INTEGER NOT NULL DEFAULT 0,
	note    TEXT NOT NULL DEFAULT '',
	claimed INTEGER NOT NULL,
	renewed INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	PRIMARY KEY (project, area, key)
);
CREATE INDEX leases_holder ON leases (holder);
CREATE INDEX leases_expires ON leases (expires);`},
	{Kind: store.KindProject, Name: "work/0003_leases_ttl_set", Script: `ALTER TABLE leases ADD COLUMN ttl_set INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindProject, Name: "work/0004_leases_fence", Script: fencing.Script},
}

var (
	_ orchcore.LeaseMover   = (*Service)(nil)
	_ orchcore.LeaseHolders = (*Service)(nil)
)

// HeldBy implements orchcore.LeaseHolders: the keys of project with a live
// lease held by holder (a replica's presence answer, SPEC 17.3).
func (s *Service) HeldBy(ctx context.Context, project, holder string) ([]string, error) {
	ls, err := queryLeases(ctx, s.leaseReadDB(project), "project = ? AND holder = ? AND expires > ? ORDER BY key",
		project, holder, store.Millis(s.now()))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Key)
	}
	return out, nil
}

// leaseLocks guards a project's lease moves: every lease operation holds
// its project's read lock, MoveLeases the write lock, so no claim lands in
// the file being emptied.
type leaseLocks struct {
	mu sync.Mutex
	m  map[string]*sync.RWMutex
}

func (l *leaseLocks) of(project string) *sync.RWMutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*sync.RWMutex{}
	}
	x, ok := l.m[project]
	if !ok {
		x = &sync.RWMutex{}
		l.m[project] = x
	}
	return x
}

// team reports whether project is a team project (its leases are in its
// project.db).
func (s *Service) team(project string) bool {
	return project != "" && project != api.ProjectNone && s.d.Store.IsTeam(project)
}

// leaseDB is where project's leases are written: a team project's writer
// connection, else registry.db.
func (s *Service) leaseDB(project string) *sql.DB {
	if s.team(project) {
		if w := s.d.Store.Writer(project); w != nil {
			return w
		}
	}
	return s.d.Store.Registry()
}

// leaseReadDB is where project's leases are read: a team project's readers,
// else registry.db.
func (s *Service) leaseReadDB(project string) *sql.DB {
	if s.team(project) {
		if db, err := s.d.Store.Project(authz.All(), authz.Read, project); err == nil {
			return db
		}
	}
	return s.d.Store.Registry()
}

// leaseDBs are registry.db and every team project's readers: what a lease
// query across projects (lists, the ticker) walks.
func (s *Service) leaseDBs() []*sql.DB {
	out := []*sql.DB{s.d.Store.Registry()}
	for _, id := range s.d.Store.TeamProjects() {
		if db, err := s.d.Store.Project(authz.All(), authz.Read, id); err == nil {
			out = append(out, db)
		}
	}
	return out
}

// queryLeasesAll runs where over every lease database. A solo project's
// rows are only in registry.db and a team project's only in its file
// (MoveLeases), but a row left behind by a crash mid-move is skipped by
// asking each file only for its own kind.
func (s *Service) queryLeasesAll(ctx context.Context, where string, args ...any) ([]lease, error) {
	var out []lease
	for i, db := range s.leaseDBs() {
		ls, err := queryLeases(ctx, db, where, args...)
		if err != nil {
			return nil, err
		}
		for _, l := range ls {
			if (i == 0) != s.team(l.Project) {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// MoveLeases implements orchcore.LeaseMover (zm project team|solo): it moves
// project's leases into its project.db (toTeam) or back to registry.db and
// flips the project's mode, while no lease operation on the project runs.
// To team, it refuses while a bound lease is live (its managed session's
// renewals are registry-side). The commit point is the registry's mode flag:
// rows are copied into the target first, then the mode flips and the source
// rows go in one transaction on the source's side; a crash before that leaves
// the copy, which the next start drops (dropStrayLeases).
func (s *Service) MoveLeases(ctx context.Context, project string, toTeam bool) (int, error) {
	lk := s.locks.of(project)
	lk.Lock()
	defer lk.Unlock()
	if s.team(project) == toTeam {
		return 0, nil
	}
	now := s.now()
	reg := s.d.Store.Registry()
	file, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return 0, err
	}
	src, dst := reg, file
	if !toTeam {
		src, dst = s.d.Store.Writer(project), reg
	}
	ls, err := queryLeases(ctx, src, "project = ?", project)
	if err != nil {
		return 0, err
	}
	// The target's tokens continue above the source's: a token is never
	// handed out twice, whichever file the project's leases live in.
	last, err := fencing.Last(ctx, src, project)
	if err != nil {
		return 0, err
	}
	if toTeam {
		for _, l := range ls {
			// A hook session's bound lease moves; its renewal waits for the move under the lease lock.
			if sess, ok := orchcore.Sessions(s.d).Lookup(ctx, l.Holder); !l.Bound || !l.live(now) || ok && !sess.Managed {
				continue
			}
			return 0, api.Errorf(api.CodeConflict, "%s holds a bound lease on %s (a managed session)", l.Holder, l.Key).
				WithHint("stop the session or release the item first, then zm project team again")
		}
	}
	err = store.Tx(ctx, dst, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE project = ?`, project); err != nil {
			return err
		}
		for _, l := range ls {
			if _, err := tx.ExecContext(ctx, `INSERT INTO leases (`+leaseCols+`) VALUES `+leaseVals, leaseArgs(l)...); err != nil {
				return err
			}
		}
		return fencing.Raise(ctx, tx, project, last)
	})
	if err != nil {
		return 0, err
	}
	if err := s.d.Projects.SetTeam(ctx, project, toTeam); err != nil {
		return 0, err
	}
	if !toTeam {
		src = s.d.Store.Writer(project) // the solo pool now; the rows are the same file
	}
	if _, err := src.ExecContext(ctx, `DELETE FROM leases WHERE project = ?`, project); err != nil {
		s.warn("move leases: drop the source rows", "project", project, "err", err)
	}
	return len(ls), nil
}

// dropStrayLeases removes lease rows a crashed move left in the file that
// is not the project's (a solo project's project.db, or registry.db rows of
// a team project). Start runs it before the ticker.
func (s *Service) dropStrayLeases(ctx context.Context) {
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		db := s.d.Store.Writer(p.ID)
		if s.team(p.ID) {
			db = s.d.Store.Registry()
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM leases WHERE project = ?`, p.ID); err != nil {
			s.warn("drop stray leases", "project", p.ID, "err", err)
		}
	}
}

// ---- claims and releases on a team project ----

// teamTake is take for a team project: the compare-and-set, a local item's
// open -> claimed, and the claimed (and replaced) events commit in one
// transaction on the file's writer.
func (s *Service) teamTake(ctx context.Context, r takeReq, l *loc) (takeRes, error) {
	var res takeRes
	moved := false
	now := s.now().UTC().Truncate(time.Millisecond)
	err := s.d.Events.TeamTx(ctx, r.project, func(tx *sql.Tx, emit func(events.Input)) error {
		var err error
		if res, err = takeTx(ctx, tx, r, now); err != nil || res.conflict != nil {
			return err
		}
		if l != nil && l.local() {
			if moved, err = setLocalStatusIf(ctx, tx, l.ref, orchapi.WorkOpen, orchapi.WorkClaimed, now); err != nil {
				return err
			}
		}
		if res.replaced != nil {
			emit(events.Input{Type: orchapi.EventWorkLeaseExpired, Subject: res.replaced.Key, Area: res.replaced.Area,
				Data: res.replaced.api(), Mentions: []string{res.replaced.Holder}})
		}
		emit(events.Input{Type: orchapi.EventWorkClaimed, Subject: r.key, Area: r.area, Data: res.lease.api(), Mentions: []string{res.lease.Holder}})
		return nil
	}, nil)
	if err == nil && res.conflict == nil {
		s.finished.Forget(r.project, r.holder, r.key)
	}
	// The status move (SPEC 10, G3) is its own event, right after the claim's.
	if err == nil && moved {
		if it, gerr := getLocal(ctx, l.db, l.project, l.ref); gerr == nil {
			s.publishStatus(ctx, *l, it)
		}
	}
	return res, err
}

// teamDrop deletes exactly the lease l in a team project, reopens a local
// item it claimed and publishes in the same transaction: released (note,
// "" = by its holder) or, for "expired", lease_expired. A tracker item's
// release goes to write-back. It reports whether it removed the lease.
func (s *Service) teamDrop(ctx context.Context, l lease, note string) (bool, error) {
	ctx = context.WithoutCancel(ctx)
	dropped, reopened := false, false
	now := s.now()
	err := s.d.Events.TeamTx(ctx, l.Project, func(tx *sql.Tx, emit func(events.Input)) error {
		if ok, err := deleteExact(ctx, tx, l); err != nil || !ok {
			return err
		}
		dropped = true
		if a, ok := parseWorkID(l.Key); ok && a.Source == LocalSource {
			var err error
			if reopened, err = setLocalStatusIf(ctx, tx, a.Ref, orchapi.WorkClaimed, orchapi.WorkOpen, now); err != nil {
				return err
			}
		}
		v := l.api()
		switch note {
		case "expired":
			emit(events.Input{Type: orchapi.EventWorkLeaseExpired, Subject: l.Key, Area: l.Area, Data: v, Mentions: []string{l.Holder}})
		default:
			if note != "" {
				v.Note = note
			}
			emit(events.Input{Type: orchapi.EventWorkReleased, Subject: l.Key, Area: l.Area, Data: v, Mentions: []string{l.Holder}})
		}
		return nil
	}, nil)
	if err == nil && dropped {
		s.writeBack(l.Project, l.Key, l.Holder, "", "", false)
	}
	if err == nil && reopened {
		if li, isItem, kerr := s.keyItem(ctx, authz.All(), l.Project, l.Key); isItem && kerr == nil {
			if it, gerr := getLocal(ctx, li.db, li.project, li.ref); gerr == nil {
				s.publishStatus(ctx, li, it)
			}
		}
	}
	return dropped, err
}

// ---- tracker write-back ----

// wbJob is one tracker write the lease implies: claim (assign the holder)
// or release.
type wbJob struct {
	project, key, holder, label string // label: see core.WithClaimLabel
	claim                       bool
	cause                       string // the claim this write is for (wbcause.Of), or ""
}

// writeBack queues a tracker item's claim or release for the background
// writer (SPEC 17.3: the tracker's assignee is derived from the lease). A
// local item needs none.
func (s *Service) writeBack(project, key, holder, label, cause string, claim bool) {
	a, ok := parseWorkID(key)
	if !ok || a.Source == LocalSource {
		return
	}
	select {
	case s.wb <- wbJob{project: project, key: canonID(a), holder: holder, label: label, claim: claim, cause: cause}:
	default:
		// Full: the mirror's next sync compares again (trackerConflicts); log it.
		s.warn("write-back queue full", "key", key)
	}
}

// runWriteBack writes queued claims and releases to their trackers. A
// tracker that refuses a claim (conflict: it holds the item for someone
// else) releases the lease with note "tracker conflict", publishes it and
// mails the holder.
func (s *Service) runWriteBack(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.wb:
			s.writeOne(ctx, j)
		}
	}
}

func (s *Service) writeOne(ctx context.Context, j wbJob) {
	a, _ := parseWorkID(j.key)
	tr, ok := orchcore.Tracker(s.d.Projects, j.project, a.Source)
	if !ok {
		return
	}
	t, err := s.tracker(ctx, j.project, tr)
	if err != nil {
		s.warn("write-back: tracker", "key", j.key, "err", err)
		return
	}
	var it orchapi.WorkItem
	if !j.claim {
		if err := s.call(ctx, t, orchapi.VerbRelease, j.key, orchapi.AdapterReleaseArgs{Holder: j.holder}, &it); err != nil {
			s.warn("write-back: release", "key", j.key, "err", err)
		} else if it.ID != "" {
			s.mirrorWriteBack(ctx, t, it, j.cause)
		}
		return
	}
	// The claim may have been released (or retaken) since it was queued.
	cur, err := s.liveLease(ctx, j.project, "", j.key)
	if err != nil || cur == nil || cur.Holder != j.holder {
		return
	}
	err = s.call(core.WithClaimLabel(ctx, j.label), t, orchapi.VerbClaim, j.key, orchapi.AdapterClaimArgs{Holder: j.holder}, &it)
	switch {
	case err == nil:
		s.mirrorWriteBack(ctx, t, it, j.cause)
	case api.IsCode(err, api.CodeConflict):
		s.trackerConflict(ctx, *cur, err.Error())
	default:
		s.warn("write-back: claim", "key", j.key, "err", err) // the next claim or sync tries again
	}
}

// mirrorWriteBack announces accepted write-back state before an incremental
// sync can mistake its silent mirror update for a state replicas already saw.
// Its status event names cause, the lease the write was for (wbcause).
func (s *Service) mirrorWriteBack(ctx context.Context, t tracked, it orchapi.WorkItem, cause string) {
	ctx = context.WithoutCancel(ctx)
	s.fix(t, &it)
	db := s.d.Store.Writer(t.project)
	if db == nil {
		return
	}
	a, _ := parseWorkID(it.ID)
	before, found, beforeErr := mirrorGet(ctx, db, t.project, t.tr.Prefix, a.Ref)
	s.mirrorPut(ctx, t, t.project, it)
	if beforeErr != nil || it.Title == "" || a.Source != t.tr.Prefix || !s.team(t.project) {
		return
	}
	err := s.d.Events.TeamTx(ctx, t.project, func(tx *sql.Tx, emit func(events.Input)) error {
		cur, err := scanMirror(tx.QueryRowContext(ctx, `SELECT `+mirrorCols+` FROM work_mirror WHERE source = ? AND ref = ?`, t.tr.Prefix, a.Ref), t.project, t.tr.Prefix)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if !cur.Updated.Equal(it.Updated) || cur.Status != it.Status || cur.Owner != it.Owner || cur.Area != it.Area || (found && cur.Area == before.Area && cur.Status == before.Status && cur.Owner == before.Owner) {
			return nil
		}
		l := loc{project: t.project, area: cur.Area, id: cur.ID, tr: &t.tr}
		raw := s.itemInTx(ctx, tx, l, cur)
		emit(events.Input{Type: orchapi.EventWorkStatusChanged, Subject: cur.ID, Area: cur.Area, Data: wbcause.Data(raw, cause), Mentions: []string{raw.Owner, raw.CreatedBy}, OwnerOnly: []string{wbcause.Key}})
		return nil
	}, nil)
	if err != nil {
		s.warn("write-back: status event", "key", it.ID, "err", err)
	}
}

// trackerConflict releases a team lease the tracker disagrees with (the
// item is assigned to someone else there): note "tracker conflict", the
// released event, and mail to the holder. The tracker's holder shows as a
// non-zm holder until someone claims through zm.
func (s *Service) trackerConflict(ctx context.Context, l lease, why string) {
	ctx = context.WithoutCancel(ctx)
	dropped, err := s.teamDropNoWriteBack(ctx, l)
	if err != nil || !dropped {
		return
	}
	if orchcore.Mail(s.d) == nil {
		return
	}
	p, ok := s.d.Projects.Lookup(l.Project)
	if !ok {
		return
	}
	body := fmt.Sprintf("zm: your claim on %s was released: its tracker holds it for someone else (%s). Check the tracker, then claim it again if it is yours.", l.Key, why)
	if len(body) > 1000 {
		body = body[:1000]
	}
	if _, err := orchcore.Mail(s.d).Send(ctx, s.d.System(), p, l.Area, orchapi.SendMailRequest{To: l.Holder, Body: body, Ref: l.Key}); err != nil {
		s.warn("tracker conflict: mail the holder", "key", l.Key, "err", err)
	}
}

// teamDropNoWriteBack is teamDrop with note "tracker conflict" and no
// release sent to the tracker (it already holds the item for someone else).
func (s *Service) teamDropNoWriteBack(ctx context.Context, l lease) (bool, error) {
	dropped := false
	err := s.d.Events.TeamTx(ctx, l.Project, func(tx *sql.Tx, emit func(events.Input)) error {
		if ok, err := deleteExact(ctx, tx, l); err != nil || !ok {
			return err
		}
		dropped = true
		v := l.api()
		v.Note = NoteTrackerConflict
		emit(events.Input{Type: orchapi.EventWorkReleased, Subject: l.Key, Area: l.Area, Data: v, Mentions: []string{l.Holder}})
		return nil
	}, nil)
	return dropped, err
}

// NoteTrackerConflict is the released note when a team project's tracker
// holds a claimed item for someone else.
const NoteTrackerConflict = "tracker conflict"

// trackerConflicts compares a team project's live tracker-item leases with
// what the mirror says after a sync: an item the tracker assigns to a
// different, non-empty owner releases its lease (trackerConflict). Our own
// write-back writes the holder, so an agreeing tracker never trips it; an
// owner the tracker reports in another form (a login) only counts once the
// write-back for the lease has had its chance (the lease is older than
// conflictGrace).
func (s *Service) trackerConflicts(ctx context.Context, project string, db *sql.DB) {
	if !s.team(project) {
		return
	}
	now := s.now()
	ls, err := queryLeases(ctx, s.leaseReadDB(project), "project = ? AND expires > ? AND key NOT LIKE ?", project, store.Millis(now), LocalSource+":%")
	if err != nil {
		return
	}
	for _, l := range ls {
		if now.Sub(l.Claimed) < conflictGrace {
			continue
		}
		a, ok := parseWorkID(l.Key)
		if !ok {
			continue
		}
		var owner string
		if err := db.QueryRowContext(ctx, `SELECT owner FROM work_mirror WHERE source = ? AND ref = ?`, a.Source, a.Ref).Scan(&owner); err != nil {
			continue
		}
		if owner != "" && owner != l.Holder {
			s.trackerConflict(ctx, l, "assigned to "+owner)
		}
	}
}

// conflictGrace is how long a team claim's write-back has before a tracker
// still showing another owner counts as a conflict (tests shorten it).
var conflictGrace = 30 * time.Second
