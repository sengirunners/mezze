package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/issues/store/sqlite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

type recoveryHomes struct {
	orchcore.HomeService
	purged chan string
	fail   map[string]bool // projects whose purge fails

	// Like homes: a fresh purge marks the project's backups (ScrubDue), and a
	// purge replaces only marked backups. failBeforeMark makes a failing purge
	// fail before its mark is written (a loadHome or updateHome error).
	mu             sync.Mutex
	failBeforeMark bool
	due            map[string]bool
	calls          []purgeCall
}

// purgeCall is one PurgeBackups call: ran says it replaced the backups.
type purgeCall struct {
	pid        string
	fresh, ran bool
	err        bool
}

func (h *recoveryHomes) WaitIdle(context.Context, string) {}
func (h *recoveryHomes) PurgeBackups(ctx context.Context, pid string, fresh bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.due == nil {
		h.due = map[string]bool{}
	}
	if h.fail[pid] {
		if fresh && !h.failBeforeMark {
			h.due[pid] = true
		}
		h.calls = append(h.calls, purgeCall{pid: pid, fresh: fresh, err: true})
		return errors.New("purge failed")
	}
	ran := fresh || h.due[pid]
	delete(h.due, pid)
	h.calls = append(h.calls, purgeCall{pid: pid, fresh: fresh, ran: ran})
	if fresh {
		h.purged <- pid
	}
	return nil
}

// callsFor is the PurgeBackups calls made for pid so far.
func (h *recoveryHomes) callsFor(pid string) []purgeCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []purgeCall
	for _, c := range h.calls {
		if c.pid == pid {
			out = append(out, c)
		}
	}
	return out
}

type recoveryStore struct {
	istore.Store
	opens int
}

func (s *recoveryStore) Project(ctx context.Context, id string, create bool) (istore.ProjectStore, error) {
	s.opens++
	return s.Store.Project(ctx, id, create)
}

type recoveryFixture struct {
	s      *Service
	native *sqlite.Store
	spy    *recoveryStore
	homes  *recoveryHomes
	pid    string
	ps     istore.ProjectStore
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus}
	p, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	native := sqlite.Open(home)
	spy := &recoveryStore{Store: native}
	s := NewWithStore(d, spy)
	orchcore.Wire(d).Issues = s
	homes := &recoveryHomes{purged: make(chan string, 100)}
	orchcore.Wire(d).Homes = homes
	d.AddScrubber(func(ctx context.Context, sc core.Scrub) error {
		return bus.Scrub(ctx, sc.Project, sc.Subjects, sc.Texts, core.MinUnscoped, core.ScrubText, core.ScrubJSON)
	})
	ps, err := native.Project(ctx, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetPrefix(ctx, "rec"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &recoveryFixture{s: s, native: native, spy: spy, homes: homes, pid: p.ID, ps: ps}
}

func recoveryApply(t *testing.T, ps istore.ProjectStore, m istore.Mutation) istore.Result {
	t.Helper()
	m.Actor = istore.Actor{Name: "ivo", Address: "human:ivo"}
	r, err := ps.Apply(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *recoveryFixture) legacy(t *testing.T, title string) (string, string) {
	t.Helper()
	ctx := context.Background()
	r := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "legacy reopen fixture"}})
	id := r.Issue.ID
	recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpClose, ID: id})
	c := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpReopen, ID: id, Reopen: &orchapi.ReopenIssueRequest{Reason: title}}).Comment
	recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpCommentRedact, ID: id, CommentID: c.ID, Moderate: func(string) bool { return true }})
	// Restore only the duplicate left by the pre-upgrade redaction bug.
	if err := f.native.Copy(ctx, f.pid, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `UPDATE history SET comment = ? WHERE issue_id = ? AND kind = 'reopened'`, title, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.d.Store.Registry().ExecContext(ctx, `INSERT INTO issue_redactions (project, comment_id, issue_id, who, at) VALUES (?, ?, ?, 'machine:mini', 0)`, f.pid, c.ID, id); err != nil {
		t.Fatal(err)
	}
	return id, c.ID
}

func recoveryHasReason(t *testing.T, ps istore.ProjectStore, id, reason string) bool {
	t.Helper()
	hs, err := ps.History(context.Background(), id, istore.Areas{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Kind == "reopened" && h.Comment == reason {
			return true
		}
	}
	return false
}

func (f *recoveryFixture) anotherProject(t *testing.T, slug, prefix string) (string, istore.ProjectStore) {
	t.Helper()
	ctx := context.Background()
	p, err := f.s.d.Projects.Create(ctx, f.s.d.Projects.Local(""), api.NewProjectRequest{Slug: slug})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := f.native.Project(ctx, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetPrefix(ctx, prefix); err != nil {
		t.Fatal(err)
	}
	return p.ID, ps
}

func recoveryMove(t *testing.T, src istore.ProjectStore, source string, dst istore.ProjectStore, dest, id, nid string) {
	t.Helper()
	recoveryMoveAt(t, src, source, dst, dest, id, nid, time.Time{})
}

// recoveryMoveAt is recoveryMove with every step at at (zero: now).
func recoveryMoveAt(t *testing.T, src istore.ProjectStore, source string, dst istore.ProjectStore, dest, id, nid string, at time.Time) {
	t.Helper()
	mv, err := src.(istore.Mover).Export(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	recoveryApply(t, dst, istore.Mutation{Op: istore.OpMoveIn, ID: id, At: at, Move: &istore.Move{Project: source, NewID: nid, From: &mv}})
	recoveryApply(t, src, istore.Mutation{Op: istore.OpMoveOut, ID: id, At: at, Move: &istore.Move{Project: dest, NewID: nid}})
	recoveryApply(t, dst, istore.Mutation{Op: istore.OpMoveDone, ID: id, At: at, Move: &istore.Move{Project: source, NewID: nid}})
}

func TestRedactionRecoveryFollowsRecordedMoveAndIsIdempotent(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const reason = "legacy-moved-reason"
	id, cid := f.legacy(t, reason)
	middle, mid := f.anotherProject(t, "intermediate", "mid")
	dest, ps := f.anotherProject(t, "destination", "dst")
	const midID = "mid-old"
	const nid = "dst-old"
	for _, at := range []struct{ project, id string }{{f.pid, id}, {middle, midID}} {
		if _, err := f.s.d.Events.Publish(ctx, events.Input{Project: at.project, Type: orchapi.EventWorkCommented, Subject: at.id, Data: map[string]string{"body": reason}}); err != nil {
			t.Fatal(err)
		}
	}
	recoveryMove(t, f.ps, f.pid, mid, middle, id, midID)
	recoveryMove(t, mid, middle, ps, dest, midID, nid)
	// Keep an old destination copy outside scheduled backups: its restore
	// must consult the propagated registry redaction, not resurrect the reason.
	oldCopy := filepath.Join(t.TempDir(), "old.db")
	if err := f.native.Copy(ctx, dest, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `VACUUM INTO ?`, oldCopy)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.s.recoverRedactions(ctx)
	if recoveryHasReason(t, ps, nid, reason) {
		t.Fatal("recovery did not follow the recorded destination")
	}
	purged := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case pid := <-f.homes.purged:
			purged[pid] = true
		default:
			t.Fatal("moved recovery did not purge every visited project's backups")
		}
	}
	for _, pid := range []string{f.pid, middle, dest} {
		if !purged[pid] {
			t.Fatalf("project %s backups were not purged", pid)
		}
		db, err := f.s.d.Store.Project(f.s.d.System().Scope, authz.Read, pid)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE instr(json, ?) > 0`, reason).Scan(&n); err != nil || n != 0 {
			t.Fatalf("visited project's event copies: %d %v", n, err)
		}
		for _, suffix := range []string{"", "-wal"} {
			b, err := os.ReadFile(filepath.Join(f.s.d.Store.ProjectDir(pid), sqlite.FileName) + suffix)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(b), reason) {
				t.Fatal("visited native store retains a reason in free pages or WAL")
			}
		}
	}
	var who string
	var at int64
	if err := f.s.d.Store.Registry().QueryRow(`SELECT who, at FROM issue_redactions WHERE project = ? AND comment_id = ? AND issue_id = ?`, dest, cid, nid).Scan(&who, &at); err != nil || who != "machine:mini" || at != 0 {
		t.Fatalf("mapped redaction lost original identity: %s %d %v", who, at, err)
	}
	cp, done, err := f.native.OpenCopy(ctx, dest, oldCopy)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if n, err := f.s.redactListed(ctx, dest, cp, nil); err != nil || n != 1 || recoveryHasReason(t, cp, nid, reason) {
		t.Fatalf("destination restore did not reapply mapped redaction: %d %v", n, err)
	}
	f.s.recovery = redactionRecovery{}
	f.s.recoverRedactions(ctx)
	select {
	case <-f.homes.purged:
		t.Fatal("clean recovery unnecessarily repeated backup work")
	default:
	}
}

func TestRedactionRecoveryRequiresRecordedProject(t *testing.T) {
	f := newRecoveryFixture(t)
	const reason = "legacy-provenance-reason"
	id, cid := f.legacy(t, reason)
	empty, _ := f.anotherProject(t, "old-empty-project", "old")
	// A stale or mismatched registry record must not resolve the globally
	// visible issue in f.pid and redact it under another project's authority.
	if _, err := f.s.d.Store.Registry().Exec(`UPDATE issue_redactions SET project = ? WHERE comment_id = ?`, empty, cid); err != nil {
		t.Fatal(err)
	}
	f.s.recoverRedactions(context.Background())
	if !recoveryHasReason(t, f.ps, id, reason) {
		t.Fatal("mismatched registry provenance modified another project")
	}
	select {
	case <-f.homes.purged:
		t.Fatal("mismatched registry provenance purged backups")
	default:
	}
}

func TestRedactionRecoveryStartsAutomatically(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const reason = "legacy-secret-startup"
	id, _ := f.legacy(t, reason)
	if _, err := f.s.d.Events.Publish(ctx, events.Input{Project: f.pid, Type: orchapi.EventWorkCommented, Subject: id, Data: map[string]string{"body": reason}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case pid := <-f.homes.purged:
		if pid != f.pid {
			t.Fatalf("purged %s, want %s", pid, f.pid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not repair the old redaction")
	}
	if recoveryHasReason(t, f.ps, id, reason) {
		t.Fatal("startup retained the legacy reopen reason")
	}
	db, err := f.s.d.Store.Project(f.s.d.System().Scope, authz.Read, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE instr(json, ?) > 0`, reason).Scan(&n); err != nil || n != 0 {
		t.Fatalf("event copies after recovery: %d %v", n, err)
	}
}

func TestRedactionRecoveryPreservesWriteHolds(t *testing.T) {
	f := newRecoveryFixture(t)
	const reason = "held-legacy-secret"
	id, _ := f.legacy(t, reason)
	release, err := f.s.HoldWrites(f.pid, "test recovery hold")
	if err != nil {
		t.Fatal(err)
	}
	f.s.recoverRedactions(context.Background())
	if !recoveryHasReason(t, f.ps, id, reason) || !f.s.recovery.retry {
		t.Fatal("recovery ignored a write hold")
	}
	release()
	for i := 0; i < 4 && !f.s.recovery.done; i++ {
		f.s.recoverRedactions(context.Background())
	}
	if recoveryHasReason(t, f.ps, id, reason) || !f.s.recovery.done {
		t.Fatal("recovery did not retry after the hold ended")
	}
}

func TestRedactionRecoveryEmptyAndBounded(t *testing.T) {
	f := newRecoveryFixture(t)
	f.s.recoverRedactions(context.Background())
	if !f.s.recovery.done || f.spy.opens != 0 {
		t.Fatalf("empty recovery: done=%v opened stores=%d", f.s.recovery.done, f.spy.opens)
	}
	f.s.recovery = redactionRecovery{}
	for i := 0; i < redactionRecoveryBatch+1; i++ {
		if _, err := f.s.d.Store.Registry().Exec(`INSERT INTO issue_redactions VALUES ('joined-only', ?, 'remote-missing', 'machine:other', 0)`, fmt.Sprintf("comment%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	f.s.recoverRedactions(context.Background())
	if f.s.recovery.comment != "comment031" || f.spy.opens != 0 {
		t.Fatalf("batch or authority boundary: %+v opens=%d", f.s.recovery, f.spy.opens)
	}
	f.s.recoverRedactions(context.Background())
	f.s.recoverRedactions(context.Background())
	if !f.s.recovery.done || f.s.recovery.comment != "comment032" {
		t.Fatalf("second batch: %+v", f.s.recovery)
	}
}

func TestRedactionRecoveryRetainsFailedScrubAndCancels(t *testing.T) {
	f := newRecoveryFixture(t)
	const reason = "legacy-retry-secret"
	id, cid := f.legacy(t, reason)
	var mu sync.Mutex
	fail := true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.d.AddScrubber(func(ctx context.Context, sc core.Scrub) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			cancel() // cancel after the store write, while copies still need work
			return errors.New("temporary scrub failure")
		}
		return nil
	})
	if err := f.s.recoverRedaction(ctx, f.pid, id, cid); err == nil {
		t.Fatal("failed maintenance scrub reported success")
	}
	f.s.mu.Lock()
	pending := f.s.pendingScrub[f.pid+"\x00"+id+"\x00"+cid]
	f.s.mu.Unlock()
	if !strings.Contains(strings.Join(pending.texts, "|"), reason) {
		t.Fatal("failed maintenance lost recovered text")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	f.s.retryPendingScrubs(context.Background())
	f.s.mu.Lock()
	n := len(f.s.pendingScrub)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatal("pending scrub did not finish")
	}
	select {
	case <-f.homes.purged:
	default:
		t.Fatal("retry did not purge backups")
	}
}

// The write committed, but the response view's Get observes cancellation.
// This boundary precedes finishRedaction, unlike a scrubber that fails later.
type responseCanceledStore struct {
	istore.Store
	cancel  context.CancelFunc
	failGet bool
}

func (s *responseCanceledStore) Project(ctx context.Context, id string, create bool) (istore.ProjectStore, error) {
	ps, err := s.Store.Project(ctx, id, create)
	return &responseCanceledProject{ProjectStore: ps, owner: s}, err
}

type responseCanceledProject struct {
	istore.ProjectStore
	owner *responseCanceledStore
}

func (p *responseCanceledProject) Apply(ctx context.Context, m istore.Mutation) (istore.Result, error) {
	r, err := p.ProjectStore.Apply(ctx, m)
	if err == nil && m.Op == istore.OpCommentRedact && len(r.Redacted) > 0 {
		p.owner.failGet = true
	}
	return r, err
}

func (p *responseCanceledProject) Get(ctx context.Context, id string, areas istore.Areas) (orchapi.Issue, error) {
	if p.owner.failGet {
		p.owner.failGet = false
		p.owner.cancel()
		return orchapi.Issue{}, context.Canceled
	}
	return p.ProjectStore.Get(ctx, id, areas)
}

func TestRedactionRecoveryRetainsTextAfterResponseGetCancellation(t *testing.T) {
	f := newRecoveryFixture(t)
	const reason = "legacy-postcommit-reason"
	id, cid := f.legacy(t, reason)
	if _, err := f.s.d.Events.Publish(context.Background(), events.Input{Project: f.pid, Type: orchapi.EventWorkCommented, Subject: id, Data: map[string]string{"body": reason}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.st = &responseCanceledStore{Store: f.s.st, cancel: cancel}
	err := f.s.recoverRedaction(ctx, f.pid, id, cid)
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("response Get did not cancel after commit: %v", err)
	}
	if recoveryHasReason(t, f.ps, id, reason) {
		t.Fatal("the cancellation fixture did not commit the history repair")
	}
	f.s.mu.Lock()
	pending := f.s.pendingScrub[f.pid+"\x00"+id+"\x00"+cid]
	f.s.mu.Unlock()
	if !strings.Contains(strings.Join(pending.texts, "|"), reason) {
		t.Fatal("postcommit response failure lost recovered text")
	}
	f.s.retryPendingScrubs(context.Background())
	f.s.mu.Lock()
	n := len(f.s.pendingScrub)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatal("postcommit pending scrub did not finish")
	}
	db, err := f.s.d.Store.Project(f.s.d.System().Scope, authz.Read, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE instr(json, ?) > 0`, reason).Scan(&n); err != nil || n != 0 {
		t.Fatalf("postcommit retry retained event copies: %d %v", n, err)
	}
	select {
	case <-f.homes.purged:
	default:
		t.Fatal("postcommit retry did not purge backups")
	}
}

type moveOnRecoveryLookupStore struct {
	istore.Store
	project, issue string
	move           func()
	fired          bool
}

func (s *moveOnRecoveryLookupStore) Project(ctx context.Context, id string, create bool) (istore.ProjectStore, error) {
	ps, err := s.Store.Project(ctx, id, create)
	if err != nil {
		return nil, err
	}
	return &moveOnRecoveryLookupProject{ProjectStore: ps, Mover: ps.(istore.Mover), owner: s, project: id}, nil
}

type moveOnRecoveryLookupProject struct {
	istore.ProjectStore
	istore.Mover
	owner   *moveOnRecoveryLookupStore
	project string
}

func (p *moveOnRecoveryLookupProject) Get(ctx context.Context, id string, areas istore.Areas) (orchapi.Issue, error) {
	it, err := p.ProjectStore.Get(ctx, id, areas)
	if err == nil && !p.owner.fired && p.project == p.owner.project && id == p.owner.issue {
		p.owner.fired = true
		p.owner.move() // move after the lookup snapshot, before mutate takes its gate
	}
	return it, err
}

func TestRedactionRecoveryRetriesMoveBetweenLookupAndWrite(t *testing.T) {
	f := newRecoveryFixture(t)
	const reason = "legacy-racing-move-reason"
	id, cid := f.legacy(t, reason)
	dest, ps := f.anotherProject(t, "racing-destination", "dst")
	const nid = "dst-raced"
	wrap := &moveOnRecoveryLookupStore{Store: f.s.st, project: f.pid, issue: id}
	wrap.move = func() { recoveryMove(t, f.ps, f.pid, ps, dest, id, nid) }
	f.s.st = wrap
	if err := f.s.recoverRedaction(context.Background(), f.pid, id, cid); err != nil {
		t.Fatal(err)
	}
	if !wrap.fired || recoveryHasReason(t, ps, nid, reason) {
		t.Fatal("lookup/write race permanently skipped the moved comment")
	}
	purged := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case pid := <-f.homes.purged:
			purged[pid] = true
		default:
			t.Fatal("racing move omitted a visited backup purge")
		}
	}
	if !purged[f.pid] || !purged[dest] {
		t.Fatalf("racing move purged projects %v", purged)
	}
}

func TestRedactionRecoveryRetainsEachMovedLocationOnFailure(t *testing.T) {
	f := newRecoveryFixture(t)
	id, cid := f.legacy(t, "legacy-moved-pending-reason")
	dest, ps := f.anotherProject(t, "pending-destination", "dst")
	const nid = "dst-pending"
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failing := true
	f.s.d.AddScrubber(func(context.Context, core.Scrub) error {
		if failing {
			cancel()
			return context.Canceled
		}
		return nil
	})
	if err := f.s.recoverRedaction(ctx, f.pid, id, cid); err == nil {
		t.Fatal("failed moved scrub reported success")
	}
	f.s.mu.Lock()
	_, sourcePending := f.s.pendingScrub[f.pid+"\x00"+id+"\x00"+cid]
	_, destPending := f.s.pendingScrub[dest+"\x00"+nid+"\x00"+cid]
	f.s.mu.Unlock()
	if !sourcePending || !destPending {
		t.Fatalf("per-location retry retained: source=%v destination=%v", sourcePending, destPending)
	}
	failing = false
	f.s.retryPendingScrubs(context.Background())
	f.s.mu.Lock()
	n := len(f.s.pendingScrub)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatal("moved per-location retries did not finish")
	}
	if len(f.homes.purged) != 2 {
		t.Fatal("moved per-location retries did not purge both backup sets")
	}
}
