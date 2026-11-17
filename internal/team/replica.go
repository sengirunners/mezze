package team

// Round C (SPEC 17.6): a team replica keeps a copy of each joined project in
// $ZM_HOME/replicas/<ULID>.db (items, comments, leases, mail, as the
// authority shows them to this replica, already redacted for it) plus an
// outbox of writes the authority has not answered yet. Reads are served
// from the copy, never over the tailnet; writes go to the outbox first (one
// local commit), are overlaid on the copy at once, and drain to the
// authority in order (outbox.go). The copy follows the authority's
// changelog (GET /v0/sync, long-polled) from a snapshot (GET
// /v0/sync/snapshot); see syncloop.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

var errMovedRefresh = errors.New("item moved out of this copy")
var errStaleCopy = errors.New("authority answer predates the current copy")

var replicaMigrations = []store.Migration{
	{Kind: store.KindReplica, Name: "team/0001_copy", Script: `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE items (
	id      TEXT PRIMARY KEY,
	area    TEXT NOT NULL DEFAULT '',
	status  TEXT NOT NULL DEFAULT '',
	source  TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL DEFAULT 0,
	base    TEXT NOT NULL DEFAULT '',
	json    TEXT NOT NULL
);
CREATE INDEX items_created ON items (created DESC, id);
CREATE TABLE comments (id TEXT PRIMARY KEY, item TEXT NOT NULL, json TEXT NOT NULL);
CREATE INDEX comments_item ON comments (item, id);
CREATE TABLE leases (key TEXT PRIMARY KEY, holder TEXT NOT NULL, bound INTEGER NOT NULL DEFAULT 0, expires INTEGER NOT NULL DEFAULT 0, json TEXT NOT NULL);
CREATE TABLE mail (
	id        TEXT PRIMARY KEY,
	recipient TEXT NOT NULL,
	sender    TEXT NOT NULL,
	needs     TEXT NOT NULL DEFAULT '',
	acked     INTEGER NOT NULL DEFAULT 0,
	reply_to  TEXT NOT NULL DEFAULT '',
	created   INTEGER NOT NULL DEFAULT 0,
	json      TEXT NOT NULL
);
CREATE INDEX mail_recipient ON mail (recipient, id);
CREATE INDEX mail_sender ON mail (sender, id);
CREATE TABLE mail_seen (mail_id TEXT NOT NULL, address TEXT NOT NULL, PRIMARY KEY (mail_id, address));
CREATE TABLE outbox (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	id      TEXT NOT NULL UNIQUE,
	kind    TEXT NOT NULL,
	method  TEXT NOT NULL,
	path    TEXT NOT NULL,
	body    TEXT NOT NULL DEFAULT '',
	as_addr TEXT NOT NULL DEFAULT '',
	item    TEXT NOT NULL DEFAULT '',
	base    TEXT NOT NULL DEFAULT '',
	state   TEXT NOT NULL,
	error   TEXT NOT NULL DEFAULT '',
	result  TEXT NOT NULL DEFAULT '',
	seen    TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	done    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX outbox_state ON outbox (state, seq);
CREATE INDEX outbox_item ON outbox (item, seq);`},
	// Items to fetch whole that must survive a restart: a redaction's
	// refetch (identifiers design, stage 2b part 2).
	{Kind: store.KindReplica, Name: "team/0002_refetch", Script: `CREATE TABLE refetch (id TEXT PRIMARY KEY);`},
	// A list or search walks items newest first and keeps the few not done:
	// with status, source and area in the index it judges each row there and
	// reads only the rows it keeps, not every row of the copy (most are done).
	{Kind: store.KindReplica, Name: "team/0003_items_list", Script: `DROP INDEX items_created;
CREATE INDEX items_list ON items (created DESC, id, status, source, area);`},
	// A rollup counts every item from an index (internal/rollup): the last
	// change is a column here, as in the mirror, so it stays a number to compare.
	{Kind: store.KindReplica, Name: "team/0005_rollup", Script: `ALTER TABLE items ADD COLUMN updated INTEGER NOT NULL DEFAULT 0;
UPDATE items SET updated = coalesce(CAST(round((julianday(json_extract(json, '$.updated')) - 2440587.5) * 86400000) AS INTEGER), 0);
` + copyTable.IndexSQL()},
	{Kind: store.KindReplica, Name: "team/0004_moved", Script: `CREATE TABLE moved_items (id TEXT PRIMARY KEY, event TEXT NOT NULL);`},
	// The outcome remains durable, but an old-scope answer cannot overlay or
	// disclose its payload through the current copy/outbox after a snapshot.
	{Kind: store.KindReplica, Name: "team/0008_copy_fence", Script: `ALTER TABLE outbox ADD COLUMN copy_fenced INTEGER NOT NULL DEFAULT 0;`},
	// A redacted comment's old text, until this machine's modules scrubbed
	// their own copies of it: written in the transaction that
	// saw the redaction, removed once the scrub went through.
	{Kind: store.KindReplica, Name: "team/0009_pending_scrubs", Script: `CREATE TABLE pending_scrubs (id INTEGER PRIMARY KEY, item TEXT NOT NULL, text TEXT NOT NULL);`},
}

// DirReplicas holds each joined project's copy: replicas/<ULID>.db.
const DirReplicas = "replicas"

// Tunables (tests shorten them).
var (
	// StaleAfter: with no answer from the authority for this long, reads are
	// marked stale (they are still served).
	StaleAfter = 30 * time.Second
	// RefuseAfter: with no answer for this long the replica refuses writes
	// (SPEC 17.4: 7 days), still serving reads marked stale.
	RefuseAfter = 7 * 24 * time.Hour
	// OutboxMax bounds the writes waiting for the authority.
	OutboxMax = 10000
)

// replica is one joined project's copy and its sync state.
type replica struct {
	db *sql.DB
	// project is the copy's project (the authority's ULID); scrub has this
	// machine's modules scrub a text redacted there (core.Deps.ScrubAll).
	project string
	scrub   func(ctx context.Context, sc core.Scrub) error
	drainMu sync.Mutex // one drain of pending_scrubs at a time
	// apply is the same file for what sync and refresh apply: synchronous
	// NORMAL, never waiting on the disk (store.OpenReplicaApply). nil: db.
	apply *sql.DB
	path  string

	mu        sync.Mutex
	ready     bool      // a snapshot was applied: reads are served here
	cursor    string    // the last change id applied
	synced    time.Time // the last answer from the authority (local clock)
	skew      time.Duration
	reachable bool
	lastErr   string
	resnap    int                // scope changes seen (a generation): a snapshot clears only those seen before it began
	snapped   int                // the resnap generation the last applied snapshot was asked under
	endPoll   context.CancelFunc // ends the changelog call in flight, when a scope change needs a snapshot
	pending   int                // outbox entries waiting (cached for status)
	lastGC    time.Time
	conflicts int
	backup    *orchapi.HomeBackup // the authority's off-site backup, from its last sync answer
	started   time.Time           // when the authority's daemon started, from the same answer
	// restore: the home's restore generation the copy was taken under (sent
	// as GET /v0/sync's gen=); lost: items the copy held that the home lost
	// in its last restore, so a queued write on one says so (settle).
	restore  string
	known    bool // the copy was taken by a zm that keeps restore (an older one's "" means nothing)
	lost     map[string]bool
	lostAcks []outEntry // acked writes the restore lost, to tell their sessions (snapshotOnce)
	refused  []outEntry // refusals of lost items its snapshot un-fenced, to tell likewise
	// fenced: refusals settled as obsolete, by id, with the snapshot epoch
	// then (settle). Only those settled while this snapshot streamed are
	// candidates for its restore to un-fence (unfenceLostRefusals).
	fenced map[string]uint64

	roster rosterCache // the owner's session list as the authority last said it (roster.go)

	kickSync        chan struct{}
	kickDrain       chan struct{}
	writing         sync.Mutex // one writer at a time (see write)
	wmu             sync.Mutex
	waiters         map[string][]chan api.WriteResult // write id -> callers waiting for its answer
	rmu             sync.Mutex
	refresh         map[string]bool             // items to fetch whole (links, comments of an item that came into view)
	testRefreshHook func(string) func()         // nil outside tests
	testCopyHook    func(string, string) func() // after an answer, before applying it; nil outside tests
	snapshotMu      sync.RWMutex
	snapshotEpoch   uint64 // changes when a snapshot starts or commits; guarded by snapshotMu
	windowFrom      uint64 // the first attempt's epoch since the last commit (0: none); snapshotMu
}

// copyFence is captured before remote I/O. Epoch orders snapshots even when
// a scope change moves the visible event cursor backwards.
type copyFence struct {
	epoch uint64
	base  string
}

func (rp *replica) copyFence() copyFence {
	rp.snapshotMu.RLock()
	defer rp.snapshotMu.RUnlock()
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return copyFence{epoch: rp.snapshotEpoch, base: rp.cursor}
}

func (rp *replica) copyReplyHook(kind, id string) func() {
	rp.mu.Lock()
	hook := rp.testCopyHook
	rp.mu.Unlock()
	if hook != nil {
		return hook(kind, id)
	}
	return nil
}

// copyItemCurrent is called inside the write transaction, while snapshotMu
// is held. A move or disappearance since the request must win over its answer.
func copyItemCurrent(ctx context.Context, tx *sql.Tx, id string, fence copyFence) (bool, error) {
	var moved string
	err := tx.QueryRowContext(ctx, `SELECT event FROM moved_items WHERE id = ?`, id).Scan(&moved)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if moved > fence.base {
		return false, nil
	}
	var base string
	err = tx.QueryRowContext(ctx, `SELECT base FROM items WHERE id = ?`, id).Scan(&base)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	var cursor string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'cursor'`).Scan(&cursor)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	return cursor <= fence.base, nil
}

// putCreated keeps the read-your-create optimization only while the reply
// still belongs to this copy. The authority's successful result is unchanged.
func (rp *replica) putCreated(ctx context.Context, it orchapi.WorkItem, fence copyFence) error {
	rp.snapshotMu.RLock()
	defer rp.snapshotMu.RUnlock()
	if rp.snapshotEpoch != fence.epoch {
		return errStaleCopy
	}
	return rp.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE id = ?`, it.ID).Scan(&n); err != nil || n > 0 {
			return err // sync already brought this row or something newer
		}
		current, err := copyItemCurrent(ctx, tx, it.ID, fence)
		if err != nil {
			return err
		}
		if !current {
			return errStaleCopy
		}
		return putItem(ctx, tx, it, "", true)
	})
}

func (rp *replica) dropItemAt(ctx context.Context, id string, fence copyFence) error {
	rp.snapshotMu.RLock()
	if rp.snapshotEpoch != fence.epoch {
		rp.snapshotMu.RUnlock()
		return errStaleCopy
	}
	err := rp.writeApply(ctx, func(tx *sql.Tx) error {
		var base string
		err := tx.QueryRowContext(ctx, `SELECT base FROM items WHERE id = ?`, id).Scan(&base)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if base > fence.base {
			return errStaleCopy
		}
		return forgetItem(ctx, tx, id)
	})
	rp.snapshotMu.RUnlock()
	if err != nil {
		return err
	}
	return rp.compactAfterApply(ctx)
}

func (s *Service) replicaPath(project string) string {
	return filepath.Join(s.d.Config.Home, DirReplicas, project+".db")
}

// openReplica opens (creating) project's copy and reads its state.
func (s *Service) openReplica(ctx context.Context, project string) (*replica, error) {
	path := s.replicaPath(project)
	db, err := s.d.Store.OpenReplica(ctx, path, 4)
	if err != nil {
		return nil, err
	}
	apply, err := s.d.Store.OpenReplicaApply(ctx, path)
	if err != nil {
		db.Close()
		return nil, err
	}
	rp := &replica{db: db, apply: apply, path: path, kickSync: make(chan struct{}, 1), kickDrain: make(chan struct{}, 1),
		waiters: map[string][]chan api.WriteResult{}, refresh: map[string]bool{}, project: project, scrub: s.d.ScrubAll}
	rp.roster.kick = make(chan struct{}, 1)
	rp.cursor = rp.meta(ctx, "cursor")
	rp.ready = rp.meta(ctx, "ready") == "1"
	rp.restore = rp.meta(ctx, "restore")
	rp.known = rp.meta(ctx, "restore_known") == "1"
	rp.lost = decodeLost(rp.meta(ctx, "restore_lost"))
	if ms := rp.meta(ctx, "synced"); ms != "" {
		var n int64
		if json.Unmarshal([]byte(ms), &n) == nil {
			rp.synced = store.FromMillis(n)
		}
	}
	rp.countOutbox(ctx)
	if rows, err := db.QueryContext(ctx, `SELECT id FROM refetch`); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				rp.want(id)
			}
		}
		rows.Close()
	}
	return rp, nil
}

// write runs one transaction on the copy. Writers queue here: left to
// SQLite, a second writer sleeps and polls (1, 2, 5... 100 ms), which puts a
// caller's queued comment behind the sync and refresh loops for tens of ms.
func (rp *replica) write(ctx context.Context, fn func(*sql.Tx) error) error {
	rp.writing.Lock()
	defer rp.writing.Unlock()
	return store.Tx(ctx, rp.db, fn)
}

// writeApply is write for what can be had again: what the authority's
// answers bring (snapshots, changes, items fetched whole) and the outbox's
// bookkeeping after an answer (settles, which the authority's write records
// make safe to replay, and deleting settled rows). It commits without
// waiting on the disk, so a caller's queued write waits for no fsync but its
// own. On a disk where an fsync takes milliseconds, a link written while
// the last one was settled, synced and refreshed waited for three
// (replica-link p50 8.7 ms on such CI runners, 0.6 ms on others).
func (rp *replica) writeApply(ctx context.Context, fn func(*sql.Tx) error) error {
	return rp.writeApplyCommitted(ctx, fn, nil)
}

// writeApplyCommitted publishes the private presence view only after the
// canonical transaction commits, while the replica writer still excludes
// accepted writes. Its callback does no SQL or network I/O.
func (rp *replica) writeApplyCommitted(ctx context.Context, fn func(*sql.Tx) error, leases *leaseCacheCommit) error {
	db := rp.apply
	if db == nil {
		db = rp.db
	}
	rp.writing.Lock()
	defer rp.writing.Unlock()
	if applyCommitsLost.Load() {
		return nil // tests: as if the power cut took it
	}
	err := store.Tx(ctx, db, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if leases != nil {
			return leases.read(ctx, tx)
		}
		return nil
	})
	if err == nil && leases != nil {
		leases.committed()
	}
	return err
}

// exec is write for one statement.
func (rp *replica) exec(ctx context.Context, query string, args ...any) error {
	return rp.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

// closeReplica closes the copy; drop also deletes its file (leave, purge,
// revoke: SPEC 17.4 "deletes the token and any local copy").
// A drop first saves the copy's pending outbox writes beside it
// (unsent.go); a failure there does not stop the drop, it is returned.
func (rp *replica) close(drop bool) (saved int, err error) {
	if rp == nil {
		return 0, nil
	}
	if drop {
		saved, err = exportUnsent(context.Background(), rp.db, rp.path)
	}
	if rp.apply != nil {
		_ = rp.apply.Close()
	}
	_ = rp.db.Close()
	if drop {
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(rp.path + sfx)
		}
	}
	return saved, err
}

func (rp *replica) meta(ctx context.Context, key string) string {
	var v string
	_ = rp.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// view is what the replica's status shows.
func (rp *replica) view() api.ReplicaStatus {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	st := api.ReplicaStatus{State: "loading", Cursor: rp.cursor, Reachable: rp.reachable, Pending: rp.pending,
		Conflicts: rp.conflicts, LastError: rp.lastErr}
	if rp.ready {
		st.State = "ok"
		if rp.stale() {
			st.State = "stale"
		}
	}
	if !rp.synced.IsZero() {
		t := rp.synced.UTC()
		st.Synced = &t
		st.AgeSecs = time.Since(rp.synced).Seconds()
	}
	return st
}

// stale: no answer from the authority for StaleAfter (rp.mu held).
func (rp *replica) stale() bool {
	return !rp.reachable || rp.synced.IsZero() || time.Since(rp.synced) > StaleAfter
}

// staleNote is WorkItem.Stale / WorkBehind for a copy that is behind, ""
// when current.
func (rp *replica) staleNote() (string, time.Time) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if !rp.stale() {
		return "", rp.synced
	}
	why := "the authority is unreachable"
	if rp.lastErr != "" {
		why = rp.lastErr
	}
	if rp.synced.IsZero() {
		return "replica copy, never synced: " + why, rp.synced
	}
	return "replica copy as of " + rp.synced.UTC().Format(time.RFC3339) + " (" + time.Since(rp.synced).Round(time.Second).String() + " ago): " + why, rp.synced
}

// isReady reports whether reads are served from the copy.
func (rp *replica) isReady() bool {
	if rp == nil {
		return false
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.ready
}

// authorityNow is the authority's clock as this replica last measured it
// (lease deadlines are the authority's).
func (rp *replica) authorityNow() time.Time {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return time.Now().Add(rp.skew)
}

// markAnswer records an answer from the authority (now = its clock, zero
// when the answer did not carry it).
func (rp *replica) markAnswer(now time.Time) {
	rp.mu.Lock()
	rp.reachable, rp.lastErr, rp.synced = true, "", time.Now()
	if !now.IsZero() {
		rp.skew = time.Until(now)
	}
	rp.mu.Unlock()
}

// markDown records a failed call: reads turn stale, queued claims stop
// waiting for an answer.
func (rp *replica) markDown(err error) {
	rp.mu.Lock()
	rp.reachable = false
	if err != nil {
		rp.lastErr = sanitize(err.Error())
	}
	rp.mu.Unlock()
}

func (rp *replica) isReachable() bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.reachable
}

// sanitize keeps an error line short and free of local paths.
func sanitize(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// ---- applying the authority's state ----

// snapshotAnswer carries independent authority restore generation and local
// post-commit lease-cache reconciliation. A direct legacy test caller with no
// answer has neither a restore generation nor a lease-cache publication.
type snapshotAnswer struct {
	restore string
	leases  *leaseCacheCommit
}

// applySnapshot replaces the copy with a snapshot's rows in one transaction
// (rows not in it are dropped); the outbox is kept and overlays again, then
// replays with its bases (SPEC 17.6 "Resync"). The restore generation is kept
// with the copy in that transaction, never from any other answer. A changed
// generation records what the home's restore lost (restore_lost).
func (rp *replica) applySnapshot(ctx context.Context, lines []api.SnapshotLine, cursor string, now time.Time, gen int, answers ...snapshotAnswer) error {
	return rp.applySnapshotAttempt(ctx, lines, cursor, now, gen, nil, answers...)
}

func (rp *replica) applySnapshotAttempt(ctx context.Context, lines []api.SnapshotLine, cursor string, now time.Time, gen int, attempt *snapshotAttempt, answers ...snapshotAnswer) error {
	var answer snapshotAnswer
	if len(answers) > 0 {
		answer = answers[0]
	}
	restore := answer.restore
	rp.snapshotMu.Lock()
	if attempt != nil && attempt.epoch != rp.snapshotEpoch {
		rp.snapshotMu.Unlock()
		return api.Errorf(api.CodeResync, "snapshot superseded locally")
	}
	rp.mu.Lock()
	prevRestore, wasReady, known := rp.restore, rp.ready, rp.known
	var during []string // refusals fenced since the first attempt of this snapshot began
	if attempt != nil && rp.windowFrom != 0 {
		for id, epoch := range rp.fenced {
			if epoch >= rp.windowFrom {
				during = append(during, id)
			}
		}
	}
	rp.mu.Unlock()
	// Only a copy taken by a zm that keeps the generation can tell a
	// restore: an upgraded copy's "" is no generation, and what it lacks
	// may be ordinary deletions. Its snapshot still drops the ghosts.
	restored := wasReady && known && restore != prevRestore
	var lost []string
	var lostAcks, refused []outEntry
	err := rp.writeApplyCommitted(ctx, func(tx *sql.Tx) error {
		lost, lostAcks, refused = nil, nil, nil
		if attempt != nil {
			for _, seq := range attempt.acked {
				if _, err := tx.ExecContext(ctx, `UPDATE outbox SET copy_fenced = 1 WHERE seq = ? AND state = ?`, seq, api.OutboxAcked); err != nil {
					return err
				}
			}
		}
		old, err := commentBodies(ctx, tx)
		if err != nil {
			return err
		}
		var held map[string]bool
		if restored {
			if held, err = itemIDs(ctx, tx); err != nil {
				return err
			}
		}
		for _, t := range []string{"items", "comments", "leases", "mail"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
				return err
			}
		}
		for _, l := range lines {
			if err := applyRow(ctx, tx, l.Kind, l.Data, cursor); err != nil {
				return err
			}
		}
		// A snapshot may show an issue that has moved back into this project.
		if _, err := tx.ExecContext(ctx, `DELETE FROM moved_items WHERE id IN (SELECT id FROM items)`); err != nil {
			return err
		}
		// A comment redacted while this copy was away comes back redacted.
		cur, err := commentBodies(ctx, tx)
		if err != nil {
			return err
		}
		for id, o := range old {
			if n, ok := cur[id]; ok {
				if err := noteRedaction(ctx, tx, o.item, o.body, n.body); err != nil {
					return err
				}
			}
		}
		if restored {
			kept, err := itemIDs(ctx, tx)
			if err != nil {
				return err
			}
			for id := range held {
				if !kept[id] {
					lost = append(lost, id)
				}
			}
			sort.Strings(lost)
			if err := setMeta(ctx, tx, "restore_lost", encodeLost(lost)); err != nil {
				return err
			}
			// Refusals first: failLostAcks turns lost acks into failed rows,
			// which are told as lost acks, never also as refusals.
			if refused, err = unfenceLostRefusals(ctx, tx, lost, during); err != nil {
				return err
			}
			if lostAcks, err = failLostAcks(ctx, tx, time.Now()); err != nil {
				return err
			}
		}
		if err := setMeta(ctx, tx, "restore", restore); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "restore_known", "1"); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "cursor", cursor); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "ready", "1"); err != nil {
			return err
		}
		ms, _ := json.Marshal(store.Millis(time.Now()))
		return setMeta(ctx, tx, "synced", string(ms))
	}, answer.leases)
	if err != nil {
		rp.snapshotMu.Unlock()
		return err
	}
	rp.mu.Lock()
	rp.ready, rp.cursor = true, cursor
	rp.snapped = gen // a scope change seen after the request went out asks for another
	rp.fenced = nil  // older fenced refusals belong to replaced copies
	rp.restore, rp.known = restore, true
	if restored {
		rp.lost = decodeLost(encodeLost(lost))
		rp.lostAcks, rp.refused = lostAcks, refused
	}
	rp.mu.Unlock()
	rp.snapshotEpoch++
	rp.windowFrom = 0
	if attempt != nil {
		attempt.committed = rp.snapshotEpoch
	}
	rp.snapshotMu.Unlock()
	rp.markAnswer(now)
	return rp.compactAfterApply(ctx)
}

type commentCopy struct{ item, body string }

// commentBodies is every comment copy's item and body, by id.
func commentBodies(ctx context.Context, tx *sql.Tx) (map[string]commentCopy, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, item, json FROM comments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]commentCopy{}
	for rows.Next() {
		var id, item, js string
		if err := rows.Scan(&id, &item, &js); err != nil {
			return nil, err
		}
		var c orchapi.Comment
		_ = json.Unmarshal([]byte(js), &c)
		out[id] = commentCopy{item: item, body: c.Body}
	}
	return out, rows.Err()
}

// applyRow upserts one snapshot row; base is the change id it is current at.
func applyRow(ctx context.Context, tx *sql.Tx, kind string, raw json.RawMessage, base string) error {
	switch kind {
	case orchapi.SnapItem:
		var it orchapi.WorkItem
		if json.Unmarshal(raw, &it) != nil || it.ID == "" {
			return nil
		}
		return putItem(ctx, tx, it, base, true)
	case orchapi.SnapComment:
		var c orchapi.Comment
		if json.Unmarshal(raw, &c) != nil || c.ID == "" {
			return nil
		}
		return putComment(ctx, tx, c)
	case orchapi.SnapLease:
		var l orchapi.Lease
		if json.Unmarshal(raw, &l) != nil || l.Key == "" {
			return nil
		}
		return putLease(ctx, tx, l)
	case orchapi.SnapMail:
		var m orchapi.Mail
		if json.Unmarshal(raw, &m) != nil || m.ID == "" {
			return nil
		}
		return putMail(ctx, tx, m)
	}
	return nil
}

// putItem upserts an item. keepLinks=false keeps the links the copy had
// (events carry items without links).
func putItem(ctx context.Context, tx *sql.Tx, it orchapi.WorkItem, base string, withLinks bool) error {
	if !withLinks {
		var old string
		if tx.QueryRowContext(ctx, `SELECT json FROM items WHERE id = ?`, it.ID).Scan(&old) == nil {
			var prev orchapi.WorkItem
			if json.Unmarshal([]byte(old), &prev) == nil {
				it.Links = prev.Links
			}
		}
	}
	it.Comments = nil
	js, _ := json.Marshal(it)
	_, err := tx.ExecContext(ctx, `INSERT INTO items (id, area, status, source, created, base, json, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET area = excluded.area, status = excluded.status, source = excluded.source,
			created = excluded.created, base = max(items.base, excluded.base), json = excluded.json, updated = excluded.updated`,
		it.ID, it.Area, string(it.Status), it.Source, store.Millis(it.Created), base, string(js), store.Millis(it.Updated))
	return err
}

func putComment(ctx context.Context, tx *sql.Tx, c orchapi.Comment) error {
	var old string
	_ = tx.QueryRowContext(ctx, `SELECT json FROM comments WHERE id = ?`, c.ID).Scan(&old)
	js, _ := json.Marshal(c)
	if _, err := tx.ExecContext(ctx, `INSERT INTO comments (id, item, json) VALUES (?, ?, ?) ON CONFLICT (id) DO UPDATE SET json = excluded.json`,
		c.ID, c.Item, string(js)); err != nil {
		return err
	}
	if old == "" {
		return nil
	}
	var prev orchapi.Comment
	_ = json.Unmarshal([]byte(old), &prev)
	return noteRedaction(ctx, tx, c.Item, prev.Body, c.Body)
}

func putLease(ctx context.Context, tx *sql.Tx, l orchapi.Lease) error {
	js, _ := json.Marshal(l)
	var exp int64
	if l.Expires != nil {
		exp = store.Millis(*l.Expires)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO leases (key, holder, bound, expires, json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET holder = excluded.holder, bound = excluded.bound, expires = excluded.expires, json = excluded.json`,
		l.Key, l.Holder, boolInt(l.Bound), exp, string(js))
	return err
}

func putMail(ctx context.Context, tx *sql.Tx, m orchapi.Mail) error {
	js, _ := json.Marshal(m)
	_, err := tx.ExecContext(ctx, `INSERT INTO mail (id, recipient, sender, needs, acked, reply_to, created, json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET recipient = excluded.recipient, sender = excluded.sender, needs = excluded.needs,
			acked = excluded.acked, reply_to = excluded.reply_to, json = excluded.json
		WHERE mail.acked <= excluded.acked`, // taken stays taken: an older answer arriving late never reopens it
		m.ID, m.Recipient, m.From, string(m.Needs), boolInt(m.AckedAt != nil), m.ReplyTo, store.Millis(m.Created), string(js))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// applyChanges applies one page of the changelog in one transaction and
// moves the cursor. It returns items to fetch whole (links changed, or an
// item came into view whose comments the copy lacks).
func (rp *replica) applyChanges(ctx context.Context, evs []api.Event, cursor string, now time.Time, leaseCommit ...*leaseCacheCommit) ([]string, error) {
	rp.snapshotMu.Lock()
	defer rp.snapshotMu.Unlock()
	var fetch []string
	err := rp.writeApplyCommitted(ctx, func(tx *sql.Tx) error {
		for _, e := range evs {
			f, err := applyEvent(ctx, tx, e)
			if err != nil {
				return err
			}
			fetch = append(fetch, f...)
		}
		if err := setMeta(ctx, tx, "cursor", cursor); err != nil {
			return err
		}
		ms, _ := json.Marshal(store.Millis(time.Now()))
		return setMeta(ctx, tx, "synced", string(ms))
	}, firstLeaseCommit(leaseCommit))
	if err != nil {
		return nil, err
	}
	rp.mu.Lock()
	rp.cursor = cursor
	rp.mu.Unlock()
	rp.markAnswer(now)
	return fetch, nil
}

// applyEvent applies one change. Unknown types are skipped (a newer
// authority may publish more than this replica keeps).
func applyEvent(ctx context.Context, tx *sql.Tx, e api.Event) ([]string, error) {
	switch e.Type {
	case orchapi.EventWorkCreated, orchapi.EventWorkStatusChanged, orchapi.EventWorkLinked, orchapi.EventWorkChanged:
		var it orchapi.WorkItem
		if json.Unmarshal(e.Data, &it) != nil || it.ID == "" {
			return nil, nil
		}
		if e.Type == orchapi.EventWorkCreated {
			if _, err := tx.ExecContext(ctx, `DELETE FROM moved_items WHERE id = ?`, it.ID); err != nil {
				return nil, err
			}
		} else {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM moved_items WHERE id = ?`, it.ID).Scan(&n); err != nil {
				return nil, err
			}
			if n != 0 {
				// A later event may be a move back into this reader's area.
				// Verify it at the authority before restoring the row.
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO refetch (id) VALUES (?)`, it.ID); err != nil {
					return nil, err
				}
				return []string{it.ID}, nil
			}
		}
		var n int
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE id = ?`, it.ID).Scan(&n)
		if err := putItem(ctx, tx, it, e.ID, false); err != nil {
			return nil, err
		}
		// A tracker item announced as created may have moved in with existing
		// links and comments; the event itself carries neither.
		if e.Type == orchapi.EventWorkLinked || (e.Type == orchapi.EventWorkCreated && it.Source != "local") || (n == 0 && e.Type != orchapi.EventWorkCreated) {
			return []string{it.ID}, nil
		}
	case orchapi.EventWorkCommented:
		var c orchapi.Comment
		if json.Unmarshal(e.Data, &c) != nil || c.ID == "" {
			return nil, nil
		}
		if err := putComment(ctx, tx, c); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET base = max(base, ?) WHERE id = ?`, e.ID, c.Item)
		return nil, err
	case orchapi.EventWorkClaimed:
		var l orchapi.Lease
		if json.Unmarshal(e.Data, &l) != nil || l.Key == "" {
			return nil, nil
		}
		if err := putLease(ctx, tx, l); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET base = max(base, ?) WHERE id = ?`, e.ID, l.Key)
		return nil, err
	case orchapi.EventWorkMoved:
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO moved_items (id, event) VALUES (?, ?)`, e.Subject, e.ID); err != nil {
			return nil, err
		}
		return nil, forgetMoved(ctx, tx, e.Subject) // it answers by its new id, in the project it went to
	case orchapi.EventWorkReleased, orchapi.EventWorkLeaseExpired:
		var l orchapi.Lease
		if json.Unmarshal(e.Data, &l) != nil || l.Key == "" {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE key = ?`, l.Key); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET base = max(base, ?) WHERE id = ?`, e.ID, l.Key)
		return nil, err
	case orchapi.EventWorkImported:
		var wi orchapi.WorkImported
		if json.Unmarshal(e.Data, &wi) == nil {
			return wi.Items, nil
		}
	case orchapi.EventMailSent, orchapi.EventMailAcked, orchapi.EventMailImported:
		var m orchapi.Mail
		if json.Unmarshal(e.Data, &m) != nil || m.ID == "" {
			return nil, nil
		}
		return nil, putMail(ctx, tx, m)
	case orchapi.EventIssueCommentRedacted:
		// A native issue's comment was redacted at the authority: refetch
		// the work items filed under it (their comment copies come back
		// redacted, and putComment scrubs what this copy kept).
		if e.Subject == "" {
			return nil, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM items WHERE id = ? OR id LIKE ?`, e.Subject, "%:"+e.Subject)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
		// Kept until the fetch lands, so a restart cannot lose it.
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO refetch (id) VALUES (?)`, id); err != nil {
				return nil, err
			}
		}
		return ids, nil
	case events.EventGone:
		// The item left every area this replica reads: it, its comments and
		// its lease go (SPEC 17.6 "Area transitions").
		return nil, forgetItem(ctx, tx, e.Subject)
	}
	return nil, nil
}

// forgetItem removes an item from the copy with everything that held its
// text: its comments, lease, pending refetch, and its settled outbox rows
// (a pending write still goes to the authority). The database is marked for
// compaction: an item can leave this replica's view before a redaction of
// it arrives, and the redaction event is then never delivered here.
func forgetItem(ctx context.Context, tx *sql.Tx, id string) error {
	if err := forgetMoved(ctx, tx, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE item = ? AND state != ?`, id, api.OutboxPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return setMeta(ctx, tx, metaCompact, "1")
	}
	return nil
}

// forgetMoved removes an item from the copy but keeps its outbox: the caller is
// still reading it elsewhere, and a failed write stays there to be seen.
func forgetMoved(ctx context.Context, tx *sql.Tx, id string) error {
	for _, q := range []string{`DELETE FROM items WHERE id = ?`, `DELETE FROM comments WHERE item = ?`, `DELETE FROM leases WHERE key = ?`,
		`DELETE FROM refetch WHERE id = ?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// putWhole stores an item fetched whole (GET /v0/work/{id}: links and
// comments) at base, the copy's cursor when the fetch began. When the row
// moved past base meanwhile (sync applied a newer change), the fetched state
// is older than the row: only its links and comments are taken.
func (rp *replica) putWhole(ctx context.Context, it orchapi.WorkItem, base string, epoch uint64) error {
	rp.snapshotMu.RLock()
	if rp.snapshotEpoch != epoch {
		rp.snapshotMu.RUnlock()
		return fmt.Errorf("item refresh predates snapshot")
	}
	err := rp.putWholeTx(ctx, it, base)
	rp.snapshotMu.RUnlock()
	if err != nil {
		return err
	}
	return rp.compactAfterApply(ctx)
}

func (rp *replica) putWholeTx(ctx context.Context, it orchapi.WorkItem, base string) error {
	return rp.writeApply(ctx, func(tx *sql.Tx) error {
		var movedAt string
		err := tx.QueryRowContext(ctx, `SELECT event FROM moved_items WHERE id = ?`, it.ID).Scan(&movedAt)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if base < movedAt {
				return errMovedRefresh
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM moved_items WHERE id = ?`, it.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM refetch WHERE id = ?`, it.ID); err != nil {
			return err
		}
		cs := it.Comments
		var cur, js string
		err = tx.QueryRowContext(ctx, `SELECT base, json FROM items WHERE id = ?`, it.ID).Scan(&cur, &js)
		if err == sql.ErrNoRows {
			var cursor string
			if e := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'cursor'`).Scan(&cursor); e != nil && e != sql.ErrNoRows {
				return e
			}
			if cursor > base {
				// A newer move, disappearance or snapshot may have removed it.
				// Retry the fetch at the new cursor instead of resurrecting it.
				return fmt.Errorf("item refresh predates replica cursor")
			}
		} else if err != nil {
			return err
		}
		if err == nil && cur > base {
			var row orchapi.WorkItem
			if json.Unmarshal([]byte(js), &row) == nil {
				row.Links = it.Links
				it = row
			}
			base = cur
		}
		if err := putItem(ctx, tx, it, base, true); err != nil {
			return err
		}
		for _, c := range cs {
			if c.Item == "" {
				c.Item = it.ID
			}
			if err := putComment(ctx, tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// dropItem removes an item the authority no longer shows this replica.
func (rp *replica) dropItem(ctx context.Context, id string) error {
	if err := rp.writeApply(ctx, func(tx *sql.Tx) error { return forgetItem(ctx, tx, id) }); err != nil {
		return err
	}
	return rp.compactAfterApply(ctx)
}

// maxLost bounds the items a restore's note remembers.
const maxLost = 1000

func itemIDs(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func encodeLost(ids []string) string {
	if len(ids) > maxLost {
		ids = ids[:maxLost]
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeLost(s string) map[string]bool {
	var ids []string
	_ = json.Unmarshal([]byte(s), &ids)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// lostToRestore: the home lost item in its last restore.
func (rp *replica) lostToRestore(item string) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return item != "" && rp.lost[item]
}
