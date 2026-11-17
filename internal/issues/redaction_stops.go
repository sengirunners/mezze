package issues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// A moved issue's comment also lived at every earlier stop: that project's
// events, free pages and backups can hold its text. A redaction finishes at
// every stop a surviving move record names (both directions, in every local
// store, so a round trip's erased redirect does not hide its middle), and
// lists each stop in the registry so a restore there reapplies it. What zm
// cannot prove clean is written down as a gap and reported, never inferred:
// a stop whose text is gone from every store zm can read (it was redacted by
// an older zm that scrubbed one place), or a "moved" hop in the issue's
// history no record accounts for. No hash of the text is kept: a hash of a
// short secret confirms a guess, which a redaction must not allow.

var redactionStopsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "issues/0002_redaction_stops", Script: `
CREATE TABLE issue_redaction_stops (
	comment_id TEXT NOT NULL,
	project    TEXT NOT NULL, -- '' when no record says which
	issue_id   TEXT NOT NULL, -- or the unaccounted hop, old>new
	why        TEXT NOT NULL, -- '' clean, 'pending' until scrubbed, 'purge' until backups are, else the gap
	at         INTEGER NOT NULL,
	PRIMARY KEY (comment_id, project, issue_id)
);`},
}

// maxStops bounds the walk; a longer history is reported, not followed.
const maxStops = 32

const (
	stopClean   = ""                    // scrubbed here, or never held the text
	stopPending = "pending"             // listed, not scrubbed yet: a crash here leaves it pending
	stopPurge   = "purge"               // scrubbed, older backups not purged yet
	gapTextGone = "text gone"           // nothing left to scrub this stop with
	gapNoRecord = "no move record"      // a hop no surviving record places
	gapTooMany  = "too many stops"      // the walk stopped at maxStops
	gapRemote   = "not on this machine" // a stop in a project this daemon does not hold
)

// stop is one (project, id) the issue held, with when it arrived and last
// left (ms; 0 and MaxInt64 when no record says).
type stop struct {
	project, id   string
	arrived, left int64
}

// held says whether the stop can have held a comment created at created and
// redacted at redacted. Times are ms, so a tie counts as held: a false gap
// is safe, a false clean leaves a copy unscrubbed.
func (st stop) held(created, redacted int64) bool {
	return st.arrived <= redacted && st.left >= created
}

type redactionGap struct{ project, id, why string }

// farStop is a stop the walk names but cannot scrub (remote, or past maxStops).
type farStop struct {
	stop
	why string
}

// moveStops lists the stops of the issue live at l (l first), and the gaps
// in what the records prove. An issue that never moved costs one History.
func (s *Service) moveStops(ctx context.Context, l located) ([]stop, []farStop, []hop, error) {
	live := stop{project: l.project, id: l.id, arrived: math.MaxInt64, left: math.MaxInt64}
	hs, err := l.ps.History(ctx, l.id, istore.Areas{All: true})
	if err != nil {
		return nil, nil, nil, err
	}
	var hops []hop
	for _, h := range hs {
		var pair [2]*string
		if h.Kind == "moved" && json.Unmarshal(h.Fields["id"], &pair) == nil && pair[0] != nil && pair[1] != nil {
			hops = append(hops, hop{old: *pair[0], new: *pair[1], at: h.At.UnixMilli()})
		}
	}
	if len(hops) == 0 {
		return []stop{live}, nil, nil, nil
	}
	metas, err := s.st.Existing(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var stores []istore.Mover
	for _, m := range metas {
		if _, ok := s.d.Projects.File(m.Project); !ok {
			continue
		}
		ps, err := s.st.Project(ctx, m.Project, false)
		if errors.Is(err, istore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if mv, ok := ps.(istore.Mover); ok {
			stores = append(stores, mv)
		}
	}
	var far []farStop
	key := func(p, id string) string { return p + "\x00" + id }
	idx := map[string]int{key(live.project, live.id): 0}
	stops := []stop{live}
	for i := 0; i < len(stops); i++ {
		cur := stops[i]
		for _, mv := range stores {
			es, err := mv.MoveEdges(ctx, cur.project, cur.id)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, e := range es {
				at := e.At.UnixMilli()
				p, id := e.ToProject, e.ToID
				if key(e.FromProject, e.FromID) != key(cur.project, cur.id) {
					p, id = e.FromProject, e.FromID
				}
				j, seen := idx[key(p, id)]
				if !seen {
					// A stop not followed holds the comment only across the
					// one edge known: before it left for here, or after.
					one := stop{project: p, id: id, arrived: 0, left: at}
					if p == e.ToProject && id == e.ToID {
						one.arrived, one.left = at, math.MaxInt64
					}
					if _, local := s.d.Projects.File(p); !local {
						far = append(far, farStop{one, gapRemote})
						continue
					}
					if len(stops) == maxStops {
						far = append(far, farStop{one, gapTooMany})
						continue
					}
					j = len(stops)
					idx[key(p, id)] = j
					stops = append(stops, stop{project: p, id: id, arrived: math.MaxInt64})
				}
				// arrived: the earliest move in; left: the latest move out.
				if p == e.ToProject && id == e.ToID {
					stops[j].arrived = min(stops[j].arrived, at)
					stops[i].left = max(stops[i].left, at)
				} else {
					stops[j].left = max(stops[j].left, at)
					stops[i].arrived = min(stops[i].arrived, at)
				}
			}
		}
	}
	ids := map[string]bool{}
	for i := range stops {
		ids[stops[i].id] = true
		if stops[i].arrived == math.MaxInt64 {
			stops[i].arrived = 0 // no move in: it began here
		}
		if stops[i].left == 0 {
			stops[i].left = math.MaxInt64
		}
	}
	for _, f := range far {
		ids[f.id] = true // a record places it, though zm cannot scrub it
	}
	var lost []hop
	for _, h := range hops {
		if !ids[h.old] || !ids[h.new] {
			lost = append(lost, h)
		}
	}
	return stops, far, lost, nil
}

type hop struct {
	old, new string
	at       int64
}

// redactEverywhere is a redaction at the issue's live location l and every
// stop its records name: the registry first, then the store, then each
// stop's copies. who and at are the redaction's registry identity (a
// recovery keeps the original's); the held window ends at textLeftAt, not
// at. finish is the context the scrubs honor.
//
// Each stop's state lives in issue_redaction_stops, written with its
// registry row: pending until a scrub with the text finished there, so an
// attempt cut off anywhere leaves it pending, and a later one that finds
// the text gone records a gap instead of reporting success.
func (s *Service) redactEverywhere(ctx, finish context.Context, sub *authz.Subject, l located, cid, who string, at int64) (istore.Result, error) {
	stops, far, lost, err := s.moveStops(ctx, l)
	if err != nil {
		return istore.Result{}, err
	}
	// No row for a bogus comment or a write the caller may not make.
	if err := s.redactable(ctx, sub, l, cid); err != nil {
		return istore.Result{}, err
	}
	settled, whys := make([]bool, len(stops)), make([]string, len(stops))
	reg := s.d.Store.Registry()
	tx, err := reg.BeginTx(ctx, nil)
	if err != nil {
		return istore.Result{}, err
	}
	defer tx.Rollback()
	for i, st := range stops {
		var rowID, why string
		hasRow, hasStop := true, true
		if err := tx.QueryRowContext(ctx, `SELECT issue_id FROM issue_redactions WHERE project = ? AND comment_id = ?`, st.project, cid).Scan(&rowID); errors.Is(err, sql.ErrNoRows) {
			hasRow = false
		} else if err != nil {
			return istore.Result{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT why FROM issue_redaction_stops WHERE comment_id = ? AND project = ? AND issue_id = ?`, cid, st.project, st.id).Scan(&why); errors.Is(err, sql.ErrNoRows) {
			hasStop = false
		} else if err != nil {
			return istore.Result{}, err
		}
		// Clean: this zm finished here, or an older one listed and
		// scrubbed this very stop (it wrote no stop rows).
		settled[i] = hasStop && why == stopClean || !hasStop && hasRow && rowID == st.id
		whys[i] = why
		// The list first: a crash after the store write must not leave a
		// redacted comment off the list (a restore would bring its text back).
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO issue_redactions (project, comment_id, issue_id, who, at) VALUES (?, ?, ?, ?, ?)`,
			st.project, cid, st.id, who, at); err != nil {
			return istore.Result{}, err
		}
		if !hasStop && !settled[i] {
			if err := s.setStop(ctx, tx, cid, st.project, st.id, stopPending); err != nil {
				return istore.Result{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return istore.Result{}, err
	}
	if err := s.cutAt("listed"); err != nil {
		return istore.Result{}, err
	}
	res, _, err := s.write(ctx, sub, l, istore.Mutation{Op: istore.OpCommentRedact, CommentID: cid})
	if len(res.Redacted) == 0 && err != nil {
		return res, err
	}
	if err := s.cutAt("written"); err != nil {
		return res, err
	}
	var created int64
	if res.Comment != nil {
		created = res.Comment.CreatedAt.UnixMilli()
	}
	// The window ends when the text left the live store, not when an
	// attempt listed it: a stop the issue reached after a cut-off attempt
	// and before the store write may hold the text.
	redactedAt := s.textLeftAt(ctx, l, res, cid)
	errs := []error{err} // a failure after the commit: the texts still get scrubbed
	for i, st := range stops {
		// A pending or purge row is an earlier attempt that listed this stop
		// and may have lost the text before scrubbing it (an unmoved issue's
		// live stop included): held, so it ends scrubbed or a gap, never a
		// false clean. A stop with no row keeps its window.
		held := st.held(created, redactedAt) || whys[i] == stopPending || whys[i] == stopPurge
		// Also with no text: an earlier scrub or backup may still wait here.
		// An unsettled stop that held the text purges fresh: its first purge
		// may have failed before homes marked any backup.
		fresh := !settled[i] && held && (whys[i] == stopClean || whys[i] == stopPending || whys[i] == stopPurge)
		scrubbed, err := s.finishRedaction(finish, located{project: st.project, id: st.id}, cid, res.Redacted, fresh)
		errs = append(errs, err)
		// Clean only once the purge succeeded too (err nil): a failed purge
		// leaves the stop unsettled, so the next attempt purges again.
		done := scrubbed || whys[i] == stopPurge || !held
		switch {
		case settled[i]:
		case err == nil && done:
			errs = append(errs, s.setStop(ctx, reg, cid, st.project, st.id, stopClean))
		case err == nil:
			errs = append(errs, s.setStop(ctx, reg, cid, st.project, st.id, gapTextGone))
		case scrubbed:
			errs = append(errs, s.setStop(ctx, reg, cid, st.project, st.id, stopPurge))
		} // a failed scrub stays as it was: its retry, or the next attempt, decides
	}
	for _, f := range far {
		if f.held(created, redactedAt) {
			errs = append(errs, s.setStop(ctx, reg, cid, f.project, f.id, f.why))
		}
	}
	for _, h := range lost {
		if h.at >= created && h.at <= redactedAt { // a tie counts, as in held
			errs = append(errs, s.setStop(ctx, reg, cid, "", h.old+">"+h.new, gapNoRecord))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return res, err
	}
	return res, s.incompleteCleanup(ctx, sub, cid)
}

// textLeftAt is when the store overwrote comment cid's text: the time of
// its latest comment_redacted history row (this write's, or an earlier
// one's when this write had nothing left to overwrite; a move keeps the
// row and its time). Unknown (or unreadable: the texts at hand must still
// be scrubbed), it is now: a later end counts more stops as held, and a
// false gap is safe where a false clean is not.
func (s *Service) textLeftAt(ctx context.Context, l located, res istore.Result, cid string) int64 {
	latest := func(hs []orchapi.IssueChange) (at int64, ok bool) {
		for _, h := range hs {
			if h.Kind == "comment_redacted" && h.Comment == cid && h.At.UnixMilli() >= at {
				at, ok = h.At.UnixMilli(), true
			}
		}
		return at, ok
	}
	if at, ok := latest(res.Changes); ok {
		return at
	}
	if hs, err := l.ps.History(ctx, l.id, istore.Areas{All: true}); err == nil {
		if at, ok := latest(hs); ok {
			return at
		}
	}
	return s.now().UnixMilli()
}

// cutAt runs Service.redactCut, a test hook (nil in the daemon) that cuts a
// redaction off at a step ("listed": after the registry rows; "written":
// after the store).
func (s *Service) cutAt(step string) error {
	if s.redactCut == nil {
		return nil
	}
	return s.redactCut(step)
}

// redactable checks, before anything is listed, that cid is a comment of
// the issue at l and that sub may redact it there.
func (s *Service) redactable(ctx context.Context, sub *authz.Subject, l located, cid string) error {
	it, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return s.storeErr(err, l.id)
	}
	if err := s.canWrite(sub, l.project, it.Area, l.id); err != nil {
		return err
	}
	if !moderates(sub, l.project, it.Area) {
		return api.Errorf(api.CodeForbidden, "only an admin of the issue's area, or the machine owner, redacts a comment")
	}
	cs, err := l.ps.Comments(ctx, l.id, l.vis)
	if err != nil {
		return s.storeErr(err, l.id)
	}
	for _, c := range cs {
		if c.ID == cid {
			return nil
		}
	}
	return api.NotFound("comment", cid)
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// setStop records a stop's state. A clean stop stays clean, and a gap is
// not demoted to pending.
func (s *Service) setStop(ctx context.Context, db execer, cid, project, id, why string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO issue_redaction_stops (comment_id, project, issue_id, why, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (comment_id, project, issue_id) DO UPDATE SET why = excluded.why, at = excluded.at WHERE why != '' AND excluded.why != 'pending'`,
		cid, project, id, why, s.now().UnixMilli())
	return err
}

// incompleteCleanup reports a comment's recorded gaps (ids only, never
// text). A stop in a project sub cannot read is not named. A gap stays: zm
// never clears one on its own, since the copies it names (events, session
// holds, outbox rows, work comment copies, backups) are not all provable by
// id once a log is pruned. An operator clears one by acknowledging it
// (ackGap, redaction_ack.go).
func (s *Service) incompleteCleanup(ctx context.Context, sub *authz.Subject, cid string) error {
	gaps, err := s.redactionGaps(ctx, cid)
	if err != nil || len(gaps) == 0 {
		return err
	}
	var ev []string
	for _, g := range gaps {
		at, _ := gapName(sub, g)
		ev = append(ev, at+" ("+g.why+")")
	}
	return &incompleteError{api.Errorf(api.CodeConflict, "the comment is redacted, but zm cannot prove every copy of its text is gone: %s", strings.Join(ev, ", ")).
		WithHint("these stops' event logs and backups may still hold it; once one is dealt with by hand, zm issues ack-gap clears it")}
}

// incompleteError is a redaction that did everything it can: retrying will
// not change it.
type incompleteError struct{ error }

func (e *incompleteError) Unwrap() error { return e.error }

func (s *Service) redactionGaps(ctx context.Context, cid string) ([]redactionGap, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project, issue_id, why FROM issue_redaction_stops WHERE comment_id = ? AND why NOT IN ('', 'pending', 'purge')`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []redactionGap
	for rows.Next() {
		var g redactionGap
		if err := rows.Scan(&g.project, &g.id, &g.why); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].project+out[i].id < out[j].project+out[j].id })
	return out, rows.Err()
}
