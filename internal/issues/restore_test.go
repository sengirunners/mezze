package issues_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

// backupFixture is a daemon with a native tracker and one sqlite backup.
type backupFixture struct {
	*leaseFixture
	t   *testing.T
	dir string // the backup target
	pid string
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := newLeaseFixture(t, true)
	f.create("first") // the prefix, and a store to copy
	p, err := f.owner.GetProject(context.Background(), "harbor")
	if err != nil {
		t.Fatal(err)
	}
	return &backupFixture{leaseFixture: f, t: t, dir: t.TempDir(), pid: p.ID}
}

func (f *backupFixture) addBackup() orchapi.BackupResult {
	f.t.Helper()
	res, err := orchclient.AddTrackerBackup(context.Background(), f.owner, "issue", orchapi.AddBackupRequest{Target: "file://" + f.dir, Kind: orchapi.BackupSqlite, RunNow: true})
	if err != nil || !res.Ran {
		f.t.Fatalf("backup: %+v %v", res, err)
	}
	return res
}

func (f *backupFixture) comment(id, text string) orchapi.IssueComment {
	f.t.Helper()
	c, err := orchclient.CommentIssue(context.Background(), f.owner, id, orchapi.IssueCommentRequest{Text: text})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// copies is every store copy under dir.
func copiesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".issues.db.enc") || strings.HasSuffix(p, ".issues.db") || strings.HasSuffix(p, ".issues.db.gz")) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func (f *backupFixture) view() orchapi.BackupView {
	f.t.Helper()
	vs, err := orchclient.TrackerBackups(context.Background(), f.owner, "issue")
	if err != nil || len(vs) != 1 {
		f.t.Fatalf("backups: %+v %v", vs, err)
	}
	return vs[0]
}

// editHome changes the first backup record in home.json (as a file: a
// simulated past, which no API should be able to reach).
func (f *backupFixture) editHome(fn func(b map[string]any)) {
	f.t.Helper()
	path := filepath.Join(orchcore.Trackers(f.dm.Deps).TrackerDir(f.pid, "issue"), "home.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		f.t.Fatal(err)
	}
	fn(st["backups"].([]any)[0].(map[string]any))
	out, err := json.Marshal(st)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// The whole path: seed a store, back it up, redact something, restore an
// older copy that a redaction could not reach, and prove the redacted text
// is gone everywhere while the rest of that copy came back and what came
// after it did not.
func TestRestoreOlderCopyEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	keep := f.create("an issue the copy holds")
	leak := f.create("a leaked secret")
	const secret = "AKIA-RESTORE-SECRET-0007"
	const stays = "an unrelated comment that stays"
	f.comment(keep, stays)
	// Long: its end sits on an overflow page, which an overwrite frees
	// with the text still on it. Only a compaction removes that.
	c := f.comment(leak, strings.Repeat("padding ", 4000)+"key is "+secret)
	// A zm work comment too, so project.db holds a copy of the text.
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+leak, orchapi.CommentRequest{Body: "again: " + secret}); err != nil {
		t.Fatal(err)
	}
	f.addBackup()
	cs := copiesIn(t, f.dir)
	if len(cs) != 1 || !strings.HasSuffix(cs[0], ".enc") || !anyFileHas(t, f.dir, secret) {
		t.Fatalf("copies %v: the backup does not hold the secret, so the test proves nothing", cs)
	}
	// An old copy somewhere a redaction does not reach: a drive in a
	// drawer, or a tier that was offline.
	aside := filepath.Join(t.TempDir(), "old"+filepath.Ext(cs[0]))
	if err := sqlbackup.Copy(ctx, cs[0], aside); err != nil {
		t.Fatal(err)
	}
	// What the copy will not hold: a new issue, and an edit, which
	// leaves the mirror's count as it was.
	later := f.create("an issue made after the copy")
	edited := "a title edited after the copy"
	if _, err := orchclient.UpdateIssue(ctx, f.owner, keep, orchapi.UpdateIssueRequest{Title: &edited}); err != nil {
		t.Fatal(err)
	}
	waitWork(t, func() bool {
		w, err := orchclient.GetWork(ctx, f.owner, "issue:"+keep)
		return err == nil && w.Title == edited
	}, "the mirror to see the edit")

	if code, body := redactAs(f.leaseFixture, leak, c.ID); code != 200 {
		t.Fatalf("redact: %d %s", code, body)
	}
	// Each comment is its own redaction.
	all, err := orchclient.IssueComments(ctx, f.owner, leak)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		if x.ID != c.ID {
			if code, body := redactAs(f.leaseFixture, leak, x.ID); code != 200 {
				t.Fatalf("redact: %d %s", code, body)
			}
		}
	}
	if p := fileWith(t, f.dir, secret); p != "" {
		t.Fatalf("the redaction left the secret in backup %s", p)
	}
	if got := copiesIn(t, f.dir); len(got) != 1 || got[0] == cs[0] {
		t.Fatalf("after the redaction: %v (was %v): want one fresh copy", got, cs)
	}
	if v := f.view(); v.ScrubPending || v.Failures != 0 {
		t.Fatalf("after the redaction: %+v", v)
	}
	home := f.dm.Deps.Config.Home
	if p := fileWith(t, home, secret); p != "" {
		t.Fatalf("the secret is in %s before the restore", p)
	}

	res, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: "file://" + aside})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.DryRun || res.Redactions == 0 || res.Kept == "" || res.Counts["issues"] != 3 {
		t.Fatalf("restore said %+v", res)
	}
	// Redacted, in the store and on every byte of the daemon's home.
	got, err := orchclient.IssueComments(ctx, f.owner, leak)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		if x.Text != "[redacted]" {
			t.Errorf("after the restore: %+v", x)
		}
	}
	if p := fileWith(t, home, secret); p != "" {
		t.Errorf("the restore brought the secret back: %s", p)
	}
	if p := fileWith(t, f.dir, secret); p != "" {
		t.Errorf("the restore left the secret in backup %s", p)
	}
	// The rest of the copy is intact.
	got, err = orchclient.IssueComments(ctx, f.owner, keep)
	if err != nil || len(got) != 1 || got[0].Text != stays {
		t.Fatalf("unrelated comment: %+v %v", got, err)
	}
	// Later changes are gone, and the mirror follows (its cursor is
	// not in the restored history).
	if _, err := orchclient.Issue(ctx, f.owner, later); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the later issue: %v", err)
	}
	waitWork(t, func() bool {
		_, err := orchclient.GetWork(ctx, f.owner, "issue:"+later)
		return api.IsCode(err, api.CodeNotFound)
	}, "the mirror to drop the later issue")
	waitWork(t, func() bool {
		w, err := orchclient.GetWork(ctx, f.owner, "issue:"+keep)
		return err == nil && w.Title == "an issue the copy holds"
	}, "the mirror to undo the edit")
	// The store it replaced is one of the backup's copies.
	if len(copiesIn(t, f.dir)) != 2 || !strings.HasPrefix(res.Kept, f.dir) {
		t.Fatalf("kept %s; copies %v", res.Kept, copiesIn(t, f.dir))
	}
	// Writes run again, and a new issue is a real one.
	again := f.create("after the restore")
	if f.issue(again).Title != "after the restore" {
		t.Fatal("no write after the restore")
	}
	// The next scheduled copy is not compared with the replaced store.
	if v := f.view(); v.ScrubPending {
		t.Fatalf("%+v", v)
	}
	if _, err := orchclient.RunTrackerBackups(ctx, f.owner, "issue", "", ""); err != nil {
		t.Fatalf("a backup after the restore: %v", err)
	}
}

func waitWork(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// When the fresh copy fails, the older ones stay, the redaction says it is
// not fully scrubbed, last_ok does not move, and the mark survives until a
// later run deletes them: never zero copies, never a false success.
func TestRedactKeepsOldCopiesWhenTheFreshOneFails(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	id := f.create("a leak")
	const secret = "AKIA-PURGE-SECRET-0008"
	c := f.comment(id, "key is "+secret)
	f.addBackup()
	before := f.view()
	old := copiesIn(t, f.dir)
	// The next copy is implausible against this record.
	f.editHome(func(b map[string]any) { b["store_bytes"] = float64(1 << 40) })

	code, body := redactAs(f.leaseFixture, id, c.ID)
	if code == 200 || !strings.Contains(body, "not fully scrubbed") {
		t.Fatalf("redact answered %d %s", code, body)
	}
	if got := copiesIn(t, f.dir); len(got) != 1 || got[0] != old[0] {
		t.Fatalf("old copies changed: %v, were %v", got, old)
	}
	v := f.view()
	if !v.ScrubPending || v.Failures != 1 || v.LastError == "" || !v.LastOK.Equal(*before.LastOK) {
		t.Fatalf("after a failed purge: %+v", v)
	}
	if w, err := orchclient.TrackerHome(ctx, f.owner, "issue"); err != nil || !strings.Contains(strings.Join(w.Notes, "\n"), "before a redaction") {
		t.Fatalf("where: %+v %v", w.Notes, err)
	}
	if tv, err := orchclient.Trackers(ctx, f.owner, "harbor"); err != nil || tv[0].Backup == nil || !tv[0].Backup.ScrubPending || !tv[0].Backup.Failing {
		t.Fatalf("tracker ls: %+v %v", tv, err)
	}
	// The cause clears; redacting again finishes it.
	f.editHome(func(b map[string]any) { delete(b, "store_bytes") })
	if code, body := redactAs(f.leaseFixture, id, c.ID); code != 200 {
		t.Fatalf("redact again: %d %s", code, body)
	}
	if p := fileWith(t, f.dir, secret); p != "" {
		t.Fatalf("the secret is still in %s", p)
	}
	if got := copiesIn(t, f.dir); len(got) != 1 {
		t.Fatalf("copies %v", got)
	}
	if v := f.view(); v.ScrubPending || v.Failures != 0 {
		t.Fatalf("after the retry: %+v", v)
	}
	// Redacting what is already redacted changes nothing: no fresh copy.
	cur := copiesIn(t, f.dir)
	if code, _ := redactAs(f.leaseFixture, id, c.ID); code != 200 {
		t.Fatal(code)
	}
	if got := copiesIn(t, f.dir); got[0] != cur[0] {
		t.Fatalf("a no-op redaction wrote a copy: %v", got)
	}
}

// homesSpy counts the waits a redaction makes for a running backup.
type homesSpy struct {
	orchcore.HomeService
	waits atomic.Int32
}

func (h *homesSpy) WaitIdle(ctx context.Context, project string) {
	h.waits.Add(1)
	h.HomeService.WaitIdle(ctx, project)
}

// A redaction waits out a running backup before its first attempt, rather
// than fail it and answer 503; one with nothing to scrub does not wait.
func TestRedactWaitsForARunningBackup(t *testing.T) {
	f := newBackupFixture(t)
	id := f.create("a leak")
	c := f.comment(id, "key is AKIA-WAIT-SECRET-0010")
	spy := &homesSpy{HomeService: orchcore.Homes(f.dm.Deps)}
	orchcore.Wire(f.dm.Deps).Homes = spy
	if code, body := redactAs(f.leaseFixture, id, c.ID); code != 200 {
		t.Fatalf("redact: %d %s", code, body)
	}
	if spy.waits.Load() != 1 {
		t.Fatalf("%d waits", spy.waits.Load())
	}
	if code, body := redactAs(f.leaseFixture, id, c.ID); code != 200 {
		t.Fatalf("again: %d %s", code, body)
	}
	if spy.waits.Load() != 1 {
		t.Fatalf("%d waits after a no-op", spy.waits.Load())
	}
}

func snapshotMigrations(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

// Refusals leave the live store as it was and writes running.
func TestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	id := f.create("live")
	f.comment(id, "a comment")
	f.addBackup()
	cp := copiesIn(t, f.dir)[0]
	work := t.TempDir()
	// A plain copy, as an older zm wrote them (they still restore), to edit.
	put := func(name string) string {
		p := filepath.Join(work, name)
		if err := os.WriteFile(p, decrypted(t, cp), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	later := f.create("after the copy")
	check := func(what string, err error, code, msg string) {
		t.Helper()
		if !api.IsCode(err, code) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v", what, err)
		}
		if _, err := orchclient.Issue(ctx, f.owner, later); err != nil {
			t.Errorf("%s changed the store: %v", what, err)
		}
		f.create("still writable after " + what)
	}
	newer := put("newer.issues.db")
	snapshotMigrations(t, newer, `INSERT INTO schema_migrations (name, applied_at) VALUES ('9999_from_the_future', 1)`)
	_, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: newer})
	check("a newer schema", err, api.CodeConflict, "newer zm")

	other := put("other.issues.db")
	snapshotMigrations(t, other, `UPDATE meta SET value = 'elsewhere' WHERE key = 'prefix'`)
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: other})
	check("another project's store", err, api.CodeConflict, "another project")

	running := put("running.issues.db")
	snapshotMigrations(t, running, `INSERT OR REPLACE INTO meta (key, value) VALUES ('import_state', 'running')`)
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: running})
	check("a copy from mid-import", err, api.CodeConflict, "import")

	// project.db is a whole database with migrations, but not a store.
	proj := strings.TrimSuffix(cp, ".issues.db.enc") + ".project.db.enc"
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: proj})
	check("a project.db", err, api.CodeBadRequest, "not an issue store")

	torn := put("torn.issues.db")
	b, err := os.ReadFile(torn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(torn, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: torn})
	check("a torn copy", err, api.CodeBadRequest, "cannot be restored")

	cut := filepath.Join(work, "cut.issues.db.enc")
	if b, err = os.ReadFile(cp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cut, b[:len(b)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: cut})
	check("a cut encrypted copy", err, api.CodeBadRequest, "cannot be restored")

	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{At: "19990101"})
	check("no such copy", err, api.CodeBadRequest, "no copy matching")
}

// A damaged store is why one restores: it cannot be copied first, so that
// takes --force, and the restore works with the store it replaces unreadable.
func TestRestoreOverADamagedStore(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	id := f.create("in the copy")
	f.addBackup()
	live := filepath.Join(f.dm.Deps.Store.ProjectDir(f.pid), "issues.db")
	// Close the open handles first, as a restart would: an open one's
	// checkpointer still holds the log, writes it back over the damage, and
	// the store copies whole. The next use reopens the file.
	if err := orchcore.Issues(f.dm.Deps).(*issues.Service).Store().Close(); err != nil {
		t.Fatal(err)
	}
	// With its log gone too: a recent store is served from there.
	os.Remove(live + "-wal")
	os.Remove(live + "-shm")
	if err := os.WriteFile(live, []byte(strings.Repeat("this is not a page ", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "could not be copied") {
		t.Fatalf("without --force: %v", err)
	}
	res, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{Force: true})
	if err != nil {
		t.Fatalf("--force: %v", err)
	}
	if res.Kept != "" || !strings.Contains(strings.Join(res.Notes, "\n"), "not kept") {
		t.Fatalf("result %+v", res)
	}
	if f.issue(id).Title != "in the copy" {
		t.Fatal("not restored")
	}
	f.create("writes run")
}

// A restore into a project with nothing to lose keeps nothing, and with a
// store and no sqlite backup to keep it in it asks for --force.
func TestRestoreFromAPathWithoutABackup(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	f.addBackup()
	cp := filepath.Join(t.TempDir(), "only.issues.db")
	if err := sqlbackup.Copy(ctx, copiesIn(t, f.dir)[0], cp); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.RemoveTrackerBackup(ctx, f.owner, "issue", "file://"+f.dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no backup and no --from: %v", err)
	}
	if _, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: cp}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "--force") && !strings.Contains(err.Error(), "copied first") {
		t.Fatalf("nowhere to keep the store: %v", err)
	}
	if _, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{From: cp, Force: true}); err != nil {
		t.Fatal(err)
	}
}

// A dry run proves a copy restores and touches nothing; the default copy is
// the newest; --at picks an older one.
func TestRestoreDryRunAndPick(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	f.addBackup()
	first := copiesIn(t, f.dir)[0]
	f.create("in the second copy only")
	if _, err := orchclient.RunTrackerBackups(ctx, f.owner, "issue", "", ""); err != nil {
		t.Fatal(err)
	}
	cs := copiesIn(t, f.dir)
	if len(cs) != 2 {
		t.Fatalf("copies %v", cs)
	}
	later := f.create("after both")
	res, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{DryRun: true})
	if err != nil || !res.DryRun || res.Copy != cs[1] || res.Counts["issues"] != 2 {
		t.Fatalf("newest: %+v %v", res, err)
	}
	stamp := strings.TrimSuffix(filepath.Base(first), ".issues.db.enc")
	stamp = stamp[strings.LastIndex(stamp, "-")+1:]
	res, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{DryRun: true, From: "file://" + f.dir, At: stamp})
	if err != nil || res.Copy != first || res.Counts["issues"] != 1 {
		t.Fatalf("--at: %+v %v", res, err)
	}
	if _, err := orchclient.Issue(ctx, f.owner, later); err != nil {
		t.Fatalf("a dry run changed the store: %v", err)
	}
	if got := copiesIn(t, f.dir); len(got) != 2 {
		t.Fatalf("a dry run wrote a copy: %v", got)
	}
}

// A restore holds writes for the swap, so it waits its turn behind a hold,
// and refuses rather than copy a store that is about to move.
func TestRestoreTakesTheWriteHold(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	f.addBackup()
	var svc *issues.Service
	for _, m := range f.dm.Modules() {
		if s, ok := m.(*issues.Service); ok {
			svc = s
		}
	}
	release, err := svc.HoldWrites(f.pid, "a test holds")
	if err != nil {
		t.Fatal(err)
	}
	_, err = orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{})
	release()
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "held") {
		t.Fatalf("under a hold: %v", err)
	}
	if _, err := orchclient.RestoreIssues(ctx, f.owner, orchapi.IssueRestoreRequest{}); err != nil {
		t.Fatal(err)
	}
	// Its own hold is released: writes run.
	f.create("after")
}

// tracker ls and backup ls report the file, not the record: the PR #80
// lesson, where a backup with no copy reported hourly success.
func TestNewestCopyIsWhatIsOnDisk(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	tv, err := orchclient.Trackers(ctx, f.owner, "harbor")
	if err != nil || tv[0].Backup != nil {
		t.Fatalf("no backup yet: %+v %v", tv[0].Backup, err)
	}
	f.addBackup()
	tv, err = orchclient.Trackers(ctx, f.owner, "harbor")
	if err != nil || tv[0].Backup == nil || tv[0].Backup.Newest == nil || tv[0].Backup.Kind != orchapi.BackupSqlite || tv[0].Backup.Overdue {
		t.Fatalf("after a backup: %+v %v", tv[0].Backup, err)
	}
	if v := f.view(); v.Newest == nil || v.Copies != 1 {
		t.Fatalf("view %+v", v)
	}
	if err := os.RemoveAll(filepath.Join(f.dir, "harbor-issue")); err != nil {
		t.Fatal(err)
	}
	tv, err = orchclient.Trackers(ctx, f.owner, "harbor")
	if err != nil || tv[0].Backup == nil || tv[0].Backup.Newest != nil {
		t.Fatalf("the copies are gone: %+v %v", tv[0].Backup, err)
	}
	if v := f.view(); v.Newest != nil || v.Copies != 0 || v.LastOK == nil {
		t.Fatalf("view %+v", v)
	}
}

// The default kind of a native tracker is the one that can restore it, and
// export stays on offer.
func TestNativeBackupKinds(t *testing.T) {
	ctx := context.Background()
	f := newBackupFixture(t)
	w, err := orchclient.TrackerHome(ctx, f.owner, "issue")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.Kinds, ",") != "sqlite,export" {
		t.Fatalf("kinds %v", w.Kinds)
	}
	r, err := orchclient.AddTrackerBackup(ctx, f.owner, "issue", orchapi.AddBackupRequest{Target: "file://" + f.dir})
	if err != nil || r.Backup.Kind != orchapi.BackupSqlite || r.Backup.Keep != 24 || r.Backup.EverySeconds != 3600 {
		t.Fatalf("default: %+v %v", r, err)
	}
}
