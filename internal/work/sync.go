package work

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Sync timing (package variables so tests can shorten them).
var (
	// syncEvery is the pause between incremental syncs of a tracker whose
	// adapter has the changes verb.
	syncEvery = 5 * time.Second
	// listEvery is the pause between full listings of a tracker without it.
	listEvery = 30 * time.Second
	// fullEvery forces a full listing of a changes tracker now and then
	// (deletions a count did not reveal, writers with a clock far behind).
	fullEvery = 24 * time.Hour
	// countFullEvery bounds how often a count mismatch may start a full
	// listing (a tracker whose count never matches must not loop).
	countFullEvery = 10 * time.Minute
	// shortFullEvery bounds how often a mirror holding fewer items than the
	// tracker counts may start one.
	shortFullEvery = 6 * time.Hour
	// maxBackoff caps the pause after failed syncs.
	maxBackoff = time.Minute
	// scanEvery is how often the mirrors notice trackers bound or unbound.
	scanEvery = 30 * time.Second
)

// pauses are syncEvery and listEvery, or Config.SyncPause for both when set.
func (m *mirrors) pauses() (syncPause, listPause time.Duration) {
	if c := m.s.d.Config; c != nil && c.SyncPause > 0 {
		return c.SyncPause, c.SyncPause
	}
	return syncEvery, listEvery
}

// staleAfter is how long after its last good sync a mirror counts as stale.
func (m *mirrors) staleAfter(mode string, poll int) time.Duration {
	syncPause, listPause := m.pauses()
	every := max(syncPause, time.Duration(poll)*time.Second)
	if mode == "list" {
		return 3*max(listPause, every) + 30*time.Second
	}
	return 6*every + 30*time.Second
}

// mirrors runs one syncer per (project, tracker) and guards each project's
// area column.
type mirrors struct {
	s      *Service
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	by      map[string]*syncer
	locks   map[string]*sync.Mutex // per project: mirror writes and area recomputes
	rules   map[string]string      // per project: the rules the area column follows
	started bool
}

func newMirrors(s *Service) *mirrors {
	ctx, cancel := context.WithCancel(context.Background())
	return &mirrors{s: s, ctx: ctx, cancel: cancel, by: map[string]*syncer{}, locks: map[string]*sync.Mutex{}, rules: map[string]string{}}
}

// rulesOf fingerprints a project's area rules (what AreaForItem reads).
func (m *mirrors) rulesOf(project string) string {
	f, ok := m.s.d.Projects.File(project)
	if !ok {
		return ""
	}
	type tr struct{ P, A string }
	var trs []tr
	for _, t := range orchcore.ProjectTrackers(f) {
		trs = append(trs, tr{t.Prefix, t.Area})
	}
	b, _ := json.Marshal(struct {
		A any
		T []tr
	}{f.Areas, trs})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

func (m *mirrors) lock(project string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[project]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[project] = l
	}
	return l
}

// fresh makes project's area column follow its current area rules before a
// read: after an area gains or loses a label, every row is placed again
// (once) before anyone reads by area.
func (m *mirrors) fresh(ctx context.Context, project string, db *sql.DB) error {
	rules := m.rulesOf(project)
	m.mu.Lock()
	ok := m.rules[project] == rules
	m.mu.Unlock()
	if ok {
		return nil
	}
	l := m.lock(project)
	l.Lock()
	defer l.Unlock()
	return m.freshLocked(ctx, project, db, rules)
}

func (m *mirrors) freshLocked(ctx context.Context, project string, db *sql.DB, rules string) error {
	have, err := mirrorRules(ctx, db)
	if err != nil {
		return err
	}
	if have != rules {
		native := map[string]bool{}
		if f, ok := m.s.d.Projects.File(project); ok {
			for _, t := range orchcore.ProjectTrackers(f) {
				if t.Adapter == orchapi.AdapterNative {
					native[t.Prefix] = true
				}
			}
		}
		if err := mirrorReArea(ctx, db, rules, func(id string, labels []string) string {
			if src, _, ok := strings.Cut(id, ":"); ok && native[src] {
				return keepArea // the issue store re-areas native issues; its changes feed brings the new area
			}
			return orchcore.AreaForItem(m.s.d.Projects, project, id, labels)
		}); err != nil {
			return err
		}
		if err := m.followLeases(ctx, project, db); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.rules[project] = rules
	m.mu.Unlock()
	return nil
}

// followLeases files every lease on a tracker item of project under the
// item's mirrored area (moveLease), so a lease taken before its item moved
// (an area rule or the item's labels changed) is still found, renewed,
// released, listed for the item's area readers and expired there. The
// caller holds the project's lock. Leases are few; the mirror reads are
// point lookups.
func (m *mirrors) followLeases(ctx context.Context, project string, db *sql.DB) error {
	ls, err := queryLeases(ctx, m.s.leaseReadDB(project), "project = ?", project)
	if err != nil {
		return err
	}
	for _, l := range ls {
		a, ok := parseWorkID(l.Key)
		if !ok || a.Source == LocalSource {
			continue
		}
		var area string
		err := db.QueryRowContext(ctx, `SELECT area FROM work_mirror WHERE source = ? AND ref = ?`, a.Source, a.Ref).Scan(&area)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && area == l.Area) {
			continue
		}
		if err != nil {
			return err
		}
		if err := moveLease(ctx, m.s.leaseDB(project), project, l.Key, area, m.s.now()); err != nil {
			return err
		}
	}
	return nil
}

// write upserts fixed items of one tracker under the project's lock, with
// areas computed after the rules were fingerprinted (so a rule change in
// between is caught by the next read's fresh).
//
// notify publishes dev.zm.work.changed for every item that is new or
// newer than its row (incremental syncs: what changed in the tracker, so
// subscribers see it live; never for a full listing).
func (m *mirrors) write(ctx context.Context, project string, db *sql.DB, t tracked, items []orchapi.WorkItem, gen int64, notify bool, force ...bool) error {
	if done := mirrorstate.Before(project, items, notify); done != nil {
		defer done()
	}
	l := m.lock(project)
	l.Lock()
	defer l.Unlock()
	if err := m.freshLocked(ctx, project, db, m.rulesOf(project)); err != nil {
		return err
	}
	out := items[:0:0]
	for _, it := range items {
		m.s.fix(t, &it)
		if a, ok := parseWorkID(it.ID); ok && a.Source == t.tr.Prefix {
			if t.tr.Adapter == orchapi.AdapterNative && orchcore.Issues(m.s.d) != nil {
				moved, err := orchcore.Issues(m.s.d).MovedOut(ctx, project, a.Ref)
				if err != nil {
					return err
				}
				if moved {
					continue
				}
			}
			out = append(out, it)
		}
	}
	var changed []orchapi.WorkItem
	if notify && len(out) > 0 {
		have, err := mirrorstate.Fingerprints(ctx, db, t.tr.Prefix, out)
		if err != nil {
			return err
		}
		for _, it := range out {
			if (len(force) != 0 && force[0]) || have[it.ID] != mirrorstate.ItemPrint(it) {
				changed = append(changed, it)
			}
		}
	}
	if !notify && len(force) != 0 && len(out) > 0 { // a full listing: silent, but for what only it can bring (mirrorstate)
		var err error
		if changed, err = mirrorstate.DeadlinesChanged(ctx, db, t.tr.Prefix, out); err != nil {
			return err
		}
	}
	if err := mirrorUpsert(ctx, db, t.tr.Prefix, out, gen); err != nil {
		return err
	}
	if len(out) > 0 { // a relabelled item may have changed area
		if err := m.followLeases(ctx, project, db); err != nil {
			return err
		}
	}
	for _, it := range changed {
		it.Links = nil
		m.s.publish(ctx, orchapi.EventWorkChanged, it.ID, project, it.Area, eventItem(it), it.Owner, it.CreatedBy)
	}
	return nil
}

// syncer returns the running syncer of tracker tr in project, starting it
// (or restarting it, when the binding changed) as needed. It never waits
// on the tracker: the persisted state is read from project.db.
//
// A replaced syncer is stopped and awaited (outside m.mu) before the new
// one wipes the mirror, so no page the old one was writing lands after the
// wipe.
func (m *mirrors) syncer(project string, tr api.Tracker) *syncer {
	key, bind := orchapi.TrackerKey(project, tr.Prefix), orchapi.TrackerBinding(tr)
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		old := m.by[key]
		if old == nil {
			break
		}
		if old.binding == bind {
			return old // (a new tracker area is a new area rule: fresh re-places rows)
		}
		old.stop()
		m.mu.Unlock()
		<-old.done
		m.mu.Lock()
		if m.by[key] == old {
			delete(m.by, key)
		}
	}
	sy := &syncer{m: m, project: project, tr: tr, binding: bind, kickc: make(chan struct{}, 1), busy: make(chan struct{}, 1), done: make(chan struct{})}
	sy.cond = sync.NewCond(&sy.mu)
	sy.load()
	var ctx context.Context
	ctx, sy.cancel = context.WithCancel(m.ctx)
	m.by[key] = sy
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(sy.done)
		sy.run(ctx)
	}()
	return sy
}

// lookup is the running syncer of tracker prefix in project, if any.
func (m *mirrors) lookup(project, prefix string) *syncer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.by[orchapi.TrackerKey(project, prefix)]
}

// kick asks the tracker's syncer for a sync now (after a zm write).
func (m *mirrors) kick(project, prefix string) {
	if sy := m.lookup(project, prefix); sy != nil {
		sy.kick()
	}
}

// scan starts a syncer for every bound tracker and forgets the mirrors of
// trackers no longer bound.
func (m *mirrors) scan(ctx context.Context) {
	for _, p := range m.s.d.Store.Projects(authz.All(), authz.Read) {
		f, ok := m.s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		bound := map[string]bool{}
		for _, tr := range orchcore.ProjectTrackers(f) {
			bound[tr.Prefix] = true
			if !m.s.adds.TryScan(p.ID, tr.Prefix) {
				continue
			}
			if current, ok := orchcore.Tracker(m.s.d.Projects, p.ID, tr.Prefix); ok && orchapi.TrackerBinding(current) == orchapi.TrackerBinding(tr) {
				m.syncer(p.ID, current)
			}
			m.s.adds.Release(p.ID, tr.Prefix)
		}
		srcs, err := mirroredSources(ctx, p.DB)
		if err != nil {
			m.s.warn("mirror: sources", "project", p.ID, "err", err)
			continue
		}
		for _, src := range srcs {
			if bound[src] {
				continue
			}
			if !m.s.adds.TryScan(p.ID, src) {
				continue
			}
			if _, rebound := orchcore.Tracker(m.s.d.Projects, p.ID, src); rebound {
				m.s.adds.Release(p.ID, src)
				continue
			}
			if err := m.forget(ctx, p.ID, src); err != nil {
				m.s.warn("mirror: forget unbound tracker", "project", p.ID, "source", src, "err", err)
			}
			m.s.adds.Release(p.ID, src)
		}
	}
}

// start runs the scan loop: every tracker starts syncing when the daemon
// starts, not on its first read.
func (m *mirrors) start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(scanEvery)
		defer t.Stop()
		for {
			m.scan(m.ctx)
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (m *mirrors) close() {
	m.cancel()
	m.wg.Wait()
}

// syncer keeps one tracker's mirror current: a full listing first (pages
// land as they come, so reads see recent work within seconds), then
// incremental syncs by cursor every syncEvery and right after zm
// writes; a full listing again when the tracker's count shows deletions,
// and every fullEvery.
type syncer struct {
	m       *mirrors
	project string
	tr      api.Tracker
	binding string
	kickc   chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	busy    chan struct{} // one slot, held for a cycle; senders queue in order

	mu       sync.Mutex
	cond     *sync.Cond
	asked    uint64 // waits asked for (wait)
	served   uint64 // the asks the last finished cycle answered
	cycles   uint64 // cycles finished (waitLoaded)
	row      syncRow
	loading  bool      // a full listing is running
	loadGen  int64     // its gen
	err      error     // the last cycle's error (nil when it worked)
	errs     int       // failed cycles in a row
	failFrom time.Time // when the current run of failed cycles started (zero when the last worked)
	path     string
	desc     orchapi.AdapterDescribe
	mode     string // "changes" or "list"
	again    bool   // a count mismatch asks for a full listing (row.announce: a restored tracker's, announced)
	nocursor bool   // the last full listing gave no cursor: listed in full, paced like mode "list"
	countAt  time.Time
	shortAt  time.Time
	loadedAt time.Time
}

func (sy *syncer) db() (*sql.DB, error) {
	return sy.m.s.d.Store.Project(authz.All(), authz.Read, sy.project)
}

// load reads the persisted state; a different binding starts over.
func (sy *syncer) load() {
	ctx := sy.m.ctx
	db, err := sy.db()
	if err != nil {
		return
	}
	r, ok, err := loadSyncRow(ctx, db, sy.tr.Prefix)
	if err != nil {
		sy.m.s.warn("mirror: load sync state", "project", sy.project, "source", sy.tr.Prefix, "err", err)
	}
	if ok && r.binding != sy.binding {
		// A relist still owed survives the rebinding: the copies need its items.
		owed := ""
		if r.announce {
			owed = sy.binding
		}
		if err := mirrorstate.Wipe(ctx, db, sy.tr.Prefix, owed); err != nil {
			sy.m.s.warn("mirror: wipe", "source", sy.tr.Prefix, "err", err)
		}
		ok = false
	}
	if !ok {
		r = syncRow{binding: sy.binding, announce: r.announce}
	}
	sy.row = r
	sy.again = r.announce
	sy.nocursor = r.complete && r.cursor == ""
	if r.err != "" {
		sy.err = api.Errorf(api.CodeUnavailable, "%s", r.err)
	}
}

func (sy *syncer) stop() { sy.cancel() }

func (sy *syncer) kick() {
	select {
	case sy.kickc <- struct{}{}:
	default:
	}
}

// wait asks for a sync and waits until a cycle that started after the
// call has finished (tests, and nothing on a read path).
func (sy *syncer) wait(ctx context.Context) error {
	sy.mu.Lock()
	sy.asked++
	want := sy.asked
	sy.mu.Unlock()
	sy.kick()
	stop := context.AfterFunc(ctx, func() { sy.mu.Lock(); sy.cond.Broadcast(); sy.mu.Unlock() })
	defer stop()
	sy.mu.Lock()
	defer sy.mu.Unlock()
	for sy.served < want {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-sy.done:
			return fmt.Errorf("syncer stopped")
		default:
		}
		sy.cond.Wait()
	}
	return nil
}

// waitLoaded waits until the mirror holds a complete listing, or a cycle
// failed before it did (zm tracker add: the first full sync, without
// waiting for a second cycle as wait would).
func (sy *syncer) waitLoaded(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { sy.mu.Lock(); sy.cond.Broadcast(); sy.mu.Unlock() })
	defer stop()
	sy.mu.Lock()
	defer sy.mu.Unlock()
	for !sy.row.complete && (sy.cycles == 0 || sy.err == nil) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-sy.done:
			return fmt.Errorf("syncer stopped")
		default:
		}
		sy.cond.Wait()
	}
	return nil
}

func (sy *syncer) run(ctx context.Context) {
	reportedState := ""
	for {
		// This cycle answers every ask so far, and any kick they sent.
		sy.mu.Lock()
		asked := sy.asked
		select {
		case <-sy.kickc:
		default:
		}
		sy.mu.Unlock()
		sy.busy <- struct{}{}
		err := sy.cycle(ctx)
		<-sy.busy
		if ctx.Err() != nil {
			sy.mu.Lock()
			sy.cond.Broadcast()
			sy.mu.Unlock()
			return
		}
		sy.mu.Lock()
		sy.err = err
		syncPause, listPause := sy.m.pauses()
		pause := max(syncPause, time.Duration(sy.desc.PollSeconds)*time.Second)
		if sy.mode == "list" {
			pause = max(listPause, pause)
		}
		if err != nil {
			sy.errs++
			if sy.failFrom.IsZero() {
				sy.failFrom = time.Now()
			}
			pause = errPause(err, sy.errs)
			sy.row.err = err.Error()
			sy.m.s.warn("mirror: sync", "project", sy.project, "source", sy.tr.Prefix, "err", err)
		} else {
			sy.errs, sy.row.err, sy.failFrom = 0, "", time.Time{}
		}
		if sy.nocursor {
			pause = max(listPause, pause)
		} else if sy.again && err == nil { // a failing tracker keeps its backoff, even owing a listing
			pause = 0
		}
		row := sy.row
		sy.served = asked
		sy.cycles++
		sy.cond.Broadcast()
		sy.mu.Unlock()
		if db, derr := sy.db(); derr == nil {
			if err := saveSyncRow(ctx, db, sy.tr.Prefix, row); err != nil && ctx.Err() == nil {
				sy.m.s.warn("mirror: save sync state", "source", sy.tr.Prefix, "err", err)
			}
		}
		state := sy.view(time.Now()).state
		if state != reportedState {
			// A content-free invalidation hint: clients refetch their scoped
			// metadata even when a successful sync changed no work items.
			sy.m.s.publish(ctx, orchapi.EventWorkRefreshed, "", sy.project, sy.tr.Area, orchapi.ProjectHint{Project: sy.project})
			reportedState = state
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-sy.kickc:
			t.Stop()
		case <-t.C:
		}
	}
}

// cycle is one sync: a full listing when the mirror is not complete (or a
// count mismatch or fullEvery asks for one), else an incremental one.
func (sy *syncer) cycle(ctx context.Context) error {
	s := sy.m.s
	t, err := s.tracker(ctx, sy.project, sy.tr)
	sy.mu.Lock()
	sy.path, sy.desc = t.path, t.desc
	if err == nil {
		sy.mode = "list"
		if t.has(orchapi.VerbChanges) {
			sy.mode = "changes"
		}
	}
	full := !sy.row.complete || sy.again || sy.nocursor || sy.mode == "list" || time.Now().Sub(sy.row.fullSync) > fullEvery
	sy.mu.Unlock()
	if err != nil {
		return err
	}
	db, err := sy.db()
	if err != nil {
		return err
	}
	if !full {
		return sy.incremental(ctx, db, t)
	}
	if sy.mode == "list" {
		return sy.fullList(ctx, db, t)
	}
	return sy.fullChanges(ctx, db, t)
}

// newGen is a generation for a full listing, newer than any before.
func (sy *syncer) newGen() int64 {
	g := time.Now().UnixNano()
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if g <= sy.row.gen {
		g = sy.row.gen + 1
	}
	sy.loading, sy.loadGen = true, g
	return g
}

// gen is the generation zm's own writes stamp rows with: the running
// listing's, so they are not dropped as unseen when it ends.
func (sy *syncer) gen() int64 {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	if sy.loading {
		return sy.loadGen
	}
	return sy.row.gen
}

func (sy *syncer) endLoad() {
	sy.mu.Lock()
	sy.loading = false
	sy.mu.Unlock()
}

func (sy *syncer) callChanges(ctx context.Context, t tracked, args orchapi.AdapterChangesArgs) (orchapi.AdapterChanges, error) {
	cctx, cancel := context.WithTimeout(ctx, adapter.Timeout)
	defer cancel()
	var res orchapi.AdapterChanges
	err := sy.m.s.call(cctx, t, orchapi.VerbChanges, "", args, &res)
	return res, err
}

func (sy *syncer) fullChanges(ctx context.Context, db *sql.DB, t tracked) error {
	sy.mu.Lock()
	announce := sy.row.announce
	sy.mu.Unlock()
	gen := sy.newGen()
	defer sy.endLoad()
	start := time.Now()
	var page string
	var cursor string
	partial := false
	for pages := 0; ; pages++ {
		if pages > 10000 {
			return api.Errorf(api.CodeUnavailable, "the %s tracker's listing did not end", sy.tr.Adapter)
		}
		res, err := sy.callChanges(ctx, t, orchapi.AdapterChangesArgs{Page: page})
		if err != nil {
			return err
		}
		if err := sy.m.write(ctx, sy.project, db, t, res.Items, gen, announce, announce); err != nil {
			return err
		}
		partial = partial || res.Partial
		if res.Next == "" {
			cursor = res.Cursor
			break
		}
		page = res.Next
	}
	// What changed while the listing ran is caught up before anything is
	// dropped as gone, so an item that moved between pages stays.
	if cursor != "" {
		res, err := sy.callChanges(ctx, t, orchapi.AdapterChangesArgs{Since: cursor})
		if err != nil {
			return err
		}
		if err := sy.m.write(ctx, sy.project, db, t, res.Items, gen, announce, announce); err != nil {
			return err
		}
		if res.Cursor != "" {
			cursor = res.Cursor
		}
	}
	if !partial { // a partial listing cannot call anything gone
		if _, err := mirrorDropUnseen(ctx, db, sy.tr.Prefix, gen); err != nil {
			return err
		}
	}
	now := time.Now()
	sy.mu.Lock()
	sy.row.gen, sy.row.cursor, sy.row.complete = gen, cursor, true
	sy.row.synced, sy.row.fullSync = now, start
	if announce { // owed until now, and on disk until the loop saves this row
		sy.row.announce = false
	}
	sy.again, sy.nocursor = sy.row.announce, cursor == ""
	sy.mu.Unlock()
	return nil
}

func (sy *syncer) incremental(ctx context.Context, db *sql.DB, t tracked) error {
	sy.mu.Lock()
	since, gen := sy.row.cursor, sy.row.gen
	sy.mu.Unlock()
	if since == "" { // a tracker that never gave a cursor is listed in full, paced like mode "list"
		sy.mu.Lock()
		sy.nocursor = true
		sy.mu.Unlock()
		return nil
	}
	// The mirror's count before the tracker's: each row came from the
	// tracker, so only a deletion leaves it above a count taken later. A
	// count after this sync's writes also holds items made since the
	// tracker counted (in this feed, or by mirrorPut): not deletions.
	before, berr := mirrorCount(ctx, db, sy.tr.Prefix)
	var deleted []string
	var count *int
	cursor := since
	for page, pages := "", 0; ; pages++ {
		res, err := sy.callChanges(ctx, t, orchapi.AdapterChangesArgs{Since: since, Page: page})
		if api.IsCode(err, api.CodeBadRequest) {
			// The adapter cannot use the cursor (a new adapter version, a
			// reset tracker): list everything again, now.
			sy.mu.Lock()
			sy.again = true
			sy.mu.Unlock()
			return nil
		}
		if err != nil {
			return err
		}
		if err := sy.m.write(ctx, sy.project, db, t, res.Items, gen, true); err != nil {
			return err
		}
		deleted = append(deleted, res.Deleted...)
		if res.Count != nil {
			count = res.Count
		}
		if res.Cursor != "" {
			cursor = res.Cursor
		}
		if res.Next == "" || pages > 1000 {
			break
		}
		page = res.Next
	}
	var fixed []string
	for _, id := range deleted {
		it := orchapi.WorkItem{ID: id}
		sy.m.s.fix(t, &it)
		fixed = append(fixed, it.ID)
	}
	gone, err := mirrorDelete(ctx, db, sy.tr.Prefix, fixed)
	if err != nil {
		return err
	}
	now := time.Now()
	sy.mu.Lock()
	defer sy.mu.Unlock()
	sy.row.cursor, sy.row.synced = cursor, now
	if count != nil {
		n, err := mirrorCount(ctx, db, sy.tr.Prefix)
		switch {
		case err != nil || berr != nil:
		case before-gone > *count && n > *count && now.Sub(sy.countAt) > countFullEvery:
			// Something was deleted in the tracker: list it all again (n
			// too: a row dropped meanwhile, a moved issue, is in before only).
			sy.again, sy.countAt = true, now
		case n < *count && now.Sub(sy.shortAt) > shortFullEvery:
			// The mirror missed something (a writer's clock far behind?). A
			// tracker whose count includes what its listing never shows
			// would ask for this forever, so only now and then.
			sy.again, sy.shortAt = true, now
		}
	}
	return nil
}

// errPause is how long sync waits after its errs-th failure in a row:
// doubling up to maxBackoff, and never sooner than a tracker's rate limit
// asks (an adapter's error with retry_after), up to an hour.
func errPause(err error, errs int) time.Duration {
	pause := min(time.Duration(1<<min(errs, 6))*time.Second, maxBackoff)
	var ae *api.Error
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		pause = max(pause, min(time.Duration(ae.RetryAfter)*time.Second, time.Hour))
	}
	return pause
}

// fullList is the fallback for adapters without changes: list everything
// (not done, then done) and drop what is gone, unless a list was cut at
// the limit (then nothing can be called gone).
func (sy *syncer) fullList(ctx context.Context, db *sql.DB, t tracked) error {
	sy.mu.Lock()
	announce := sy.row.announce // a restored tracker's listing is announced here too
	sy.mu.Unlock()
	gen := sy.newGen()
	defer sy.endLoad()
	start := time.Now()
	whole := true
	for _, st := range []orchapi.WorkStatus{"", orchapi.WorkDone} {
		lctx, cancel := context.WithTimeout(ctx, adapter.Timeout)
		var l orchapi.WorkList
		err := sy.m.s.call(lctx, t, orchapi.VerbList, "", orchapi.AdapterListArgs{Status: st, Limit: maxLimit}, &l)
		cancel()
		if err != nil {
			return err
		}
		whole = whole && len(l.Items) < maxLimit
		force := []bool{} // none: a plain list-mode listing stays silent, deadlines included
		if announce {
			force = []bool{true}
		}
		if err := sy.m.write(ctx, sy.project, db, t, l.Items, gen, announce, force...); err != nil {
			return err
		}
	}
	if whole {
		if _, err := mirrorDropUnseen(ctx, db, sy.tr.Prefix, gen); err != nil {
			return err
		}
	}
	now := time.Now()
	sy.mu.Lock()
	sy.row.gen, sy.row.complete, sy.row.synced, sy.row.fullSync = gen, true, now, start
	if announce {
		sy.row.announce = false
	}
	sy.again = sy.row.announce
	sy.mu.Unlock()
	return nil
}

// syncView is a snapshot of a syncer for reads.
type syncView struct {
	state    string // orchapi.SyncLoading, SyncOK, SyncStale
	failing  bool   // never completed and the last cycle failed: nothing to serve
	err      error
	synced   time.Time
	fullSync time.Time
	path     string
	desc     orchapi.AdapterDescribe
	mode     string
	failFrom time.Time // the current run of failed cycles started then
	fails    int       // failed cycles in a row
}

func (sy *syncer) view(now time.Time) syncView {
	sy.mu.Lock()
	defer sy.mu.Unlock()
	v := syncView{err: sy.err, synced: sy.row.synced, fullSync: sy.row.fullSync, path: sy.path, desc: sy.desc, mode: sy.mode,
		failFrom: sy.failFrom, fails: sy.errs}
	switch {
	case !sy.row.complete:
		v.state = orchapi.SyncLoading
		v.failing = sy.err != nil
	case sy.err != nil || now.Sub(sy.row.synced) > sy.m.staleAfter(sy.mode, sy.desc.PollSeconds):
		v.state = orchapi.SyncStale
	default:
		v.state = orchapi.SyncOK
	}
	return v
}

var _ orchcore.TrackerHealth = (*Service)(nil)

// FailingTrackers implements orchcore.TrackerHealth.
func (s *Service) FailingTrackers(sc authz.Scope, atLeast time.Duration) []orchapi.TrackerFailure {
	m := s.m
	m.mu.Lock()
	sys := make([]*syncer, 0, len(m.by))
	for _, sy := range m.by {
		sys = append(sys, sy)
	}
	m.mu.Unlock()
	now := time.Now()
	var out []orchapi.TrackerFailure
	for _, sy := range sys {
		if !sc.WholeProject(sy.project) {
			continue
		}
		sy.mu.Lock()
		from, n := sy.failFrom, sy.errs
		sy.mu.Unlock()
		if from.IsZero() || now.Sub(from) < atLeast {
			continue
		}
		out = append(out, orchapi.TrackerFailure{Project: sy.project, Slug: s.d.Projects.Slug(sy.project), Prefix: sy.tr.Prefix, Since: from.UTC(), Failures: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
