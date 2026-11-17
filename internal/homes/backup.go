package homes

// zm tracker backup (SPEC 17.9). Two kinds: "sqlite" (native issues: copies
// of the issue store and project.db a restore can use, native_backup.go) and
// "export" (any adapter: a JSON Lines snapshot of the tracker's items from
// the mirror, with zm's comments, keeping the last N). The daemon runs each
// backup on its schedule; a backup never runs while a restore or another
// backup of the same tracker does.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

// Backup defaults.
var (
	backupTick         = time.Minute // how often the daemon looks for due backups
	defaultBackupEvery = 3600        // seconds
	minBackupEvery     = 60
	defaultKeep        = 48 // export snapshots
	retryAfterFailure  = 10 * time.Minute
)

// backupHook runs as a backup starts (tests).
var backupHook func(prefix string)

// backupBase checks a backup target's directory is there before anything
// is written: a file:// directory must exist (an unmounted volume would
// otherwise fill the boot disk unnoticed); icloud needs iCloud Drive and
// gets its zm-backups folder made.
func backupBase(kind, base string) error {
	if kind == home.KindICloud {
		uh, _ := userHomeDir()
		root := filepath.Dir(filepath.Join(uh, home.ICloudRel))
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return api.Errorf(api.CodeBadRequest, "iCloud Drive is not on this Mac (%s is missing)", root).WithHint("turn on iCloud Drive, or use file:///path")
		}
		return os.MkdirAll(base, 0o700)
	}
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return api.Errorf(api.CodeBadRequest, "backup directory %s does not exist (an unmounted volume?)", base).WithHint("create it (or mount it) first")
	}
	return nil
}

func backupDir(base, slug, prefix, kind string) string {
	return filepath.Join(base, slug+"-"+prefix, kind)
}

// backupKinds is what tracker tr offers: its native kinds, then export.
func (s *Service) backupKinds(ctx context.Context, pid string, tr api.Tracker) []string {
	var kinds []string
	if d, err := orchcore.Trackers(s.d).Describe(ctx, pid, tr); err == nil {
		kinds = append(kinds, d.Backup...)
	}
	return append(kinds, orchapi.BackupExport)
}

func (s *Service) backupView(pid, slug, prefix string, b backupRec, now time.Time) orchapi.BackupView {
	v := orchapi.BackupView{ID: b.ID, Target: b.Target, Kind: b.Kind, Path: backupDir(b.Base, slug, prefix, b.Kind),
		EverySeconds: b.Every, Keep: b.Keep, Created: b.Created, LastRun: b.LastRun, LastOK: b.LastOK,
		LastError: b.LastError, Failures: b.Failures, Items: b.Items, Bytes: b.Bytes, ScrubPending: b.ScrubDue != nil}
	v.Newest, v.Copies = newestCopy(b, slug, prefix)
	if b.LastOK != nil {
		v.AgeSeconds = now.Sub(*b.LastOK).Seconds()
	}
	d := dueAt(b)
	v.Due = &d
	v.Running = s.ops.running(pid, prefix) == "backup "+b.ID
	return v
}

// dueAt is when backup b runs next: at once if never run or a redaction is
// waiting for it, else a period after its last attempt (sooner after a
// failure). A failing backup waits out its retry even with a redaction
// waiting: it may be a whole store each time.
func dueAt(b backupRec) time.Time {
	if b.LastRun == nil {
		return b.Created
	}
	if b.Failures > 0 {
		return b.LastRun.Add(min(retryAfterFailure, time.Duration(b.Every)*time.Second))
	}
	if b.ScrubDue != nil {
		return *b.ScrubDue
	}
	return b.LastRun.Add(time.Duration(b.Every) * time.Second)
}

func (s *Service) findBackup(st homeState, id, kind string) (backupRec, error) {
	uh, _ := userHomeDir()
	canon := id
	if t, err := home.Parse(id, uh); err == nil {
		canon = t.String()
	}
	var hits []backupRec
	for _, b := range st.Backups {
		if (b.ID == id || b.Target == canon) && (kind == "" || b.Kind == kind) {
			hits = append(hits, b)
		}
	}
	switch len(hits) {
	case 0:
		return backupRec{}, api.NotFound("backup", id)
	case 1:
		return hits[0], nil
	}
	return backupRec{}, api.Errorf(api.CodeBadRequest, "%s has more than one kind of backup; pass --kind", id)
}

// addBackup is POST /v0/trackers/{prefix}/backups.
func (s *Service) addBackup(ctx context.Context, prefix string, req orchapi.AddBackupRequest) (orchapi.BackupResult, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.BackupResult{}, err
	}
	tr, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix)
	if !ok {
		return orchapi.BackupResult{}, api.NotFound("tracker", prefix)
	}
	uh, _ := userHomeDir()
	t, err := home.Parse(req.Target, uh)
	if err != nil {
		return orchapi.BackupResult{}, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	if err := t.BuiltAsBackup(); err != nil {
		return orchapi.BackupResult{}, api.Errorf(api.CodeNotImplemented, "%v", err)
	}
	if err := backupBase(t.Kind, t.Path); err != nil {
		return orchapi.BackupResult{}, err
	}
	kinds := s.backupKinds(ctx, pid, tr)
	kind := req.Kind
	if kind == "" {
		kind = kinds[0]
	}
	if !slices.Contains(kinds, kind) {
		return orchapi.BackupResult{}, api.Errorf(api.CodeBadRequest, "the %s tracker offers backups of kind %s, not %q", tr.Adapter, strings.Join(kinds, " and "), kind)
	}
	every, keep, err := backupSchedule(kind, t.Kind, req.EverySeconds, req.Keep)
	if err != nil {
		return orchapi.BackupResult{}, err
	}
	rec := backupRec{ID: t.Spec(uh), Target: t.String(), Base: t.Path, Kind: kind, Every: every, Keep: keep, Created: s.now().UTC()}
	if _, err := s.updateHome(pid, prefix, func(st *homeState) error {
		for _, b := range st.Backups {
			if b.Target == rec.Target && b.Kind == rec.Kind {
				return api.Errorf(api.CodeConflict, "tracker %s already has a %s backup to %s", prefix, kind, rec.ID)
			}
		}
		st.Backups = append(st.Backups, rec)
		return nil
	}); err != nil {
		return orchapi.BackupResult{}, err
	}
	if req.RunNow {
		return s.runBackup(homeCtx(ctx), pid, slug, tr, rec)
	}
	return orchapi.BackupResult{Backup: s.backupView(pid, slug, prefix, rec, s.now())}, nil
}

// backupSchedule is a new backup's period and retention, defaulted and
// checked. A sqlite backup stays on this machine's disks: each copy is a whole
// store, and uploading those stalled the machine.
func backupSchedule(kind, targetKind string, every, keep int) (int, int, error) {
	native := kind == orchapi.BackupSqlite
	if native && targetKind == home.KindICloud {
		return 0, 0, api.Errorf(api.CodeBadRequest, "a sqlite backup cannot go to iCloud: each copy is a whole store, and it stays on local disks").
			WithHint("use a file:/// target; --kind export still goes to iCloud")
	}
	if every == 0 {
		every = defaultBackupEvery
	}
	if every < minBackupEvery {
		return 0, 0, api.Errorf(api.CodeBadRequest, "--every must be at least %ds", minBackupEvery)
	}
	switch {
	case keep < 0:
		return 0, 0, api.Errorf(api.CodeBadRequest, "--keep must be positive")
	case kind == orchapi.BackupExport && keep == 0:
		keep = defaultKeep
	case native && keep == 0:
		keep = defaultNativeKeep
	}
	return every, keep, nil
}

// listBackups is GET /v0/trackers/{prefix}/backups.
func (s *Service) listBackups(ctx context.Context, prefix string) ([]orchapi.BackupView, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix); !ok {
		return nil, api.NotFound("tracker", prefix)
	}
	st, err := s.loadHome(pid, prefix)
	if err != nil {
		return nil, err
	}
	out := []orchapi.BackupView{}
	for _, b := range st.Backups {
		out = append(out, s.backupView(pid, slug, prefix, b, s.now()))
	}
	return out, nil
}

// removeBackup is DELETE /v0/trackers/{prefix}/backups: the schedule goes,
// the copies stay.
func (s *Service) removeBackup(ctx context.Context, prefix, id, kind string) (orchapi.BackupView, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.BackupView{}, err
	}
	if _, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix); !ok {
		return orchapi.BackupView{}, api.NotFound("tracker", prefix)
	}
	var gone backupRec
	_, err = s.updateHome(pid, prefix, func(st *homeState) error {
		b, err := s.findBackup(*st, id, kind)
		if err != nil {
			return err
		}
		gone = b
		st.Backups = slices.DeleteFunc(st.Backups, func(x backupRec) bool { return x.Target == b.Target && x.Kind == b.Kind })
		return nil
	})
	if err != nil {
		return orchapi.BackupView{}, err
	}
	return s.backupView(pid, slug, prefix, gone, s.now()), nil
}

// runBackups is POST /v0/trackers/{prefix}/backups/run: one backup (id),
// or every backup of the tracker.
func (s *Service) runBackups(ctx context.Context, prefix, id, kind string) ([]orchapi.BackupResult, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return nil, err
	}
	tr, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix)
	if !ok {
		return nil, api.NotFound("tracker", prefix)
	}
	st, err := s.loadHome(pid, prefix)
	if err != nil {
		return nil, err
	}
	recs := st.Backups
	if id != "" {
		b, err := s.findBackup(st, id, kind)
		if err != nil {
			return nil, err
		}
		recs = []backupRec{b}
	}
	if len(recs) == 0 {
		return nil, api.Errorf(api.CodeBadRequest, "tracker %s has no backups", prefix).WithHint("zm tracker backup add " + prefix + " <target>")
	}
	var out []orchapi.BackupResult
	var firstErr error
	for _, b := range recs {
		r, err := s.runBackup(homeCtx(ctx), pid, slug, tr, b) // a Ctrl-C does not cut a backup short
		if err != nil && firstErr == nil {
			firstErr = err
		}
		out = append(out, r)
	}
	if len(out) == 1 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// runBackup runs one backup now and records the outcome.
func (s *Service) runBackup(ctx context.Context, pid, slug string, tr api.Tracker, b backupRec) (orchapi.BackupResult, error) {
	release, err := s.ops.acquire(pid, tr.Prefix, "backup "+b.ID)
	if err != nil {
		return orchapi.BackupResult{Backup: s.backupView(pid, slug, tr.Prefix, b, s.now())}, err
	}
	defer release()
	if backupHook != nil {
		backupHook(tr.Prefix)
	}
	start := s.now().UTC()
	t0 := time.Now()
	var res orchapi.BackupResult
	var items int
	var bytes, storeBytes int64
	var purged *time.Time
	tk := home.KindFile
	if strings.HasPrefix(b.Target, home.KindICloud+":") {
		tk = home.KindICloud
	}
	err = backupBase(tk, b.Base)
	switch {
	case err != nil:
	case b.Kind == orchapi.BackupExport:
		items, bytes, err = s.backupExport(ctx, pid, slug, tr, b)
	case b.Kind == orchapi.BackupSqlite:
		bytes, storeBytes, purged, err = s.backupNative(ctx, pid, slug, tr, b, start)
	default:
		err = fmt.Errorf("unknown backup kind %q", b.Kind)
	}
	if err == nil && bytes <= 0 {
		err = fmt.Errorf("the backup left nothing on disk") // last_ok means a copy exists
	}
	res.Seconds = time.Since(t0).Seconds()
	if err != nil && ctx.Err() != nil {
		return res, err // cut off by a daemon stop: not the backup's failure; it runs again
	}
	st, uerr := s.updateHome(pid, tr.Prefix, func(st *homeState) error {
		for i := range st.Backups {
			x := &st.Backups[i]
			if x.Target != b.Target || x.Kind != b.Kind {
				continue
			}
			x.LastRun = timePtr(start)
			if err != nil {
				x.Failures++
				x.LastError = core.ClipText(err.Error(), 500)
			} else {
				x.Failures, x.LastError = 0, ""
				x.LastOK = timePtr(s.now().UTC())
				x.Items, x.Bytes, x.StoreBytes = items, bytes, storeBytes
				// A later redaction marked it meanwhile: that one still waits.
				if purged != nil && x.ScrubDue != nil && x.ScrubDue.Equal(*purged) {
					x.ScrubDue = nil
				}
			}
			b = *x
		}
		return nil
	})
	_ = st
	if uerr != nil && err == nil {
		err = uerr
	}
	res.Backup = s.backupView(pid, slug, tr.Prefix, b, s.now())
	res.Ran = err == nil
	if err != nil {
		if _, ok := err.(*api.Error); !ok {
			err = api.Errorf(api.CodeUnavailable, "backup %s of %s failed: %v", b.ID, tr.Prefix, err)
		}
		s.warn("tracker backup failed", "project", pid, "prefix", tr.Prefix, "backup", b.ID, "err", err)
	}
	return res, err
}

// backupExport has the work module write a JSON Lines snapshot of the mirror
// (SPEC 17.9), then keeps the newest b.Keep files.
func (s *Service) backupExport(ctx context.Context, pid, slug string, tr api.Tracker, b backupRec) (int, int64, error) {
	dir := backupDir(b.Base, slug, tr.Prefix, orchapi.BackupExport)
	items, path, err := orchcore.Trackers(s.d).Export(ctx, pid, slug, tr, dir, s.now())
	if err != nil {
		return 0, 0, err
	}
	// No keep on record means the default (a record an older zm wrote may
	// have 0, which would delete every snapshot).
	keep := b.Keep
	if keep <= 0 {
		keep = defaultKeep
	}
	// A backup that reports success has a file to show for it.
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("the snapshot is not there after writing it: %w", err)
	}
	if err := sqlbackup.Prune(dir, slug+"-"+tr.Prefix+"-", ".jsonl", keep); err != nil {
		s.warn("tracker backup: pruning old snapshots", "prefix", tr.Prefix, "err", err)
	}
	return items, st.Size(), nil
}

// backupDue starts, for each tracker with backups due and none running,
// a goroutine that runs them one after another, so a long copy of one
// tracker never delays another's schedule.
func (s *Service) backupDue(ctx context.Context) {
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			st, err := s.loadHome(p.ID, tr.Prefix)
			if err != nil || len(st.Backups) == 0 {
				continue
			}
			var due []backupRec
			for _, b := range st.Backups {
				if !s.now().Before(dueAt(b)) {
					due = append(due, b)
				}
			}
			key := orchapi.TrackerKey(p.ID, tr.Prefix)
			if len(due) == 0 {
				continue
			}
			if _, busy := s.bk.LoadOrStore(key, true); busy {
				continue
			}
			pid, slug := p.ID, f.Slug
			s.wg.Go(func() {
				defer s.bk.Delete(key)
				for _, b := range due {
					if ctx.Err() != nil {
						return
					}
					_, _ = s.runBackup(ctx, pid, slug, tr, b) // the outcome is recorded; a busy tracker waits a tick
				}
			})
		}
	}
}
