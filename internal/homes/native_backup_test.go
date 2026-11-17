package homes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/issues"
	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

// nativeHarness is the homes module with a real issue store under a native
// tracker of project harbor.
type nativeHarness struct {
	*harness
	tr api.Tracker
}

func newNativeHarness(t *testing.T) *nativeHarness {
	t.Helper()
	h := newHarness(t)
	svc := issues.New(h.d)
	t.Cleanup(func() { svc.Close() })
	orchcore.Wire(h.d).Issues = svc
	ps, err := svc.Store().Project(h.ctx, h.harbor.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetPrefix(h.ctx, "oc"); err != nil {
		t.Fatal(err)
	}
	tr := api.Tracker{Adapter: orchapi.AdapterNative, Prefix: "issue"}
	if _, err := orchcore.SetTrackers(h.ctx, h.d.Projects, h.harbor.ID, func(old []api.Tracker) ([]api.Tracker, error) {
		return append(old, tr), nil
	}); err != nil {
		t.Fatal(err)
	}
	return &nativeHarness{harness: h, tr: tr}
}

// rec schedules a backup straight in home.json, as addBackup would.
func (h *nativeHarness) rec(target, base string, every, keep int) backupRec {
	h.t.Helper()
	if err := os.MkdirAll(base, 0o700); err != nil {
		h.t.Fatal(err)
	}
	b := backupRec{ID: target, Target: target, Base: base, Kind: orchapi.BackupSqlite, Every: every, Keep: keep, Created: h.s.now().UTC()}
	if _, err := h.s.updateHome(h.harbor.ID, h.tr.Prefix, func(st *homeState) error {
		st.Backups = append(st.Backups, b)
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *nativeHarness) load(b backupRec) backupRec {
	h.t.Helper()
	st, err := h.s.loadHome(h.harbor.ID, h.tr.Prefix)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, x := range st.Backups {
		if x.Target == b.Target {
			return x
		}
	}
	h.t.Fatalf("no record %s", b.Target)
	return b
}

func (h *nativeHarness) run(b backupRec) (orchapi.BackupResult, error) {
	return h.s.runBackup(h.ctx, h.harbor.ID, "harbor", h.tr, h.load(b))
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func nativeDir(base string) string { return backupDir(base, "harbor", "issue", orchapi.BackupSqlite) }

// Retention is per suffix, keeps the newest, and a keep of 0 keeps one.
func TestLocalTierPrunes(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 2)
	for range 4 {
		if _, err := h.run(b); err != nil {
			t.Fatal(err)
		}
		h.clock.add(time.Hour)
	}
	if got := files(t, nativeDir(base)); len(got) != 4 {
		t.Fatalf("keep 2 of a set of two files: %v", got)
	}
}

// Two runs in one millisecond do not overwrite a copy.
func TestCopyNamesAreUnique(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	for range 2 {
		if _, err := h.run(b); err != nil {
			t.Fatal(err)
		}
	}
	if got := files(t, nativeDir(base)); len(got) != 4 {
		t.Fatalf("copies %v", got)
	}
}

// A run removes what a crash left: the staging and the half-written copy
// hold what a copy holds, and no scrubber looks there.
func TestRunSweepsLeftovers(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	dir := nativeDir(base)
	stage := h.s.localScratch(h.harbor.ID, "issue", "restore-crashed")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	move := h.s.localScratch(h.harbor.ID, "issue", "01jmoveid") // a move's: logged, not ours
	if err := os.MkdirAll(move, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "harbor-issue-20260101T000000.000Z.issues.db.tmp")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	if pathExists(stage) || pathExists(tmp) {
		t.Fatal("leftovers stayed")
	}
	if !pathExists(move) {
		t.Fatal("a move's staging was removed")
	}
}

// A set is whole or gone: a run that fails on project.db leaves no store copy,
// or an hourly failure would fill a disk (only a good run prunes).
func TestAFailedSetLeavesNoCopy(t *testing.T) {
	h := newNativeHarness(t)
	dir := t.TempDir()
	// A project with an issue store and no project.db.
	if _, _, err := h.s.writeCopies(h.ctx, "01NOPROJECTDB", "issue", dir, "n", 0); err == nil || !strings.Contains(err.Error(), "project.db") {
		t.Fatalf("err %v", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

// A target that vanished is a failure, not a success with no file; last_ok
// stays put and the failure shows.
func TestMissingTargetFailsTheRun(t *testing.T) {
	h := newNativeHarness(t)
	base := filepath.Join(t.TempDir(), "volume")
	b := h.rec("file://"+base, base, 3600, 5)
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if res, err := h.run(b); err == nil || res.Ran {
		t.Fatalf("%+v %v", res, err)
	}
	x := h.load(b)
	if x.LastOK != nil || x.Failures != 1 || x.LastError == "" {
		t.Fatalf("record %+v", x)
	}
}

// A store emptied by mistake cannot rotate the good copies out.
func TestShrunkStoreKeepsGoodCopies(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 1)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	good := files(t, nativeDir(base))
	x := h.load(b)
	if _, err := h.s.updateHome(h.harbor.ID, "issue", func(st *homeState) error {
		st.Backups[0].StoreBytes = x.StoreBytes * 100
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Hour)
	if _, err := h.run(b); err == nil || !strings.Contains(err.Error(), "under 1/") {
		t.Fatalf("a 99%% smaller copy: %v", err)
	}
	if got := files(t, nativeDir(base)); strings.Join(got, " ") != strings.Join(good, " ") {
		t.Fatalf("copies %v, were %v", got, good)
	}
	if y := h.load(b); !y.LastOK.Equal(*x.LastOK) {
		t.Fatalf("last_ok moved: %v", y.LastOK)
	}
}

// The redaction order, backup by backup: a target that fails keeps its copies
// and its mark while the other one is purged, the scheduler picks the failing
// one up after its retry, and then it is purged too.
func TestPurgeIsPerBackupAndSurvivesInTheRecord(t *testing.T) {
	h := newNativeHarness(t)
	goodBase, flakyBase := t.TempDir(), filepath.Join(t.TempDir(), "volume")
	good := h.rec("file://"+goodBase, goodBase, 3600, 5)
	flaky := h.rec("file://"+flakyBase, flakyBase, 3600, 5)
	for _, b := range []backupRec{good, flaky} {
		if _, err := h.run(b); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.add(time.Minute)
	if _, err := h.run(good); err != nil { // two sets
		t.Fatal(err)
	}
	h.clock.add(time.Minute)
	oldFlaky := files(t, nativeDir(flakyBase))
	// The volume goes away.
	if err := os.Rename(flakyBase, flakyBase+".away"); err != nil {
		t.Fatal(err)
	}
	err := h.s.PurgeBackups(h.ctx, h.harbor.ID, true)
	if err == nil || !strings.Contains(err.Error(), flakyBase) {
		t.Fatalf("purge: %v", err)
	}
	if got := files(t, nativeDir(goodBase)); len(got) != 2 {
		t.Fatalf("copies after the purge: %v", got)
	}
	if x := h.load(good); x.ScrubDue != nil {
		t.Fatalf("good: %+v", x)
	}
	x := h.load(flaky)
	if x.ScrubDue == nil || x.Failures != 1 {
		t.Fatalf("flaky: %+v", x)
	}
	// It comes back; a stopped daemon lost nothing: the mark is in home.json.
	if err := os.Rename(flakyBase+".away", flakyBase); err != nil {
		t.Fatal(err)
	}
	if !dueAt(h.load(flaky)).After(h.clock.now()) {
		t.Fatal("a failing backup ran again at once")
	}
	h.clock.add(retryAfterFailure)
	h.s.backupDue(h.ctx)
	h.s.wg.Wait()
	if x := h.load(flaky); x.ScrubDue != nil || x.Failures != 0 {
		t.Fatalf("after the retry: %+v", x)
	}
	got := files(t, nativeDir(flakyBase))
	if len(got) != 2 || got[0] == oldFlaky[0] {
		t.Fatalf("copies %v, were %v", got, oldFlaky)
	}
}

// A prefix edit must not turn copies holding old text into a successful
// purge. Report a gap both when home.json stays behind and when it moves to
// the new prefix. Active backups still replace their own copies first.
func TestPurgeReportsOldPrefixCopies(t *testing.T) {
	for _, moveState := range []bool{false, true} {
		t.Run(fmt.Sprintf("move-state=%t", moveState), func(t *testing.T) {
			h := newNativeHarness(t)
			base := t.TempDir()
			b := h.rec("file://"+base, base, 3600, 5)
			if _, err := h.run(b); err != nil {
				t.Fatal(err)
			}
			oldDir := nativeDir(base)
			oldFiles := files(t, oldDir)
			// Legacy and interrupted copies need the same gap as encrypted ones.
			for _, suf := range []string{plainIssues, plainProject, copyIssues + ".tmp"} {
				name := "harbor-issue-20260101T000000.000Z" + suf
				if err := os.WriteFile(filepath.Join(oldDir, name), []byte("old text"), 0o600); err != nil {
					t.Fatal(err)
				}
				oldFiles = append(oldFiles, name)
			}
			oldState := h.s.homeStateFile(h.harbor.ID, h.tr.Prefix)
			h.tr.Prefix = "fresh"
			if _, err := orchcore.SetTrackers(h.ctx, h.d.Projects, h.harbor.ID, func(trs []api.Tracker) ([]api.Tracker, error) {
				for i := range trs {
					if trs[i].Adapter == orchapi.AdapterNative {
						trs[i] = h.tr
					}
				}
				return trs, nil
			}); err != nil {
				t.Fatal(err)
			}
			if moveState {
				dst := h.s.homeStateFile(h.harbor.ID, h.tr.Prefix)
				if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(oldState, dst); err != nil {
					t.Fatal(err)
				}
			} else {
				// The old target is no longer named by any current tracker.
				base = t.TempDir()
				b = h.rec("file://"+base, base, 3600, 5)
			}
			for range 2 {
				h.clock.add(time.Minute)
				if _, err := h.run(b); err != nil {
					t.Fatal(err)
				}
			}
			err := h.s.PurgeBackups(h.ctx, h.harbor.ID, true)
			if err == nil {
				t.Fatal("old-prefix copies were reported fully scrubbed")
			}
			for _, n := range oldFiles {
				p := filepath.Join(oldDir, n)
				if !strings.Contains(err.Error(), p) || !pathExists(p) {
					t.Errorf("old copy must be reported and kept: %s; purge: %v", p, err)
				}
			}
			if got := files(t, backupDir(base, "harbor", "fresh", orchapi.BackupSqlite)); len(got) != 2 || h.load(b).ScrubDue != nil {
				t.Fatalf("active backup did not finish its purge: %v", got)
			}
			if err := h.s.PurgeBackups(h.ctx, h.harbor.ID, false); err == nil {
				t.Fatal("retry lost the old-prefix gap")
			}
			if err := os.RemoveAll(oldDir); err != nil {
				t.Fatal(err)
			}
			if err := h.s.PurgeBackups(h.ctx, h.harbor.ID, false); err != nil {
				t.Fatalf("gap stayed after old copies were removed: %v", err)
			}
		})
	}
}

// A migrated directory can still hold files named with the former prefix.
func TestPurgeReportsOldStemsInCurrentDirectory(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	dir := nativeDir(base)
	old := filepath.Join(dir, "harbor-former-issue-20260101T000000.000Z"+copyProject)
	if err := os.WriteFile(old, []byte("old text"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Neither other projects nor exports belong to this sqlite purge, even a
	// project whose slug extends this one ("harbor-ios-issue" is not prefix
	// "ios-issue" of harbor: harbor-ios uses prefix "issue").
	ios, err := h.d.Projects.Create(h.ctx, h.owner, api.NewProjectRequest{Slug: "harbor-ios"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchcore.SetTrackers(h.ctx, h.d.Projects, ios.ID, func(old []api.Tracker) ([]api.Tracker, error) {
		return append(old, api.Tracker{Adapter: orchapi.AdapterNative, Prefix: "issue"}), nil
	}); err != nil {
		t.Fatal(err)
	}
	// But harbor-ios never used prefix "app": "harbor-ios-app" holds harbor's
	// copy under its forgotten old prefix "ios-app".
	sibling := filepath.Join(backupDir(base, "harbor", "ios-app", orchapi.BackupSqlite), "harbor-ios-app-20260101T000000.000Z"+copyIssues)
	if err := os.MkdirAll(filepath.Dir(sibling), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("old text"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := []string{
		filepath.Join(backupDir(base, "side", "issue", orchapi.BackupSqlite), "side-issue-20260101T000000.000Z"+copyIssues),
		filepath.Join(backupDir(base, "harbor-ios", "issue", orchapi.BackupSqlite), "harbor-ios-issue-20260101T000000.000Z"+copyIssues),
		filepath.Join(backupDir(base, "harbor", "retired", orchapi.BackupExport), "harbor-retired-20260101T000000Z.jsonl"),
	}
	for _, p := range unrelated {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.add(time.Minute)
	// The redaction's hint names each copy to delete.
	err = h.s.PurgeBackups(h.ctx, h.harbor.ID, true)
	got, other := orchcore.UnreplacedCopies(err)
	want := []string{old, sibling}
	slices.Sort(got)
	slices.Sort(want)
	if other || !slices.Equal(got, want) {
		t.Fatalf("old copies reported: %q (other errors %v), want %q: %v", got, other, want, err)
	}
	for _, p := range got {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.PurgeBackups(h.ctx, h.harbor.ID, false); err != nil {
		t.Fatalf("unrelated files reported as a gap: %v", err)
	}
	for _, p := range unrelated {
		if got, err := os.ReadFile(p); err != nil || string(got) != "unrelated" {
			t.Fatalf("unrelated copy changed: %s: %q, %v", p, got, err)
		}
	}
	// The mirror: harbor is known to use "ios-app" (its tracker folder) and
	// harbor-ios is not known to use "app", so harbor-ios-app holds harbor's
	// copies: harbor reports them, harbor-ios, scanning the same base, does not.
	if _, err := h.s.updateHome(ios.ID, "issue", func(st *homeState) error {
		st.Backups = append(st.Backups, backupRec{ID: b.Target, Target: b.Target, Base: base, Kind: orchapi.BackupSqlite, Every: 3600, Keep: 5, Created: h.s.now().UTC()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(orchcore.Trackers(h.d).TrackerDir(h.harbor.ID, "ios-app"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("old text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := orchcore.UnreplacedCopies(h.s.PurgeBackups(h.ctx, ios.ID, false)); len(got) > 0 {
		t.Fatalf("harbor-ios reported harbor's copies as its own: %q", got)
	}
	if got, _ := orchcore.UnreplacedCopies(h.s.PurgeBackups(h.ctx, h.harbor.ID, false)); !slices.Equal(got, []string{sibling}) {
		t.Fatalf("harbor's copy under its known prefix ios-app: %q, want %q", got, sibling)
	}
}

// failing makes one step of the issues module fail.
type failing struct {
	orchcore.IssueService
	snapshot, restore error
}

func (f *failing) SnapshotNative(ctx context.Context, project, dst string, prev int64) (int64, error) {
	if f.snapshot != nil {
		return 0, f.snapshot
	}
	return f.IssueService.SnapshotNative(ctx, project, dst, prev)
}

func (f *failing) RestoreNative(ctx context.Context, project, path string, dry bool) (orchapi.IssueRestoreResult, error) {
	if !dry && f.restore != nil {
		return orchapi.IssueRestoreResult{}, f.restore
	}
	return f.IssueService.RestoreNative(ctx, project, path, dry)
}

// A swap that fails says where the current store went: the answer is an error,
// which carries no result.
func TestFailedSwapNamesTheKeptCopy(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	ps, err := orchcore.Issues(h.d).(*issues.Service).Store().Project(h.ctx, h.harbor.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Apply(h.ctx, istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "history"}, At: h.clock.now(),
		Actor: istore.Actor{Name: "ana"}, Authorize: func(string, string) error { return nil }, AreaOf: func(string, []string) string { return "" }}); err != nil {
		t.Fatal(err)
	}
	orchcore.Wire(h.d).Issues = &failing{IssueService: orchcore.Issues(h.d), restore: api.Errorf(api.CodeUnavailable, "replacing the store failed: boom")}
	h.clock.add(time.Minute)
	_, err = orchclient.RestoreIssues(h.ctx, h.as(h.owner, "harbor"), orchapi.IssueRestoreRequest{})
	if !isCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), nativeDir(base)) {
		t.Fatalf("err %v", err)
	}
}

// A disk that fills during a run leaves no new copy, the older ones, last_ok
// where it was, and the failure on show.
func TestDiskFullKeepsOlderCopies(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 1)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	before, was := files(t, nativeDir(base)), h.load(b)
	orchcore.Wire(h.d).Issues = &failing{IssueService: orchcore.Issues(h.d), snapshot: fmt.Errorf("VACUUM INTO: %w", syscall.ENOSPC)}
	h.clock.add(time.Hour)
	if _, err := h.run(b); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("err %v", err)
	}
	if got := files(t, nativeDir(base)); strings.Join(got, " ") != strings.Join(before, " ") {
		t.Fatalf("copies %v, were %v", got, before)
	}
	if x := h.load(b); !x.LastOK.Equal(*was.LastOK) || x.Failures != 1 {
		t.Fatalf("record %+v", x)
	}
}

// A copy does not sit on one of project.db's pool connections: it runs with
// the whole pool taken.
func TestProjectCopyTakesNoPoolConnection(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	db, err := h.d.Store.Project(authz.All(), authz.Read, h.harbor.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range db.Stats().MaxOpenConnections {
		c, err := db.Conn(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	if _, err := h.s.runBackup(ctx, h.harbor.ID, "harbor", h.tr, h.load(b)); err != nil {
		t.Fatalf("with the pool taken: %v", err)
	}
}

// WaitIdle waits out a running operation, and gives up at its bound.
func TestWaitIdle(t *testing.T) {
	h := newNativeHarness(t)
	release, err := h.s.ops.acquire(h.harbor.ID, "issue", "a backup")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()
	t0 := time.Now()
	h.s.WaitIdle(h.ctx, h.harbor.ID)
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("returned after %v, with the lock held", d)
	}
	if h.s.ops.running(h.harbor.ID, "issue") != "" {
		t.Fatal("returned with the lock held")
	}
	release2, err := h.s.ops.acquire(h.harbor.ID, "issue", "a backup")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	old := opsWait
	opsWait = 100 * time.Millisecond
	defer func() { opsWait = old }()
	t0 = time.Now()
	h.s.WaitIdle(h.ctx, h.harbor.ID)
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("waited %v past its bound", d)
	}
}

// relistFails is a mirror that does not come back after the swap.
type relistFails struct {
	orchcore.TrackerHost
	fail bool
	n    int
}

func (r *relistFails) Relist(ctx context.Context, project string, tr api.Tracker) error {
	r.n++
	if r.fail {
		return errors.New("mirror down")
	}
	return r.TrackerHost.Relist(ctx, project, tr)
}

// A failure between the swap and the mirror's relist is a restore that
// happened, and says so; restoring again finishes it.
func TestRelistFailureAfterTheSwap(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	r := &relistFails{TrackerHost: orchcore.Trackers(h.d), fail: true}
	orchcore.Wire(h.d).Trackers = r
	owner := h.as(h.owner, "harbor")
	res, err := orchclient.RestoreIssues(h.ctx, owner, orchapi.IssueRestoreRequest{})
	if err != nil || r.n != 1 || res.DryRun || !strings.Contains(strings.Join(res.Notes, "\n"), "mirror down") {
		t.Fatalf("%+v %v (%d relists)", res, err, r.n)
	}
	r.fail = false
	res, err = orchclient.RestoreIssues(h.ctx, owner, orchapi.IssueRestoreRequest{})
	if err != nil || r.n != 2 || strings.Contains(strings.Join(res.Notes, "\n"), "mirror down") {
		t.Fatalf("%+v %v", res, err)
	}
}

// A mark set after a run began is not satisfied by that run.
func TestAMarkDuringARunWaits(t *testing.T) {
	h := newNativeHarness(t)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Minute)
	// As if the redaction finished after this run took its start time.
	mark := h.clock.now().Add(time.Second)
	if _, err := h.s.updateHome(h.harbor.ID, "issue", func(st *homeState) error {
		st.Backups[0].ScrubDue = &mark
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	if got := files(t, nativeDir(base)); len(got) != 4 {
		t.Fatalf("a run older than the mark purged: %v", got)
	}
	if x := h.load(b); x.ScrubDue == nil {
		t.Fatal("the mark was cleared")
	}
	h.clock.add(time.Minute)
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	if got := files(t, nativeDir(base)); len(got) != 2 {
		t.Fatalf("after a run past the mark: %v", got)
	}
	if x := h.load(b); x.ScrubDue != nil {
		t.Fatal("the mark stayed")
	}
}

func TestDueAt(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	mark := t0.Add(time.Minute)
	b := backupRec{Target: "file:///x", Every: 3600, LastRun: &t0}
	if got := dueAt(b); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("on schedule: %v", got)
	}
	b.ScrubDue = &mark
	if got := dueAt(b); !got.Equal(mark) {
		t.Fatalf("a redaction waiting: %v", got)
	}
	b.Failures = 2
	if got := dueAt(b); !got.Equal(t0.Add(retryAfterFailure)) {
		t.Fatalf("failing with a redaction waiting: %v", got)
	}
}

func TestBackupSchedule(t *testing.T) {
	for _, c := range []struct {
		kind, target string
		every, keep  int
		wantE, wantK int
		bad          string
	}{
		{orchapi.BackupSqlite, home.KindFile, 0, 0, 3600, defaultNativeKeep, ""},
		{orchapi.BackupSqlite, home.KindICloud, 0, 0, 0, 0, "cannot go to iCloud"},
		{orchapi.BackupSqlite, home.KindICloud, 172800, 3, 0, 0, "cannot go to iCloud"},
		{orchapi.BackupSqlite, home.KindFile, 30, 0, 0, 0, "at least"},
		{orchapi.BackupSqlite, home.KindFile, 0, -1, 0, 0, "positive"},
		{orchapi.BackupExport, home.KindICloud, 3600, 0, 3600, defaultKeep, ""},
		{orchapi.BackupExport, home.KindICloud, 0, 0, defaultBackupEvery, defaultKeep, ""},
	} {
		e, k, err := backupSchedule(c.kind, c.target, c.every, c.keep)
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%+v: %v", c, err)
			}
			continue
		}
		if err != nil || e != c.wantE || k != c.wantK {
			t.Errorf("%+v: %d %d %v", c, e, k, err)
		}
	}
}

func TestNewestCopyReadsTheName(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		want       time.Time
	}{
		{orchapi.BackupSqlite, "s-p-20261101T101112.345Z.issues.db.enc", time.Date(2026, 11, 1, 10, 11, 12, 345e6, time.UTC)},
		{orchapi.BackupSqlite, "s-p-20261101T101112.345Z.issues.db", time.Date(2026, 11, 1, 10, 11, 12, 345e6, time.UTC)}, // an older zm's
		{orchapi.BackupExport, "s-p-20261101T101112Z.jsonl", time.Date(2026, 11, 1, 10, 11, 12, 0, time.UTC)},
	} {
		dir := t.TempDir()
		d := backupDir(dir, "s", "p", tc.kind)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, tc.name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, n := newestCopy(backupRec{Kind: tc.kind, Base: dir, Target: "file://" + dir}, "s", "p")
		if got == nil || n != 1 || !got.Equal(tc.want) {
			t.Fatalf("%s: %v %d", tc.name, got, n)
		}
	}
}

// Sqlite copies are encrypted with the home's backup key: project.db's secret
// names (grants, the secret ledger) are on the target only as ciphertext, the
// copy opens to the store with the key, and an older zm's plain copies go once
// an encrypted set is there.
func TestSqliteCopiesAreEncrypted(t *testing.T) {
	h := newNativeHarness(t)
	const secretName = "ZZ_GRANTED_SECRET_NAME_IN_PROJECT_DB"
	h.marker(secretName)
	base := t.TempDir()
	b := h.rec("file://"+base, base, 3600, 5)
	dir := nativeDir(base)
	os.MkdirAll(dir, 0o700)
	for _, suf := range plainSuffixes {
		os.WriteFile(filepath.Join(dir, "harbor-issue-20260101T000000.000Z"+suf), []byte("plain "+secretName), 0o600)
	}
	if _, err := h.run(b); err != nil {
		t.Fatal(err)
	}
	got := files(t, dir)
	if len(got) != 2 || !strings.HasSuffix(got[0], copyIssues) || !strings.HasSuffix(got[1], copyProject) {
		t.Fatalf("copies %v: want one encrypted set and no plain copy", got)
	}
	for _, n := range got {
		raw, _ := os.ReadFile(filepath.Join(dir, n))
		if strings.Contains(string(raw), secretName) || strings.HasPrefix(string(raw), "SQLite format 3") {
			t.Fatalf("%s is readable", n)
		}
		if st, _ := os.Stat(filepath.Join(dir, n)); st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", n, st.Mode().Perm())
		}
	}
	plain := filepath.Join(t.TempDir(), "project.db")
	if _, _, err := bkcrypt.DecryptFile(h.ctx, filepath.Join(dir, got[1]), plain, h.backupKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlbackup.Check(h.ctx, plain); err != nil {
		t.Fatalf("the decrypted project.db: %v", err)
	}
	if raw, _ := os.ReadFile(plain); !strings.Contains(string(raw), secretName) {
		t.Fatal("the decrypted project.db lacks its rows")
	}
	if at, n := newestCopy(h.load(b), "harbor", "issue"); at == nil || n != 1 {
		t.Fatalf("newest copy %v, %d", at, n)
	}
	// Nothing plain stays in the tracker's staging either.
	if des, _ := os.ReadDir(filepath.Dir(h.s.localScratch(h.harbor.ID, "issue", "x"))); len(des) != 0 {
		t.Fatalf("staging left %v", des)
	}
}
