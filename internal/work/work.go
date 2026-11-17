// Package work is the work module: local work items and their history (per
// project, in project.db), tracker adapters (zm-work-<name> executables,
// one per project.toml [[trackers]] entry), leases with server-enforced
// deadlines (registry.db), the lease ticker, and the read-only inflight view.
// Owned by the work builder. See SPEC.md "Work items and leases" and "Work
// adapters".
//
// Every read goes through the caller's scope: items are located only in
// projects and areas the caller can read (locate), lists filter rows with
// AreaSQL / Scope.SQL, and every address returned (holder, owner,
// created_by, comment author) goes through d.Redact. Anything hidden answers
// exactly like something missing.
//
// The lease ticker (Start) runs every d.Config.LeaseTick(). For each bound
// lease it looks up the holder (d.Sessions.Lookup): ended or gone -> release
// (note "session ended"); running -> renew (expires = now +
// d.Config.BoundLeaseTTL()); idle / needs_human -> leave the deadline alone.
// Then every lease (TTL or bound) past its deadline expires: event
// dev.zm.work.lease_expired + a notice to a live holder via
// d.Mail.Send(d.System(), ...). Renewal never happens on an agent's prompt path.
package work

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/address"
	"github.com/sengirunners/mezze/internal/work/clean"
	"github.com/sengirunners/mezze/internal/work/finished"
	"github.com/sengirunners/mezze/internal/work/setup"
	"github.com/sengirunners/mezze/internal/work/wbcause"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Service implements orchcore.WorkService and the /v0/work and /v0/leases routes.
type Service struct {
	d    *core.Deps
	now  func() time.Time
	ads  adapterCache
	pool *adapter.Pool  // serve-mode adapters, one process each
	m    *mirrors       // tracker mirrors and their syncers
	adds setup.Prefixes // tracker binding lifecycle reservations

	locks leaseLocks // a project's lease operations vs. its move (team.go)
	wb    chan wbJob // team projects' tracker write-back queue

	finished finished.Leases // leases the GitHub link's close ended

	stop context.CancelFunc
	wg   sync.WaitGroup
}

var (
	_ orchcore.WorkService = (*Service)(nil)
	_ core.Module          = (*Service)(nil)
	_ core.Starter         = (*Service)(nil)
	_ core.Closer          = (*Service)(nil)
)

// New returns the work service.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: time.Now, ads: adapterCache{m: map[string]described{}}, pool: adapter.NewPool(), wb: make(chan wbJob, 4096)}
	s.m = newMirrors(s)
	d.AddScrubber(s.scrub)
	orchcore.AddOwedSource(d, s.owedSource())
	return s
}

// scrub overwrites a redaction's texts in this project's work comment
// copies filed under its subjects (comments sent through zm work comment
// are kept here as well as in their tracker).
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	db := s.d.Store.Writer(sc.Project)
	if db == nil {
		return nil
	}
	for _, subj := range sc.Subjects {
		rows, err := db.QueryContext(ctx, `SELECT id, body FROM work_comments WHERE item = ?`, subj)
		if err != nil {
			return err
		}
		type cm struct{ id, body string }
		var cs []cm
		for rows.Next() {
			var c cm
			if err := rows.Scan(&c.id, &c.body); err != nil {
				rows.Close()
				return err
			}
			cs = append(cs, c)
		}
		rows.Close()
		for _, c := range cs {
			// A copy holds the comment's text exactly; anything else under the
			// item is another comment and is left alone.
			for _, t := range sc.Texts {
				if c.body == t {
					if _, err := db.ExecContext(ctx, `UPDATE work_comments SET body = ? WHERE id = ?`, core.Redacted, c.id); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return s.scrubBackups(ctx, sc)
}

// List limits.
const (
	defaultLimit = 50
	maxLimit     = 5000 // high enough for `zm work export`
	maxBody      = 64 << 10
	maxTitle     = 500
)

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	}
	return n
}

// localVerbs are what the built-in local source supports.
var localVerbs = []string{orchapi.VerbList, orchapi.VerbGet, orchapi.VerbClaim, orchapi.VerbRelease, orchapi.VerbComment, orchapi.VerbSetStatus, orchapi.VerbLink}

// publish appends an event after the change committed. Data is the
// unredacted object; mentions are the addresses in it the bus redacts per
// subscriber.
func (s *Service) publish(ctx context.Context, typ, subject, project, area string, data any, mentions ...string) {
	if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: subject, Project: project,
		Area: area, Data: data, Mentions: mentions}); err != nil && s.d.Log != nil {
		s.d.Log.Warn("work: publish event", "type", typ, "err", err)
	}
}

// publishStatus publishes work.status_changed for an item a claim, release
// or expiry moved (open <-> claimed), as set_status does: the item without
// links or comments, rendered as the daemon sees it.
func (s *Service) publishStatus(ctx context.Context, l loc, it orchapi.WorkItem) {
	s.publishItem(ctx, orchapi.EventWorkStatusChanged, l, it)
}

// publishItem publishes an item event (created, status_changed, linked):
// the item without links or comments, as the daemon sees it. In a team
// project the item is read again inside the transaction that mints the
// event, under the file's write lock (SPEC 17.6): an event carries the
// item's state at its place in the changelog, so a replica applying events
// in order never ends on an older state (rendering after the commit, then
// minting, let a concurrent write's event slip in between with a newer
// state; the round C load test caught a copy left on "claimed").
func (s *Service) publishItem(ctx context.Context, typ string, l loc, it orchapi.WorkItem) {
	ctx = context.WithoutCancel(ctx)
	if !s.team(l.project) {
		raw, err := s.render(ctx, s.d.System(), l, it, false)
		if err != nil {
			s.warn("item event", "id", l.id, "err", err)
			return
		}
		s.publish(ctx, typ, l.id, l.project, l.area, eventItem(raw), raw.Owner, raw.CreatedBy)
		return
	}
	err := s.d.Events.TeamTx(ctx, l.project, func(tx *sql.Tx, emit func(events.Input)) error {
		raw := s.itemInTx(ctx, tx, l, it)
		emit(events.Input{Type: typ, Subject: l.id, Area: l.area, Data: raw, Mentions: []string{raw.Owner, raw.CreatedBy}})
		return nil
	}, func(evs []api.Event) {
		if typ == orchapi.EventWorkStatusChanged {
			for _, e := range evs {
				core.NoteStatusEvent(ctx, e.ID) // a claim that moved the item: the team write path's status seen
			}
		}
	})
	if err != nil {
		s.warn("item event", "type", typ, "id", l.id, "err", err)
	}
}

// itemInTx is it as an event carries it, read through tx (a team file's
// writer, holding its write lock): a local item's row and its live lease's
// holder as they are now. A tracker item keeps the adapter's answer, with
// the owner from its live lease.
func (s *Service) itemInTx(ctx context.Context, tx *sql.Tx, l loc, it orchapi.WorkItem) orchapi.WorkItem {
	if l.local() {
		if cur, err := getLocal(ctx, tx, l.project, l.ref); err == nil {
			it = cur
		}
	}
	it.Project, it.Area = l.project, l.area
	now := s.now()
	if cur, err := itemLease(ctx, tx, l.project, l.area, l.id, now); err == nil && cur != nil && cur.live(now) {
		it.Owner = cur.Holder
	} else if l.local() {
		it.Owner = ""
	}
	return eventItem(it)
}

func (s *Service) warn(msg string, args ...any) {
	if s.d.Log != nil {
		s.d.Log.Warn("work: "+msg, args...)
	}
}

// eventItem is an item as events carry it: links are left out (a link
// target may be in an area some subscribers of this item cannot read;
// readers fetch the item for its links).
func eventItem(it orchapi.WorkItem) orchapi.WorkItem {
	it.Links, it.Comments = nil, nil
	return it
}

// create makes a local item in the resolved project/area as sub.
func (s *Service) create(ctx context.Context, sub *authz.Subject, req orchapi.CreateWorkRequest) (orchapi.WorkItem, error) {
	t, err := s.d.WriteTarget(ctx, req.Area, authz.Write)
	if err != nil {
		return orchapi.WorkItem{}, err
	}
	req.Title = strings.TrimSpace(textclean.Clean(req.Title))
	req.Body = textclean.Clean(req.Body)
	if textclean.Blank(req.Title) || len(req.Title) > maxTitle || strings.ContainsAny(req.Title, "\n\r") {
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "a work item needs a one-line title (at most %d bytes)", maxTitle)
	}
	if len(req.Body) > maxBody {
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "body is over %d bytes", maxBody)
	}
	clean.Labels(req.Labels)
	for _, l := range req.Labels {
		if l = strings.TrimSpace(l); textclean.Blank(l) || len(l) > 64 || strings.ContainsAny(l, " \t\n,") {
			return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "bad label %q", l)
		}
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, t.Project.ID)
	if err != nil {
		return orchapi.WorkItem{}, err
	}
	it := orchapi.WorkItem{Project: t.Project.ID, Area: t.Area, Title: req.Title, Body: req.Body, CreatedBy: sub.Address,
		Labels: req.Labels}
	if s.team(it.Project) {
		// The row and its event in one transaction (SPEC 17.6): a restart
		// between them would leave an item no replica ever hears of.
		err := s.d.Events.TeamTx(context.WithoutCancel(ctx), it.Project, func(tx *sql.Tx, emit func(events.Input)) error {
			if err := insertLocal(ctx, tx, &it, s.now()); err != nil {
				return err
			}
			l := loc{project: it.Project, area: it.Area, id: it.ID, ref: strings.TrimPrefix(it.ID, LocalSource+":"), db: db}
			raw := s.itemInTx(ctx, tx, l, it)
			emit(events.Input{Type: orchapi.EventWorkCreated, Subject: it.ID, Area: it.Area, Data: raw, Mentions: []string{raw.CreatedBy}})
			return nil
		}, nil)
		if err != nil {
			return orchapi.WorkItem{}, err
		}
		return s.render(ctx, sub, loc{project: it.Project, area: it.Area, id: it.ID, ref: strings.TrimPrefix(it.ID, LocalSource+":"), db: db}, it, false)
	}
	if err := insertLocal(ctx, db, &it, s.now()); err != nil {
		return orchapi.WorkItem{}, err
	}
	l := loc{project: it.Project, area: it.Area, id: it.ID, ref: strings.TrimPrefix(it.ID, LocalSource+":"), db: db}
	s.publishItem(ctx, orchapi.EventWorkCreated, l, it)
	return s.render(ctx, sub, l, it, false)
}

// load returns the item at l as stored (local row or tracker answer).
func (s *Service) load(ctx context.Context, sub *authz.Subject, l loc) (orchapi.WorkItem, error) {
	if l.local() {
		return getLocal(ctx, l.db, l.project, l.ref)
	}
	return s.trackerItem(ctx, sub, l)
}

// render finishes an item for sub: live lease owner, redacted addresses,
// links (hidden targets collapsed per rel) and, when asked, its history.
func (s *Service) render(ctx context.Context, sub *authz.Subject, l loc, it orchapi.WorkItem, comments bool) (orchapi.WorkItem, error) {
	it.Project, it.Area = l.project, l.area
	cur, err := s.liveLease(ctx, l.project, l.area, l.id)
	if err != nil {
		return it, err
	}
	switch {
	case cur != nil:
		it.Owner = cur.Holder
	case l.local():
		it.Owner = ""
	}
	it.Owner = s.d.Redact(ctx, sub, it.Owner)
	it.CreatedBy = s.d.Redact(ctx, sub, it.CreatedBy)
	if it.Links, err = s.links(ctx, sub, l, it.Links); err != nil {
		return it, err
	}
	it.Comments = nil
	if comments {
		if it.Comments, err = s.comments(ctx, sub, l); err != nil {
			return it, err
		}
	}
	return it, nil
}

func (s *Service) comments(ctx context.Context, sub *authz.Subject, l loc) ([]orchapi.Comment, error) {
	cs, err := listComments(ctx, l.db, l.id)
	for i := range cs {
		cs[i].Author = s.d.Redact(ctx, sub, cs[i].Author)
	}
	return cs, err
}

// links merges stored links with a tracker's own, and hides every target sub
// cannot read: one {"rel", "hidden": true} per rel, so counts do not leak.
func (s *Service) links(ctx context.Context, sub *authz.Subject, l loc, fromTracker []orchapi.Link) ([]orchapi.Link, error) {
	stored, err := listLinks(ctx, l.db, l.id)
	if err != nil {
		return nil, err
	}
	for _, tl := range fromTracker {
		sl := storedLink{Rel: tl.Rel, Target: tl.Target}
		if !slices.Contains(stored, sl) {
			stored = append(stored, sl)
		}
	}
	var out []orchapi.Link
	hidden := map[string]bool{}
	for _, sl := range stored {
		tp := sl.TargetProject
		if tp == "" {
			tp = l.project
		}
		if !s.targetVisible(ctx, sub, tp, sl.Target) {
			if !hidden[sl.Rel] {
				hidden[sl.Rel] = true
				out = append(out, orchapi.Link{Rel: sl.Rel, Hidden: true})
			}
			continue
		}
		out = append(out, orchapi.Link{Rel: sl.Rel, Target: sl.Target, Project: sl.TargetProject})
	}
	return out, nil
}

// targetVisible reports whether sub may read a link target: a session (the
// session rule), a work item in project tp (it must be readable there), or
// anything else (a URL).
func (s *Service) targetVisible(ctx context.Context, sub *authz.Subject, tp, target string) bool {
	if isURL(target) {
		return true
	}
	a, err := api.ParseAddress(target)
	if err != nil {
		return false
	}
	switch a.Kind {
	case api.KindSession:
		return s.d.Visible(ctx, sub.Scope, a.String())
	case api.KindWork:
		db, err := s.d.Store.Project(sub.Scope, authz.Read, tp)
		if err != nil {
			return false
		}
		_, ok, err := s.locateIn(ctx, sub.Scope, tp, db, a)
		return err == nil && ok
	}
	return true
}

func isURL(s string) bool { return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") }

func (s *Service) get(ctx context.Context, sub *authz.Subject, l loc) (orchapi.WorkItem, error) {
	it, err := s.load(ctx, sub, l)
	if err != nil {
		return it, err
	}
	return s.render(ctx, sub, l, it, true)
}

// comment adds to an item's history (a tracker item's tracker gets it too).
// A replica write id (SPEC 17.6) makes retries return the stored comment.
func (s *Service) comment(ctx context.Context, sub *authz.Subject, l loc, body, id string, fence int64) (orchapi.Comment, error) {
	ctx = withActor(ctx, sub) // tracker writes are made for the caller
	if err := sub.Check(authz.Write, l.obj(), "work item", l.id); err != nil {
		return orchapi.Comment{}, err
	}
	if id != "" {
		var c orchapi.Comment
		var ms int64
		err := l.db.QueryRowContext(ctx, `SELECT id, item, author, body, created FROM work_comments WHERE id = ?`, id).Scan(&c.ID, &c.Item, &c.Author, &c.Body, &ms)
		switch {
		case err == nil && c.Item == l.id && c.Author == sub.Address && c.Body == textclean.Clean(body):
			c.Created = store.FromMillis(ms)
			return c, nil
		case err == nil:
			return orchapi.Comment{}, api.Errorf(api.CodeConflict, "write id %s is already used", id)
		case err != sql.ErrNoRows:
			return orchapi.Comment{}, err
		}
	}
	body = textclean.Clean(body)
	if textclean.Blank(body) {
		return orchapi.Comment{}, api.Errorf(api.CodeBadRequest, "empty comment")
	}
	if len(body) > maxBody {
		return orchapi.Comment{}, api.Errorf(api.CodeBadRequest, "comment is over %d bytes", maxBody)
	}
	if err := s.checkFence(ctx, s.leaseReadDB(l.project), sub, l, fence); err != nil {
		return orchapi.Comment{}, err
	}
	if !l.local() {
		t, err := s.tracker(ctx, l.project, *l.tr)
		if err == nil {
			err = s.call(ctx, t, orchapi.VerbComment, l.id, orchapi.AdapterCommentArgs{Body: body, Author: sub.Address, DeliveryID: id}, nil)
		}
		if err != nil {
			return orchapi.Comment{}, s.adapterErr(sub, t, l.id, err)
		}
		s.m.kick(l.project, l.tr.Prefix)
	} else if _, err := getLocal(ctx, l.db, l.project, l.ref); err != nil {
		return orchapi.Comment{}, err
	}
	if id == "" {
		id = ids.New()
	}
	c := orchapi.Comment{ID: id, Item: l.id, Author: sub.Address, Body: body, Created: s.now().UTC().Truncate(time.Millisecond)}
	if s.team(l.project) {
		// The comment and its event in one transaction (SPEC 17.6).
		err := s.d.Events.TeamTx(context.WithoutCancel(ctx), l.project, func(tx *sql.Tx, emit func(events.Input)) error {
			if l.local() { // the lease and the comment in one transaction (a tracker's has its comment already)
				if err := s.checkFence(ctx, tx, sub, l, fence); err != nil {
					return err
				}
			}
			if err := addComment(ctx, tx, c, l.area); err != nil {
				return err
			}
			if l.local() {
				touchLocal(ctx, tx, l.ref, s.now())
			}
			emit(events.Input{Type: orchapi.EventWorkCommented, Subject: l.id, Area: l.area, Data: c, Mentions: []string{c.Author}})
			return nil
		}, nil)
		return c, err
	}
	if err := addComment(ctx, l.db, c, l.area); err != nil {
		return orchapi.Comment{}, err
	}
	if l.local() {
		touchLocal(ctx, l.db, l.ref, s.now())
	}
	s.publish(ctx, orchapi.EventWorkCommented, l.id, l.project, l.area, c, c.Author)
	return c, nil
}

// setStatus is POST /v0/work/{id}/status.
//
// base, when set, is a team replica's: the write is refused (conflict) when
// the item's status changed after it. For a local item of a team project the
// check, the row change and its event are one transaction under the file's
// write lock, so no status change can slip between them; a tracker item's is
// checked before the adapter call (the tracker is outside the transaction).
//
// fence, when set, makes it a write under the caller's lease (checkFence).
func (s *Service) setStatus(ctx context.Context, sub *authz.Subject, l loc, st orchapi.WorkStatus, base string, fence int64) (orchapi.WorkItem, error) {
	ctx = withActor(ctx, sub) // tracker writes are made for the caller
	if err := sub.Check(authz.Write, l.obj(), "work item", l.id); err != nil {
		return orchapi.WorkItem{}, err
	}
	if !orchapi.ValidStatus(st) {
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "bad status %q (open, claimed, in_review, done)", st)
	}
	var it orchapi.WorkItem
	var err error
	if err := s.checkFence(ctx, s.leaseReadDB(l.project), sub, l, fence); err != nil {
		return it, err
	}
	if l.local() && s.team(l.project) {
		if st == orchapi.WorkClaimed {
			return it, api.Errorf(api.CodeBadRequest, "an item is claimed by taking a lease").WithHint("zm claim " + l.id)
		}
		return s.setTeamStatus(ctx, sub, l, st, base, fence)
	}
	if base != "" {
		if err := s.checkBase(ctx, l.db, l, base, sub.Address); err != nil {
			return it, err
		}
	}
	if l.local() {
		if st == orchapi.WorkClaimed {
			return it, api.Errorf(api.CodeBadRequest, "an item is claimed by taking a lease").WithHint("zm claim " + l.id)
		}
		if _, err = getLocal(ctx, l.db, l.project, l.ref); err != nil {
			return it, err
		}
		if err = setLocalStatus(ctx, l.db, l.ref, st, s.now()); err != nil {
			return it, err
		}
		it, err = getLocal(ctx, l.db, l.project, l.ref)
	} else {
		var t tracked
		if t, err = s.tracker(ctx, l.project, *l.tr); err == nil {
			err = s.call(ctx, t, orchapi.VerbSetStatus, l.id, orchapi.AdapterSetStatusArgs{Status: st}, &it)
		}
		if err != nil {
			return it, s.adapterErr(sub, t, l.id, err)
		}
		s.fix(t, &it)
		s.mirrorPut(ctx, t, l.project, it)
	}
	if err != nil {
		return it, err
	}
	s.publishItem(ctx, orchapi.EventWorkStatusChanged, l, it)
	return s.render(ctx, sub, l, it, false)
}

// checkBase refuses writer's field write whose base is older than the item's last status
// change (SPEC 17.6: no silent last writer wins), except one a write-back minted for
// writer's own live lease (wbcause): its own claim's, however late it ran.
func (s *Service) checkBase(ctx context.Context, q queryer, l loc, base, writer string) error {
	if q == nil {
		return nil
	}
	own, now := "", s.now()
	if cur, err := itemLease(ctx, q, l.project, l.area, l.id, now); err == nil && cur != nil && cur.live(now) && cur.Holder == writer {
		own = wbcause.Of(cur.Holder, cur.Claimed)
	}
	var last sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT max(id) FROM events WHERE subject = ? AND type = ? AND `+wbcause.NotOwn,
		l.id, orchapi.EventWorkStatusChanged, own, own).Scan(&last); err != nil {
		return err
	}
	if last.String > base {
		return api.Errorf(api.CodeConflict, "%s's status changed at the authority after this write was made; it was not applied", l.id).
			WithHint("zm work show " + l.id + " shows the current status; set it again if you still mean it")
	}
	return nil
}

// setTeamStatus is setStatus for a local item of a team project: base
// check, row and event in one TeamTx.
func (s *Service) setTeamStatus(ctx context.Context, sub *authz.Subject, l loc, st orchapi.WorkStatus, base string, fence int64) (orchapi.WorkItem, error) {
	ectx := context.WithoutCancel(ctx)
	err := s.d.Events.TeamTx(ectx, l.project, func(tx *sql.Tx, emit func(events.Input)) error {
		if _, err := getLocal(ectx, tx, l.project, l.ref); err != nil {
			return err
		}
		if err := s.checkFence(ectx, tx, sub, l, fence); err != nil {
			return err
		}
		if base != "" {
			if err := s.checkBase(ectx, tx, l, base, sub.Address); err != nil {
				return err
			}
		}
		if err := setLocalStatus(ectx, tx, l.ref, st, s.now()); err != nil {
			return err
		}
		raw := s.itemInTx(ectx, tx, l, orchapi.WorkItem{})
		emit(events.Input{Type: orchapi.EventWorkStatusChanged, Subject: l.id, Area: l.area, Data: raw, Mentions: []string{raw.Owner, raw.CreatedBy}})
		return nil
	}, func(evs []api.Event) {
		for _, e := range evs {
			core.NoteStatusEvent(ctx, e.ID)
		}
	})
	if err != nil {
		return orchapi.WorkItem{}, err
	}
	it, err := getLocal(ctx, l.db, l.project, l.ref)
	if err != nil {
		return it, err
	}
	return s.render(ctx, sub, l, it, false)
}

// link is POST /v0/work/{id}/link. A target sub cannot read is not_found,
// exactly like a missing one.
func (s *Service) link(ctx context.Context, sub *authz.Subject, sc authz.Scope, l loc, req orchapi.Link) (orchapi.WorkItem, error) {
	ctx = withActor(ctx, sub) // tracker writes are made for the caller
	if err := sub.Check(authz.Write, l.obj(), "work item", l.id); err != nil {
		return orchapi.WorkItem{}, err
	}
	rel, target := strings.ToLower(strings.TrimSpace(req.Rel)), strings.TrimSpace(req.Target)
	if !orchapi.ValidLinkRel(rel) {
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "bad link rel %q (%s)", req.Rel, strings.Join(orchapi.LinkRels, ", "))
	}
	if target == "" || len(target) > 2048 || strings.ContainsAny(target, " \t\r\n") {
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "bad link target %q", target)
	}
	sl := storedLink{Rel: rel, Target: target}
	switch a, err := api.ParseAddress(target); {
	case isURL(target):
		if rel == "session" {
			return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "a session link needs a session address (<machine>/<session8>)")
		}
	case err == nil && a.Kind == api.KindSession:
		sl.Target = a.String()
		if orchcore.Sessions(s.d) == nil {
			return orchapi.WorkItem{}, api.NotFound("session", target)
		}
		if _, ok := orchcore.Sessions(s.d).Lookup(ctx, sl.Target); !ok || !s.d.Visible(ctx, sub.Scope, sl.Target) {
			return orchapi.WorkItem{}, api.NotFound("session", target)
		}
	case err == nil && a.Kind == api.KindWork:
		if rel == "session" || rel == "pr" || rel == "url" {
			return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "rel %s needs a URL or a session address, not a work item", rel)
		}
		tl, err := s.locateTarget(ctx, sub, sc, l.project, target)
		if err != nil {
			return orchapi.WorkItem{}, hintLinkTarget(err)
		}
		if tl.project == l.project && tl.id == l.id {
			return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "an item cannot link to itself")
		}
		sl.Target = tl.id
		if tl.project != l.project {
			sl.TargetProject = tl.project
		}
	default:
		return orchapi.WorkItem{}, api.Errorf(api.CodeBadRequest, "a link target is a URL, a session address or a work item id, not %q", target)
	}
	if _, err := s.load(ctx, sub, l); err != nil {
		return orchapi.WorkItem{}, err
	}
	if s.team(l.project) && l.local() {
		// The link and its event in one transaction (SPEC 17.6).
		err := s.d.Events.TeamTx(context.WithoutCancel(ctx), l.project, func(tx *sql.Tx, emit func(events.Input)) error {
			if err := addLink(ctx, tx, l.id, l.area, sl, s.now()); err != nil {
				return err
			}
			touchLocal(ctx, tx, l.ref, s.now())
			raw := s.itemInTx(ctx, tx, l, orchapi.WorkItem{})
			emit(events.Input{Type: orchapi.EventWorkLinked, Subject: l.id, Area: l.area, Data: raw, Mentions: []string{raw.Owner, raw.CreatedBy}})
			return nil
		}, nil)
		if err != nil {
			return orchapi.WorkItem{}, err
		}
		it, err := s.load(ctx, sub, l)
		if err != nil {
			return it, err
		}
		return s.render(ctx, sub, l, it, false)
	}
	if err := addLink(ctx, l.db, l.id, l.area, sl, s.now()); err != nil {
		return orchapi.WorkItem{}, err
	}
	if l.local() {
		touchLocal(ctx, l.db, l.ref, s.now())
	} else if isURL(sl.Target) {
		// Trackers that keep links get URLs only: a work id or session
		// address could name something its readers cannot see.
		if t, err := s.tracker(ctx, l.project, *l.tr); err == nil && t.has(orchapi.VerbLink) {
			var lt orchapi.WorkItem
			if err := s.call(ctx, t, orchapi.VerbLink, l.id, orchapi.AdapterLinkArgs{Rel: sl.Rel, Target: sl.Target}, &lt); err != nil {
				s.warn("tracker link", "id", l.id, "err", err)
			} else if lt.ID != "" {
				s.fix(t, &lt)
				s.mirrorPut(ctx, t, l.project, lt)
			}
		}
	}
	it, err := s.load(ctx, sub, l)
	if err != nil {
		return it, err
	}
	s.publishItem(ctx, orchapi.EventWorkLinked, l, it)
	return s.render(ctx, sub, l, it, false)
}

// locateTarget finds a link target among the projects sc reads, preferring
// the linking item's own project when the id exists in several.
func (s *Service) locateTarget(ctx context.Context, sub *authz.Subject, sc authz.Scope, project, id string) (loc, error) {
	if db, err := s.d.Store.Project(sc, authz.Read, project); err == nil {
		if a, ok := parseWorkID(id); ok {
			if l, ok, err := s.locateIn(ctx, sc, project, db, a); err != nil {
				return loc{}, err
			} else if ok {
				return l, nil
			}
		}
	}
	return s.locate(ctx, sub, sc, id)
}

// ---- claims ----

// holderOf is who a claim by sub is held by, and whether it is bound (renewed
// while the session runs, dropped when it ends): a claim by a live session
// asking no TTL of its own (as bd's). A named TTL wins for every session.
func (s *Service) holderOf(ctx context.Context, sub *authz.Subject, ttlSeconds int) (string, bool) {
	if ttlSeconds == 0 && sub.Session != "" && orchcore.Sessions(s.d) != nil {
		if sess, ok := orchcore.Sessions(s.d).Lookup(ctx, sub.Session); ok && sess.State != orchapi.StateEnded {
			return sub.Address, true
		}
	}
	return sub.Address, false
}

// claimItem takes or renews an item lease; bind names a spawned session.
// Local items become claimed; tracker conflicts undo the lease.
func (s *Service) claimItem(ctx context.Context, sub *authz.Subject, l loc, req orchapi.ClaimRequest, bind string) (orchapi.ClaimResponse, error) {
	ctx = withActor(ctx, sub) // tracker writes are made for the caller
	if err := sub.Check(authz.Write, l.obj(), "work item", l.id); err != nil {
		return orchapi.ClaimResponse{}, err
	}
	req.Note = textclean.Clean(req.Note)
	holder, bound := s.holderOf(ctx, sub, req.TTLSeconds)
	if bind != "" {
		holder, bound = bind, true
	}
	ttl, err := ttlOf(req.TTLSeconds)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if bound {
		ttl = s.d.Config.BoundLeaseTTL()
	}
	if len(req.Note) > 500 {
		return orchapi.ClaimResponse{}, api.Errorf(api.CodeBadRequest, "note is over 500 bytes")
	}
	var t tracked
	if l.local() {
		cur, err := getLocal(ctx, l.db, l.project, l.ref)
		if err != nil {
			return orchapi.ClaimResponse{}, err
		}
		if cur.Status == orchapi.WorkDone {
			return orchapi.ClaimResponse{}, api.Errorf(api.CodeConflict, "%s is done", l.id).WithHint("reopen it first: zm work status " + l.id + " open")
		}
	} else if t, err = s.tracker(ctx, l.project, *l.tr); err != nil {
		return orchapi.ClaimResponse{}, s.adapterErr(sub, t, l.id, err)
	}
	lk := s.locks.of(l.project)
	lk.RLock()
	defer lk.RUnlock()
	tr := takeReq{project: l.project, area: l.area, key: l.id, holder: holder, note: req.Note, bound: bound, ttlSet: req.TTLSeconds > 0,
		ttl: ttl, renewOnly: req.Renew, item: true, fence: req.Fence}
	if s.team(l.project) {
		return s.teamClaimItem(ctx, sub, l, tr)
	}
	res, err := s.take(ctx, tr)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if res.conflict != nil {
		return orchapi.ClaimResponse{}, s.conflictErr(ctx, sub, *res.conflict)
	}
	var it orchapi.WorkItem
	moved := false
	if l.local() {
		if moved, err = setLocalStatusIf(ctx, l.db, l.ref, orchapi.WorkOpen, orchapi.WorkClaimed, s.now()); err != nil {
			return orchapi.ClaimResponse{}, err
		}
		it, err = getLocal(ctx, l.db, l.project, l.ref)
	} else {
		// A tracker item's status before the claim is only what its mirror
		// last saw (it may be stale or missing), so a fresh claim always
		// publishes the adapter's answer; a renewal only when that differs
		// from the mirror. Subscribers may see a status they already have,
		// never miss a move.
		var before orchapi.WorkStatus
		if was, ok, _ := mirrorGet(ctx, l.db, l.project, l.tr.Prefix, l.ref); ok {
			before = was.Status
		}
		if res.replaced != nil && res.replaced.Holder != holder { // end the expired holder's tracker claim, or the tracker refuses this one
			if rerr := s.call(ctx, t, orchapi.VerbRelease, l.id, orchapi.AdapterReleaseArgs{Holder: res.replaced.Holder}, &orchapi.WorkItem{}); rerr != nil {
				s.warn("release the replaced holder's claim", "id", l.id, "err", rerr)
			}
		}
		args := orchapi.AdapterClaimArgs{Holder: holder, Note: req.Note, TTLSeconds: int(ttl / time.Second)}
		if err = s.call(ctx, t, orchapi.VerbClaim, l.id, args, &it); err != nil {
			switch {
			case !res.renewed:
				s.undo(ctx, res.lease)
			case api.IsCode(err, api.CodeConflict):
				// A renewal the source refuses (closed or taken meanwhile):
				// the lease no longer stands for a claim, so it goes; a
				// transient failure keeps it.
				if _, derr := s.dropAndAfter(context.WithoutCancel(ctx), res.lease, "claim refused"); derr != nil {
					s.warn("drop refused renewal", "id", l.id, "err", derr)
				}
			}
			return orchapi.ClaimResponse{}, s.adapterErr(sub, t, l.id, err)
		}
		s.fix(t, &it)
		s.mirrorPut(ctx, t, l.project, it)
		moved = it.Status != "" && (!res.renewed || it.Status != before)
	}
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if res.replaced != nil {
		s.publish(ctx, orchapi.EventWorkLeaseExpired, res.replaced.Key, res.replaced.Project, res.replaced.Area, res.replaced.api(), res.replaced.Holder)
	}
	s.publish(ctx, orchapi.EventWorkClaimed, l.id, l.project, l.area, res.lease.api(), res.lease.Holder)
	if moved {
		s.publishStatus(ctx, l, it)
	}
	out, err := s.render(ctx, sub, l, it, false)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	return orchapi.ClaimResponse{Lease: s.leaseView(ctx, sub, res.lease), Item: &out}, nil
}

// teamClaimItem is claimItem for a team project (SPEC 17.3): the lease at
// the authority is the claim, taken by compare-and-set with its events in
// one transaction; a tracker item's assignee follows by write-back.
func (s *Service) teamClaimItem(ctx context.Context, sub *authz.Subject, l loc, tr takeReq) (orchapi.ClaimResponse, error) {
	res, err := s.teamTake(ctx, tr, &l)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if res.conflict != nil {
		return orchapi.ClaimResponse{}, s.conflictErr(ctx, sub, *res.conflict)
	}
	if !l.local() && !res.renewed {
		if res.replaced != nil && res.replaced.Holder != tr.holder { // end the expired holder's tracker claim first (one FIFO worker)
			s.writeBack(l.project, l.id, res.replaced.Holder, "", wbcause.Of(res.lease.Holder, res.lease.Claimed), false)
		}
		s.writeBack(l.project, l.id, tr.holder, core.ClaimLabelOf(ctx), wbcause.Of(res.lease.Holder, res.lease.Claimed), true) // its status events are this claim's own (checkBase)
	}
	it, err := s.load(ctx, sub, l)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if l.local() {
		it.Status = orchapi.WorkClaimed
	}
	out, err := s.render(ctx, sub, l, it, false)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	return orchapi.ClaimResponse{Lease: s.leaseView(ctx, sub, res.lease), Item: &out}, nil
}

// dropLocked is dropAndAfter under the project's lease lock.
func (s *Service) dropLocked(ctx context.Context, l lease, note string) (bool, error) {
	lk := s.locks.of(l.Project)
	lk.RLock()
	defer lk.RUnlock()
	return s.dropAndAfter(ctx, l, note)
}

// claimKey is POST /v0/leases: a work item id of the resolved project is
// claimed as that item; otherwise it is a plain lock.
func (s *Service) claimKey(ctx context.Context, sub *authz.Subject, req orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	t, err := s.d.WriteTarget(ctx, "", authz.Write)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	key, err := canonKey(req.Key)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if l, isItem, err := s.keyItem(ctx, sub.Scope, t.Project.ID, key); isItem {
		if err != nil {
			return orchapi.ClaimResponse{}, err
		}
		return s.claimItem(ctx, sub, l, req, "")
	}
	req.Note = textclean.Clean(req.Note)
	holder, bound := s.holderOf(ctx, sub, req.TTLSeconds)
	ttl, err := ttlOf(req.TTLSeconds)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if bound {
		ttl = s.d.Config.BoundLeaseTTL()
	}
	lk := s.locks.of(t.Project.ID)
	lk.RLock()
	defer lk.RUnlock()
	tr := takeReq{project: t.Project.ID, area: t.Area, key: key, holder: holder, note: req.Note, bound: bound, ttlSet: req.TTLSeconds > 0,
		ttl: ttl, renewOnly: req.Renew, fence: req.Fence}
	if s.team(t.Project.ID) {
		res, err := s.teamTake(ctx, tr, nil)
		if err != nil {
			return orchapi.ClaimResponse{}, err
		}
		if res.conflict != nil {
			return orchapi.ClaimResponse{}, s.conflictErr(ctx, sub, *res.conflict)
		}
		return orchapi.ClaimResponse{Lease: s.leaseView(ctx, sub, res.lease)}, nil
	}
	res, err := s.take(ctx, tr)
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	if res.conflict != nil {
		return orchapi.ClaimResponse{}, s.conflictErr(ctx, sub, *res.conflict)
	}
	if res.replaced != nil {
		s.publish(ctx, orchapi.EventWorkLeaseExpired, res.replaced.Key, res.replaced.Project, res.replaced.Area, res.replaced.api(), res.replaced.Holder)
	}
	s.publish(ctx, orchapi.EventWorkClaimed, key, t.Project.ID, t.Area, res.lease.api(), res.lease.Holder)
	return orchapi.ClaimResponse{Lease: s.leaseView(ctx, sub, res.lease)}, nil
}

// dropAndAfter drops exactly l and runs its side effects (afterRelease); a
// team project's in one transaction with its event (teamDrop).
func (s *Service) dropAndAfter(ctx context.Context, l lease, note string) (bool, error) {
	if s.team(l.Project) {
		return s.teamDrop(ctx, l, note)
	}
	ok, err := s.drop(ctx, l)
	if err == nil && ok {
		s.afterRelease(ctx, l, note)
	}
	return ok, err
}

// afterRelease runs the side effects of a lease leaving (released, ended or
// expired): a local item still "claimed" goes back to open; a tracker item
// is released in its tracker (best effort); a released event unless note
// says otherwise ("" = released by its holder).
func (s *Service) afterRelease(ctx context.Context, cur lease, note string) {
	ctx = context.WithoutCancel(ctx)
	if a, ok := parseWorkID(cur.Key); ok && a.Source != LocalSource {
		// A tracker item: released in its tracker whatever its mirror knows.
		if tr, ok := orchcore.Tracker(s.d.Projects, cur.Project, a.Source); ok {
			if t, err := s.tracker(ctx, cur.Project, tr); err == nil {
				var it orchapi.WorkItem
				if err := s.call(ctx, t, orchapi.VerbRelease, canonID(a), orchapi.AdapterReleaseArgs{Holder: cur.Holder}, &it); err != nil {
					s.warn("tracker release", "id", cur.Key, "err", err)
				} else if it.ID != "" {
					s.fix(t, &it)
					s.mirrorPut(ctx, t, cur.Project, it)
					// The adapter's answer is published whatever the mirror
					// held before (stale or missing): a release always means
					// to move the item, and applying an unchanged status is
					// harmless. The item is filed under the lease's area
					// (leases follow their item's mirrored area).
					if it.Status != "" {
						l, isItem, lerr := s.keyItem(ctx, authz.All(), cur.Project, cur.Key)
						if !isItem || lerr != nil || l.tr == nil {
							l = loc{project: cur.Project, area: cur.Area, id: cur.Key, ref: a.Ref, tr: &tr}
							l.db, _ = s.d.Store.Project(authz.All(), authz.Read, cur.Project)
						}
						if l.db != nil {
							s.publishStatus(ctx, l, it)
						}
					}
				}
			}
		}
	} else if l, isItem, err := s.keyItem(ctx, authz.All(), cur.Project, cur.Key); isItem && err == nil && l.local() {
		if moved, err := setLocalStatusIf(ctx, l.db, l.ref, orchapi.WorkClaimed, orchapi.WorkOpen, s.now()); err != nil {
			s.warn("reopen item", "id", l.id, "err", err)
		} else if moved {
			if it, err := getLocal(ctx, l.db, l.project, l.ref); err == nil {
				s.publishStatus(ctx, l, it)
			}
		}
	}
	if note == "expired" {
		return
	}
	v := cur.api()
	if note != "" {
		v.Note = note
	}
	s.publish(ctx, orchapi.EventWorkReleased, cur.Key, cur.Project, cur.Area, v, cur.Holder)
}

// mirrorPut writes an item a tracker just answered a zm write with into
// its mirror at once (optimistic: the next sync corrects it) and asks for
// that sync now. An answer without a title is taken for a stub, not the
// item (it must not blank the row's title and labels): only the sync
// writes it.
func (s *Service) mirrorPut(ctx context.Context, t tracked, project string, it orchapi.WorkItem) {
	ctx = context.WithoutCancel(ctx)
	sy := s.m.syncer(project, t.tr)
	defer sy.kick()
	if it.Title == "" {
		return
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return
	}
	if err := s.m.write(ctx, project, db, t, []orchapi.WorkItem{it}, sy.gen(), false); err != nil {
		s.warn("mirror: write", "id", it.ID, "err", err)
	}
}

// liveLease is the live lease on work item key (now in area) of project,
// or nil.
func (s *Service) liveLease(ctx context.Context, project, area, key string) (*lease, error) {
	cur, err := itemLease(ctx, s.leaseReadDB(project), project, area, key, s.now())
	if err != nil || cur == nil || !cur.live(s.now()) {
		return nil, err
	}
	return cur, nil
}

// mailTarget is where mail to the item at l goes: its live lease holder,
// else the tracker's owner, else its creator; else refused.
func (s *Service) mailTarget(ctx context.Context, sub *authz.Subject, l loc) (to, via string, err error) {
	cur, err := s.liveLease(ctx, l.project, l.area, l.id)
	if err != nil {
		return "", "", err
	}
	if cur != nil {
		return cur.Holder, "holder of " + l.id, nil
	}
	it, err := s.load(ctx, sub, l)
	if err != nil {
		return "", "", err
	}
	if !l.local() && address.Routable(it.Owner) {
		return address.LiveRecipient(ctx, orchcore.Sessions(s.d), s.d.Config, l.project, l.area, it.Owner, "holder of "+l.id)
	}
	if address.Routable(it.CreatedBy) {
		return address.LiveRecipient(ctx, orchcore.Sessions(s.d), s.d.Config, l.project, l.area, it.CreatedBy, "creator of "+l.id)
	}
	if !l.local() && it.Owner != "" {
		// Dogfood: "nobody holds it" while zm work show said "held by dev-01/...".
		return "", "", api.Errorf(api.CodeConflict, "%s is held in its tracker by %s, which is not a zm address, so zm cannot deliver mail to it", l.id, s.d.Redact(ctx, sub, it.Owner)).
			WithHint(fmt.Sprintf("comment on the item instead (zm work comment %s ...), or mail the group mailbox (machine:<name>)", l.id))
	}
	return "", "", api.Errorf(api.CodeConflict, "nobody holds %s and zm does not know who created it", l.id).
		WithHint(fmt.Sprintf("see zm who %s; to reach whoever is around, mail the group mailbox (machine:<name>)", l.id))
}

// ---- orchcore.WorkService ----

// itemIn locates id inside one project (slug or ULID) for sub.
func (s *Service) itemIn(ctx context.Context, sub *authz.Subject, project, id string) (loc, error) {
	p, ok := s.d.Projects.Lookup(project)
	if !ok || !sub.Scope.SeesProject(p.ID) {
		return loc{}, api.NotFound("work item", id)
	}
	return s.locate(ctx, sub, sub.Scope.Only(p.ID), id)
}

// MailTarget implements orchcore.WorkService.
func (s *Service) MailTarget(ctx context.Context, sub *authz.Subject, project, id string) (string, string, string, error) {
	l, err := s.itemIn(ctx, sub, project, id)
	if err != nil {
		return "", "", "", err
	}
	to, via, err := s.mailTarget(ctx, sub, l)
	return to, via, l.area, err
}

// OwnerOf returns the address holding the live lease on work item id in
// project, redacted for sub ("" when nobody holds it). Mail and the TUI use
// it to show who an item reaches.
func (s *Service) OwnerOf(ctx context.Context, sub *authz.Subject, project, id string) (string, error) {
	l, err := s.itemIn(ctx, sub, project, id)
	if err != nil {
		return "", err
	}
	cur, err := s.liveLease(ctx, l.project, l.area, l.id)
	if err != nil || cur == nil {
		return "", err
	}
	return s.d.Redact(ctx, sub, cur.Holder), nil
}

// BindLease implements orchcore.WorkService: a bound lease on id for session.
func (s *Service) BindLease(ctx context.Context, sub *authz.Subject, project, id, session string) (orchapi.Lease, error) {
	a, err := api.ParseAddress(session)
	if err != nil || a.Kind != api.KindSession {
		return orchapi.Lease{}, api.Errorf(api.CodeBadRequest, "a bound lease needs a session address, not %q", session)
	}
	l, err := s.itemIn(ctx, sub, project, id)
	if err != nil {
		return orchapi.Lease{}, err
	}
	res, err := s.claimItem(ctx, sub, l, orchapi.ClaimRequest{Note: "bound to " + a.String()}, a.String())
	return res.Lease, err
}

// ClaimItem implements orchcore.WorkService: claim work item id in project for
// sub, exactly as POST /v0/work/{id}/claim does.
func (s *Service) ClaimItem(ctx context.Context, sub *authz.Subject, project, id string, req orchapi.ClaimRequest) (orchapi.ClaimResponse, error) {
	l, err := s.itemIn(ctx, sub, project, id)
	p, seen := s.d.Projects.Lookup(project)
	if api.IsCode(err, api.CodeNotFound) && seen && sub.Scope.SeesProject(p.ID) && s.mirrorOne(ctx, project, id) == nil {
		// Made a moment ago (bd create, then bd update --claim): its mirror
		// row comes at the next sync, so read it through now. The caller's
		// scope still decides what it may see.
		l, err = s.itemIn(ctx, sub, project, id)
	}
	if err != nil {
		return orchapi.ClaimResponse{}, err
	}
	return s.claimItem(ctx, sub, l, req, "")
}

// mirrorOne reads tracker item id from its source into the mirror.
func (s *Service) mirrorOne(ctx context.Context, project, id string) error {
	a, ok := parseWorkID(id)
	if !ok || a.Source == LocalSource {
		return api.NotFound("work item", id)
	}
	tr, ok := orchcore.Tracker(s.d.Projects, project, a.Source)
	if !ok {
		return api.NotFound("work item", id)
	}
	t, err := s.tracker(ctx, project, tr)
	if err != nil {
		return err
	}
	var it orchapi.WorkItem
	if err := s.call(ctx, t, orchapi.VerbGet, canonID(a), nil, &it); err != nil {
		return err
	}
	s.fix(t, &it)
	s.mirrorPut(ctx, t, project, it)
	return nil
}

// SessionEnded implements orchcore.WorkService: every bound lease the session
// held is released at once (note "session ended").
func (s *Service) SessionEnded(ctx context.Context, address string) error {
	return s.SessionLeftProject(ctx, address, "")
}

// SessionLeftProject implements orchcore.SessionProjectEnder ("" = every project).
func (s *Service) SessionLeftProject(ctx context.Context, address, project string) error {
	ls, err := s.queryLeasesAll(ctx, "holder = ? AND bound = 1 AND (? = '' OR project = ?)", address, project, project)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if _, err := s.dropLocked(ctx, l, "session ended"); err != nil {
			return err
		}
	}
	return nil
}

// CommentTask files a task result with the same scope and write checks as the work route.
func (s *Service) CommentTask(ctx context.Context, sub *authz.Subject, project, id, body string) error {
	l, err := s.locate(ctx, sub, sub.Scope.Only(project), id)
	if err != nil {
		return err
	}
	_, err = s.comment(ctx, sub, l, body, "", 0)
	return err
}
