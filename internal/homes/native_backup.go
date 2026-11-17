package homes

// The sqlite backup kind (SPEC 17.9): copies of a native tracker's issue
// store and of its project.db, which is what a restore needs (an export holds
// only mirror items and work comments). Copies go on file targets only: each
// is a whole store, and uploading those to iCloud Drive stalled the machine.
// They are made by potluck/kit/sqlbackup (VACUUM INTO, see its package doc for why
// not Litestream) and each is checked before it gets its name, so last_ok means
// a verified file is there.
//
// Copies are encrypted (bkcrypt, the home's secrets/backup.key): project.db
// holds secret names (the grants table, the secret ledger). Each store is first
// copied into the tracker's staging in the home (moves/backup-*, where the
// stores already are), checked there, then encrypted onto the target, and the
// encrypted file is read back through the cipher and compared before it gets
// its name. Plain copies an older zm wrote are deleted once an encrypted set
// is there.
//
// A redaction reaches these copies by replacing them. PurgeBackups marks the
// backup (ScrubDue, which holds no text), a run started after the mark writes
// a fresh verified copy and only then deletes the older ones. If it fails the
// old ones stay, the mark stays, and the scheduler runs it again; so a backup
// never ends with no copy, and a redaction that did not reach a copy says so.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/sqlbackup"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// A copy is <slug>-<prefix>-<stamp>.issues.db.enc and .project.db.enc
// (encrypted; an older zm wrote them plain, without .enc). The stamp sorts by
// time and is unique within the directory.
const (
	copyIssues   = ".issues.db.enc"
	copyProject  = ".project.db.enc"
	plainIssues  = ".issues.db"
	plainProject = ".project.db"
	copyStamp    = "20060102T150405.000Z"

	defaultNativeKeep = 24 // hourly: a day

	copyBusy = 5 * time.Second // the busy timeout of a copy's own connection
)

// opsWait bounds WaitIdle.
var opsWait = 30 * time.Second

// copySuffixes are what one set holds; plainSuffixes an older zm's sets.
var (
	copySuffixes  = []string{copyIssues, copyProject}
	plainSuffixes = []string{plainIssues, plainProject}
)

// backupNative writes one set of copies and retires what it replaces. It
// returns the bytes on disk, the issue store copy's alone, and the redaction
// mark it satisfied (nil: none). Called holding the tracker's home lock.
func (s *Service) backupNative(ctx context.Context, pid, slug string, tr api.Tracker, b backupRec, start time.Time) (int64, int64, *time.Time, error) {
	if tr.Adapter != orchapi.AdapterNative {
		return 0, 0, nil, api.Errorf(api.CodeBadRequest, "tracker %s is not zm-native, so it has no store for a sqlite backup", tr.Prefix).
			WithHint("zm tracker backup rm " + tr.Prefix + " " + b.ID + " --kind sqlite")
	}
	if orchcore.Issues(s.d) == nil {
		return 0, 0, nil, api.Errorf(api.CodeUnavailable, "native issues are not available in this daemon")
	}
	// The record as it is now, not as the schedule read it: a redaction may
	// have marked it since.
	st, err := s.loadHome(pid, tr.Prefix)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, x := range st.Backups {
		if x.Target == b.Target && x.Kind == b.Kind {
			b = x
		}
	}
	dir := backupDir(b.Base, slug, tr.Prefix, orchapi.BackupSqlite)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, 0, nil, err
	}
	stem := slug + "-" + tr.Prefix + "-"
	s.sweepScratch(pid, tr.Prefix)
	if err := sqlbackup.Sweep(dir, stem); err != nil {
		return 0, 0, nil, err
	}
	name := copyName(dir, stem, start)
	total, store, err := s.writeCopies(ctx, pid, tr.Prefix, dir, name, b.StoreBytes)
	if err != nil {
		return 0, 0, nil, err
	}
	var errs []error
	if b.ScrubDue != nil && !start.Before(*b.ScrubDue) {
		// Everything before this set may hold the text.
		for _, suf := range copySuffixes {
			errs = append(errs, sqlbackup.KeepOnly(dir, stem, suf, name+suf))
		}
		errs = append(errs, removePlain(dir, stem))
		if err := errors.Join(errs...); err != nil {
			return 0, 0, nil, fmt.Errorf("deleting the older copies: %w", err)
		}
		return total, store, b.ScrubDue, nil
	}
	keep := b.Keep
	if keep <= 0 {
		keep = defaultNativeKeep
	}
	for _, suf := range copySuffixes {
		errs = append(errs, sqlbackup.Prune(dir, stem, suf, keep))
	}
	if err := errors.Join(errs...); err != nil {
		s.warn("tracker backup: pruning old copies", "prefix", tr.Prefix, "err", err) // the new copy is good
	}
	if err := removePlain(dir, stem); err != nil {
		return 0, 0, nil, fmt.Errorf("deleting the unencrypted copies an older zm wrote: %w", err)
	}
	return total, store, nil, nil
}

// removePlain deletes the plain copies an older zm wrote in dir. Called
// only once an encrypted set is there.
func removePlain(dir, stem string) error {
	var errs []error
	for _, suf := range plainSuffixes {
		names, err := sqlbackup.List(dir, stem, suf)
		errs = append(errs, err)
		for _, n := range names {
			errs = append(errs, os.Remove(filepath.Join(dir, n)))
		}
	}
	return errors.Join(errs...)
}

// copyName is a set's name, free in dir.
func copyName(dir, stem string, at time.Time) string {
	for {
		name := stem + at.UTC().Format(copyStamp)
		free := true
		for _, suf := range slices.Concat(copySuffixes, plainSuffixes) {
			free = free && !pathExists(filepath.Join(dir, name+suf))
		}
		if free {
			return name
		}
		at = at.Add(time.Millisecond)
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeCopies writes the set name into dir: the issue store and project.db,
// each copied and checked in the tracker's staging, then encrypted into dir.
// It returns the encrypted bytes and the issue store copy's plain size.
func (s *Service) writeCopies(ctx context.Context, pid, prefix, dir, name string, prevStore int64) (total, store int64, err error) {
	key, err := bkcrypt.Ensure(s.home())
	if err != nil {
		return 0, 0, fmt.Errorf("the backup key: %w", err)
	}
	stage := s.localScratch(pid, prefix, "backup-"+strings.ToLower(ids.New()))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return 0, 0, err
	}
	defer removeAllQuiet(stage)
	issues, proj := filepath.Join(stage, "issues.db"), filepath.Join(stage, "project.db")
	if store, err = orchcore.Issues(s.d).SnapshotNative(ctx, pid, issues, prevStore); err != nil {
		return 0, 0, fmt.Errorf("the issue store: %w", err)
	}
	if _, err := s.copyProjectDB(ctx, pid, proj); err != nil {
		return 0, 0, fmt.Errorf("project.db: %w", err)
	}
	// A set is whole or gone: failing every hour must not pile up store
	// copies past retention, which only a good run applies.
	var written []string
	for _, c := range []struct{ src, suf, what string }{{issues, copyIssues, "the issue store"}, {proj, copyProject, "project.db"}} {
		dst := filepath.Join(dir, name+c.suf)
		n, err := sealCopy(ctx, c.src, dst, key)
		if err != nil {
			for _, w := range written {
				os.Remove(w)
			}
			return 0, 0, fmt.Errorf("%s: %w", c.what, err)
		}
		written = append(written, dst)
		total += n
	}
	return total, store, nil
}

// sealCopy encrypts the checked copy src to dst, reads dst back through the
// cipher to prove it opens to the same content, and returns dst's size.
func sealCopy(ctx context.Context, src, dst string, key *bkcrypt.Key) (int64, error) {
	_, sum, err := bkcrypt.EncryptFile(ctx, src, dst, key)
	if err != nil {
		return 0, err
	}
	_, back, err := bkcrypt.DecryptFile(ctx, dst, "", key)
	if err == nil && back != sum {
		err = errors.New("the encrypted copy does not read back as the copy")
	}
	var st os.FileInfo
	if err == nil {
		st, err = os.Stat(dst)
	}
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	return st.Size(), nil
}

// copyProjectDB uses its own connection: a copy of the file must not sit on
// one of the project pool's few for as long as it takes.
func (s *Service) copyProjectDB(ctx context.Context, pid, dst string) (int64, error) {
	if _, err := s.d.Store.Project(authz.All(), authz.Read, pid); err != nil {
		return 0, err
	}
	db, err := zmstore.OpenFile(ctx, filepath.Join(s.d.Store.ProjectDir(pid), zmstore.FileProject), 1, copyBusy, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	info, err := sqlbackup.Snapshot(ctx, db, dst, 0)
	return info.Bytes, err
}

// sweepScratch removes staging a crash left: it holds what a copy holds, and
// no scrubber looks there. Called holding the tracker's home lock.
func (s *Service) sweepScratch(pid, prefix string) {
	moves := filepath.Dir(s.localScratch(pid, prefix, "x"))
	des, err := os.ReadDir(moves)
	if err != nil {
		return
	}
	for _, de := range des {
		if n := de.Name(); strings.HasPrefix(n, "restore-") || strings.HasPrefix(n, "backup-") {
			removeAllQuiet(filepath.Join(moves, n))
		}
	}
}

// PurgeBackups implements core.HomeService. See the top of this file.
func (s *Service) PurgeBackups(ctx context.Context, project string, fresh bool) error {
	f, ok := s.d.Projects.File(project)
	if !ok {
		return nil
	}
	var errs []error
	for _, tr := range orchcore.ProjectTrackers(f) {
		if tr.Adapter != orchapi.AdapterNative {
			continue
		}
		st, err := s.loadHome(project, tr.Prefix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		has := false
		for _, b := range st.Backups {
			has = has || b.Kind == orchapi.BackupSqlite
		}
		if !has {
			if release, err := s.ops.acquire(project, tr.Prefix, "redaction scrub"); err == nil {
				s.sweepScratch(project, tr.Prefix)
				release()
			}
			continue
		}
		if fresh {
			now := s.now().UTC()
			if st, err = s.updateHome(project, tr.Prefix, func(st *homeState) error {
				for i := range st.Backups {
					if st.Backups[i].Kind == orchapi.BackupSqlite {
						st.Backups[i].ScrubDue = &now
					}
				}
				return nil
			}); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		for _, b := range st.Backups {
			if b.Kind != orchapi.BackupSqlite || b.ScrubDue == nil {
				continue
			}
			if _, err := s.runBackup(ctx, project, f.Slug, tr, b); err != nil {
				errs = append(errs, fmt.Errorf("backup %s of %s: %w", b.ID, tr.Prefix, err))
			}
		}
	}
	// The home's off-site sets hold every project's stores.
	errs = append(errs, s.nativeBackupGaps(project, f.Slug, orchcore.ProjectTrackers(f)))
	errs = append(errs, s.purgeOffsite(ctx, fresh))
	return errors.Join(errs...)
}

// WaitIdle implements core.HomeService. It gives up after opsWait: the caller
// then fails as it would have, and retries.
func (s *Service) WaitIdle(ctx context.Context, project string) {
	f, ok := s.d.Projects.File(project)
	if !ok {
		return
	}
	deadline := time.Now().Add(opsWait)
	for s.snapping.Load() && time.Now().Before(deadline) { // the off-site backup reads the stores
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, tr := range orchcore.ProjectTrackers(f) {
		if tr.Adapter != orchapi.AdapterNative {
			continue
		}
		for s.ops.running(project, tr.Prefix) != "" && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// newestCopy is when b's newest copy on disk was made, and how many there
// are: what is there, not what the record says.
func newestCopy(b backupRec, slug, prefix string) (*time.Time, int) {
	dir := backupDir(b.Base, slug, prefix, b.Kind)
	stem := slug + "-" + prefix + "-"
	var names []string
	switch b.Kind {
	case orchapi.BackupSqlite:
		names, _ = sqlbackup.List(dir, stem, copyIssues)
		old, _ := sqlbackup.List(dir, stem, plainIssues)
		names = append(names, old...)
		slices.Sort(names)
	case orchapi.BackupExport:
		names, _ = sqlbackup.List(dir, stem, ".jsonl")
	default: // one incremental copy, whose time is its last success
		if !pathExists(dir) {
			return nil, 0
		}
		if b.LastOK == nil {
			return nil, 1
		}
		return b.LastOK, 1
	}
	if len(names) == 0 {
		return nil, 0
	}
	last := names[len(names)-1]
	stamp := strings.TrimPrefix(last, stem)
	if i := strings.IndexByte(stamp, 'Z'); i >= 0 {
		stamp = stamp[:i+1]
	}
	for _, layout := range []string{copyStamp, "20060102T150405Z"} {
		if t, err := time.Parse(layout, stamp); err == nil {
			return &t, len(names)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, last)); err == nil {
		t := st.ModTime().UTC()
		return &t, len(names)
	}
	return nil, len(names)
}

// NewestCopy implements core.HomeService.
func (s *Service) NewestCopy(project, slug, prefix string) *orchapi.BackupBrief {
	st, err := s.loadHome(project, prefix)
	if err != nil || len(st.Backups) == 0 {
		return nil
	}
	recs := st.Backups
	var native []backupRec
	for _, b := range recs {
		if b.Kind == orchapi.BackupSqlite {
			native = append(native, b)
		}
	}
	if len(native) > 0 {
		recs = native
	}
	out := &orchapi.BackupBrief{ID: recs[0].ID, Kind: recs[0].Kind}
	for _, b := range recs {
		out.Failing = out.Failing || b.Failures > 0
		out.ScrubPending = out.ScrubPending || b.ScrubDue != nil
		t, _ := newestCopy(b, slug, prefix)
		if t == nil || out.Newest != nil && !t.After(*out.Newest) {
			continue
		}
		age := s.now().Sub(*t)
		out.ID, out.Kind, out.Newest, out.AgeSeconds = b.ID, b.Kind, t, age.Seconds()
		out.Overdue = age > 2*time.Duration(b.Every)*time.Second
	}
	return out
}
